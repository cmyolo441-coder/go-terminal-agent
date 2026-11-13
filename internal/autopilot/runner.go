package autopilot

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/agent"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

// Phase identifies a stage of a plan-then-execute run.
type Phase string

const (
	PhasePlan     Phase = "plan"
	PhaseReview   Phase = "review"
	PhaseApproval Phase = "approval"
	PhaseExecute  Phase = "execute"
	PhaseVerify   Phase = "verify"
	PhaseDone     Phase = "done"
)

// Phases lists the working phases in pipeline order.
var Phases = []Phase{PhasePlan, PhaseReview, PhaseApproval, PhaseExecute, PhaseVerify}

// Errors returned by Runner.
var (
	ErrBusy      = errors.New("autopilot: a run is already in progress")
	ErrEmptyTask = errors.New("autopilot: the task is empty")
)

// PlanDecision is the user's answer to a plan.
type PlanDecision int

const (
	// DecisionApprove executes the plan.
	DecisionApprove PlanDecision = iota + 1
	// DecisionReject stops without changing anything.
	DecisionReject
	// DecisionRevise sends the plan back to the planner with Feedback.
	DecisionRevise
)

func (d PlanDecision) String() string {
	switch d {
	case DecisionApprove:
		return "approve"
	case DecisionReject:
		return "reject"
	case DecisionRevise:
		return "revise"
	}
	return fmt.Sprintf("PlanDecision(%d)", int(d))
}

// Approval is the user's decision on a plan.
type Approval struct {
	Decision PlanDecision
	// Feedback is required for DecisionRevise and optional otherwise.
	Feedback string
}

// ApprovalRequest is what the user decides on.
type ApprovalRequest struct {
	Task string
	Plan *Plan
	// Review is the latest review, nil when review is skipped.
	Review *Review
	// Unresolved are the blocker and major issues of the latest review when
	// the reviewer did not approve within the round cap.
	Unresolved []Issue
	// Revisions counts plan revisions so far (reviewer and user driven).
	Revisions int
}

// Callbacks report progress. Every field is optional. Calls are
// serialized, never concurrent, and must not call back into the Runner.
type Callbacks struct {
	// Phase is called when a phase starts or restarts; detail is a short
	// label such as "round 2", or empty.
	Phase func(p Phase, detail string)
	// Activity reports a helper agent's tool call, e.g. "Read main.go".
	Activity func(p Phase, event string)
	// Plan reports every plan draft; revision is 0 for the first.
	Plan func(p *Plan, revision int)
	// Review reports each reviewer verdict; round counts from 1.
	Review func(round int, r *Review)
	// Steps reports step progress during execution.
	Steps func(steps []StepState)
	// Tests reports a test run made during verification.
	Tests func(run *TestRun)
	// Verified reports each verification report with the diff it judged.
	Verified func(v *VerifyReport, changes *ChangeSet)
	// Usage reports every helper agent model call (planner, reviewer,
	// verifier), which no agent bills. When nil, the Runner bills them to
	// the executor with agent.AddUsage. Executor calls are billed by the
	// executor itself and are not reported here.
	Usage func(p Phase, model string, u llm.Usage)
	// Notice reports conditions the user should know about.
	Notice func(msg string)
}

