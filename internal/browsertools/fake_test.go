package browsertools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// fakeEl is one element of the simulated page. Its box is in document
// coordinates; the viewport shows it shifted by the scroll position.
type fakeEl struct {
	ref, role, name, tag, typ string
	selector                  string
	box                       Rect
	value                     string
	checked                   Tri
	disabled                  bool
	editable                  bool
	isSelect                  bool
	multiple                  bool
	options                   [][2]string // label, value
	blocker                   string      // hit tests on it report this blocker
	hidden                    bool
	text                      string
	depth                     int
	// focusTo, when set, sends a click's focus to another element.
	noFocus bool
	onClick func(f *fakeDriver)
}

// fakeDriver simulates a page well enough to drive the session end to end:
// it records input events and answers the page script's methods in Go.
type fakeDriver struct {
	mu        sync.Mutex
	url       string
	title     string
	vw, vh    int
	sx, sy    float64
	sh        float64
	els       []*fakeEl
	focused   *fakeEl
	selectAll bool
	mods      map[string]bool
	events    []string
	moves     []Point
	wheels    [][2]float64
	inserts   []string
	evals     []string
	shot      []byte
	idle      int
	onEnter   func(f *fakeDriver)
	rawEval   func(expr string) (json.RawMessage, error)
	navErr    error
	pageText  string
	navigated []string
	evalHook  func(method string) // runs before each script method
}

func newFakePage() *fakeDriver {
	return &fakeDriver{url: "https://example.test/", title: "Example", vw: 1000, vh: 600, sh: 600, mods: map[string]bool{}}
}

func (f *fakeDriver) add(e *fakeEl) *fakeEl {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e.tag == "" {
		e.tag = "div"
	}
	f.els = append(f.els, e)
	if bottom := e.box.Y + e.box.H + 50; bottom > f.sh {
		f.sh = bottom
	}
	return e
}

func (f *fakeDriver) find(ref string) *fakeEl {
	for _, e := range f.els {
		if e.ref == ref && !e.hidden {
			return e
		}
	}
	return nil
}

func (f *fakeDriver) log(format string, a ...any) {
	f.events = append(f.events, fmt.Sprintf(format, a...))
}

func (f *fakeDriver) Events() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

func (f *fakeDriver) viewBox(e *fakeEl) Rect {
	return Rect{e.box.X - f.sx, e.box.Y - f.sy, e.box.W, e.box.H}
}

func (f *fakeDriver) Navigate(ctx context.Context, url string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.navErr != nil {
		return f.navErr
	}
	f.navigated = append(f.navigated, url)
	f.url = url
	f.log("navigate %s", url)
	return nil
}
func (f *fakeDriver) Reload(ctx context.Context) error {
	f.mu.Lock()
	f.log("reload")
	f.mu.Unlock()
	return nil
}
func (f *fakeDriver) Back(ctx context.Context) error {
	f.mu.Lock()
	f.log("back")
	f.mu.Unlock()
	return nil
}
func (f *fakeDriver) Forward(ctx context.Context) error {
	f.mu.Lock()
	f.log("forward")
	f.mu.Unlock()
	return nil
}
func (f *fakeDriver) URL() string { f.mu.Lock(); defer f.mu.Unlock(); return f.url }
func (f *fakeDriver) Title(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.title, nil
}
func (f *fakeDriver) Screenshot(ctx context.Context, format string, quality int) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("screenshot %s %d", format, quality)
	return f.shot, nil
}
func (f *fakeDriver) Viewport() (int, int) { return f.vw, f.vh }

func (f *fakeDriver) MouseMove(ctx context.Context, x, y float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.moves = append(f.moves, Point{x, y})
	f.log("move")
	return nil
}

func (f *fakeDriver) MouseDown(ctx context.Context, x, y float64, button string, n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("down %s %d", button, n)
	return nil
}

func (f *fakeDriver) MouseUp(ctx context.Context, x, y float64, button string, n int) error {
	f.mu.Lock()
	f.log("up %s %d", button, n)
	var hit *fakeEl
	for i := len(f.els) - 1; i >= 0; i-- {
		e := f.els[i]
		if !e.hidden && f.viewBox(e).Contains(Point{x, y}) {
			hit = e
			break
		}
	}
	var cb func(*fakeDriver)
	if hit != nil && button == "left" && n == 1 && !hit.disabled {
		if !hit.noFocus {
			f.focused = hit
			f.selectAll = false
		}
		switch hit.checked {
		case "true":
			if hit.typ != "radio" {
				hit.checked = "false"
			}
		case "false":
			hit.checked = "true"
		}
		cb = hit.onClick
	}
	f.mu.Unlock()
	if cb != nil {
		cb(f)
	}
	return nil
}

