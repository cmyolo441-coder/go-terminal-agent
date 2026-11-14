package browsertools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // screenshot dimensions
	_ "image/png"  // screenshot dimensions
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

// Limits on tool input.
const (
	maxTypeChars    = 20000
	maxFillFields   = 50
	maxSelectValues = 50
	maxRepeat       = 100
	maxScrollAmount = 100000
	maxWaitTimeout  = 120 * time.Second
	maxWaitDuration = 30 * time.Second
	maxExtractChars = 100000
	defExtractChars = 8000
	maxEvalChars    = 100000
	maxEvalOutput   = 20000
	minSnapshotCap  = 500
	maxSnapshotCap  = 100000
)

// Tools returns the browser tools bound to s.
func Tools(s *Session) []tools.Tool {
	ts := []tools.Tool{
		navigateTool(s), snapshotTool(s), clickTool(s), fillTool(s), typeTool(s), pressTool(s),
		selectTool(s), checkTool(s), hoverTool(s), scrollTool(s), waitTool(s), screenshotTool(s),
		extractTool(s), historyTool(s, "browser_back"), historyTool(s, "browser_forward"),
		historyTool(s, "browser_reload"),
	}
	if !s.opts.DisableEval {
		ts = append(ts, evalTool(s))
	}
	return ts
}

// tool is a browser tool defined by its spec and a prepare function.
type tool struct {
	spec    llm.ToolSpec
	kind    tools.Kind
	prepare func(env *tools.Env, raw json.RawMessage) (*tools.Call, error)
}

func (t *tool) Spec() llm.ToolSpec { return t.spec }
func (t *tool) Kind() tools.Kind   { return t.kind }
func (t *tool) Prepare(env *tools.Env, raw json.RawMessage) (*tools.Call, error) {
	return t.prepare(env, raw)
}

// decode strictly decodes tool input: unknown fields and trailing data are
// rejected so a truncated or malformed eager stream never runs.
func decode(input json.RawMessage, v any) error {
	if len(bytes.TrimSpace(input)) == 0 {
		input = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid tool input: %w", err)
	}
	if dec.More() {
		return errors.New("invalid tool input: trailing data")
	}
	return nil
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("invalid tool input: "+format, a...)
}

// targetInput is the element-targeting part of a tool input.
type targetInput struct {
	Ref       string `json:"ref"`
	Selector  string `json:"selector"`
	MatchText string `json:"match_text"`
}

func (t targetInput) target() Target {
	return Target{Ref: strings.TrimSpace(t.Ref), Selector: t.Selector, Text: t.MatchText}
}

func (t targetInput) required() (Target, error) {
	tg := t.target()
	if err := tg.Validate(); err != nil {
		return Target{}, invalid("%v", err)
	}
	return tg, nil
}

func (t targetInput) optional() (Target, error) {
	tg := t.target()
	if tg.IsZero() {
		return tg, nil
	}
	return t.required()
}

