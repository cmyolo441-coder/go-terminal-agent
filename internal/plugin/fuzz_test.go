package plugin

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// FuzzParseManifest checks that manifest parsing never panics, and that a
// manifest it accepts has the properties the loader relies on.
func FuzzParseManifest(f *testing.F) {
	for _, s := range []string{
		`{"name":"x","version":"1.0.0","description":"d","tools":[{"name":"t","description":"d","command":"./t","input_schema":{"type":"object","properties":{"q":{"type":"string","pattern":"^a"}},"required":["q"]}}]}`,
		`{"name":"x","version":"v1.2.3-rc.1+b","description":"d","commands":[{"name":"c","description":"d","prompt":"do {{args}} {{ 1 }}"}]}`,
		`{"name":"x","version":"1.0.0","description":"d","commands":[{"name":"c","description":"d","prompt":"{{0}}"}]}`,
		`{"name":"x","version":"1.0.0","description":"d","env":{"GTA_X":"1","ok":"${PLUGIN_DIR}"},"pass_env":["1x"],"tools":[]}`,
		`{"name":"x","version":"1.0.0","description":"d","tools":[{"name":"t","description":"d","command":"t","input_schema":{"type":"object","anyOf":[{"not":{"type":"null"}}],"properties":{"a":{"type":["array"],"items":{"enum":[1,"x"]}}}}}]}`,
		`{"name":"x"} trailing`, `{"name":1}`, `[`, ``, `{"name":"x","version":"99999999999.0.0"}`,
		`{"name":"x","version":"1.0.0","description":"d","tools":[{"name":"t","description":"d","command":"t","input_schema":{"$ref":"#"}}]}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := ParseManifest("plugin.json", bytes.NewReader(data))
		if err != nil {
			if err.Error() == "" {
				t.Fatal("empty error message")
			}
			return
		}
		if m.Name == "" || len(m.Tools)+len(m.Commands) == 0 {
			t.Fatalf("accepted manifest without a name or items: %+v", m)
		}
		for _, tm := range m.Tools {
			s, err := compileSchema(tm.InputSchema)
			if err != nil {
				t.Fatalf("accepted tool %q whose schema does not compile: %v", tm.Name, err)
			}
			_ = s.validate(map[string]any{}, "")
			if _, err := json.Marshal(s.properties()); err != nil {
				t.Fatalf("schema properties do not marshal: %v", err)
			}
		}
		for _, cm := range m.Commands {
			if strings.TrimSpace(cm.Prompt) != "" {
				_, _ = expandTemplate(cm.Prompt, "a 'b c' \"d\"")
			}
		}
	})
}

// FuzzExpandTemplate checks template expansion: no panics, {{args}} is
// replaced by the trimmed arguments, and text outside placeholders survives.
func FuzzExpandTemplate(f *testing.F) {
	for _, s := range []string{"Review {{args}}", "{{1}} then {{2}}", "plain", "{{ args }}{{9}}{{10}}", "{{", "}}{{}}", "{{args}}{{args}}"} {
		f.Add(s, "a b 'c d'")
	}
	f.Fuzz(func(t *testing.T, tmpl, args string) {
		out, err := expandTemplate(tmpl, args)
		if err != nil {
			return
		}
		if !placeholder.MatchString(tmpl) {
			want := tmpl
			if a := strings.TrimSpace(args); a != "" {
				want = strings.TrimRight(tmpl, "\n") + "\n\n" + a
			}
			if out != want {
				t.Fatalf("expand(%q, %q) = %q, want %q", tmpl, args, out, want)
			}
		}
		if tmpl == "{{args}}" && out != strings.TrimSpace(args) {
			t.Fatalf("{{args}} with %q gave %q", args, out)
		}
	})
}

// FuzzSplitWords checks that word splitting never panics and that quoting
// a word with single quotes round-trips it.
func FuzzSplitWords(f *testing.F) {
	for _, s := range []string{"a b", `"x y" 'z'`, `\ `, `"unterminated`, "é\xff", `a\`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		_, _ = splitWords(s)
		if strings.ContainsRune(s, '\'') {
			return
		}
		words, err := splitWords("'" + s + "'")
		if err != nil || len(words) != 1 || words[0] != s {
			t.Fatalf("single-quoted %q split into %q (%v)", s, words, err)
		}
	})
}

// FuzzSchemaValidate checks that validating arbitrary JSON input against a
// representative schema never panics.
func FuzzSchemaValidate(f *testing.F) {
	schemaSrc := `{"type":"object","properties":{"q":{"type":"string","minLength":1,"maxLength":10,"pattern":"^[a-z]+$"},
		"n":{"type":"integer","minimum":0,"exclusiveMaximum":100},"tags":{"type":"array","items":{"type":"string"},"maxItems":3},
		"mode":{"enum":["a","b",1,null]},"obj":{"type":"object","additionalProperties":false,"properties":{"x":{"const":{"k":[1]}}}},
		"alt":{"oneOf":[{"type":"string"},{"type":"number"}]},"neg":{"not":{"type":"null"}}},"required":["q"]}`
	s, err := compileSchema(json.RawMessage(schemaSrc))
	if err != nil {
		f.Fatal(err)
	}
	for _, in := range []string{`{"q":"abc"}`, `{"q":"","n":1.5}`, `{"q":"a","tags":["x",1],"mode":null,"obj":{"x":{"k":[1.0]}}}`, `{"alt":true,"neg":null}`, `{"n":1e400}`} {
		f.Add([]byte(in))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		var v any
		if dec.Decode(&v) != nil {
			return
		}
		_ = s.validate(v, "")
	})
}
