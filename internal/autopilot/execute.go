package autopilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/agent"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

// executePhase hands the approved plan to the executor and nudges it, a
// bounded number of times, while steps remain unfinished and it is still
// making progress.
func (rn *run) executePhase(ctx context.Context) error {
	rn.setPhase(PhaseExecute, "")
	snap, err := TakeSnapshot(ctx, rn.root, rn.cfg.Snapshot)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rn.note(fmt.Sprintf("The workspace snapshot failed (%v); the change set will be unavailable.", err))
	} else {
		rn.before = snap
		if snap.Incomplete {
			rn.note("The workspace is large; changes to files beyond the snapshot limit are not tracked.")
		}
	}
	rn.tracked = rn.seedTodos(ctx)
	rn.refreshSteps()

	prompt := executePrompt(rn.task, rn.res.Plan, rn.tracked)
	for round := 0; ; round++ {
		sink := rn.newTurnSink(PhaseExecute)
		if err := rn.runExecutor(ctx, prompt, sink); err != nil {
			return err
		}
		if d := sink.takeDeclined(); d != "" {
			rn.stopDeclined(d)
			return nil
		}
		if !rn.tracked {
			return nil
		}
		pending := pendingSteps(rn.refreshSteps())
		switch {
		case len(pending) == 0:
			return nil
		case sink.actionCount() == 0:
			rn.note(fmt.Sprintf("Execution stopped making progress with %d step%s unfinished.", len(pending), plural(len(pending))))
			return nil
		case round >= rn.cfg.MaxExecRounds:
			rn.note(fmt.Sprintf("%d step%s still unfinished after %d continuation round%s.", len(pending), plural(len(pending)), round, plural(round)))
			return nil
		}
		rn.setPhase(PhaseExecute, fmt.Sprintf("continuation %d", round+1))
		prompt = continuePrompt(pending)
	}
}

func (rn *run) newTurnSink(p Phase) *turnSink {
	return &turnSink{
		inner:   rn.cfg.Sink,
		onUsage: func(_ string, u llm.Usage) { rn.countUsage(p, u) },
		onTool:  func() { rn.stats(p, func(s *PhaseStats) { s.ToolCalls++ }) },
		onTodos: func() { rn.refreshSteps() },
	}
}

func (rn *run) runExecutor(ctx context.Context, prompt string, sink *turnSink) error {
	err := rn.cfg.Executor.Run(ctx, llm.UserText(prompt), sink)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, agent.ErrInterrupted) || ctx.Err() != nil:
		return ctxErr(ctx)
	default:
		return fmt.Errorf("execute: %w", err)
	}
}

func (rn *run) stopDeclined(action string) {
	rn.res.Declined = action
	rn.res.Outcome = OutcomeStopped
	rn.note(fmt.Sprintf("Execution stopped because you declined: %s", action))
}

// seedTodos replaces the executor's todo list with the plan's steps, so
// the user sees the plan and the executor tracks progress against it. It
// reports false when the executor has no todo tool.
func (rn *run) seedTodos(ctx context.Context) bool {
	tl, ok := rn.cfg.Executor.Registry().Get("todo_write")
	if !ok {
		rn.note("The executor has no todo_write tool, so step progress is not tracked.")
		return false
	}
	raw, err := json.Marshal(map[string]any{"todos": rn.res.Plan.Todos()})
	if err != nil {
		return false
	}
	call, err := tl.Prepare(rn.cfg.Executor.Env, raw)
	if err == nil {
		_, err = call.Run(ctx)
	}
	if err != nil {
		rn.note(fmt.Sprintf("Could not seed the todo list (%v); step progress is not tracked.", err))
		return false
	}
	return true
}

// refreshSteps recomputes step states from the executor's todo list and
// reports them when they changed.
func (rn *run) refreshSteps() []StepState {
	states := stepStates(rn.res.Plan, rn.cfg.Executor.Env.Todos())
	rn.mu.Lock()
	changed := !slices.Equal(states, rn.res.Steps)
	if changed {
		rn.res.Steps = states
	}
	rn.mu.Unlock()
	if changed {
		snapshot := slices.Clone(states)
		rn.r.emit(func(c *Callbacks) {
			if c.Steps != nil {
				c.Steps(snapshot)
			}
		})
	}
	return states
}

func pendingSteps(states []StepState) []StepState {
	var out []StepState
	for _, s := range states {
		if s.Status != StatusCompleted {
			out = append(out, s)
		}
	}
	return out
}

