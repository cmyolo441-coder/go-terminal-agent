// Package agent runs the model/tool loop: it streams a response, executes the
// tool calls it contains behind the permission system, and repeats until done.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/config"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/hooks"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permissions"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/session"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

// ErrInterrupted is returned when the user cancels a turn.
var ErrInterrupted = errors.New("interrupted")

// PermissionPrompt is shown to the user when a tool call needs confirmation.
type PermissionPrompt struct {
	Tool    string
	Summary string
	Detail  string // full command, or a preview of the edit
	Rule    string // what "always allow" would add; empty when not offered
	IsEdit  bool
}

// PermissionAnswer is the user's reply.
type PermissionAnswer struct {
	Allow     bool
	Always    bool
	Feedback  string
	Interrupt bool
}

// UI receives everything the agent wants to show or ask.
type UI interface {
	Status(label string) // start or relabel the activity indicator; "" hides it
	Text(delta string)
	Thinking(delta string)
	BlockEnd()
	ToolStart(id, name, summary string)
	ToolEnd(id, name, summary string, res tools.Result, dur time.Duration)
	ServerTool(name, summary string)
	Notice(msg string)
	AskPermission(p PermissionPrompt) PermissionAnswer
	// ApprovePlan shows a plan; it returns whether to proceed, the mode to
	// switch to and optional feedback for the model.
	ApprovePlan(plan string) (approved bool, mode permissions.Mode, feedback string)
}

// Agent is one conversation with the model.
type Agent struct {
	Client    anthropic.Client
	Model     string
	Effort    string
	MaxTokens int64
	System    string
	Tools     *tools.Registry
	Env       *tools.Env
	Perm      *permissions.Checker
	Hooks     *hooks.Runner
	UI        UI
	Session   *session.Session
	Usage     *Usage
	MaxTurns  int
	WebSearch bool
	Fallback  bool
	// AutoCompact summarizes the history when the context window fills up.
	AutoCompact bool
	Cwd         string

	Messages    []anthropic.BetaMessageParam
	LastContext int64 // input tokens of the most recent request
	depth       int   // 0 for the main agent, 1 for sub-agents
}

// Run sends a user turn and loops until the model stops calling tools.
// It returns the final assistant text.
func (a *Agent) Run(ctx context.Context, prompt string, extra ...anthropic.BetaContentBlockParamUnion) (string, error) {
	blocks := append([]anthropic.BetaContentBlockParamUnion{anthropic.NewBetaTextBlock(prompt)}, extra...)
	if a.Perm.Mode() == permissions.ModePlan {
		blocks = append(blocks, anthropic.NewBetaTextBlock(planReminder))
	}
	a.Session.SetTitle(prompt)
	a.push(anthropic.NewBetaUserMessage(blocks...))
	defer a.UI.Status("")

	for turn := 1; ; turn++ {
		if a.MaxTurns > 0 && turn > a.MaxTurns {
			return "", fmt.Errorf("stopped after reaching the limit of %d turns", a.MaxTurns)
		}
		if a.AutoCompact && a.LastContext > compactThreshold(a.Model) {
			a.UI.Notice("Context is nearly full; compacting the conversation…")
			if err := a.Compact(ctx, "Then continue the task from where it stopped."); err != nil {
				a.UI.Notice("Auto-compact failed: " + err.Error())
			}
		}
		a.UI.Status("Thinking…")
		msg, err := a.stream(ctx)
		a.UI.Status("")
		if err != nil {
			if ctx.Err() != nil {
				return "", ErrInterrupted
			}
			return "", describeError(err)
		}
		a.Usage.Add(a.Model, msg.Usage)
		a.LastContext = msg.Usage.InputTokens + msg.Usage.CacheReadInputTokens + msg.Usage.CacheCreationInputTokens
		if string(msg.Model) != "" && string(msg.Model) != a.Model && !strings.HasPrefix(string(msg.Model), a.Model) {
			a.UI.Notice(fmt.Sprintf("This response was served by %s instead of %s.", msg.Model, a.Model))
		}

		// Check the stop reason before trusting the content.
		switch msg.StopReason {
		case anthropic.BetaStopReasonRefusal:
			reason := "The model declined this request"
			if c := string(msg.StopDetails.Category); c != "" {
				reason += " (category: " + c + ")"
			}
			if msg.StopDetails.Explanation != "" {
				reason += ": " + msg.StopDetails.Explanation
			}
			return "", errors.New(reason + ". Rephrase the request or switch models with /model.")
		case anthropic.BetaStopReasonModelContextWindowExceeded:
			return "", errors.New("the conversation no longer fits in the model's context window; run /compact or /clear")
		}

		calls, valid := toolCalls(msg)
		if msg.StopReason == anthropic.BetaStopReasonMaxTokens && len(calls) > 0 || !valid {
			// The response was cut off inside a tool call, so its input is
			// incomplete. Leave it out of the history and ask for a retry.
			a.push(anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(
				"<system-reminder>Your previous response was cut off at the output limit in the middle of a tool call, so it was discarded. Continue, using smaller steps (for example, write large files in several Edit calls).</system-reminder>")))
			a.UI.Notice("Response hit the output limit mid tool call; asking the model to retry in smaller steps.")
			continue
		}
		if len(msg.Content) > 0 {
			a.push(msg.ToParam())
		}
		for _, b := range msg.Content {
			if b.Type == "server_tool_use" {
				a.UI.ServerTool(b.Name, serverSummary(b.Input))
			}
		}

		switch msg.StopReason {
		case anthropic.BetaStopReasonPauseTurn:
			continue // a server tool is still running; resend to resume
		case anthropic.BetaStopReasonToolUse:
			results, interrupted := a.runTools(ctx, calls)
			a.push(anthropic.NewBetaUserMessage(results...))
			if interrupted {
				return "", ErrInterrupted
			}
			continue
		case anthropic.BetaStopReasonMaxTokens:
			a.UI.Notice("Response reached the output limit. Send \"continue\" to let the model keep going.")
		}

		final := finalText(msg)
		if a.depth == 0 {
			if out := a.Hooks.Run(ctx, hooks.Stop, "", nil); out.Blocked {
				a.push(anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(
					"<system-reminder>A Stop hook asked you to keep working: " + out.Reason + "</system-reminder>")))
				continue
			}
		}
		return final, nil
	}
}

