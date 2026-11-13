package autopilot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/agent"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permission"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

// readOnlyTools are the tools helper agents (planner, reviewer, verifier)
// get, in registration order.
var readOnlyTools = []string{"read_file", "list_dir", "glob", "grep"}

// readOnlyRegistry picks the read-only tools from the parent registry,
// falling back to the built-in implementations. A tool that does not
// declare itself read-only is never included.
func readOnlyRegistry(parent *tools.Registry) *tools.Registry {
	defaults := tools.Default()
	var picked []tools.Tool
	for _, name := range readOnlyTools {
		var tl tools.Tool
		if parent != nil {
			tl, _ = parent.Get(name)
		}
		if tl == nil || tl.Kind() != tools.KindRead {
			tl, _ = defaults.Get(name)
		}
		if tl != nil && tl.Kind() == tools.KindRead {
			picked = append(picked, tl)
		}
	}
	return tools.NewRegistry(picked...)
}

// FormatError is returned when a helper agent's reply could not be parsed
// even after one corrective re-ask.
type FormatError struct {
	Phase Phase
	// What names the expected artifact, e.g. "plan".
	What string
	// Err is the parse error of the second reply.
	Err error
	// First is the parse error of the first reply.
	First error
}

func (e *FormatError) Error() string {
	return fmt.Sprintf("%s: the %s could not be parsed, even after asking again: %v", e.Phase, e.What, e.Err)
}

func (e *FormatError) Unwrap() error { return e.Err }

// helperConfig is what a helper agent needs from its owner.
type helperConfig struct {
	provider     llm.Provider
	settings     agent.Settings
	registry     *tools.Registry
	baseEnv      *tools.Env
	system       string
	phase        Phase
	onActivity   func(Phase, string)
	onUsage      func(Phase, string, llm.Usage)
	onToolCall   func(Phase)
	onNoticeText func(Phase, string)
	setup        func(*agent.Agent)
}

// helper is a read-only agent with its own conversation.
type helper struct {
	phase Phase
	agent *agent.Agent
	sink  *helperSink
}

func newHelper(c helperConfig) (*helper, error) {
	env, err := tools.NewEnv(c.baseEnv.Root)
	if err != nil {
		return nil, fmt.Errorf("%s agent environment: %w", c.phase, err)
	}
	// A fresh Env gives the helper its own read tracking and task list.
	if c.baseEnv.ShellTimeout > 0 {
		env.ShellTimeout = c.baseEnv.ShellTimeout
	}
	if c.baseEnv.OutputLimit > 0 {
		env.OutputLimit = c.baseEnv.OutputLimit
	}
	gate := permission.NewGate(permission.ModePlan, nil)
	a := agent.New(c.provider, c.registry, env, gate, c.system, c.settings)
	if c.setup != nil {
		c.setup(a)
	}
	return &helper{phase: c.phase, agent: a, sink: &helperSink{cfg: c}}, nil
}

