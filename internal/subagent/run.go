package subagent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/agent"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permission"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

// Fixed progress events. Tool activity events are the call's Summary.
const (
	// EventQueued is sent for every task when a batch launches.
	EventQueued = "Queued"
	// EventStarted is sent when a child begins (after waiting for a slot).
	EventStarted = "Started"
	// EventDonePrefix starts the terminal event of a child that produced a report.
	EventDonePrefix = "Done"
	// EventFailedPrefix starts the terminal event of a child that failed.
	EventFailedPrefix = "Failed"
	// EventCancelled is the terminal event of a child stopped by cancellation.
	EventCancelled = "Cancelled"
	// EventApprovalPrefix starts the event sent while a child waits for approval.
	EventApprovalPrefix = "Waiting for approval: "
)

// Terminal reports whether a progress event is the last one for its child.
func Terminal(event string) bool {
	return event == EventCancelled || strings.HasPrefix(event, EventDonePrefix) || strings.HasPrefix(event, EventFailedPrefix)
}

// Result is the outcome of one child.
type Result struct {
	// Index is the task's 0-based position in the call.
	Index       int
	Description string
	// Report is the child's final report: the text of its last assistant
	// message. On failure it holds whatever the child last said, if anything.
	Report string
	// Err is non-nil when the child did not finish its task.
	Err error
	// Notes are conditions worth knowing about a report, such as hitting the
	// turn limit.
	Notes      []string
	Usage      llm.Usage
	ModelCalls int
	ToolCalls  int
	Duration   time.Duration
}

// Report is the outcome of one batch.
type Report struct {
	Mode       Mode
	Model      string
	Results    []Result
	Usage      llm.Usage
	ModelCalls int
	ToolCalls  int
	Duration   time.Duration
}

// Failed counts the children that did not finish.
func (r *Report) Failed() int {
	n := 0
	for _, res := range r.Results {
		if res.Err != nil {
			n++
		}
	}
	return n
}

// Run launches one child per task, at most MaxParallel at a time, waits for
// all of them and returns their results in task order. A failing child never
// affects its siblings. The error is non-nil only when nothing could be
// launched (the report is then nil) or when ctx was cancelled (the report
// then holds whatever finished).
func (t *Tool) Run(ctx context.Context, env *tools.Env, mode Mode, tasks []Task) (*Report, error) {
	if err := t.ready(env); err != nil {
		return nil, err
	}
	if mode != ModeExplore && mode != ModeGeneral {
		return nil, fmt.Errorf("unknown sub-agent mode %q", mode)
	}
	if len(tasks) == 0 {
		return nil, errors.New("no tasks to run")
	}
	settings := t.settings()
	b := &batch{
		tool:     t,
		env:      env,
		mode:     mode,
		settings: settings,
		total:    len(tasks),
		slots:    make(chan struct{}, t.cfg.MaxParallel),
	}
	if mode == ModeGeneral {
		// One gate per batch, so "allow for this session" answered for one
		// child also covers its siblings.
		gm := permission.ModeAsk
		if t.cfg.ParentMode != nil {
			gm = t.cfg.ParentMode()
		}
		b.gate = permission.NewGate(gm, t.cfg.AllowCommands)
	}

	start := time.Now()
	for i, tk := range tasks {
		t.progress(i, tk.Description, EventQueued)
	}
	results := make([]Result, len(tasks))
	var wg sync.WaitGroup
	for i, tk := range tasks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = b.runChild(ctx, i, tk)
		}()
	}
	wg.Wait()

	rep := &Report{Mode: mode, Model: settings.Model, Results: results, Duration: time.Since(start)}
	for _, r := range results {
		rep.Usage.Add(r.Usage)
		rep.ModelCalls += r.ModelCalls
		rep.ToolCalls += r.ToolCalls
	}
	if err := ctx.Err(); err != nil {
		return rep, err
	}
	return rep, nil
}

// batch is the shared state of one Run.
type batch struct {
	tool     *Tool
	env      *tools.Env
	mode     Mode
	settings agent.Settings
	total    int
	slots    chan struct{}
	gate     *permission.Gate // shared by general children; nil for explore
}