func (f *fakeDriver) Wheel(ctx context.Context, x, y, dx, dy float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wheels = append(f.wheels, [2]float64{dx, dy})
	f.sy = min(max(f.sy+dy, 0), max(f.sh-float64(f.vh), 0))
	f.log("wheel %.0f", dy)
	return nil
}

func (f *fakeDriver) KeyDown(ctx context.Context, key string) error {
	f.mu.Lock()
	f.log("keydown %s", key)
	switch key {
	case "Control", "Meta", "Shift", "Alt":
		f.mods[key] = true
		f.mu.Unlock()
		return nil
	}
	var cb func(*fakeDriver)
	if fe := f.focused; fe != nil && fe.editable {
		switch {
		case key == "a" && (f.mods["Control"] || f.mods["Meta"]):
			f.selectAll = true
		case key == "Backspace":
			if f.selectAll {
				fe.value = ""
				f.selectAll = false
			} else if fe.value != "" {
				_, n := utf8.DecodeLastRuneInString(fe.value)
				fe.value = fe.value[:len(fe.value)-n]
			}
		}
	}
	if key == "Enter" {
		cb = f.onEnter
	}
	f.mu.Unlock()
	if cb != nil {
		cb(f)
	}
	return nil
}

func (f *fakeDriver) KeyUp(ctx context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("keyup %s", key)
	delete(f.mods, key)
	return nil
}

func (f *fakeDriver) InsertText(ctx context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("insert %s", text)
	f.inserts = append(f.inserts, text)
	if fe := f.focused; fe != nil && fe.editable {
		if f.selectAll {
			fe.value = ""
			f.selectAll = false
		}
		fe.value += text
	}
	return nil
}

func (f *fakeDriver) WaitIdle(ctx context.Context, quiet time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.idle++
	return ctx.Err()
}

