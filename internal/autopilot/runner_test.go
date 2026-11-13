package autopilot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permission"
)

// planDoc builds a plan reply with the given step titles.
func planDoc(goal string, titles ...string) string {
	var steps []map[string]any
	for i, t := range titles {
		steps = append(steps, map[string]any{"id": i + 1, "title": t, "details": "do " + t, "files": []string{"hello.txt"}})
	}
	return fenced(map[string]any{
		"goal":         goal,
		"steps":        steps,
		"files":        []map[string]string{{"path": "hello.txt", "action": "create", "reason": "the greeting"}},
		"risks":        []map[string]string{{"risk": "clobbering an existing file", "mitigation": "it does not exist yet"}},
		"verification": map[string]any{"commands": []string{"cat hello.txt"}},
	})
}

func approve() step {
	return say(fenced(map[string]any{"verdict": "approve", "summary": "looks right", "issues": []any{}}))
}

func revise(desc string) step {
	return say(fenced(map[string]any{"verdict": "revise", "summary": "needs work",
		"issues": []map[string]any{{"severity": "blocker", "step": 1, "description": desc, "suggestion": "fix it"}}}))
}

func pass() step {
	return say(fenced(map[string]any{"verdict": "pass", "summary": "all steps implemented"}))
}

func gaps(desc string) step {
	return say(fenced(map[string]any{"verdict": "gaps", "summary": "missing pieces",
		"gaps": []map[string]any{{"severity": "major", "step": 2, "description": desc}}}))
}

// recorder captures every callback.
type recorder struct {
	mu        sync.Mutex
	phases    []string
	activity  []string
	plans     []string
	reviews   []string
	steps     [][]StepState
	verified  []*VerifyReport
	changes   []*ChangeSet
	tests     []*TestRun
	usage     int
	notices   []string
	approvals []ApprovalRequest
	decide    func(n int, req ApprovalRequest) (Approval, error)
	inFlight  int
	overlap   bool
}

func (r *recorder) enter() func() {
	r.mu.Lock()
	r.inFlight++
	if r.inFlight > 1 {
		r.overlap = true
	}
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		r.inFlight--
		r.mu.Unlock()
	}
}

func (r *recorder) callbacks() Callbacks {
	return Callbacks{
		Phase: func(p Phase, d string) {
			defer r.enter()()
			s := string(p)
			if d != "" {
				s += "(" + d + ")"
			}
			r.phases = append(r.phases, s)
		},
		Activity: func(p Phase, ev string) { defer r.enter()(); r.activity = append(r.activity, string(p)+":"+ev) },
		Plan: func(p *Plan, rev int) {
			defer r.enter()()
			r.plans = append(r.plans, fmt.Sprintf("%d:%s", rev, p.Goal))
		},
		Review: func(round int, rv *Review) {
			defer r.enter()()
			r.reviews = append(r.reviews, fmt.Sprintf("%d:%s", round, rv.Verdict))
		},
		Steps: func(s []StepState) { defer r.enter()(); r.steps = append(r.steps, s) },
		Tests: func(tr *TestRun) { defer r.enter()(); r.tests = append(r.tests, tr) },
		Verified: func(v *VerifyReport, cs *ChangeSet) {
			defer r.enter()()
			r.verified = append(r.verified, v)
			r.changes = append(r.changes, cs)
		},
		Usage:  func(Phase, string, llm.Usage) { defer r.enter()(); r.usage++ },
		Notice: func(m string) { defer r.enter()(); r.notices = append(r.notices, m) },
	}
}

func (r *recorder) approve(_ context.Context, req ApprovalRequest) (Approval, error) {
	r.mu.Lock()
	r.approvals = append(r.approvals, req)
	n := len(r.approvals)
	r.mu.Unlock()
	if r.decide != nil {
		return r.decide(n, req)
	}
	return Approval{Decision: DecisionApprove}, nil
}

type harness struct {
	t    *testing.T
	root string
	p    *fake
	ui   *uiSink
	rec  *recorder
	cfg  Config
}

func newHarness(t *testing.T, mode permission.Mode) *harness {
	t.Helper()
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"README.md": "# demo\n"})
	h := &harness{t: t, root: root, p: newFake(), ui: &uiSink{decision: permission.DecisionAllowOnce}, rec: &recorder{}}
	h.cfg = Config{
		Executor:  newMainAgent(t, h.p, root, mode),
		Sink:      h.ui,
		Callbacks: h.rec.callbacks(),
		Approve:   h.rec.approve,
	}
	return h
}