// push appends a message to the history and the transcript.
func (a *Agent) push(m anthropic.BetaMessageParam) {
	a.Messages = append(a.Messages, m)
	if a.Session != nil {
		if b, err := json.Marshal(m); err == nil {
			_ = a.Session.Append(b)
		}
	}
}

// LoadHistory restores messages saved by a previous session.
func (a *Agent) LoadHistory(raw []json.RawMessage) error {
	msgs := make([]anthropic.BetaMessageParam, 0, len(raw))
	for i, r := range raw {
		var m anthropic.BetaMessageParam
		if err := json.Unmarshal(r, &m); err != nil {
			return fmt.Errorf("message %d: %w", i+1, err)
		}
		msgs = append(msgs, m)
	}
	a.Messages = msgs
	return nil
}

// Clear drops the conversation.
func (a *Agent) Clear() {
	a.Messages = nil
	a.LastContext = 0
	if a.Session != nil {
		_ = a.Session.Reset(nil)
	}
}

func (a *Agent) params() anthropic.BetaMessageNewParams {
	p := anthropic.BetaMessageNewParams{
		Model:     anthropic.Model(a.Model),
		MaxTokens: a.MaxTokens,
		System: []anthropic.BetaTextBlockParam{{
			Text:         a.System,
			CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
		}},
		Messages: a.Messages,
		Tools:    a.toolParams(),
		// Caches the growing conversation prefix on each request.
		CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
	}
	caps := capsFor(a.Model)
	if caps.adaptive {
		p.Thinking = anthropic.BetaThinkingConfigParamUnion{OfAdaptive: &anthropic.BetaThinkingConfigAdaptiveParam{
			Display: anthropic.BetaThinkingConfigAdaptiveDisplaySummarized,
		}}
		if a.Effort != "" {
			p.OutputConfig = anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffort(a.Effort)}
		}
	}
	if a.Fallback && caps.fallback {
		// If a safety classifier declines a request, the API re-serves it on
		// another model instead of failing the turn.
		p.Betas = append(p.Betas, anthropic.AnthropicBetaServerSideFallback2026_07_01)
		p.Fallbacks = anthropic.BetaFallbacksParamOfDefault()
	}
	return p
}