// runChild runs one task to completion and never panics.
func (b *batch) runChild(ctx context.Context, idx int, tk Task) (res Result) {
	t := b.tool
	res = Result{Index: idx, Description: tk.Description}
	start := time.Now()
	sink := &childSink{tool: t, idx: idx, desc: tk.Description}
	defer func() {
		if r := recover(); r != nil {
			res.Err = fmt.Errorf("sub-agent crashed: %v", r)
		}
		res.Duration = time.Since(start)
		res.Usage, res.ModelCalls, res.ToolCalls = sink.stats()
		t.progress(idx, tk.Description, terminalEvent(res))
	}()

	select {
	case b.slots <- struct{}{}:
	case <-ctx.Done():
		res.Err = ctx.Err()
		return res
	}
	defer func() { <-b.slots }()
	if err := ctx.Err(); err != nil {
		res.Err = err
		return res
	}
	t.progress(idx, tk.Description, EventStarted)

	child, err := b.newChild(idx)
	if err != nil {
		res.Err = err
		return res
	}
	runErr := child.Run(ctx, llm.UserText(tk.Prompt), sink)

	history := child.History()
	report, finalHasText := finalReport(history)
	res.Report = report
	// The turn limit cut the child off when its last model call still asked
	// for tools (the agent loop ends without feeding their results back).
	cutOff := sink.modelCalls() >= b.settings.MaxTurns && endsWithToolUse(history)
	switch {
	case errors.Is(runErr, agent.ErrInterrupted):
		res.Err = ctx.Err()
		if res.Err == nil {
			res.Err = context.Canceled
		}
	case runErr != nil:
		res.Err = runErr
	case sink.declinedAction() != "":
		res.Err = fmt.Errorf("stopped because the user declined: %s", sink.declinedAction())
	case cutOff && report == "":
		res.Err = fmt.Errorf("stopped at the %d-turn limit before writing a report", b.settings.MaxTurns)
	case report == "":
		res.Err = errors.New("finished without a report")
	}
	if res.Err == nil && cutOff {
		res.Notes = append(res.Notes, fmt.Sprintf("Stopped at the %d-turn limit before finishing; this is its latest message and may be incomplete.", b.settings.MaxTurns))
	} else if res.Err == nil && !finalHasText {
		res.Notes = append(res.Notes, "The final message had no text; this is the sub-agent's latest earlier message.")
	}
	res.Notes = append(res.Notes, sink.noticeList()...)
	return res
}

// newChild builds a child agent with its own environment, tools and gate.
func (b *batch) newChild(idx int) (*agent.Agent, error) {
	// A forked Env gives the child its own read tracking (so it must read a
	// file before editing it, even if the parent or a sibling already did)
	// and its own task list, which never overwrites the parent's, while
	// keeping the parent's sandbox and policy hooks.
	env := b.env.Fork()
	var (
		reg  *tools.Registry
		gate *permission.Gate
	)
	if b.mode == ModeExplore {
		reg = readOnlyRegistry(b.tool.cfg.Tools)
		gate = permission.NewGate(permission.ModePlan, nil)
	} else {
		reg = generalRegistry(b.tool.cfg.Tools)
		gate = b.gate
	}
	sys := systemPrompt(b.mode, env.Root, reg.Names(), idx, b.total, b.settings.MaxTurns, b.tool.cfg.Instructions)
	child := agent.New(b.tool.cfg.Provider, reg, env, gate, sys, b.settings)
	if b.tool.cfg.Setup != nil {
		b.tool.cfg.Setup(child)
	}
	return child, nil
}

// readOnlyRegistry picks the read-only tools from the parent's registry,
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

// generalRegistry is the parent's tool set without the task tool, so
// children cannot launch sub-agents of their own.
func generalRegistry(parent *tools.Registry) *tools.Registry {
	if parent == nil {
		parent = tools.Default()
	}
	var picked []tools.Tool
	for _, spec := range parent.Specs() {
		if spec.Name == Name {
			continue
		}
		if tl, ok := parent.Get(spec.Name); ok {
			picked = append(picked, tl)
		}
	}
	return tools.NewRegistry(picked...)
}

// finalReport returns the text of the last assistant message. When that
// message has no text it falls back to the latest one that does, and
// reports false.
func finalReport(history []llm.Message) (string, bool) {
	last := true
	for i := len(history) - 1; i >= 0; i-- {
		m := history[i]
		if m.Role != llm.RoleAssistant {
			continue
		}
		if txt := strings.TrimSpace(m.Text()); txt != "" {
			return txt, last
		}
		last = false
	}
	return "", false
}

