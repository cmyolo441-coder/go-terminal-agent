// Package agent runs the agentic loop: send the conversation to the model,
// stream the reply, run the tools it asks for (with permission checks), feed
// the results back, and repeat until the model is done.
package agent

import (
	"context"
	"errors"
	"fmt"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/hooks"
	"strings"
	"sync"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permission"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/pricing"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

// Sink receives everything the agent does, for display.
type Sink interface {
	// Stream receives streaming deltas from the model.
	Stream(ev llm.Event)
	// ToolStart is called once a tool call is validated, before it runs.
	ToolStart(id string, call *tools.Call)
	// ToolDone reports a finished tool call.
	ToolDone(id string, call *tools.Call, output string, err error)
	// ToolFailed reports a call that never ran (invalid input, denied, unknown tool).
	ToolFailed(id, name, reason string)
	// Usage reports token usage for one model call.
	Usage(model string, u llm.Usage)
	// Notice reports a condition the user should know about.
	Notice(msg string)
	// Approve asks the user whether a call may run.
	Approve(ctx context.Context, call *tools.Call, reason string) permission.Decision
}

// Settings are the per-session model settings.
type Settings struct {
	Model        string
	Effort       string
	MaxTokens    int64
	ShowThinking bool
	MaxTurns     int
}

// Agent holds one conversation.
type Agent struct {
	Provider llm.Provider
	Tools    *tools.Registry
	Env      *tools.Env
	Gate     *permission.Gate
	System   string
	// Hooks runs user-configured commands around prompts and tool calls.
	// Nil means no hooks.
	Hooks *hooks.Runner
	// BeforeWrite, when set, is called before any file-changing tool runs,
	// for example to checkpoint the files it is about to touch.
	BeforeWrite func(call *tools.Call)

	mu       sync.Mutex
	settings Settings
	history  []llm.Message
	usage    llm.Usage
	cost     float64
	context  int64
}

// New builds an agent.
func New(p llm.Provider, reg *tools.Registry, env *tools.Env, gate *permission.Gate, system string, s Settings) *Agent {
	if s.MaxTurns <= 0 {
		s.MaxTurns = 200
	}
	return &Agent{Provider: p, Tools: reg, Env: env, Gate: gate, System: system, settings: s}
}

// Settings returns the current settings.
func (a *Agent) Settings() Settings {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.settings
}

// Update changes settings for subsequent turns.
func (a *Agent) Update(f func(*Settings)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f(&a.settings)
}

// History returns a copy of the conversation.
func (a *Agent) History() []llm.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]llm.Message(nil), a.history...)
}

// SetHistory replaces the conversation (used to resume a session).
func (a *Agent) SetHistory(h []llm.Message, u llm.Usage, cost float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.history = append([]llm.Message(nil), h...)
	a.usage = u
	a.cost = cost
	a.context = 0
}

// Reset clears the conversation.
func (a *Agent) Reset() { a.SetHistory(nil, llm.Usage{}, 0) }

// Stats returns cumulative usage, estimated cost and the last context size.
func (a *Agent) Stats() (llm.Usage, float64, int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.usage, a.cost, a.context
}

func (a *Agent) appendUser(blocks []llm.Block) {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Two user turns in a row (after an interrupt or a declined tool) are
	// merged so the conversation always alternates roles.
	if n := len(a.history); n > 0 && a.history[n-1].Role == llm.RoleUser {
		a.history[n-1].Blocks = append(a.history[n-1].Blocks, blocks...)
		return
	}
	a.history = append(a.history, llm.Message{Role: llm.RoleUser, Blocks: blocks})
}

func (a *Agent) appendAssistant(m llm.Message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.history = append(a.history, m)
}

func (a *Agent) record(model string, u llm.Usage) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.usage.Add(u)
	if c, ok := pricing.Cost(model, u); ok {
		a.cost += c
	}
	a.context = u.ContextTokens() + u.OutputTokens
}

// ErrInterrupted is returned when the user cancels a turn.
var ErrInterrupted = errors.New("interrupted")

