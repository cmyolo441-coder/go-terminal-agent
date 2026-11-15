package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/hooks"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/mockapi"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permissions"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/session"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

// recUI records what the agent shows and answers permission prompts from a script.
type recUI struct {
	mu      sync.Mutex
	text    strings.Builder
	think   strings.Builder
	tools   []string
	results []tools.Result
	asked   []PermissionPrompt
	answer  PermissionAnswer
	notices []string
}

func (u *recUI) Status(string) {}
func (u *recUI) Text(d string) { u.mu.Lock(); u.text.WriteString(d); u.mu.Unlock() }
func (u *recUI) Thinking(d string) {
	u.mu.Lock()
	u.think.WriteString(d)
	u.mu.Unlock()
}
func (u *recUI) BlockEnd() {}
func (u *recUI) ToolStart(_, name, _ string) {
	u.mu.Lock()
	u.tools = append(u.tools, name)
	u.mu.Unlock()
}
func (u *recUI) ToolEnd(_, _, _ string, r tools.Result, _ time.Duration) {
	u.mu.Lock()
	u.results = append(u.results, r)
	u.mu.Unlock()
}
func (u *recUI) ServerTool(string, string) {}
func (u *recUI) Notice(m string)           { u.mu.Lock(); u.notices = append(u.notices, m); u.mu.Unlock() }
func (u *recUI) AskPermission(p PermissionPrompt) PermissionAnswer {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.asked = append(u.asked, p)
	return u.answer
}
func (u *recUI) ApprovePlan(string) (bool, permissions.Mode, string) {
	return true, permissions.ModeAcceptEdits, ""
}

func newAgent(t *testing.T, srv *mockapi.Server, mode permissions.Mode, ui *recUI) (*Agent, string) {
	t.Helper()
	t.Setenv("GOCODE_HOME", t.TempDir())
	cwd := t.TempDir()
	a := &Agent{
		Client:    anthropic.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0)),
		Model:     "claude-opus-5-5",
		Effort:    "high",
		MaxTokens: 1000,
		System:    "system prompt",
		Tools:     tools.Builtin(),
		Env:       tools.NewEnv(cwd),
		Perm:      permissions.New(mode, cwd, nil, nil, nil),
		Hooks:     &hooks.Runner{},
		UI:        ui,
		Session:   session.New(cwd),
		Usage:     &Usage{},
		Fallback:  true,
		WebSearch: true,
		Cwd:       cwd,
	}
	a.RegisterAgentTools()
	return a, cwd
}

