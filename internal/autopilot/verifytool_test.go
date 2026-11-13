package autopilot

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permission"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

func envAt(t *testing.T, root string) *tools.Env {
	t.Helper()
	env, err := tools.NewEnv(root)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func TestVerifyToolRunsDetectedTests(t *testing.T) {
	root := brokenModule(t)
	tool := NewVerifyTool(VerifyToolConfig{})
	if tool.Kind() != tools.KindExec || tool.Spec().Name != VerifyToolName {
		t.Fatalf("spec %+v", tool.Spec())
	}
	env := envAt(t, root)
	c, err := tool.Prepare(env, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != tools.KindExec || c.Command != "go test ./..." || c.Summary != "Verify: go test ./..." {
		t.Fatalf("call %+v", c)
	}
	// The gate's allow-list applies to the test command like to bash.
	gate := permission.NewGate(permission.ModeAsk, []string{"go test"})
	if v, _ := gate.Check(c); v != permission.Allow {
		t.Fatal("an allow-listed test command must run without asking")
	}
	if v, _ := permission.NewGate(permission.ModePlan, nil).Check(c); v != permission.Deny {
		t.Fatal("plan mode must refuse the verify tool")
	}
	out, err := c.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "1 failure") {
		t.Fatalf("a failing suite must be an error result: %v", err)
	}
	for _, s := range []string{"FAIL go test ./...", "TestAdd", "calc_test.go:7", "Add(2, 3) = -1, want 5"} {
		if !strings.Contains(out, s) {
			t.Errorf("report lacks %q:\n%s", s, out)
		}
	}

	if err := os.WriteFile(filepath.Join(root, "calc.go"), []byte("package calc\n\nfunc Add(a, b int) int { return a + b }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err = tool.Prepare(env, json.RawMessage(`{"filter": "TestAdd", "timeout_sec": 120}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Command != "go test -run 'TestAdd' ./..." {
		t.Fatalf("filtered command %q", c.Command)
	}
	out, err = c.Run(context.Background())
	if err != nil || !strings.HasPrefix(out, "PASS go test -run 'TestAdd' ./...") {
		t.Fatalf("passing run: %v\n%s", err, out)
	}
}

func TestVerifyToolInputValidation(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"go.mod": "module x\n"})
	tool := NewVerifyTool(VerifyToolConfig{})
	env := envAt(t, root)
	for _, in := range []string{`{"bogus": 1}`, `{"filter": "a\nb"}`, `{"timeout_sec": -1}`, `{} {}`, `{"filter": "` + strings.Repeat("x", 201) + `"}`} {
		if _, err := tool.Prepare(env, json.RawMessage(in)); err == nil {
			t.Errorf("input %s must be rejected", in)
		}
	}
	if _, err := tool.Prepare(env, nil); err != nil {
		t.Errorf("empty input means no filter: %v", err)
	}
	if _, err := tool.Prepare(envAt(t, t.TempDir()), json.RawMessage(`{}`)); !errors.Is(err, ErrNoTestCommand) {
		t.Errorf("no test command: %v", err)
	}
	custom := NewVerifyTool(VerifyToolConfig{Command: &TestCommand{Command: "make check"}})
	if !strings.Contains(custom.Spec().Description, "`make check`") {
		t.Error("a configured command should be named in the description")
	}
	if _, err := custom.Prepare(env, json.RawMessage(`{"filter": "x"}`)); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("filter on an unknown runner: %v", err)
	}
}

func TestWithFilter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX quoting")
	}
	cases := []struct {
		tc   TestCommand
		want string
	}{
		{TestCommand{Command: "go test ./...", Framework: FrameworkGo}, "go test -run 'it'\\''s' ./..."},
		{TestCommand{Command: "pytest", Framework: FrameworkPytest}, "pytest -k 'it'\\''s'"},
		{TestCommand{Command: "npm test", Framework: FrameworkJest}, "npm test -- -t 'it'\\''s'"},
		{TestCommand{Command: "npx vitest run", Framework: FrameworkVitest}, "npx vitest run -t 'it'\\''s'"},
		{TestCommand{Command: "cargo test", Framework: FrameworkCargo}, "cargo test 'it'\\''s'"},
		{TestCommand{Command: "mvn -q test", Framework: FrameworkMaven}, "mvn -q test -Dtest='it'\\''s'"},
		{TestCommand{Command: "./gradlew test", Framework: FrameworkGradle}, "./gradlew test --tests 'it'\\''s'"},
	}
	for _, c := range cases {
		got, err := withFilter(c.tc, "it's")
		if err != nil || got != c.want {
			t.Errorf("withFilter(%q) = %q, %v; want %q", c.tc.Command, got, err, c.want)
		}
	}
	if _, err := withFilter(TestCommand{Command: "make test", Framework: FrameworkAuto}, "x"); err == nil {
		t.Error("make test has no filter syntax")
	}
}

func TestVerifyToolInAgentLoop(t *testing.T) {
	root := brokenModule(t)
	p := newFake().on(roleMain,
		call(use(VerifyToolName, map[string]any{})),
		say("The suite fails in TestAdd."),
	)
	a := newMainAgent(t, p, root, permission.ModeYolo)
	a.SetRegistry(tools.NewRegistry(tools.ReadFile{}, NewVerifyTool(VerifyToolConfig{})))
	if err := a.Run(context.Background(), llm.UserText("run the tests"), &uiSink{}); err != nil {
		t.Fatal(err)
	}
	res := a.History()[2].Blocks[0]
	if res.Type != llm.BlockToolResult || !res.IsError || !strings.Contains(res.Text, "TestAdd") || !strings.Contains(res.Text, "Error: 1 failure") {
		t.Fatalf("tool result %+v", res)
	}
}