// ask runs one turn and returns the text the helper wrote in it.
func (h *helper) ask(ctx context.Context, prompt string) (string, error) {
	before := len(h.agent.History())
	err := h.agent.Run(ctx, llm.UserText(prompt), h.sink)
	if err != nil {
		if errors.Is(err, agent.ErrInterrupted) || ctx.Err() != nil {
			return "", ctxErr(ctx)
		}
		return "", fmt.Errorf("%s agent: %w", h.phase, err)
	}
	h2 := h.agent.History()
	if before > len(h2) {
		before = len(h2)
	}
	var parts []string
	for _, m := range h2[before:] {
		if m.Role == llm.RoleAssistant {
			if t := strings.TrimSpace(m.Text()); t != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

// askJSON asks for a structured reply, re-asking once with the parse error
// when the first reply cannot be used.
func askJSON[T any](ctx context.Context, h *helper, prompt, what string, parse func(string) (T, error)) (T, error) {
	var zero T
	text, err := h.ask(ctx, prompt)
	if err != nil {
		return zero, err
	}
	v, first := parse(text)
	if first == nil {
		return v, nil
	}
	h.sink.notice(fmt.Sprintf("The %s reply could not be parsed (%v); asking again.", what, first))
	text, err = h.ask(ctx, repairPrompt(what, first))
	if err != nil {
		return zero, err
	}
	v, second := parse(text)
	if second != nil {
		return zero, &FormatError{Phase: h.phase, What: what, Err: second, First: first}
	}
	return v, nil
}

// helperSink receives a helper's activity. The agent may call it from
// several goroutines (read-only tools run concurrently).
type helperSink struct {
	cfg helperConfig
}

var _ agent.Sink = (*helperSink)(nil)

func (s *helperSink) Stream(llm.Event) {}

func (s *helperSink) ToolStart(_ string, call *tools.Call) {
	if s.cfg.onToolCall != nil {
		s.cfg.onToolCall(s.cfg.phase)
	}
	if s.cfg.onActivity != nil {
		s.cfg.onActivity(s.cfg.phase, call.Summary)
	}
}

func (s *helperSink) ToolDone(string, *tools.Call, string, error) {}
func (s *helperSink) ToolFailed(string, string, string)           {}

func (s *helperSink) Usage(model string, u llm.Usage) {
	if s.cfg.onUsage != nil {
		s.cfg.onUsage(s.cfg.phase, model, u)
	}
}

func (s *helperSink) Notice(msg string) {
	// The turn-limit notice is addressed to a human; a helper that ran out
	// of turns is handled by the structured re-ask instead.
	if strings.HasPrefix(msg, "Stopped after ") {
		return
	}
	s.notice(msg)
}

func (s *helperSink) notice(msg string) {
	if s.cfg.onNoticeText != nil {
		s.cfg.onNoticeText(s.cfg.phase, msg)
	}
}

// Approve declines everything: helpers are read-only, and the plan-mode
// gate already refuses writes and commands.
func (s *helperSink) Approve(context.Context, *tools.Call, string) permission.Decision {
	return permission.DecisionDeny
}

// ctxErr returns ctx's error, or context.Canceled when the agent reported
// an interruption without one.
func ctxErr(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return context.Canceled
}

// turnSink wraps the UI's sink for turns of the main agent, counting
// usage and actions and noticing declined approvals.
type turnSink struct {
	inner agent.Sink
	// onUsage, onTool and onTodos are optional hooks.
	onUsage func(model string, u llm.Usage)
	onTool  func()
	onTodos func()

	mu        sync.Mutex
	actions   int
	declined  string
	lastAsked string
}

var (
	_ agent.Sink         = (*turnSink)(nil)
	_ agent.ProgressSink = (*turnSink)(nil)
)

func (s *turnSink) Stream(ev llm.Event) { s.inner.Stream(ev) }

func (s *turnSink) ToolStart(id string, call *tools.Call) {
	s.inner.ToolStart(id, call)
	if s.onTool != nil {
		s.onTool()
	}
}

func (s *turnSink) ToolDone(id string, call *tools.Call, output string, err error) {
	s.inner.ToolDone(id, call, output, err)
	if err != nil {
		return
	}
	isTodo := call.Tool == "todo_write"
	if call.Kind != tools.KindRead || isTodo {
		s.mu.Lock()
		s.actions++
		s.mu.Unlock()
	}
	if isTodo && s.onTodos != nil {
		s.onTodos()
	}
}

func (s *turnSink) ToolFailed(id, name, reason string) {
	s.inner.ToolFailed(id, name, reason)
	if reason == "declined" {
		s.mu.Lock()
		s.declined = s.lastAsked
		if s.declined == "" {
			s.declined = name
		}
		s.mu.Unlock()
	}
}

func (s *turnSink) Usage(model string, u llm.Usage) {
	s.inner.Usage(model, u)
	if s.onUsage != nil {
		s.onUsage(model, u)
	}
}

func (s *turnSink) Notice(msg string) { s.inner.Notice(msg) }

func (s *turnSink) Approve(ctx context.Context, call *tools.Call, reason string) permission.Decision {
	s.mu.Lock()
	s.lastAsked = call.Summary
	s.mu.Unlock()
	return s.inner.Approve(ctx, call, reason)
}

// SubagentProgress forwards sub-agent progress when the UI's sink shows it.
func (s *turnSink) SubagentProgress(index int, description, event string) {
	if p, ok := s.inner.(agent.ProgressSink); ok {
		p.SubagentProgress(index, description, event)
	}
}

func (s *turnSink) actionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.actions
}

// takeDeclined returns and clears the declined action.
func (s *turnSink) takeDeclined() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.declined
	s.declined = ""
	return d
}