// Config wires a Runner into a session. Executor and Sink are required, and
// Approve unless AutoApprove is set; every other zero value selects a
// sensible default.
type Config struct {
	// Executor is the main agent that carries out the approved plan.
	Executor *agent.Agent
	// Sink is the UI sink executor turns stream to.
	Sink agent.Sink
	// Provider serves the helper agents; nil uses the executor's provider.
	Provider llm.Provider
	// Settings returns the base model settings for helpers; nil uses the
	// executor's settings.
	Settings func() agent.Settings
	// PlannerModel and ReviewerModel override the model of the planner and
	// of the reviewer and verifier.
	PlannerModel, ReviewerModel string
	// Tools is the registry helpers pick read-only tools from; nil uses the
	// executor's registry.
	Tools *tools.Registry
	// Instructions are project instructions (memory files) for helpers.
	Instructions string
	// Background is optional context from the conversation for the planner.
	Background string

	// Approve asks the user to approve the plan.
	Approve func(ctx context.Context, req ApprovalRequest) (Approval, error)
	// AutoApprove executes reviewer-approved plans without asking, for
	// headless use. A plan the reviewer did not approve is not executed.
	AutoApprove bool

	// SkipReview skips the reviewer.
	SkipReview bool
	// MaxReviewRounds caps reviewer rounds per approval cycle (default 2).
	MaxReviewRounds int
	// MaxRevisions caps user-requested revisions (default 3).
	MaxRevisions int
	// HelperMaxTurns is each helper's model-call limit per turn (default 30).
	HelperMaxTurns int
	// MaxExecRounds caps continuation nudges when steps are left unfinished
	// (default 2).
	MaxExecRounds int

	// SkipVerify skips the verification phase.
	SkipVerify bool
	// VerifyTests runs the test command during verification.
	VerifyTests bool
	// TestCommand overrides test command detection for VerifyTests.
	TestCommand *TestCommand
	// TestTimeout bounds the verification test run (default 10 minutes).
	TestTimeout time.Duration
	// GapFixRounds is how often verification gaps are sent back to the
	// executor before re-verifying (default 0: report only).
	GapFixRounds int
	// DiffBudget caps the diff shown to the verifier (default 60000 bytes).
	DiffBudget int
	// Snapshot bounds the workspace snapshot used to compute the diff.
	Snapshot SnapshotOptions

	Callbacks Callbacks

	// Setup, when set, adjusts each planner, reviewer and verifier agent
	// after it is built, for example to give it the executor's governance.
	Setup func(*agent.Agent)
}

// Runner runs plan-then-execute pipelines. It is safe for concurrent use,
// but runs one pipeline at a time: Run returns ErrBusy while one is active.
type Runner struct {
	cfg  Config
	busy atomic.Bool
	cbMu sync.Mutex
}

// New validates cfg and builds a Runner.
func New(cfg Config) (*Runner, error) {
	switch {
	case cfg.Executor == nil:
		return nil, errors.New("autopilot: Config.Executor is required")
	case cfg.Executor.Env == nil || cfg.Executor.Env.Root == "":
		return nil, errors.New("autopilot: the executor has no workspace environment")
	case cfg.Sink == nil:
		return nil, errors.New("autopilot: Config.Sink is required")
	case cfg.Approve == nil && !cfg.AutoApprove:
		return nil, errors.New("autopilot: Config.Approve is required unless AutoApprove is set")
	case cfg.MaxReviewRounds < 0, cfg.MaxRevisions < 0, cfg.HelperMaxTurns < 0, cfg.MaxExecRounds < 0, cfg.GapFixRounds < 0:
		return nil, errors.New("autopilot: limits must not be negative")
	}
	if cfg.Provider == nil {
		cfg.Provider = cfg.Executor.Provider
	}
	if cfg.Provider == nil {
		return nil, errors.New("autopilot: no model provider is configured")
	}
	if cfg.MaxReviewRounds == 0 {
		cfg.MaxReviewRounds = 2
	}
	if cfg.MaxRevisions == 0 {
		cfg.MaxRevisions = 3
	}
	if cfg.HelperMaxTurns == 0 {
		cfg.HelperMaxTurns = 30
	}
	if cfg.MaxExecRounds == 0 {
		cfg.MaxExecRounds = 2
	}
	if cfg.TestTimeout <= 0 {
		cfg.TestTimeout = DefaultTestTimeout
	}
	if cfg.DiffBudget <= 0 {
		cfg.DiffBudget = DefaultDiffBudget
	}
	return &Runner{cfg: cfg}, nil
}

// Outcome summarizes how a run ended.
type Outcome string

const (
	// OutcomeDone: executed and verified (or verification skipped with
	// every step completed).
	OutcomeDone Outcome = "done"
	// OutcomeGaps: executed, but verification found blocker or major gaps.
	OutcomeGaps Outcome = "gaps"
	// OutcomeIncomplete: execution ended with steps not completed.
	OutcomeIncomplete Outcome = "incomplete"
	// OutcomeRejected: the plan was not approved; nothing was executed.
	OutcomeRejected Outcome = "rejected"
	// OutcomeStopped: the user declined an action during execution.
	OutcomeStopped Outcome = "stopped"
	// OutcomeCancelled: the context was cancelled.
	OutcomeCancelled Outcome = "cancelled"
	// OutcomeFailed: an error stopped the run.
	OutcomeFailed Outcome = "failed"
)

