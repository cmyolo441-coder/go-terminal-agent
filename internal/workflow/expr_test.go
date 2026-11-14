package workflow

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func exprVars() map[string]any {
	return map[string]any{
		"inputs": map[string]any{
			"name":    "World",
			"count":   3.0,
			"flag":    true,
			"targets": []any{"linux", "darwin"},
			"empty":   "",
			"obj":     map[string]any{"k": "v", "n": 2.0},
		},
		"steps": map[string]any{
			"build": map[string]any{"status": "succeeded", "outputs": map[string]any{"bin": "/out/app", "size": 1024.0}},
			"run-tests": map[string]any{
				"status": "failed", "outputs": map[string]any{},
			},
		},
		"env": map[string]any{"HOME": "/home/u"},
	}
}

func TestExprEval(t *testing.T) {
	tests := []struct {
		src  string
		want any
	}{
		{"1 + 2 * 3", 7.0},
		{"(1 + 2) * 3", 9.0},
		{"10 / 4", 2.5},
		{"10 % 4", 2.0},
		{"-inputs.count", -3.0},
		{"--3", 3.0},
		{"1.5e2", 150.0},
		{"'a' + \"b\"", "ab"},
		{`'it\'s\n'`, "it's\n"},
		{`"é"`, "é"},
		{"inputs.name == 'World'", true},
		{"inputs.name != 'World'", false},
		{"inputs.count >= 3 && inputs.count < 4", true},
		{"'abc' < 'abd'", true},
		{"!inputs.flag", false},
		{"!inputs.empty", true},
		{"inputs.empty || 'fallback'", true},
		{"false && inputs.missing.deep", false}, // short-circuit
		{"true || 1 / 0", true},
		{"'linux' in inputs.targets", true},
		{"'windows' in inputs.targets", false},
		{"'orl' in inputs.name", true},
		{"'k' in inputs.obj", true},
		{"'x' in null", false},
		{"!('x' in inputs.targets)", true},
		{"inputs.targets[1]", "darwin"},
		{"inputs.targets[5]", nil},
		{"inputs.obj['n'] * 2", 4.0},
		{"inputs.missing", nil},
		{"inputs.missing.deeper", nil},
		{"steps.build.outputs.bin", "/out/app"},
		{"steps['run-tests'].status == 'failed'", true},
		{"[1, 'a', true]", []any{1.0, "a", true}},
		{"[1] + [2]", []any{1.0, 2.0}},
		{"null == null", true},
		{"1 == '1'", false},
		{"[1, [2]] == [1, [2]]", true},
		{"len(inputs.targets)", 2.0},
		{"len('héllo')", 5.0},
		{"lower('ABC') + upper('def')", "abcDEF"},
		{"trim('  x ')", "x"},
		{"trimPrefix('v1.2', 'v')", "1.2"},
		{"trimSuffix('a.go', '.go')", "a"},
		{"startsWith(env.HOME, '/home')", true},
		{"endsWith('file.txt', '.go')", false},
		{"contains(inputs.targets, 'darwin')", true},
		{"contains('haystack', 'st')", true},
		{"replace('a-b-c', '-', '+')", "a+b+c"},
		{"split('a,b,,c', ',')", []any{"a", "b", "", "c"}},
		{"join(inputs.targets, ' ')", "linux darwin"},
		{"join([1, 2])", "1,2"},
		{"matches('v1.20.3', '^v[0-9]+\\\\.[0-9]+')", true},
		{"format('{0}-{1}-{0} {{x}}', 'a', 2)", "a-2-a {x}"},
		{"toJSON(inputs.obj)", `{"k":"v","n":2}`},
		{"fromJSON('{\"a\": [1, 2]}').a[1]", 2.0},
		{"string(42) + string(true) + string(null)", "42true"},
		{"number('  2.5 ') + number(true)", 3.5},
		{"bool('x') && !bool('')", true},
		{"default(inputs.missing, 'd')", "d"},
		{"default(inputs.empty, 'd')", "d"},
		{"default(inputs.name, 'd')", "World"},
		{"keys(inputs.obj)", []any{"k", "n"}},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			e, err := ParseExpr(tt.src)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			got, err := e.Eval(exprVars())
			if err != nil {
				t.Fatalf("eval: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestExprParseErrors(t *testing.T) {
	tests := []struct {
		src, want string
	}{
		{"", "unexpected end of expression"},
		{"1 +", "unexpected end of expression"},
		{"a = b", "use == to compare"},
		{"a & b", "use && or ||"},
		{"(1", "expected ')'"},
		{"[1, 2", "expected ',' or closing bracket"},
		{"a.", "expected a field name"},
		{"a.1", "expected a field name"},
		{"'abc", "unterminated string"},
		{`'\q'`, "unknown escape"},
		{"nosuch(1)", `unknown function "nosuch"`},
		{"len()", "len() takes 1 argument(s), got 0"},
		{"replace('a')", "takes 3 argument(s)"},
		{"1 2", `unexpected "2"`},
		{"12abc", "invalid number literal"},
		{"in", `unexpected keyword "in"`},
		{"#", "unexpected character"},
		{strings.Repeat("(", 100) + "1" + strings.Repeat(")", 100), "nested deeper than"},
		{strings.Repeat("!", 200) + "true", "nested deeper than"},
		{strings.Repeat("a", MaxExprLen+1), "the limit is"},
	}
	for _, tt := range tests {
		t.Run(clip(tt.src, 30), func(t *testing.T) {
			_, err := ParseExpr(tt.src)
			if err == nil {
				t.Fatalf("expected error")
			}
			var ee *ExprError
			if !errors.As(err, &ee) {
				t.Fatalf("error is %T, want *ExprError", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}

func TestExprEvalErrors(t *testing.T) {
	tests := []struct {
		src, want string
	}{
		{"nosuch", `unknown identifier "nosuch"`},
		{"1 / 0", "division by zero"},
		{"5 % 0", "modulo by zero"},
		{"'a' - 1", "cannot apply -"},
		{"1 < 'a'", "cannot compare"},
		{"-'a'", "cannot negate"},
		{"inputs.name.x", `cannot read field "x" of a string`},
		{"inputs.targets['x']", "list index must be an integer"},
		{"inputs.obj[1]", "map index must be a string"},
		{"1 in 'abc'", "left side must be a string"},
		{"'a' in 5", "cannot search a number"},
		{"len(5)", "cannot take the length"},
		{"matches('a', '(')", "matches()"},
		{"format('{1}', 'a')", "has no argument"},
		{"format('}')", "unmatched"},
		{"number('abc')", "is not a number"},
		{"fromJSON('{')", "fromJSON()"},
		{"success()", "only available in step `when` conditions"},
		{"upper(1)", "argument 1 must be a string"},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			e, err := ParseExpr(tt.src)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			_, err = e.Eval(exprVars())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %v does not contain %q", err, tt.want)
			}
		})
	}
}

func TestExprRefs(t *testing.T) {
	e, err := ParseExpr("steps.build.outputs.bin + steps['run-tests'].outputs[inputs.key].x + format('{0}', env.HOME) + matrix.os")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range e.Refs() {
		got = append(got, r.String())
	}
	want := []string{"steps.build.outputs.bin", "inputs.key", "steps.run-tests.outputs", "env.HOME", "matrix.os"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("refs = %q, want %q", got, want)
	}
	if calls := e.Calls(); !reflect.DeepEqual(calls, []string{"format"}) {
		t.Fatalf("calls = %q", calls)
	}
}

func TestTemplate(t *testing.T) {
	vars := exprVars()
	tests := []struct {
		src  string
		want any
	}{
		{"plain", "plain"},
		{"Hello ${inputs.name}!", "Hello World!"},
		{"${inputs.count}", 3.0}, // a lone expression keeps its type
		{"n=${inputs.count}", "n=3"},
		{"${inputs.targets}", []any{"linux", "darwin"}},
		{"list=${inputs.targets}", `list=["linux","darwin"]`},
		{"$${literal} ${'}'}", "${literal} }"},
		{"${ inputs.flag && true }", true},
		{"${format('{0}', '}')}x", "}x"},
		{"${inputs.missing}|", "|"},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			tpl, err := ParseTemplate(tt.src)
			if err != nil {
				t.Fatal(err)
			}
			got, err := tpl.Eval(vars)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %#v, want %#v", got, tt.want)
			}
		})
	}
	for _, bad := range []string{"${", "x ${inputs.name", "${}", "${1 +}"} {
		if _, err := ParseTemplate(bad); err == nil {
			t.Errorf("ParseTemplate(%q) succeeded", bad)
		}
	}
}

func FuzzParseExpr(f *testing.F) {
	for _, s := range []string{
		"1 + 2 * 3", "inputs.a == 'x' && !(b in [1, 2])", "steps['a-b'].outputs.c[0]",
		"format('{0}', x)", `"é\n"`, "-(-1) % 3", "a.b.c.d[e[f]]", "len(split(s, ','))",
		"((((1))))", "matches(x, '^a+$')", "1e400", "'unterminated", "a ||| b",
	} {
		f.Add(s)
	}
	vars := exprVars()
	f.Fuzz(func(t *testing.T, src string) {
		e, err := ParseExpr(src)
		if err != nil {
			var ee *ExprError
			if !errors.As(err, &ee) {
				t.Fatalf("parse error is %T, want *ExprError", err)
			}
			return
		}
		// Parsing is deterministic and evaluation never panics.
		if _, err := ParseExpr(src); err != nil {
			t.Fatalf("second parse failed: %v", err)
		}
		_, _ = e.Eval(vars)
		_ = e.Refs()
		_ = e.Calls()
		if _, err := ParseTemplate("${" + src + "}"); err != nil && !strings.ContainsAny(src, "}'\"") {
			t.Fatalf("template of valid expression failed: %v", err)
		}
	})
}