func TestToolLoopWritesFileAndPersists(t *testing.T) {
	srv := mockapi.New(
		mockapi.Response{Blocks: []mockapi.Block{
			{Type: "thinking", Text: "I should write the file."},
			{Type: "text", Text: "Creating the file."},
			{Type: "tool_use", ID: "toolu_1", Name: "Write", Input: `{"file_path":"hello.txt","content":"hi\n"}`},
		}},
		mockapi.Response{Blocks: []mockapi.Block{{Type: "text", Text: "Done."}}},
	)
	defer srv.Close()
	ui := &recUI{}
	a, cwd := newAgent(t, srv, permissions.ModeAcceptEdits, ui)

	out, err := a.Run(context.Background(), "make hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if out != "Done." {
		t.Fatalf("final text = %q", out)
	}
	if b, err := os.ReadFile(filepath.Join(cwd, "hello.txt")); err != nil || string(b) != "hi\n" {
		t.Fatalf("file not written: %q %v", b, err)
	}
	if len(ui.asked) != 0 {
		t.Fatalf("acceptEdits should not prompt for an edit inside the workspace: %+v", ui.asked)
	}
	if !strings.Contains(ui.think.String(), "write the file") || !strings.Contains(ui.text.String(), "Creating") {
		t.Fatalf("streamed output missing: think=%q text=%q", ui.think.String(), ui.text.String())
	}

	// Request shape: cached system prompt, adaptive thinking, effort, fallbacks, tools.
	req := srv.Request(0)
	sys := req["system"].([]any)[0].(map[string]any)
	if sys["cache_control"] == nil {
		t.Error("system prompt has no cache_control")
	}
	if th := req["thinking"].(map[string]any); th["type"] != "adaptive" || th["display"] != "summarized" {
		t.Errorf("thinking = %v", th)
	}
	if oc := req["output_config"].(map[string]any); oc["effort"] != "high" {
		t.Errorf("output_config = %v", oc)
	}
	if req["fallbacks"] != "default" {
		t.Errorf("fallbacks = %v", req["fallbacks"])
	}
	names := map[string]bool{}
	for _, tl := range req["tools"].([]any) {
		names[tl.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"Bash", "Read", "Write", "Edit", "Glob", "Grep", "Task", "TodoWrite", "web_search"} {
		if !names[want] {
			t.Errorf("tool %s missing from request", want)
		}
	}

	// The second request carries the assistant turn (thinking kept intact) and the tool result.
	msgs := srv.Request(1)["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("want 3 messages in 2nd request, got %d", len(msgs))
	}
	asst := msgs[1].(map[string]any)["content"].([]any)
	if first := asst[0].(map[string]any); first["type"] != "thinking" || first["signature"] != "sig-mock" {
		t.Errorf("thinking block not replayed unchanged: %v", first)
	}
	res := msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if res["type"] != "tool_result" || res["tool_use_id"] != "toolu_1" {
		t.Errorf("tool result = %v", res)
	}

	// The transcript restores to byte-identical messages.
	_, raw, err := session.Load(a.Session.Path)
	if err != nil || len(raw) != 4 {
		t.Fatalf("transcript: %d messages, err %v", len(raw), err)
	}
	b := &Agent{}
	if err := b.LoadHistory(raw); err != nil {
		t.Fatal(err)
	}
	for i := range raw {
		again, _ := json.Marshal(b.Messages[i])
		if !jsonEqual(raw[i], again) {
			t.Errorf("message %d changed in round trip:\n%s\n%s", i, raw[i], again)
		}
	}
}

func jsonEqual(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return string(xa) == string(ya)
}

func TestPermissionDeclineIsReportedToModel(t *testing.T) {
	srv := mockapi.New(
		mockapi.Response{Blocks: []mockapi.Block{{Type: "tool_use", ID: "t1", Name: "Bash", Input: `{"command":"touch nope.txt"}`}}},
		mockapi.Response{Blocks: []mockapi.Block{{Type: "text", Text: "Okay, skipped."}}},
	)
	defer srv.Close()
	ui := &recUI{answer: PermissionAnswer{Feedback: "use a different name"}}
	a, cwd := newAgent(t, srv, permissions.ModeDefault, ui)
	if _, err := a.Run(context.Background(), "touch a file"); err != nil {
		t.Fatal(err)
	}
	if len(ui.asked) != 1 || ui.asked[0].Rule != "Bash(touch:*)" {
		t.Fatalf("prompt = %+v", ui.asked)
	}
	if _, err := os.Stat(filepath.Join(cwd, "nope.txt")); err == nil {
		t.Fatal("declined command ran anyway")
	}
	res := srv.Request(1)["messages"].([]any)[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if res["is_error"] != true || !strings.Contains(res["content"].([]any)[0].(map[string]any)["text"].(string), "use a different name") {
		t.Errorf("decline not passed back: %v", res)
	}
}

func TestAlwaysAllowAddsRule(t *testing.T) {
	srv := mockapi.New(
		mockapi.Response{Blocks: []mockapi.Block{{Type: "tool_use", ID: "t1", Name: "Bash", Input: `{"command":"touch a.txt"}`}}},
		mockapi.Response{Blocks: []mockapi.Block{{Type: "tool_use", ID: "t2", Name: "Bash", Input: `{"command":"touch b.txt"}`}}},
		mockapi.Response{Blocks: []mockapi.Block{{Type: "text", Text: "ok"}}},
	)
	defer srv.Close()
	ui := &recUI{answer: PermissionAnswer{Allow: true, Always: true}}
	a, cwd := newAgent(t, srv, permissions.ModeDefault, ui)
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if len(ui.asked) != 1 {
		t.Fatalf("asked %d times, want 1", len(ui.asked))
	}
	for _, f := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(cwd, f)); err != nil {
			t.Errorf("%s missing", f)
		}
	}
	b, _ := os.ReadFile(filepath.Join(cwd, ".gocode", "settings.local.json"))
	if !strings.Contains(string(b), "Bash(touch:*)") {
		t.Errorf("rule not saved: %s", b)
	}
}

func TestPlanModeBlocksWritesUntilApproved(t *testing.T) {
	srv := mockapi.New(
		mockapi.Response{Blocks: []mockapi.Block{{Type: "tool_use", ID: "t1", Name: "Write", Input: `{"file_path":"x.txt","content":"1"}`}}},
		mockapi.Response{Blocks: []mockapi.Block{{Type: "tool_use", ID: "t2", Name: "ExitPlanMode", Input: `{"plan":"write x"}`}}},
		mockapi.Response{Blocks: []mockapi.Block{{Type: "tool_use", ID: "t3", Name: "Write", Input: `{"file_path":"x.txt","content":"1"}`}}},
		mockapi.Response{Blocks: []mockapi.Block{{Type: "text", Text: "done"}}},
	)
	defer srv.Close()
	ui := &recUI{}
	a, cwd := newAgent(t, srv, permissions.ModePlan, ui)
	if _, err := a.Run(context.Background(), "plan it"); err != nil {
		t.Fatal(err)
	}
	if !ui.results[0].IsError || !strings.Contains(ui.results[0].Text, "plan mode") {
		t.Errorf("write in plan mode was not denied: %+v", ui.results[0])
	}
	if a.Perm.Mode() != permissions.ModeAcceptEdits {
		t.Errorf("mode after approval = %s", a.Perm.Mode())
	}
	if _, err := os.Stat(filepath.Join(cwd, "x.txt")); err != nil {
		t.Error("file not written after plan approval")
	}
	first := srv.Request(0)["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if !strings.Contains(first[len(first)-1].(map[string]any)["text"].(string), "Plan mode is active") {
		t.Error("plan reminder missing from the prompt")
	}
}

