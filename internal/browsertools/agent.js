// Page-side helper for gta's browser tools.
//
// This file is evaluated as a single function expression and invoked as
//   (<this file>)("method", [arg, ...])
// It installs itself once per document on window.__gtaAgent (reinstalled
// when the version changes) and dispatches to the named method. Every method
// returns plain JSON-serializable data; logical failures are reported as
// {error: "..."} rather than thrown, so the Go side gets a clear message.
//
// It walks the composed tree: open shadow roots (through <slot> assignment)
// and same-origin iframes, whose coordinates are translated into the top
// frame's viewport so the Go side can drive the mouse with them.
(function (method, args) {
  "use strict";
  var VERSION = 4;
  var ATTR = "data-gta-ref";
  var TOP = window;

  var A = TOP.__gtaAgent;
  if (!A || A.version !== VERSION) {
    A = install();
    try {
      Object.defineProperty(TOP, "__gtaAgent", { value: A, configurable: true, writable: true, enumerable: false });
    } catch (e) {
      TOP.__gtaAgent = A;
    }
  }
  var fn = A.api[method];
  if (typeof fn !== "function") {
    return { error: "unknown agent method: " + method };
  }
  try {
    return fn.apply(null, args || []);
  } catch (e) {
    return { error: String((e && e.message) || e) };
  }

  function install() {
    var state = { version: VERSION, next: 1, cache: new Map(), api: {} };

    var SKIP_TAGS = { SCRIPT: 1, STYLE: 1, NOSCRIPT: 1, TEMPLATE: 1, HEAD: 1, META: 1, LINK: 1, TITLE: 1, BASE: 1 };
    var INTERACTIVE = {
      link: 1, button: 1, checkbox: 1, radio: 1, "switch": 1, textbox: 1, searchbox: 1, combobox: 1,
      listbox: 1, option: 1, menuitem: 1, menuitemcheckbox: 1, menuitemradio: 1, tab: 1, slider: 1,
      spinbutton: 1, treeitem: 1, clickable: 1, gridcell: 1, file: 1, date: 1, color: 1
    };
    var CONTAINERS = {
      banner: 1, navigation: 1, main: 1, complementary: 1, contentinfo: 1, region: 1, form: 1, search: 1,
      dialog: 1, alertdialog: 1, group: 1, menu: 1, menubar: 1, listbox: 1, tablist: 1, tree: 1, grid: 1,
      radiogroup: 1, toolbar: 1, iframe: 1
    };
    var SEMANTIC = { heading: 1, img: 1, alert: 1, status: 1, label: 1, tabpanel: 1 };
    var KNOWN_ROLES = {};
    [INTERACTIVE, CONTAINERS, SEMANTIC].forEach(function (set) {
      for (var k in set) KNOWN_ROLES[k] = 1;
    });
    ["log", "marquee", "timer", "progressbar", "meter", "scrollbar", "separator", "tooltip", "feed",
      "article", "list", "listitem", "table", "row", "cell", "columnheader", "rowheader", "none",
      "presentation", "generic", "paragraph", "figure", "note", "math", "definition", "term"].forEach(function (r) {
        KNOWN_ROLES[r] = 1;
      });
    var NAME_FROM_CONTENT = {
      link: 1, button: 1, checkbox: 1, radio: 1, "switch": 1, heading: 1, tab: 1, menuitem: 1,
      menuitemcheckbox: 1, menuitemradio: 1, option: 1, treeitem: 1, clickable: 1, gridcell: 1,
      label: 1, alert: 1, status: 1, tooltip: 1, cell: 1
    };
    var TEXT_INPUTS = { "": 1, text: 1, email: 1, password: 1, search: 1, tel: 1, url: 1, number: 1 };

    function norm(s) {
      return String(s == null ? "" : s).replace(/[\s ​]+/g, " ").trim();
    }
    function cap(s, n) {
      s = norm(s);
      return s.length > n ? s.slice(0, n - 1) + "…" : s;
    }
    function lower(s) {
      return norm(s).toLowerCase();
    }
    function styleOf(el) {
      try {
        return el.ownerDocument.defaultView.getComputedStyle(el);
      } catch (e) {
        return null;
      }
    }
    function tagOf(el) {
      return (el.tagName || "").toUpperCase();
    }
    function inputType(el) {
      return tagOf(el) === "INPUT" ? String(el.getAttribute("type") || "").toLowerCase() : "";
    }

    // ---- frames and geometry -------------------------------------------

    // frameOffset returns the top-viewport offset of a same-origin frame
    // window's content box, and the clip rect of the frame chain.
    function frameOffset(win) {
      var x = 0, y = 0;
      var clip = { x: 0, y: 0, w: TOP.innerWidth, h: TOP.innerHeight };
      var chain = [];
      while (win && win !== TOP) {
        var fe;
        try {
          fe = win.frameElement;
        } catch (e) {
          fe = null;
        }
        if (!fe) break;
        chain.push(fe);
        win = win.parent;
      }
      for (var i = chain.length - 1; i >= 0; i--) {
        var f = chain[i];
        var r = f.getBoundingClientRect();
        var cs = styleOf(f);
        var pl = cs ? parseFloat(cs.paddingLeft) || 0 : 0;
        var pt = cs ? parseFloat(cs.paddingTop) || 0 : 0;
        x += r.left + f.clientLeft + pl;
        y += r.top + f.clientTop + pt;
        clip = intersect(clip, { x: x, y: y, w: f.clientWidth - pl, h: f.clientHeight - pt });
      }
      return { x: x, y: y, clip: clip };
    }
    function intersect(a, b) {
      var x1 = Math.max(a.x, b.x), y1 = Math.max(a.y, b.y);
      var x2 = Math.min(a.x + a.w, b.x + b.w), y2 = Math.min(a.y + a.h, b.y + b.h);
      return { x: x1, y: y1, w: Math.max(0, x2 - x1), h: Math.max(0, y2 - y1) };
    }
    function rectOf(el, off) {
      var r = el.getBoundingClientRect();
      return { x: r.left + off.x, y: r.top + off.y, w: r.width, h: r.height };
    }
    function round(r) {
      return [Math.round(r.x), Math.round(r.y), Math.round(r.w), Math.round(r.h)];
    }

    // ---- visibility ------------------------------------------------------

    function hasBox(el) {
      var r = el.getBoundingClientRect();
      return r.width > 0 && r.height > 0;
    }
    function isHiddenStyle(cs) {
      return !cs || cs.visibility === "hidden" || cs.visibility === "collapse";
    }
    function isVisible(el) {
      if (!el || !el.isConnected) return false;
      if (typeof el.checkVisibility === "function") {
        if (!el.checkVisibility({ visibilityProperty: true, checkVisibilityCSS: true })) return false;
      }
      var cs = styleOf(el);
      if (!cs || cs.display === "none" || isHiddenStyle(cs)) return false;
      if (hasBox(el)) return true;
      // display:contents and zero-size wrappers: visible if a child is.
      if (cs.display === "contents") {
        for (var c = el.firstElementChild; c; c = c.nextElementSibling) if (isVisible(c)) return true;
      }
      return false;
    }
    // A control that is present but not seen (opacity 0 or a 1px box),
    // the usual pattern for custom-styled checkboxes behind a label.
    function isVisuallyHidden(el) {
      var cs = styleOf(el);
      if (!cs) return true;
      if (parseFloat(cs.opacity) === 0) return true;
      var r = el.getBoundingClientRect();
      return r.width * r.height < 4;
    }

    // ---- roles and names ---------------------------------------------------

    function explicitRole(el) {
      var r = el.getAttribute && el.getAttribute("role");
      if (!r) return "";
      var parts = r.trim().toLowerCase().split(/\s+/);
      for (var i = 0; i < parts.length; i++) if (KNOWN_ROLES[parts[i]]) return parts[i];
      return "";
    }
    function isEditableRoot(el) {
      if (!el.isContentEditable) return false;
      var p = el.parentElement;
      return !(p && p.isContentEditable);
    }
    function implicitRole(el) {
      var tag = tagOf(el);
      switch (tag) {
        case "A":
        case "AREA":
          return el.hasAttribute("href") ? "link" : "";
        case "BUTTON":
          return "button";
        case "SUMMARY":
          return "button";
        case "INPUT":
          var t = inputType(el);
          if (t === "hidden") return "";
          if (t === "button" || t === "submit" || t === "reset" || t === "image") return "button";
          if (t === "checkbox") return "checkbox";
          if (t === "radio") return "radio";
          if (t === "range") return "slider";
          if (t === "number") return "spinbutton";
          if (t === "search") return "searchbox";
          if (t === "file") return "file";
          if (t === "color") return "color";
          if (t === "date" || t === "datetime-local" || t === "month" || t === "time" || t === "week") return "date";
          return "textbox";
        case "SELECT":
          return el.multiple || el.size > 1 ? "listbox" : "combobox";
        case "TEXTAREA":
          return "textbox";
        case "H1": case "H2": case "H3": case "H4": case "H5": case "H6":
          return "heading";
        case "IMG":
          return norm(el.getAttribute("alt")) ? "img" : "";
        case "NAV":
          return "navigation";
        case "MAIN":
          return "main";
        case "ASIDE":
          return "complementary";
        case "FORM":
          return "form";
        case "DIALOG":
          return "dialog";
        case "FIELDSET":
          return "group";
        case "IFRAME":
        case "FRAME":
          return "iframe";
        case "HEADER":
          return el.closest("article,aside,main,nav,section") ? "" : "banner";
        case "FOOTER":
          return el.closest("article,aside,main,nav,section") ? "" : "contentinfo";
        case "SECTION":
          return el.hasAttribute("aria-label") || el.hasAttribute("aria-labelledby") ? "region" : "";
        case "OUTPUT":
          return "status";
        case "LABEL":
          return "label";
      }
      if (isEditableRoot(el)) return "textbox";
      return "";
    }
    function roleOf(el) {
      return explicitRole(el) || implicitRole(el);
    }
    function isClickableGeneric(el, cs) {
      if (el.hasAttribute("onclick")) return true;
      var ti = el.getAttribute("tabindex");
      if (ti !== null && parseInt(ti, 10) >= 0) return true;
      if (cs && cs.cursor === "pointer") {
        var p = el.parentElement;
        var pcs = p ? styleOf(p) : null;
        return !(pcs && pcs.cursor === "pointer");
      }
      return false;
    }
    function controlOf(el) {
      if (el && tagOf(el) === "LABEL") {
        var c = el.control;
        if (c) return c;
      }
      return el;
    }
    function rootOf(el) {
      var r = el.getRootNode ? el.getRootNode() : el.ownerDocument;
      return r && r.getElementById ? r : el.ownerDocument;
    }
    function textOf(el, depth) {
      if (!el) return "";
      depth = depth || 0;
      if (depth > 30) return "";
      if (el.nodeType === 3) return el.nodeValue;
      if (el.nodeType !== 1 && el.nodeType !== 11) return "";
      if (el.nodeType === 1) {
        var tag = tagOf(el);
        if (SKIP_TAGS[tag]) return "";
        if (el.getAttribute("aria-hidden") === "true") return "";
        var cs = styleOf(el);
        if (cs && (cs.display === "none" || isHiddenStyle(cs))) return "";
        if (tag === "IMG") return el.getAttribute("alt") || "";
        if (tag === "INPUT") {
          var t = inputType(el);
          if (t === "button" || t === "submit" || t === "reset") return el.value || "";
          return "";
        }
        if (tag === "SELECT") return "";
        if (el.localName === "svg") {
          var st = el.querySelector("title");
          return (st && st.textContent) || el.getAttribute("aria-label") || "";
        }
        var al = el.getAttribute("aria-label");
        if (al && depth > 0) return al;
      }
      var out = "";
      var kids = el.nodeType === 1 && el.shadowRoot ? el.shadowRoot.childNodes : el.childNodes;
      for (var i = 0; i < kids.length; i++) {
        var k = kids[i];
        if (k.nodeType === 1 && tagOf(k) === "SLOT") {
          var assigned = k.assignedNodes({ flatten: true });
          var nodes = assigned.length ? assigned : k.childNodes;
          for (var j = 0; j < nodes.length; j++) out += " " + textOf(nodes[j], depth + 1);
        } else {
          out += " " + textOf(k, depth + 1);
        }
        if (out.length > 400) break;
      }
      return out;
    }
    function labelledBy(el) {
      var ids = el.getAttribute("aria-labelledby");
      if (!ids) return "";
      var root = rootOf(el);
      return ids.split(/\s+/).map(function (id) {
        var n = root.getElementById(id) || el.ownerDocument.getElementById(id);
        return n ? textOf(n, 1) : "";
      }).join(" ");
    }
    function accName(el, role) {
      var n = norm(labelledBy(el));
      if (n) return n;
      n = norm(el.getAttribute("aria-label"));
      if (n) return n;
      var tag = tagOf(el);
      if (tag === "LABEL") {
        n = norm(textOf(el, 0));
        if (n) return n;
      }
      if (tag === "INPUT" || tag === "SELECT" || tag === "TEXTAREA" || tag === "METER" || tag === "PROGRESS") {
        if (el.labels && el.labels.length) {
          n = norm(Array.prototype.map.call(el.labels, function (l) { return textOf(l, 1); }).join(" "));
          if (n) return n;
        }
        var t = inputType(el);
        if (t === "button" || t === "submit" || t === "reset") {
          n = norm(el.value) || (t === "submit" ? "Submit" : t === "reset" ? "Reset" : "");
          if (n) return n;
        }
        if (t === "image") {
          n = norm(el.getAttribute("alt"));
          if (n) return n;
        }
        n = norm(el.getAttribute("placeholder"));
        if (n) return n;
      }
      if (tag === "IMG") {
        n = norm(el.getAttribute("alt"));
        if (n) return n;
      }
      if (tag === "FIELDSET") {
        var lg = el.querySelector("legend");
        if (lg) {
          n = norm(textOf(lg, 1));
          if (n) return n;
        }
      }
      if (tag === "IFRAME" || tag === "FRAME") {
        n = norm(el.getAttribute("title")) || norm(el.getAttribute("name"));
        if (n) return n;
        try {
          return new URL(el.src, el.ownerDocument.baseURI).host;
        } catch (e) {
          return "";
        }
      }
      if (NAME_FROM_CONTENT[role]) {
        n = norm(textOf(el, 0));
        if (n) return n;
      }
      n = norm(el.getAttribute("title"));
      if (n) return n;
      if (role === "dialog" || role === "alertdialog") {
        var h = el.querySelector("h1,h2,h3,h4,h5,h6,[role=heading]");
        if (h) return norm(textOf(h, 1));
      }
      return "";
    }

    // ---- state -------------------------------------------------------------

    function valueOf(el) {
      var tag = tagOf(el);
      if (tag === "INPUT") {
        var t = inputType(el);
        if (t === "checkbox" || t === "radio" || t === "button" || t === "submit" || t === "reset" || t === "image" || t === "file" && !el.files) return null;
        if (t === "file") return el.files.length ? Array.prototype.map.call(el.files, function (f) { return f.name; }).join(", ") : "";
        if (t === "password") return el.value ? "•".repeat(Math.min(el.value.length, 8)) : "";
        return el.value;
      }
      if (tag === "TEXTAREA") return el.value;
      if (tag === "SELECT") {
        return Array.prototype.filter.call(el.options, function (o) { return o.selected; })
          .map(function (o) { return norm(o.label || o.text); }).join(", ");
      }
      if (isEditableRoot(el)) return el.innerText;
      var vn = el.getAttribute && el.getAttribute("aria-valuenow");
      if (vn !== null && vn !== undefined) return el.getAttribute("aria-valuetext") || vn;
      return null;
    }
    function deepActive(doc) {
      var a = (doc || document).activeElement;
      for (var i = 0; a && i < 50; i++) {
        if (a.shadowRoot && a.shadowRoot.activeElement) {
          a = a.shadowRoot.activeElement;
          continue;
        }
        if ((tagOf(a) === "IFRAME" || tagOf(a) === "FRAME")) {
          var d = null;
          try {
            d = a.contentDocument;
          } catch (e) {
            d = null;
          }
          if (d && d.activeElement) {
            a = d.activeElement;
            continue;
          }
        }
        break;
      }
      return a;
    }
    function ariaBool(el, name) {
      var v = el.getAttribute(name);
      if (v === "true") return true;
      if (v === "false") return false;
      if (v === "mixed") return "mixed";
      return undefined;
    }
    function fillState(node, el, role) {
      var c = controlOf(el);
      var tag = tagOf(c);
      var t = inputType(c);
      if (t === "checkbox" || t === "radio") {
        node.checked = c.indeterminate ? "mixed" : !!c.checked;
      } else {
        var ac = ariaBool(c, "aria-checked");
        if (ac !== undefined) node.checked = ac;
        else if (role === "checkbox" || role === "radio" || role === "switch" || role === "menuitemcheckbox" || role === "menuitemradio") node.checked = false;
      }
      var disabled = false;
      try {
        disabled = (c.matches && c.matches(":disabled")) || c.getAttribute("aria-disabled") === "true";
      } catch (e) {
        disabled = !!c.disabled;
      }
      if (disabled) node.disabled = true;
      var ex = ariaBool(c, "aria-expanded");
      if (tag === "SUMMARY" && c.parentElement && tagOf(c.parentElement) === "DETAILS") ex = !!c.parentElement.open;
      if (ex !== undefined) node.expanded = ex;
      var sel = ariaBool(c, "aria-selected");
      if (sel !== undefined) node.selected = sel;
      var pr = ariaBool(c, "aria-pressed");
      if (pr !== undefined) node.pressed = pr;
      if (c.required || c.getAttribute("aria-required") === "true") node.required = true;
      if (c.readOnly || c.getAttribute("aria-readonly") === "true") node.readonly = true;
      if (c.getAttribute("aria-invalid") === "true") node.invalid = true;
      if (role === "heading") {
        var m = /^H([1-6])$/.exec(tag);
        node.level = m ? +m[1] : parseInt(c.getAttribute("aria-level") || "2", 10);
      }
      if (role === "dialog" && tag === "DIALOG" && c.hasAttribute("open")) {
        try {
          if (c.matches(":modal")) node.modal = true;
        } catch (e) { /* older engines */ }
      }
      if (c.getAttribute("aria-modal") === "true") node.modal = true;
      var v = valueOf(c);
      if (v !== null && v !== undefined && role !== "link" && role !== "button") node.value = cap(v, 120);
      if (tag === "INPUT" && t && t !== "text" && TEXT_INPUTS[t]) node.type = t;
      if (tag === "TEXTAREA") node.multiline = true;
      if (tag === "SELECT") {
        node.options = Array.prototype.slice.call(c.options, 0, 12).map(function (o) { return cap(o.label || o.text, 40); });
        if (c.options.length > 12) node.moreOptions = c.options.length - 12;
        if (c.multiple) node.multiple = true;
      }
      if (role === "link") {
        var href = c.getAttribute("href") || "";
        if (href && !/^javascript:/i.test(href)) {
          try {
            var u = new URL(href, c.ownerDocument.baseURI);
            var here = c.ownerDocument.location;
            node.href = cap(u.origin === here.origin ? u.pathname + u.search + u.hash : u.href, 80);
          } catch (e) {
            node.href = cap(href, 80);
          }
        }
      }
      try {
        if (deepActive(document) === c) node.focused = true;
      } catch (e) { /* ignore */ }
    }

    // ---- refs ------------------------------------------------------------------

    function refNum(r) {
      var m = /^e(\d+)$/.exec(r || "");
      return m ? parseInt(m[1], 10) : 0;
    }
    function ensureRef(el) {
      var r = el.getAttribute(ATTR);
      if (r && refNum(r)) {
        if (refNum(r) >= state.next) state.next = refNum(r) + 1;
      } else {
        r = "e" + state.next++;
        try {
          el.setAttribute(ATTR, r);
        } catch (e) { /* read-only element */ }
      }
      state.cache.set(r, new WeakRef(el));
      return r;
    }
    function forEachRoot(cb) {
      var seen = new Set();
      function visit(root) {
        if (!root || seen.has(root)) return;
        seen.add(root);
        if (cb(root) === false) return false;
        var all = root.querySelectorAll("*");
        for (var i = 0; i < all.length; i++) {
          var e = all[i];
          if (e.shadowRoot && visit(e.shadowRoot) === false) return false;
          var t = tagOf(e);
          if (t === "IFRAME" || t === "FRAME") {
            var d = null;
            try {
              d = e.contentDocument;
            } catch (err) {
              d = null;
            }
            if (d && visit(d) === false) return false;
          }
        }
      }
      visit(document);
    }
    function findRef(ref) {
      var w = state.cache.get(ref);
      var el = w && w.deref();
      if (el && el.isConnected && el.getAttribute(ATTR) === ref) return el;
      el = null;
      var sel = "[" + ATTR + '="' + ref + '"]';
      forEachRoot(function (root) {
        var hit = root.querySelector(sel);
        if (hit) {
          el = hit;
          return false;
        }
      });
      if (el) state.cache.set(ref, new WeakRef(el));
      return el;
    }
    function queryDeep(selector) {
      var out = [];
      var err = null;
      forEachRoot(function (root) {
        try {
          var list = root.querySelectorAll(selector);
          for (var i = 0; i < list.length; i++) out.push(list[i]);
        } catch (e) {
          err = e;
          return false;
        }
      });
      if (err) throw new Error("invalid CSS selector " + JSON.stringify(selector) + ": " + (err.message || err));
      return out;
    }

    // ---- walk ------------------------------------------------------------------

    // collect walks the composed tree from root and returns included nodes
    // in document order with element handles (not serialized).
    function collect(opts) {
      var max = opts.max || 600;
      var nodes = [];
      var total = 0;
      var crossOrigin = 0;
      var vw = TOP.innerWidth, vh = TOP.innerHeight;
      var viewport = { x: 0, y: 0, w: vw, h: vh };

      function consider(el, ctx) {
        var tag = tagOf(el);
        if (SKIP_TAGS[tag]) return;
        if (el.getAttribute("aria-hidden") === "true" || el.hasAttribute("inert")) return;
        var cs = styleOf(el);
        if (!cs || cs.display === "none") {
          // A display:none control can still be driven through its label;
          // the label is considered on its own.
          return;
        }
        if (tag === "DIALOG" && !el.hasAttribute("open")) return;
        if (tag === "DETAILS" && !el.open) {
          // Only the summary of a closed <details> is rendered.
          var sm = el.querySelector(":scope > summary");
          if (sm) consider(sm, ctx);
          return;
        }
        var hidden = isHiddenStyle(cs);
        var role = hidden ? "" : roleOf(el);
        if (role === "none" || role === "presentation" || role === "generic") role = "";
        var include = false;
        var labelFor = null;
        if (role && hasBox(el)) {
          if (role === "label") {
            // A label stands in for its control when the control itself is
            // not visible (custom checkbox pattern); otherwise its text is
            // already the control's name.
            var c = el.control;
            if (c && (!isVisible(c) || isVisuallyHidden(c))) {
              labelFor = c;
              include = true;
            }
          } else if (role === "checkbox" || role === "radio") {
            include = !(isVisuallyHidden(el) && el.labels && Array.prototype.some.call(el.labels, isVisible));
          } else if (INTERACTIVE[role]) {
            include = true;
          } else if (ctx.inInteractive) {
            include = false;
          } else if (SEMANTIC[role] || CONTAINERS[role]) {
            include = true;
          }
        } else if (role && CONTAINERS[role]) {
          // Zero-size containers (display:contents) still group children.
          include = role !== "iframe";
        } else if (!hidden && !role && !ctx.inInteractive && hasBox(el) && isClickableGeneric(el, cs) &&
          norm(textOf(el, 0)).length > 0 && tag !== "BODY" && tag !== "HTML") {
          role = "clickable";
          include = true;
        }
        if (include && opts.viewportOnly && !CONTAINERS[role]) {
          var vr = intersect(intersect(rectOf(el, ctx.off), viewport), ctx.off.clip);
          if (vr.w <= 0 || vr.h <= 0) include = false;
        }
        var startLen = nodes.length;
        var childCtx = ctx;
        if (include) {
          total++;
          if (nodes.length >= max) {
            include = false;
          } else {
            var effRole = labelFor ? roleOf(labelFor) || "label" : role;
            var node = { el: el, role: effRole, depth: ctx.depth };
            node.name = cap(labelFor ? accName(el, "label") || accName(labelFor, effRole) : accName(el, role), 100);
            fillState(node, labelFor || el, effRole);
            if (labelFor) node.viaLabel = true;
            var r = rectOf(el, ctx.off);
            var vis = intersect(intersect(r, viewport), ctx.off.clip);
            node.box = round(r);
            node.inView = vis.w > 0 && vis.h > 0;
            nodes.push(node);
            childCtx = {
              depth: CONTAINERS[role] ? ctx.depth + 1 : ctx.depth,
              off: ctx.off,
              inInteractive: ctx.inInteractive || (!!INTERACTIVE[effRole] && effRole !== "listbox" && effRole !== "combobox"),
            };
          }
        }
        if (labelFor) {
          // The label's content is folded into the node.
        } else if (tag === "IFRAME" || tag === "FRAME") {
          var doc = null;
          try {
            doc = el.contentDocument;
          } catch (e) {
            doc = null;
          }
          if (doc && doc.documentElement) {
            var fo = frameOffset(doc.defaultView);
            walkChildren(doc.body || doc.documentElement, { depth: childCtx.depth, off: fo, inInteractive: false });
          } else if (include) {
            nodes[nodes.length - 1].crossOrigin = true;
            crossOrigin++;
          }
        } else if (tag === "SELECT" || tag === "TEXTAREA") {
          // Options and text are summarized in the node itself.
        } else {
          walkChildren(el, childCtx);
        }
        // Drop containers that ended up with no content.
        if (include && CONTAINERS[role] && nodes.length === startLen + 1 && role !== "dialog" && role !== "alertdialog" && role !== "iframe") {
          nodes.pop();
          total--;
        }
      }
      function walkChildren(el, ctx) {
        var kids;
        if (el.shadowRoot) {
          kids = el.shadowRoot.children;
        } else {
          kids = el.children;
        }
        for (var i = 0; i < kids.length; i++) {
          var k = kids[i];
          if (tagOf(k) === "SLOT") {
            var assigned = k.assignedElements ? k.assignedElements({ flatten: true }) : [];
            if (assigned.length) {
              for (var j = 0; j < assigned.length; j++) consider(assigned[j], ctx);
            } else {
              walkChildren(k, ctx);
            }
            continue;
          }
          consider(k, ctx);
        }
      }

      var start = document.body || document.documentElement;
      var ctx0 = { depth: 0, off: { x: 0, y: 0, clip: viewport }, inInteractive: false };
      if (opts.scope) {
        var scopeEl = findRef(opts.scope);
        if (!scopeEl) return { error: staleMsg(opts.scope) };
        var fo = frameOffset(scopeEl.ownerDocument.defaultView);
        consider(scopeEl, { depth: 0, off: fo, inInteractive: false });
      } else if (start) {
        consider(start, ctx0);
      }
      return { nodes: nodes, total: total, crossOrigin: crossOrigin };
    }
    function staleMsg(ref) {
      return "ref " + ref + " is not on the page any more (the page changed); call browser_snapshot for fresh refs";
    }
    function scrollInfo() {
      var se = document.scrollingElement || document.documentElement;
      return {
        sx: Math.round(TOP.scrollX), sy: Math.round(TOP.scrollY),
        sw: se ? se.scrollWidth : 0, sh: se ? se.scrollHeight : 0,
        vw: TOP.innerWidth, vh: TOP.innerHeight
      };
    }
    function serialize(node) {
      var out = { ref: ensureRef(node.el), role: node.role, name: node.name, depth: node.depth, box: node.box };
      for (var k in node) {
        if (k !== "el" && k !== "role" && k !== "name" && k !== "depth" && k !== "box" && !(k in out)) out[k] = node[k];
      }
      out.inView = !!node.inView;
      return out;
    }

    // ---- element info for actions ---------------------------------------------

    function info(el) {
      var ref = ensureRef(el);
      var off = frameOffset(el.ownerDocument.defaultView);
      var r = rectOf(el, off);
      var viewport = { x: 0, y: 0, w: TOP.innerWidth, h: TOP.innerHeight };
      var vis = intersect(intersect(r, viewport), off.clip);
      var role = roleOf(el) || (isClickableGeneric(el, styleOf(el)) ? "clickable" : "");
      var c = controlOf(el);
      var node = { el: el, role: role, depth: 0 };
      fillState(node, c, role === "label" ? roleOf(c) : role);
      var t = inputType(c);
      var editable = (tagOf(c) === "INPUT" && (!!TEXT_INPUTS[t] || t === "date" || t === "datetime-local" || t === "month" || t === "time" || t === "week" || t === "color")) ||
        tagOf(c) === "TEXTAREA" || !!c.isContentEditable;
      return {
        ref: ref,
        role: role === "label" ? roleOf(c) || "label" : role,
        name: cap(accName(el, role) || (c !== el ? accName(c, roleOf(c)) : ""), 100),
        tag: tagOf(el).toLowerCase(),
        type: t,
        box: [r.x, r.y, r.w, r.h],
        vis: [vis.x, vis.y, vis.w, vis.h],
        vw: TOP.innerWidth,
        vh: TOP.innerHeight,
        visible: isVisible(el),
        disabled: !!node.disabled,
        readonly: !!node.readonly,
        editable: editable && !node.readonly,
        isSelect: tagOf(c) === "SELECT",
        multiple: tagOf(c) === "SELECT" && !!c.multiple,
        checkable: t === "checkbox" || t === "radio" || node.checked !== undefined,
        checked: node.checked === true ? true : node.checked === "mixed" ? "mixed" : false,
        value: node.value === undefined ? null : node.value,
        focused: !!node.focused,
        mac: /Mac|iPhone|iPad/.test(navigator.platform || "")
      };
    }

    function describe(el) {
      if (!el) return "nothing";
      var s = tagOf(el).toLowerCase();
      if (el.id) s += "#" + el.id;
      var cls = typeof el.className === "string" ? el.className.trim().split(/\s+/).filter(Boolean).slice(0, 2) : [];
      if (cls.length) s += "." + cls.join(".");
      var ref = el.getAttribute && el.getAttribute(ATTR);
      var role = roleOf(el);
      var name = cap(accName(el, role || "clickable"), 50);
      if (name) s += " " + JSON.stringify(name);
      if (ref) s += " [" + ref + "]";
      return s;
    }

    function matchScore(name, needle) {
      var n = lower(name);
      if (!n || !needle) return 0;
      if (n === needle) return 3;
      if (n.indexOf(needle) === 0) return 2;
      if (n.indexOf(needle) >= 0) return 1;
      return 0;
    }

    function locateByText(text) {
      var needle = lower(text);
      if (!needle) return { error: "empty text" };
      var res = collect({ max: 5000 });
      var best = 0, hits = [];
      res.nodes.forEach(function (n) {
        if (CONTAINERS[n.role]) return;
        var s = matchScore(n.name, needle);
        if (!s) return;
        var interactive = INTERACTIVE[n.role] ? 1 : 0;
        var score = s * 2 + interactive;
        if (score > best) {
          best = score;
          hits = [n];
        } else if (score === best) {
          hits.push(n);
        }
      });
      if (!hits.length) {
        // Fall back to any visible text node: target the closest
        // interactive ancestor, or the smallest element holding the text.
        var found = null;
        forEachRoot(function (root) {
          var doc = root.ownerDocument || root;
          var tw = doc.createTreeWalker(root, NodeFilter.SHOW_TEXT);
          for (var t = tw.nextNode(); t; t = tw.nextNode()) {
            if (lower(t.nodeValue).indexOf(needle) < 0) continue;
            var p = t.parentElement;
            if (!p || SKIP_TAGS[tagOf(p)] || !isVisible(p)) continue;
            var act = p.closest("a[href],button,label,summary,[role],[onclick],[tabindex]");
            found = act && isVisible(act) ? act : p;
            return false;
          }
        });
        if (!found) return { error: "no visible element matches text " + JSON.stringify(text) };
        var inf = info(found);
        inf.count = 1;
        return inf;
      }
      // Several equally good matches: an exact single interactive match
      // is required to avoid clicking the wrong one.
      var uniq = [];
      hits.forEach(function (h) {
        if (uniq.indexOf(h.el) < 0) uniq.push(h.el);
      });
      if (uniq.length > 1) {
        return {
          error: uniq.length + " elements match text " + JSON.stringify(text) + "; use a ref instead",
          candidates: uniq.slice(0, 8).map(function (e) {
            var r = ensureRef(e);
            var role = roleOf(e) || "clickable";
            return "[" + r + "] " + role + " " + JSON.stringify(cap(accName(e, role), 60));
          })
        };
      }
      var out = info(uniq[0]);
      out.count = 1;
      return out;
    }

    function locate(target) {
      target = target || {};
      var el = null, count = 1;
      if (target.ref) {
        el = findRef(target.ref);
        if (!el) return { error: staleMsg(target.ref) };
      } else if (target.selector) {
        var list = queryDeep(target.selector);
        if (!list.length) return { error: "no element matches selector " + JSON.stringify(target.selector) };
        var vis = list.filter(isVisible);
        count = list.length;
        el = vis.length ? vis[0] : list[0];
      } else if (target.text) {
        return locateByText(target.text);
      } else {
        return { error: "no target given" };
      }
      var out = info(el);
      out.count = count;
      return out;
    }

    // deepHit returns the innermost element at top-viewport point (x, y),
    // descending into open shadow roots and same-origin iframes.
    function deepHit(x, y) {
      var doc = document, ox = 0, oy = 0, h = null;
      for (var guard = 0; guard < 20; guard++) {
        h = doc.elementFromPoint(x - ox, y - oy);
        for (var i = 0; h && h.shadowRoot && i < 20; i++) {
          var inner = h.shadowRoot.elementFromPoint(x - ox, y - oy);
          if (!inner || inner === h) break;
          h = inner;
        }
        if (h && (tagOf(h) === "IFRAME" || tagOf(h) === "FRAME")) {
          var d = null;
          try {
            d = h.contentDocument;
          } catch (e) {
            d = null;
          }
          if (d) {
            var fo = frameOffset(d.defaultView);
            ox = fo.x;
            oy = fo.y;
            doc = d;
            continue;
          }
        }
        break;
      }
      return h;
    }
    function composedContains(outer, inner) {
      for (var n = inner, i = 0; n && i < 10000; i++) {
        if (n === outer) return true;
        if (n.parentNode) {
          n = n.parentNode;
        } else if (n.host) {
          n = n.host;
        } else {
          var w = n.defaultView;
          var fe = null;
          try {
            fe = w && w.frameElement;
          } catch (e) {
            fe = null;
          }
          n = fe;
        }
      }
      return false;
    }
    function hit(ref, x, y) {
      var el = findRef(ref);
      if (!el) return { error: staleMsg(ref) };
      var h = deepHit(x, y);
      if (!h) return { ok: false, blocker: "nothing (the point is outside the page)" };
      if (composedContains(el, h)) return { ok: true };
      var c = controlOf(el);
      // A click on a control's label activates the control.
      var lab = h.closest && h.closest("label");
      if (lab && (lab === el || lab.control === c)) return { ok: true };
      if (c !== el && composedContains(c, h)) return { ok: true };
      // An element inside our target's label (custom checkbox art).
      if (c.labels) {
        for (var i = 0; i < c.labels.length; i++) if (composedContains(c.labels[i], h)) return { ok: true };
      }
      // An <option> is hit-tested through its <select>.
      if (tagOf(el) === "OPTION" && composedContains(el.closest("select"), h)) return { ok: true };
      // Ancestor of the target hit: the target is transparent to the point
      // (pointer-events:none) — clicking reaches the ancestor, not the target.
      return { ok: false, blocker: describe(h) };
    }

    // ---- value manipulation --------------------------------------------------------

    function setNativeValue(el, v) {
      var proto = tagOf(el) === "TEXTAREA" ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
      var win = el.ownerDocument.defaultView;
      if (win && win !== TOP) {
        proto = tagOf(el) === "TEXTAREA" ? win.HTMLTextAreaElement.prototype : win.HTMLInputElement.prototype;
      }
      var desc = Object.getOwnPropertyDescriptor(proto, "value");
      if (desc && desc.set) desc.set.call(el, v);
      else el.value = v;
    }
    function fire(el, type) {
      var win = el.ownerDocument.defaultView || TOP;
      var ev = type === "input" ? new win.InputEvent("input", { bubbles: true, composed: true, inputType: "deleteContentBackward" }) :
        new win.Event(type, { bubbles: true, composed: true });
      el.dispatchEvent(ev);
    }
    function clearValue(ref) {
      var el = findRef(ref);
      if (!el) return { error: staleMsg(ref) };
      var c = controlOf(el);
      if (tagOf(c) === "INPUT" || tagOf(c) === "TEXTAREA") {
        setNativeValue(c, "");
        fire(c, "input");
        fire(c, "change");
      } else if (c.isContentEditable) {
        c.textContent = "";
        fire(c, "input");
      } else {
        return { error: "element is not editable" };
      }
      return { value: valueOf(c) || "" };
    }
    function selectAll(ref) {
      var el = findRef(ref);
      if (!el) return { error: staleMsg(ref) };
      var c = controlOf(el);
      if ((tagOf(c) === "INPUT" || tagOf(c) === "TEXTAREA") && typeof c.select === "function") {
        try {
          c.select();
          return { ok: true };
        } catch (e) { /* types without selection, e.g. number */ }
      }
      if (c.isContentEditable) {
        var doc = c.ownerDocument;
        var sel = doc.getSelection();
        var range = doc.createRange();
        range.selectNodeContents(c);
        sel.removeAllRanges();
        sel.addRange(range);
        return { ok: true };
      }
      return { ok: false };
    }
    function focusRef(ref) {
      var el = findRef(ref);
      if (!el) return { error: staleMsg(ref) };
      var c = controlOf(el);
      try {
        c.focus({ preventScroll: true });
      } catch (e) {
        c.focus();
      }
      return { focused: deepActive(document) === c };
    }
    function stateOf(ref) {
      var el = findRef(ref);
      if (!el) return { error: staleMsg(ref) };
      var c = controlOf(el);
      var t = inputType(c);
      var raw = null;
      if (tagOf(c) === "INPUT" || tagOf(c) === "TEXTAREA") raw = t === "password" ? null : c.value;
      else if (c.isContentEditable) raw = c.innerText;
      var node = {};
      fillState(node, c, roleOf(c));
      return {
        value: raw === null ? (valueOf(c) || "") : raw,
        masked: t === "password",
        length: tagOf(c) === "INPUT" || tagOf(c) === "TEXTAREA" ? c.value.length : (raw || "").length,
        checked: node.checked === undefined ? null : node.checked,
        expanded: node.expanded === undefined ? null : node.expanded,
        focused: deepActive(document) === c
      };
    }
    function selectOptions(ref, values, by) {
      var el = findRef(ref);
      if (!el) return { error: staleMsg(ref) };
      var c = controlOf(el);
      if (tagOf(c) !== "SELECT") return { custom: true };
      if (c.disabled) return { error: "the select is disabled" };
      var opts = Array.prototype.slice.call(c.options);
      var label = function (o) { return norm(o.label || o.text); };
      var picked = [];
      for (var i = 0; i < values.length; i++) {
        var want = values[i], lw = lower(want), hit = null;
        var tries = by === "value" ? [function (o) { return o.value === want; }] :
          by === "label" ? [function (o) { return label(o) === norm(want); }, function (o) { return lower(label(o)) === lw; }] :
            [function (o) { return label(o) === norm(want); }, function (o) { return o.value === want; },
              function (o) { return lower(label(o)) === lw; }, function (o) { return lower(label(o)).indexOf(lw) >= 0; }];
        for (var k = 0; k < tries.length && !hit; k++) {
          for (var j = 0; j < opts.length; j++) {
            if (!opts[j].disabled && tries[k](opts[j])) {
              hit = opts[j];
              break;
            }
          }
        }
        if (!hit) {
          return {
            error: "no option matching " + JSON.stringify(want) + (by && by !== "auto" ? " by " + by : "") +
              "; options are: " + opts.slice(0, 30).map(function (o) { return JSON.stringify(label(o)) + (o.value !== label(o) ? " (value " + JSON.stringify(o.value) + ")" : ""); }).join(", ")
          };
        }
        picked.push(hit);
      }
      if (!c.multiple && picked.length > 1) return { error: "the select allows one option; got " + picked.length };
      if (c.multiple) {
        opts.forEach(function (o) { o.selected = picked.indexOf(o) >= 0; });
      } else {
        c.value = picked[0].value;
        picked[0].selected = true;
      }
      try {
        c.focus({ preventScroll: true });
      } catch (e) { /* ignore */ }
      fire(c, "input");
      fire(c, "change");
      return { selected: picked.map(label) };
    }
    function findOption(text) {
      var needle = lower(text);
      var res = collect({ max: 5000 });
      var cands = [];
      res.nodes.forEach(function (n) {
        if (n.role === "option" || n.role === "menuitem" || n.role === "menuitemradio" || n.role === "menuitemcheckbox" || n.role === "treeitem" || n.role === "gridcell") {
          cands.push(n);
        }
      });
      if (!cands.length) {
        // Plain list items inside a listbox-ish popup.
        forEachRoot(function (root) {
          var list = root.querySelectorAll("[role=listbox] li, [role=menu] li, ul[aria-expanded] li, [class*=option], [class*=Option]");
          for (var i = 0; i < list.length; i++) {
            if (isVisible(list[i])) cands.push({ el: list[i], name: norm(textOf(list[i], 0)), role: "option" });
          }
        });
      }
      var best = null, bestScore = 0;
      cands.forEach(function (n) {
        var s = matchScore(n.name, needle);
        if (s > bestScore) {
          bestScore = s;
          best = n;
        }
      });
      if (!best) {
        return {
          error: "no visible option matching " + JSON.stringify(text) +
            (cands.length ? "; visible options: " + cands.slice(0, 20).map(function (n) { return JSON.stringify(n.name); }).join(", ") : "; no option list is open")
        };
      }
      var out = info(best.el);
      out.count = 1;
      return out;
    }

    // ---- text ------------------------------------------------------------------------

    function readable(el) {
      var t = "";
      try {
        t = el.innerText || "";
      } catch (e) {
        t = el.textContent || "";
      }
      // innerText does not include iframe documents; append them.
      var frames = el.querySelectorAll ? el.querySelectorAll("iframe,frame") : [];
      for (var i = 0; i < frames.length; i++) {
        var d = null;
        try {
          d = frames[i].contentDocument;
        } catch (e) {
          d = null;
        }
        if (d && d.body) t += "\n\n" + readable(d.body);
      }
      // innerText does not see into shadow roots either.
      var all = el.querySelectorAll ? el.querySelectorAll("*") : [];
      for (var j = 0; j < all.length; j++) {
        if (all[j].shadowRoot && isVisible(all[j])) {
          var st = norm(textOf(all[j], 0));
          if (st && t.indexOf(st) < 0) t += "\n" + st;
        }
      }
      return t;
    }
    function extract(ref, max) {
      var el = ref ? findRef(ref) : document.body || document.documentElement;
      if (!el) return { error: ref ? staleMsg(ref) : "the page has no body" };
      var text = readable(el).replace(/[ \t ]+\n/g, "\n").replace(/\n{3,}/g, "\n\n").replace(/[ \t]{2,}/g, " ").trim();
      max = max || 20000;
      return { text: text.slice(0, max), total: text.length, truncated: text.length > max, url: String(location.href), title: document.title };
    }
    function probe(kind, value) {
      switch (kind) {
        case "text":
        case "text_gone":
          var has = lower(readable(document.body || document.documentElement)).indexOf(lower(value)) >= 0;
          return { ok: kind === "text" ? has : !has };
        case "ref":
          var el = findRef(value);
          return { ok: !!el && isVisible(el) };
        case "selector":
          return { ok: queryDeep(value).some(isVisible) };
      }
      return { error: "unknown probe " + kind };
    }

    state.api = {
      snapshot: function (opts) {
        opts = opts || {};
        var res = collect(opts);
        if (res.error) return res;
        var si = scrollInfo();
        return {
          url: String(location.href),
          title: document.title,
          vw: si.vw, vh: si.vh, sx: si.sx, sy: si.sy, sw: si.sw, sh: si.sh,
          nodes: res.nodes.map(serialize),
          total: res.total,
          truncated: res.total > res.nodes.length,
          crossOriginFrames: res.crossOrigin
        };
      },
      locate: locate,
      findOption: findOption,
      hit: hit,
      scrollIntoView: function (ref) {
        var el = findRef(ref);
        if (!el) return { error: staleMsg(ref) };
        el.scrollIntoView({ block: "center", inline: "center", behavior: "instant" });
        var out = info(el);
        out.count = 1;
        return out;
      },
      scrollInfo: scrollInfo,
      state: stateOf,
      focus: focusRef,
      selectAll: selectAll,
      clear: clearValue,
      select: selectOptions,
      extract: extract,
      probe: probe,
      page: function () {
        return { url: String(location.href), title: document.title, ready: document.readyState };
      }
    };
    return state;
  }
})