func (h *harness) run(task string) (*Result, error) {
	h.t.Helper()
	r, err := New(h.cfg)
	if err != nil {
		h.t.Fatal(err)
	}
	return r.Run(context.Background(), task)
}

// writeHello is the executor turn that implements the two-step plan.
func writeHello() []step {
	return []step{
		call(todos(StatusInProgress, StatusPending), use("write_file", map[string]any{"path": "hello.txt", "content": "hi\n"})),
		call(todos(StatusCompleted, StatusCompleted)),
		say("Created hello.txt and checked it."),
	}
}

func TestRunnerFullPipeline(t *testing.T) {
	h := newHarness(t, permission.ModeAutoEdit)
	h.p.on(rolePlanner,
		call(use("list_dir", map[string]any{})),
		say(planDoc("Greet the user", "Write hello.txt", "Check it")),
		say(planDoc("Greet the user politely", "Write hello.txt", "Check it")),
	).on(roleReviewer,
		revise("hello.txt must end with a newline"),
		approve(),
	).on(roleMain, writeHello()...).on(roleVerifier, pass())

	res, err := h.run("Create hello.txt containing hi")
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeDone {
		t.Fatalf("outcome %s:\n%s", res.Outcome, res.Summary())
	}
	wantPhases := "plan,review(round 1),plan(revision 1),review(round 2),approval,execute,verify,done(done)"
	if got := strings.Join(h.rec.phases, ","); got != wantPhases {
		t.Fatalf("phases:\n got %s\nwant %s", got, wantPhases)
	}
	if res.Revisions != 1 || len(res.Reviews) != 2 || !res.Reviews[1].Approved() || len(res.Unresolved) != 0 {
		t.Fatalf("review loop: %+v", res)
	}
	if strings.Join(h.rec.plans, "|") != "0:Greet the user|1:Greet the user politely" || strings.Join(h.rec.reviews, "|") != "1:revise|2:approve" {
		t.Fatalf("plan/review callbacks: %v %v", h.rec.plans, h.rec.reviews)
	}
	if len(h.rec.approvals) != 1 || h.rec.approvals[0].Plan.Goal != "Greet the user politely" || !h.rec.approvals[0].Review.Approved() || h.rec.approvals[0].Revisions != 1 {
		t.Fatalf("approval request: %+v", h.rec.approvals)
	}
	if got, _ := os.ReadFile(filepath.Join(h.root, "hello.txt")); string(got) != "hi\n" {
		t.Fatalf("hello.txt = %q", got)
	}
	if StepsDone(res.Steps) != 2 || len(h.rec.steps) < 2 || StepsDone(h.rec.steps[0]) != 0 {
		t.Fatalf("step progress: %+v (callbacks %v)", res.Steps, h.rec.steps)
	}
	if todos := h.cfg.Executor.Env.Todos(); len(todos) != 2 || todos[1].Status != StatusCompleted {
		t.Fatalf("executor todos %+v", todos)
	}
	if res.Changes == nil || len(res.Changes.Files) != 1 || res.Changes.Files[0].Path != "hello.txt" || res.Changes.Files[0].Kind != ChangeAdded {
		t.Fatalf("changes %+v", res.Changes)
	}
	if !res.Verification.Passed() || len(h.rec.verified) != 1 || h.rec.changes[0].Files[0].Path != "hello.txt" {
		t.Fatalf("verification %+v", res.Verification)
	}

	// Usage: planner 3 + reviewer 2 + verifier 1 helper calls, executor 3.
	if h.rec.usage != 6 || res.ModelCalls != 9 || res.Usage.InputTokens != 900 {
		t.Fatalf("usage: callbacks %d, calls %d, %+v", h.rec.usage, res.ModelCalls, res.Usage)
	}
	if s := res.Phases[PhaseExecute]; s.ModelCalls != 3 || s.ToolCalls != 3 {
		t.Fatalf("execute stats %+v", s)
	}
	if s := res.Phases[PhasePlan]; s.ModelCalls != 3 || s.ToolCalls != 1 {
		t.Fatalf("plan stats %+v", s)
	}
	if h.rec.overlap {
		t.Fatal("callbacks overlapped")
	}

	// Helpers are read-only and see the right context.
	planReq := h.p.reqs(rolePlanner)
	var names []string
	for _, s := range planReq[0].Tools {
		names = append(names, s.Name)
	}
	if strings.Join(names, ",") != "read_file,list_dir,glob,grep" {
		t.Fatalf("planner tools %v", names)
	}
	if !strings.Contains(planReq[0].System, "Workspace root: "+h.root) || !strings.Contains(planReq[0].System, `"verification"`) {
		t.Fatalf("planner system prompt:\n%s", planReq[0].System)
	}
	if !strings.Contains(lastUserText(planReq[2]), "hello.txt must end with a newline") {
		t.Fatalf("revision prompt lacks the reviewer's issue:\n%s", lastUserText(planReq[2]))
	}
	if !strings.Contains(lastUserText(h.p.reqs(roleReviewer)[1]), "Re-review") {
		t.Fatal("second review must be a re-review in the same conversation")
	}
	execPrompt := lastUserText(h.p.reqs(roleMain)[0])
	for _, s := range []string{"Approved plan", "1. Write hello.txt", "todo_write", "Create hello.txt containing hi"} {
		if !strings.Contains(execPrompt, s) {
			t.Errorf("execute prompt lacks %q:\n%s", s, execPrompt)
		}
	}
	verifyReq := lastUserText(h.p.reqs(roleVerifier)[0])
	if !strings.Contains(verifyReq, "+hi") || !strings.Contains(verifyReq, "1. Write hello.txt: completed") {
		t.Errorf("verifier prompt lacks diff or step status:\n%s", verifyReq)
	}
	if len(h.rec.activity) == 0 || !strings.HasPrefix(h.rec.activity[0], "plan:") {
		t.Errorf("helper activity not reported: %v", h.rec.activity)
	}
	sum := res.Summary()
	for _, s := range []string{"Autopilot done", "Plan: 2 steps, 1 file, revised 1 time, 2 review rounds, approved by the reviewer", "Steps: 2/2 completed", "A  hello.txt  +1 -0", "Verification passed", "Model calls: 9"} {
		if !strings.Contains(sum, s) {
			t.Errorf("summary lacks %q:\n%s", s, sum)
		}
	}
}

