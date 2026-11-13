package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

func runTool(t *testing.T, tool tools.Tool, env *tools.Env, input string) (string, error) {
	t.Helper()
	call, err := tool.Prepare(env, json.RawMessage(input))
	if err != nil {
		return "", err
	}
	if call.Kind != tools.KindRead || call.Summary == "" {
		t.Errorf("call %+v", call)
	}
	return call.Run(testCtx(t))
}

func toolEnv(t *testing.T, root string) *tools.Env {
	t.Helper()
	env, err := tools.NewEnv(root)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func TestToolSpecs(t *testing.T) {
	ts := Tools(nil)
	if len(ts) != 2 {
		t.Fatalf("tools %d", len(ts))
	}
	names := map[string]bool{}
	for _, tool := range ts {
		spec := tool.Spec()
		names[spec.Name] = true
		if tool.Kind() != tools.KindRead || spec.Description == "" || len(spec.Properties) == 0 {
			t.Errorf("spec %+v", spec)
		}
		if _, err := json.Marshal(spec); err != nil {
			t.Errorf("%s: %v", spec.Name, err)
		}
	}
	if !names["diagnostics"] || !names["code_nav"] {
		t.Fatalf("names %v", names)
	}
	// Without a manager the tools refuse cleanly.
	env := toolEnv(t, t.TempDir())
	if _, err := ts[0].Prepare(env, json.RawMessage(`{"path":"a.go"}`)); err == nil {
		t.Error("diagnostics without a manager")
	}
	if _, err := ts[1].Prepare(env, json.RawMessage(`{"operation":"hover","path":"a.go","line":1,"column":1}`)); err == nil {
		t.Error("code_nav without a manager")
	}
}

func TestDiagnosticsTool(t *testing.T) {
	m, root, _ := newFakeManager(t, "", nil)
	env := toolEnv(t, root)
	tool := DiagnosticsTool{Manager: m}
	writeFile(t, filepath.Join(root, "a.fk"), sampleSource)
	writeFile(t, filepath.Join(root, "clean.fk"), "fine\n")
	writeFile(t, filepath.Join(root, "notes.txt"), "ERROR\n")

	out, err := runTool(t, tool, env, `{"path":"a.fk"}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"a.fk (fake): 1 error, 1 warning", "L3:14 error: found ERROR", "L4:2 warning: found WARN … [fake 42]"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	out, err = runTool(t, tool, env, `{"paths":["a.fk","clean.fk","notes.txt","gone.fk"],"severity":"error","wait_ms":3000}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"a.fk (fake): 1 error, 1 warning",
		"clean.fk (fake): no problems",
		"notes.txt: no language server is configured",
		"gone.fk: diagnostics unavailable",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "warning: found WARN") {
		t.Errorf("severity filter ignored:\n%s", out)
	}

	bad := []string{
		`{}`,
		`{"path":"a.fk","extra":1}`,
		`{"path":"a.fk","severity":"fatal"}`,
		`{"path":"a.fk","wait_ms":-1}`,
		`{"path":"a.fk"} trailing`,
		`{"path":"."}`,
		`{"paths":[""]}`,
		fmt.Sprintf(`{"paths":[%s]}`, strings.TrimSuffix(strings.Repeat(`"a.fk",`, 21), ",")),
	}
	for _, in := range bad {
		if _, err := tool.Prepare(env, json.RawMessage(in)); err == nil {
			t.Errorf("accepted %s", in)
		}
	}
}

func TestDiagnosticsToolMissingServer(t *testing.T) {
	m, root, _ := newFakeManager(t, "", func(o *Options, c *ServerConfig) { c.Command = "gta-no-such-language-server" })
	writeFile(t, filepath.Join(root, "a.fk"), "x\n")
	out, err := runTool(t, DiagnosticsTool{Manager: m}, toolEnv(t, root), `{"path":"a.fk"}`)
	if err != nil || !strings.Contains(out, "language server not installed") || !strings.Contains(out, "gta-no-such-language-server") {
		t.Fatalf("output %q %v", out, err)
	}
}

func TestDiagnosticsToolStaleNote(t *testing.T) {
	m, root, _ := newFakeManager(t, "silent", nil)
	writeFile(t, filepath.Join(root, "a.fk"), "x\n")
	out, err := runTool(t, DiagnosticsTool{Manager: m}, toolEnv(t, root), `{"path":"a.fk","wait_ms":200}`)
	if err != nil || !strings.Contains(out, "had not finished analysing") {
		t.Fatalf("output %q %v", out, err)
	}
}

func TestNavTool(t *testing.T) {
	m, root, _ := newFakeManager(t, "", nil)
	env := toolEnv(t, root)
	tool := NavTool{Manager: m}
	writeFile(t, filepath.Join(root, "a.fk"), navA)
	writeFile(t, filepath.Join(root, "pkg", "b.fk"), navB)

	out, err := runTool(t, tool, env, `{"operation":"definition","path":"a.fk","line":3,"symbol":"helper"}`)
	if err != nil || out != "a.fk:1:6  func helper() {}\n" {
		t.Fatalf("definition %q %v", out, err)
	}
	out, err = runTool(t, tool, env, `{"operation":"references","path":"pkg/b.fk","line":2,"symbol":"helper","include_declaration":false}`)
	if err != nil || !strings.HasPrefix(out, "2 references\n") || !strings.Contains(out, "pkg/b.fk:2:2  helper()") {
		t.Fatalf("references %q %v", out, err)
	}
	out, err = runTool(t, tool, env, `{"operation":"hover","path":"a.fk","line":1,"column":7}`)
	if err != nil || !strings.Contains(out, "**helper**") {
		t.Fatalf("hover %q %v", out, err)
	}
	out, err = runTool(t, tool, env, `{"operation":"hover","path":"a.fk","line":1,"column":17}`)
	if err != nil || out != "No hover information at that position." {
		t.Fatalf("empty hover %q %v", out, err)
	}
	if _, err := runTool(t, tool, env, `{"operation":"definition","path":"a.fk","line":1,"symbol":"absent"}`); err == nil {
		t.Fatal("missing symbol accepted")
	}

	bad := []string{
		`{"operation":"rename","path":"a.fk","line":1,"column":1}`,
		`{"operation":"hover","line":1,"column":1}`,
		`{"operation":"hover","path":"a.fk","line":0,"column":1}`,
		`{"operation":"hover","path":"a.fk","line":1}`,
		`{"operation":"hover","path":"a.fk","line":1,"column":-2}`,
		`{"operation":"hover","path":"a.fk","line":1,"column":1,"bogus":true}`,
	}
	for _, in := range bad {
		if _, err := tool.Prepare(env, json.RawMessage(in)); err == nil {
			t.Errorf("accepted %s", in)
		}
	}
}

func TestLimitOutput(t *testing.T) {
	s := strings.Repeat("é", 100)
	out := limitOutput(s, 50)
	if len(out) > 90 || !strings.Contains(out, "bytes omitted") || !strings.HasPrefix(out, "é") || !strings.HasSuffix(out, "é") {
		t.Fatalf("limited %q", out)
	}
	for _, r := range out {
		if r == '\uFFFD' {
			t.Fatal("cut inside a rune")
		}
	}
	if limitOutput("short", 50) != "short" {
		t.Fatal("short text changed")
	}
}

// TestGoplsIntegration runs against a real gopls when one is installed.
func TestGoplsIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not installed")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/m\n\ngo 1.21\n")
	path := writeFile(t, filepath.Join(root, "main.go"), "package main\n\nfunc helper() int { return 1 }\n\nfunc main() {\n\tvar s string = helper()\n\t_ = s\n}\n")
	m, err := NewManager(Options{Root: root, DiagnosticsWait: 60 * time.Second, StartTimeout: 2 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = m.Close(ctx)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	out := m.AfterEdit(ctx, path)
	if out == "" {
		// The first analysis of a fresh module can outlast AfterEdit's
		// bound; ask again with a long wait.
		res, err := m.Diagnostics(ctx, path, 60*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		out = FormatDiagnostics(res, 0)
	}
	if !strings.Contains(out, "L6:") || !strings.Contains(strings.ToLower(out), "cannot use") {
		t.Fatalf("gopls did not report the type error:\n%s", out)
	}
	locs, err := m.Definition(ctx, path, 6, 17)
	if err != nil || len(locs) == 0 || locs[0].Range.Start.Line != 2 {
		t.Fatalf("definition %+v %v", locs, err)
	}
	hover, err := m.Hover(ctx, path, 6, 17)
	if err != nil || !strings.Contains(hover, "func helper() int") {
		t.Fatalf("hover %q %v", hover, err)
	}
	writeFile(t, path, "package main\n\nfunc main() {}\n")
	res, err := m.Diagnostics(ctx, path, 60*time.Second)
	if err != nil || !res.Fresh {
		t.Fatalf("after fix %+v %v", res, err)
	}
	for _, d := range res.Diagnostics {
		if d.EffectiveSeverity() == SeverityError {
			t.Fatalf("error after fix: %+v", d)
		}
	}
}
