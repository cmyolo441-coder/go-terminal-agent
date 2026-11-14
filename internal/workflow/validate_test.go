package workflow

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testRegistry() *Registry {
	r := NewBuiltinRegistry(BuiltinOptions{
		Shell: func(context.Context, ShellCommand) (ShellResult, error) { return ShellResult{}, nil },
	})
	return r
}

func TestParseSpecStrict(t *testing.T) {
	tests := []struct {
		name, src, want string
	}{
		{"unknown top-level field", `{"steps":[], "stpes": []}`, `unknown field "stpes"`},
		{"unknown step field", `{"steps":[{"id":"a","type":"noop","dependson":["b"]}]}`, `unknown field "dependson"`},
		{"unknown retry field", `{"steps":[{"id":"a","type":"noop","retry":{"attempts":3}}]}`, `unknown field "attempts"`},
		{"trailing data", `{"steps":[]} {}`, "unexpected data after"},
		{"bad duration", `{"steps":[{"id":"a","type":"noop","timeout":"soon"}]}`, `invalid duration "soon"`},
		{"negative duration", `{"steps":[{"id":"a","type":"noop","timeout":-1}]}`, "invalid duration"},
		{"syntax error position", "{\n  \"steps\": [,]\n}", "line 2, column"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseSpec([]byte(tt.src))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %v does not contain %q", err, tt.want)
			}
		})
	}
	s, err := ParseSpec([]byte(`{"steps":[{"id":"a","type":"sleep","timeout":1.5,"inputs":{"duration":"1s"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Steps[0].Timeout.D(); got != 1500*time.Millisecond {
		t.Fatalf("timeout = %v", got)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name string
		spec string
		want []string // substrings that must all appear; empty means valid
	}{
		{"valid", `{
			"inputs": {"target": {"type": "string", "default": "x"}},
			"groups": {"db": 1},
			"steps": [
				{"id": "a", "type": "noop", "outputs": ["v"], "inputs": {"v": "${inputs.target}"}},
				{"id": "b", "type": "noop", "depends_on": ["a"], "group": "db",
				 "inputs": {"x": "${steps.a.outputs.v} ${env.HOME} ${run.id}"},
				 "when": "steps.a.status == 'succeeded'",
				 "compensate": {"type": "noop", "inputs": {"undo": "${self.outputs.x}"}}},
				{"id": "m", "type": "noop", "depends_on": ["b"], "matrix": {"os": ["l", "d"]}, "max_parallel": 1,
				 "inputs": {"os": "${matrix.os}"}},
				{"id": "z", "type": "noop", "depends_on": ["m"], "when": "${failure() || always()}",
				 "inputs": {"all": "${steps.m.outputs.os}", "first": "${steps.m.instances[0].status}"}}
			],
			"outputs": {"result": "${steps.z.outputs.all}"}
		}`, nil},
		{"cycle with path", `{"steps": [
			{"id": "a", "type": "noop", "depends_on": ["c"]},
			{"id": "b", "type": "noop", "depends_on": ["a"]},
			{"id": "c", "type": "noop", "depends_on": ["b"]}]}`,
			[]string{"dependency cycle: a -> c -> b -> a"}},
		{"self dependency", `{"steps": [{"id": "a", "type": "noop", "depends_on": ["a"]}]}`,
			[]string{"step depends on itself"}},
		{"missing dependency with suggestion", `{"steps": [
			{"id": "build", "type": "noop"},
			{"id": "test", "type": "noop", "depends_on": ["biuld"]}]}`,
			[]string{`depends on unknown step "biuld" (did you mean "build"?)`}},
		{"duplicate ids and deps", `{"steps": [
			{"id": "a", "type": "noop"}, {"id": "a", "type": "noop"},
			{"id": "b", "type": "noop", "depends_on": ["a", "a"]}]}`,
			[]string{`duplicate step id "a"`, `duplicate dependency "a"`}},
		{"bad id and type", `{"steps": [{"id": "1x", "type": "nope"}]}`,
			[]string{"invalid step id", `unknown step type "nope"`}},
		{"no steps", `{"steps": []}`, []string{"workflow has no steps"}},
		{"reference to non-ancestor", `{"steps": [
			{"id": "a", "type": "noop"},
			{"id": "b", "type": "noop", "inputs": {"x": "${steps.a.outputs.v}"}}]}`,
			[]string{`step "a" is not an upstream dependency of "b"`}},
		{"undeclared output", `{"steps": [
			{"id": "a", "type": "noop", "outputs": ["v"]},
			{"id": "b", "type": "noop", "depends_on": ["a"], "inputs": {"x": "${steps.a.outputs.w}"}}]}`,
			[]string{`step "a" does not declare output "w"`}},
		{"undeclared input", `{"steps": [{"id": "a", "type": "noop", "inputs": {"x": "${inputs.nope}"}}]}`,
			[]string{`undeclared input "nope"`}},
		{"unknown root", `{"steps": [{"id": "a", "type": "noop", "inputs": {"x": "${secrets.k}"}}]}`,
			[]string{`unknown identifier "secrets"`}},
		{"unknown step ref", `{"steps": [{"id": "a", "type": "noop", "inputs": {"x": "${steps.zz.outputs}"}}]}`,
			[]string{`unknown step "zz"`}},
		{"status function outside when", `{"steps": [{"id": "a", "type": "noop", "inputs": {"x": "${success()}"}}]}`,
			[]string{"success() is only allowed in `when` conditions"}},
		{"matrix without matrix", `{"steps": [{"id": "a", "type": "noop", "inputs": {"x": "${matrix.os}"}}]}`,
			[]string{"the step has no matrix"}},
		{"unknown matrix axis", `{"steps": [{"id": "a", "type": "noop", "matrix": {"os": ["l"]}, "inputs": {"x": "${matrix.arch}"}}]}`,
			[]string{`unknown matrix axis "arch"`}},
		{"bad matrix", `{"steps": [{"id": "a", "type": "noop", "matrix": {"os": [], "v": [1, 1], "o": [{"a": 1}], "s": "plain"}}]}`,
			[]string{"matrix axis has no values", `duplicate matrix value "1"`, "must be strings, numbers or booleans", "must be a list of values"}},
		{"matrix expression reads steps", `{"steps": [
			{"id": "a", "type": "noop"},
			{"id": "b", "type": "noop", "depends_on": ["a"], "matrix": {"x": "${steps.a.outputs.list}"}}]}`,
			[]string{"matrix expressions may only read inputs and env"}},
		{"self outside compensation", `{"steps": [{"id": "a", "type": "noop", "inputs": {"x": "${self.outputs}"}}]}`,
			[]string{"self is only available in compensation inputs"}},
		{"env whole map", `{"steps": [{"id": "a", "type": "noop", "inputs": {"x": "${env}"}}]}`,
			[]string{"read environment variables by name"}},
		{"bad when", `{"steps": [{"id": "a", "type": "noop", "when": "a = b"}]}`,
			[]string{"use == to compare"}},
		{"unknown group and retry problems", `{"steps": [{"id": "a", "type": "noop", "group": "gpu",
			"retry": {"max_attempts": 1000, "multiplier": 0.5, "retry_on": ["permanent", "weird"]}}]}`,
			[]string{`unknown concurrency group "gpu"`, "max_attempts: must be between", "multiplier: must be", "permanent errors are never retried", `unknown error class "weird"`}},
		{"executor inputs checked", `{"steps": [
			{"id": "s", "type": "shell", "inputs": {"cmd": "ls"}},
			{"id": "z", "type": "sleep"}]}`,
			[]string{`missing required input "command" for type "shell"`, "steps.s.inputs.cmd: unknown input", `missing required input "duration"`}},
		{"max_parallel without matrix", `{"steps": [{"id": "a", "type": "noop", "max_parallel": 2}]}`,
			[]string{"only applies to matrix steps"}},
		{"input declarations", `{"inputs": {"n": {"type": "number", "default": "x"}, "r": {"required": true, "default": 1}, "t": {"type": "weird"}},
			"steps": [{"id": "a", "type": "noop"}]}`,
			[]string{"want a number, got a string", "a required input cannot have a default", `unknown type "weird"`}},
		{"bad version and limits", `{"version": 2, "concurrency": -1, "groups": {"g": 0}, "steps": [{"id": "a", "type": "noop"}]}`,
			[]string{"unsupported version 2", "concurrency: must not be negative", "limit must be at least 1"}},
		{"compensation type", `{"steps": [{"id": "a", "type": "noop", "compensate": {"type": "nope"}}]}`,
			[]string{`steps.a.compensate.type: unknown step type "nope"`}},
		{"workflow output reference", `{"steps": [{"id": "a", "type": "noop", "outputs": ["v"]}], "outputs": {"o": "${steps.a.outputs.w}"}}`,
			[]string{`does not declare output "w"`}},
	}
	reg := testRegistry()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, err := ParseSpec([]byte(tt.spec))
			if err != nil {
				t.Fatal(err)
			}
			err = Validate(spec, reg)
			if len(tt.want) == 0 {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("error = %v, want *ValidationError", err)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error does not contain %q:\n%v", w, err)
				}
			}
		})
	}
}

func TestBindInputs(t *testing.T) {
	spec := &Spec{Inputs: map[string]InputSpec{
		"req": {Type: "string", Required: true},
		"n":   {Type: "number", Default: 3},
		"opt": {},
	}}
	got, err := BindInputs(spec, map[string]any{"req": "x"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"req": "x", "n": 3.0, "opt": nil}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	_, err = BindInputs(spec, map[string]any{"n": "three", "extra": 1})
	for _, w := range []string{"inputs.extra: unknown input", "want a number", "inputs.req: required input is missing"} {
		if err == nil || !strings.Contains(err.Error(), w) {
			t.Errorf("error %v does not contain %q", err, w)
		}
	}
}

func TestPlanMatrixAndCriticalPath(t *testing.T) {
	spec, err := ParseSpec([]byte(`{
		"inputs": {"versions": {"type": "list", "default": ["1.25", "1.26"]}},
		"steps": [
			{"id": "fetch", "type": "noop", "estimate": "1s"},
			{"id": "build", "type": "noop", "depends_on": ["fetch"], "estimate": "5s",
			 "matrix": {"os": ["linux", "darwin"], "go": "${inputs.versions}"}},
			{"id": "lint", "type": "noop", "depends_on": ["fetch"], "estimate": "2s"},
			{"id": "release", "type": "noop", "depends_on": ["build", "lint"], "estimate": "1s"}
		]}`))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Compile(spec, testRegistry(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range plan.Steps() {
		ids = append(ids, s.ID)
	}
	want := []string{
		"fetch",
		"build[go=1.25,os=linux]", "build[go=1.25,os=darwin]",
		"build[go=1.26,os=linux]", "build[go=1.26,os=darwin]",
		"lint", "release",
	}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("order = %q\nwant    %q", ids, want)
	}
	release := plan.Steps()[6]
	if len(release.DependsOn) != 5 {
		t.Fatalf("release depends on %q, want the 4 build instances and lint", release.DependsOn)
	}
	path, total := plan.CriticalPath()
	if !reflect.DeepEqual(path, []string{"fetch", "build[go=1.25,os=linux]", "release"}) || total != 7*time.Second {
		t.Fatalf("critical path = %q (%v)", path, total)
	}
	levels := plan.Levels()
	if len(levels) != 3 || len(levels[1]) != 5 {
		t.Fatalf("levels = %q", levels)
	}
	var b strings.Builder
	if err := plan.Describe(&b); err != nil || !strings.Contains(b.String(), "critical path (~7.00s)") {
		t.Fatalf("describe: %v\n%s", err, b.String())
	}

	// A matrix expression must yield a list of scalars.
	if _, err := Compile(spec, testRegistry(), map[string]any{"versions": []any{}}, nil); err == nil ||
		!strings.Contains(err.Error(), "matrix axis has no values") {
		t.Fatalf("empty matrix error = %v", err)
	}
}