func TestRunnerRepairsUnparseablePlan(t *testing.T) {
	h := newHarness(t, permission.ModeAutoEdit)
	h.cfg.SkipReview, h.cfg.SkipVerify = true, true
	broken := "```json\n{\"goal\": \"g\", \"steps\": [{\"title\": \"Write hello.txt\"}]}\n```"
	h.p.on(rolePlanner, say(broken), say(planDoc("g", "Write hello.txt", "Check it"))).
		on(roleMain, writeHello()...)
	res, err := h.run("task")
	if err != nil || res.Outcome != OutcomeDone {
		t.Fatalf("%v %+v", err, res)
	}
	repair := lastUserText(h.p.reqs(rolePlanner)[1])
	if !strings.Contains(repair, "could not be used") || !strings.Contains(repair, `"verification" must list`) || !strings.Contains(repair, "Do not call tools") {
		t.Fatalf("repair prompt:\n%s", repair)
	}
	if len(h.rec.notices) == 0 || !strings.Contains(h.rec.notices[0], "asking again") {
		t.Fatalf("notices %v", h.rec.notices)
	}
}

func TestRunnerFailsOnTwiceUnparseablePlan(t *testing.T) {
	h := newHarness(t, permission.ModeAutoEdit)
	h.p.on(rolePlanner, say("no json here"), say("still none"))
	res, err := h.run("task")
	var fe *FormatError
	if !errors.As(err, &fe) || fe.Phase != PhasePlan || fe.What != "plan" || res.Outcome != OutcomeFailed {
		t.Fatalf("err %v res %+v", err, res)
	}
	if len(h.p.reqs(roleMain)) != 0 || len(h.rec.approvals) != 0 {
		t.Fatal("nothing may run after a planning failure")
	}
	if h.rec.phases[len(h.rec.phases)-1] != "done(failed)" {
		t.Fatalf("phases %v", h.rec.phases)
	}
}

