package workflow

import (
	"container/heap"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"runtime/debug"
	"slices"
	"time"
)

// Errors reported by runs. Test them with errors.Is.
var (
	// ErrRunFailed wraps the error of a run that finished unsuccessfully.
	ErrRunFailed = errors.New("workflow run failed")
	// ErrInterrupted wraps the error of a run stopped by its context (or
	// by a journal failure) before it finished; it can be resumed.
	ErrInterrupted = errors.New("workflow run interrupted")
	// ErrRunTimeout is the failure cause of a run that exceeded
	// Spec.Timeout.
	ErrRunTimeout = errors.New("workflow run timed out")

	errStepTimeout = errors.New("step attempt timed out")
	errFailFast    = errors.New("cancelled because another step failed")
)

// Options configures an Engine.
type Options struct {
	// Registry resolves step types. Required.
	Registry *Registry
	// Clock drives timers and timestamps; nil means the system clock.
	Clock Clock
	// Rand returns uniformly distributed values in [0, 1) for backoff
	// jitter; nil means math/rand/v2.
	Rand func() float64
	// LookupEnv resolves ${env.NAME}; nil means os.LookupEnv.
	LookupEnv func(string) (string, bool)
	// Observer is called synchronously, from the scheduler goroutine, for
	// every event. It must return quickly and must not call back into the
	// engine.
	Observer func(Event)
	// Events receives every event without blocking; events that do not
	// fit in the channel's buffer are dropped and counted in
	// Result.DroppedEvents. The engine never closes it.
	Events chan<- Event
	// MaxConcurrency, when positive, caps the spec's concurrency.
	MaxConcurrency int
	// NewRunID generates run IDs; nil means a timestamped random ID.
	NewRunID func() string
}

// Engine runs workflows. It is safe for concurrent use; each run is
// independent.
type Engine struct {
	opts Options
}

// NewEngine returns an engine.
func NewEngine(opts Options) (*Engine, error) {
	if opts.Registry == nil {
		return nil, errors.New("workflow: Options.Registry is required")
	}
	if opts.Clock == nil {
		opts.Clock = SystemClock()
	}
	if opts.Rand == nil {
		opts.Rand = mrand.Float64
	}
	if opts.NewRunID == nil {
		clock := opts.Clock
		opts.NewRunID = func() string { return newRunID(clock.Now()) }
	}
	return &Engine{opts: opts}, nil
}

func newRunID(now time.Time) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return now.UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b[:])
}

// Validate checks spec against the engine's registry.
func (e *Engine) Validate(spec *Spec) error { return Validate(spec, e.opts.Registry) }

// Plan compiles spec with inputs without running it.
func (e *Engine) Plan(spec *Spec, inputs map[string]any) (*Plan, error) {
	return Compile(spec, e.opts.Registry, inputs, e.opts.LookupEnv)
}

// Run executes spec without a durable journal. The returned error is nil
// exactly when Result.Status is RunSucceeded; a failed run still returns
// its Result.
func (e *Engine) Run(ctx context.Context, spec *Spec, inputs map[string]any) (*Result, error) {
	plan, err := e.Plan(spec, inputs)
	if err != nil {
		return nil, err
	}
	return e.start(ctx, plan, &memJournal{}, "")
}

// Start executes spec, journaling every transition to a new file at
// journalPath so the run can be resumed with Resume after a crash or
// interruption. The file must not exist.
func (e *Engine) Start(ctx context.Context, spec *Spec, inputs map[string]any, journalPath string) (*Result, error) {
	plan, err := e.Plan(spec, inputs)
	if err != nil {
		return nil, err
	}
	j, err := CreateJournal(journalPath)
	if err != nil {
		return nil, err
	}
	defer j.Close()
	return e.start(ctx, plan, j, journalPath)
}

func (e *Engine) start(ctx context.Context, plan *Plan, j Journal, path string) (*Result, error) {
	specJSON, err := json.Marshal(plan.c.spec)
	if err != nil {
		return nil, fmt.Errorf("encode spec: %w", err)
	}
	r := e.newRun(plan, e.opts.NewRunID(), j, path)
	nodes := make([]string, len(plan.nodes))
	for i, n := range plan.nodes {
		nodes[i] = n.id
	}
	if err := r.log(&Record{Type: RecRunStarted, Spec: specJSON, Inputs: plan.inputs, Nodes: nodes, Reason: plan.c.spec.Name}); err != nil {
		return nil, err
	}
	r.started = r.now()
	return r.execute(ctx, false)
}