// verifyPhase diffs the workspace, optionally runs the tests, and has the
// verifier judge the result against the plan; gaps go back to the executor
// for up to GapFixRounds rounds.
func (rn *run) verifyPhase(ctx context.Context) error {
	rn.setPhase(PhaseVerify, "")
	cs, tests, err := rn.observe(ctx)
	if err != nil {
		return err
	}
	h, err := rn.newHelper(PhaseVerify, verifierIntro, rn.cfg.ReviewerModel)
	if err != nil {
		return err
	}
	rn.verifier = h
	steps := len(rn.res.Plan.Steps)
	parse := func(text string) (*VerifyReport, error) { return ParseVerifyReport(text, steps) }
	rep, err := askJSON(ctx, h, verifyPrompt(rn.task, rn.res.Plan, rn.currentSteps(), cs, tests, DefaultFailureBudget), "verification report", parse)
	if err != nil {
		return err
	}
	rn.setVerification(rep, cs, tests)

	for i := 0; i < rn.cfg.GapFixRounds && !rep.Passed(); i++ {
		rn.setPhase(PhaseExecute, fmt.Sprintf("gap fix %d", i+1))
		sink := rn.newTurnSink(PhaseExecute)
		if err := rn.runExecutor(ctx, gapFixPrompt(rep), sink); err != nil {
			return err
		}
		if d := sink.takeDeclined(); d != "" {
			rn.stopDeclined(d)
			return nil
		}
		rn.res.GapFixRounds++
		rn.setPhase(PhaseVerify, fmt.Sprintf("round %d", i+2))
		cs, tests, err = rn.observe(ctx)
		if err != nil {
			return err
		}
		rep, err = askJSON(ctx, h, reverifyPrompt(cs, tests, DefaultFailureBudget), "verification report", parse)
		if err != nil {
			return err
		}
		rn.setVerification(rep, cs, tests)
	}
	return nil
}

func (rn *run) currentSteps() []StepState {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	return slices.Clone(rn.res.Steps)
}

// observe computes the change set since execution began and, when
// configured, runs the tests.
func (rn *run) observe(ctx context.Context) (*ChangeSet, *TestRun, error) {
	var cs *ChangeSet
	if rn.before != nil {
		c, err := rn.before.Changes(ctx, rn.cfg.DiffBudget)
		switch {
		case err == nil:
			cs = c
		case ctx.Err() != nil:
			return nil, nil, ctx.Err()
		default:
			rn.note(fmt.Sprintf("Could not compute the changes (%v).", err))
		}
	}
	rn.res.Changes = cs
	tests, err := rn.runVerifyTests(ctx)
	if err != nil {
		return nil, nil, err
	}
	rn.res.Tests = tests
	return cs, tests, nil
}

func (rn *run) runVerifyTests(ctx context.Context) (*TestRun, error) {
	if !rn.cfg.VerifyTests {
		return nil, nil
	}
	var tc TestCommand
	if rn.cfg.TestCommand != nil {
		tc = *rn.cfg.TestCommand
	} else {
		c, err := DetectTestCommand(rn.root)
		if err != nil {
			rn.note("No test command was detected, so tests were not run during verification.")
			return nil, nil
		}
		tc = c
	}
	tr, err := RunTests(ctx, rn.root, tc, RunOptions{Timeout: rn.cfg.TestTimeout, Wrap: rn.cfg.Executor.Env.Wrap})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		rn.note(fmt.Sprintf("The tests could not be run during verification: %v", err))
		return nil, nil
	}
	rn.r.emit(func(c *Callbacks) {
		if c.Tests != nil {
			c.Tests(tr)
		}
	})
	return tr, nil
}

// setVerification records a report, enforcing that failing tests can never
// pass verification whatever the verifier said.
func (rn *run) setVerification(rep *VerifyReport, cs *ChangeSet, tests *TestRun) {
	if tests != nil && !tests.Passed() && rep.Passed() {
		rep.Verdict = VerdictGaps
		rep.Gaps = append([]Gap{{Severity: SeverityBlocker, Description: "the test suite is failing: " + tests.Headline()}}, rep.Gaps...)
		rn.note("The verifier passed the work, but the tests fail; verification is marked as having gaps.")
	}
	rn.res.Verification = rep
	rn.r.emit(func(c *Callbacks) {
		if c.Verified != nil {
			c.Verified(rep, cs)
		}
	})
}
