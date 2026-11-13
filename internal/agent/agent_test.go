package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permission"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

// scripted replays canned responses and records requests.
type scripted struct {
	mu        sync.Mutex
	responses []*llm.Response
	requests  []llm.Request
}

func (s *scripted) Name() string { return "fake" }

func (s *scripted) Stream(ctx context.Context, req llm.Request, on func(llm.Event)) (*llm.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, req)
	r := s.responses[0]
	s.responses = s.responses[1:]
	if t := r.Message.Text(); t != "" {
		on(llm.Event{Type: llm.EventTextDelta, Text: t})
	}
	return r, nil
}

func toolUse(id, name string, input any) llm.Block {
	raw, _ := json.Marshal(input)
	return llm.Block{Type: llm.BlockToolUse, ToolUseID: id, ToolName: name, Input: raw}
}

func reply(stop llm.StopReason, blocks ...llm.Block) *llm.Response {
	return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Blocks: blocks}, StopReason: stop, Model: "claude-opus-5-5",
		Usage: llm.Usage{InputTokens: 100, OutputTokens: 10}}
}

type sink struct {
	mu       sync.Mutex
	decision permission.Decision
	asked    []string
	done     []string
	failed   []string
	notices  []string
}

func (s *sink) Stream(llm.Event)              {}
func (s *sink) ToolStart(string, *tools.Call) {}
func (s *sink) Usage(string, llm.Usage)       {}
func (s *sink) Notice(m string)               { s.notices = append(s.notices, m) }
func (s *sink) ToolFailed(_, name, reason string) {
	s.mu.Lock()
	s.failed = append(s.failed, name+": "+reason)
	s.mu.Unlock()
}
func (s *sink) ToolDone(_ string, c *tools.Call, out string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.done = append(s.done, c.Tool)
}
func (s *sink) Approve(_ context.Context, c *tools.Call, _ string) permission.Decision {
	s.asked = append(s.asked, c.Summary)
	return s.decision
}

func newAgent(t *testing.T, p llm.Provider, mode permission.Mode) (*Agent, string) {
	t.Helper()
	root := t.TempDir()
	env, err := tools.NewEnv(root)
	if err != nil {
		t.Fatal(err)
	}
	return New(p, tools.Default(), env, permission.NewGate(mode, nil), "sys", Settings{Model: "claude-opus-5-5", MaxTokens: 4096}), root
}

func TestLoopRunsToolsAndFeedsResultsBack(t *testing.T) {
	p := &scripted{responses: []*llm.Response{
		reply(llm.StopToolUse,
			llm.Block{Type: llm.BlockText, Text: "Creating it."},
			toolUse("t1", "write_file", map[string]any{"path": "hello.txt", "content": "hi\n"}),
			toolUse("t2", "list_dir", map[string]any{})),
		reply(llm.StopEndTurn, llm.Block{Type: llm.BlockText, Text: "Done."}),
	}}
	a, root := newAgent(t, p, permission.ModeAsk)
	s := &sink{decision: permission.DecisionAllowOnce}
	if err := a.Run(context.Background(), llm.UserText("make hello.txt"), s); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "hello.txt")); string(got) != "hi\n" {
		t.Fatalf("file not written: %q", got)
	}
	if len(s.asked) != 1 || !strings.Contains(s.asked[0], "hello.txt") {
		t.Fatalf("expected one approval prompt, got %v", s.asked)
	}
	h := a.History()
	if len(h) != 4 {
		t.Fatalf("history length %d", len(h))
	}
	res := h[2]
	if res.Role != llm.RoleUser || len(res.Blocks) != 2 || res.Blocks[0].ToolUseID != "t1" || res.Blocks[1].ToolUseID != "t2" {
		t.Fatalf("tool results not in call order: %+v", res)
	}
	if len(p.requests) != 2 || len(p.requests[1].Messages) != 3 {
		t.Fatalf("second request should carry results: %d", len(p.requests[1].Messages))
	}
	u, cost, _ := a.Stats()
	if u.InputTokens != 200 || cost <= 0 {
		t.Fatalf("usage not recorded: %+v %v", u, cost)
	}
}