// Resume continues the run journaled at journalPath. Completed steps are
// not run again; steps that were in flight are re-run with the same
// idempotency key and StepContext.Resumed set. A finished run's Result is
// returned without running anything. The spec and inputs come from the
// journal; environment variables are read afresh.
func (e *Engine) Resume(ctx context.Context, journalPath string) (*Result, error) {
	j, recs, err := OpenJournal(journalPath)
	if err != nil {
		return nil, err
	}
	defer j.Close()
	if len(recs) == 0 || recs[0].Type != RecRunStarted {
		return nil, fmt.Errorf("workflow journal %s does not begin with a run_started record", journalPath)
	}
	first := recs[0]
	spec, err := ParseSpec(first.Spec)
	if err != nil {
		return nil, fmt.Errorf("journaled spec: %w", err)
	}
	plan, err := e.Plan(spec, first.Inputs)
	if err != nil {
		return nil, fmt.Errorf("journaled spec: %w", err)
	}
	ids := make([]string, len(plan.nodes))
	for i, n := range plan.nodes {
		ids[i] = n.id
	}
	if !slices.Equal(ids, first.Nodes) {
		return nil, fmt.Errorf("workflow journal %s: the plan no longer expands to the journaled steps (matrix inputs or environment changed)", journalPath)
	}
	r := e.newRun(plan, first.RunID, j, journalPath)
	r.started = first.Time
	if err := r.replay(recs[1:]); err != nil {
		return nil, fmt.Errorf("workflow journal %s: %w", journalPath, err)
	}
	if r.finished {
		return r.replayedResult()
	}
	if err := r.log(&Record{Type: RecRunResumed}); err != nil {
		return nil, err
	}
	return r.execute(ctx, true)
}

// ---- run state ----

type stepState struct {
	node      *pnode
	status    StepStatus
	attempt   int // current (or next) attempt number
	attempts  int // attempts started
	resumed   bool
	inputs    map[string]any
	outputs   map[string]any
	err       error
	class     ErrorClass
	tolerated bool
	reason    string
	started   time.Time
	finished  time.Time
	remaining int // non-terminal dependencies

	comp        CompensationStatus
	compAttempt int
	compErr     error
}

type attemptResult struct {
	st      *stepState
	attempt int
	outputs map[string]any
	err     error
	class   ErrorClass
}

type run struct {
	e           *Engine
	plan        *Plan
	spec        *Spec
	runID       string
	journal     Journal
	journalPath string
	clock       Clock
	started     time.Time

	states    []*stepState
	stepsView map[string]any

	ctx       context.Context
	runCtx    context.Context
	cancelRun context.CancelCauseFunc

	results      chan attemptResult
	wakeups      chan *stepState
	ready        readyQueue
	decideQ      []*stepState
	running      int
	waiting      int
	groupRunning map[string]int
	concurrency  int

	stopping     bool
	interrupted  bool
	failed       bool
	failErr      error
	fatalErr     error
	compensating bool
	compErrs     []error

	finished    bool
	finishedAt  time.Time
	finalStatus RunStatus
	finalErr    string
	outputs     map[string]any
	dropped     int
}

func (e *Engine) newRun(plan *Plan, runID string, j Journal, path string) *run {
	r := &run{
		e: e, plan: plan, spec: plan.c.spec, runID: runID, journal: j, journalPath: path,
		clock: e.opts.Clock, stepsView: map[string]any{},
		results: make(chan attemptResult), wakeups: make(chan *stepState),
		groupRunning: map[string]int{}, concurrency: plan.c.spec.Concurrency,
	}
	if m := e.opts.MaxConcurrency; m > 0 && (r.concurrency == 0 || m < r.concurrency) {
		r.concurrency = m
	}
	r.states = make([]*stepState, len(plan.nodes))
	for i, n := range plan.nodes {
		r.states[i] = &stepState{node: n, status: StepPending}
	}
	return r
}

func (r *run) now() time.Time { return r.clock.Now() }

// log appends a record. A journal failure is fatal to the run: it is
// interrupted (not failed), because its state can no longer be recorded.
func (r *run) log(rec *Record) error {
	if r.fatalErr != nil {
		return r.fatalErr
	}
	rec.Time = r.now().UTC()
	rec.RunID = r.runID
	if err := r.journal.Append(rec); err != nil {
		r.fatalErr = fmt.Errorf("workflow journal: %w", err)
		if r.cancelRun != nil {
			r.interrupt()
		}
		return r.fatalErr
	}
	return nil
}

func (r *run) emit(ev Event) {
	ev.Time = r.now()
	ev.RunID = r.runID
	if f := r.e.opts.Observer; f != nil {
		f(ev)
	}
	if ch := r.e.opts.Events; ch != nil {
		select {
		case ch <- ev:
		default:
			r.dropped++
		}
	}
}

