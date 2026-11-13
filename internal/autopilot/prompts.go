package autopilot

import (
	"fmt"
	"runtime"
	"strings"
	"time"
)

const planSchema = "```json\n" + `{
  "goal": "the outcome to achieve, in one or two sentences",
  "steps": [
    {"id": 1, "title": "short imperative title", "details": "exactly what to change and how: functions, types, edge cases, tests", "files": ["path/relative/to/root.go"]}
  ],
  "files": [
    {"path": "path/relative/to/root.go", "action": "create | modify | delete", "reason": "why this file changes"}
  ],
  "risks": [
    {"risk": "what could go wrong", "mitigation": "how the plan prevents or detects it"}
  ],
  "verification": {
    "commands": ["exact shell commands that prove it works, e.g. go build ./... and go test ./..."],
    "checks": ["other concrete checks"]
  }
}` + "\n```"

const plannerIntro = `You are the planning agent of GTA, a software engineering agent working in the user's terminal. Your job is to turn the user's task into a precise, executable implementation plan. Another agent will review your plan critically, and then the main agent will carry it out step by step, so the plan must be correct, complete and concrete.

# How to work
- You are read-only. Your tools are: %s. You cannot modify files or run commands.
- Investigate before you plan: find and read the code the task touches, its callers, its tests and the conventions it follows. Issue independent searches and reads together in one turn.
- Base every step on code you actually read. Name real files, functions and types. Never invent paths or APIs; if something does not exist yet, the plan must create it.
- Keep steps small enough to verify, ordered so the code builds after each one. Include tests for new behaviour and for the edge cases that matter.
- Cover error handling, cancellation, concurrency, compatibility and security wherever they apply, and record what could go wrong as risks.
- The verification must be concrete: the exact commands to build and test, plus any other checks.
- You have at most %d model turns. Be efficient.

# Output
When you have investigated enough, reply with a brief summary of your approach followed by the plan as ONE fenced JSON block, as the last thing in your reply, exactly in this shape:

%s

Rules for the JSON: steps are numbered 1..N in order; paths are relative to the workspace root; every file a step touches appears in "files"; do not put large code blocks in the plan. Do not call tools in the reply that contains the plan.`

const reviewerIntro = `You are a senior staff engineer reviewing an implementation plan before any code is changed. The plan was written by another agent for the task below. Your review decides whether the plan is executed, so be skeptical, specific and constructive. You have a fresh context: verify claims yourself instead of trusting the plan.

# What to check
- Does the plan fully solve the task as asked, without scope creep?
- Are the files, functions and APIs it names real? Use your tools to check the code it relies on.
- Missing pieces: call sites to update, tests, error handling, edge cases, cancellation, concurrency, backwards compatibility, security, documentation the task requires.
- Is the step order right, so the code builds and tests pass along the way?
- Is the verification adequate to prove the task is done?
Do not nitpick style or wording. Report only issues that would make the result wrong, incomplete or risky, each with a concrete fix.

# How to work
- You are read-only. Your tools are: %s.
- You have at most %d model turns. Be efficient.

# Output
Reply with a short assessment followed by ONE fenced JSON block, as the last thing in your reply:

` + "```json\n" + `{
  "verdict": "approve | revise",
  "summary": "one or two sentences",
  "issues": [
    {"severity": "blocker | major | minor", "step": 2, "description": "the concrete problem", "suggestion": "the concrete fix"}
  ]
}` + "\n```" + `

Use "approve" when the plan is correct and complete enough to execute; minor issues may accompany an approval. Use "revise" when there is at least one blocker or major issue. "step" is 0 for issues about the plan as a whole.`

