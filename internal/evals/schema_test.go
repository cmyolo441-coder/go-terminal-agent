package evals

import (
	"strings"
	"testing"
)

func TestSchemaValidation(t *testing.T) {
	cases := []struct {
		name   string
		schema string
		valid  []string
		bad    []string
	}{
		{"type", `{"type":"string"}`, []string{`"x"`}, []string{`1`, `null`, `{}`}},
		{"type list", `{"type":["string","null"]}`, []string{`"x"`, `null`}, []string{`1`}},
		{"integer", `{"type":"integer"}`, []string{`1`, `1.0`, `-3`, `1e2`}, []string{`1.5`, `"1"`}},
		{"number accepts integer", `{"type":"number"}`, []string{`1`, `1.5`}, []string{`true`}},
		{"enum", `{"enum":["a",1,null,{"k":[1]}]}`, []string{`"a"`, `1.0`, `null`, `{"k":[1]}`}, []string{`"b"`, `{"k":[2]}`}},
		{"const", `{"const":{"a":1}}`, []string{`{"a":1}`, `{"a":1.0}`}, []string{`{"a":2}`, `{}`}},
		{"object", `{"type":"object","properties":{"name":{"type":"string","minLength":2},"age":{"type":"integer","minimum":0}},"required":["name"],"additionalProperties":false}`,
			[]string{`{"name":"Al"}`, `{"name":"Bo","age":3}`},
			[]string{`{}`, `{"name":"A"}`, `{"name":"Al","age":-1}`, `{"name":"Al","x":1}`, `[]`}},
		{"additional schema", `{"additionalProperties":{"type":"number"}}`, []string{`{"a":1}`}, []string{`{"a":"x"}`}},
		{"patternProperties", `{"patternProperties":{"^x_":{"type":"integer"}},"additionalProperties":false}`, []string{`{"x_a":1}`}, []string{`{"x_a":"s"}`, `{"y":1}`}},
		{"propertyNames", `{"propertyNames":{"pattern":"^[a-z]+$"}}`, []string{`{"abc":1}`}, []string{`{"aB":1}`}},
		{"min/max properties", `{"minProperties":1,"maxProperties":2}`, []string{`{"a":1}`}, []string{`{}`, `{"a":1,"b":2,"c":3}`}},
		{"dependentRequired", `{"dependentRequired":{"card":["billing"]}}`, []string{`{}`, `{"card":1,"billing":2}`}, []string{`{"card":1}`}},
		{"array", `{"type":"array","items":{"type":"integer"},"minItems":1,"maxItems":3,"uniqueItems":true}`,
			[]string{`[1]`, `[1,2,3]`}, []string{`[]`, `[1,2,3,4]`, `[1,1]`, `[1,"a"]`, `[1, 1.0]`}},
		{"tuple prefixItems", `{"prefixItems":[{"type":"string"},{"type":"integer"}],"items":false}`, []string{`["a",1]`, `["a"]`}, []string{`[1,"a"]`}},
		{"tuple draft7", `{"items":[{"type":"string"}],"additionalItems":false}`, []string{`["a"]`}, []string{`["a",1]`}},
		{"contains", `{"contains":{"const":"x"}}`, []string{`["a","x"]`}, []string{`["a"]`, `[]`}},
		{"string", `{"maxLength":3,"pattern":"^[a-z]+$"}`, []string{`"abc"`}, []string{`"abcd"`, `"AB"`}},
		{"length counts runes", `{"minLength":3,"maxLength":3}`, []string{`"日本語"`}, []string{`"日本"`}},
		{"numbers", `{"minimum":0,"exclusiveMaximum":10,"multipleOf":0.5}`, []string{`0`, `9.5`, `2`}, []string{`-1`, `10`, `0.3`}},
		{"draft4 exclusive", `{"minimum":0,"exclusiveMinimum":true}`, []string{`0.1`}, []string{`0`}},
		{"multipleOf float", `{"multipleOf":0.01}`, []string{`0.07`, `19.99`}, []string{`0.075`}},
		{"allOf", `{"allOf":[{"type":"integer"},{"minimum":5}]}`, []string{`5`}, []string{`4`, `5.5`}},
		{"anyOf", `{"anyOf":[{"type":"string"},{"minimum":5}]}`, []string{`"a"`, `7`}, []string{`1`}},
		{"oneOf", `{"oneOf":[{"type":"integer"},{"minimum":5}]}`, []string{`1`, `5.5`}, []string{`7`, `2.5`}},
		{"not", `{"not":{"type":"null"}}`, []string{`1`}, []string{`null`}},
		{"if/then/else", `{"if":{"properties":{"kind":{"const":"a"}}},"then":{"required":["a"]},"else":{"required":["b"]}}`,
			[]string{`{"kind":"a","a":1}`, `{"kind":"z","b":1}`}, []string{`{"kind":"a"}`, `{"kind":"z"}`}},
		{"ref defs", `{"$defs":{"pos":{"type":"integer","minimum":1}},"type":"array","items":{"$ref":"#/$defs/pos"}}`, []string{`[1,2]`}, []string{`[0]`}},
		{"recursive ref", `{"type":"object","properties":{"v":{"type":"integer"},"next":{"$ref":"#"}},"required":["v"]}`,
			[]string{`{"v":1,"next":{"v":2,"next":{"v":3}}}`}, []string{`{"v":1,"next":{"x":2}}`}},
		{"definitions with escape", `{"definitions":{"a/b":{"type":"string"}},"$ref":"#/definitions/a~1b"}`, []string{`"s"`}, []string{`1`}},
		{"boolean schemas", `{"properties":{"yes":true,"no":false}}`, []string{`{"yes":1}`}, []string{`{"no":1}`}},
		{"formats", `{"type":"object","properties":{"d":{"format":"date-time"},"e":{"format":"email"},"u":{"format":"uuid"},"i":{"format":"ipv4"},"h":{"format":"hostname"},"x":{"format":"custom-unknown"}}}`,
			[]string{`{"d":"2024-01-02T03:04:05Z","e":"a@b.co","u":"123e4567-e89b-12d3-a456-426614174000","i":"10.0.0.1","h":"example.com","x":"anything"}`},
			[]string{`{"d":"yesterday"}`, `{"e":"nope"}`, `{"u":"123"}`, `{"i":"::1"}`, `{"h":"-bad-"}`}},
	}
	for _, c := range cases {
		s, err := CompileSchema([]byte(c.schema))
		if err != nil {
			t.Errorf("%s: compile: %v", c.name, err)
			continue
		}
		for _, v := range c.valid {
			errs, err := s.ValidateJSON([]byte(v))
			if err != nil || len(errs) > 0 {
				t.Errorf("%s: %s should be valid: %v %v", c.name, v, errs, err)
			}
		}
		for _, v := range c.bad {
			errs, err := s.ValidateJSON([]byte(v))
			if err != nil || len(errs) == 0 {
				t.Errorf("%s: %s should be invalid (err %v)", c.name, v, err)
			}
		}
	}
}