// replay rebuilds run state from journal records.
func (r *run) replay(recs []Record) error {
	for _, rec := range recs {
		var st *stepState
		if rec.Step != "" {
			n, ok := r.plan.byID[rec.Step]
			if !ok {
				return fmt.Errorf("record %d names unknown step %q", rec.Seq, rec.Step)
			}
			st = r.states[n.seq]
		}
		needStep := func() error {
			if st == nil {
				return fmt.Errorf("record %d (%s) names no step", rec.Seq, rec.Type)
			}
			return nil
		}
		switch rec.Type {
		case RecRunResumed:
		case RecStepStarted, RecStepRetry, RecStepSucceeded, RecStepFailed, RecStepSkipped, RecStepCancelled,
			RecCompensationStarted, RecCompensationSucceeded, RecCompensationFailed:
			if err := needStep(); err != nil {
				return err
			}
			r.replayStep(st, rec)
		case RecRunCompensating:
			r.compensating = true
			if !r.failed {
				r.failed = true
				r.failErr = errors.New(rec.Reason)
			}
		case RecRunFinished:
			r.finished = true
			r.finishedAt = rec.Time
			r.finalStatus = rec.Status
			r.finalErr = rec.Error
			r.outputs = rec.Outputs
		case RecRunStarted:
			return fmt.Errorf("record %d: duplicate run_started", rec.Seq)
		default:
			return fmt.Errorf("record %d: unknown type %q", rec.Seq, rec.Type)
		}
	}
	for _, st := range r.states {
		if st.status.Terminal() {
			r.updateView(st.node.cs.spec.ID)
		}
	}
	return nil
}

func (r *run) replayStep(st *stepState, rec Record) {
	recErr := func() error {
		if rec.Error == "" {
			return nil
		}
		return &StepError{Class: rec.Class, Err: errors.New(rec.Error)}
	}
	switch rec.Type {
	case RecStepStarted:
		st.status = StepRunning
		st.attempt = rec.Attempt
		st.attempts = max(st.attempts, rec.Attempt)
		if st.started.IsZero() {
			st.started = rec.Time
		}
	case RecStepRetry:
		st.status = StepRetrying
		st.attempt = rec.Attempt + 1
		st.err, st.class = recErr(), rec.Class
	case RecStepSucceeded:
		st.status = StepSucceeded
		st.outputs = rec.Outputs
		if st.outputs == nil {
			st.outputs = map[string]any{}
		}
		st.err, st.class = nil, ""
		st.finished = rec.Time
	case RecStepFailed:
		st.status = StepFailed
		st.err, st.class, st.tolerated = recErr(), rec.Class, rec.Tolerated
		st.finished = rec.Time
		if !st.tolerated && !r.failed {
			r.failed = true
			r.failErr = fmt.Errorf("step %s failed: %w", st.node.id, st.err)
		}
	case RecStepSkipped:
		st.status, st.reason, st.finished = StepSkipped, rec.Reason, rec.Time
	case RecStepCancelled:
		st.status, st.reason, st.finished = StepCancelled, rec.Reason, rec.Time
	case RecCompensationStarted:
		st.comp, st.compAttempt = CompensationRunning, rec.Attempt
	case RecCompensationSucceeded:
		st.comp, st.compErr = CompensationSucceeded, nil
	case RecCompensationFailed:
		st.comp, st.compErr = CompensationFailed, recErr()
		r.compErrs = append(r.compErrs, fmt.Errorf("compensate %s: %w", st.node.id, st.compErr))
	}
}

// ---- scheduling ----

type readyQueue []*stepState

func (q readyQueue) Len() int { return len(q) }
func (q readyQueue) Less(i, j int) bool {
	a, b := q[i].node, q[j].node
	if a.rank != b.rank {
		return a.rank > b.rank
	}
	return a.seq < b.seq
}
func (q readyQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *readyQueue) Push(x any)   { *q = append(*q, x.(*stepState)) }
func (q *readyQueue) Pop() any {
	old := *q
	st := old[len(old)-1]
	old[len(old)-1] = nil
	*q = old[:len(old)-1]
	return st
}

func (r *run) execute(ctx context.Context, resumed bool) (*Result, error) {
	r.ctx = ctx
	r.runCtx, r.cancelRun = context.WithCancelCause(ctx)
	defer r.cancelRun(nil)
	if resumed {
		r.emit(Event{Type: EventRunResumed})
	} else {
		r.emit(Event{Type: EventRunStarted})
	}
	if r.fatalErr == nil {
		r.schedule()
	}
	return r.finish()
}

