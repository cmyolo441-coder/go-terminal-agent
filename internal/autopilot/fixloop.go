package autopilot

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/agent"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

// FixConfig configures FixTests. Agent and Sink are required.
type FixConfig struct {
	// Agent fixes the code; normally the session's main agent. It must not
	// be running another turn.
	Agent *agent.Agent
	// Sink is the UI sink the agent's turns stream to.
	Sink agent.Sink
	// Command overrides test command detection.
	Command *TestCommand
	// MaxIterations caps fix attempts (default 5).
	MaxIterations int
	// StallRounds stops the loop after this many consecutive fix attempts
	// that leave the failing set unchanged (default 2).
	StallRounds int
	// Timeout bounds each test run (default 10 minutes).
	Timeout time.Duration
	// FailureBudget caps the failure report sent to the agent in bytes
	// (default 6000).
	FailureBudget int
	Callbacks     FixCallbacks
}

// FixCallbacks report loop progress. Every field is optional.
type FixCallbacks struct {
	// TestStart is called before each test run; iteration 0 is the baseline.
	TestStart func(iteration int, cmd TestCommand)
	// TestDone reports each test run.
	TestDone func(iteration int, run *TestRun)
	// FixStart is called before each fix attempt with the failures shown.
	FixStart func(iteration int, failures []Failure)
	// Notice reports conditions the user should know about.
	Notice func(msg string)
}

// FixOutcome says why the loop stopped.
type FixOutcome string

const (
	// FixGreen: the suite passes (possibly without any fix attempt).
	FixGreen FixOutcome = "green"
	// FixMaxIterations: the iteration cap was reached with failures left.
	FixMaxIterations FixOutcome = "max-iterations"
	// FixNoProgress: the failing set stayed the same for StallRounds attempts.
	FixNoProgress FixOutcome = "no-progress"
	// FixStopped: the user declined an action the agent needed.
	FixStopped FixOutcome = "stopped"
	// FixCancelled: the context was cancelled.
	FixCancelled FixOutcome = "cancelled"
	// FixFailed: an error stopped the loop.
	FixFailed FixOutcome = "failed"
)

// FixRun summarizes one test run.
type FixRun struct {
	Iteration int
	ExitCode  int
	TimedOut  bool
	Failures  int
	Duration  time.Duration
}

// FixReport is the outcome of FixTests.
type FixReport struct {
	Command TestCommand
	Outcome FixOutcome
	// Iterations counts fix attempts by the agent.
	Iterations int
	// Initial are the failures of the baseline run.
	Initial []Failure
	// Fixed are failures seen at some point that no longer fail.
	Fixed []Failure
	// Remaining are the failures of the last run.
	Remaining []Failure
	// Introduced are remaining failures that were not failing at the start.
	Introduced []Failure
	Runs       []FixRun
	// Last is the final test run.
	Last *TestRun
	// Declined is the action the user declined, for FixStopped.
	Declined   string
	Notes      []string
	Usage      llm.Usage
	ModelCalls int
	// TestTime is the total time spent running tests.
	TestTime time.Duration
	Duration time.Duration
}

