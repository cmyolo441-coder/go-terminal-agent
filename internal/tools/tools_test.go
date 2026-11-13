package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func newEnv(t *testing.T) *Env {
	t.Helper()
	env, err := NewEnv(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func run(t *testing.T, env *Env, tool Tool, input any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(input)
	call, err := tool.Prepare(env, raw)
	if err != nil {
		return "", err
	}
	return call.Run(context.Background())
}

func write(t *testing.T, env *Env, rel, content string) string {
	t.Helper()
	p := filepath.Join(env.Root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadFileLineNumbersAndPaging(t *testing.T) {
	env := newEnv(t)
	write(t, env, "a.txt", "one\ntwo\nthree\n")
	out, err := run(t, env, ReadFile{}, map[string]any{"path": "a.txt", "offset": 2, "limit": 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "     2\ttwo") || strings.Contains(out, "one") {
		t.Fatalf("unexpected output: %q", out)
	}
	if !strings.Contains(out, "offset=3") {
		t.Fatalf("expected continuation hint: %q", out)
	}
}

func TestReadFileRejectsBinaryAndDirs(t *testing.T) {
	env := newEnv(t)
	write(t, env, "bin", "a\x00b")
	if _, err := run(t, env, ReadFile{}, map[string]any{"path": "bin"}); err == nil {
		t.Fatal("expected binary error")
	}
	if _, err := run(t, env, ReadFile{}, map[string]any{"path": "."}); err == nil {
		t.Fatal("expected directory error")
	}
}

func TestStrictDecodeRejectsMalformedInput(t *testing.T) {
	env := newEnv(t)
	cases := []string{`{"path":"a.txt","bogus":1}`, `{"path":"a.txt"`, `{"path":"a.txt"} {}`, `{}`}
	for _, c := range cases {
		if _, err := (ReadFile{}).Prepare(env, json.RawMessage(c)); err == nil {
			t.Errorf("expected error for %s", c)
		}
	}
}

func TestWriteRequiresReadForExistingFile(t *testing.T) {
	env := newEnv(t)
	write(t, env, "a.txt", "old\n")
	if _, err := run(t, env, WriteFile{}, map[string]any{"path": "a.txt", "content": "new\n"}); err == nil {
		t.Fatal("expected read-before-write error")
	}
	if _, err := run(t, env, ReadFile{}, map[string]any{"path": "a.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, env, WriteFile{}, map[string]any{"path": "a.txt", "content": "new\n"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(env.Root, "a.txt"))
	if string(got) != "new\n" {
		t.Fatalf("got %q", got)
	}
}

func TestWriteCreatesNestedFileAndDiff(t *testing.T) {
	env := newEnv(t)
	raw, _ := json.Marshal(map[string]any{"path": "x/y/z.go", "content": "package z\n"})
	call, err := (WriteFile{}).Prepare(env, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(call.Preview, "+package z") || call.OutsideRoot {
		t.Fatalf("bad preview or root check: %+v", call)
	}
	if _, err := call.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(env.Root, "x/y/z.go")); err != nil {
		t.Fatal(err)
	}
}

func TestWriteDetectsExternalChange(t *testing.T) {
	env := newEnv(t)
	p := write(t, env, "a.txt", "v1\n")
	if _, err := run(t, env, ReadFile{}, map[string]any{"path": "a.txt"}); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(2 * time.Second)
	os.WriteFile(p, []byte("v2\n"), 0o644)
	os.Chtimes(p, later, later)
	if _, err := run(t, env, EditFile{}, map[string]any{"path": "a.txt", "old_string": "v2", "new_string": "v3"}); err == nil {
		t.Fatal("expected stale-file error")
	}
}

func TestEditFile(t *testing.T) {
	env := newEnv(t)
	write(t, env, "a.go", "foo()\nbar()\nfoo()\n")
	run(t, env, ReadFile{}, map[string]any{"path": "a.go"})

	_, err := run(t, env, EditFile{}, map[string]any{"path": "a.go", "old_string": "foo()", "new_string": "baz()"})
	if err == nil || !strings.Contains(err.Error(), "matches 2 places") {
		t.Fatalf("expected ambiguity error, got %v", err)
	}
	if _, err := run(t, env, EditFile{}, map[string]any{"path": "a.go", "old_string": "nope", "new_string": "x"}); err == nil {
		t.Fatal("expected not-found error")
	}
	out, err := run(t, env, EditFile{}, map[string]any{"path": "a.go", "old_string": "foo()", "new_string": "baz()", "replace_all": true})
	if err != nil || !strings.Contains(out, "2 replacements") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	got, _ := os.ReadFile(filepath.Join(env.Root, "a.go"))
	if string(got) != "baz()\nbar()\nbaz()\n" {
		t.Fatalf("got %q", got)
	}
}

func TestEditOutsideRootIsFlagged(t *testing.T) {
	env := newEnv(t)
	outside := filepath.Join(t.TempDir(), "o.txt")
	raw, _ := json.Marshal(map[string]any{"path": outside, "old_string": "", "new_string": "hi"})
	call, err := (EditFile{}).Prepare(env, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !call.OutsideRoot {
		t.Fatal("expected OutsideRoot")
	}
}

func TestUnifiedDiff(t *testing.T) {
	a := "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n"
	b := "1\n2\n3\n4\nFIVE\n6\n7\n8\n9\n10\n"
	d := UnifiedDiff("f", a, b)
	for _, want := range []string{"--- f", "@@ -2,7 +2,7 @@", "-5", "+FIVE", " 4"} {
		if !strings.Contains(d, want) {
			t.Fatalf("diff missing %q:\n%s", want, d)
		}
	}
	if UnifiedDiff("f", a, a) != "" {
		t.Fatal("identical inputs should give empty diff")
	}
}

func TestGlobSkipsVendorDirs(t *testing.T) {
	env := newEnv(t)
	write(t, env, "a/b.go", "")
	write(t, env, "c.go", "")
	write(t, env, "node_modules/x.go", "")
	out, err := run(t, env, Glob{}, map[string]any{"pattern": "**/*.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, filepath.Join("a", "b.go")) || !strings.Contains(out, "c.go") || strings.Contains(out, "node_modules") {
		t.Fatalf("unexpected: %q", out)
	}
}

func TestGrepModes(t *testing.T) {
	env := newEnv(t)
	write(t, env, "a.go", "package a\nfunc Hello() {}\n")
	write(t, env, "b.txt", "hello world\nbye\n")
	out, err := run(t, env, Grep{}, map[string]any{"pattern": "hello", "ignore_case": true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.go:2:func Hello") || !strings.Contains(out, "b.txt:1:hello world") {
		t.Fatalf("content mode: %q", out)
	}
	out, _ = run(t, env, Grep{}, map[string]any{"pattern": "hello", "include": "*.txt", "output_mode": "files"})
	if strings.TrimSpace(out) != "b.txt" {
		t.Fatalf("files mode: %q", out)
	}
	out, _ = run(t, env, Grep{}, map[string]any{"pattern": "e", "output_mode": "count"})
	if !strings.Contains(out, "total:") {
		t.Fatalf("count mode: %q", out)
	}
	if _, err := run(t, env, Grep{}, map[string]any{"pattern": "("}); err == nil {
		t.Fatal("expected regex error")
	}
}

func TestListDir(t *testing.T) {
	env := newEnv(t)
	write(t, env, "src/main.go", "")
	write(t, env, "README.md", "")
	out, err := run(t, env, ListDir{}, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "src/\n  main.go") || !strings.Contains(out, "README.md") {
		t.Fatalf("unexpected: %q", out)
	}
}

func TestBash(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix shell")
	}
	env := newEnv(t)
	out, err := run(t, env, Bash{}, map[string]any{"command": "echo hi && pwd"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hi") || !strings.Contains(out, env.Root) || !strings.Contains(out, "exit code 0") {
		t.Fatalf("unexpected: %q", out)
	}
	out, err = run(t, env, Bash{}, map[string]any{"command": "echo oops >&2; exit 3"})
	if err == nil || !strings.Contains(out, "oops") || !strings.Contains(out, "exit code 3") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	start := time.Now()
	_, err = run(t, env, Bash{}, map[string]any{"command": "sleep 5", "timeout_sec": 1})
	if err == nil || time.Since(start) > 4*time.Second {
		t.Fatalf("timeout not enforced: %v after %s", err, time.Since(start))
	}
}

func TestTodoWrite(t *testing.T) {
	env := newEnv(t)
	var got []Todo
	env.OnTodos(func(t []Todo) { got = t })
	out, err := run(t, env, TodoWrite{}, map[string]any{"todos": []map[string]string{
		{"content": "a", "status": "completed"}, {"content": "b", "status": "in_progress"},
	}})
	if err != nil || !strings.Contains(out, "1/2") || len(got) != 2 {
		t.Fatalf("out=%q err=%v got=%v", out, err, got)
	}
	if _, err := run(t, env, TodoWrite{}, map[string]any{"todos": []map[string]string{{"content": "a", "status": "done"}}}); err == nil {
		t.Fatal("expected status validation error")
	}
}

func TestTruncateKeepsHeadAndTail(t *testing.T) {
	s := strings.Repeat("a", 100) + strings.Repeat("z", 100)
	got := truncate(s, 60)
	if !strings.HasPrefix(got, "aaaa") || !strings.HasSuffix(got, "zzzz") || !strings.Contains(got, "omitted") {
		t.Fatalf("got %q", got)
	}
}

func TestUnifiedDiffShowsDeletionsFirst(t *testing.T) {
	d := UnifiedDiff("f", "a\nold\nc\n", "a\nnew\nc\n")
	if !strings.Contains(d, "-old\n+new\n") {
		t.Fatalf("diff = %q", d)
	}
}
