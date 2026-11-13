package audit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/secrets"
)

// fakeToken builds a GitHub-shaped token at run time.
func fakeToken(n int) string {
	const cs = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 36)
	for i := range b {
		b[i] = cs[(i*7+n*13)%len(cs)]
	}
	return "ghp_" + string(b)
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Millisecond)
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

func open(t testing.TB, dir string, mod func(*Options)) *FileLogger {
	t.Helper()
	opts := Options{Dir: dir, Session: "s-1", Workspace: "/work", User: "tester", Host: "box"}
	if mod != nil {
		mod(&opts)
	}
	l, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func mustVerify(t testing.TB, dir string, opts VerifyOptions) Report {
	t.Helper()
	rep, err := Verify(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Fatalf("chain broken: %s", rep.Broken)
	}
	return rep
}

func segFiles(t testing.TB, dir string) []string {
	t.Helper()
	segs, err := listSegments(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, s := range segs {
		out = append(out, filepath.Join(dir, s.name))
	}
	return out
}

func TestLogVerifyAndRedact(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "audit")
	l := open(t, dir, nil)
	tok := fakeToken(1)
	input := json.RawMessage(`{"command":"git push https://u:` + tok + `@github.com/o/r.git","password":"Hunter2-Secret!"}`)
	recs := []Record{
		{Event: EventSessionStart},
		ToolCall("bash", "tu_1", "exec", input, DecisionAllow, 1500*time.Millisecond, errors.New("exit 1: token "+tok)),
		Approval("write_file", "tu_2", DecisionOnce, "user approved"),
		ModelCall("anthropic", "claude-opus-5-5", llm.Usage{InputTokens: 1000, OutputTokens: 50, CacheReadTokens: 7}, 0.0123, 2*time.Second, nil),
		PolicyDecision("bash", DecisionDeny, "denied by policy", map[string]string{"rule": "commands.deny", "api_key": "abc123secret", "note": "uses " + tok}),
	}
	for _, r := range recs {
		if err := l.Log(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Log(Record{}); err == nil {
		t.Fatal("record without event should fail")
	}
	seq, head := l.Head()
	if seq != 5 || len(head) != 64 {
		t.Fatalf("head = %d %q", seq, head)
	}
	rep := mustVerify(t, dir, VerifyOptions{})
	if rep.Records != 5 || rep.FirstSeq != 1 || rep.LastSeq != 5 || rep.LastHash != head || rep.Files != 1 {
		t.Fatalf("report = %+v", rep)
	}
	files := segFiles(t, dir)
	raw, _ := os.ReadFile(files[0])
	if bytes.Contains(raw, []byte(tok)) || bytes.Contains(raw, []byte("Hunter2-Secret!")) || bytes.Contains(raw, []byte("abc123secret")) {
		t.Fatalf("secret reached the audit log:\n%s", raw)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(files[0])
		di, _ := os.Stat(dir)
		if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
			t.Fatalf("modes: file %v dir %v", fi.Mode(), di.Mode())
		}
	}
	var got []Record
	if err := Read(dir, func(r Record) error { got = append(got, r); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("read %d records", len(got))
	}
	tc := got[1]
	if tc.Event != EventToolCall || tc.Tool != "bash" || tc.DurationMS != 1500 || tc.Session != "s-1" || tc.User != "tester" ||
		tc.Host != "box" || tc.Workspace != "/work" || tc.PID != os.Getpid() || tc.Redactions != 3 || tc.Alg != AlgSHA256 {
		t.Fatalf("tool record = %+v", tc)
	}
	if !json.Valid([]byte(tc.Input)) || !strings.Contains(tc.Input, "[REDACTED:") {
		t.Fatalf("input = %s", tc.Input)
	}
	mc := got[3]
	if mc.InputTokens != 1000 || mc.OutputTokens != 50 || mc.CacheReadTokens != 7 || mc.CostUSD != 0.0123 || mc.Model != "claude-opus-5-5" {
		t.Fatalf("model record = %+v", mc)
	}
	if got[4].Details["rule"] != "commands.deny" || got[4].Redactions != 2 {
		t.Fatalf("details = %+v", got[4])
	}
	if got[0].PrevHash != Genesis || got[1].PrevHash != got[0].Hash || got[0].Time.Location() != time.UTC {
		t.Fatal("chain links or time zone wrong")
	}
}

func rewrite(t *testing.T, path string, f func(lines [][]byte) [][]byte) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.SplitAfter(raw, []byte("\n"))
	if len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	if err := os.WriteFile(path, bytes.Join(f(lines), nil), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTamperDetection(t *testing.T) {
	build := func(t *testing.T, key []byte) (string, string) {
		dir := t.TempDir()
		l := open(t, dir, func(o *Options) { o.HMACKey = key })
		for i := 0; i < 6; i++ {
			if err := l.Log(Record{Event: EventToolCall, Tool: "read_file", Input: fmt.Sprintf(`{"path":"f%d.go"}`, i)}); err != nil {
				t.Fatal(err)
			}
		}
		l.Close()
		return dir, segFiles(t, dir)[0]
	}
	cases := []struct {
		name   string
		edit   func([][]byte) [][]byte
		line   int
		reason string
	}{
		{"modify field", func(ls [][]byte) [][]byte {
			ls[2] = bytes.Replace(ls[2], []byte("f2.go"), []byte("f9.go"), 1)
			return ls
		}, 3, "hash mismatch"},
		{"delete record", func(ls [][]byte) [][]byte { return append(ls[:2], ls[3:]...) }, 3, "prev_hash"},
		{"reorder", func(ls [][]byte) [][]byte { ls[1], ls[2] = ls[2], ls[1]; return ls }, 2, "prev_hash"},
		{"duplicate", func(ls [][]byte) [][]byte { return append(ls[:3], append([][]byte{ls[2]}, ls[3:]...)...) }, 4, "prev_hash"},
		{"torn tail", func(ls [][]byte) [][]byte { ls[5] = ls[5][:len(ls[5])-10]; return ls }, 6, "incomplete"},
		{"garbage", func(ls [][]byte) [][]byte { ls[0] = []byte("hello\n"); return ls }, 1, "no trailing hash"},
		{"unknown alg", func(ls [][]byte) [][]byte {
			ls[0] = bytes.Replace(ls[0], []byte(`"alg":"sha256"`), []byte(`"alg":"md5"`), 1)
			return ls
		}, 1, "unknown hash algorithm"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir, file := build(t, nil)
			rewrite(t, file, c.edit)
			rep, err := Verify(dir, VerifyOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if rep.OK() || rep.Broken.Line != c.line || !strings.Contains(rep.Broken.Reason, c.reason) {
				t.Fatalf("broken = %+v, want line %d with %q", rep.Broken, c.line, c.reason)
			}
		})
	}

	t.Run("recomputed chain without key", func(t *testing.T) {
		key := []byte("org-audit-key")
		dir, file := build(t, key)
		mustVerify(t, dir, VerifyOptions{HMACKey: key, RequireHMAC: true})
		if rep, _ := Verify(dir, VerifyOptions{}); rep.OK() {
			t.Fatal("HMAC records must not verify without the key")
		}
		if rep, _ := Verify(dir, VerifyOptions{HMACKey: []byte("wrong")}); rep.OK() {
			t.Fatal("wrong key verified")
		}
		// An attacker edits a record and rebuilds the chain as plain sha256.
		var recs []Record
		Read(dir, func(r Record) error { recs = append(recs, r); return nil })
		recs[1].Input = `{"path":"innocent.go"}`
		var buf bytes.Buffer
		prev := Genesis
		for _, r := range recs {
			r.Alg, r.PrevHash, r.Hash = AlgSHA256, prev, ""
			line, sum, _ := seal(r, nil)
			buf.Write(line)
			prev = sum
		}
		os.WriteFile(file, buf.Bytes(), 0o600)
		if rep, _ := Verify(dir, VerifyOptions{}); !rep.OK() {
			t.Fatalf("forged unkeyed chain should be self-consistent: %v", rep.Broken)
		}
		rep, _ := Verify(dir, VerifyOptions{HMACKey: key, RequireHMAC: true})
		if rep.OK() || !strings.Contains(rep.Broken.Reason, "not HMAC") {
			t.Fatalf("downgrade not caught: %+v", rep.Broken)
		}
	})
}

func TestRotationAndDayRollover(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: time.Date(2026, 11, 13, 23, 59, 0, 0, time.UTC)}
	l := open(t, dir, func(o *Options) { o.MaxFileBytes = 2048; o.Now = c.now; o.Sync = SyncNever })
	for i := 0; i < 40; i++ {
		if i == 25 {
			c.set(time.Date(2026, 11, 14, 0, 0, 1, 0, time.UTC))
		}
		if err := l.Log(Record{Event: EventToolCall, Tool: "grep", Input: strings.Repeat("x", 200)}); err != nil {
			t.Fatal(err)
		}
	}
	files := segFiles(t, dir)
	var d13, d14 int
	for _, f := range files {
		fi, _ := os.Stat(f)
		if fi.Size() > 2048 {
			t.Fatalf("%s is %d bytes, over the rotation size", f, fi.Size())
		}
		if strings.Contains(f, "2026-11-13") {
			d13++
		} else if strings.Contains(f, "2026-11-14") {
			d14++
		}
	}
	if d13 < 2 || d14 < 2 || !strings.HasSuffix(files[1], "audit-2026-11-13.0001.jsonl") {
		t.Fatalf("segments = %v", files)
	}
	rep := mustVerify(t, dir, VerifyOptions{})
	if rep.Records != 40 || rep.Files != len(files) {
		t.Fatalf("report = %+v", rep)
	}

	// Archiving old segments: verify from a published anchor or pruned head.
	var anchorSeq uint64
	var anchor string
	readFile(files[0], func(r Record) error { anchorSeq, anchor = r.Seq, r.Hash; return nil })
	os.Remove(files[0])
	if rep, _ := Verify(dir, VerifyOptions{}); rep.OK() {
		t.Fatal("missing first segment should break the chain from genesis")
	}
	mustVerify(t, dir, VerifyOptions{Anchor: anchor, AnchorSeq: anchorSeq})
	mustVerify(t, dir, VerifyOptions{AllowPrunedHead: true})

	// Removing a middle segment is detected.
	os.Remove(files[2])
	if rep, _ := Verify(dir, VerifyOptions{AllowPrunedHead: true}); rep.OK() || !strings.Contains(rep.Broken.Reason, "prev_hash") {
		t.Fatalf("missing middle segment not detected: %+v", rep.Broken)
	}
}

func TestReopenContinuesChain(t *testing.T) {
	dir := t.TempDir()
	l := open(t, dir, nil)
	l.Log(Record{Event: EventSessionStart})
	l.Close()
	if err := l.Log(Record{Event: EventSessionEnd}); !errors.Is(err, ErrClosed) {
		t.Fatalf("log after close: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatal("second close should be a no-op")
	}
	l2 := open(t, dir, nil)
	l2.Log(Record{Event: EventSessionStart})
	if rep := mustVerify(t, dir, VerifyOptions{}); rep.Records != 2 {
		t.Fatalf("records = %d", rep.Records)
	}
}

func TestTornTailRecovery(t *testing.T) {
	dir := t.TempDir()
	l := open(t, dir, nil)
	l.Log(Record{Event: EventSessionStart})
	l.Log(Record{Event: EventToolCall, Tool: "bash"})
	l.Close()
	file := segFiles(t, dir)[0]
	f, _ := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0o600)
	partial := `{"v":1,"seq":3,"time":"2026-11-13T10:00:00Z","event":"tool_ca`
	f.WriteString(partial)
	f.Close()
	if rep, _ := Verify(dir, VerifyOptions{}); rep.OK() {
		t.Fatal("torn record should fail verification before repair")
	}
	l2 := open(t, dir, nil)
	l2.Log(Record{Event: EventSessionEnd})
	rep := mustVerify(t, dir, VerifyOptions{})
	if rep.Records != 4 {
		t.Fatalf("records = %d", rep.Records)
	}
	var rec Record
	Read(dir, func(r Record) error {
		if r.Event == EventRecovered {
			rec = r
		}
		return nil
	})
	if rec.Details["discarded_bytes"] != strconv.Itoa(len(partial)) {
		t.Fatalf("recovery record = %+v", rec)
	}
}

func TestCorruptHeadFailsClosed(t *testing.T) {
	dir := t.TempDir()
	l := open(t, dir, nil)
	l.Log(Record{Event: EventSessionStart})
	l.Close()
	os.WriteFile(segFiles(t, dir)[0], []byte("{\"not\":\"a record\"}\n"), 0o600)
	if _, err := Open(Options{Dir: dir}); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("Open on corrupt head: %v", err)
	}
}

func TestPermissionsTightened(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	dir := t.TempDir()
	os.Chmod(dir, 0o755)
	name := filepath.Join(dir, segmentName(time.Now().UTC().Format(time.DateOnly), 0))
	os.WriteFile(name, nil, 0o644)
	l := open(t, dir, nil)
	l.Log(Record{Event: EventSessionStart})
	fi, _ := os.Stat(name)
	di, _ := os.Stat(dir)
	if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
		t.Fatalf("modes: %v %v", fi.Mode(), di.Mode())
	}
}

func TestSyncModesAndTruncation(t *testing.T) {
	if _, err := ParseSyncMode("sometimes"); err == nil {
		t.Fatal("bad sync mode accepted")
	}
	if _, err := Open(Options{}); err == nil {
		t.Fatal("Open without dir should fail")
	}
	dir := t.TempDir()
	l := open(t, dir, func(o *Options) { o.Sync = SyncInterval; o.SyncEvery = 5 * time.Millisecond; o.MaxFieldBytes = 100 })
	long := strings.Repeat("é", 200) // 400 bytes of two-byte runes
	if err := l.Log(Record{Event: EventToolCall, Reason: long}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := l.Flush(); err != nil {
		t.Fatal(err)
	}
	var r Record
	Read(dir, func(x Record) error { r = x; return nil })
	if !utf8.ValidString(r.Reason) || !strings.Contains(r.Reason, "truncated 300 bytes") || len(r.Reason) > 140 {
		t.Fatalf("reason = %q", r.Reason)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	var n Nop
	if n.Log(Record{}) != nil || n.Close() != nil {
		t.Fatal("Nop")
	}
	var _ Logger = l
	var _ Logger = Nop{}
}

func TestConcurrentLoggersShareOneChain(t *testing.T) {
	dir := t.TempDir()
	const writers, each = 6, 40
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		// Separate loggers model separate processes: each has its own file
		// handles and cached head.
		l := open(t, dir, func(o *Options) { o.MaxFileBytes = 8 << 10; o.Sync = SyncNever })
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if err := l.Log(Record{Event: EventToolCall, Tool: fmt.Sprintf("w%d", w), Input: `{"i":` + strconv.Itoa(i) + `}`}); err != nil {
					t.Error(err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	if rep := mustVerify(t, dir, VerifyOptions{}); rep.Records != writers*each {
		t.Fatalf("records = %d", rep.Records)
	}
}

func TestHelperAuditProcess(t *testing.T) {
	if os.Getenv("AUDIT_HELPER") != "1" {
		t.Skip("helper process only")
	}
	l, err := Open(Options{Dir: os.Getenv("AUDIT_DIR"), MaxFileBytes: 16 << 10, Sync: SyncNever, Redactor: secrets.Default()})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for i := 0; i < 50; i++ {
		if err := l.Log(Record{Event: EventModelCall, Model: "m", Input: strconv.Itoa(i)}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMultiProcessChain(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	dir := t.TempDir()
	var cmds []*exec.Cmd
	for i := 0; i < 4; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperAuditProcess$")
		cmd.Env = append(os.Environ(), "AUDIT_HELPER=1", "AUDIT_DIR="+dir)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, cmd)
		t.Cleanup(func() {
			if t.Failed() {
				t.Log(out.String())
			}
		})
	}
	for _, c := range cmds {
		if err := c.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	if rep := mustVerify(t, dir, VerifyOptions{}); rep.Records != 200 || rep.Files < 2 {
		t.Fatalf("report = %+v", rep)
	}
}

func TestVerifyReaderAndLongLines(t *testing.T) {
	rep, err := VerifyReader("x", strings.NewReader(""), VerifyOptions{})
	if err != nil || !rep.OK() || rep.Records != 0 {
		t.Fatalf("empty: %+v %v", rep, err)
	}
	huge := strings.Repeat("a", maxLine+10)
	rep, err = VerifyReader("x", strings.NewReader(huge+"\n"), VerifyOptions{})
	if err != nil || rep.OK() || !strings.Contains(rep.Broken.Reason, "16 MiB") {
		t.Fatalf("huge: %+v %v", rep.Broken, err)
	}
	if s := (Break{File: "f", Line: 2, Seq: 3, Reason: "r"}).String(); s != "f:2 (seq 3): r" {
		t.Fatal(s)
	}
}

func BenchmarkLogSyncNever(b *testing.B) {
	l := open(b, b.TempDir(), func(o *Options) { o.Sync = SyncNever })
	in := json.RawMessage(`{"command":"go test ./...","timeout":120}`)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := l.Log(ToolCall("bash", "tu", "exec", in, DecisionAllow, time.Second, nil)); err != nil {
			b.Fatal(err)
		}
	}
}