// PhaseStats is the cost of one phase.
type PhaseStats struct {
	Usage      llm.Usage
	ModelCalls int
	ToolCalls  int
	Duration   time.Duration
}

// Result is everything a run produced. Run returns it even on error, with
// whatever was completed.
type Result struct {
	Task    string
	Outcome Outcome
	// Plan is the latest (for executed runs, the approved) plan.
	Plan *Plan
	// Revisions counts plan revisions; UserRevisions those the user asked for.
	Revisions     int
	UserRevisions int
	Reviews       []*Review
	// Unresolved are reviewer issues still open when the plan went to approval.
	Unresolved []Issue
	Approval   *Approval
	Steps      []StepState
	// Declined is the action the user declined, for OutcomeStopped.
	Declined     string
	Changes      *ChangeSet
	Tests        *TestRun
	Verification *VerifyReport
	GapFixRounds int
	Notes        []string
	Phases       map[Phase]*PhaseStats
	Usage        llm.Usage
	ModelCalls   int
	Duration     time.Duration
}

// Run executes the full pipeline for task: plan, review, approval, execute,
// verify. The error is non-nil when the run could not complete (a provider
// error, a plan that could not be parsed, cancellation); the Result is
// non-nil whenever the run started.
func (r *Runner) Run(ctx context.Context, task string) (*Result, error) {
	task = strings.TrimSpace(task)
	if task == "" {
		return nil, ErrEmptyTask
	}
	if !r.busy.CompareAndSwap(false, true) {
		return nil, ErrBusy
	}
	defer r.busy.Store(false)

	rn := &run{r: r, cfg: &r.cfg, task: task, root: r.cfg.Executor.Env.Root,
		res: &Result{Task: task, Phases: map[Phase]*PhaseStats{}}}
	start := time.Now()
	err := rn.pipeline(ctx)
	rn.closePhase()
	res := rn.res
	res.Duration = time.Since(start)
	for _, p := range Phases {
		if s := res.Phases[p]; s != nil {
			res.Usage.Add(s.Usage)
			res.ModelCalls += s.ModelCalls
		}
	}
	switch {
	case err == nil:
	case ctx.Err() != nil && (errors.Is(err, ctx.Err()) || errors.Is(err, context.Canceled)):
		res.Outcome = OutcomeCancelled
		err = ctx.Err()
	default:
		res.Outcome = OutcomeFailed
	}
	r.emit(func(c *Callbacks) {
		if c.Phase != nil {
			c.Phase(PhaseDone, string(res.Outcome))
		}
	})
	return res, err
}

// emit runs a callback under the callback lock.
func (r *Runner) emit(f func(c *Callbacks)) {
	r.cbMu.Lock()
	defer r.cbMu.Unlock()
	f(&r.cfg.Callbacks)
}

// run is the state of one pipeline.
type run struct {
	r    *Runner
	cfg  *Config
	task string
	root string
	res  *Result

	mu         sync.Mutex // guards res.Phases and res.Steps
	curPhase   Phase
	phaseStart time.Time

	planner  *helper
	reviewer *helper
	verifier *helper
	before   *Snapshot
	tracked  bool
}

func (rn *run) pipeline(ctx context.Context) error {
	if err := rn.planPhase(ctx); err != nil {
		return err
	}
	for {
		ap, err := rn.approvalPhase(ctx)
		if err != nil {
			return err
		}
		rn.res.Approval = &ap
		switch ap.Decision {
		case DecisionReject:
			rn.res.Outcome = OutcomeRejected
			return nil
		case DecisionRevise:
			if rn.res.UserRevisions >= rn.cfg.MaxRevisions {
				rn.note(fmt.Sprintf("The plan was sent back %d times, the limit; stopping without executing.", rn.res.UserRevisions))
				rn.res.Outcome = OutcomeRejected
				return nil
			}
			rn.res.UserRevisions++
			rn.setPhase(PhasePlan, fmt.Sprintf("revision %d (your feedback)", rn.res.Revisions+1))
			if err := rn.revise(ctx, reviseFromUserPrompt(ap.Feedback)); err != nil {
				return err
			}
			if err := rn.reviewLoop(ctx); err != nil {
				return err
			}
			continue
		}
		break
	}
	if err := rn.executePhase(ctx); err != nil {
		return err
	}
	if rn.res.Outcome == OutcomeStopped {
		return nil
	}
	if !rn.cfg.SkipVerify {
		if err := rn.verifyPhase(ctx); err != nil {
			return err
		}
	}
	if rn.res.Outcome == OutcomeStopped {
		return nil
	}
	rn.res.Outcome = rn.finalOutcome()
	return nil
}