func (r *run) schedule() {
	for _, st := range r.states {
		st.remaining = 0
		for _, d := range st.node.deps {
			if !r.states[d.seq].status.Terminal() {
				st.remaining++
			}
		}
	}
	for _, st := range r.states {
		if st.status.Terminal() || st.remaining > 0 {
			continue
		}
		switch st.status {
		case StepRunning: // in flight when the run stopped
			st.resumed = true
			r.enqueue(st)
		case StepRetrying:
			r.enqueue(st)
		default:
			r.decideQ = append(r.decideQ, st)
		}
	}
	if r.compensating || r.failed && !r.spec.ContinueOnFailure {
		r.stopFailFast(r.failErr)
	}
	r.drainDecisions()

	var timeoutC <-chan time.Time
	if t := r.spec.Timeout.D(); t > 0 && !r.stopping {
		timer := r.clock.NewTimer(r.started.Add(t).Sub(r.now()))
		defer timer.Stop()
		timeoutC = timer.C()
	}
	ctxDone := r.ctx.Done()
	for {
		if ctxDone != nil && r.ctx.Err() != nil {
			ctxDone = nil
			r.interrupt()
		}
		if !r.stopping {
			r.dispatch()
		}
		if r.running == 0 && r.waiting == 0 && (r.stopping || r.ready.Len() == 0) {
			break
		}
		select {
		case res := <-r.results:
			r.noticeInterrupt()
			r.handleResult(res)
		case st := <-r.wakeups:
			r.noticeInterrupt()
			r.handleWakeup(st)
		case <-ctxDone:
			ctxDone = nil
			r.interrupt()
		case <-timeoutC:
			timeoutC = nil
			if !r.stopping {
				if !r.failed {
					r.failed = true
					r.failErr = fmt.Errorf("%w after %s", ErrRunTimeout, r.spec.Timeout.D())
				}
				r.stopFailFast(r.failErr)
			}
		}
		r.drainDecisions()
	}
	if !r.interrupted {
		for _, st := range r.states {
			if !st.status.Terminal() {
				// Unreachable by construction; fail loudly rather than
				// report a run as complete with steps that never ran.
				r.fatalErr = fmt.Errorf("internal scheduler error: step %s left %s", st.node.id, st.status)
				r.interrupted = true
				return
			}
		}
	}
}

// noticeInterrupt applies a parent cancellation before a result is
// handled: the result may have been caused by it and must then be treated
// as an interruption, not a step failure or cancellation.
func (r *run) noticeInterrupt() {
	if !r.interrupted && r.ctx.Err() != nil {
		r.interrupt()
	}
}

// interrupt stops the run without recording anything about unfinished
// steps, so Resume picks them up.
func (r *run) interrupt() {
	r.interrupted = true
	r.stopping = true
	cause := context.Cause(r.ctx)
	if cause == nil {
		cause = r.fatalErr
	}
	r.cancelRun(cause)
}

// stopFailFast stops dispatch, cancels running steps and records every
// step that will not run as cancelled.
func (r *run) stopFailFast(cause error) {
	if r.stopping {
		return
	}
	if cause == nil {
		cause = errFailFast
	}
	r.stopping = true
	r.cancelRun(errFailFast)
	for r.ready.Len() > 0 {
		r.cancelStep(heap.Pop(&r.ready).(*stepState), "run is stopping: "+cause.Error())
	}
}

func (r *run) drainDecisions() {
	for len(r.decideQ) > 0 {
		st := r.decideQ[0]
		r.decideQ[0] = nil
		r.decideQ = r.decideQ[1:]
		r.decide(st)
	}
}

func (r *run) depSummary(st *stepState) *depSummary {
	d := &depSummary{total: len(st.node.deps)}
	for _, dn := range st.node.deps {
		ds := r.states[dn.seq]
		switch {
		case ds.status == StepSucceeded, ds.status == StepFailed && ds.tolerated:
			d.succeeded++
		case ds.status == StepFailed:
			d.failed++
		case ds.status == StepSkipped:
			d.skipped++
		default:
			d.cancelled++
		}
	}
	return d
}

// decide runs once all of a step's dependencies are terminal: it skips,
// fails, cancels or enqueues the step.
func (r *run) decide(st *stepState) {
	if r.stopping {
		if !r.interrupted {
			r.cancelStep(st, "run is stopping")
		}
		return
	}
	cs := st.node.cs
	sum := r.depSummary(st)
	if (cs.when == nil || !cs.whenStatus) && !sum.allSucceeded() {
		r.skipStep(st, "a dependency did not succeed")
		return
	}
	if cs.when != nil {
		v, err := cs.when.eval(&evalEnv{vars: r.vars(st, nil), deps: sum})
		if err != nil {
			r.failStep(st, Permanent(fmt.Errorf("evaluate when: %w", err)), ClassPermanent, 0)
			return
		}
		if !truthy(v) {
			r.skipStep(st, "condition is false: "+clip(cs.when.String(), 120))
			return
		}
	}
	r.enqueue(st)
}

func (r *run) enqueue(st *stepState) {
	in, err := r.resolve(st.node.cs.inputs, r.vars(st, nil))
	if err != nil {
		r.failStep(st, Permanent(fmt.Errorf("resolve inputs: %w", err)), ClassPermanent, st.attempt)
		return
	}
	st.inputs = in
	if st.attempt == 0 {
		st.attempt = 1
	}
	st.status = StepPending
	heap.Push(&r.ready, st)
}

func (r *run) vars(st *stepState, self map[string]any) map[string]any {
	v := map[string]any{
		"inputs": r.plan.inputs,
		"env":    r.plan.env,
		"steps":  r.stepsView,
		"run":    map[string]any{"id": r.runID, "name": r.spec.Name},
	}
	if st != nil && st.node.matrix != nil {
		v["matrix"] = st.node.matrix
	}
	if self != nil {
		v["self"] = self
	}
	return v
}