func targetProps(extra map[string]any, what string) map[string]any {
	p := map[string]any{
		"ref":        map[string]any{"type": "string", "pattern": "^e[0-9]+$", "description": "Element ref from the latest browser_snapshot, e.g. \"e12\". Preferred."},
		"selector":   map[string]any{"type": "string", "description": "CSS selector; the first visible match " + what + " (pierces open shadow roots and same-origin frames)."},
		"match_text": map[string]any{"type": "string", "description": "Visible text or accessible name of the element; must match a single element."},
	}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

const targetHelp = " Target the element with exactly one of ref (from browser_snapshot), selector or match_text."

func newCall(name string, kind tools.Kind, summary, preview string, run func(ctx context.Context) (string, error)) *tools.Call {
	return tools.NewCall(name, kind, summary, preview, "", run)
}

// ---- browser_navigate --------------------------------------------------------

func navigateTool(s *Session) tools.Tool {
	return &tool{
		kind: tools.KindExec,
		spec: llm.ToolSpec{
			Name: "browser_navigate",
			Description: "Open a URL in the browser and return the page outline (elements with refs such as [e12] that other browser tools target). " +
				"Allowed: http(s) URLs, about:blank, and files inside the workspace (a path or file:// URL). A bare host gets https://.",
			Properties: map[string]any{
				"url": map[string]any{"type": "string", "description": "The URL to open."},
			},
			Required: []string{"url"},
		},
		prepare: func(env *tools.Env, raw json.RawMessage) (*tools.Call, error) {
			var in struct {
				URL string `json:"url"`
			}
			if err := decode(raw, &in); err != nil {
				return nil, err
			}
			u, err := CheckURL(in.URL, env.Root)
			if err != nil {
				return nil, invalid("%v", err)
			}
			return newCall("browser_navigate", tools.KindExec, "Open "+u, u, func(ctx context.Context) (string, error) {
				return s.Navigate(ctx, u)
			}), nil
		},
	}
}

// ---- browser_snapshot --------------------------------------------------------

func snapshotTool(s *Session) tools.Tool {
	return &tool{
		kind: tools.KindRead,
		spec: llm.ToolSpec{
			Name: "browser_snapshot",
			Description: "Describe the current page as an indented outline of its visible interactive and semantic elements (links, buttons, fields, " +
				"checkboxes, headings, dialogs...), each with a ref like [e12], its accessible name, state and value. " +
				"Refs stay stable while the element exists. Take a snapshot before acting on a page you have not seen.",
			Properties: map[string]any{
				"ref":           map[string]any{"type": "string", "pattern": "^e[0-9]+$", "description": "Limit the outline to this element's subtree, e.g. a form or dialog."},
				"viewport_only": map[string]any{"type": "boolean", "description": "Only include elements currently on screen."},
				"max_chars":     map[string]any{"type": "integer", "minimum": minSnapshotCap, "maximum": maxSnapshotCap, "description": "Cap on the outline length."},
			},
		},
		prepare: func(env *tools.Env, raw json.RawMessage) (*tools.Call, error) {
			var in struct {
				Ref          string `json:"ref"`
				ViewportOnly bool   `json:"viewport_only"`
				MaxChars     int    `json:"max_chars"`
			}
			if err := decode(raw, &in); err != nil {
				return nil, err
			}
			if in.Ref != "" && !refPattern.MatchString(in.Ref) {
				return nil, invalid("ref %q is not a snapshot ref like e12", in.Ref)
			}
			if in.MaxChars != 0 && (in.MaxChars < minSnapshotCap || in.MaxChars > maxSnapshotCap) {
				return nil, invalid("max_chars must be between %d and %d", minSnapshotCap, maxSnapshotCap)
			}
			limit := in.MaxChars
			if limit == 0 {
				limit = s.opts.MaxSnapshotChars
			}
			summary := "Snapshot the page"
			if in.Ref != "" {
				summary = "Snapshot " + in.Ref
			}
			return newCall("browser_snapshot", tools.KindRead, summary, "", func(ctx context.Context) (string, error) {
				snap, err := s.Snapshot(ctx, SnapshotOptions{Scope: in.Ref, ViewportOnly: in.ViewportOnly})
				if err != nil {
					return "", err
				}
				return snap.Render(limit), nil
			}), nil
		},
	}
}

// ---- browser_click -------------------------------------------------------------

func clickTool(s *Session) tools.Tool {
	return &tool{
		kind: tools.KindExec,
		spec: llm.ToolSpec{
			Name: "browser_click",
			Description: "Click an element the way a person would: scroll it into view, move the pointer onto it and click. " +
				"Fails with a clear message if something (a dialog, cookie banner...) covers it. Returns how the page changed." + targetHelp,
			Properties: targetProps(map[string]any{
				"button":      map[string]any{"type": "string", "enum": []string{"left", "right", "middle"}, "description": "Mouse button (default left)."},
				"click_count": map[string]any{"type": "integer", "minimum": 1, "maximum": 3, "description": "2 for a double click, 3 for a triple click."},
				"modifiers":   map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": []string{"Control", "Shift", "Alt", "Meta"}}, "description": "Keys held during the click."},
			}, "is clicked"),
		},
		prepare: func(env *tools.Env, raw json.RawMessage) (*tools.Call, error) {
			var in struct {
				targetInput
				Button     string   `json:"button"`
				ClickCount int      `json:"click_count"`
				Modifiers  []string `json:"modifiers"`
			}
			if err := decode(raw, &in); err != nil {
				return nil, err
			}
			t, err := in.required()
			if err != nil {
				return nil, err
			}
			switch in.Button {
			case "", "left", "right", "middle":
			default:
				return nil, invalid("button must be left, right or middle")
			}
			if in.ClickCount < 0 || in.ClickCount > 3 {
				return nil, invalid("click_count must be 1, 2 or 3")
			}
			mods, err := parseModifiers(in.Modifiers)
			if err != nil {
				return nil, err
			}
			o := ClickOptions{Button: in.Button, Count: in.ClickCount, Modifiers: mods}
			return newCall("browser_click", tools.KindExec, "Click "+t.String(), "", func(ctx context.Context) (string, error) {
				return s.Click(ctx, t, o)
			}), nil
		},
	}
}

func parseModifiers(in []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, m := range in {
		k, ok := modifierKeys[strings.ToLower(strings.TrimSpace(m))]
		if !ok {
			return nil, invalid("unknown modifier %q (use Control, Shift, Alt or Meta)", m)
		}
		if seen[k] {
			return nil, invalid("modifier %s given twice", k)
		}
		seen[k] = true
		out = append(out, k)
	}
	return out, nil
}

// ---- browser_fill ----------------------------------------------------------------

func fillTool(s *Session) tools.Tool {
	field := map[string]any{
		"type": "object",
		"properties": targetProps(map[string]any{
			"value":  map[string]any{"type": "string", "description": "Text to enter; for a checkbox or switch \"true\"/\"false\"; for a select the option label or value."},
			"append": map[string]any{"type": "boolean", "description": "Keep the field's existing text and append (default: replace it)."},
		}, "is filled"),
		"required":             []string{"value"},
		"additionalProperties": false,
	}
	return &tool{
		kind: tools.KindExec,
		spec: llm.ToolSpec{
			Name: "browser_fill",
			Description: "Fill several form fields in one call, in order, like a person: click each field, replace its text and type the value. " +
				"Text fields, textareas, contenteditable areas, selects (by option label or value) and checkboxes (true/false) are supported. " +
				"Each field is targeted by ref, selector or match_text (its label). Returns each field's resulting value and how the page changed.",
			Properties: map[string]any{
				"fields": map[string]any{"type": "array", "minItems": 1, "maxItems": maxFillFields, "items": field, "description": "Fields to fill, in order."},
				"submit": map[string]any{"type": "boolean", "description": "Press Enter after the last field to submit the form."},
			},
			Required: []string{"fields"},
		},
		prepare: func(env *tools.Env, raw json.RawMessage) (*tools.Call, error) {
			var in struct {
				Fields []struct {
					targetInput
					Value  *string `json:"value"`
					Append bool    `json:"append"`
				} `json:"fields"`
				Submit bool `json:"submit"`
			}
			if err := decode(raw, &in); err != nil {
				return nil, err
			}
			if len(in.Fields) == 0 {
				return nil, invalid("\"fields\" needs at least one field")
			}
			if len(in.Fields) > maxFillFields {
				return nil, invalid("at most %d fields per call", maxFillFields)
			}
			fields := make([]FillField, 0, len(in.Fields))
			var names []string
			for i, f := range in.Fields {
				t, err := f.required()
				if err != nil {
					return nil, fmt.Errorf("field %d: %w", i+1, err)
				}
				if f.Value == nil {
					return nil, invalid("field %d: \"value\" is required", i+1)
				}
				if utf8.RuneCountInString(*f.Value) > maxTypeChars {
					return nil, invalid("field %d: value is longer than %d characters", i+1, maxTypeChars)
				}
				fields = append(fields, FillField{Target: t, Value: *f.Value, NoClear: f.Append})
				names = append(names, t.String())
			}
			summary := fmt.Sprintf("Fill %d field(s): %s", len(fields), strings.Join(names, ", "))
			return newCall("browser_fill", tools.KindExec, summary, "", func(ctx context.Context) (string, error) {
				return s.Fill(ctx, fields, in.Submit)
			}), nil
		},
	}
}

// ---- browser_type ------------------------------------------------------------------

func typeTool(s *Session) tools.Tool {
	return &tool{
		kind: tools.KindExec,
		spec: llm.ToolSpec{
			Name: "browser_type",
			Description: "Type text with the keyboard. With a target, click it first (and optionally clear it); without one, type into whatever has focus. " +
				"Newlines press Enter and tabs press Tab. Returns the field's resulting value and how the page changed.",
			Properties: targetProps(map[string]any{
				"text":   map[string]any{"type": "string", "description": "The text to type."},
				"clear":  map[string]any{"type": "boolean", "description": "Select and delete the field's existing text first."},
				"submit": map[string]any{"type": "boolean", "description": "Press Enter after typing."},
			}, "is typed into"),
			Required: []string{"text"},
		},
		prepare: func(env *tools.Env, raw json.RawMessage) (*tools.Call, error) {
			var in struct {
				targetInput
				Text   *string `json:"text"`
				Clear  bool    `json:"clear"`
				Submit bool    `json:"submit"`
			}
			if err := decode(raw, &in); err != nil {
				return nil, err
			}
			t, err := in.optional()
			if err != nil {
				return nil, err
			}
			if in.Text == nil || (*in.Text == "" && !in.Submit) {
				return nil, invalid("\"text\" is required")
			}
			if utf8.RuneCountInString(*in.Text) > maxTypeChars {
				return nil, invalid("text is longer than %d characters", maxTypeChars)
			}
			if in.Clear && t.IsZero() {
				return nil, invalid("clear needs a target element")
			}
			text := *in.Text
			where := "the focused element"
			if !t.IsZero() {
				where = t.String()
			}
			summary := fmt.Sprintf("Type %d characters into %s", utf8.RuneCountInString(text), where)
			return newCall("browser_type", tools.KindExec, summary, text, func(ctx context.Context) (string, error) {
				return s.Type(ctx, t, text, TypeOptions{Clear: in.Clear, Submit: in.Submit})
			}), nil
		},
	}
}

// ---- browser_press ---------------------------------------------------------------------

func pressTool(s *Session) tools.Tool {
	return &tool{
		kind: tools.KindExec,
		spec: llm.ToolSpec{
			Name: "browser_press",
			Description: "Press a key or key chord on the focused element, e.g. \"Enter\", \"Escape\", \"Tab\", \"ArrowDown\", \"PageDown\", " +
				"\"Control+A\", \"Shift+Tab\", \"Meta+K\". Returns how the page changed.",
			Properties: map[string]any{
				"keys":   map[string]any{"type": "string", "description": "Key or chord; modifiers joined with + (Control, Shift, Alt, Meta)."},
				"repeat": map[string]any{"type": "integer", "minimum": 1, "maximum": maxRepeat, "description": "How many times to press it (default 1)."},
			},
			Required: []string{"keys"},
		},
		prepare: func(env *tools.Env, raw json.RawMessage) (*tools.Call, error) {
			var in struct {
				Keys   string `json:"keys"`
				Repeat int    `json:"repeat"`
			}
			if err := decode(raw, &in); err != nil {
				return nil, err
			}
			if strings.TrimSpace(in.Keys) == "" {
				return nil, invalid("\"keys\" is required")
			}
			c, err := ParseChord(in.Keys)
			if err != nil {
				return nil, invalid("%v", err)
			}
			if in.Repeat < 0 || in.Repeat > maxRepeat {
				return nil, invalid("repeat must be between 1 and %d", maxRepeat)
			}
			keys := in.Keys
			return newCall("browser_press", tools.KindExec, "Press "+c.String(), "", func(ctx context.Context) (string, error) {
				return s.Press(ctx, keys, in.Repeat)
			}), nil
		},
	}
}

// ---- browser_select -----------------------------------------------------------------------

func selectTool(s *Session) tools.Tool {
	return &tool{
		kind: tools.KindExec,
		spec: llm.ToolSpec{
			Name: "browser_select",
			Description: "Choose option(s) in a dropdown. Native <select> elements match by option label or value; custom dropdowns " +
				"(comboboxes, listboxes, menus) are opened with a click and the option with matching text is clicked." + targetHelp,
			Properties: targetProps(map[string]any{
				"values": map[string]any{"type": "array", "minItems": 1, "maxItems": maxSelectValues, "items": map[string]any{"type": "string"}, "description": "Options to choose; more than one only for multi-selects."},
				"by":     map[string]any{"type": "string", "enum": []string{"auto", "label", "value"}, "description": "How to match options (default auto: label, then value, then partial label)."},
			}, "is used"),
			Required: []string{"values"},
		},
		prepare: func(env *tools.Env, raw json.RawMessage) (*tools.Call, error) {
			var in struct {
				targetInput
				Values []string `json:"values"`
				By     string   `json:"by"`
			}
			if err := decode(raw, &in); err != nil {
				return nil, err
			}
			t, err := in.required()
			if err != nil {
				return nil, err
			}
			if len(in.Values) == 0 {
				return nil, invalid("\"values\" needs at least one option")
			}
			if len(in.Values) > maxSelectValues {
				return nil, invalid("at most %d values", maxSelectValues)
			}
			for i, v := range in.Values {
				if strings.TrimSpace(v) == "" {
					return nil, invalid("values[%d] is blank", i)
				}
			}
			switch in.By {
			case "":
				in.By = "auto"
			case "auto", "label", "value":
			default:
				return nil, invalid("by must be auto, label or value")
			}
			summary := fmt.Sprintf("Select %s in %s", quoteList(in.Values), t)
			return newCall("browser_select", tools.KindExec, summary, "", func(ctx context.Context) (string, error) {
				return s.Select(ctx, t, in.Values, in.By)
			}), nil
		},
	}
}

// ---- browser_check ------------------------------------------------------------------------

func checkTool(s *Session) tools.Tool {
	return &tool{
		kind: tools.KindExec,
		spec: llm.ToolSpec{
			Name:        "browser_check",
			Description: "Check or uncheck a checkbox, switch or radio button by clicking it only if needed, then verify its state." + targetHelp,
			Properties: targetProps(map[string]any{
				"checked": map[string]any{"type": "boolean", "description": "Wanted state (default true)."},
			}, "is used"),
		},
		prepare: func(env *tools.Env, raw json.RawMessage) (*tools.Call, error) {
			var in struct {
				targetInput
				Checked *bool `json:"checked"`
			}
			if err := decode(raw, &in); err != nil {
				return nil, err
			}
			t, err := in.required()
			if err != nil {
				return nil, err
			}
			want := in.Checked == nil || *in.Checked
			verb := "Check "
			if !want {
				verb = "Uncheck "
			}
			return newCall("browser_check", tools.KindExec, verb+t.String(), "", func(ctx context.Context) (string, error) {
				return s.Check(ctx, t, want)
			}), nil
		},
	}
}

// ---- browser_hover ------------------------------------------------------------------------

func hoverTool(s *Session) tools.Tool {
	return &tool{
		kind: tools.KindRead,
		spec: llm.ToolSpec{
			Name:        "browser_hover",
			Description: "Move the pointer onto an element and leave it there, e.g. to open a hover menu or tooltip. Returns how the page changed." + targetHelp,
			Properties:  targetProps(nil, "is used"),
		},
		prepare: func(env *tools.Env, raw json.RawMessage) (*tools.Call, error) {
			var in targetInput
			if err := decode(raw, &in); err != nil {
				return nil, err
			}
			t, err := in.required()
			if err != nil {
				return nil, err
			}
			return newCall("browser_hover", tools.KindRead, "Hover "+t.String(), "", func(ctx context.Context) (string, error) {
				return s.Hover(ctx, t)
			}), nil
		},
	}
}

// ---- browser_scroll -----------------------------------------------------------------------

func scrollTool(s *Session) tools.Tool {
	return &tool{
		kind: tools.KindRead,
		spec: llm.ToolSpec{
			Name: "browser_scroll",
			Description: "Scroll with the mouse wheel. Give direction (and optionally amount in pixels; default most of a screen) to scroll the page, " +
				"to=top|bottom to jump to an end, or only a target element to scroll it into view. With a target and a direction, " +
				"scroll inside that element (a scrollable list or panel).",
			Properties: targetProps(map[string]any{
				"direction": map[string]any{"type": "string", "enum": []string{"up", "down", "left", "right"}},
				"amount":    map[string]any{"type": "integer", "minimum": 1, "maximum": maxScrollAmount, "description": "Pixels to scroll."},
				"to":        map[string]any{"type": "string", "enum": []string{"top", "bottom"}},
			}, "is used"),
		},
		prepare: func(env *tools.Env, raw json.RawMessage) (*tools.Call, error) {
			var in struct {
				targetInput
				Direction string `json:"direction"`
				Amount    int    `json:"amount"`
				To        string `json:"to"`
			}
			if err := decode(raw, &in); err != nil {
				return nil, err
			}
			t, err := in.optional()
			if err != nil {
				return nil, err
			}
			switch in.Direction {
			case "", "up", "down", "left", "right":
			default:
				return nil, invalid("direction must be up, down, left or right")
			}
			switch in.To {
			case "", "top", "bottom":
			default:
				return nil, invalid("to must be top or bottom")
			}
			if in.Direction != "" && in.To != "" {
				return nil, invalid("give direction or to, not both")
			}
			if in.To != "" && !t.IsZero() {
				return nil, invalid("to scrolls the page; it cannot be combined with a target")
			}
			if in.Amount < 0 || in.Amount > maxScrollAmount {
				return nil, invalid("amount must be between 1 and %d", maxScrollAmount)
			}
			if in.Amount != 0 && in.Direction == "" {
				return nil, invalid("amount needs a direction")
			}
			if in.Direction == "" && in.To == "" && t.IsZero() {
				return nil, invalid("give a direction, to (top or bottom), or a target element to scroll into view")
			}
			o := ScrollOptions{Target: t, Direction: in.Direction, Amount: float64(in.Amount), To: in.To}
			parts := []string{"Scroll"}
			for _, p := range []string{in.Direction, in.To} {
				if p != "" {
					parts = append(parts, p)
				}
			}
			if !t.IsZero() {
				parts = append(parts, t.String())
			}
			summary := strings.Join(parts, " ")
			return newCall("browser_scroll", tools.KindRead, summary, "", func(ctx context.Context) (string, error) {
				return s.Scroll(ctx, o)
			}), nil
		},
	}
}

// ---- browser_wait ---------------------------------------------------------------------------

func waitTool(s *Session) tools.Tool {
	return &tool{
		kind: tools.KindRead,
		spec: llm.ToolSpec{
			Name: "browser_wait",
			Description: "Wait until every given condition holds: text appears or disappears, an element (ref or selector) is visible, " +
				"the URL contains a substring, or the network is idle. Fails after timeout_ms (default 10000). " +
				"duration_ms alone just pauses.",
			Properties: map[string]any{
				"text":         map[string]any{"type": "string", "description": "Text that must appear on the page (case-insensitive)."},
				"text_gone":    map[string]any{"type": "string", "description": "Text that must disappear."},
				"ref":          map[string]any{"type": "string", "pattern": "^e[0-9]+$", "description": "Element ref that must be visible."},
				"selector":     map[string]any{"type": "string", "description": "CSS selector that must match a visible element."},
				"url_contains": map[string]any{"type": "string", "description": "Substring the page URL must contain."},
				"network_idle": map[string]any{"type": "boolean", "description": "Wait for network activity to stop."},
				"duration_ms":  map[string]any{"type": "integer", "minimum": 1, "maximum": maxWaitDuration.Milliseconds(), "description": "Fixed pause before checking."},
				"timeout_ms":   map[string]any{"type": "integer", "minimum": 100, "maximum": maxWaitTimeout.Milliseconds(), "description": "Give up after this long."},
			},
		},
		prepare: func(env *tools.Env, raw json.RawMessage) (*tools.Call, error) {
			var in struct {
				Text        string `json:"text"`
				TextGone    string `json:"text_gone"`
				Ref         string `json:"ref"`
				Selector    string `json:"selector"`
				URLContains string `json:"url_contains"`
				NetworkIdle bool   `json:"network_idle"`
				DurationMS  int64  `json:"duration_ms"`
				TimeoutMS   int64  `json:"timeout_ms"`
			}
			if err := decode(raw, &in); err != nil {
				return nil, err
			}
			if in.Ref != "" && !refPattern.MatchString(in.Ref) {
				return nil, invalid("ref %q is not a snapshot ref like e12", in.Ref)
			}
			for name, v := range map[string]string{"text": in.Text, "text_gone": in.TextGone, "selector": in.Selector, "url_contains": in.URLContains} {
				if v != "" && strings.TrimSpace(v) == "" {
					return nil, invalid("%s is blank", name)
				}
				if len(v) > 1000 {
					return nil, invalid("%s is longer than 1000 bytes", name)
				}
			}
			if in.DurationMS < 0 || time.Duration(in.DurationMS)*time.Millisecond > maxWaitDuration {
				return nil, invalid("duration_ms must be between 1 and %d", maxWaitDuration.Milliseconds())
			}
			if in.TimeoutMS != 0 && (in.TimeoutMS < 100 || time.Duration(in.TimeoutMS)*time.Millisecond > maxWaitTimeout) {
				return nil, invalid("timeout_ms must be between 100 and %d", maxWaitTimeout.Milliseconds())
			}
			o := WaitOptions{
				Text: in.Text, TextGone: in.TextGone, Ref: in.Ref, Selector: in.Selector, URLContains: in.URLContains,
				NetworkIdle: in.NetworkIdle, Duration: time.Duration(in.DurationMS) * time.Millisecond,
				Timeout: time.Duration(in.TimeoutMS) * time.Millisecond,
			}
			cond := o.describe()
			if cond == "" {
				return nil, invalid("give something to wait for: text, text_gone, ref, selector, url_contains, network_idle or duration_ms")
			}
			return newCall("browser_wait", tools.KindRead, "Wait for "+cond, "", func(ctx context.Context) (string, error) {
				return s.Wait(ctx, o)
			}), nil
		},
	}
}

// ---- browser_screenshot ---------------------------------------------------------------------

func screenshotTool(s *Session) tools.Tool {
	return &tool{
		kind: tools.KindRead,
		spec: llm.ToolSpec{
			Name: "browser_screenshot",
			Description: "Capture the visible part of the page. With path, save it to that file inside the workspace (.png or .jpg); " +
				"without, report the image size only. Prefer browser_snapshot to read the page.",
			Properties: map[string]any{
				"path":    map[string]any{"type": "string", "description": "Where to save the image, relative to the workspace."},
				"format":  map[string]any{"type": "string", "enum": []string{"png", "jpeg"}, "description": "Image format (default from the path's extension, else png)."},
				"quality": map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "description": "JPEG quality (default 80)."},
			},
		},
		prepare: func(env *tools.Env, raw json.RawMessage) (*tools.Call, error) {
			var in struct {
				Path    string `json:"path"`
				Format  string `json:"format"`
				Quality int    `json:"quality"`
			}
			if err := decode(raw, &in); err != nil {
				return nil, err
			}
			format := strings.ToLower(in.Format)
			if format == "jpg" {
				format = "jpeg"
			}
			switch format {
			case "", "png", "jpeg":
			default:
				return nil, invalid("format must be png or jpeg")
			}
			if in.Quality < 0 || in.Quality > 100 {
				return nil, invalid("quality must be between 1 and 100")
			}
			var abs string
			if strings.TrimSpace(in.Path) != "" {
				abs = env.Resolve(in.Path)
				if env.Outside(abs) {
					return nil, invalid("path %s is outside the workspace", in.Path)
				}
				ext := strings.ToLower(filepath.Ext(abs))
				extFormat := map[string]string{".png": "png", ".jpg": "jpeg", ".jpeg": "jpeg"}[ext]
				if extFormat == "" {
					return nil, invalid("path must end in .png, .jpg or .jpeg")
				}
				if format != "" && format != extFormat {
					return nil, invalid("format %s does not match the %s extension", format, ext)
				}
				format = extFormat
				if st, err := os.Stat(abs); err == nil && st.IsDir() {
					return nil, invalid("%s is a directory", in.Path)
				}
			}
			if format == "" {
				format = "png"
			}
			if in.Quality != 0 && format != "jpeg" {
				return nil, invalid("quality applies to jpeg only")
			}
			quality := in.Quality
			if format == "jpeg" && quality == 0 {
				quality = 80
			}
			run := func(ctx context.Context) (string, error) {
				data, err := s.Screenshot(ctx, format, quality)
				if err != nil {
					return "", err
				}
				dims := "unknown size"
				if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
					dims = fmt.Sprintf("%dx%d", cfg.Width, cfg.Height)
				}
				if abs == "" {
					return fmt.Sprintf("Screenshot taken: %s %s, %s (not saved; give path to save it).", dims, format, humanBytes(len(data))), nil
				}
				if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
					return "", err
				}
				if err := os.WriteFile(abs, data, 0o644); err != nil {
					return "", err
				}
				return fmt.Sprintf("Saved screenshot (%s %s, %s) to %s", dims, format, humanBytes(len(data)), env.Rel(abs)), nil
			}
			if abs == "" {
				return newCall("browser_screenshot", tools.KindRead, "Take a screenshot", "", run), nil
			}
			c := newCall("browser_screenshot", tools.KindWrite, "Save a screenshot to "+env.Rel(abs), "", run)
			c.Path = abs
			return c, nil
		},
	}
}

