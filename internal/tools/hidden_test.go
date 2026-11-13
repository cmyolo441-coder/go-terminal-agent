package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Env.Hidden keeps files a policy denies out of list_dir, glob and grep.
func TestHiddenPathsAreLeftOutOfSearches(t *testing.T) {
	env := newEnv(t)
	for name, body := range map[string]string{
		"main.go":            "package main // needle\n",
		".env":               "TOKEN=needle\n",
		"private/keys.go":    "package keys // needle\n",
		"private/sub/more.g": "needle\n",
	} {
		p := filepath.Join(env.Root, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	env.Hidden = func(abs string) bool {
		rel, _ := filepath.Rel(env.Root, abs)
		rel = filepath.ToSlash(rel)
		return rel == ".env" || rel == "private" || strings.HasPrefix(rel, "private/")
	}
	for _, c := range []struct {
		tool  Tool
		input map[string]any
	}{
		{ListDir{}, map[string]any{"depth": 5}},
		{Glob{}, map[string]any{"pattern": "**/*"}},
		{Grep{}, map[string]any{"pattern": "needle"}},
		{Grep{}, map[string]any{"pattern": "needle", "output_mode": "files"}},
		{Grep{}, map[string]any{"pattern": "needle", "path": ".env"}},
	} {
		out, err := run(t, env, c.tool, c.input)
		if err != nil {
			t.Fatalf("%T %v: %v", c.tool, c.input, err)
		}
		if strings.Contains(out, ".env") || strings.Contains(out, "private") || strings.Contains(out, "keys.go") || strings.Contains(out, "TOKEN") {
			t.Errorf("%T %v revealed a hidden path:\n%s", c.tool, c.input, out)
		}
		if c.input["path"] == nil && !strings.Contains(out, "main.go") {
			t.Errorf("%T %v lost a visible file:\n%s", c.tool, c.input, out)
		}
	}
}

func TestForkKeepsSettings(t *testing.T) {
	env := newEnv(t)
	env.Wrap = func(shell, command string) ([]string, error) { return nil, nil }
	env.Hidden = func(string) bool { return true }
	env.CheckCommand = func(string) error { return nil }
	env.ShellTimeout = 7
	f := env.Fork()
	if f.Root != env.Root || f.Wrap == nil || f.Hidden == nil || f.CheckCommand == nil || f.ShellTimeout != 7 || f.OutputLimit != env.OutputLimit {
		t.Fatalf("fork lost settings: %+v", f)
	}
	env.markRead(filepath.Join(env.Root, "x"))
	if len(f.readMods) != 0 {
		t.Fatal("fork shares read tracking")
	}
}