func (r *run) resolve(t valueTemplate, vars map[string]any) (map[string]any, error) {
	if t == nil {
		return map[string]any{}, nil
	}
	v, err := t.eval(&evalEnv{vars: vars})
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("inputs evaluated to a %s", typeName(v))
	}
	return deepCopy(m).(map[string]any), nil
}

func (r *run) groupsFree(st *stepState) bool {
	for _, g := range st.node.cs.groups {
		if lim := r.plan.groupLimits[g]; lim > 0 && r.groupRunning[g] >= lim {
			return false
		}
	}
	return true
}

// dispatch starts ready steps in priority order while capacity allows. A
// step blocked only by a full group does not block other steps.
func (r *run) dispatch() {
	var blocked []*stepState
	for r.ready.Len() > 0 && !r.stopping {
		if r.concurrency > 0 && r.running >= r.concurrency {
			break
		}
		st := heap.Pop(&r.ready).(*stepState)
		if !r.groupsFree(st) {
			blocked = append(blocked, st)
			continue
		}
		r.launch(st)
	}
	for _, st := range blocked {
		heap.Push(&r.ready, st)
	}
}

func (r *run) launch(st *stepState) {
	cs := st.node.cs
	ex, ok := r.e.opts.Registry.Lookup(cs.spec.Type)
	if !ok {
		r.failStep(st, Permanent(fmt.Errorf("no executor registered for type %q", cs.spec.Type)), ClassPermanent, st.attempt)
		return
	}
	key := idempotencyKey(r.runID, st.node.id, "step")
	if err := r.log(&Record{Type: RecStepStarted, Step: st.node.id, Attempt: st.attempt, Key: key}); err != nil {
		heap.Push(&r.ready, st)
		return
	}
	now := r.now()
	st.status = StepRunning
	if st.started.IsZero() {
		st.started = now
	}
	st.attempts = max(st.attempts, st.attempt)
	r.running++
	for _, g := range cs.groups {
		r.groupRunning[g]++
	}
	sc := StepContext{
		RunID: r.runID, StepID: st.node.id, BaseStepID: cs.spec.ID, Type: cs.spec.Type,
		Attempt: st.attempt, MaxAttempts: cs.retry.maxAttempts, IdempotencyKey: key,
		Inputs: deepCopy(st.inputs).(map[string]any), Matrix: deepCopyMap(st.node.matrix),
		Resumed: st.resumed, Clock: r.clock,
	}
	st.resumed = false
	r.emit(Event{Type: EventStepStarted, Step: st.node.id, Attempt: st.attempt})
	go func() {
		out, class, err := callExecutor(r.runCtx, r.clock, ex, sc, cs.timeout, cs.outputs)
		r.results <- attemptResult{st: st, attempt: sc.Attempt, outputs: out, err: err, class: class}
	}()
}

// callExecutor runs one attempt with a timeout, recovering panics and
// normalizing outputs. The class is classCancelled when parent was
// cancelled, which is not a failure of the step.
func callExecutor(parent context.Context, clock Clock, ex Executor, sc StepContext, timeout time.Duration, declared map[string]bool) (map[string]any, ErrorClass, error) {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	stop := func() {}
	if timeout > 0 {
		t := clock.NewTimer(timeout)
		done := make(chan struct{})
		go func() {
			select {
			case <-t.C():
				cancel(errStepTimeout)
			case <-done:
			case <-ctx.Done():
			}
		}()
		stop = func() { close(done); t.Stop() }
	}
	out, err := safeExecute(ctx, ex, sc)
	stop()
	if err != nil {
		switch {
		case errors.Is(context.Cause(ctx), errStepTimeout) && parent.Err() == nil:
			return nil, ClassTimeout, &StepError{Class: ClassTimeout, Err: fmt.Errorf("attempt timed out after %s: %w", timeout, err)}
		case parent.Err() != nil:
			return nil, classCancelled, err
		}
		return nil, Classify(err), err
	}
	norm, nerr := normalizeMap(out)
	if nerr != nil {
		return nil, ClassPermanent, Permanent(fmt.Errorf("outputs are not JSON values: %w", nerr))
	}
	for _, name := range sortedKeys(declared) {
		if _, ok := norm[name]; !ok {
			return nil, ClassPermanent, Permanent(fmt.Errorf("executor did not produce declared output %q", name))
		}
	}
	return norm, "", nil
}

func safeExecute(ctx context.Context, ex Executor, sc StepContext) (out Outputs, err error) {
	defer func() {
		if p := recover(); p != nil {
			stack := string(debug.Stack())
			err = Permanent(fmt.Errorf("executor panicked: %v\n%s", p, clip(stack, 4096)))
		}
	}()
	return ex.Execute(ctx, sc)
}

func (r *run) release(st *stepState) {
	r.running--
	for _, g := range st.node.cs.groups {
		r.groupRunning[g]--
	}
}