func TestRunnerRejectBillsHelpersToExecutor(t *testing.T) {
	h := newHarness(t, permission.ModeAutoEdit)
	h.cfg.Callbacks.Usage = nil
	h.rec.decide = func(int, ApprovalRequest) (Approval, error) { return Approval{Decision: DecisionReject}, nil }
	h.p.on(rolePlanner, say(planDoc("g", "Write hello.txt"))).on(roleReviewer, approve())
	res, err := h.run("task")
	if err != nil || res.Outcome != OutcomeRejected {
		t.Fatalf("%v %+v", err, res)
	}
	if len(h.p.reqs(roleMain)) != 0 || res.Changes != nil {
		t.Fatal("a rejected plan must not execute")
	}
	if _, err := os.Stat(filepath.Join(h.root, "hello.txt")); err == nil {
		t.Fatal("rejected plan changed the workspace")
	}
	u, cost, _ := h.cfg.Executor.Stats()
	if u.InputTokens != 200 || cost <= 0 {
		t.Fatalf("helper usage must be billed to the executor: %+v %v", u, cost)
	}
}

func TestRunnerUserRevision(t *testing.T) {
	h := newHarness(t, permission.ModeAutoEdit)
	h.cfg.SkipVerify = true
	h.rec.decide = func(n int, _ ApprovalRequest) (Approval, error) {
		if n == 1 {
			return Approval{Decision: DecisionRevise, Feedback: "  Also print a farewell.  "}, nil
		}
		return Approval{Decision: DecisionApprove}, nil
	}
	h.p.on(rolePlanner, say(planDoc("v1", "Write hello.txt", "Check it")), say(planDoc("v2", "Write hello.txt", "Check it"))).
		on(roleReviewer, approve(), approve()).
		on(roleMain, writeHello()...)
	res, err := h.run("task")
	if err != nil || res.Outcome != OutcomeDone || res.UserRevisions != 1 || res.Revisions != 1 || res.Plan.Goal != "v2" {
		t.Fatalf("%v %+v", err, res)
	}
	if got := lastUserText(h.p.reqs(rolePlanner)[1]); !strings.Contains(got, "The user reviewed the plan") || !strings.Contains(got, "Also print a farewell.") {
		t.Fatalf("feedback prompt:\n%s", got)
	}
	if len(h.rec.approvals) != 2 || h.rec.approvals[1].Plan.Goal != "v2" || len(res.Reviews) != 2 {
		t.Fatalf("the revised plan must be reviewed and approved again: %+v", h.rec.approvals)
	}
}

func TestRunnerRevisionLimitAndInvalidDecisions(t *testing.T) {
	h := newHarness(t, permission.ModeAutoEdit)
	h.cfg.SkipReview, h.cfg.MaxRevisions = true, 1
	h.rec.decide = func(int, ApprovalRequest) (Approval, error) {
		return Approval{Decision: DecisionRevise, Feedback: "again"}, nil
	}
	h.p.on(rolePlanner, say(planDoc("v1", "a")), say(planDoc("v2", "a")))
	res, err := h.run("task")
	if err != nil || res.Outcome != OutcomeRejected || res.UserRevisions != 1 || !strings.Contains(strings.Join(res.Notes, " "), "limit") {
		t.Fatalf("%v %+v", err, res)
	}

	for _, ap := range []Approval{{Decision: DecisionRevise}, {Decision: PlanDecision(42)}, {}} {
		h := newHarness(t, permission.ModeAutoEdit)
		h.cfg.SkipReview = true
		h.rec.decide = func(int, ApprovalRequest) (Approval, error) { return ap, nil }
		h.p.on(rolePlanner, say(planDoc("v1", "a")))
		if _, err := h.run("task"); err == nil {
			t.Errorf("decision %+v must be rejected as invalid", ap)
		}
	}

	h = newHarness(t, permission.ModeAutoEdit)
	h.cfg.SkipReview = true
	h.rec.decide = func(int, ApprovalRequest) (Approval, error) { return Approval{}, errors.New("ui closed") }
	h.p.on(rolePlanner, say(planDoc("v1", "a")))
	if res, err := h.run("task"); err == nil || !strings.Contains(err.Error(), "ui closed") || res.Outcome != OutcomeFailed {
		t.Fatalf("approval error: %v", err)
	}
}

