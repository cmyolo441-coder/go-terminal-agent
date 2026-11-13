package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/agent"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/autopilot"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/config"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permission"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/prompt"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/ui"
)

// phaseRows gives each autopilot phase a row in the live sub-agent view.
var phaseRows = map[autopilot.Phase]int{autopilot.PhasePlan: 0, autopilot.PhaseReview: 1, autopilot.PhaseVerify: 2}

// registerAutopilotCommands adds /plan and /fixtests.
func registerAutopilotCommands(ag *agent.Agent, cfg config.Config, root string) {
	ui.Register(
		ui.Extra{Name: "plan", Args: "<task>", Help: "Plan with a reviewer, approve, execute, then verify against the plan", Job: func(ctx context.Context, arg string, sink agent.Sink) (string, error) {
			return runPlan(ctx, ag, cfg, root, strings.TrimSpace(arg), sink)
		}},
		ui.Extra{Name: "fixtests", Args: "[max-iterations]", Help: "Run the tests and fix failures until they pass", Job: func(ctx context.Context, arg string, sink agent.Sink) (string, error) {
			return runFixTests(ctx, ag, strings.TrimSpace(arg), sink)
		}},
	)
}

func runPlan(ctx context.Context, ag *agent.Agent, cfg config.Config, root, task string, sink agent.Sink) (string, error) {
	if task == "" {
		return "", errors.New("usage: /plan <what you want done>")
	}
	progress, _ := sink.(agent.ProgressSink)
	r, err := autopilot.New(autopilot.Config{
		Executor:      ag,
		Sink:          sink,
		PlannerModel:  cfg.SubagentModel,
		ReviewerModel: cfg.SubagentModel,
		Instructions:  prompt.Memory(root),
		VerifyTests:   true,
		Setup:         func(a *agent.Agent) { a.Gov = ag.Gov.Child() },
		Approve: func(ctx context.Context, req autopilot.ApprovalRequest) (autopilot.Approval, error) {
			preview := req.Plan.Markdown()
			if len(req.Unresolved) > 0 {
				preview += "\n\n**Reviewer concerns not resolved**\n"
				for _, is := range req.Unresolved {
					preview += "- " + is.Description + "\n"
				}
			}
			call := tools.NewCall("plan", tools.KindRead, "Run "+oneLine(req.Plan.Goal, 70), preview, "", nil)
			switch sink.Approve(ctx, call, "") {
			case permission.DecisionAllowSession:
				if m, why := ag.Gate.Switch(permission.ModeAutoEdit); why != "" {
					sink.Notice(why)
				} else {
					sink.Notice("Mode set to " + string(m) + " for the plan's edits.")
				}
				return autopilot.Approval{Decision: autopilot.DecisionApprove}, nil
			case permission.DecisionAllowOnce:
				return autopilot.Approval{Decision: autopilot.DecisionApprove}, nil
			}
			if ctx.Err() != nil {
				return autopilot.Approval{}, ctx.Err()
			}
			return autopilot.Approval{Decision: autopilot.DecisionReject}, nil
		},
		Callbacks: autopilot.Callbacks{
			Phase: func(p autopilot.Phase, detail string) {
				label := map[autopilot.Phase]string{
					autopilot.PhasePlan:     "Planning",
					autopilot.PhaseReview:   "Reviewing the plan",
					autopilot.PhaseApproval: "Waiting for your approval",
					autopilot.PhaseExecute:  "Executing the plan",
					autopilot.PhaseVerify:   "Verifying the result",
					autopilot.PhaseDone:     "Done",
				}[p]
				if label == "" {
					label = string(p)
				}
				if detail != "" {
					label += " · " + detail
				}
				sink.Notice("Autopilot: " + label)
			},
			Activity: func(p autopilot.Phase, ev string) {
				if i, ok := phaseRows[p]; ok && progress != nil {
					progress.SubagentProgress(i, string(p), ev)
				}
			},
			Plan: func(p *autopilot.Plan, revision int) {
				title := "### Plan"
				if revision > 0 {
					title = fmt.Sprintf("### Plan (revision %d)", revision)
				}
				sink.Stream(llm.Event{Type: llm.EventTextDelta, Text: title + "\n\n" + p.Markdown() + "\n\n"})
			},
			Review: func(round int, rv *autopilot.Review) {
				msg := fmt.Sprintf("Reviewer (round %d): %s", round, rv.Verdict)
				if n := len(rv.Blocking()); n > 0 {
					msg += fmt.Sprintf(" · %d blocking issue(s)", n)
				}
				if rv.Summary != "" {
					msg += " · " + oneLine(rv.Summary, 120)
				}
				sink.Notice(msg)
			},
			Tests: func(run *autopilot.TestRun) { sink.Notice("Tests: " + run.Headline()) },
			Verified: func(v *autopilot.VerifyReport, ch *autopilot.ChangeSet) {
				msg := "Verifier: " + v.Verdict
				if !ch.Empty() {
					msg += " · " + ch.Stat()
				}
				sink.Notice(msg)
			},
			Notice: sink.Notice,
		},
	})
	if err != nil {
		return "", err
	}
	res, err := r.Run(ctx, task)
	if res == nil {
		return "", err
	}
	return summaryMarkdown(res.Summary()), err
}

func runFixTests(ctx context.Context, ag *agent.Agent, arg string, sink agent.Sink) (string, error) {
	n := 0
	if arg != "" {
		v, err := strconv.Atoi(arg)
		if err != nil || v < 1 || v > 50 {
			return "", errors.New("usage: /fixtests [max-iterations, 1-50]")
		}
		n = v
	}
	rep, err := autopilot.FixTests(ctx, autopilot.FixConfig{
		Agent:         ag,
		Sink:          sink,
		MaxIterations: n,
		Callbacks: autopilot.FixCallbacks{
			TestStart: func(i int, cmd autopilot.TestCommand) {
				sink.Notice(fmt.Sprintf("Fix loop %d: running %s", i, cmd.String()))
			},
			TestDone: func(i int, run *autopilot.TestRun) { sink.Notice(fmt.Sprintf("Fix loop %d: %s", i, run.Headline())) },
			FixStart: func(i int, f []autopilot.Failure) {
				sink.Notice(fmt.Sprintf("Fix loop %d: fixing %d failure(s)", i, len(f)))
			},
			Notice: sink.Notice,
		},
	})
	if rep == nil {
		return "", err
	}
	return summaryMarkdown(rep.Summary()), err
}

// summaryMarkdown turns an indented plain-text summary into Markdown:
// top-level lines become paragraphs and indented lines become bullets.
func summaryMarkdown(s string) string {
	var b strings.Builder
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if strings.HasPrefix(l, "  ") {
			item := strings.TrimPrefix(strings.TrimSpace(l), "- ")
			b.WriteString("- " + item + "\n")
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(l + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