func (r *run) handleResult(res attemptResult) {
	st := res.st
	r.release(st)
	if res.err == nil {
		st.status = StepSucceeded
		st.outputs = res.outputs
		st.err, st.class = nil, ""
		st.finished = r.now()
		_ = r.log(&Record{Type: RecStepSucceeded, Step: st.node.id, Attempt: res.attempt, Outputs: res.outputs})
		r.emit(Event{Type: EventStepSucceeded, Step: st.node.id, Attempt: res.attempt, Duration: st.finished.Sub(st.started)})
		r.onTerminal(st)
		return
	}
	if res.class == classCancelled {
		if r.interrupted {
			return // in flight: Resume re-runs it
		}
		r.cancelStep(st, "cancelled: "+context.Cause(r.runCtx).Error())
		return
	}
	if r.interrupted {
		return
	}
	policy := st.node.cs.retry
	if !r.stopping && policy.shouldRetry(res.attempt, res.class) {
		delay := policy.backoff(res.attempt, r.e.opts.Rand)
		if err := r.log(&Record{Type: RecStepRetry, Step: st.node.id, Attempt: res.attempt, Error: res.err.Error(), Class: res.class, Delay: Duration(delay)}); err != nil {
			return
		}
		r.emit(Event{Type: EventStepRetrying, Step: st.node.id, Attempt: res.attempt, Err: res.err, Class: res.class, Delay: delay})
		st.status = StepRetrying
		st.err, st.class = res.err, res.class
		st.attempt = res.attempt + 1
		r.waiting++
		go r.waitRetry(st, delay)
		return
	}
	r.failStep(st, res.err, res.class, res.attempt)
}

func (r *run) waitRetry(st *stepState, d time.Duration) {
	t := r.clock.NewTimer(d)
	select {
	case <-t.C():
	case <-r.runCtx.Done():
		t.Stop()
	}
	r.wakeups <- st
}

func (r *run) handleWakeup(st *stepState) {
	r.waiting--
	if r.stopping {
		if !r.interrupted {
			r.cancelStep(st, "run is stopping")
		}
		return
	}
	st.status = StepPending
	heap.Push(&r.ready, st)
}

func (r *run) failStep(st *stepState, err error, class ErrorClass, attempt int) {
	st.status = StepFailed
	st.err, st.class = err, class
	st.tolerated = st.node.cs.spec.ContinueOnError
	st.finished = r.now()
	if st.started.IsZero() {
		st.started = st.finished
	}
	if r.log(&Record{Type: RecStepFailed, Step: st.node.id, Attempt: attempt, Error: err.Error(), Class: class, Tolerated: st.tolerated}) != nil {
		return
	}
	r.emit(Event{Type: EventStepFailed, Step: st.node.id, Attempt: attempt, Err: err, Class: class, Duration: st.finished.Sub(st.started)})
	if !st.tolerated {
		if !r.failed {
			r.failed = true
			r.failErr = fmt.Errorf("step %s failed: %w", st.node.id, err)
		}
		if !r.spec.ContinueOnFailure {
			r.stopFailFast(r.failErr)
		}
	}
	r.onTerminal(st)
}

func (r *run) skipStep(st *stepState, reason string) {
	st.status, st.reason = StepSkipped, reason
	st.finished = r.now()
	if r.log(&Record{Type: RecStepSkipped, Step: st.node.id, Reason: reason}) != nil {
		return
	}
	r.emit(Event{Type: EventStepSkipped, Step: st.node.id, Reason: reason})
	r.onTerminal(st)
}

func (r *run) cancelStep(st *stepState, reason string) {
	st.status, st.reason = StepCancelled, reason
	st.finished = r.now()
	if r.log(&Record{Type: RecStepCancelled, Step: st.node.id, Reason: reason}) != nil {
		return
	}
	r.emit(Event{Type: EventStepCancelled, Step: st.node.id, Reason: reason})
	r.onTerminal(st)
}

func (r *run) onTerminal(st *stepState) {
	r.updateView(st.node.cs.spec.ID)
	for _, d := range st.node.dependents {
		ds := r.states[d.seq]
		ds.remaining--
		if ds.remaining == 0 {
			r.decideQ = append(r.decideQ, ds)
		}
	}
}

// updateView refreshes what expressions see as steps.<id>. A matrix step
// exposes its outputs as lists in instance order, plus its instances.
func (r *run) updateView(id string) {
	insts := r.plan.instances[id]
	if len(insts) == 1 && insts[0].matrix == nil {
		st := r.states[insts[0].seq]
		out := st.outputs
		if out == nil {
			out = map[string]any{}
		}
		r.stepsView[id] = map[string]any{"status": string(st.status), "outputs": out}
		return
	}
	keys := map[string]bool{}
	var statuses []StepStatus
	list := make([]any, len(insts))
	for i, n := range insts {
		st := r.states[n.seq]
		for k := range st.outputs {
			keys[k] = true
		}
		statuses = append(statuses, effectiveStatus(st))
		out := st.outputs
		if out == nil {
			out = map[string]any{}
		}
		list[i] = map[string]any{"matrix": n.matrix, "status": string(st.status), "outputs": out}
	}
	outputs := map[string]any{}
	for _, k := range sortedKeys(keys) {
		vals := make([]any, len(insts))
		for i, n := range insts {
			vals[i] = r.states[n.seq].outputs[k]
		}
		outputs[k] = vals
	}
	r.stepsView[id] = map[string]any{"status": string(aggregateStatus(statuses)), "outputs": outputs, "instances": list}
}

