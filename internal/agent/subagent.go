package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/permissions"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

// RegisterAgentTools adds the tools that need the agent itself: Task and ExitPlanMode.
func (a *Agent) RegisterAgentTools() {
	a.Tools.Add(a.taskTool())
	a.Tools.Add(a.exitPlanTool())
}

func (a *Agent) taskTool() *tools.Tool {
	return &tools.Tool{
		Name: "Task",
		Description: `Launches a sub-agent to handle a self-contained task and returns its final report.

Use it for open-ended research across many files, or an independent multi-step sub-problem, when doing it yourself would flood your context. The sub-agent has the same tools (except Task) and permissions, starts with no knowledge of this conversation, and cannot ask you questions, so the prompt must be a complete brief: the goal, what you already know, and exactly what to report back. Several Task calls in one response run in parallel.`,
		Properties: map[string]any{
			"description": map[string]any{"type": "string", "description": "Short label for the task (3-5 words)"},
			"prompt":      map[string]any{"type": "string", "description": "The complete task brief for the sub-agent"},
		},
		Required: []string{"description", "prompt"},
		ReadOnly: true, // the sub-agent's own tool calls go through permission checks
		Summary: func(in json.RawMessage, _ *tools.Env) string {
			var v struct {
				Description string `json:"description"`
			}
			_ = json.Unmarshal(in, &v)
			return v.Description
		},
		Run: func(ctx context.Context, raw json.RawMessage, _ *tools.Env) tools.Result {
			var in struct {
				Description string `json:"description"`
				Prompt      string `json:"prompt"`
			}
			if err := json.Unmarshal(raw, &in); err != nil || in.Prompt == "" {
				return tools.Errorf("prompt is required")
			}
			sub := *a
			sub.Messages, sub.Session, sub.LastContext = nil, nil, 0
			sub.depth = a.depth + 1
			sub.Tools = a.Tools.Clone("Task", "ExitPlanMode")
			sub.UI = &subUI{parent: a.UI, label: in.Description}
			if sub.MaxTurns == 0 || sub.MaxTurns > 60 {
				sub.MaxTurns = 60
			}
			out, err := sub.Run(ctx, in.Prompt)
			if err != nil {
				return tools.Errorf("sub-agent failed: %v", err)
			}
			if out == "" {
				out = "(the sub-agent finished without a report)"
			}
			return tools.Result{Text: out, Display: "Done"}
		},
	}
}

func (a *Agent) exitPlanTool() *tools.Tool {
	return &tools.Tool{
		Name:        "ExitPlanMode",
		Description: `Presents your implementation plan to the user for approval. Use it only in plan mode, once your research is done and the plan is concrete. If the user approves, plan mode ends and you may start implementing.`,
		Properties: map[string]any{
			"plan": map[string]any{"type": "string", "description": "The plan, in concise markdown"},
		},
		Required: []string{"plan"},
		ReadOnly: true,
		Summary:  func(json.RawMessage, *tools.Env) string { return "" },
		Run: func(_ context.Context, raw json.RawMessage, _ *tools.Env) tools.Result {
			var in struct {
				Plan string `json:"plan"`
			}
			if err := json.Unmarshal(raw, &in); err != nil || in.Plan == "" {
				return tools.Errorf("plan is required")
			}
			if a.Perm.Mode() != permissions.ModePlan {
				return tools.Result{Text: "Plan mode is not active, so no approval is needed. Proceed with the work."}
			}
			a.UI.Status("")
			ok, mode, feedback := a.UI.ApprovePlan(in.Plan)
			if !ok {
				msg := "The user did not approve the plan. Stay in plan mode and revise it."
				if feedback != "" {
					msg += " Their feedback: " + feedback
				}
				return tools.Errorf("%s", msg)
			}
			a.Perm.SetMode(mode)
			return tools.Result{Text: "The user approved the plan. Plan mode is off; start implementing it now.", Display: "Plan approved"}
		},
	}
}

// subUI shows a sub-agent's activity as compact nested lines and forwards
// questions to the parent UI.
type subUI struct {
	parent UI
	label  string
}

func (s *subUI) Status(string)   {}
func (s *subUI) Text(string)     {}
func (s *subUI) Thinking(string) {}
func (s *subUI) BlockEnd()       {}
func (s *subUI) ToolStart(_, name, summary string) {
	s.parent.Notice("  ↳ [" + s.label + "] " + name + "(" + oneLine(summary, 70) + ")")
}
func (s *subUI) ToolEnd(string, string, string, tools.Result, time.Duration) {}
func (s *subUI) ServerTool(name, summary string) {
	s.parent.Notice("  ↳ [" + s.label + "] " + name + "(" + oneLine(summary, 70) + ")")
}
func (s *subUI) Notice(msg string) { s.parent.Notice("  ↳ [" + s.label + "] " + msg) }
func (s *subUI) AskPermission(p PermissionPrompt) PermissionAnswer {
	p.Summary = "[sub-agent: " + s.label + "] " + p.Summary
	return s.parent.AskPermission(p)
}
func (s *subUI) ApprovePlan(string) (bool, permissions.Mode, string) {
	return false, permissions.ModePlan, "sub-agents cannot leave plan mode"
}

func oneLine(s string, n int) string {
	for i, r := range s {
		if r == '\n' {
			s = s[:i] + "…"
			break
		}
	}
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
