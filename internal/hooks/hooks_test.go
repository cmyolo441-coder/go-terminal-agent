//go:build !windows

package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newRunner(t *testing.T, cfg Config) (*Runner, string) {
	t.Helper()
	root := t.TempDir()
	r, err := NewRunner(cfg, Options{Root: root, SessionID: "sess-1"})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return r, root
}

func pre(t *testing.T, r *Runner, tool string) Result {
	t.Helper()
	res, err := r.PreToolUse(context.Background(), tool, json.RawMessage(`{"command":"ls"}`))
	if err != nil {
		t.Fatalf("PreToolUse: %v", err)
	}
	return res
}

func TestNilAndEmptyRunnerAreNoOps(t *testing.T) {
	var r *Runner
	ctx := context.Background()
	if res, err := r.PreToolUse(ctx, "bash", nil); err != nil || res.Blocked {
		t.Fatalf("nil runner: %+v %v", res, err)
	}
	if res, err := r.UserPromptSubmit(ctx, "hi"); err != nil || res.Blocked {
		t.Fatalf("nil runner: %+v %v", res, err)
	}
	r.SetSessionID("x")
	if r.Has(Stop) {
		t.Fatal("nil runner has hooks")
	}
	empty, _ := newRunner(t, nil)
	for _, f := range []func() (Result, error){
		func() (Result, error) { return empty.PreToolUse(ctx, "bash", nil) },
		func() (Result, error) { return empty.PostToolUse(ctx, "bash", nil, "out", false) },
		func() (Result, error) { return empty.UserPromptSubmit(ctx, "p") },
		func() (Result, error) { return empty.Stop(ctx) },
		func() (Result, error) { return empty.SessionStart(ctx) },
	} {
		res, err := f()
		if err != nil || res.Blocked || len(res.Feedback)+len(res.Warnings)+len(res.Context) != 0 {
			t.Fatalf("empty runner: %+v %v", res, err)
		}
	}
}

func TestAllowCollectsFeedback(t *testing.T) {
	r, _ := newRunner(t, Config{PreToolUse: {{Command: "echo looks fine"}}})
	res := pre(t, r, "bash")
	if res.Blocked || len(res.Warnings) != 0 {
		t.Fatalf("unexpected: %+v", res)
	}
	if len(res.Feedback) != 1 || res.Feedback[0] != "looks fine" {
		t.Fatalf("feedback = %q", res.Feedback)
	}
}

func TestBlockViaExit2(t *testing.T) {
	r, _ := newRunner(t, Config{PreToolUse: {{Command: "echo 'rm is not allowed' >&2; exit 2"}}})
	res := pre(t, r, "bash")
	if !res.Blocked || res.Reason != "rm is not allowed" {
		t.Fatalf("got %+v", res)
	}
}

func TestBlockViaExit2WithoutStderrHasReason(t *testing.T) {
	r, _ := newRunner(t, Config{PreToolUse: {{Command: "exit 2"}}})
	res := pre(t, r, "bash")
	if !res.Blocked || !strings.Contains(res.Reason, "blocked by pre_tool_use hook") {
		t.Fatalf("got %+v", res)
	}
}

func TestJSONDecisionOverridesExitCode(t *testing.T) {
	r, _ := newRunner(t, Config{PreToolUse: {{Command: `echo '{"decision":"block","reason":"policy says no"}'`}}})
	res := pre(t, r, "bash")
	if !res.Blocked || res.Reason != "policy says no" || len(res.Feedback) != 0 {
		t.Fatalf("block decision: %+v", res)
	}

	r, _ = newRunner(t, Config{PreToolUse: {{Command: `echo '{"decision":"allow","reason":"auto-approved"}'; echo ignored >&2; exit 2`}}})
	res = pre(t, r, "bash")
	if res.Blocked || !res.Approved || len(res.Warnings) != 0 {
		t.Fatalf("allow decision: %+v", res)
	}
	if len(res.Feedback) != 1 || res.Feedback[0] != "auto-approved" {
		t.Fatalf("allow feedback: %q", res.Feedback)
	}
}

func TestNonDecisionJSONIsPlainFeedback(t *testing.T) {
	r, _ := newRunner(t, Config{PreToolUse: {{Command: `echo '{"lint":"ok"}'`}}})
	res := pre(t, r, "bash")
	if res.Blocked || res.Approved || len(res.Feedback) != 1 || res.Feedback[0] != `{"lint":"ok"}` {
		t.Fatalf("got %+v", res)
	}
}

