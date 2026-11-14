package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permission"
)

// hooked runs before each scripted response, to act mid-turn.
type hooked struct {
	*scripted
	before func(call int)
	n      int
}

func (h *hooked) Stream(ctx context.Context, req llm.Request, on func(llm.Event)) (*llm.Response, error) {
	h.before(h.n)
	h.n++
	return h.scripted.Stream(ctx, req, on)
}

func lastUserText(req llm.Request) string {
	m := req.Messages[len(req.Messages)-1]
	var b strings.Builder
	for _, bl := range m.Blocks {
		if bl.Type == llm.BlockText {
			b.WriteString(bl.Text)
		}
	}
	return b.String()
}

func TestSteerJoinsNextModelCall(t *testing.T) {
	s := &scripted{responses: []*llm.Response{
		reply(llm.StopToolUse, toolUse("t1", "list_dir", map[string]any{})),
		reply(llm.StopEndTurn, llm.Block{Type: llm.BlockText, Text: "ok"}),
	}}
	var a *Agent
	p := &hooked{scripted: s, before: func(call int) {
		if call == 0 && !a.Steer("use the staging database instead") {
			t.Error("Steer refused during a running turn")
		}
	}}
	a, _ = newAgent(t, p, permission.ModeAsk)
	if a.Steer("nobody is listening") {
		t.Fatal("Steer accepted with no turn running")
	}
	if err := a.Run(context.Background(), llm.UserText("migrate"), &sink{}); err != nil {
		t.Fatal(err)
	}
	if len(s.requests) != 2 {
		t.Fatalf("model calls = %d", len(s.requests))
	}
	second := s.requests[1]
	last := second.Messages[len(second.Messages)-1]
	if last.Blocks[0].Type != llm.BlockToolResult {
		t.Fatalf("tool result must come first, got %v", last.Blocks[0].Type)
	}
	if got := lastUserText(second); !strings.Contains(got, "use the staging database instead") || !strings.Contains(got, "<user-steering>") {
		t.Fatalf("steering missing from the next call: %q", got)
	}
	if left := a.TakeSteering(); len(left) != 0 {
		t.Fatalf("steering left over: %v", left)
	}
}

func TestSteerDuringFinalReplyContinuesTurn(t *testing.T) {
	s := &scripted{responses: []*llm.Response{
		reply(llm.StopEndTurn, llm.Block{Type: llm.BlockText, Text: "All done."}),
		reply(llm.StopEndTurn, llm.Block{Type: llm.BlockText, Text: "Switched to tabs."}),
	}}
	var a *Agent
	p := &hooked{scripted: s, before: func(call int) {
		if call == 0 {
			a.Steer("actually use tabs")
		}
	}}
	a, _ = newAgent(t, p, permission.ModeAsk)
	if err := a.Run(context.Background(), llm.UserText("format it"), &sink{}); err != nil {
		t.Fatal(err)
	}
	if len(s.requests) != 2 || !strings.Contains(lastUserText(s.requests[1]), "actually use tabs") {
		t.Fatalf("turn did not continue with the steering: %d calls", len(s.requests))
	}
	h := a.History()
	for i := 1; i < len(h); i++ {
		if h[i].Role == h[i-1].Role {
			t.Fatalf("roles do not alternate at %d", i)
		}
	}
}