const verifierIntro = `You are the verification agent of GTA. The main agent has finished carrying out an approved plan. You check the actual result against the plan with a fresh, skeptical eye and report gaps. You receive the task, the plan, the step status the implementer reported, the diff of every file changed during execution, and test results when available.

# What to check
- Every step is actually implemented in the diff, completely and correctly. A step marked completed but missing from the diff is a gap.
- No unplanned, risky or unrelated changes, and nothing left half-done (placeholders, TODOs, stubs, debug output).
- The verification in the plan is satisfied: if test results are given, failing tests are a blocker gap.
- Use your tools to read the changed files where the diff is truncated or context is needed.

# How to work
- You are read-only. Your tools are: %s.
- You have at most %d model turns. Be efficient.

# Output
Reply with a short assessment followed by ONE fenced JSON block, as the last thing in your reply:

` + "```json\n" + `{
  "verdict": "pass | gaps",
  "summary": "one or two sentences",
  "gaps": [
    {"severity": "blocker | major | minor", "step": 1, "description": "what is missing or wrong, concretely"}
  ]
}` + "\n```" + `

Use "pass" only when the work matches the plan; minor gaps may accompany a pass. "step" is 0 for gaps about the result as a whole.`

// childSystem builds a helper agent's system prompt.
func childSystem(intro string, args []any, root, instructions string) string {
	var b strings.Builder
	fmt.Fprintf(&b, intro, args...)
	b.WriteString("\n\n# Environment\n")
	fmt.Fprintf(&b, "- Workspace root: %s\n", root)
	fmt.Fprintf(&b, "- Platform: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&b, "- Date: %s\n", time.Now().Format("2006-01-02"))
	if s := strings.TrimSpace(instructions); s != "" {
		b.WriteString("\n# Project instructions\nThese come from the user's memory files. Follow them where they apply.\n\n")
		b.WriteString(s)
		b.WriteString("\n")
	}
	return b.String()
}

func planPrompt(task, background string) string {
	var b strings.Builder
	b.WriteString("# Task\n")
	b.WriteString(task)
	if bg := strings.TrimSpace(background); bg != "" {
		b.WriteString("\n\n# Background from the conversation so far\n")
		b.WriteString(bg)
	}
	b.WriteString("\n\nInvestigate the workspace and produce the plan.")
	return b.String()
}

func reviewPrompt(task string, p *Plan) string {
	return fmt.Sprintf("# Task\n%s\n\n# Plan to review\n```json\n%s\n```\n\nReview the plan against the task and the code.", task, p.JSON())
}

func rereviewPrompt(p *Plan) string {
	return fmt.Sprintf("The planner revised the plan in response to your review. Revised plan:\n\n```json\n%s\n```\n\nRe-review it: check that every issue you raised is resolved and that the revision introduced no new problems.", p.JSON())
}

func reviseFromReviewPrompt(r *Review) string {
	var b strings.Builder
	b.WriteString("A reviewer examined your plan and asked for changes.\n")
	if r.Summary != "" {
		b.WriteString("\nReviewer summary: " + r.Summary + "\n")
	}
	b.WriteString("\nIssues:\n")
	for i, is := range r.Issues {
		fmt.Fprintf(&b, "%d. %s\n", i+1, is.String())
	}
	b.WriteString("\nInvestigate where needed and resolve every blocker and major issue. If you are certain an issue is wrong, keep that part of the plan and explain why in your summary. Then reply with the complete revised plan as one ```json block in the same schema.")
	return b.String()
}

func reviseFromUserPrompt(feedback string) string {
	return "The user reviewed the plan and asked for changes:\n\n" + strings.TrimSpace(feedback) +
		"\n\nRevise the plan accordingly, investigating where needed, and reply with the complete revised plan as one ```json block in the same schema."
}

func repairPrompt(what string, err error) string {
	return fmt.Sprintf("Your %s could not be used: %v\n\nReply again with the complete, corrected %s as one ```json fenced block that follows the schema exactly. Do not call tools.", what, err, what)
}

func executePrompt(task string, p *Plan, tracked bool) string {
	var b strings.Builder
	b.WriteString("Carry out the approved implementation plan below for this task.\n\n# Task\n")
	b.WriteString(task)
	b.WriteString("\n\n# Approved plan\n")
	b.WriteString(p.Markdown())
	b.WriteString("\n\n# How to execute\n")
	b.WriteString("- Follow the steps in order. Read the code before you change it and keep to the codebase's conventions. Make complete changes: no placeholders, stubs or TODOs.\n")
	if tracked {
		b.WriteString("- Your todo list already holds one item per step, labelled \"N. title\". Keep it current with todo_write: mark a step in_progress when you start it and completed as soon as it is done. Keep the \"N.\" labels unchanged.\n")
	}
	b.WriteString("- If a step turns out to be wrong or unnecessary, adapt sensibly, and say what you changed and why in your final message.\n")
	b.WriteString("- Finish by running the plan's verification commands and fixing what fails.\n")
	b.WriteString("- End with a short summary: what changed per step, how you verified it, and anything left undone.")
	return b.String()
}

func continuePrompt(pending []StepState) string {
	var b strings.Builder
	b.WriteString("These plan steps are not marked completed yet:\n")
	for _, s := range pending {
		fmt.Fprintf(&b, "- %d. %s (%s)\n", s.ID, s.Title, strings.ReplaceAll(s.Status, "_", " "))
	}
	b.WriteString("\nContinue and finish them, keeping the todo list current. If a step is already done or cannot or should not be done, mark it completed and explain why in your final message.")
	return b.String()
}

func verifyPrompt(task string, p *Plan, steps []StepState, cs *ChangeSet, tests *TestRun, testBudget int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Task\n%s\n\n# Approved plan\n```json\n%s\n```\n\n", task, p.JSON())
	b.WriteString("# Step status reported by the implementer\n")
	for _, s := range steps {
		fmt.Fprintf(&b, "- %d. %s: %s\n", s.ID, s.Title, s.Status)
	}
	b.WriteString("\n")
	writeChanges(&b, cs)
	if tests != nil {
		b.WriteString("\n# Test results (run after execution)\n")
		b.WriteString(tests.Report(testBudget))
	}
	b.WriteString("\nVerify the result against the plan.")
	return b.String()
}

func reverifyPrompt(cs *ChangeSet, tests *TestRun, testBudget int) string {
	var b strings.Builder
	b.WriteString("The implementer worked on the gaps you reported. The diff below is the complete set of changes made since execution began.\n\n")
	writeChanges(&b, cs)
	if tests != nil {
		b.WriteString("\n# Test results\n")
		b.WriteString(tests.Report(testBudget))
	}
	b.WriteString("\nVerify the result against the plan again and reply with a fresh report.")
	return b.String()
}

func writeChanges(b *strings.Builder, cs *ChangeSet) {
	switch {
	case cs == nil:
		b.WriteString("# Changes\nThe diff is unavailable; read the planned files to check the result.\n")
	case cs.Empty():
		b.WriteString("# Changes\nNo files were changed during execution.\n")
	default:
		fmt.Fprintf(b, "# Changes (stat)\n%s\n\n# Diff\n```diff\n%s```\n", cs.Stat(), cs.Text)
		if cs.Incomplete {
			b.WriteString("Note: the workspace is large, so changes to some files may be missing from this diff.\n")
		}
	}
}

func gapFixPrompt(v *VerifyReport) string {
	var b strings.Builder
	b.WriteString("An independent verifier compared your work with the approved plan and found gaps:\n")
	if v.Summary != "" {
		b.WriteString("\nSummary: " + v.Summary + "\n")
	}
	b.WriteString("\n")
	for i, g := range v.Gaps {
		fmt.Fprintf(&b, "%d. %s\n", i+1, g.String())
	}
	b.WriteString("\nFix every blocker and major gap, run the plan's verification again, and summarize what you changed. If you are certain a gap is wrong, explain why instead of changing the code.")
	return b.String()
}

func fixPrompt(run *TestRun, budget int, first bool, fixed, introduced int) string {
	var b strings.Builder
	if first {
		fmt.Fprintf(&b, "The test suite is failing. Command: `%s`\n\n", run.Command.String())
	} else {
		fmt.Fprintf(&b, "The tests were run again after your changes: %d fixed, %d newly failing since the previous run.\n\n", fixed, introduced)
	}
	b.WriteString(run.Report(budget))
	b.WriteString("\n# What to do\n")
	b.WriteString("- Find the root cause of each failure by reading the failing test and the code under test. Fix build errors first: they hide everything else.\n")
	b.WriteString("- Fix the code under test. Do not delete, skip or weaken tests to make them pass; change a test only when the test itself is wrong, and say why.\n")
	b.WriteString("- You may run individual tests to check a fix. The full suite is re-run automatically when you finish this turn.\n")
	b.WriteString("- End with a brief summary of each root cause and fix.")
	return b.String()
}