func TestOtherExitCodeIsWarning(t *testing.T) {
	r, _ := newRunner(t, Config{PreToolUse: {
		{Command: "echo linter crashed >&2; exit 1"},
		{Command: "echo second ran"},
	}})
	res := pre(t, r, "bash")
	if res.Blocked {
		t.Fatalf("exit 1 must not block: %+v", res)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "exited with code 1") || !strings.Contains(res.Warnings[0], "linter crashed") {
		t.Fatalf("warnings = %q", res.Warnings)
	}
	if len(res.Feedback) != 1 || res.Feedback[0] != "second ran" {
		t.Fatalf("chain should continue after a warning: %q", res.Feedback)
	}
}

func TestMatcherFiltersByToolName(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "ran")
	cfg := Config{PreToolUse: {
		{Matcher: "write_file|edit", Command: "echo \"$GTA_TOOL_NAME\" >> " + marker},
		{Matcher: "*", Command: "echo any:$GTA_TOOL_NAME >> " + marker},
	}}
	r, err := NewRunner(cfg, Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"bash", "edit", "write_file", "edit_extra"} {
		pre(t, r, tool)
	}
	data, _ := os.ReadFile(marker)
	want := "any:bash\nedit\nany:edit\nwrite_file\nany:write_file\nany:edit_extra\n"
	if string(data) != want {
		t.Fatalf("hooks ran:\n%s\nwant:\n%s", data, want)
	}
}

func TestPayloadOnStdin(t *testing.T) {
	root := t.TempDir()
	cfg := Config{
		PreToolUse:       {{Command: "cat > pre.json"}},
		PostToolUse:      {{Command: "cat > post.json"}},
		UserPromptSubmit: {{Command: "cat > prompt.json"}},
	}
	r, err := NewRunner(cfg, Options{Root: root, SessionID: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	input := json.RawMessage(`{"path":"a.go","n":3}`)
	if _, err := r.PreToolUse(ctx, "read_file", input); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PostToolUse(ctx, "read_file", input, "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := r.UserPromptSubmit(ctx, "fix the bug"); err != nil {
		t.Fatal(err)
	}

	read := func(name string) map[string]any {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("%s: %v: %s", name, err, data)
		}
		return m
	}
	p := read("pre.json")
	if p["event"] != "pre_tool_use" || p["session_id"] != "abc" || p["cwd"] != root || p["tool_name"] != "read_file" {
		t.Fatalf("pre payload: %v", p)
	}
	if in, _ := p["tool_input"].(map[string]any); in["path"] != "a.go" || in["n"] != float64(3) {
		t.Fatalf("tool_input: %v", p["tool_input"])
	}
	for _, k := range []string{"tool_output", "is_error", "prompt"} {
		if _, ok := p[k]; ok {
			t.Fatalf("pre payload should omit %s: %v", k, p)
		}
	}
	post := read("post.json")
	if post["event"] != "post_tool_use" || post["tool_output"] != "" || post["is_error"] != false {
		t.Fatalf("post payload: %v", post)
	}
	pr := read("prompt.json")
	if pr["event"] != "user_prompt_submit" || pr["prompt"] != "fix the bug" {
		t.Fatalf("prompt payload: %v", pr)
	}
	if _, ok := pr["tool_name"]; ok {
		t.Fatalf("prompt payload should omit tool_name: %v", pr)
	}
}

func TestMalformedToolInputStaysValidJSON(t *testing.T) {
	r, root := newRunner(t, Config{PreToolUse: {{Command: "cat > p.json"}}})
	if _, err := r.PreToolUse(context.Background(), "bash", json.RawMessage(`{not json`)); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(root, "p.json"))
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil || m["tool_input"] != "{not json" {
		t.Fatalf("payload %s: %v", data, err)
	}
}

func TestEnvironmentVariables(t *testing.T) {
	r, root := newRunner(t, Config{PostToolUse: {{Command: `printf '%s|%s|%s|%s' "$GTA_EVENT" "$GTA_TOOL_NAME" "$GTA_SESSION_ID" "$GTA_PROJECT_DIR"`}}})
	r.SetSessionID("sess-2")
	res, err := r.PostToolUse(context.Background(), "grep", nil, "matches", true)
	if err != nil {
		t.Fatal(err)
	}
	want := "post_tool_use|grep|sess-2|" + root
	if len(res.Feedback) != 1 || res.Feedback[0] != want {
		t.Fatalf("feedback = %q, want %q", res.Feedback, want)
	}
}