func (rn *run) finalOutcome() Outcome {
	if v := rn.res.Verification; v != nil {
		if v.Passed() {
			return OutcomeDone
		}
		return OutcomeGaps
	}
	if rn.tracked && StepsDone(rn.res.Steps) < len(rn.res.Steps) {
		return OutcomeIncomplete
	}
	return OutcomeDone
}

// --- phases: plan and review ---

func (rn *run) helperSettings(model string) agent.Settings {
	var s agent.Settings
	if rn.cfg.Settings != nil {
		s = rn.cfg.Settings()
	} else {
		s = rn.cfg.Executor.Settings()
	}
	if model != "" {
		s.Model = model
	}
	s.MaxTurns = rn.cfg.HelperMaxTurns
	s.ShowThinking = false
	return s
}

func (rn *run) newHelper(phase Phase, intro, model string) (*helper, error) {
	reg := rn.cfg.Tools
	if reg == nil {
		reg = rn.cfg.Executor.Registry()
	}
	ro := readOnlyRegistry(reg)
	system := childSystem(intro, []any{strings.Join(ro.Names(), ", "), rn.cfg.HelperMaxTurns}, rn.root, rn.cfg.Instructions)
	if phase == PhasePlan {
		system = childSystem(intro, []any{strings.Join(ro.Names(), ", "), rn.cfg.HelperMaxTurns, planSchema}, rn.root, rn.cfg.Instructions)
	}
	return newHelper(helperConfig{
		provider: rn.cfg.Provider,
		settings: rn.helperSettings(model),
		registry: ro,
		baseEnv:  rn.cfg.Executor.Env,
		system:   system,
		phase:    phase,
		setup:    rn.cfg.Setup,
		onActivity: func(p Phase, ev string) {
			rn.r.emit(func(c *Callbacks) {
				if c.Activity != nil {
					c.Activity(p, ev)
				}
			})
		},
		onUsage:      rn.billHelper,
		onToolCall:   func(p Phase) { rn.stats(p, func(s *PhaseStats) { s.ToolCalls++ }) },
		onNoticeText: func(p Phase, msg string) { rn.notice(fmt.Sprintf("%s agent: %s", p, msg)) },
	})
}

func (rn *run) parsePlan(text string) (*Plan, error) { return ParsePlan(text, rn.root) }

func (rn *run) planPhase(ctx context.Context) error {
	rn.setPhase(PhasePlan, "")
	h, err := rn.newHelper(PhasePlan, plannerIntro, rn.cfg.PlannerModel)
	if err != nil {
		return err
	}
	rn.planner = h
	plan, err := askJSON(ctx, h, planPrompt(rn.task, rn.cfg.Background), "plan", rn.parsePlan)
	if err != nil {
		return err
	}
	rn.setPlan(plan)
	return rn.reviewLoop(ctx)
}

func (rn *run) setPlan(p *Plan) {
	rn.res.Plan = p
	rev := rn.res.Revisions
	rn.r.emit(func(c *Callbacks) {
		if c.Plan != nil {
			c.Plan(p, rev)
		}
	})
}

func (rn *run) revise(ctx context.Context, prompt string) error {
	plan, err := askJSON(ctx, rn.planner, prompt, "plan", rn.parsePlan)
	if err != nil {
		return err
	}
	rn.res.Revisions++
	rn.setPlan(plan)
	return nil
}