func (a *Agent) toolParams() []anthropic.BetaToolUnionParam {
	var out []anthropic.BetaToolUnionParam
	for _, t := range a.Tools.List() {
		props := t.Properties
		if props == nil {
			props = map[string]any{}
		}
		out = append(out, anthropic.BetaToolUnionParam{OfTool: &anthropic.BetaToolParam{
			Name:        t.Name,
			Description: anthropic.String(t.Description),
			InputSchema: anthropic.BetaToolInputSchemaParam{Properties: props, Required: t.Required},
			// Stream large inputs (file contents) as they are generated. The
			// API then skips validation, so tools validate their own input.
			EagerInputStreaming: anthropic.Bool(true),
		}})
	}
	if a.WebSearch && capsFor(a.Model).webSearch {
		out = append(out, anthropic.BetaToolUnionParam{OfWebSearchTool20260209: &anthropic.BetaWebSearchTool20260209Param{}})
	}
	return out
}

// stream sends the request and forwards deltas to the UI as they arrive.
func (a *Agent) stream(ctx context.Context) (*anthropic.BetaMessage, error) {
	stream := a.Client.Beta.Messages.NewStreaming(ctx, a.params())
	defer stream.Close()
	var msg anthropic.BetaMessage
	for stream.Next() {
		ev := stream.Current()
		if err := msg.Accumulate(ev); err != nil {
			return nil, err
		}
		switch ev.Type {
		case "content_block_start":
			switch ev.ContentBlock.Type {
			case "tool_use":
				a.UI.Status("Preparing " + ev.ContentBlock.Name + "…")
			case "server_tool_use":
				a.UI.Status("Searching the web…")
			}
		case "content_block_delta":
			switch ev.Delta.Type {
			case "text_delta":
				a.UI.Text(ev.Delta.Text)
			case "thinking_delta":
				a.UI.Thinking(ev.Delta.Thinking)
			}
		case "content_block_stop":
			a.UI.BlockEnd()
		}
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	return &msg, nil
}

type toolCall struct {
	id, name string
	input    json.RawMessage
	tool     *tools.Tool
	result   *tools.Result // set when the call is resolved without running
	summary  string
	ran      bool
}

// toolCalls extracts tool_use blocks. valid is false when any input is not a
// complete JSON object, which happens when a streamed input was truncated.
func toolCalls(msg *anthropic.BetaMessage) (calls []*toolCall, valid bool) {
	valid = true
	for _, b := range msg.Content {
		if b.Type != "tool_use" {
			continue
		}
		in := b.Input
		if len(in) == 0 {
			in = json.RawMessage("{}")
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(in, &obj) != nil {
			valid = false
		}
		calls = append(calls, &toolCall{id: b.ID, name: b.Name, input: in})
	}
	return calls, valid
}

func finalText(msg *anthropic.BetaMessage) string {
	var parts []string
	for _, b := range msg.Content {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func serverSummary(in json.RawMessage) string {
	var m map[string]any
	_ = json.Unmarshal(in, &m)
	for _, k := range []string{"query", "url"} {
		if s, ok := m[k].(string); ok {
			return s
		}
	}
	return ""
}

// runTools resolves permissions for every call, executes them, and returns one
// tool_result block per call, in order.
func (a *Agent) runTools(ctx context.Context, calls []*toolCall) (results []anthropic.BetaContentBlockParamUnion, interrupted bool) {
	// Phase 1: validation, hooks and permissions, one call at a time so the
	// user is never asked two questions at once.
	for _, c := range calls {
		if interrupted || ctx.Err() != nil {
			interrupted = true
			r := tools.Errorf("Interrupted by the user before this tool ran.")
			c.result = &r
			continue
		}
		t, ok := a.Tools.Get(c.name)
		if !ok {
			r := tools.Errorf("Unknown tool %q.", c.name)
			c.result = &r
			continue
		}
		c.tool = t
		if t.Summary != nil {
			c.summary = t.Summary(c.input, a.Env)
		}
		var inputAny any
		_ = json.Unmarshal(c.input, &inputAny)
		if out := a.Hooks.Run(ctx, hooks.PreToolUse, c.name, map[string]any{"tool_input": inputAny}); out.Blocked {
			r := tools.Errorf("Blocked by a PreToolUse hook: %s", out.Reason)
			c.result = &r
			continue
		}
		req := permissions.Request{Tool: c.name, ReadOnly: t.ReadOnly, IsEdit: t.IsEdit}
		if t.Key != nil {
			req.Key = t.Key(c.input, a.Env)
		}
		decision, reason := a.Perm.Check(req)
		switch decision {
		case permissions.Deny:
			r := tools.Errorf("Permission denied: %s", reason)
			c.result = &r
		case permissions.Ask:
			a.UI.Status("")
			ans := a.UI.AskPermission(PermissionPrompt{
				Tool: c.name, Summary: c.summary, Detail: permissionDetail(c), IsEdit: t.IsEdit,
				Rule: permissions.SuggestRule(req),
			})
			switch {
			case ans.Interrupt:
				interrupted = true
				r := tools.Errorf("The user declined this tool call and interrupted the turn.")
				c.result = &r
			case !ans.Allow:
				text := "The user declined this tool call. Do not retry it unchanged."
				if ans.Feedback != "" {
					text += " They said: " + ans.Feedback
				}
				r := tools.Errorf("%s", text)
				c.result = &r
			case ans.Always:
				if t.IsEdit {
					if a.Perm.Mode() == permissions.ModeDefault {
						a.Perm.SetMode(permissions.ModeAcceptEdits)
					}
				} else {
					rule := permissions.SuggestRule(req)
					a.Perm.AddAllow(rule)
					if err := config.AddLocalAllowRule(a.Cwd, rule); err != nil {
						a.UI.Notice("Could not save the rule: " + err.Error())
					}
				}
			}
		}
	}

	// Phase 2: execution. A batch made only of read-only tools runs in parallel.
	var pending []*toolCall
	parallel := true
	for _, c := range calls {
		if c.result == nil {
			pending = append(pending, c)
			parallel = parallel && c.tool.ReadOnly
		}
	}
	run := func(c *toolCall) {
		start := time.Now()
		res := c.tool.Run(ctx, c.input, a.Env)
		if ctx.Err() != nil && !res.IsError {
			res.IsError = true
		}
		var inputAny any
		_ = json.Unmarshal(c.input, &inputAny)
		if out := a.Hooks.Run(ctx, hooks.PostToolUse, c.name, map[string]any{"tool_input": inputAny, "tool_response": res.Text}); out.Blocked {
			res.Text += "\n\n<system-reminder>PostToolUse hook feedback: " + out.Reason + "</system-reminder>"
		}
		c.result, c.ran = &res, true
		a.UI.ToolEnd(c.id, c.name, c.summary, res, time.Since(start))
	}
	if parallel && len(pending) > 1 {
		for _, c := range pending {
			a.UI.ToolStart(c.id, c.name, c.summary)
		}
		a.UI.Status(fmt.Sprintf("Running %d tools…", len(pending)))
		var wg sync.WaitGroup
		for _, c := range pending {
			wg.Add(1)
			go func(c *toolCall) { defer wg.Done(); run(c) }(c)
		}
		wg.Wait()
	} else {
		for _, c := range pending {
			if ctx.Err() != nil {
				r := tools.Errorf("Interrupted by the user before this tool ran.")
				c.result = &r
				continue
			}
			a.UI.ToolStart(c.id, c.name, c.summary)
			a.UI.Status("Running " + c.name + "…")
			run(c)
		}
	}
	a.UI.Status("")
	if ctx.Err() != nil {
		interrupted = true
	}

	for _, c := range calls {
		if !c.ran {
			// Resolved without running: still show the user what happened.
			a.UI.ToolStart(c.id, c.name, c.summary)
			a.UI.ToolEnd(c.id, c.name, c.summary, *c.result, 0)
		}
		results = append(results, resultBlock(c.id, *c.result))
	}
	return results, interrupted
}

func permissionDetail(c *toolCall) string {
	switch c.name {
	case "Bash":
		var in struct {
			Command     string `json:"command"`
			Description string `json:"description"`
		}
		_ = json.Unmarshal(c.input, &in)
		if in.Description != "" {
			return in.Command + "\n" + in.Description
		}
		return in.Command
	case "Edit":
		var in struct {
			Old string `json:"old_string"`
			New string `json:"new_string"`
		}
		_ = json.Unmarshal(c.input, &in)
		return tools.Diff(in.Old, in.New)
	case "Write":
		var in struct {
			Content string `json:"content"`
		}
		_ = json.Unmarshal(c.input, &in)
		return tools.Diff("", in.Content)
	}
	b, _ := json.MarshalIndent(json.RawMessage(c.input), "", "  ")
	return string(b)
}

func resultBlock(id string, r tools.Result) anthropic.BetaContentBlockParamUnion {
	text := r.Text
	if text == "" {
		text = "(no output)"
	}
	if len(r.Images) == 0 {
		return anthropic.NewBetaToolResultBlock(id, text, r.IsError)
	}
	content := []anthropic.BetaToolResultBlockParamContentUnion{{OfText: &anthropic.BetaTextBlockParam{Text: text}}}
	for _, img := range r.Images {
		content = append(content, anthropic.BetaToolResultBlockParamContentUnion{OfImage: &anthropic.BetaImageBlockParam{
			Source: anthropic.BetaImageBlockParamSourceUnion{OfBase64: &anthropic.BetaBase64ImageSourceParam{
				Data: img.Data, MediaType: anthropic.BetaBase64ImageSourceMediaType(img.MediaType),
			}},
		}})
	}
	return anthropic.BetaContentBlockParamUnion{OfToolResult: &anthropic.BetaToolResultBlockParam{
		ToolUseID: id, Content: content, IsError: anthropic.Bool(r.IsError),
	}}
}

// Compact replaces the history with a model-written summary.
func (a *Agent) Compact(ctx context.Context, instructions string) error {
	if len(a.Messages) == 0 {
		return errors.New("nothing to compact")
	}
	// Reuse the exact request prefix (system, tools, history) so the summary
	// request is served from the prompt cache.
	p := a.params()
	ask := compactPrompt
	if instructions != "" {
		ask += "\n\nAdditional instructions for the summary: " + instructions
	}
	p.Messages = append(append([]anthropic.BetaMessageParam{}, a.Messages...),
		anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(ask)))
	a.UI.Status("Compacting conversation…")
	defer a.UI.Status("")
	stream := a.Client.Beta.Messages.NewStreaming(ctx, p)
	defer stream.Close()
	var msg anthropic.BetaMessage
	for stream.Next() {
		if err := msg.Accumulate(stream.Current()); err != nil {
			return err
		}
	}
	if err := stream.Err(); err != nil {
		if ctx.Err() != nil {
			return ErrInterrupted
		}
		return describeError(err)
	}
	a.Usage.Add(a.Model, msg.Usage)
	summary := finalText(&msg)
	if summary == "" {
		return errors.New("the model returned an empty summary; the conversation was left unchanged")
	}
	first := anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(
		"This session continues an earlier conversation that was compacted to save context. Summary of that conversation:\n\n" + summary))
	a.Messages = []anthropic.BetaMessageParam{first}
	a.LastContext = msg.Usage.OutputTokens
	if a.Session != nil {
		if b, err := json.Marshal(first); err == nil {
			_ = a.Session.Reset([]json.RawMessage{b})
		}
	}
	return nil
}

// describeError turns API errors into something a user can act on.
func describeError(err error) error {
	var apierr *anthropic.Error
	if !errors.As(err, &apierr) {
		return fmt.Errorf("request failed: %w", err)
	}
	detail := apierr.Error()
	switch apierr.StatusCode {
	case 401:
		return fmt.Errorf("authentication failed (401). Set ANTHROPIC_API_KEY, or log in with `ant auth login`.\n%s", detail)
	case 403:
		return fmt.Errorf("permission denied by the API (403).\n%s", detail)
	case 404:
		return fmt.Errorf("not found (404): the model id may be wrong. Pick another with /model.\n%s", detail)
	case 413:
		return fmt.Errorf("request too large (413). Run /compact or /clear.\n%s", detail)
	case 429:
		return fmt.Errorf("rate limited (429). Wait a moment and try again.\n%s", detail)
	case 500, 502, 503, 529:
		return fmt.Errorf("the API is temporarily unavailable (%d). Try again shortly.\n%s", apierr.StatusCode, detail)
	}
	return fmt.Errorf("API error (%d): %s", apierr.StatusCode, detail)
}
