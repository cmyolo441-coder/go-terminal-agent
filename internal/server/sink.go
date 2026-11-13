package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/agent"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permission"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

// Event is one Server-Sent Event: Type becomes the SSE event name and Data
// is sent as JSON.
type Event struct {
	Type string
	Data any
}

// SSE event names.
const (
	EventTurnStart        = "turn_start"
	EventText             = "text"
	EventThinking         = "thinking"
	EventToolCall         = "tool_call"
	EventServerTool       = "server_tool"
	EventToolStart        = "tool_start"
	EventToolDone         = "tool_done"
	EventToolFailed       = "tool_failed"
	EventApprovalRequired = "approval_required"
	EventApprovalResolved = "approval_resolved"
	EventUsage            = "usage"
	EventNotice           = "notice"
	EventSubagent         = "subagent"
	EventError            = "error"
	EventDone             = "done"
)

// Decision names accepted by the approvals endpoint.
const (
	DecisionAllowOnce    = "allow_once"
	DecisionAllowSession = "allow_session"
	DecisionDeny         = "deny"
)

// ParseDecision maps an API decision name to a permission.Decision.
func ParseDecision(s string) (permission.Decision, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case DecisionAllowOnce, "allow", "yes":
		return permission.DecisionAllowOnce, nil
	case DecisionAllowSession, "always":
		return permission.DecisionAllowSession, nil
	case DecisionDeny, "no":
		return permission.DecisionDeny, nil
	}
	return permission.DecisionDeny, fmt.Errorf("decision %q must be allow_once, allow_session or deny", s)
}

func decisionName(d permission.Decision) string {
	switch d {
	case permission.DecisionAllowOnce:
		return DecisionAllowOnce
	case permission.DecisionAllowSession:
		return DecisionAllowSession
	}
	return DecisionDeny
}

// ErrUnknownApproval is returned when resolving an approval that is not
// pending (never existed, already answered, or timed out).
var ErrUnknownApproval = errors.New("no pending approval with that id")

// DefaultApprovalTimeout is how long a call waits for an API decision
// before it is denied.
const DefaultApprovalTimeout = 5 * time.Minute

// DefaultMaxEventOutput caps tool output carried in a tool_done event.
const DefaultMaxEventOutput = 16 << 10

// EventSink implements agent.Sink by turning agent activity into Events,
// and answers approval prompts with decisions posted to the API.
type EventSink struct {
	emit      func(Event)
	timeout   time.Duration
	maxOutput int

	mu      sync.Mutex
	pending map[string]*pendingApproval
}

type pendingApproval struct {
	ch      chan permission.Decision
	call    *tools.Call
	expires time.Time
}

var (
	_ agent.Sink         = (*EventSink)(nil)
	_ agent.ProgressSink = (*EventSink)(nil)
)

// NewEventSink builds a sink. emit must not block for long; it is called
// from the agent's goroutines. A zero timeout uses DefaultApprovalTimeout.
func NewEventSink(emit func(Event), approvalTimeout time.Duration) *EventSink {
	if approvalTimeout <= 0 {
		approvalTimeout = DefaultApprovalTimeout
	}
	return &EventSink{emit: emit, timeout: approvalTimeout, maxOutput: DefaultMaxEventOutput, pending: map[string]*pendingApproval{}}
}

// SetMaxOutput changes how much tool output a tool_done event carries.
func (s *EventSink) SetMaxOutput(n int) {
	if n > 0 {
		s.maxOutput = n
	}
}

// Stream implements agent.Sink.
func (s *EventSink) Stream(ev llm.Event) {
	switch ev.Type {
	case llm.EventTextDelta:
		if ev.Text != "" {
			s.emit(Event{EventText, map[string]any{"text": ev.Text}})
		}
	case llm.EventThinkingDelta:
		if ev.Text != "" {
			s.emit(Event{EventThinking, map[string]any{"text": ev.Text}})
		}
	case llm.EventToolUseStart:
		s.emit(Event{EventToolCall, map[string]any{"id": ev.ToolID, "name": ev.ToolName}})
	case llm.EventServerToolStart:
		s.emit(Event{EventServerTool, map[string]any{"id": ev.ToolID, "name": ev.ToolName}})
	}
}

