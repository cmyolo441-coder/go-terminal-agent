package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/agent"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/codeintel"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/config"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/lsp"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

func TestCodeIntelWiring(t *testing.T) {
	t.Setenv("GTA_CONFIG_DIR", t.TempDir())
	root := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/demo\n\ngo 1.26\n")
	write("server.go", "package demo\n\n// Server answers requests.\ntype Server struct{ Addr string }\n\n// Start runs the server.\nfunc (s *Server) Start() error { return nil }\n")
	write("main.go", "package demo\n\nfunc run() error { s := &Server{}; return s.Start() }\n")

	cfg := config.Default()
	// A server that is not installed: LSP stays quiet instead of failing.
	cfg.LSPServers = []lsp.ServerConfig{{Name: "gopls", Command: "gta-no-such-gopls", Extensions: []string{".go"}}}
	ci := startCodeIntel(cfg, root, nil)
	defer ci.close()

	names := map[string]bool{}
	for _, tl := range ci.tools() {
		names[tl.Spec().Name] = true
	}
	for _, n := range []string{"code_search", "symbols", "repo_map", "diagnostics", "code_nav"} {
		if !names[n] {
			t.Errorf("tool %s missing; have %v", n, names)
		}
	}

	m := ci.systemPrompt()
	if !strings.Contains(m, "<repo-map>") || !strings.Contains(m, "server.go") || !strings.Contains(m, "Server") {
		t.Fatalf("repo map missing or empty:\n%s", m)
	}
	if ci.mapTokens == 0 {
		t.Fatal("map size not recorded")
	}

	ag := &agent.Agent{}
	ci.attach(ag)
	if ag.AfterWrite == nil {
		t.Fatal("AfterWrite not attached")
	}
	write("extra.go", "package demo\n\nfunc FreshlyAdded() {}\n")
	if out := ag.AfterWrite(context.Background(), &tools.Call{Path: filepath.Join(root, "extra.go")}); out != "" {
		t.Fatalf("no server installed, want no diagnostics, got %q", out)
	}
	hits, err := ci.ix.Symbols(context.Background(), codeintel.SymbolQuery{Name: "FreshlyAdded"})
	if err != nil || len(hits) == 0 {
		t.Fatalf("index did not pick up the edited file: %v %v", hits, err)
	}

	rows := map[string]string{}
	for _, c := range ci.capabilities() {
		rows[c.Name] = c.Value
	}
	if !strings.Contains(rows["index"], "files") || !strings.HasPrefix(rows["lsp"], "on") || strings.Contains(rows["lsp"], "gopls") {
		t.Fatalf("capability rows: %v", rows)
	}
}

func TestCodeIntelOff(t *testing.T) {
	cfg := config.Default()
	cfg.CodeIndex, cfg.LSP = false, false
	ci := startCodeIntel(cfg, t.TempDir(), nil)
	defer ci.close()
	if len(ci.tools()) != 0 || ci.systemPrompt() != "" {
		t.Fatal("parts that are off must add nothing")
	}
	ag := &agent.Agent{}
	ci.attach(ag)
	if ag.AfterWrite != nil {
		t.Fatal("AfterWrite set with everything off")
	}
}

// TestCodeIntelLiveDiagnostics runs a real language server when one is
// installed: an edit with a type error must come back with that error.
func TestCodeIntelLiveDiagnostics(t *testing.T) {
	if _, err := exec.LookPath("pyright-langserver"); err != nil {
		t.Skip("pyright-langserver not installed")
	}
	if testing.Short() {
		t.Skip("starts a language server")
	}
	t.Setenv("GTA_CONFIG_DIR", t.TempDir())
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "pyproject.toml"), []byte("[project]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.LSPDiagnosticsWaitMs = 20000
	ci := startCodeIntel(cfg, root, nil)
	defer ci.close()
	path := filepath.Join(root, "app.py")
	if err := os.WriteFile(path, []byte("def add(a: int, b: int) -> int:\n    return a + b\n\nx: str = add(1, 2)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := ci.afterWrite(context.Background(), &tools.Call{Path: path})
	if !strings.Contains(out, "app.py") || !strings.Contains(out, "4:") {
		t.Fatalf("want the type error on line 4, got %q", out)
	}
	t.Logf("diagnostics:\n%s", out)
}