func effectiveStatus(st *stepState) StepStatus {
	if st.status == StepFailed && st.tolerated {
		return StepSucceeded
	}
	return st.status
}

func aggregateStatus(ss []StepStatus) StepStatus {
	count := map[StepStatus]int{}
	for _, s := range ss {
		count[s]++
	}
	switch {
	case count[StepFailed] > 0:
		return StepFailed
	case count[StepCancelled] > 0:
		return StepCancelled
	case count[StepSucceeded] == len(ss):
		return StepSucceeded
	case count[StepSkipped] == len(ss):
		return StepSkipped
	case count[StepSucceeded]+count[StepSkipped] == len(ss):
		return StepSucceeded
	}
	return StepRunning
}

// ---- completion and compensation ----

func (r *run) finish() (*Result, error) {
	if r.interrupted {
		return r.interruptedResult()
	}
	if !r.failed {
		out, err := r.evalOutputs()
		if err != nil {
			r.failed = true
			r.failErr = fmt.Errorf("workflow outputs: %w", err)
		} else {
			r.outputs = out
		}
	}
	if r.failed {
		if !r.compensating {
			if r.log(&Record{Type: RecRunCompensating, Reason: r.failErr.Error()}) != nil {
				return r.interruptedResult()
			}
			r.compensating = true
		}
		r.compensate()
		if r.interrupted {
			return r.interruptedResult()
		}
	}
	status := RunSucceeded
	var runErr error
	if r.failed {
		status = RunFailed
		runErr = r.failureError()
		r.outputs = nil
	}
	rec := &Record{Type: RecRunFinished, Status: status, Outputs: r.outputs}
	if runErr != nil {
		rec.Error = runErr.Error()
	}
	if r.log(rec) != nil {
		return r.interruptedResult()
	}
	r.emit(Event{Type: EventRunFinished, Status: status, Err: runErr})
	res := r.result(status, runErr)
	return res, runErr
}

func (r *run) failureError() error {
	err := fmt.Errorf("%w: %w", ErrRunFailed, r.failErr)
	if len(r.compErrs) > 0 {
		err = errors.Join(append([]error{err}, r.compErrs...)...)
	}
	return err
}

func (r *run) interruptedResult() (*Result, error) {
	cause := r.fatalErr
	if cause == nil {
		cause = context.Cause(r.ctx)
	}
	if cause == nil {
		cause = context.Canceled
	}
	err := fmt.Errorf("%w: %w", ErrInterrupted, cause)
	r.emit(Event{Type: EventRunFinished, Status: RunInterrupted, Err: err})
	return r.result(RunInterrupted, err), err
}

func (r *run) evalOutputs() (map[string]any, error) {
	out := map[string]any{}
	vars := r.vars(nil, nil)
	for _, name := range sortedKeys(r.plan.c.outputs) {
		v, err := r.plan.c.outputs[name].eval(&evalEnv{vars: vars})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out[name] = deepCopy(v)
	}
	return out, nil
}

// compensate undoes completed steps that declare a compensation, in
// reverse topological order, one at a time. A failed compensation is
// recorded and the remaining ones still run.
func (r *run) compensate() {
	for i := len(r.states) - 1; i >= 0; i-- {
		st := r.states[i]
		if st.status != StepSucceeded || st.node.cs.comp == nil {
			continue
		}
		if st.comp == CompensationSucceeded || st.comp == CompensationFailed {
			continue
		}
		if r.ctx.Err() != nil {
			r.interrupted = true
			return
		}
		r.compensateStep(st)
		if r.interrupted {
			return
		}
	}
}