// endsWithToolUse reports whether the last assistant message asked for tools.
func endsWithToolUse(history []llm.Message) bool {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == llm.RoleAssistant {
			return len(history[i].ToolUses()) > 0
		}
	}
	return false
}

func terminalEvent(r Result) string {
	switch {
	case r.Err == nil:
		return fmt.Sprintf("%s (%s, %s)", EventDonePrefix, plural(r.ToolCalls, "tool call"), fmtDuration(r.Duration))
	case errors.Is(r.Err, context.Canceled) || errors.Is(r.Err, context.DeadlineExceeded):
		return EventCancelled
	default:
		return EventFailedPrefix + ": " + oneLine(r.Err.Error(), 200)
	}
}

// progress forwards a progress event, serialized.
func (t *Tool) progress(idx int, desc, event string) {
	if t.cfg.Progress == nil {
		return
	}
	t.cbMu.Lock()
	defer t.cbMu.Unlock()
	t.cfg.Progress(idx, desc, event)
}

// recordUsage adds a child model call to the totals and forwards it.
func (t *Tool) recordUsage(model string, u llm.Usage) {
	t.usageMu.Lock()
	t.usage.Add(u)
	t.calls++
	t.usageMu.Unlock()
	if t.cfg.OnUsage == nil {
		return
	}
	t.cbMu.Lock()
	defer t.cbMu.Unlock()
	t.cfg.OnUsage(model, u)
}

// approve delegates a child's approval prompt to the parent, one at a time.
func (t *Tool) approve(ctx context.Context, idx int, desc string, call *tools.Call, reason string) permission.Decision {
	if t.cfg.Approve == nil {
		return permission.DecisionDeny
	}
	t.progress(idx, desc, EventApprovalPrefix+call.Summary)
	select {
	case t.approvals <- struct{}{}:
	case <-ctx.Done():
		return permission.DecisionDeny
	}
	defer func() { <-t.approvals }()
	if ctx.Err() != nil {
		return permission.DecisionDeny
	}
	who := fmt.Sprintf("sub-agent %d: %s", idx+1, desc)
	if reason != "" {
		who += " (" + reason + ")"
	}
	return t.cfg.Approve(ctx, call, who)
}

// childSink receives one child's activity. The agent may call it from
// several goroutines (read-only tools run concurrently).
type childSink struct {
	tool *Tool
	idx  int
	desc string

	mu        sync.Mutex
	usage     llm.Usage
	calls     int
	toolCalls int
	notices   []string
	declined  string
	lastAsked string
}

var _ agent.Sink = (*childSink)(nil)

func (s *childSink) Stream(llm.Event) {}

func (s *childSink) ToolStart(_ string, call *tools.Call) {
	s.mu.Lock()
	s.toolCalls++
	s.mu.Unlock()
	s.tool.progress(s.idx, s.desc, call.Summary)
}

func (s *childSink) ToolDone(string, *tools.Call, string, error) {}

func (s *childSink) ToolFailed(_, name, reason string) {
	s.mu.Lock()
	if reason == "declined" {
		s.declined = s.lastAsked
		if s.declined == "" {
			s.declined = name
		}
	}
	s.mu.Unlock()
}

func (s *childSink) Usage(model string, u llm.Usage) {
	s.mu.Lock()
	s.usage.Add(u)
	s.calls++
	s.mu.Unlock()
	s.tool.recordUsage(model, u)
}

func (s *childSink) Notice(msg string) {
	// The agent's turn-limit notice is addressed to a human ("say continue");
	// the turn limit is reported in the result's own words instead.
	if strings.HasPrefix(msg, "Stopped after ") {
		return
	}
	s.mu.Lock()
	s.notices = append(s.notices, msg)
	s.mu.Unlock()
}

func (s *childSink) Approve(ctx context.Context, call *tools.Call, reason string) permission.Decision {
	s.mu.Lock()
	s.lastAsked = call.Summary
	s.mu.Unlock()
	return s.tool.approve(ctx, s.idx, s.desc, call, reason)
}

func (s *childSink) stats() (llm.Usage, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage, s.calls, s.toolCalls
}

func (s *childSink) modelCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *childSink) declinedAction() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.declined
}

func (s *childSink) noticeList() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.notices...)
}