// Run sends a user message and drives the loop until the model finishes.
func (a *Agent) Run(ctx context.Context, user llm.Message, sink Sink) error {
	blocks := user.Blocks
	if a.Hooks.Has(hooks.UserPromptSubmit) {
		res, err := a.Hooks.UserPromptSubmit(ctx, user.Text())
		if err != nil {
			return ErrInterrupted
		}
		a.hookNotices(res, sink)
		if res.Blocked {
			return fmt.Errorf("prompt blocked by a user_prompt_submit hook: %s", orDefault(res.Reason, "no reason given"))
		}
		if c := res.ContextText(); c != "" {
			blocks = append(append([]llm.Block{}, blocks...), llm.Block{Type: llm.BlockText, Text: "<hook-context>\n" + c + "\n</hook-context>"})
		}
	}
	a.appendUser(blocks)
	s := a.Settings()
	stopRetries := 0
	for turn := 0; turn < s.MaxTurns; turn++ {
		s = a.Settings()
		req := llm.Request{
			Model:        s.Model,
			System:       a.System,
			Messages:     a.History(),
			Tools:        a.Tools.Specs(),
			MaxTokens:    s.MaxTokens,
			Effort:       s.Effort,
			ShowThinking: s.ShowThinking,
		}
		resp, err := a.Provider.Stream(ctx, req, sink.Stream)
		if err != nil {
			if ctx.Err() != nil {
				return ErrInterrupted
			}
			return err
		}
		model := resp.Model
		if model == "" {
			model = s.Model
		}
		a.record(model, resp.Usage)
		sink.Usage(model, resp.Usage)

		if resp.StopReason == llm.StopRefusal {
			// The refused reply is not kept, so the conversation can carry on.
			msg := "The model declined this request."
			if resp.StopDetail != "" {
				msg += " " + resp.StopDetail
			}
			sink.Notice(msg)
			return nil
		}
		a.appendAssistant(resp.Message)

		uses := resp.Message.ToolUses()
		switch {
		case resp.StopReason == llm.StopPauseTurn:
			continue
		case len(uses) == 0:
			if resp.StopReason == llm.StopMaxTokens {
				sink.Notice("The reply hit the max_tokens limit. Say \"continue\" to resume, or raise max_tokens.")
				return nil
			}
			if a.Hooks.Has(hooks.Stop) && stopRetries < maxStopRetries {
				res, err := a.Hooks.Stop(ctx)
				if err != nil {
					return ErrInterrupted
				}
				a.hookNotices(res, sink)
				if res.Blocked {
					stopRetries++
					reason := orDefault(res.Reason, "a stop hook asked you to keep working")
					sink.Notice("Stop hook: " + reason)
					a.appendUser([]llm.Block{{Type: llm.BlockText, Text: "<stop-hook>\n" + reason + "\n</stop-hook>"}})
					continue
				}
			}
			return nil
		case resp.StopReason == llm.StopMaxTokens:
			var results []llm.Block
			for _, u := range uses {
				reason := "Not run: the reply hit max_tokens before this call's input was complete. Retry in smaller steps."
				sink.ToolFailed(u.ToolUseID, u.ToolName, "input truncated at max_tokens")
				results = append(results, llm.Block{Type: llm.BlockToolResult, ToolUseID: u.ToolUseID, Text: reason, IsError: true})
			}
			a.appendUser(results)
			continue
		}

		results, stop := a.runTools(ctx, uses, sink)
		a.appendUser(results)
		if ctx.Err() != nil {
			return ErrInterrupted
		}
		if stop {
			return nil
		}
	}
	sink.Notice(fmt.Sprintf("Stopped after %d model calls (max_turns). Say \"continue\" to keep going.", s.MaxTurns))
	return nil
}

type prepared struct {
	use     llm.Block
	call    *tools.Call
	verdict permission.Verdict
	reason  string
	result  llm.Block
	done    bool
}