func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

// ---- browser_extract ------------------------------------------------------------------------

func extractTool(s *Session) tools.Tool {
	return &tool{
		kind: tools.KindRead,
		spec: llm.ToolSpec{
			Name: "browser_extract",
			Description: "Return the readable text of the page, or of one element (an article, a table, a dialog) when a target is given. " +
				"Includes same-origin frames and open shadow roots. Capped at max_chars (default 8000).",
			Properties: targetProps(map[string]any{
				"max_chars": map[string]any{"type": "integer", "minimum": 100, "maximum": maxExtractChars, "description": "Cap on the text returned."},
			}, "is read"),
		},
		prepare: func(env *tools.Env, raw json.RawMessage) (*tools.Call, error) {
			var in struct {
				targetInput
				MaxChars int `json:"max_chars"`
			}
			if err := decode(raw, &in); err != nil {
				return nil, err
			}
			t, err := in.optional()
			if err != nil {
				return nil, err
			}
			if in.MaxChars != 0 && (in.MaxChars < 100 || in.MaxChars > maxExtractChars) {
				return nil, invalid("max_chars must be between 100 and %d", maxExtractChars)
			}
			limit := in.MaxChars
			if limit == 0 {
				limit = defExtractChars
			}
			summary := "Read the page text"
			if !t.IsZero() {
				summary = "Read the text of " + t.String()
			}
			return newCall("browser_extract", tools.KindRead, summary, "", func(ctx context.Context) (string, error) {
				return s.Extract(ctx, t, limit)
			}), nil
		},
	}
}