// Summary renders a human-readable report.
func (r *FixReport) Summary() string {
	var b strings.Builder
	switch r.Outcome {
	case FixGreen:
		if r.Iterations == 0 {
			b.WriteString("Tests already pass")
		} else {
			fmt.Fprintf(&b, "Tests pass after %d fix iteration%s", r.Iterations, plural(r.Iterations))
		}
	case FixMaxIterations:
		fmt.Fprintf(&b, "Stopped after %d fix iterations (the limit) with tests still failing", r.Iterations)
	case FixNoProgress:
		fmt.Fprintf(&b, "Stopped after %d fix iteration%s: no progress (the same tests kept failing)", r.Iterations, plural(r.Iterations))
	case FixStopped:
		fmt.Fprintf(&b, "Stopped after %d fix iteration%s because you declined %q", r.Iterations, plural(r.Iterations), r.Declined)
	case FixCancelled:
		fmt.Fprintf(&b, "Cancelled after %d fix iteration%s", r.Iterations, plural(r.Iterations))
	default:
		fmt.Fprintf(&b, "Failed after %d fix iteration%s", r.Iterations, plural(r.Iterations))
	}
	fmt.Fprintf(&b, " — `%s`\n", r.Command.String())
	fmt.Fprintf(&b, "Failing at start: %d · fixed: %d · still failing: %d", len(r.Initial), len(r.Fixed), len(r.Remaining))
	if len(r.Introduced) > 0 {
		fmt.Fprintf(&b, " · newly failing: %d", len(r.Introduced))
	}
	b.WriteString("\n")
	for _, f := range r.Fixed {
		b.WriteString("  ✓ " + f.Title() + "\n")
	}
	for _, f := range SortByRelevance(r.Remaining) {
		b.WriteString("  ✗ " + f.Title() + "\n")
	}
	for _, n := range r.Notes {
		b.WriteString("Note: " + n + "\n")
	}
	fmt.Fprintf(&b, "Runs: %d (%s in tests) · model calls: %d · time: %s",
		len(r.Runs), r.TestTime.Round(100*time.Millisecond), r.ModelCalls, r.Duration.Round(100*time.Millisecond))
	return b.String()
}

// FixTests runs the test suite and lets the agent fix failures until the
// suite is green, the iteration cap is hit, no progress is made for
// StallRounds attempts, the user declines an action, or ctx is cancelled.
// The report is non-nil whenever the baseline run started; the error is
// non-nil for setup problems, an unrunnable command, agent errors and
// cancellation.
func FixTests(ctx context.Context, cfg FixConfig) (*FixReport, error) {
	if cfg.Agent == nil || cfg.Agent.Env == nil || cfg.Agent.Env.Root == "" {
		return nil, errors.New("fix tests: an agent with a workspace environment is required")
	}
	if cfg.Sink == nil {
		return nil, errors.New("fix tests: a sink is required")
	}
	if cfg.MaxIterations < 0 || cfg.StallRounds < 0 {
		return nil, errors.New("fix tests: limits must not be negative")
	}
	if cfg.MaxIterations == 0 {
		cfg.MaxIterations = 5
	}
	if cfg.StallRounds == 0 {
		cfg.StallRounds = 2
	}
	if cfg.FailureBudget <= 0 {
		cfg.FailureBudget = DefaultFailureBudget
	}
	root := cfg.Agent.Env.Root
	var tc TestCommand
	if cfg.Command != nil && strings.TrimSpace(cfg.Command.Command) != "" {
		tc = *cfg.Command
	} else {
		c, err := DetectTestCommand(root)
		if err != nil {
			return nil, err
		}
		tc = c
	}
	if tc.Framework == "" {
		tc.Framework = GuessFramework(tc.Command, root)
	}
	l := &fixLoop{cfg: cfg, tc: tc, root: root, rep: &FixReport{Command: tc}, seen: map[string]Failure{}}
	start := time.Now()
	err := l.run(ctx)
	l.rep.Duration = time.Since(start)
	l.finish()
	switch {
	case err == nil:
	case ctx.Err() != nil:
		l.rep.Outcome = FixCancelled
		err = ctx.Err()
	default:
		l.rep.Outcome = FixFailed
	}
	return l.rep, err
}

type fixLoop struct {
	cfg  FixConfig
	tc   TestCommand
	root string
	rep  *FixReport
	// seen holds every failure observed, by key, first occurrence kept;
	// order lists the keys in order of first appearance.
	seen  map[string]Failure
	order []string
	// prevKeys is the failing set of the run before the latest one.
	prevKeys map[string]bool
}