// Evaluate answers page-script calls; other expressions go to rawEval.
func (f *fakeDriver) Evaluate(ctx context.Context, expr string) (json.RawMessage, error) {
	if !strings.HasPrefix(expr, agentJS) {
		f.mu.Lock()
		f.evals = append(f.evals, expr)
		raw := f.rawEval
		f.mu.Unlock()
		if raw == nil {
			return json.RawMessage(`null`), nil
		}
		return raw(expr)
	}
	call := strings.TrimSuffix(strings.TrimPrefix(expr, agentJS+"("), ")")
	var parts []json.RawMessage
	if err := json.Unmarshal([]byte("["+call+"]"), &parts); err != nil || len(parts) != 2 {
		return nil, fmt.Errorf("fake: bad agent call %q: %v", call, err)
	}
	var method string
	var args []json.RawMessage
	if err := json.Unmarshal(parts[0], &method); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(parts[1], &args); err != nil {
		return nil, err
	}
	f.mu.Lock()
	hook := f.evalHook
	f.mu.Unlock()
	if hook != nil {
		hook(method)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.evals = append(f.evals, method)
	out, err := f.agent(method, args)
	if err != nil {
		return nil, err
	}
	return json.Marshal(out)
}

func str(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

func (f *fakeDriver) info(e *fakeEl) map[string]any {
	vb := f.viewBox(e)
	vis := intersectRect(vb, Rect{0, 0, float64(f.vw), float64(f.vh)})
	var value any
	if e.editable || e.isSelect {
		value = e.value
	}
	var checked any = false
	if e.checked == "true" {
		checked = true
	}
	return map[string]any{
		"ref": e.ref, "role": e.role, "name": e.name, "tag": e.tag, "type": e.typ,
		"box": []float64{vb.X, vb.Y, vb.W, vb.H}, "vis": []float64{vis.X, vis.Y, vis.W, vis.H},
		"vw": f.vw, "vh": f.vh, "visible": !e.hidden, "disabled": e.disabled, "readonly": false,
		"editable": e.editable, "isSelect": e.isSelect, "multiple": e.multiple,
		"checkable": e.checked != "", "checked": checked, "value": value,
		"focused": f.focused == e, "mac": false, "count": 1,
	}
}

func intersectRect(a, b Rect) Rect {
	x1, y1 := max(a.X, b.X), max(a.Y, b.Y)
	x2, y2 := min(a.X+a.W, b.X+b.W), min(a.Y+a.H, b.Y+b.H)
	return Rect{x1, y1, max(0, x2-x1), max(0, y2-y1)}
}

func stale(ref string) map[string]any {
	return map[string]any{"error": "ref " + ref + " is not on the page any more (the page changed); call browser_snapshot for fresh refs"}
}

func (f *fakeDriver) agent(method string, args []json.RawMessage) (any, error) {
	arg := func(i int) json.RawMessage {
		if i < len(args) {
			return args[i]
		}
		return json.RawMessage("null")
	}
	switch method {
	case "snapshot":
		var o struct {
			Scope string `json:"scope"`
		}
		_ = json.Unmarshal(arg(0), &o)
		var nodes []map[string]any
		for _, e := range f.els {
			if e.hidden || e.role == "" || (o.Scope != "" && e.ref != o.Scope) {
				continue
			}
			vb := f.viewBox(e)
			vis := intersectRect(vb, Rect{0, 0, float64(f.vw), float64(f.vh)})
			n := map[string]any{"ref": e.ref, "role": e.role, "name": e.name, "depth": e.depth,
				"box": []float64{vb.X, vb.Y, vb.W, vb.H}, "inView": vis.Area() > 0}
			if e.editable || e.isSelect {
				n["value"] = e.value
			}
			if e.checked != "" {
				n["checked"] = e.checked == "true"
			}
			if e.disabled {
				n["disabled"] = true
			}
			if f.focused == e {
				n["focused"] = true
			}
			nodes = append(nodes, n)
		}
		return map[string]any{"url": f.url, "title": f.title, "vw": f.vw, "vh": f.vh, "sx": f.sx, "sy": f.sy,
			"sw": f.vw, "sh": f.sh, "nodes": nodes, "total": len(nodes), "truncated": false}, nil
	case "locate":
		var t struct{ Ref, Selector, Text string }
		_ = json.Unmarshal(arg(0), &t)
		switch {
		case t.Ref != "":
			e := f.find(t.Ref)
			if e == nil {
				return stale(t.Ref), nil
			}
			return f.info(e), nil
		case t.Selector != "":
			var hits []*fakeEl
			for _, e := range f.els {
				if e.selector == t.Selector && !e.hidden {
					hits = append(hits, e)
				}
			}
			if len(hits) == 0 {
				return map[string]any{"error": "no element matches selector " + quote(t.Selector)}, nil
			}
			m := f.info(hits[0])
			m["count"] = len(hits)
			return m, nil
		default:
			var hits []*fakeEl
			for _, e := range f.els {
				if !e.hidden && strings.EqualFold(e.name, t.Text) {
					hits = append(hits, e)
				}
			}
			if len(hits) == 0 {
				return map[string]any{"error": "no visible element matches text " + quote(t.Text)}, nil
			}
			if len(hits) > 1 {
				var c []string
				for _, h := range hits {
					c = append(c, "["+h.ref+"] "+h.role+" "+quote(h.name))
				}
				return map[string]any{"error": fmt.Sprintf("%d elements match text %s; use a ref instead", len(hits), quote(t.Text)), "candidates": c}, nil
			}
			return f.info(hits[0]), nil
		}
	case "hit":
		ref := str(arg(0))
		var x, y float64
		_ = json.Unmarshal(arg(1), &x)
		_ = json.Unmarshal(arg(2), &y)
		e := f.find(ref)
		if e == nil {
			return stale(ref), nil
		}
		if e.blocker != "" {
			return map[string]any{"ok": false, "blocker": e.blocker}, nil
		}
		if !f.viewBox(e).Contains(Point{x, y}) {
			return map[string]any{"ok": false, "blocker": "body"}, nil
		}
		return map[string]any{"ok": true}, nil
	case "scrollIntoView":
		ref := str(arg(0))
		e := f.find(ref)
		if e == nil {
			return stale(ref), nil
		}
		f.sy = min(max(e.box.Y+e.box.H/2-float64(f.vh)/2, 0), max(f.sh-float64(f.vh), 0))
		f.log("scrollIntoView %s", ref)
		return f.info(e), nil
	case "scrollInfo":
		return map[string]any{"sx": f.sx, "sy": f.sy, "sw": f.vw, "sh": f.sh, "vw": f.vw, "vh": f.vh}, nil
	case "state":
		ref := str(arg(0))
		e := f.find(ref)
		if e == nil {
			return stale(ref), nil
		}
		var checked any
		if e.checked != "" {
			checked = e.checked == "true"
		}
		return map[string]any{"value": e.value, "masked": e.typ == "password", "length": utf8.RuneCountInString(e.value),
			"checked": checked, "focused": f.focused == e}, nil
	case "focus":
		e := f.find(str(arg(0)))
		if e == nil {
			return stale(str(arg(0))), nil
		}
		f.focused = e
		return map[string]any{"focused": true}, nil
	case "selectAll":
		f.selectAll = true
		return map[string]any{"ok": true}, nil
	case "clear":
		e := f.find(str(arg(0)))
		if e == nil {
			return stale(str(arg(0))), nil
		}
		e.value = ""
		return map[string]any{"value": ""}, nil
	case "select":
		e := f.find(str(arg(0)))
		if e == nil {
			return stale(str(arg(0))), nil
		}
		if !e.isSelect {
			return map[string]any{"custom": true}, nil
		}
		var vals []string
		_ = json.Unmarshal(arg(1), &vals)
		by := str(arg(2))
		var picked []string
		for _, v := range vals {
			found := ""
			for _, o := range e.options {
				if (by != "value" && strings.EqualFold(o[0], v)) || (by != "label" && o[1] == v) {
					found = o[0]
					break
				}
			}
			if found == "" {
				return map[string]any{"error": "no option matching " + quote(v)}, nil
			}
			picked = append(picked, found)
		}
		e.value = strings.Join(picked, ", ")
		return map[string]any{"selected": picked}, nil
	case "findOption":
		text := str(arg(0))
		for _, e := range f.els {
			if !e.hidden && e.role == "option" && strings.Contains(strings.ToLower(e.name), strings.ToLower(text)) {
				return f.info(e), nil
			}
		}
		return map[string]any{"error": "no visible option matching " + quote(text)}, nil
	case "extract":
		var ref string
		_ = json.Unmarshal(arg(0), &ref)
		var max int
		_ = json.Unmarshal(arg(1), &max)
		text := f.pageText
		if ref != "" {
			e := f.find(ref)
			if e == nil {
				return stale(ref), nil
			}
			text = e.text
		}
		total := len(text)
		trunc := false
		if max > 0 && len(text) > max {
			text, trunc = text[:max], true
		}
		return map[string]any{"text": text, "total": total, "truncated": trunc, "url": f.url, "title": f.title}, nil
	case "probe":
		kind, value := str(arg(0)), str(arg(1))
		switch kind {
		case "text":
			return map[string]any{"ok": strings.Contains(strings.ToLower(f.pageText), strings.ToLower(value))}, nil
		case "text_gone":
			return map[string]any{"ok": !strings.Contains(strings.ToLower(f.pageText), strings.ToLower(value))}, nil
		case "ref":
			return map[string]any{"ok": f.find(value) != nil}, nil
		case "selector":
			for _, e := range f.els {
				if e.selector == value && !e.hidden {
					return map[string]any{"ok": true}, nil
				}
			}
			return map[string]any{"ok": false}, nil
		}
	}
	return map[string]any{"error": "unknown agent method: " + method}, nil
}

// fakeClock is a manual clock whose Sleep advances time instantly.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	return nil
}

func (c *fakeClock) Sleeps() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.sleeps...)
}