// runTools executes tool calls and returns their results in call order.
// stop is true when the user declined an action and the turn should end.
func (a *Agent) runTools(ctx context.Context, uses []llm.Block, sink Sink) ([]llm.Block, bool) {
	items := make([]*prepared, len(uses))
	for i, u := range uses {
		p := &prepared{use: u}
		items[i] = p
		fail := func(reason string) {
			sink.ToolFailed(u.ToolUseID, u.ToolName, reason)
			p.result = errorResult(u.ToolUseID, reason)
			p.done = true
		}
		tool, ok := a.Tools.Get(u.ToolName)
		if !ok {
			fail(fmt.Sprintf("unknown tool %q", u.ToolName))
			continue
		}
		call, err := tool.Prepare(a.Env, u.Input)
		if err != nil {
			fail(err.Error())
			continue
		}
		p.call = call
		hookApproved := false
		if a.Hooks.Has(hooks.PreToolUse) {
			res, err := a.Hooks.PreToolUse(ctx, u.ToolName, u.Input)
			if err != nil {
				fail("interrupted")
				continue
			}
			a.hookNotices(res, sink)
			if res.Blocked {
				fail("blocked by a pre_tool_use hook: " + orDefault(res.Reason, "no reason given"))
				continue
			}
			hookApproved = res.Approved
		}
		p.verdict, p.reason = a.Gate.Check(call)
		if p.verdict == permission.Deny {
			fail("not allowed: " + p.reason)
		} else if p.verdict == permission.Ask && hookApproved && !permission.IsDestructive(call.Command) {
			p.verdict = permission.Allow
		}
	}

	// Read-only calls run concurrently; they never need approval.
	var wg sync.WaitGroup
	for _, p := range items {
		if p.done || p.call.Kind != tools.KindRead || p.verdict != permission.Allow {
			continue
		}
		wg.Add(1)
		go func(p *prepared) {
			defer wg.Done()
			a.execute(ctx, p, sink)
		}(p)
	}
	wg.Wait()

	stop := false
	for _, p := range items {
		if p.done {
			continue
		}
		if ctx.Err() != nil || stop {
			reason := "Not run: the user interrupted."
			if stop {
				reason = "Not run: the user declined an earlier action in this batch."
			}
			p.result = errorResult(p.use.ToolUseID, reason)
			p.done = true
			continue
		}
		if p.verdict == permission.Ask {
			switch sink.Approve(ctx, p.call, p.reason) {
			case permission.DecisionAllowSession:
				a.Gate.Grant(p.call)
			case permission.DecisionAllowOnce:
			default:
				if ctx.Err() != nil {
					p.result = errorResult(p.use.ToolUseID, "Not run: the user interrupted.")
				} else {
					sink.ToolFailed(p.use.ToolUseID, p.use.ToolName, "declined")
					p.result = errorResult(p.use.ToolUseID, "The user declined this action. Do not retry it; wait for their instructions.")
					stop = true
				}
				p.done = true
				continue
			}
		}
		a.execute(ctx, p, sink)
	}

	out := make([]llm.Block, len(items))
	for i, p := range items {
		out[i] = p.result
	}
	return out, stop
}

func (a *Agent) execute(ctx context.Context, p *prepared, sink Sink) {
	if p.call.Kind == tools.KindWrite && a.BeforeWrite != nil {
		a.BeforeWrite(p.call)
	}
	sink.ToolStart(p.use.ToolUseID, p.call)
	output, err := p.call.Run(ctx)
	sink.ToolDone(p.use.ToolUseID, p.call, output, err)
	text := output
	if err != nil {
		if text != "" {
			text = strings.TrimRight(text, "\n") + "\n"
		}
		text += "Error: " + err.Error()
	}
	if a.Hooks.Has(hooks.PostToolUse) && ctx.Err() == nil {
		if res, herr := a.Hooks.PostToolUse(ctx, p.use.ToolName, p.use.Input, output, err != nil); herr == nil {
			a.hookNotices(res, sink)
			if res.Reason != "" {
				text = strings.TrimRight(text, "\n") + "\n\n<post-tool-hook>\n" + res.Reason + "\n</post-tool-hook>"
			}
		}
	}
	p.result = llm.Block{Type: llm.BlockToolResult, ToolUseID: p.use.ToolUseID, Text: text, IsError: err != nil}
	p.done = true
}

func errorResult(id, msg string) llm.Block {
	return llm.Block{Type: llm.BlockToolResult, ToolUseID: id, Text: msg, IsError: true}
}

// maxStopRetries caps how often a stop hook can send the model back to work
// in one turn, so a broken hook cannot loop forever.
const maxStopRetries = 3

// hookNotices shows hook warnings and feedback to the user.
func (a *Agent) hookNotices(res hooks.Result, sink Sink) {
	for _, w := range res.Warnings {
		sink.Notice("Hook warning: " + w)
	}
	for _, f := range res.Feedback {
		sink.Notice("Hook: " + strings.TrimSpace(f))
	}
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
