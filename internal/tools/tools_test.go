package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func run(t *testing.T, env *Env, name string, in any) Result {
	t.Helper()
	tool, ok := Builtin().Get(name)
	if !ok {
		t.Fatalf("no tool %s", name)
	}
	b, _ := json.Marshal(in)
	return tool.Run(context.Background(), b, env)
}

func TestReadEditWrite(t *testing.T) {
	dir := t.TempDir()
	env := NewEnv(dir)
	p := filepath.Join(dir, "a.txt")
	os.WriteFile(p, []byte("one\ntwo\nthree\ntwo\n"), 0o600)

	if r := run(t, env, "Edit", map[string]any{"file_path": "a.txt", "old_string": "one", "new_string": "1"}); !r.IsError || !strings.Contains(r.Text, "Read it first") {
		t.Fatalf("edit before read should fail: %+v", r)
	}
	r := run(t, env, "Read", map[string]any{"file_path": "a.txt", "offset": 2, "limit": 2})
	if r.IsError || !strings.Contains(r.Text, "     2\ttwo") || strings.Contains(r.Text, "one") || !strings.Contains(r.Text, "offset=4") {
		t.Fatalf("read: %q", r.Text)
	}
	if r := run(t, env, "Edit", map[string]any{"file_path": "a.txt", "old_string": "two", "new_string": "2"}); !r.IsError || !strings.Contains(r.Text, "2 times") {
		t.Fatalf("ambiguous edit should fail: %+v", r)
	}
	if r := run(t, env, "Edit", map[string]any{"file_path": "a.txt", "old_string": "two", "new_string": "2", "replace_all": true}); r.IsError {
		t.Fatalf("replace_all: %+v", r)
	}
	if r := run(t, env, "Edit", map[string]any{"file_path": "a.txt", "old_string": "three", "new_string": "3"}); r.IsError {
		t.Fatalf("second edit without re-reading should work: %+v", r)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "one\n2\n3\n2\n" {
		t.Fatalf("content = %q", b)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Errorf("file mode changed to %v", st.Mode().Perm())
	}

	// A change made behind the agent's back must be re-read first.
	future := time.Now().Add(time.Hour)
	os.WriteFile(p, []byte("changed\n"), 0o600)
	os.Chtimes(p, future, future)
	if r := run(t, env, "Write", map[string]any{"file_path": "a.txt", "content": "x"}); !r.IsError || !strings.Contains(r.Text, "changed on disk") {
		t.Fatalf("stale write should fail: %+v", r)
	}
	if r := run(t, env, "Write", map[string]any{"file_path": "sub/new.txt", "content": "x\n"}); r.IsError {
		t.Fatalf("write new file: %+v", r)
	}
	if r := run(t, env, "Write", map[string]any{"file_path": "sub/new.txt"}); !r.IsError || !strings.Contains(r.Text, "content") {
		t.Fatalf("missing required field should fail: %+v", r)
	}
}

func TestGlobAndGrep(t *testing.T) {
	dir := t.TempDir()
	env := NewEnv(dir)
	files := map[string]string{
		"main.go": "package main\nfunc Hello() {}\n", "pkg/util/util.go": "package util\n// hello world\n",
		"pkg/util/util_test.go": "package util\n", "web/app.ts": "const hello = 1\n", "node_modules/x/x.go": "package x\nfunc Hello() {}\n",
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(content), 0o644)
	}
	r := run(t, env, "Glob", map[string]any{"pattern": "**/*.go"})
	for _, want := range []string{"main.go", "pkg/util/util.go", "pkg/util/util_test.go"} {
		if !strings.Contains(r.Text, want) {
			t.Errorf("glob missing %s in %q", want, r.Text)
		}
	}
	if strings.Contains(r.Text, "node_modules") || strings.Contains(r.Text, "app.ts") {
		t.Errorf("glob returned excluded files: %q", r.Text)
	}
	if r := run(t, env, "Glob", map[string]any{"pattern": "pkg/**/*_test.go"}); strings.TrimSpace(r.Text) != "pkg/util/util_test.go" {
		t.Errorf("glob subdir = %q", r.Text)
	}
	if r := run(t, env, "Glob", map[string]any{"pattern": "*.{ts,tsx}"}); strings.TrimSpace(r.Text) != "web/app.ts" {
		t.Errorf("glob braces = %q", r.Text)
	}

	for _, engine := range []string{"builtin", "auto"} {
		if engine == "builtin" {
			t.Setenv("GOCODE_NO_RG", "1")
		} else {
			t.Setenv("GOCODE_NO_RG", "")
		}
		r := run(t, env, "Grep", map[string]any{"pattern": "hello", "-i": true})
		for _, want := range []string{"main.go", "pkg/util/util.go", "web/app.ts"} {
			if !strings.Contains(r.Text, want) {
				t.Errorf("[%s] grep missing %s in %q", engine, want, r.Text)
			}
		}
		if strings.Contains(r.Text, "node_modules") {
			t.Errorf("[%s] grep searched node_modules", engine)
		}
		r = run(t, env, "Grep", map[string]any{"pattern": "func Hello", "output_mode": "content", "glob": "*.go"})
		if !strings.Contains(r.Text, "main.go:2:func Hello() {}") {
			t.Errorf("[%s] grep content = %q", engine, r.Text)
		}
		if r := run(t, env, "Grep", map[string]any{"pattern": "nomatchhere"}); r.Text != "No matches found" {
			t.Errorf("[%s] no-match = %q", engine, r.Text)
		}
	}
}

func TestBash(t *testing.T) {
	env := NewEnv(t.TempDir())
	if r := run(t, env, "Bash", map[string]any{"command": "echo out; echo err >&2; exit 3"}); !r.IsError ||
		!strings.Contains(r.Text, "out") || !strings.Contains(r.Text, "err") || !strings.Contains(r.Text, "exit code 3") {
		t.Errorf("bash failure: %+v", r)
	}
	start := time.Now()
	r := run(t, env, "Bash", map[string]any{"command": "sleep 30 & sleep 30", "timeout": 300})
	if !r.IsError || !strings.Contains(r.Text, "timed out") || time.Since(start) > 5*time.Second {
		t.Errorf("timeout should kill the whole process group quickly: %+v after %s", r, time.Since(start))
	}

	r = run(t, env, "Bash", map[string]any{"command": "echo started; sleep 30", "run_in_background": true})
	if r.IsError || !strings.Contains(r.Text, "bash_1") {
		t.Fatalf("background: %+v", r)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		out := run(t, env, "BashOutput", map[string]any{"shell_id": "bash_1"})
		if strings.Contains(out.Text, "started") && strings.Contains(out.Text, "running") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no background output: %q", out.Text)
		}
		time.Sleep(50 * time.Millisecond)
	}
	run(t, env, "KillShell", map[string]any{"shell_id": "bash_1"})
	deadline = time.Now().Add(3 * time.Second)
	for {
		if out := run(t, env, "BashOutput", map[string]any{"shell_id": "bash_1"}); strings.Contains(out.Text, "exited") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background shell was not killed")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestHTMLToText(t *testing.T) {
	got := HTMLToText(`<html><head><title>x</title><style>p{}</style></head><body><h1>Title</h1><p>Hello &amp; <a href="https://e.com">link</a></p><script>bad()</script><ul><li>one</li><li>two</li></ul></body></html>`)
	for _, want := range []string{"# Title", "Hello & [link](https://e.com)", "- one", "- two"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "bad()") || strings.Contains(got, "p{}") {
		t.Errorf("script or style leaked: %q", got)
	}
}