// newTestSession returns a session on f with a fake clock.
func newTestSession(f *fakeDriver, speed Speed) (*Session, *fakeClock) {
	clk := newFakeClock()
	s := NewSession(func(context.Context) (Driver, error) { return f, nil }, Options{
		Speed: speed, Seed: 42, Sleep: clk.Sleep, Now: clk.Now,
	})
	return s, clk
}

// loginPage builds a small sign-in form.
func loginPage() *fakeDriver {
	f := newFakePage()
	f.title = "Sign in"
	f.add(&fakeEl{ref: "e1", role: "heading", name: "Sign in", tag: "h1", box: Rect{20, 20, 300, 40}})
	f.add(&fakeEl{ref: "e2", role: "textbox", name: "Email", tag: "input", typ: "email", selector: "#email", box: Rect{20, 100, 300, 30}, editable: true})
	f.add(&fakeEl{ref: "e3", role: "textbox", name: "Password", tag: "input", typ: "password", selector: "#pw", box: Rect{20, 150, 300, 30}, editable: true})
	f.add(&fakeEl{ref: "e4", role: "checkbox", name: "Remember me", tag: "input", typ: "checkbox", box: Rect{20, 200, 20, 20}, checked: "false"})
	f.add(&fakeEl{ref: "e5", role: "combobox", name: "Country", tag: "select", box: Rect{20, 240, 200, 30}, isSelect: true,
		options: [][2]string{{"United States", "us"}, {"Germany", "de"}}, value: "United States"})
	f.add(&fakeEl{ref: "e6", role: "button", name: "Sign in", tag: "button", selector: "button[type=submit]", box: Rect{20, 290, 120, 36}})
	return f
}