func TestRunnerReviewCapWithAutoApproveDoesNotExecute(t *testing.T) {
	h := newHarness(t, permission.ModeAutoEdit)
	h.cfg.Approve, h.cfg.AutoApprove = nil, true
	h.p.on(rolePlanner, say(planDoc("v1", "a")), say(planDoc("v2", "a"))).
		on(roleReviewer, revise("wrong file"), revise("still the wrong file"))
	res, err := h.run("task")
	if err != nil || res.Outcome != OutcomeRejected {
		t.Fatalf("%v %+v", err, res)
	}
	if len(res.Reviews) != 2 || res.Revisions != 1 || len(res.Unresolved) != 1 || res.Unresolved[0].Description != "still the wrong file" {
		t.Fatalf("review cap: %+v", res)
	}
	if len(h.p.reqs(roleMain)) != 0 {
		t.Fatal("auto-approve must not execute a plan the reviewer rejected")
	}
	if !strings.Contains(res.Summary(), "Unresolved review issues") {
		t.Fatalf("summary:\n%s", res.Summary())
	}
}

func TestRunnerUserCanApproveOverUnresolvedIssues(t *testing.T) {
	h := newHarness(t, permission.ModeAutoEdit)
	h.cfg.MaxReviewRounds, h.cfg.SkipVerify = 1, true
	h.p.on(rolePlanner, say(planDoc("v1", "Write hello.txt", "Check it"))).
		on(roleReviewer, revise("concern")).
		on(roleMain, writeHello()...)
	res, err := h.run("task")
	if err != nil || res.Outcome != OutcomeDone {
		t.Fatalf("%v %+v", err, res)
	}
	if len(h.rec.approvals) != 1 || len(h.rec.approvals[0].Unresolved) != 1 || h.rec.approvals[0].Review.Approved() {
		t.Fatalf("the user must see the unresolved issues: %+v", h.rec.approvals)
	}
}

func TestRunnerStopsExecutionWithoutProgress(t *testing.T) {
	h := newHarness(t, permission.ModeAutoEdit)
	h.cfg.SkipReview, h.cfg.SkipVerify = true, true
	h.p.on(rolePlanner, say(planDoc("g", "Write hello.txt", "Check it"))).
		on(roleMain,
			call(todos(StatusCompleted, StatusPending), use("write_file", map[string]any{"path": "hello.txt", "content": "hi\n"})),
			say("Step 1 done."),
			say("I will not do more."),
		)
	res, err := h.run("task")
	if err != nil || res.Outcome != OutcomeIncomplete || StepsDone(res.Steps) != 1 {
		t.Fatalf("%v %+v", err, res)
	}
	reqs := h.p.reqs(roleMain)
	if len(reqs) != 3 || !strings.Contains(lastUserText(reqs[2]), "2. Check it (pending)") {
		t.Fatalf("expected one continuation nudge naming the pending step; %d requests", len(reqs))
	}
	if !strings.Contains(strings.Join(res.Notes, " "), "stopped making progress") {
		t.Fatalf("notes %v", res.Notes)
	}
	if !strings.Contains(strings.Join(h.rec.phases, ","), "execute(continuation 1)") {
		t.Fatalf("phases %v", h.rec.phases)
	}
}

func TestRunnerExecRoundCap(t *testing.T) {
	h := newHarness(t, permission.ModeAutoEdit)
	h.cfg.SkipReview, h.cfg.SkipVerify, h.cfg.MaxExecRounds = true, true, 1
	busy := []step{call(todos(StatusCompleted, StatusInProgress)), say("working")}
	h.p.on(rolePlanner, say(planDoc("g", "a", "b"))).on(roleMain, append(busy, busy...)...)
	res, err := h.run("task")
	if err != nil || res.Outcome != OutcomeIncomplete || len(h.p.reqs(roleMain)) != 4 {
		t.Fatalf("%v %+v", err, res)
	}
	if !strings.Contains(strings.Join(res.Notes, " "), "still unfinished after 1 continuation round") {
		t.Fatalf("notes %v", res.Notes)
	}
}

