package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/agent"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permission"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

// HeadlessResult is the JSON document printed by headless --json runs.
type HeadlessResult struct {
	Result     string    `json:"result"`
	IsError    bool      `json:"is_error"`
	Error      string    `json:"error,omitempty"`
	SessionID  string    `json:"session_id,omitempty"`
	Model      string    `json:"model"`
	DurationMS int64     `json:"duration_ms"`
	Turns      int       `json:"turns"`
	Usage      llm.Usage `json:"usage"`
	CostUSD    float64   `json:"cost_usd"`
	ToolCalls  []string  `json:"tool_calls"`
}

// headlessSink streams text to stdout and activity to stderr. Approval
// prompts cannot be answered, so calls that need approval are declined
// with an explanation; use --mode auto-edit or yolo to allow them.
type headlessSink struct {
	mu      sync.Mutex
	out     io.Writer
	log     io.Writer
	json    bool
	text    strings.Builder
	last    strings.Builder
	turns   int
	calls   []string
	newline bool
}

func (s *headlessSink) Stream(ev llm.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ev.Type != llm.EventTextDelta {
		return
	}
	s.text.WriteString(ev.Text)
	s.last.WriteString(ev.Text)
	if !s.json {
		fmt.Fprint(s.out, ev.Text)
		s.newline = !strings.HasSuffix(ev.Text, "\n")
	}
}

func (s *headlessSink) ToolStart(id string, c *tools.Call) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, c.Tool)
	fmt.Fprintf(s.log, "● %s %s\n", displayName(c.Tool), toolArg(c))
}

func (s *headlessSink) ToolDone(id string, c *tools.Call, out string, err error) {
	if err != nil {
		fmt.Fprintf(s.log, "  ⎿ error: %s\n", oneLine(err.Error(), 200))
	}
}

func (s *headlessSink) ToolFailed(id, name, reason string) {
	fmt.Fprintf(s.log, "● %s failed: %s\n", displayName(name), oneLine(reason, 200))
}

func (s *headlessSink) Usage(string, llm.Usage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turns++
	// Each model call starts a new reply; keep only the final one as the result.
	if s.last.Len() > 0 {
		s.last.Reset()
	}
	if s.newline && !s.json {
		fmt.Fprintln(s.out)
		s.newline = false
	}
}

func (s *headlessSink) Notice(m string) { fmt.Fprintln(s.log, "! "+m) }

func (s *headlessSink) Approve(_ context.Context, c *tools.Call, _ string) permission.Decision {
	fmt.Fprintf(s.log, "! declined %q: approval needed but running non-interactively (use --mode auto-edit or --mode yolo)\n", c.Summary)
	return permission.DecisionDeny
}

// RunHeadless runs one prompt without the interactive UI.
func RunHeadless(ctx context.Context, ag *agent.Agent, promptText, sessionID string, asJSON bool, out, log io.Writer) error {
	start := time.Now()
	sink := &headlessSink{out: out, log: log, json: asJSON}
	body, attached := expandMentions(ag.Env, promptText)
	for _, a := range attached {
		fmt.Fprintf(log, "attached %s\n", a)
	}
	runErr := ag.Run(ctx, llm.UserText(body), sink)
	u, cost, _ := ag.Stats()
	if !asJSON {
		if sink.newline {
			fmt.Fprintln(out)
		}
		return runErr
	}
	final := ""
	h := ag.History()
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].Role == llm.RoleAssistant {
			final = h[i].Text()
			break
		}
	}
	res := HeadlessResult{
		Result: strings.TrimSpace(final), SessionID: sessionID, Model: ag.Settings().Model,
		DurationMS: time.Since(start).Milliseconds(), Turns: sink.turns, Usage: u, CostUSD: cost, ToolCalls: sink.calls,
	}
	if res.ToolCalls == nil {
		res.ToolCalls = []string{}
	}
	if runErr != nil {
		res.IsError = true
		res.Error = runErr.Error()
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(res); err != nil {
		return err
	}
	return runErr
}
