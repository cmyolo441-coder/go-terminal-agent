package autopilot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/agent"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permission"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

const testModel = "claude-opus-5-5"

// Roles of the conversations a fake provider serves, told apart by their
// system prompts.
const (
	rolePlanner  = "planner"
	roleReviewer = "reviewer"
	roleVerifier = "verifier"
	roleMain     = "main"
)

func roleOf(req llm.Request) string {
	switch {
	case strings.HasPrefix(req.System, "You are the planning agent"):
		return rolePlanner
	case strings.HasPrefix(req.System, "You are a senior staff engineer reviewing"):
		return roleReviewer
	case strings.HasPrefix(req.System, "You are the verification agent"):
		return roleVerifier
	default:
		return roleMain
	}
}

// step produces one scripted response.
type step func(ctx context.Context, req llm.Request) (*llm.Response, error)

// fake is a scripted provider with one queue of steps per role. It is safe
// for concurrent use.
type fake struct {
	mu       sync.Mutex
	queues   map[string][]step
	requests map[string][]llm.Request
}

func newFake() *fake {
	return &fake{queues: map[string][]step{}, requests: map[string][]llm.Request{}}
}

// on appends steps to a role's queue.
func (f *fake) on(role string, steps ...step) *fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queues[role] = append(f.queues[role], steps...)
	return f
}

func (f *fake) Name() string { return "fake" }

func (f *fake) Stream(ctx context.Context, req llm.Request, on func(llm.Event)) (*llm.Response, error) {
	role := roleOf(req)
	f.mu.Lock()
	f.requests[role] = append(f.requests[role], req)
	q := f.queues[role]
	if len(q) == 0 {
		f.mu.Unlock()
		return nil, fmt.Errorf("fake provider: no scripted reply left for %s (request %d)", role, len(f.requests[role]))
	}
	s := q[0]
	f.queues[role] = q[1:]
	f.mu.Unlock()
	r, err := s(ctx, req)
	if err == nil && r != nil {
		if t := r.Message.Text(); t != "" {
			on(llm.Event{Type: llm.EventTextDelta, Text: t})
		}
	}
	return r, err
}

func (f *fake) reqs(role string) []llm.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]llm.Request(nil), f.requests[role]...)
}

func (f *fake) remaining(role string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queues[role])
}

// lastUserText is the text of the newest user message of a request.
func lastUserText(req llm.Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if m := req.Messages[i]; m.Role == llm.RoleUser {
			var parts []string
			for _, b := range m.Blocks {
				if b.Type == llm.BlockText {
					parts = append(parts, b.Text)
				}
			}
			if len(parts) > 0 {
				return strings.Join(parts, "\n")
			}
		}
	}
	return ""
}

func response(stop llm.StopReason, blocks ...llm.Block) *llm.Response {
	return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Blocks: blocks}, StopReason: stop, Model: testModel,
		Usage: llm.Usage{InputTokens: 100, OutputTokens: 10}}
}

// say replies with text and ends the turn.
func say(text string) step {
	return func(context.Context, llm.Request) (*llm.Response, error) {
		return response(llm.StopEndTurn, llm.Block{Type: llm.BlockText, Text: text}), nil
	}
}

// call replies with tool calls.
func call(uses ...llm.Block) step {
	return func(context.Context, llm.Request) (*llm.Response, error) {
		return response(llm.StopToolUse, uses...), nil
	}
}

var useCounter struct {
	sync.Mutex
	n int
}

func use(name string, input any) llm.Block {
	raw, _ := json.Marshal(input)
	useCounter.Lock()
	useCounter.n++
	id := fmt.Sprintf("toolu_%d", useCounter.n)
	useCounter.Unlock()
	return llm.Block{Type: llm.BlockToolUse, ToolUseID: id, ToolName: name, Input: raw}
}

func todos(statuses ...string) llm.Block {
	var list []map[string]string
	for i, s := range statuses {
		list = append(list, map[string]string{"content": fmt.Sprintf("%d. step %d", i+1, i+1), "status": s})
	}
	return use("todo_write", map[string]any{"todos": list})
}

// fenced wraps JSON in a reply.
func fenced(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(err)
	}
	return "Here is my answer.\n```json\n" + string(b) + "\n```"
}

// uiSink is a recording agent.Sink standing in for the UI.
type uiSink struct {
	mu       sync.Mutex
	decision permission.Decision
	usage    int
	tools    []string
	failed   []string
	notices  []string
	asked    []string
}

var _ agent.Sink = (*uiSink)(nil)

func (s *uiSink) Stream(llm.Event) {}
func (s *uiSink) ToolStart(_ string, c *tools.Call) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools = append(s.tools, c.Tool)
}
func (s *uiSink) ToolDone(string, *tools.Call, string, error) {}
func (s *uiSink) ToolFailed(_, name, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed = append(s.failed, name+": "+reason)
}
func (s *uiSink) Usage(string, llm.Usage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usage++
}
func (s *uiSink) Notice(m string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notices = append(s.notices, m)
}
func (s *uiSink) Approve(_ context.Context, c *tools.Call, _ string) permission.Decision {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, c.Summary)
	return s.decision
}

// newMainAgent builds the session's main agent over root.
func newMainAgent(t *testing.T, p llm.Provider, root string, mode permission.Mode) *agent.Agent {
	t.Helper()
	env, err := tools.NewEnv(root)
	if err != nil {
		t.Fatal(err)
	}
	return agent.New(p, tools.Default(), env, permission.NewGate(mode, nil), "main agent system prompt",
		agent.Settings{Model: testModel, MaxTokens: 4096, MaxTurns: 20})
}
