package browsertools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

func testEnv(t *testing.T) *tools.Env {
	t.Helper()
	env, err := tools.NewEnv(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func toolByName(t *testing.T, s *Session, name string) tools.Tool {
	t.Helper()
	for _, tl := range Tools(s) {
		if tl.Spec().Name == name {
			return tl
		}
	}
	t.Fatalf("no tool %s", name)
	return nil
}

func TestToolSet(t *testing.T) {
	s, _ := newTestSession(newFakePage(), SpeedInstant)
	want := []string{"browser_navigate", "browser_snapshot", "browser_click", "browser_fill", "browser_type", "browser_press",
		"browser_select", "browser_check", "browser_hover", "browser_scroll", "browser_wait", "browser_screenshot",
		"browser_extract", "browser_back", "browser_forward", "browser_reload", "browser_eval"}
	ts := Tools(s)
	if len(ts) != len(want) {
		t.Fatalf("got %d tools, want %d", len(ts), len(want))
	}
	reads := map[string]bool{"browser_snapshot": true, "browser_hover": true, "browser_scroll": true, "browser_wait": true, "browser_screenshot": true, "browser_extract": true}
	for i, tl := range ts {
		spec := tl.Spec()
		if spec.Name != want[i] {
			t.Errorf("tool %d is %s, want %s", i, spec.Name, want[i])
		}
		if len(spec.Description) < 40 {
			t.Errorf("%s: description too short", spec.Name)
		}
		if spec.Properties == nil {
			t.Errorf("%s: nil properties", spec.Name)
		}
		for _, r := range spec.Required {
			if _, ok := spec.Properties[r]; !ok {
				t.Errorf("%s: required %q is not a property", spec.Name, r)
			}
		}
		if _, err := json.Marshal(spec.Properties); err != nil {
			t.Errorf("%s: properties not JSON: %v", spec.Name, err)
		}
		wantKind := tools.KindExec
		if reads[spec.Name] {
			wantKind = tools.KindRead
		}
		if tl.Kind() != wantKind {
			t.Errorf("%s: kind %v, want %v", spec.Name, tl.Kind(), wantKind)
		}
	}
	reg := tools.NewRegistry(ts...)
	if len(reg.Names()) != len(want) {
		t.Fatal("duplicate tool names")
	}
	noEval := NewSession(nil, Options{DisableEval: true})
	for _, tl := range Tools(noEval) {
		if tl.Spec().Name == "browser_eval" {
			t.Fatal("DisableEval should drop browser_eval")
		}
	}
}

// TestToolValidation feeds every tool bad input; each must be refused in
// Prepare, before anything runs.
func TestToolValidation(t *testing.T) {
	s, _ := newTestSession(newFakePage(), SpeedInstant)
	env := testEnv(t)
	cases := map[string][]struct{ in, want string }{
		"browser_navigate": {
			{`{}`, "url is required"},
			{`{"url":"javascript:alert(1)"}`, "not allowed"},
			{`{"url":"https://x.org","x":1}`, "unknown field"},
			{`{"url":"https://x.org"} {}`, "trailing data"},
			{`{"url":5}`, "invalid tool input"},
			{`{"url":"file:///etc/passwd"}`, "outside the workspace"},
		},
		"browser_snapshot": {
			{`{"ref":"12"}`, "not a snapshot ref"},
			{`{"max_chars":10}`, "max_chars must be between"},
			{`{"scope":"e1"}`, "unknown field"},
		},
		"browser_click": {
			{`{}`, "an element is required"},
			{`{"ref":"e1","selector":"a"}`, "only one of"},
			{`{"ref":"E1"}`, "not a snapshot ref"},
			{`{"ref":"e0"}`, "not a snapshot ref"},
			{`{"selector":"   "}`, "selector is blank"},
			{`{"match_text":"  "}`, "match_text is blank"},
			{`{"match_text":"` + strings.Repeat("x", 201) + `"}`, "longer than 200"},
			{`{"ref":"e1","button":"side"}`, "button must be"},
			{`{"ref":"e1","click_count":4}`, "click_count must be"},
			{`{"ref":"e1","modifiers":["Hyper"]}`, "unknown modifier"},
			{`{"ref":"e1","modifiers":["ctrl","Control"]}`, "given twice"},
			{`{"ref":"e1","text":"x"}`, "unknown field"},
		},
		"browser_fill": {
			{`{}`, "at least one field"},
			{`{"fields":[]}`, "at least one field"},
			{`{"fields":[{"value":"x"}]}`, "field 1: invalid tool input: an element is required"},
			{`{"fields":[{"ref":"e1"}]}`, "\"value\" is required"},
			{`{"fields":[{"ref":"e1","value":"x","bogus":1}]}`, "unknown field"},
			{`{"fields":[{"ref":"e1","value":"a"},{"ref":"e2","selector":"#x","value":"b"}]}`, "field 2"},
			{`{"fields":"e1"}`, "invalid tool input"},
		},
		"browser_type": {
			{`{"ref":"e1"}`, "\"text\" is required"},
			{`{"ref":"e1","text":""}`, "\"text\" is required"},
			{`{"text":"x","clear":true}`, "clear needs a target"},
			{`{"text":"` + strings.Repeat("x", maxTypeChars+1) + `"}`, "longer than"},
			{`{"ref":"bad","text":"x"}`, "not a snapshot ref"},
		},
		"browser_press": {
			{`{}`, "\"keys\" is required"},
			{`{"keys":"Hyper+X"}`, "not a modifier"},
			{`{"keys":"Enterr"}`, "unknown key"},
			{`{"keys":"Enter","repeat":101}`, "repeat must be"},
			{`{"keys":"Enter","repeat":-1}`, "repeat must be"},
		},
		"browser_select": {
			{`{"ref":"e1"}`, "at least one option"},
			{`{"values":["a"]}`, "an element is required"},
			{`{"ref":"e1","values":[" "]}`, "values[0] is blank"},
			{`{"ref":"e1","values":["a"],"by":"index"}`, "by must be"},
			{`{"ref":"e1","value":"a"}`, "unknown field"},
		},
		"browser_check": {
			{`{}`, "an element is required"},
			{`{"ref":"e1","checked":"yes"}`, "invalid tool input"},
		},
		"browser_hover": {
			{`{}`, "an element is required"},
			{`{"ref":"e1","x":3}`, "unknown field"},
		},
		"browser_scroll": {
			{`{}`, "give a direction"},
			{`{"direction":"sideways"}`, "direction must be"},
			{`{"to":"middle"}`, "to must be"},
			{`{"direction":"up","to":"top"}`, "not both"},
			{`{"to":"top","ref":"e1"}`, "cannot be combined"},
			{`{"amount":100}`, "amount needs a direction"},
			{`{"direction":"down","amount":100001}`, "amount must be"},
		},
		"browser_wait": {
			{`{}`, "give something to wait for"},
			{`{"ref":"x1"}`, "not a snapshot ref"},
			{`{"text":"  "}`, "text is blank"},
			{`{"text":"a","timeout_ms":5}`, "timeout_ms must be"},
			{`{"duration_ms":999999}`, "duration_ms must be"},
			{`{"selector":"` + strings.Repeat("a", 1001) + `"}`, "longer than 1000"},
		},
		"browser_screenshot": {
			{`{"format":"gif"}`, "format must be"},
			{`{"path":"shot.bmp"}`, "must end in .png"},
			{`{"path":"shot.png","format":"jpeg"}`, "does not match"},
			{`{"path":"../outside.png"}`, "outside the workspace"},
			{`{"quality":50}`, "jpeg only"},
			{`{"format":"jpeg","quality":101}`, "quality must be"},
		},
		"browser_extract": {
			{`{"max_chars":5}`, "max_chars must be"},
			{`{"ref":"e1","match_text":"x"}`, "only one of"},
		},
		"browser_back":    {{`{"x":1}`, "unknown field"}},
		"browser_forward": {{`[]`, "invalid tool input"}},
		"browser_reload":  {{`{"hard":true}`, "unknown field"}},
		"browser_eval": {
			{`{}`, "\"expression\" is required"},
			{`{"expression":"   "}`, "\"expression\" is required"},
			{`{"expression":"` + strings.Repeat("1", maxEvalChars+1) + `"}`, "longer than"},
		},
	}
	for _, tl := range Tools(s) {
		name := tl.Spec().Name
		cs, ok := cases[name]
		if !ok {
			t.Errorf("no validation cases for %s", name)
			continue
		}
		for _, c := range cs {
			call, err := tl.Prepare(env, json.RawMessage(c.in))
			if err == nil {
				t.Errorf("%s(%s): accepted, want error containing %q", name, trunc(c.in), c.want)
				continue
			}
			if call != nil {
				t.Errorf("%s: returned a call with an error", name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s(%s): error %q, want %q", name, trunc(c.in), err, c.want)
			}
		}
	}
	// Nothing ran, so no driver was started.
	if s.Started() {
		t.Fatal("validation must not start the browser")
	}
}

func trunc(s string) string {
	if len(s) > 60 {
		return s[:60] + "…"
	}
	return s
}

func TestToolPrepareAccepts(t *testing.T) {
	s, _ := newTestSession(newFakePage(), SpeedInstant)
	env := testEnv(t)
	cases := []struct {
		tool, in string
		kind     tools.Kind
		summary  string
	}{
		{"browser_navigate", `{"url":"example.com"}`, tools.KindExec, "Open https://example.com"},
		{"browser_snapshot", ``, tools.KindRead, "Snapshot the page"},
		{"browser_snapshot", `{"ref":"e4","viewport_only":true,"max_chars":2000}`, tools.KindRead, "Snapshot e4"},
		{"browser_click", `{"match_text":"Sign in","click_count":2,"modifiers":["ctrl"]}`, tools.KindExec, `Click text "Sign in"`},
		{"browser_fill", `{"fields":[{"ref":"e2","value":"a@b.c"},{"match_text":"Password","value":"pw","append":true}],"submit":true}`, tools.KindExec, "Fill 2 field(s)"},
		{"browser_type", `{"text":"hello\n"}`, tools.KindExec, "Type 6 characters into the focused element"},
		{"browser_type", `{"text":"","submit":true}`, tools.KindExec, "Type 0 characters"},
		{"browser_press", `{"keys":"Control+Shift+K","repeat":3}`, tools.KindExec, "Press Control+Shift+K"},
		{"browser_select", `{"selector":"#country","values":["Germany"],"by":"label"}`, tools.KindExec, `Select "Germany" in selector "#country"`},
		{"browser_check", `{"ref":"e4","checked":false}`, tools.KindExec, "Uncheck e4"},
		{"browser_hover", `{"ref":"e9"}`, tools.KindRead, "Hover e9"},
		{"browser_scroll", `{"direction":"down","amount":300}`, tools.KindRead, "Scroll down"},
		{"browser_scroll", `{"ref":"e30"}`, tools.KindRead, "Scroll e30"},
		{"browser_wait", `{"text":"Welcome","timeout_ms":5000}`, tools.KindRead, `Wait for text "Welcome"`},
		{"browser_wait", `{"duration_ms":500}`, tools.KindRead, "Wait for 500ms"},
		{"browser_screenshot", `{}`, tools.KindRead, "Take a screenshot"},
		{"browser_screenshot", `{"path":"shots/a.jpg","quality":70}`, tools.KindWrite, "Save a screenshot to shots/a.jpg"},
		{"browser_extract", `{"ref":"e1","max_chars":500}`, tools.KindRead, "Read the text of e1"},
		{"browser_back", `{}`, tools.KindExec, "Go back"},
		{"browser_eval", `{"expression":"document.title"}`, tools.KindExec, "Run JavaScript in the page: document.title"},
	}
	for _, c := range cases {
		call, err := toolByName(t, s, c.tool).Prepare(env, json.RawMessage(c.in))
		if err != nil {
			t.Errorf("%s(%s): %v", c.tool, c.in, err)
			continue
		}
		if call.Kind != c.kind {
			t.Errorf("%s(%s): kind %v, want %v", c.tool, c.in, call.Kind, c.kind)
		}
		if !strings.HasPrefix(call.Summary, c.summary) {
			t.Errorf("%s(%s): summary %q, want prefix %q", c.tool, c.in, call.Summary, c.summary)
		}
		if call.Tool != c.tool {
			t.Errorf("call.Tool = %s", call.Tool)
		}
	}
	call, _ := toolByName(t, s, "browser_screenshot").Prepare(env, json.RawMessage(`{"path":"shots/a.png"}`))
	if call.Path != filepath.Join(env.Root, "shots", "a.png") {
		t.Errorf("screenshot call path = %q", call.Path)
	}
	if s.Started() {
		t.Fatal("Prepare must not start the browser")
	}
}

func TestToolsEndToEnd(t *testing.T) {
	f := loginPage()
	f.pageText = "Sign in to continue"
	f.shot = pngBytes(t, 64, 48)
	f.rawEval = func(expr string) (json.RawMessage, error) { return json.RawMessage(`{"a":[1,2]}`), nil }
	s, _ := newTestSession(f, SpeedInstant)
	env := testEnv(t)
	run := func(tool, in string) string {
		t.Helper()
		call, err := toolByName(t, s, tool).Prepare(env, json.RawMessage(in))
		if err != nil {
			t.Fatalf("%s prepare: %v", tool, err)
		}
		out, err := call.Run(context.Background())
		if err != nil {
			t.Fatalf("%s run: %v", tool, err)
		}
		return out
	}
	out := run("browser_navigate", `{"url":"https://example.test/login"}`)
	if !strings.HasPrefix(out, "Navigated to https://example.test/login\n\nPage: Sign in") || !strings.Contains(out, `[e2] textbox "Email"`) {
		t.Fatalf("navigate:\n%s", out)
	}
	out = run("browser_fill", `{"fields":[{"ref":"e2","value":"me@x.org"},{"selector":"#pw","value":"s3cret"},{"match_text":"Remember me","value":"true"},{"ref":"e5","value":"de"}]}`)
	for _, want := range []string{
		`1. [e2] textbox "Email" value="me@x.org"`,
		`2. [e3] textbox "Password" value=(6 characters, masked)`,
		`3. [e4] checkbox "Remember me" is now checked`,
		`4. [e5] combobox "Country" now has "Germany" selected`,
		"Page changes:",
		`~ [e2] textbox "Email" value="me@x.org"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("fill result lacks %q:\n%s", want, out)
		}
	}
	out = run("browser_eval", `{"expression":"1"}`)
	if out != "{\n  \"a\": [\n    1,\n    2\n  ]\n}" {
		t.Errorf("eval = %q", out)
	}
	out = run("browser_screenshot", `{"path":"out/s.png"}`)
	if !strings.Contains(out, "64x48 png") {
		t.Errorf("screenshot = %q", out)
	}
	if b, err := os.ReadFile(filepath.Join(env.Root, "out", "s.png")); err != nil || len(b) != len(f.shot) {
		t.Fatalf("screenshot file: %v", err)
	}
	out = run("browser_screenshot", `{}`)
	if !strings.Contains(out, "64x48 png") || !strings.Contains(out, "not saved") {
		t.Errorf("screenshot dims = %q", out)
	}
	out = run("browser_extract", `{}`)
	if !strings.Contains(out, "Sign in to continue") {
		t.Errorf("extract = %q", out)
	}
	out = run("browser_snapshot", `{"max_chars":500}`)
	if len([]rune(out)) > 500 {
		t.Errorf("snapshot over cap: %d", len([]rune(out)))
	}
	out = run("browser_back", `{}`)
	if !strings.HasPrefix(out, "Went back to") {
		t.Errorf("back = %q", out)
	}
	kinds := map[string]int{}
	for _, a := range s.Actions() {
		kinds[a.Kind]++
	}
	for _, k := range []string{"navigate", "fill", "eval", "screenshot", "extract", "snapshot", "back"} {
		if kinds[k] == 0 {
			t.Errorf("action log has no %s: %v", k, kinds)
		}
	}
}