func (l *fixLoop) run(ctx context.Context) error {
	cur, err := l.test(ctx, 0)
	if err != nil {
		return err
	}
	l.rep.Initial = cur.Failures
	if cur.Passed() {
		l.rep.Outcome = FixGreen
		return nil
	}
	stall := 0
	prev := cur
	for it := 1; ; it++ {
		if it > l.cfg.MaxIterations {
			l.rep.Outcome = FixMaxIterations
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		l.rep.Iterations = it
		fixed, introduced := 0, 0
		if it > 1 {
			fixed, introduced = diffKeys(l.prevKeys, prev.Keys())
		}
		if f := l.cfg.Callbacks.FixStart; f != nil {
			f(it, SortByRelevance(prev.Failures))
		}
		sink := &turnSink{inner: l.cfg.Sink, onUsage: func(_ string, u llm.Usage) {
			l.rep.Usage.Add(u)
			l.rep.ModelCalls++
		}}
		runErr := l.cfg.Agent.Run(ctx, llm.UserText(fixPrompt(prev, l.cfg.FailureBudget, it == 1, fixed, introduced)), sink)
		switch {
		case runErr == nil:
		case errors.Is(runErr, agent.ErrInterrupted) || ctx.Err() != nil:
			return ctxErr(ctx)
		default:
			return fmt.Errorf("fix tests: %w", runErr)
		}
		if d := sink.takeDeclined(); d != "" {
			l.rep.Declined = d
			l.rep.Outcome = FixStopped
			return nil
		}
		next, err := l.test(ctx, it)
		if err != nil {
			return err
		}
		if next.Passed() {
			l.rep.Outcome = FixGreen
			return nil
		}
		if maps.Equal(next.Keys(), prev.Keys()) {
			stall++
		} else {
			stall = 0
		}
		if stall >= l.cfg.StallRounds {
			l.rep.Outcome = FixNoProgress
			return nil
		}
		l.prevKeys = prev.Keys()
		prev = next
	}
}

// test runs the suite once and records it.
func (l *fixLoop) test(ctx context.Context, it int) (*TestRun, error) {
	if f := l.cfg.Callbacks.TestStart; f != nil {
		f(it, l.tc)
	}
	tr, err := RunTests(ctx, l.root, l.tc, RunOptions{Timeout: l.cfg.Timeout, Wrap: l.cfg.Agent.Env.Wrap, Check: l.cfg.Agent.Env.CheckCommand})
	if tr != nil {
		l.rep.TestTime += tr.Duration
	}
	if err != nil {
		return nil, err
	}
	l.rep.Runs = append(l.rep.Runs, FixRun{Iteration: it, ExitCode: tr.ExitCode, TimedOut: tr.TimedOut, Failures: len(tr.Failures), Duration: tr.Duration})
	l.rep.Last = tr
	for _, f := range tr.Failures {
		if _, ok := l.seen[f.Key()]; !ok {
			l.seen[f.Key()] = f
			l.order = append(l.order, f.Key())
		}
	}
	if f := l.cfg.Callbacks.TestDone; f != nil {
		f(it, tr)
	}
	if tr.TimedOut {
		l.notice(fmt.Sprintf("The test run timed out after %s.", tr.Duration.Round(time.Second)))
	}
	return tr, nil
}

func (l *fixLoop) notice(msg string) {
	l.rep.Notes = append(l.rep.Notes, msg)
	if f := l.cfg.Callbacks.Notice; f != nil {
		f(msg)
	}
}

// finish computes fixed, remaining and introduced failures.
func (l *fixLoop) finish() {
	var final map[string]bool
	if l.rep.Last != nil {
		final = l.rep.Last.Keys()
		if !l.rep.Last.Passed() {
			l.rep.Remaining = l.rep.Last.Failures
		}
	}
	initial := map[string]bool{}
	for _, f := range l.rep.Initial {
		initial[f.Key()] = true
	}
	for _, k := range l.order {
		if !final[k] {
			l.rep.Fixed = append(l.rep.Fixed, l.seen[k])
		}
	}
	for _, f := range l.rep.Remaining {
		if !initial[f.Key()] {
			l.rep.Introduced = append(l.rep.Introduced, f)
		}
	}
}

// diffKeys counts keys that disappeared from and appeared in the set.
func diffKeys(before, after map[string]bool) (fixed, introduced int) {
	for k := range before {
		if !after[k] {
			fixed++
		}
	}
	for k := range after {
		if !before[k] {
			introduced++
		}
	}
	return fixed, introduced
}