func TestDeclineStopsTurnAndNextMessageMerges(t *testing.T) {
	p := &scripted{responses: []*llm.Response{
		reply(llm.StopToolUse, toolUse("t1", "bash", map[string]any{"command": "echo hi"})),
		reply(llm.StopEndTurn, llm.Block{Type: llm.BlockText, Text: "ok"}),
	}}
	a, _ := newAgent(t, p, permission.ModeAsk)
	s := &sink{decision: permission.DecisionDeny}
	if err := a.Run(context.Background(), llm.UserText("run it"), s); err != nil {
		t.Fatal(err)
	}
	if len(p.requests) != 1 {
		t.Fatalf("declined action should end the turn, got %d requests", len(p.requests))
	}
	if err := a.Run(context.Background(), llm.UserText("do something else"), s); err != nil {
		t.Fatal(err)
	}
	h := a.History()
	last := h[2]
	if len(h) != 4 || last.Role != llm.RoleUser || len(last.Blocks) != 2 || !last.Blocks[0].IsError || last.Blocks[1].Text != "do something else" {
		t.Fatalf("expected merged user turn, got %+v", h)
	}
}

func TestInvalidInputAndUnknownToolBecomeErrors(t *testing.T) {
	p := &scripted{responses: []*llm.Response{
		reply(llm.StopToolUse, toolUse("t1", "read_file", map[string]any{"wrong": 1}), toolUse("t2", "nope", map[string]any{})),
		reply(llm.StopEndTurn),
	}}
	a, _ := newAgent(t, p, permission.ModeYolo)
	s := &sink{}
	if err := a.Run(context.Background(), llm.UserText("x"), s); err != nil {
		t.Fatal(err)
	}
	res := a.History()[2].Blocks
	if !res[0].IsError || !res[1].IsError || len(s.failed) != 2 {
		t.Fatalf("expected two error results: %+v %v", res, s.failed)
	}
}

func TestPlanModeRefusesWrites(t *testing.T) {
	p := &scripted{responses: []*llm.Response{
		reply(llm.StopToolUse, toolUse("t1", "write_file", map[string]any{"path": "a", "content": "b"})),
		reply(llm.StopEndTurn),
	}}
	a, root := newAgent(t, p, permission.ModePlan)
	if err := a.Run(context.Background(), llm.UserText("x"), &sink{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "a")); err == nil {
		t.Fatal("plan mode must not write")
	}
}

func TestMaxTokensWithToolUseDoesNotRunTool(t *testing.T) {
	p := &scripted{responses: []*llm.Response{
		reply(llm.StopMaxTokens, toolUse("t1", "write_file", map[string]any{"path": "a", "content": "partial"})),
		reply(llm.StopEndTurn),
	}}
	a, root := newAgent(t, p, permission.ModeYolo)
	if err := a.Run(context.Background(), llm.UserText("x"), &sink{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "a")); err == nil {
		t.Fatal("truncated tool call must not run")
	}
	if r := a.History()[2].Blocks[0]; !r.IsError {
		t.Fatal("expected error result")
	}
}

func TestRefusalIsNotKept(t *testing.T) {
	p := &scripted{responses: []*llm.Response{{StopReason: llm.StopRefusal, StopDetail: "policy", Message: llm.Message{Role: llm.RoleAssistant}}}}
	a, _ := newAgent(t, p, permission.ModeAsk)
	s := &sink{}
	if err := a.Run(context.Background(), llm.UserText("x"), s); err != nil {
		t.Fatal(err)
	}
	if len(a.History()) != 1 || len(s.notices) != 1 {
		t.Fatalf("refusal handling: %+v %v", a.History(), s.notices)
	}
}

func TestPauseTurnContinues(t *testing.T) {
	p := &scripted{responses: []*llm.Response{reply(llm.StopPauseTurn), reply(llm.StopEndTurn)}}
	a, _ := newAgent(t, p, permission.ModeAsk)
	if err := a.Run(context.Background(), llm.UserText("x"), &sink{}); err != nil {
		t.Fatal(err)
	}
	if len(p.requests) != 2 {
		t.Fatalf("pause_turn should resume, got %d requests", len(p.requests))
	}
}
