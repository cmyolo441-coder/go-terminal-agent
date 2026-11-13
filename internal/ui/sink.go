package ui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permission"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

// Messages the agent goroutine sends to the UI.
type (
	streamMsg    struct{ ev llm.Event }
	toolStartMsg struct {
		id   string
		call *tools.Call
	}
	toolDoneMsg struct {
		id     string
		call   *tools.Call
		output string
		err    error
	}
	toolFailedMsg struct{ id, name, reason string }
	usageMsg      struct {
		model string
		usage llm.Usage
	}
	noticeMsg  struct{ text string }
	todosMsg   struct{ todos []tools.Todo }
	approveMsg struct {
		call   *tools.Call
		reason string
		reply  chan permission.Decision
	}
	runDoneMsg   struct{ err error }
	shellDoneMsg struct {
		cmd, output string
		err         error
	}
)

// teaSink forwards agent activity into the Bubble Tea program.
type teaSink struct{ p *tea.Program }

func (s teaSink) Stream(ev llm.Event)                { s.p.Send(streamMsg{ev}) }
func (s teaSink) ToolStart(id string, c *tools.Call) { s.p.Send(toolStartMsg{id, c}) }
func (s teaSink) ToolFailed(id, name, reason string) { s.p.Send(toolFailedMsg{id, name, reason}) }
func (s teaSink) Usage(model string, u llm.Usage)    { s.p.Send(usageMsg{model, u}) }
func (s teaSink) Notice(m string)                    { s.p.Send(noticeMsg{m}) }
func (s teaSink) ToolDone(id string, c *tools.Call, out string, err error) {
	s.p.Send(toolDoneMsg{id, c, out, err})
}

func (s teaSink) Approve(ctx context.Context, c *tools.Call, reason string) permission.Decision {
	reply := make(chan permission.Decision, 1)
	s.p.Send(approveMsg{call: c, reason: reason, reply: reply})
	select {
	case d := <-reply:
		return d
	case <-ctx.Done():
		return permission.DecisionDeny
	}
}