// reviewLoop reviews the current plan and has the planner revise it until
// the reviewer approves or the round cap is reached.
func (rn *run) reviewLoop(ctx context.Context) error {
	rn.res.Unresolved = nil
	if rn.cfg.SkipReview {
		return nil
	}
	for round := 1; ; round++ {
		rn.setPhase(PhaseReview, fmt.Sprintf("round %d", round))
		rv, err := rn.review(ctx)
		if err != nil {
			return err
		}
		rn.res.Reviews = append(rn.res.Reviews, rv)
		rn.r.emit(func(c *Callbacks) {
			if c.Review != nil {
				c.Review(round, rv)
			}
		})
		if rv.Approved() {
			return nil
		}
		if round >= rn.cfg.MaxReviewRounds {
			rn.res.Unresolved = rv.Blocking()
			rn.note(fmt.Sprintf("The reviewer still had %d unresolved issue%s after %d round%s.",
				len(rn.res.Unresolved), plural(len(rn.res.Unresolved)), round, plural(round)))
			return nil
		}
		rn.setPhase(PhasePlan, fmt.Sprintf("revision %d", rn.res.Revisions+1))
		if err := rn.revise(ctx, reviseFromReviewPrompt(rv)); err != nil {
			return err
		}
	}
}

func (rn *run) review(ctx context.Context) (*Review, error) {
	steps := len(rn.res.Plan.Steps)
	parse := func(text string) (*Review, error) { return ParseReview(text, steps) }
	if rn.reviewer == nil {
		h, err := rn.newHelper(PhaseReview, reviewerIntro, rn.cfg.ReviewerModel)
		if err != nil {
			return nil, err
		}
		rn.reviewer = h
		return askJSON(ctx, h, reviewPrompt(rn.task, rn.res.Plan), "review", parse)
	}
	return askJSON(ctx, rn.reviewer, rereviewPrompt(rn.res.Plan), "review", parse)
}