// ToolStart implements agent.Sink.
func (s *EventSink) ToolStart(id string, c *tools.Call) {
	s.emit(Event{EventToolStart, callInfo(id, c)})
}

// ToolDone implements agent.Sink.
func (s *EventSink) ToolDone(id string, c *tools.Call, output string, err error) {
	out, cut := clipOutput(output, s.maxOutput)
	data := map[string]any{"id": id, "tool": c.Tool, "output": out, "truncated": cut, "is_error": err != nil}
	if err != nil {
		data["error"] = err.Error()
	}
	s.emit(Event{EventToolDone, data})
}

// ToolFailed implements agent.Sink.
func (s *EventSink) ToolFailed(id, name, reason string) {
	s.emit(Event{EventToolFailed, map[string]any{"id": id, "tool": name, "reason": reason}})
}

// Usage implements agent.Sink.
func (s *EventSink) Usage(model string, u llm.Usage) {
	s.emit(Event{EventUsage, map[string]any{"model": model, "usage": u}})
}

// Notice implements agent.Sink.
func (s *EventSink) Notice(msg string) {
	s.emit(Event{EventNotice, map[string]any{"message": msg}})
}

// SubagentProgress implements agent.ProgressSink.
func (s *EventSink) SubagentProgress(index int, description, event string) {
	s.emit(Event{EventSubagent, map[string]any{"index": index, "description": description, "event": event}})
}

// Approve implements agent.Sink: it announces an approval_required event
// and waits for Resolve, the turn's cancellation, or the timeout, which
// denies the call.
func (s *EventSink) Approve(ctx context.Context, c *tools.Call, reason string) permission.Decision {
	id := newID("apr_")
	p := &pendingApproval{ch: make(chan permission.Decision, 1), call: c, expires: time.Now().Add(s.timeout)}
	s.mu.Lock()
	s.pending[id] = p
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()

	data := callInfo("", c)
	delete(data, "id")
	data["request_id"] = id
	data["reason"] = reason
	data["expires_at"] = p.expires.UTC().Format(time.RFC3339)
	data["destructive"] = c.Kind == tools.KindExec && permission.IsDestructive(c.Command)
	s.emit(Event{EventApprovalRequired, data})

	timer := time.NewTimer(s.timeout)
	defer timer.Stop()
	resolved := func(d permission.Decision, by string) permission.Decision {
		s.emit(Event{EventApprovalResolved, map[string]any{"request_id": id, "decision": decisionName(d), "by": by}})
		return d
	}
	select {
	case d := <-p.ch:
		return resolved(d, "api")
	case <-timer.C:
		return resolved(permission.DecisionDeny, "timeout")
	case <-ctx.Done():
		return resolved(permission.DecisionDeny, "cancelled")
	}
}

// Resolve answers a pending approval.
func (s *EventSink) Resolve(id string, d permission.Decision) error {
	s.mu.Lock()
	p, ok := s.pending[id]
	if ok {
		delete(s.pending, id)
	}
	s.mu.Unlock()
	if !ok {
		return ErrUnknownApproval
	}
	p.ch <- d
	return nil
}

// Pending lists the ids of approvals waiting for a decision.
func (s *EventSink) Pending() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.pending))
	for id := range s.pending {
		out = append(out, id)
	}
	return out
}

func callInfo(id string, c *tools.Call) map[string]any {
	data := map[string]any{"id": id, "tool": c.Tool, "kind": c.Kind.String(), "summary": c.Summary}
	if c.Preview != "" {
		prev, _ := clipOutput(c.Preview, DefaultMaxEventOutput)
		data["preview"] = prev
	}
	if c.Command != "" {
		data["command"] = c.Command
	}
	if c.Path != "" {
		data["path"] = c.Path
		data["outside_root"] = c.OutsideRoot
	}
	return data
}

func clipOutput(s string, n int) (string, bool) {
	if n <= 0 || len(s) <= n {
		return s, false
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

func newID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("server: crypto/rand failed: " + err.Error())
	}
	return prefix + hex.EncodeToString(b[:])
}