func TestSubAgentAndParallelReads(t *testing.T) {
	srv := mockapi.New(
		// main: launch a sub-agent
		mockapi.Response{Blocks: []mockapi.Block{{Type: "tool_use", ID: "t1", Name: "Task", Input: `{"description":"look","prompt":"list go files"}`}}},
		// sub-agent: two parallel read-only calls, then its report
		mockapi.Response{Blocks: []mockapi.Block{
			{Type: "tool_use", ID: "s1", Name: "Glob", Input: `{"pattern":"*.go"}`},
			{Type: "tool_use", ID: "s2", Name: "Grep", Input: `{"pattern":"package"}`},
		}},
		mockapi.Response{Blocks: []mockapi.Block{{Type: "text", Text: "Found main.go"}}},
		mockapi.Response{Blocks: []mockapi.Block{{Type: "text", Text: "The sub-agent found main.go"}}},
	)
	defer srv.Close()
	ui := &recUI{}
	a, cwd := newAgent(t, srv, permissions.ModeDefault, ui)
	_ = os.WriteFile(filepath.Join(cwd, "main.go"), []byte("package main\n"), 0o644)
	out, err := a.Run(context.Background(), "explore")
	if err != nil || !strings.Contains(out, "main.go") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	// The sub-agent must not be offered Task, and its report comes back as the tool result.
	for _, tl := range srv.Request(1)["tools"].([]any) {
		if tl.(map[string]any)["name"] == "Task" {
			t.Error("sub-agent was offered the Task tool")
		}
	}
	sub := srv.Request(2)["messages"].([]any)
	results := sub[2].(map[string]any)["content"].([]any)
	if len(results) != 2 || !strings.Contains(results[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string), "main.go") {
		t.Errorf("parallel results wrong: %v", results)
	}
	main := srv.Request(3)["messages"].([]any)
	if got := main[2].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]; got != "Found main.go" {
		t.Errorf("task result = %v", got)
	}
	if _, _, _, _, reqs, cost := a.Usage.Snapshot(); reqs != 4 || cost <= 0 {
		t.Errorf("usage not aggregated across sub-agent: reqs=%d cost=%v", reqs, cost)
	}
}

func TestTruncatedToolInputIsDiscarded(t *testing.T) {
	srv := mockapi.New(
		mockapi.Response{StopReason: "max_tokens", Blocks: []mockapi.Block{{Type: "tool_use", ID: "t1", Name: "Write", Input: `{"file_path":"big.txt","content":"abc`}}},
		mockapi.Response{Blocks: []mockapi.Block{{Type: "text", Text: "retrying smaller"}}},
	)
	defer srv.Close()
	ui := &recUI{}
	a, cwd := newAgent(t, srv, permissions.ModeBypass, ui)
	if _, err := a.Run(context.Background(), "write big"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cwd, "big.txt")); err == nil {
		t.Fatal("a truncated tool call was executed")
	}
	msgs := srv.Request(1)["messages"].([]any)
	if len(msgs) != 2 || msgs[1].(map[string]any)["role"] != "user" {
		t.Fatalf("truncated assistant turn should be left out of the history: %v", msgs)
	}
}

func TestCompactReplacesHistory(t *testing.T) {
	srv := mockapi.New(
		mockapi.Response{Blocks: []mockapi.Block{{Type: "text", Text: "hello"}}},
		mockapi.Response{Blocks: []mockapi.Block{{Type: "text", Text: "SUMMARY: user said hi"}}},
	)
	defer srv.Close()
	a, _ := newAgent(t, srv, permissions.ModeDefault, &recUI{})
	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if err := a.Compact(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if len(a.Messages) != 1 {
		t.Fatalf("history has %d messages after compact", len(a.Messages))
	}
	_, raw, _ := session.Load(a.Session.Path)
	if len(raw) != 1 || !strings.Contains(string(raw[0]), "SUMMARY: user said hi") {
		t.Errorf("transcript after compact: %s", raw)
	}
}

func TestAPIErrorIsExplained(t *testing.T) {
	srv := mockapi.New(mockapi.Response{Status: 401})
	defer srv.Close()
	a, _ := newAgent(t, srv, permissions.ModeDefault, &recUI{})
	_, err := a.Run(context.Background(), "hi")
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("err = %v", err)
	}
}
