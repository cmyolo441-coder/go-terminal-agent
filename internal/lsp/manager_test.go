package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// logSink collects manager log lines.
type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logSink) contains(s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		if strings.Contains(line, s) {
			return true
		}
	}
	return false
}

func fakeConfig(t *testing.T, mode string) ServerConfig {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return ServerConfig{
		Name:        "fake",
		Command:     exe,
		Extensions:  []string{"fk"},
		LanguageIDs: map[string]string{".fk": "fakelang"},
		RootMarkers: []string{"fake.root"},
		Env: map[string]string{
			fakeEnv:       "1",
			fakeModeEnv:   mode,
			fakeMarkerEnv: filepath.Join(t.TempDir(), "crash-marker"),
			// A race-enabled binary otherwise sleeps a second on exit.
			"GORACE": "atexit_sleep_ms=0",
		},
		InitializationOptions: json.RawMessage(`{"opt":true}`),
		Settings:              json.RawMessage(`{"fake":{"a":1,"nested":{"key":"v"}}}`),
	}
}

// newFakeManager starts a manager whose only server is the fake.
func newFakeManager(t *testing.T, mode string, tweak func(*Options, *ServerConfig)) (*Manager, string, *logSink) {
	t.Helper()
	root := t.TempDir()
	cfg := fakeConfig(t, mode)
	logs := &logSink{}
	opts := Options{
		Root:              root,
		NoDefaults:        true,
		StartTimeout:      30 * time.Second,
		DiagnosticsWait:   10 * time.Second,
		DiagnosticsSettle: 30 * time.Millisecond,
		Logf:              logs.logf,
	}
	if tweak != nil {
		tweak(&opts, &cfg)
	}
	opts.Servers = append([]ServerConfig{cfg}, opts.Servers...)
	m, err := NewManager(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = m.Close(ctx)
	})
	return m, m.Root(), logs
}

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// stateOf asks the fake server for what it has seen.
func stateOf(t *testing.T, m *Manager, path string) fakeState {
	t.Helper()
	ctx := testCtx(t)
	c, _, err := m.client(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	var st fakeState
	if err := c.conn.Call(ctx, "fake/state", nil, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func hasMethod(st fakeState, method string) bool {
	for _, m := range st.Methods {
		if m == method {
			return true
		}
	}
	return false
}

// The ERROR on line 3 sits after an astral-plane emoji, so its UTF-16
// offset (14) differs from its rune column (13, 0-based).
const sampleSource = "package x\nfunc main() {\n\ts := \"😀é\" + ERROR\n\tWARN here\n}\n"

func TestDiagnosticsPushAndSync(t *testing.T) {
	m, root, _ := newFakeManager(t, "", nil)
	ctx := testCtx(t)
	path := writeFile(t, filepath.Join(root, "a.fk"), sampleSource)

	res, err := m.Diagnostics(ctx, path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Fresh || res.Version != 1 || res.Server != "fake" || res.Encoding != EncodingUTF16 {
		t.Fatalf("unexpected result %+v", res)
	}
	if len(res.Diagnostics) != 2 {
		t.Fatalf("want 2 diagnostics, got %+v", res.Diagnostics)
	}
	var errDiag *Diagnostic
	for i := range res.Diagnostics {
		if res.Diagnostics[i].EffectiveSeverity() == SeverityError {
			errDiag = &res.Diagnostics[i]
		}
	}
	if errDiag == nil || errDiag.Range.Start.Line != 2 || errDiag.Range.Start.Character != 14 || errDiag.CodeString() != "E1" {
		t.Fatalf("bad error diagnostic %+v", errDiag)
	}
	if line, col := displayPos(res.Lines, errDiag.Range.Start, res.Encoding); line != 3 || col != 14 {
		t.Fatalf("display position = %d:%d, want 3:14", line, col)
	}

	// Edit the file: the change is synced as version 2 and saved.
	writeFile(t, path, strings.Replace(sampleSource, "ERROR", "ok", 1))
	res, err = m.Diagnostics(ctx, path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Fresh || res.Version != 2 || len(res.Diagnostics) != 1 || res.Diagnostics[0].EffectiveSeverity() != SeverityWarning {
		t.Fatalf("after edit: %+v", res)
	}
	if res.Diagnostics[0].CodeString() != "42" {
		t.Fatalf("numeric code = %q", res.Diagnostics[0].CodeString())
	}

	// Unchanged content: no new version, still fresh.
	start := time.Now()
	res, err = m.Diagnostics(ctx, path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Fresh || res.Version != 2 || time.Since(start) > 3*time.Second {
		t.Fatalf("unchanged: %+v after %s", res, time.Since(start))
	}

	st := stateOf(t, m, path)
	uri := PathToURI(path)
	if st.Versions[uri] != 2 || st.LanguageIDs[uri] != "fakelang" || st.Saves != 1 {
		t.Fatalf("server state: %+v", st)
	}
	for _, method := range []string{"initialize", "initialized", "workspace/didChangeConfiguration", "textDocument/didOpen", "textDocument/didChange", "textDocument/didSave"} {
		if !hasMethod(st, method) {
			t.Errorf("server never saw %s: %v", method, st.Methods)
		}
	}
	if st.RootURI != PathToURI(root) {
		t.Errorf("rootUri = %s, want %s", st.RootURI, PathToURI(root))
	}
	if string(st.InitOptions) != `{"opt":true}` {
		t.Errorf("initializationOptions = %s", st.InitOptions)
	}
}

func TestServerRequestsAnswered(t *testing.T) {
	m, root, logs := newFakeManager(t, "", nil)
	path := writeFile(t, filepath.Join(root, "a.fk"), "hello\n")
	var st fakeState
	deadline := time.Now().Add(20 * time.Second)
	for {
		st = stateOf(t, m, path)
		if len(st.Replies) >= 5 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	want := map[string]string{
		"workspace/configuration":        `{"result":[{"a":1,"nested":{"key":"v"}},"v",null]}`,
		"window/workDoneProgress/create": `{"result":null}`,
		"client/registerCapability":      `{"result":null}`,
	}
	for method, w := range want {
		if got := compactJSON(t, st.Replies[method]); got != w {
			t.Errorf("%s answered %s, want %s", method, got, w)
		}
	}
	if !strings.Contains(string(st.Replies["workspace/applyEdit"]), `"applied":false`) {
		t.Errorf("applyEdit answered %s", st.Replies["workspace/applyEdit"])
	}
	if !strings.Contains(string(st.Replies["fake/unknownRequest"]), `-32601`) {
		t.Errorf("unknown request answered %s", st.Replies["fake/unknownRequest"])
	}
	if compactJSON(t, st.Settings) != `{"fake":{"a":1,"nested":{"key":"v"}}}` {
		t.Errorf("didChangeConfiguration settings = %s", st.Settings)
	}
	if !logs.contains("fake server ready") {
		t.Errorf("error-level log message not forwarded: %v", logs.lines)
	}
}

func compactJSON(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("bad json %s: %v", raw, err)
	}
	out, _ := json.Marshal(v)
	return string(out)
}

func TestUTF8Encoding(t *testing.T) {
	m, root, _ := newFakeManager(t, "utf8", nil)
	ctx := testCtx(t)
	path := writeFile(t, filepath.Join(root, "a.fk"), sampleSource)
	res, err := m.Diagnostics(ctx, path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Encoding != EncodingUTF8 {
		t.Fatalf("encoding = %s", res.Encoding)
	}
	out := FormatDiagnostics(res, 0)
	if !strings.Contains(out, "L3:14 error: found ERROR …") {
		t.Fatalf("utf-8 positions misrendered:\n%s", out)
	}
	st := m.Status()
	if len(st) != 1 || st[0].Encoding != EncodingUTF8 || st[0].State != "running" || st[0].OpenDocs != 1 || st[0].PID == 0 {
		t.Fatalf("status %+v", st)
	}
}

func TestAfterEdit(t *testing.T) {
	m, root, _ := newFakeManager(t, "", func(o *Options, _ *ServerConfig) { o.AfterEditLimit = 1 })
	ctx := testCtx(t)
	path := writeFile(t, filepath.Join(root, "dir", "a.fk"), sampleSource)
	out := m.AfterEdit(ctx, path)
	for _, want := range []string{
		`<diagnostics file="dir/a.fk" server="fake">`,
		"reports 1 error(s)",
		"L3:14 error: found ERROR … [fake E1]",
		`    s := "😀é" + ERROR`,
		"</diagnostics>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("AfterEdit output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "WARN") {
		t.Errorf("AfterEdit should report errors only:\n%s", out)
	}

	writeFile(t, path, "ERROR one\nERROR two\n")
	out = m.AfterEdit(ctx, path)
	if !strings.Contains(out, "reports 2 error(s)") || !strings.Contains(out, "… 1 more not shown") {
		t.Errorf("limit not applied:\n%s", out)
	}

	writeFile(t, path, "all good\nWARN only\n")
	if out := m.AfterEdit(ctx, path); out != "" {
		t.Errorf("clean file produced %q", out)
	}
	if out := m.AfterEdit(ctx, filepath.Join(root, "notes.txt")); out != "" {
		t.Errorf("unknown type produced %q", out)
	}
	var nilManager *Manager
	if out := nilManager.AfterEdit(ctx, path); out != "" {
		t.Errorf("nil manager produced %q", out)
	}
}

func TestSilentServerIsStale(t *testing.T) {
	m, root, _ := newFakeManager(t, "silent", func(o *Options, _ *ServerConfig) {
		o.DiagnosticsWait = 300 * time.Millisecond
	})
	ctx := testCtx(t)
	path := writeFile(t, filepath.Join(root, "a.fk"), "ERROR\n")
	// Warm up so timing below measures only the wait.
	if _, _, err := m.client(ctx, path); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	res, err := m.Diagnostics(ctx, path, 250*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if res.Fresh || len(res.Diagnostics) != 0 {
		t.Fatalf("silent server result %+v", res)
	}
	if el := time.Since(start); el < 250*time.Millisecond || el > 5*time.Second {
		t.Fatalf("waited %s", el)
	}
	if out := m.AfterEdit(ctx, path); out != "" {
		t.Fatalf("stale diagnostics must not be reported: %q", out)
	}
}

func TestUnversionedPublishes(t *testing.T) {
	m, root, _ := newFakeManager(t, "noversion", nil)
	ctx := testCtx(t)
	path := writeFile(t, filepath.Join(root, "a.fk"), "ERROR\n")
	res, err := m.Diagnostics(ctx, path, 0)
	if err != nil || !res.Fresh || len(res.Diagnostics) != 1 {
		t.Fatalf("first: %+v %v", res, err)
	}
	writeFile(t, path, "fine\n")
	res, err = m.Diagnostics(ctx, path, 0)
	if err != nil || !res.Fresh || len(res.Diagnostics) != 0 {
		t.Fatalf("after fix: %+v %v", res, err)
	}
}

func TestOutdatedPublishIgnored(t *testing.T) {
	m, root, _ := newFakeManager(t, "stale-first", func(o *Options, _ *ServerConfig) { o.DiagnosticsSettle = -1 })
	ctx := testCtx(t)
	path := writeFile(t, filepath.Join(root, "a.fk"), "ERROR\n")
	for i := 0; i < 3; i++ {
		res, err := m.Diagnostics(ctx, path, 0)
		if err != nil || !res.Fresh || len(res.Diagnostics) != 1 {
			t.Fatalf("round %d: took an outdated publish: %+v %v", i, res, err)
		}
		writeFile(t, path, fmt.Sprintf("ERROR %d\n", i))
	}
}

func TestSettleCollectsFollowUpPublish(t *testing.T) {
	t.Run("settle", func(t *testing.T) {
		m, root, _ := newFakeManager(t, "phased", func(o *Options, _ *ServerConfig) { o.DiagnosticsSettle = 400 * time.Millisecond })
		path := writeFile(t, filepath.Join(root, "a.fk"), "ERROR\nWARN\n")
		res, err := m.Diagnostics(testCtx(t), path, 0)
		if err != nil || len(res.Diagnostics) != 2 {
			t.Fatalf("settled result: %+v %v", res, err)
		}
	})
	t.Run("no settle", func(t *testing.T) {
		m, root, _ := newFakeManager(t, "phased", func(o *Options, _ *ServerConfig) { o.DiagnosticsSettle = -1 })
		path := writeFile(t, filepath.Join(root, "a.fk"), "ERROR\nWARN\n")
		res, err := m.Diagnostics(testCtx(t), path, 0)
		if err != nil || len(res.Diagnostics) != 1 || res.Diagnostics[0].EffectiveSeverity() != SeverityWarning {
			t.Fatalf("first publish only: %+v %v", res, err)
		}
	})
}

func TestPullDiagnostics(t *testing.T) {
	m, root, _ := newFakeManager(t, "pull", nil)
	ctx := testCtx(t)
	path := writeFile(t, filepath.Join(root, "a.fk"), sampleSource)
	res, err := m.Diagnostics(ctx, path, 0)
	if err != nil || !res.Fresh || len(res.Diagnostics) != 2 {
		t.Fatalf("pull: %+v %v", res, err)
	}
	if !hasMethod(stateOf(t, m, path), "textDocument/diagnostic") {
		t.Fatal("pull request never sent")
	}
}

func TestPullFallsBackToPush(t *testing.T) {
	m, root, _ := newFakeManager(t, "pull-broken", nil)
	path := writeFile(t, filepath.Join(root, "a.fk"), sampleSource)
	res, err := m.Diagnostics(testCtx(t), path, 0)
	if err != nil || !res.Fresh || len(res.Diagnostics) != 2 {
		t.Fatalf("fallback: %+v %v", res, err)
	}
}

const navA = "func helper() {}\nfunc main() {\n\t😀 := helper()\n}\n"
const navB = "func other() {\n\thelper()\n}\n"

func TestNavigation(t *testing.T) {
	for _, mode := range []string{"", "links", "utf8"} {
		t.Run("mode="+mode, func(t *testing.T) {
			m, root, _ := newFakeManager(t, mode, nil)
			ctx := testCtx(t)
			a := writeFile(t, filepath.Join(root, "a.fk"), navA)
			writeFile(t, filepath.Join(root, "pkg", "b.fk"), navB)

			// Column 7 is the "h" of helper after the emoji (rune column).
			locs, err := m.Definition(ctx, a, 3, 7)
			if err != nil {
				t.Fatal(err)
			}
			if len(locs) != 1 || locs[0].URI != PathToURI(a) || locs[0].Range.Start.Line != 0 || locs[0].Range.Start.Character != 5 {
				t.Fatalf("definition = %+v", locs)
			}
			out := FormatLocations(m.Root(), locs, m.encodingFor, 0)
			if out != "a.fk:1:6  func helper() {}\n" {
				t.Fatalf("formatted definition %q", out)
			}

			refs, err := m.References(ctx, a, 3, 8, true)
			if err != nil {
				t.Fatal(err)
			}
			if len(refs) != 3 {
				t.Fatalf("references with declaration = %+v", refs)
			}
			refs, err = m.References(ctx, a, 3, 8, false)
			if err != nil || len(refs) != 2 {
				t.Fatalf("references without declaration = %+v %v", refs, err)
			}
			out = FormatLocations(m.Root(), refs, m.encodingFor, 1)
			if !strings.Contains(out, "a.fk:3:7  😀 := helper()") || !strings.Contains(out, "… 1 more not shown") {
				t.Fatalf("formatted references:\n%s", out)
			}

			hover, err := m.Hover(ctx, a, 1, 6)
			if err != nil || !strings.Contains(hover, "**helper**") {
				t.Fatalf("hover = %q %v", hover, err)
			}
			if _, err := m.Hover(ctx, a, 99, 1); err == nil || !strings.Contains(err.Error(), "line 99") {
				t.Fatalf("out of range line: %v", err)
			}
		})
	}
}

func TestCrashIsRestarted(t *testing.T) {
	m, root, logs := newFakeManager(t, "crash-once", nil)
	ctx := testCtx(t)
	path := writeFile(t, filepath.Join(root, "a.fk"), "ERROR\n")
	res, err := m.Diagnostics(ctx, path, 0)
	if err != nil || !res.Fresh || len(res.Diagnostics) != 1 {
		t.Fatalf("after crash: %+v %v", res, err)
	}
	st := m.Status()
	if len(st) != 1 || st[0].Starts != 2 || st[0].Crashes != 1 || st[0].State != "running" {
		t.Fatalf("status %+v", st)
	}
	if !logs.contains("exited unexpectedly; restarting") {
		t.Errorf("crash not logged: %v", logs.lines)
	}
}

func TestCrashMidRequestRetries(t *testing.T) {
	m, root, _ := newFakeManager(t, "crash-hover-once", nil)
	path := writeFile(t, filepath.Join(root, "a.fk"), navA)
	hover, err := m.Hover(testCtx(t), path, 1, 6)
	if err != nil || !strings.Contains(hover, "helper") {
		t.Fatalf("hover after crash = %q %v", hover, err)
	}
}

func TestCrashLoopGivesUp(t *testing.T) {
	m, root, _ := newFakeManager(t, "crash-always", func(o *Options, _ *ServerConfig) { o.MaxRestarts = 2 })
	ctx := testCtx(t)
	path := writeFile(t, filepath.Join(root, "a.fk"), "x\n")
	var err error
	for i := 0; i < 6; i++ {
		_, err = m.Diagnostics(ctx, path, 0)
		if err != nil && strings.Contains(err.Error(), "not restarting") {
			break
		}
	}
	if err == nil || !strings.Contains(err.Error(), "not restarting") {
		t.Fatalf("crash loop error = %v", err)
	}
	st := m.Status()
	if len(st) != 1 || st[0].State != "failed" || st[0].Starts > 4 {
		t.Fatalf("status %+v", st)
	}
	// A failed server answers at once.
	start := time.Now()
	if _, err := m.Diagnostics(ctx, path, 0); err == nil || time.Since(start) > time.Second {
		t.Fatalf("failed slot: %v after %s", err, time.Since(start))
	}
	if out := m.AfterEdit(ctx, path); out != "" {
		t.Fatalf("AfterEdit on a failed server = %q", out)
	}
}

func TestStartFailureReportsStderr(t *testing.T) {
	m, root, _ := newFakeManager(t, "init-fail", func(o *Options, _ *ServerConfig) { o.MaxRestarts = 1 })
	ctx := testCtx(t)
	path := writeFile(t, filepath.Join(root, "a.fk"), "x\n")
	_, err := m.Diagnostics(ctx, path, 0)
	if err == nil || !strings.Contains(err.Error(), "missing toolchain") || !strings.Contains(err.Error(), "initializing fake") {
		t.Fatalf("first failure = %v", err)
	}
	_, err = m.Diagnostics(ctx, path, 0)
	if err == nil || !strings.Contains(err.Error(), "not restarting") || !strings.Contains(err.Error(), "missing toolchain") {
		t.Fatalf("second failure = %v", err)
	}
}

func TestSlowStartSurvivesShortCaller(t *testing.T) {
	m, root, _ := newFakeManager(t, "", func(_ *Options, c *ServerConfig) { c.Env[fakeDelayEnv] = "700" })
	path := writeFile(t, filepath.Join(root, "a.fk"), "ERROR\n")
	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := m.Diagnostics(short, path, 0); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("short caller: %v", err)
	}
	if st := m.Status(); len(st) != 1 || st[0].State != "starting" {
		t.Fatalf("status during start %+v", st)
	}
	res, err := m.Diagnostics(testCtx(t), path, 0)
	if err != nil || len(res.Diagnostics) != 1 {
		t.Fatalf("after start: %+v %v", res, err)
	}
	if st := m.Status(); st[0].Starts != 1 {
		t.Fatalf("start was repeated: %+v", st)
	}
}

func TestRootMarkersSplitServers(t *testing.T) {
	m, root, _ := newFakeManager(t, "", nil)
	ctx := testCtx(t)
	writeFile(t, filepath.Join(root, "sub", "fake.root"), "")
	inner := writeFile(t, filepath.Join(root, "sub", "deep", "x.fk"), "ERROR\n")
	outer := writeFile(t, filepath.Join(root, "y.fk"), "WARN\n")
	for _, p := range []string{inner, outer} {
		if _, err := m.Diagnostics(ctx, p, 0); err != nil {
			t.Fatal(err)
		}
	}
	st := m.Status()
	if len(st) != 2 || st[0].Root != root || st[1].Root != filepath.Join(root, "sub") {
		t.Fatalf("status %+v", st)
	}
	if got := stateOf(t, m, inner).RootURI; got != PathToURI(filepath.Join(root, "sub")) {
		t.Fatalf("inner server root %s", got)
	}
}

func TestPathRouting(t *testing.T) {
	m, root, _ := newFakeManager(t, "", func(o *Options, _ *ServerConfig) {
		o.Servers = []ServerConfig{{Name: "ghost", Command: "gta-no-such-language-server", Extensions: []string{".ghost"}}}
	})
	ctx := testCtx(t)
	if _, err := m.Diagnostics(ctx, filepath.Join(root, "a.txt"), 0); !errors.Is(err, ErrNoServer) {
		t.Fatalf("unknown extension: %v", err)
	}
	if _, err := m.Diagnostics(ctx, filepath.Join(root, "noext"), 0); !errors.Is(err, ErrNoServer) {
		t.Fatalf("no extension: %v", err)
	}
	ghost := writeFile(t, filepath.Join(root, "a.ghost"), "x")
	if _, err := m.Diagnostics(ctx, ghost, 0); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing binary: %v", err)
	}
	if m.Available(ghost) || !m.Available("x.FK") {
		t.Fatal("Available wrong")
	}
	if out := m.AfterEdit(ctx, ghost); out != "" {
		t.Fatalf("AfterEdit for a missing server = %q", out)
	}
	outside := filepath.Join(filepath.Dir(root), "elsewhere.fk")
	if _, err := m.Diagnostics(ctx, outside, 0); err == nil || !strings.Contains(err.Error(), "outside the workspace") {
		t.Fatalf("outside path: %v", err)
	}
	if _, err := m.Diagnostics(ctx, "../escape.fk", 0); err == nil || !strings.Contains(err.Error(), "outside the workspace") {
		t.Fatalf("relative escape: %v", err)
	}
	writeFile(t, filepath.Join(root, "rel.fk"), "ERROR\n")
	if res, err := m.Diagnostics(ctx, "rel.fk", 0); err != nil || len(res.Diagnostics) != 1 {
		t.Fatalf("relative path: %+v %v", res, err)
	}
	if _, err := m.Diagnostics(ctx, filepath.Join(root, "missing.fk"), 0); err == nil {
		t.Fatal("missing file should fail")
	}
}

func TestDeletedFileIsClosed(t *testing.T) {
	m, root, _ := newFakeManager(t, "", nil)
	ctx := testCtx(t)
	path := writeFile(t, filepath.Join(root, "a.fk"), "ERROR\n")
	if _, err := m.Diagnostics(ctx, path, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Diagnostics(ctx, path, 0); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted file: %v", err)
	}
	st := stateOf(t, m, path)
	if _, open := st.Versions[PathToURI(path)]; open || !hasMethod(st, "textDocument/didClose") {
		t.Fatalf("document not closed: %+v", st)
	}
	if s := m.Status(); s[0].OpenDocs != 0 {
		t.Fatalf("open docs %d", s[0].OpenDocs)
	}
	// Re-creating it reopens it from version 1.
	writeFile(t, path, "WARN\n")
	res, err := m.Diagnostics(ctx, path, 0)
	if err != nil || res.Version != 1 || len(res.Diagnostics) != 1 {
		t.Fatalf("reopen: %+v %v", res, err)
	}
}

func TestCancelSendsCancelRequest(t *testing.T) {
	m, root, _ := newFakeManager(t, "hang-hover", nil)
	path := writeFile(t, filepath.Join(root, "a.fk"), navA)
	if _, _, err := m.client(testCtx(t), path); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := m.Hover(ctx, path, 1, 6); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hover = %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		st := stateOf(t, m, path)
		if len(st.Cancelled) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never got $/cancelRequest: %+v", st)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The server is still usable.
	if st := m.Status(); st[0].State != "running" || st[0].Starts != 1 {
		t.Fatalf("status %+v", st)
	}
}

func TestConcurrentUse(t *testing.T) {
	m, root, _ := newFakeManager(t, "", nil)
	ctx := testCtx(t)
	var paths []string
	for i := 0; i < 8; i++ {
		paths = append(paths, writeFile(t, filepath.Join(root, fmt.Sprintf("f%d.fk", i)), fmt.Sprintf("func f%d() {}\nERROR %d\n", i, i)))
	}
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := paths[i%len(paths)]
			if i%2 == 0 {
				res, err := m.Diagnostics(ctx, p, 0)
				if err == nil && (!res.Fresh || len(res.Diagnostics) != 1) {
					err = fmt.Errorf("%s: %+v", p, res)
				}
				if err != nil {
					errs <- err
				}
				return
			}
			if _, err := m.Hover(ctx, p, 1, 6); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if st := m.Status(); len(st) != 1 || st[0].Starts != 1 || st[0].OpenDocs != len(paths) {
		t.Fatalf("status %+v", st)
	}
}

func TestCloseShutsDown(t *testing.T) {
	t.Run("graceful", func(t *testing.T) {
		m, root, _ := newFakeManager(t, "", nil)
		path := writeFile(t, filepath.Join(root, "a.fk"), "x\n")
		c, _, err := m.client(testCtx(t), path)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.Close(testCtx(t)); err != nil {
			t.Fatal(err)
		}
		select {
		case <-c.Done():
		case <-time.After(10 * time.Second):
			t.Fatal("server still running after Close")
		}
		if _, err := m.Diagnostics(testCtx(t), path, 0); !errors.Is(err, ErrManagerClosed) {
			t.Fatalf("after close: %v", err)
		}
		if err := m.Close(testCtx(t)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("unresponsive", func(t *testing.T) {
		m, root, _ := newFakeManager(t, "ignore-shutdown", nil)
		path := writeFile(t, filepath.Join(root, "a.fk"), "x\n")
		c, _, err := m.client(testCtx(t), path)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		start := time.Now()
		_ = m.Close(ctx)
		select {
		case <-c.Done():
		case <-time.After(10 * time.Second):
			t.Fatal("unresponsive server not killed")
		}
		if el := time.Since(start); el > 8*time.Second {
			t.Fatalf("Close took %s", el)
		}
	})
	t.Run("during start", func(t *testing.T) {
		m, root, _ := newFakeManager(t, "", func(_ *Options, c *ServerConfig) { c.Env[fakeDelayEnv] = "400" })
		path := writeFile(t, filepath.Join(root, "a.fk"), "x\n")
		m.Warm(path)
		deadline := time.Now().Add(5 * time.Second)
		for len(m.Status()) == 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if err := m.Close(testCtx(t)); err != nil {
			t.Fatal(err)
		}
		for _, st := range m.Status() {
			if st.State == "running" || st.State == "starting" {
				t.Fatalf("server left behind: %+v", st)
			}
		}
	})
}

func TestWarmStartsInBackground(t *testing.T) {
	m, root, _ := newFakeManager(t, "", nil)
	path := writeFile(t, filepath.Join(root, "a.fk"), "x\n")
	m.Warm(path)
	m.Warm(filepath.Join(root, "ignored.txt"))
	deadline := time.Now().Add(20 * time.Second)
	for {
		st := m.Status()
		if len(st) == 1 && st[0].State == "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("warm did not start the server: %+v", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestNewManagerValidation(t *testing.T) {
	if _, err := NewManager(Options{}); err == nil {
		t.Fatal("empty root accepted")
	}
	root := t.TempDir()
	bad := [][]ServerConfig{
		{{Command: "x", Extensions: []string{".x"}}},
		{{Name: "a", Command: "x", Extensions: []string{".x"}}, {Name: "a", Command: "y", Extensions: []string{".y"}}},
		{{Name: "a", Extensions: []string{".x"}}},
		{{Name: "a", Command: "x"}},
	}
	for i, servers := range bad {
		if _, err := NewManager(Options{Root: root, Servers: servers}); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}

	m, err := NewManager(Options{Root: root, Servers: []ServerConfig{
		{Name: "gopls", Disabled: true},
		{Name: "pyright", Command: "basedpyright-langserver", Args: []string{"--stdio"}, Extensions: []string{"py"}},
		{Name: "zls", Command: "zls", Extensions: []string{"zig"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.ServerFor("main.go"); ok {
		t.Error("disabled default still routes .go")
	}
	if s, ok := m.ServerFor("x.PY"); !ok || s.Command != "basedpyright-langserver" {
		t.Errorf("override not used: %+v", s)
	}
	if s, ok := m.ServerFor("a.zig"); !ok || s.Name != "zls" {
		t.Errorf("added server not used: %+v", s)
	}
	if s, ok := m.ServerFor("a.tsx"); !ok || s.Name != "typescript-language-server" {
		t.Errorf("default kept: %+v", s)
	}
	m2, err := NewManager(Options{Root: root, NoDefaults: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m2.ServerFor("a.go"); ok {
		t.Error("NoDefaults still has defaults")
	}

	names := map[string]bool{}
	for _, s := range DefaultServers() {
		names[s.Command] = true
	}
	for _, want := range []string{"gopls", "typescript-language-server", "pyright-langserver", "rust-analyzer", "clangd"} {
		if !names[want] {
			t.Errorf("default %s missing", want)
		}
	}
}