func TestRunnerStopsWhenUserDeclines(t *testing.T) {
	h := newHarness(t, permission.ModeAsk)
	h.ui.decision = permission.DecisionDeny
	h.cfg.SkipReview = true
	h.p.on(rolePlanner, say(planDoc("g", "Write hello.txt"))).
		on(roleMain, call(use("write_file", map[string]any{"path": "hello.txt", "content": "hi\n"})))
	res, err := h.run("task")
	if err != nil || res.Outcome != OutcomeStopped || !strings.Contains(res.Declined, "hello.txt") {
		t.Fatalf("%v %+v", err, res)
	}
	if len(h.p.reqs(roleVerifier)) != 0 || res.Verification != nil {
		t.Fatal("verification must not run after the user stopped execution")
	}
	if !strings.Contains(res.Summary(), "Stopped: you declined") {
		t.Fatalf("summary:\n%s", res.Summary())
	}
}

func TestRunnerCancellationDuringExecution(t *testing.T) {
	h := newHarness(t, permission.ModeAutoEdit)
	h.cfg.SkipReview = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.p.on(rolePlanner, say(planDoc("g", "a"))).
		on(roleMain, func(c context.Context, _ llm.Request) (*llm.Response, error) {
			cancel()
			return nil, c.Err()
		})
	r, err := New(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Run(ctx, "task")
	if !errors.Is(err, context.Canceled) || res.Outcome != OutcomeCancelled {
		t.Fatalf("%v %+v", err, res)
	}
	if h.rec.phases[len(h.rec.phases)-1] != "done(cancelled)" {
		t.Fatalf("phases %v", h.rec.phases)
	}
	// The runner is free again afterwards.
	if _, err := r.Run(context.Background(), " "); !errors.Is(err, ErrEmptyTask) {
		t.Fatalf("want ErrEmptyTask, got %v", err)
	}
}

func TestRunnerCancellationDuringApproval(t *testing.T) {
	h := newHarness(t, permission.ModeAutoEdit)
	h.cfg.SkipReview = true
	ctx, cancel := context.WithCancel(context.Background())
	h.cfg.Approve = func(c context.Context, _ ApprovalRequest) (Approval, error) {
		cancel()
		<-c.Done()
		return Approval{}, c.Err()
	}
	h.p.on(rolePlanner, say(planDoc("g", "a")))
	r, _ := New(h.cfg)
	res, err := r.Run(ctx, "task")
	if !errors.Is(err, context.Canceled) || res.Outcome != OutcomeCancelled || len(h.p.reqs(roleMain)) != 0 {
		t.Fatalf("%v %+v", err, res)
	}
}

// failingGoScript prints a go-test failure and exits 1.
const failingGoScript = "printf -- '--- FAIL: TestHello (0.00s)\\n    hello_test.go:3: wrong greeting\\nFAIL\\nFAIL\\texample.com/h\\t0.01s\\n'; exit 1\n"

func TestRunnerFailingTestsOverrideVerifierPass(t *testing.T) {
	skipWithoutShell(t)
	h := newHarness(t, permission.ModeAutoEdit)
	h.cfg.SkipReview, h.cfg.VerifyTests = true, true
	writeFiles(t, h.root, map[string]string{"t.sh": failingGoScript})
	h.cfg.TestCommand = &TestCommand{Command: "sh t.sh", Framework: FrameworkGo}
	h.p.on(rolePlanner, say(planDoc("g", "Write hello.txt", "Check it"))).
		on(roleMain, writeHello()...).
		on(roleVerifier, pass())
	res, err := h.run("task")
	if err != nil || res.Outcome != OutcomeGaps {
		t.Fatalf("%v %+v", err, res)
	}
	g := res.Verification.Gaps[0]
	if g.Severity != SeverityBlocker || !strings.Contains(g.Description, "test suite is failing") {
		t.Fatalf("gaps %+v", res.Verification.Gaps)
	}
	if len(h.rec.tests) != 1 || res.Tests.Passed() {
		t.Fatalf("tests callback %v", h.rec.tests)
	}
	if prompt := lastUserText(h.p.reqs(roleVerifier)[0]); !strings.Contains(prompt, "TestHello") || !strings.Contains(prompt, "wrong greeting") {
		t.Fatalf("verifier must see the test failures:\n%s", prompt)
	}
}

func TestRunnerGapFixRound(t *testing.T) {
	h := newHarness(t, permission.ModeAutoEdit)
	h.cfg.SkipReview, h.cfg.GapFixRounds = true, 2
	h.p.on(rolePlanner, say(planDoc("g", "Write hello.txt", "Write bye.txt"))).
		on(roleMain, writeHello()...).
		on(roleMain,
			call(use("write_file", map[string]any{"path": "bye.txt", "content": "bye\n"})),
			say("Added bye.txt."),
		).
		on(roleVerifier, gaps("bye.txt is missing"), pass())
	res, err := h.run("task")
	if err != nil || res.Outcome != OutcomeDone || res.GapFixRounds != 1 {
		t.Fatalf("%v %+v", err, res)
	}
	if got := lastUserText(h.p.reqs(roleMain)[3]); !strings.Contains(got, "bye.txt is missing") {
		t.Fatalf("gap prompt:\n%s", got)
	}
	reverify := lastUserText(h.p.reqs(roleVerifier)[1])
	if !strings.Contains(reverify, "worked on the gaps") || !strings.Contains(reverify, "+bye") || !strings.Contains(reverify, "+hi") {
		t.Fatalf("re-verification must see the cumulative diff:\n%s", reverify)
	}
	if len(res.Changes.Files) != 2 || len(h.rec.verified) != 2 {
		t.Fatalf("changes %+v verified %d", res.Changes, len(h.rec.verified))
	}
	if !strings.Contains(strings.Join(h.rec.phases, ","), "execute(gap fix 1),verify(round 2)") {
		t.Fatalf("phases %v", h.rec.phases)
	}
}

func TestRunnerReportsGapsWithoutFixRounds(t *testing.T) {
	h := newHarness(t, permission.ModeAutoEdit)
	h.cfg.SkipReview = true
	h.p.on(rolePlanner, say(planDoc("g", "Write hello.txt", "Check it"))).
		on(roleMain, writeHello()...).
		on(roleVerifier, gaps("no check was run"))
	res, err := h.run("task")
	if err != nil || res.Outcome != OutcomeGaps || len(h.p.reqs(roleMain)) != 3 {
		t.Fatalf("%v %+v", err, res)
	}
	if !strings.Contains(res.Summary(), "Verification found gaps: missing pieces") {
		t.Fatalf("summary:\n%s", res.Summary())
	}
}

func TestRunnerIsExclusive(t *testing.T) {
	h := newHarness(t, permission.ModeAutoEdit)
	h.cfg.SkipReview = true
	entered := make(chan struct{})
	release := make(chan struct{})
	h.cfg.Approve = func(context.Context, ApprovalRequest) (Approval, error) {
		close(entered)
		<-release
		return Approval{Decision: DecisionReject}, nil
	}
	h.p.on(rolePlanner, say(planDoc("g", "a")))
	r, _ := New(h.cfg)
	done := make(chan error, 1)
	go func() {
		_, err := r.Run(context.Background(), "first")
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("first run never reached approval")
	}
	if _, err := r.Run(context.Background(), "second"); !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRunnerProviderErrorFails(t *testing.T) {
	h := newHarness(t, permission.ModeAutoEdit)
	h.p.on(rolePlanner, func(context.Context, llm.Request) (*llm.Response, error) { return nil, errors.New("rate limited") })
	res, err := h.run("task")
	if err == nil || !strings.Contains(err.Error(), "rate limited") || res.Outcome != OutcomeFailed {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestNewValidatesConfig(t *testing.T) {
	h := newHarness(t, permission.ModeAutoEdit)
	good := h.cfg
	cases := map[string]func(c *Config){
		"no executor": func(c *Config) { c.Executor = nil },
		"no sink":     func(c *Config) { c.Sink = nil },
		"no approve":  func(c *Config) { c.Approve = nil },
		"negative":    func(c *Config) { c.MaxReviewRounds = -1 },
	}
	for name, mutate := range cases {
		c := good
		mutate(&c)
		if _, err := New(c); err == nil {
			t.Errorf("%s: New must fail", name)
		}
	}
	c := good
	c.Approve, c.AutoApprove = nil, true
	if _, err := New(c); err != nil {
		t.Fatalf("auto-approve config: %v", err)
	}
	r, _ := New(good)
	if _, err := r.Run(context.Background(), "\n"); !errors.Is(err, ErrEmptyTask) {
		t.Fatalf("want ErrEmptyTask, got %v", err)
	}
}