func (r *run) compensateStep(st *stepState) {
	cc := st.node.cs.comp
	id := st.node.id
	fail := func(err error, class ErrorClass, attempt int) {
		st.comp, st.compErr = CompensationFailed, err
		r.compErrs = append(r.compErrs, fmt.Errorf("compensate %s: %w", id, err))
		if r.log(&Record{Type: RecCompensationFailed, Step: id, Attempt: attempt, Error: err.Error(), Class: class}) != nil {
			return
		}
		r.emit(Event{Type: EventCompensationFailed, Step: id, Attempt: attempt, Err: err, Class: class})
	}
	ex, ok := r.e.opts.Registry.Lookup(cc.typ)
	if !ok {
		fail(fmt.Errorf("no executor registered for type %q", cc.typ), ClassPermanent, 0)
		return
	}
	self := map[string]any{"outputs": st.outputs}
	inputs, err := r.resolve(cc.inputs, r.vars(st, self))
	if err != nil {
		fail(fmt.Errorf("resolve inputs: %w", err), ClassPermanent, 0)
		return
	}
	key := idempotencyKey(r.runID, id, "compensate")
	attempt := max(st.compAttempt, 1)
	resumed := st.comp == CompensationRunning
	for {
		if r.log(&Record{Type: RecCompensationStarted, Step: id, Attempt: attempt, Key: key}) != nil {
			r.interrupted = true
			return
		}
		st.comp, st.compAttempt = CompensationRunning, attempt
		r.emit(Event{Type: EventCompensationStarted, Step: id, Attempt: attempt})
		sc := StepContext{
			RunID: r.runID, StepID: id, BaseStepID: st.node.cs.spec.ID, Type: cc.typ,
			Attempt: attempt, MaxAttempts: cc.retry.maxAttempts, IdempotencyKey: key,
			Inputs: deepCopy(inputs).(map[string]any), Matrix: deepCopyMap(st.node.matrix),
			Resumed: resumed, Compensating: true, Outputs: deepCopyMap(st.outputs), Clock: r.clock,
		}
		_, class, err := callExecutor(r.ctx, r.clock, ex, sc, cc.timeout, nil)
		if err == nil {
			st.comp = CompensationSucceeded
			if r.log(&Record{Type: RecCompensationSucceeded, Step: id, Attempt: attempt}) != nil {
				r.interrupted = true
				return
			}
			r.emit(Event{Type: EventStepCompensated, Step: id, Attempt: attempt})
			return
		}
		if class == classCancelled {
			r.interrupted = true
			return
		}
		if cc.retry.shouldRetry(attempt, class) {
			delay := cc.retry.backoff(attempt, r.e.opts.Rand)
			r.emit(Event{Type: EventStepRetrying, Step: id, Attempt: attempt, Err: err, Class: class, Delay: delay, Reason: "compensation"})
			if sleepCtx(r.ctx, r.clock, delay) != nil {
				r.interrupted = true
				return
			}
			attempt++
			resumed = false
			continue
		}
		fail(err, class, attempt)
		return
	}
}

// ---- results ----

// StepResult is the outcome of one step instance.
type StepResult struct {
	// ID is the instance ID; Step is the spec step ID.
	ID        string
	Step      string
	Type      string
	Status    StepStatus
	Attempts  int
	Outputs   map[string]any
	Err       error
	Class     ErrorClass
	Tolerated bool
	// Reason explains a skip or cancellation.
	Reason          string
	Started         time.Time
	Finished        time.Time
	Compensation    CompensationStatus
	CompensationErr error
}

// Duration returns the step's wall time, or 0 if it never ran.
func (s StepResult) Duration() time.Duration {
	if s.Started.IsZero() || s.Finished.IsZero() {
		return 0
	}
	return s.Finished.Sub(s.Started)
}

// Result is the outcome of a run.
type Result struct {
	RunID string
	Name  string
	// Status is the run outcome; RunInterrupted runs can be resumed.
	Status RunStatus
	Err    error
	// Outputs are the evaluated workflow outputs of a successful run.
	Outputs map[string]any
	// Steps are in topological order.
	Steps         []StepResult
	Started       time.Time
	Finished      time.Time
	JournalPath   string
	DroppedEvents int
}

// Step returns the result of the step instance with the given ID.
func (r *Result) Step(id string) (StepResult, bool) {
	for _, s := range r.Steps {
		if s.ID == id {
			return s, true
		}
	}
	return StepResult{}, false
}

// Count returns how many steps ended with the given status.
func (r *Result) Count(status StepStatus) int {
	n := 0
	for _, s := range r.Steps {
		if s.Status == status {
			n++
		}
	}
	return n
}

func (r *run) result(status RunStatus, err error) *Result {
	res := &Result{
		RunID: r.runID, Name: r.spec.Name, Status: status, Err: err,
		Outputs: deepCopyMap(r.outputs), Started: r.started, Finished: r.now(),
		JournalPath: r.journalPath, DroppedEvents: r.dropped,
	}
	for _, st := range r.states {
		res.Steps = append(res.Steps, StepResult{
			ID: st.node.id, Step: st.node.cs.spec.ID, Type: st.node.cs.spec.Type,
			Status: st.status, Attempts: st.attempts, Outputs: deepCopyMap(st.outputs),
			Err: st.err, Class: st.class, Tolerated: st.tolerated, Reason: st.reason,
			Started: st.started, Finished: st.finished,
			Compensation: st.comp, CompensationErr: st.compErr,
		})
	}
	return res
}

func (r *run) replayedResult() (*Result, error) {
	var err error
	if r.finalErr != "" {
		err = fmt.Errorf("%w: %s", ErrRunFailed, r.finalErr)
	}
	res := r.result(r.finalStatus, err)
	res.Finished = r.finishedAt
	return res, err
}