func TestSchemaErrorPaths(t *testing.T) {
	s, err := CompileSchema([]byte(`{"type":"object","properties":{"items":{"type":"array","items":{"type":"object","properties":{"a/b":{"type":"string"}}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	errs, _ := s.ValidateJSON([]byte(`{"items":[{"a/b":"ok"},{"a/b":5}]}`))
	if len(errs) != 1 || errs[0].Path != "/items/1/a~1b" {
		t.Fatalf("errors = %v", errs)
	}
	if !strings.Contains(errs[0].Error(), "expected string, got integer") {
		t.Errorf("message = %q", errs[0].Error())
	}
}

func TestSchemaCompileErrors(t *testing.T) {
	bad := []string{
		`not json`,
		`[]`,
		`{"type":"strng"}`,
		`{"type":5}`,
		`{"minLength":-1}`,
		`{"pattern":"("}`,
		`{"$ref":"http://example.com/s.json"}`,
		`{"$ref":"#/definitions/missing"}`,
		`{"multipleOf":0}`,
		`{"allOf":[]}`,
		`{"properties":[]}`,
		`{"exclusiveMinimum":true}`,
	}
	for _, b := range bad {
		if _, err := CompileSchema([]byte(b)); err == nil {
			t.Errorf("%s: want compile error", b)
		}
	}
	// A self-reference that consumes no input terminates with an error.
	s, err := CompileSchema([]byte(`{"$ref":"#"}`))
	if err != nil {
		t.Fatal(err)
	}
	errs := s.Validate(map[string]any{})
	if len(errs) == 0 || !strings.Contains(errs[0].Message, "recursion") {
		t.Errorf("want recursion error, got %v", errs)
	}
}

func TestSchemaValidateGoValues(t *testing.T) {
	s, _ := CompileSchema([]byte(`{"type":"object","properties":{"n":{"type":"integer"},"tags":{"type":"array","items":{"type":"string"}}}}`))
	if errs := s.Validate(map[string]any{"n": 3, "tags": []string{"a"}}); len(errs) != 0 {
		t.Errorf("Go values: %v", errs)
	}
	if errs := s.Validate(map[string]any{"n": 3.5}); len(errs) != 1 {
		t.Errorf("Go float: %v", errs)
	}
}