// ---- browser_back / browser_forward / browser_reload ---------------------------------------

func historyTool(s *Session, name string) tools.Tool {
	desc := map[string]string{
		"browser_back":    "Go back to the previous page in history and return its outline.",
		"browser_forward": "Go forward in history and return the page outline.",
		"browser_reload":  "Reload the current page and return its outline.",
	}[name]
	return &tool{
		kind: tools.KindExec,
		spec: llm.ToolSpec{Name: name, Description: desc, Properties: map[string]any{}},
		prepare: func(env *tools.Env, raw json.RawMessage) (*tools.Call, error) {
			var in struct{}
			if err := decode(raw, &in); err != nil {
				return nil, err
			}
			var run func(ctx context.Context) (string, error)
			summary := ""
			switch name {
			case "browser_back":
				run, summary = s.Back, "Go back"
			case "browser_forward":
				run, summary = s.Forward, "Go forward"
			default:
				run, summary = s.Reload, "Reload the page"
			}
			return newCall(name, tools.KindExec, summary, "", run), nil
		},
	}
}

// ---- browser_eval -----------------------------------------------------------------------------

func evalTool(s *Session) tools.Tool {
	return &tool{
		kind: tools.KindExec,
		spec: llm.ToolSpec{
			Name: "browser_eval",
			Description: "Evaluate a JavaScript expression in the page and return its JSON value. Use only when the other browser tools cannot " +
				"do the job (reading app state, a value not shown on the page). Do not use it to click or type: those must go through the " +
				"other tools so they behave like a person.",
			Properties: map[string]any{
				"expression": map[string]any{"type": "string", "description": "A JavaScript expression, e.g. document.querySelectorAll('tr').length or (() => { ... })()."},
			},
			Required: []string{"expression"},
		},
		prepare: func(env *tools.Env, raw json.RawMessage) (*tools.Call, error) {
			var in struct {
				Expression string `json:"expression"`
			}
			if err := decode(raw, &in); err != nil {
				return nil, err
			}
			if strings.TrimSpace(in.Expression) == "" {
				return nil, invalid("\"expression\" is required")
			}
			if len(in.Expression) > maxEvalChars {
				return nil, invalid("expression is longer than %d bytes", maxEvalChars)
			}
			expr := in.Expression
			return newCall("browser_eval", tools.KindExec, "Run JavaScript in the page: "+firstLineOf(expr, 60), expr, func(ctx context.Context) (string, error) {
				res, err := s.Eval(ctx, expr)
				if err != nil {
					return "", err
				}
				return formatEval(res), nil
			}), nil
		},
	}
}

func formatEval(res json.RawMessage) string {
	res = bytes.TrimSpace(res)
	if len(res) == 0 {
		return "undefined"
	}
	var buf bytes.Buffer
	if json.Indent(&buf, res, "", "  ") == nil && buf.Len() <= maxEvalOutput {
		return buf.String()
	}
	out := string(res)
	if len(out) > maxEvalOutput {
		cut := maxEvalOutput
		for cut > 0 && !utf8.RuneStart(out[cut]) {
			cut--
		}
		out = out[:cut] + fmt.Sprintf("\n… [%d more bytes]", len(res)-cut)
	}
	return out
}