func TestWorkingDirectoryIsRoot(t *testing.T) {
	r, root := newRunner(t, Config{Stop: {{Command: "pwd -P"}}})
	res, err := r.Stop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(root)
	if len(res.Feedback) != 1 || res.Feedback[0] != want {
		t.Fatalf("pwd = %q, want %q", res.Feedback, want)
	}
}

func TestTimeoutKillsProcessGroup(t *testing.T) {
	r, root := newRunner(t, Config{PreToolUse: {
		{Command: "sleep 30 & echo $! > child.pid; sleep 30", TimeoutSec: 1},
		{Command: "echo after"},
	}})
	start := time.Now()
	res := pre(t, r, "bash")
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
	if res.Blocked {
		t.Fatalf("a timeout must not block: %+v", res)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "timed out after 1s") {
		t.Fatalf("warnings = %q", res.Warnings)
	}
	if len(res.Feedback) != 1 || res.Feedback[0] != "after" {
		t.Fatalf("feedback = %q", res.Feedback)
	}
	data, err := os.ReadFile(filepath.Join(root, "child.pid"))
	if err != nil {
		t.Fatalf("hook did not record its child: %v", err)
	}
	pid := strings.TrimSpace(string(data))
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		return // no procfs to inspect
	}
	deadline := time.Now().Add(3 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("background child %s survived the timeout", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// processAlive reports whether pid is running and not a zombie, using procfs.
func processAlive(pid string) bool {
	data, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil {
		return false
	}
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 >= len(s) {
		return false
	}
	return s[i+2] != 'Z' && s[i+2] != 'X'
}

func TestBackgroundChildDoesNotHoldRunner(t *testing.T) {
	r, _ := newRunner(t, Config{Stop: {{Command: "sleep 20 & echo done"}}})
	start := time.Now()
	res, err := r.Stop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("runner waited %s for a backgrounded child", d)
	}
	if len(res.Feedback) != 1 || res.Feedback[0] != "done" {
		t.Fatalf("feedback = %q", res.Feedback)
	}
}

func TestContextCancelReturnsError(t *testing.T) {
	r, _ := newRunner(t, Config{Stop: {{Command: "sleep 30"}, {Command: "echo never"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := r.Stop(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("cancel took %s", d)
	}
}

func TestUserPromptSubmitContextAndReject(t *testing.T) {
	r, _ := newRunner(t, Config{UserPromptSubmit: {
		{Command: "echo 'branch: main'"},
		{Command: `read -r line; case "$line" in *secret*) echo 'prompt mentions a secret' >&2; exit 2;; esac; echo 'extra'`},
	}})
	ctx := context.Background()
	res, err := r.UserPromptSubmit(ctx, "refactor the parser")
	if err != nil {
		t.Fatal(err)
	}
	if res.Blocked || len(res.Feedback) != 0 {
		t.Fatalf("got %+v", res)
	}
	if got := res.ContextText(); got != "branch: main\n\nextra" {
		t.Fatalf("context = %q", got)
	}
	res, err = r.UserPromptSubmit(ctx, "print the secret token")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Blocked || res.Reason != "prompt mentions a secret" {
		t.Fatalf("got %+v", res)
	}
}

func TestSessionStartCannotBlock(t *testing.T) {
	r, _ := newRunner(t, Config{SessionStart: {
		{Command: "echo nope >&2; exit 2"},
		{Command: "echo 'project notes'"},
	}})
	res, err := r.SessionStart(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Blocked || len(res.Warnings) != 1 || res.ContextText() != "project notes" {
		t.Fatalf("got %+v", res)
	}
}

func TestOrderingAndChainStop(t *testing.T) {
	r, root := newRunner(t, Config{Stop: {{Command: "echo a"}, {Command: "echo b"}, {Command: "echo c"}}})
	res, err := r.Stop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Feedback, ",") != "a,b,c" {
		t.Fatalf("order = %q", res.Feedback)
	}

	log := filepath.Join(root, "log")
	r, err = NewRunner(Config{PreToolUse: {
		{Command: "echo 1 >> " + log},
		{Command: "echo 2 >> " + log + "; echo stop here >&2; exit 2"},
		{Command: "echo 3 >> " + log},
	}}, Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	res = pre(t, r, "bash")
	if !res.Blocked || res.Reason != "stop here" {
		t.Fatalf("got %+v", res)
	}
	data, _ := os.ReadFile(log)
	if string(data) != "1\n2\n" {
		t.Fatalf("ran %q, want hooks 1 and 2 only", data)
	}
}

func TestApprovedClearedByLaterBlock(t *testing.T) {
	r, _ := newRunner(t, Config{PreToolUse: {
		{Command: `echo '{"decision":"allow"}'`},
		{Command: "exit 2"},
	}})
	res := pre(t, r, "bash")
	if !res.Blocked || res.Approved {
		t.Fatalf("got %+v", res)
	}
}

func TestConcurrentUse(t *testing.T) {
	r, _ := newRunner(t, Config{PreToolUse: {{Command: `echo "$GTA_TOOL_NAME"`}}})
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := []string{"bash", "edit"}[i%2]
			res, err := r.PreToolUse(context.Background(), name, nil)
			if err != nil || len(res.Feedback) != 1 || res.Feedback[0] != name {
				t.Errorf("%s: %+v %v", name, res, err)
			}
			r.SetSessionID(name)
		}()
	}
	wg.Wait()
}

func TestOutputIsCapped(t *testing.T) {
	r, _ := newRunner(t, Config{Stop: {{Command: "head -c 200000 /dev/zero | tr '\\0' x"}}})
	res, err := r.Stop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Feedback) != 1 || len(res.Feedback[0]) > maxCapture+64 || !strings.HasSuffix(res.Feedback[0], "[output truncated]") {
		t.Fatalf("feedback length %d", len(res.Feedback[0]))
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"unknown event", Config{"pre_tool": {{Command: "x"}}}, `unknown event "pre_tool"`},
		{"empty command", Config{Stop: {{Command: "  "}}}, "command is empty"},
		{"bad regex", Config{PreToolUse: {{Matcher: "(", Command: "x"}}}, "invalid matcher"},
		{"negative timeout", Config{Stop: {{Command: "x", TimeoutSec: -1}}}, "out of range"},
		{"huge timeout", Config{Stop: {{Command: "x", TimeoutSec: 601}}}, "out of range"},
		{"matcher on prompt", Config{UserPromptSubmit: {{Matcher: "bash", Command: "x"}}}, "only supported for"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if _, err := NewRunner(tc.cfg, Options{Root: t.TempDir()}); err == nil {
				t.Fatal("NewRunner accepted an invalid config")
			}
		})
	}
	ok := Config{PreToolUse: {{Matcher: "bash", Command: "true", TimeoutSec: 5}}, Stop: {{Command: "true"}}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	if ok[PreToolUse][0].re == nil || !ok[PreToolUse][0].matches("bash") || ok[PreToolUse][0].matches("bashx") {
		t.Fatal("matcher not compiled and anchored")
	}
	if ok.Count() != 2 {
		t.Fatalf("count = %d", ok.Count())
	}
}

func TestParseAndLoadFile(t *testing.T) {
	cfg, err := Parse([]byte(`{"pre_tool_use":[{"matcher":"bash","command":"echo hi","timeout_sec":5}],"stop":[{"command":"true"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg[PreToolUse]) != 1 || cfg[PreToolUse][0].TimeoutSec != 5 || len(cfg[Stop]) != 1 {
		t.Fatalf("cfg = %+v", cfg)
	}
	if _, err := Parse([]byte(`{"stop":[{"command":"x","timeout":5}]}`)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field err = %v", err)
	}
	if _, err := Parse([]byte(`{"stop":[]} {}`)); err == nil {
		t.Fatal("trailing data accepted")
	}
	if c, err := Parse([]byte("  ")); err != nil || c.Count() != 0 {
		t.Fatalf("empty: %v %v", c, err)
	}

	dir := t.TempDir()
	if c, err := LoadFile(filepath.Join(dir, "missing.json")); err != nil || c.Count() != 0 {
		t.Fatalf("missing file: %v %v", c, err)
	}
	path := filepath.Join(dir, "hooks.json")
	if err := os.WriteFile(path, []byte(`{"stop":[{"command":""}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(path); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("bad file err = %v", err)
	}
}

func TestRunnerOwnsItsConfig(t *testing.T) {
	cfg := Config{Stop: {{Command: "echo original"}}}
	r, _ := newRunner(t, cfg)
	cfg[Stop][0].Command = "echo changed"
	res, err := r.Stop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Feedback) != 1 || res.Feedback[0] != "original" {
		t.Fatalf("feedback = %q", res.Feedback)
	}
}
