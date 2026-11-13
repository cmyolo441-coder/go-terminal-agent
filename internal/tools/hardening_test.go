package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// A command that prints without end is bounded by the output limit, not by
// memory, and its head and tail are kept.
func TestBashOutputIsBounded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix shell")
	}
	env := newEnv(t)
	env.OutputLimit = 1000
	out, _ := run(t, env, Bash{}, map[string]any{"command": "echo START; head -c 20000000 /dev/zero | tr '\\0' x; echo; echo END"})
	if len(out) > 3000 || !strings.Contains(out, "START") || !strings.Contains(out, "END") || !strings.Contains(out, "omitted") {
		t.Fatalf("unexpected output (%d bytes): %.300q", len(out), out)
	}
	h := newHeadTail(4)
	for _, s := range []string{"ab", "cdefg", "hij", "k"} {
		h.Write([]byte(s))
	}
	if got := h.String(); !strings.HasPrefix(got, "abcd") || !strings.HasSuffix(got, "hijk") || !strings.Contains(got, "3 bytes") {
		t.Fatalf("headTail = %q", got)
	}
}

// A background process that keeps the output pipe open must not hold the
// tool until its timeout.
func TestBashBackgroundProcessDoesNotHang(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix shell")
	}
	env := newEnv(t)
	start := time.Now()
	out, err := run(t, env, Bash{}, map[string]any{"command": "sleep 30 & echo started", "timeout_sec": 20})
	if err != nil || !strings.Contains(out, "started") || time.Since(start) > 10*time.Second {
		t.Fatalf("out=%q err=%v after %s", out, err, time.Since(start))
	}
}

func TestEditCRLFFile(t *testing.T) {
	env := newEnv(t)
	p := write(t, env, "w.txt", "one\r\ntwo\r\nthree")
	if out, err := run(t, env, ReadFile{}, map[string]any{"path": "w.txt"}); err != nil || strings.Contains(out, "\r") {
		t.Fatalf("read: %q %v", out, err)
	}
	if _, err := run(t, env, EditFile{}, map[string]any{"path": "w.txt", "old_string": "one\ntwo", "new_string": "1\n2\n2.5"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "1\r\n2\r\n2.5\r\nthree" {
		t.Fatalf("CRLF edit gave %q", b)
	}
	// Editing the last line of a file without a trailing newline keeps it so.
	if _, err := run(t, env, EditFile{}, map[string]any{"path": "w.txt", "old_string": "three", "new_string": "3"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "1\r\n2\r\n2.5\r\n3" {
		t.Fatalf("got %q", b)
	}
}

func TestDiffShowsTrailingNewlineChange(t *testing.T) {
	d := UnifiedDiff("f", "a\nb", "a\nb\n")
	if !strings.Contains(d, "-b\n"+noNewline+"\n+b\n") {
		t.Fatalf("diff = %q", d)
	}
	if d := UnifiedDiff("f", "", "x\n"); !strings.Contains(d, "@@ -0,0 +1,1 @@") {
		t.Fatalf("diff = %q", d)
	}
}

func TestReadFileLongLineKeepsUTF8(t *testing.T) {
	env := newEnv(t)
	write(t, env, "u.txt", "a"+strings.Repeat("é", maxLineLen)+"\n")
	out, err := run(t, env, ReadFile{}, map[string]any{"path": "u.txt"})
	if err != nil || !utf8.ValidString(out) || !strings.Contains(out, "line truncated") {
		t.Fatalf("out valid=%v err=%v", utf8.ValidString(out), err)
	}
}

// Writing through a symlink updates the target and keeps the link; a link
// that points outside the workspace makes the write an outside write.
func TestWriteThroughSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	env := newEnv(t)
	outside := filepath.Join(t.TempDir(), "rc")
	os.WriteFile(outside, []byte("old\n"), 0o600)
	link := filepath.Join(env.Root, "rc-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	env.MarkRead(link)
	raw, _ := json.Marshal(map[string]any{"path": "rc-link", "content": "new\n"})
	call, err := WriteFile{}.Prepare(env, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !call.OutsideRoot {
		t.Fatal("a write through a link to outside the workspace was not flagged")
	}
	if _, err := call.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Lstat(link); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced by a regular file")
	}
	if b, _ := os.ReadFile(outside); string(b) != "new\n" {
		t.Fatalf("target = %q", b)
	}
	inside := write(t, env, "real.txt", "x\n")
	os.Symlink(inside, filepath.Join(env.Root, "in-link"))
	if env.Outside(filepath.Join(env.Root, "in-link")) || env.Outside(filepath.Join(env.Root, "new.txt")) {
		t.Fatal("inside paths were flagged as outside")
	}
	if env.Rel(filepath.Join(env.Root, "..x")) != "..x" {
		t.Fatalf("Rel(..x) = %q", env.Rel(filepath.Join(env.Root, "..x")))
	}
}