func (rn *run) approvalPhase(ctx context.Context) (Approval, error) {
	rn.setPhase(PhaseApproval, "")
	var last *Review
	if n := len(rn.res.Reviews); n > 0 {
		last = rn.res.Reviews[n-1]
	}
	if rn.cfg.AutoApprove {
		if last != nil && !last.Approved() {
			rn.note("Auto-approve did not execute the plan because the reviewer did not approve it.")
			return Approval{Decision: DecisionReject, Feedback: "the reviewer did not approve the plan"}, nil
		}
		return Approval{Decision: DecisionApprove}, nil
	}
	req := ApprovalRequest{
		Task:       rn.task,
		Plan:       rn.res.Plan,
		Review:     last,
		Unresolved: slices.Clone(rn.res.Unresolved),
		Revisions:  rn.res.Revisions,
	}
	ap, err := rn.cfg.Approve(ctx, req)
	if err != nil {
		if ctx.Err() != nil {
			return Approval{}, ctx.Err()
		}
		return Approval{}, fmt.Errorf("plan approval: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Approval{}, err
	}
	ap.Feedback = strings.TrimSpace(ap.Feedback)
	switch ap.Decision {
	case DecisionApprove, DecisionReject:
	case DecisionRevise:
		if ap.Feedback == "" {
			return Approval{}, errors.New("plan approval: a revision needs feedback for the planner")
		}
	default:
		return Approval{}, fmt.Errorf("plan approval: invalid decision %v", ap.Decision)
	}
	return ap, nil
}

// --- bookkeeping ---

// setPhase starts a phase, closing the previous one's timer.
func (rn *run) setPhase(p Phase, detail string) {
	rn.closePhase()
	rn.mu.Lock()
	rn.curPhase, rn.phaseStart = p, time.Now()
	rn.mu.Unlock()
	rn.r.emit(func(c *Callbacks) {
		if c.Phase != nil {
			c.Phase(p, detail)
		}
	})
}

func (rn *run) closePhase() {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	if rn.curPhase == "" {
		return
	}
	s := rn.statsLocked(rn.curPhase)
	s.Duration += time.Since(rn.phaseStart)
	rn.curPhase = ""
}

func (rn *run) statsLocked(p Phase) *PhaseStats {
	s := rn.res.Phases[p]
	if s == nil {
		s = &PhaseStats{}
		rn.res.Phases[p] = s
	}
	return s
}

func (rn *run) stats(p Phase, f func(*PhaseStats)) {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	f(rn.statsLocked(p))
}

func (rn *run) countUsage(p Phase, u llm.Usage) {
	rn.stats(p, func(s *PhaseStats) {
		s.Usage.Add(u)
		s.ModelCalls++
	})
}

// billHelper records a helper model call and bills it.
func (rn *run) billHelper(p Phase, model string, u llm.Usage) {
	rn.countUsage(p, u)
	reported := false
	rn.r.emit(func(c *Callbacks) {
		if c.Usage != nil {
			c.Usage(p, model, u)
			reported = true
		}
	})
	if !reported {
		rn.cfg.Executor.AddUsage(model, u)
	}
}

// note records a note in the result and reports it.
func (rn *run) note(msg string) {
	rn.mu.Lock()
	rn.res.Notes = append(rn.res.Notes, msg)
	rn.mu.Unlock()
	rn.notice(msg)
}

func (rn *run) notice(msg string) {
	rn.r.emit(func(c *Callbacks) {
		if c.Notice != nil {
			c.Notice(msg)
		}
	})
}

// Summary renders a human-readable report of the run.
func (res *Result) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Autopilot %s: %s\n", res.Outcome, clipRunes(firstLine(res.Task), 120))
	if p := res.Plan; p != nil {
		fmt.Fprintf(&b, "Plan: %d step%s, %d file%s", len(p.Steps), plural(len(p.Steps)), len(p.Files), plural(len(p.Files)))
		if res.Revisions > 0 {
			fmt.Fprintf(&b, ", revised %d time%s", res.Revisions, plural(res.Revisions))
		}
		if n := len(res.Reviews); n > 0 {
			verdict := "not approved by the reviewer"
			if res.Reviews[n-1].Approved() {
				verdict = "approved by the reviewer"
			}
			fmt.Fprintf(&b, ", %d review round%s, %s", n, plural(n), verdict)
		}
		b.WriteString("\n")
	}
	if len(res.Unresolved) > 0 {
		b.WriteString("Unresolved review issues:\n")
		for _, i := range res.Unresolved {
			b.WriteString("  - " + i.String() + "\n")
		}
	}
	if len(res.Steps) > 0 {
		fmt.Fprintf(&b, "Steps: %d/%d completed\n", StepsDone(res.Steps), len(res.Steps))
		for _, s := range res.Steps {
			mark := map[string]string{StatusCompleted: "✓", StatusInProgress: "…"}[s.Status]
			if mark == "" {
				mark = "○"
			}
			fmt.Fprintf(&b, "  %s %d. %s\n", mark, s.ID, s.Title)
		}
	}
	if res.Declined != "" {
		fmt.Fprintf(&b, "Stopped: you declined %q\n", res.Declined)
	}
	if res.Changes != nil {
		b.WriteString("Changes:\n")
		for _, l := range strings.Split(res.Changes.Stat(), "\n") {
			b.WriteString("  " + l + "\n")
		}
	}
	if res.Tests != nil {
		b.WriteString("Tests: " + res.Tests.Headline() + "\n")
	}
	if v := res.Verification; v != nil {
		status := "passed"
		if !v.Passed() {
			status = "found gaps"
		}
		fmt.Fprintf(&b, "Verification %s", status)
		if res.GapFixRounds > 0 {
			fmt.Fprintf(&b, " after %d gap-fix round%s", res.GapFixRounds, plural(res.GapFixRounds))
		}
		if v.Summary != "" {
			b.WriteString(": " + v.Summary)
		}
		b.WriteString("\n")
		for _, g := range v.Gaps {
			b.WriteString("  - " + g.String() + "\n")
		}
	}
	for _, n := range res.Notes {
		b.WriteString("Note: " + n + "\n")
	}
	fmt.Fprintf(&b, "Model calls: %d (%d in, %d out tokens); time %s",
		res.ModelCalls, res.Usage.InputTokens+res.Usage.CacheReadTokens+res.Usage.CacheWriteTokens, res.Usage.OutputTokens,
		res.Duration.Round(100*time.Millisecond))
	var parts []string
	for _, p := range Phases {
		if s := res.Phases[p]; s != nil && (s.ModelCalls > 0 || s.Duration >= time.Millisecond) {
			parts = append(parts, fmt.Sprintf("%s %d call%s/%s", p, s.ModelCalls, plural(s.ModelCalls), s.Duration.Round(100*time.Millisecond)))
		}
	}
	if len(parts) > 0 {
		b.WriteString(" [" + strings.Join(parts, ", ") + "]")
	}
	return b.String()
}
