package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func mustSpec(t *testing.T, src string) *Spec {
	t.Helper()
	s, err := ParseSpec([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newEngine(t *testing.T, reg *Registry, mod func(*Options)) *Engine {
	t.Helper()
	opts := Options{Registry: reg, NewRunID: func() string { return "run-1" }}
	if mod != nil {
		mod(&opts)
	}
	e, err := NewEngine(opts)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

type eventLog struct {
	mu     sync.Mutex
	events []Event
}

func (l *eventLog) observe(e Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *eventLog) of(typ EventType) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Event
	for _, e := range l.events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func statuses(res *Result) map[string]StepStatus {
	out := map[string]StepStatus{}
	for _, s := range res.Steps {
		out[s.ID] = s.Status
	}
	return out
}

func TestRunDataFlow(t *testing.T) {
	reg := NewBuiltinRegistry(BuiltinOptions{})
	reg.MustRegister("double", ExecutorFunc(func(_ context.Context, sc StepContext) (Outputs, error) {
		n, _ := toNumber(sc.Inputs["n"])
		return Outputs{"n": int64(n * 2)}, nil // int64 is normalized to float64
	}))
	spec := mustSpec(t, `{
		"name": "flow",
		"inputs": {"start": {"type": "number", "default": 2}},
		"steps": [
			{"id": "a", "type": "double", "inputs": {"n": "${inputs.start}"}, "outputs": ["n"]},
			{"id": "m", "type": "double", "depends_on": ["a"], "matrix": {"k": [1, 10]},
			 "inputs": {"n": "${steps.a.outputs.n * matrix.k}"}},
			{"id": "skipped", "type": "noop", "depends_on": ["a"], "when": "steps.a.outputs.n > 100"},
			{"id": "after_skip", "type": "noop", "depends_on": ["skipped"]},
			{"id": "forced", "type": "noop", "depends_on": ["skipped"], "when": "always()"},
			{"id": "sum", "type": "noop", "depends_on": ["m"],
			 "inputs": {"all": "${steps.m.outputs.n}", "label": "${format('{0}:{1}', run.name, steps.m.status)}"}}
		],
		"outputs": {"all": "${steps.sum.outputs.all}", "label": "${steps.sum.outputs.label}"}
	}`)
	events := &eventLog{}
	res, err := newEngine(t, reg, func(o *Options) { o.Observer = events.observe }).Run(context.Background(), spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != RunSucceeded {
		t.Fatalf("status = %s", res.Status)
	}
	want := map[string]any{"all": []any{8.0, 80.0}, "label": "flow:succeeded"}
	if !reflect.DeepEqual(res.Outputs, want) {
		t.Fatalf("outputs = %v, want %v", res.Outputs, want)
	}
	st := statuses(res)
	if st["skipped"] != StepSkipped || st["after_skip"] != StepSkipped || st["forced"] != StepSucceeded {
		t.Fatalf("statuses = %v", st)
	}
	if s, _ := res.Step("skipped"); !strings.Contains(s.Reason, "condition is false") {
		t.Fatalf("skip reason = %q", s.Reason)
	}
	if n := len(events.of(EventStepSucceeded)); n != 5 {
		t.Fatalf("%d step_succeeded events, want 5", n)
	}
	if len(events.of(EventRunStarted)) != 1 || len(events.of(EventRunFinished)) != 1 {
		t.Fatal("missing run events")
	}
}

// concurrencyProbe records the maximum number of executions in flight,
// overall and per key.
type concurrencyProbe struct {
	mu      sync.Mutex
	cur     map[string]int
	maxSeen map[string]int
}

func newProbe() *concurrencyProbe {
	return &concurrencyProbe{cur: map[string]int{}, maxSeen: map[string]int{}}
}

func (p *concurrencyProbe) executor(keyOf func(StepContext) []string) Executor {
	return ExecutorFunc(func(ctx context.Context, sc StepContext) (Outputs, error) {
		keys := append(keyOf(sc), "*")
		p.mu.Lock()
		for _, k := range keys {
			p.cur[k]++
			p.maxSeen[k] = max(p.maxSeen[k], p.cur[k])
		}
		p.mu.Unlock()
		select {
		case <-time.After(15 * time.Millisecond):
		case <-ctx.Done():
		}
		p.mu.Lock()
		for _, k := range keys {
			p.cur[k]--
		}
		p.mu.Unlock()
		return nil, ctx.Err()
	})
}

func TestConcurrencyLimits(t *testing.T) {
	probe := newProbe()
	reg := NewRegistry()
	reg.MustRegister("work", probe.executor(func(sc StepContext) []string {
		var keys []string
		if g, _ := sc.Inputs["g"].(string); g != "" {
			keys = append(keys, g)
		}
		return append(keys, sc.BaseStepID)
	}))
	spec := mustSpec(t, `{
		"concurrency": 3,
		"groups": {"db": 1},
		"steps": [
			{"id": "d1", "type": "work", "group": "db", "inputs": {"g": "db"}},
			{"id": "d2", "type": "work", "group": "db", "inputs": {"g": "db"}},
			{"id": "d3", "type": "work", "group": "db", "inputs": {"g": "db"}},
			{"id": "free1", "type": "work"},
			{"id": "free2", "type": "work"},
			{"id": "m", "type": "work", "matrix": {"i": [1, 2, 3, 4, 5, 6]}, "max_parallel": 2}
		]}`)
	res, err := newEngine(t, reg, nil).Run(context.Background(), spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Count(StepSucceeded) != 11 {
		t.Fatalf("statuses = %v", statuses(res))
	}
	if got := probe.maxSeen["*"]; got != 3 {
		t.Errorf("max overall concurrency = %d, want 3", got)
	}
	if got := probe.maxSeen["db"]; got != 1 {
		t.Errorf("max db concurrency = %d, want 1", got)
	}
	if got := probe.maxSeen["m"]; got != 2 {
		t.Errorf("max matrix concurrency = %d, want 2", got)
	}

	// Options.MaxConcurrency caps the spec.
	probe2 := newProbe()
	reg2 := NewRegistry()
	reg2.MustRegister("work", probe2.executor(func(StepContext) []string { return nil }))
	if _, err := newEngine(t, reg2, func(o *Options) { o.MaxConcurrency = 1 }).Run(context.Background(), spec, nil); err != nil {
		t.Fatal(err)
	}
	if got := probe2.maxSeen["*"]; got != 1 {
		t.Errorf("max concurrency with MaxConcurrency=1: %d", got)
	}
}

func TestSchedulingPrefersCriticalPath(t *testing.T) {
	var mu sync.Mutex
	var order []string
	reg := NewRegistry()
	reg.MustRegister("rec", ExecutorFunc(func(_ context.Context, sc StepContext) (Outputs, error) {
		mu.Lock()
		order = append(order, sc.StepID)
		mu.Unlock()
		return nil, nil
	}))
	spec := mustSpec(t, `{"concurrency": 1, "steps": [
		{"id": "short", "type": "rec", "estimate": "1s"},
		{"id": "tie_a", "type": "rec", "estimate": "2s"},
		{"id": "long", "type": "rec", "estimate": "10s"},
		{"id": "tie_b", "type": "rec", "estimate": "2s"}]}`)
	if _, err := newEngine(t, reg, nil).Run(context.Background(), spec, nil); err != nil {
		t.Fatal(err)
	}
	if want := []string{"long", "tie_a", "tie_b", "short"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %q, want %q", order, want)
	}
}

func runAsync(e *Engine, ctx context.Context, spec *Spec, path string) <-chan struct {
	res *Result
	err error
} {
	ch := make(chan struct {
		res *Result
		err error
	}, 1)
	go func() {
		var res *Result
		var err error
		if path == "" {
			res, err = e.Run(ctx, spec, nil)
		} else {
			res, err = e.Start(ctx, spec, nil, path)
		}
		ch <- struct {
			res *Result
			err error
		}{res, err}
	}()
	return ch
}

func TestRetryBackoffWithFakeClock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	clock := NewFakeClock(time.Unix(1_700_000_000, 0))
	events := &eventLog{}
	e := newEngine(t, NewBuiltinRegistry(BuiltinOptions{}), func(o *Options) {
		o.Clock = clock
		o.Rand = func() float64 { return 1 } // full jitter at its ceiling
		o.Observer = events.observe
	})
	spec := mustSpec(t, `{"steps": [{"id": "flaky", "type": "fail",
		"inputs": {"fail_attempts": 2, "class": "transient"},
		"retry": {"max_attempts": 3, "initial_backoff": "1s", "multiplier": 3, "max_backoff": "2s"}}]}`)
	done := runAsync(e, ctx, spec, "")

	for i, want := range []time.Duration{time.Second, 2 * time.Second} { // 1s, then min(3s, cap 2s)
		if err := clock.WaitForTimers(ctx, 1); err != nil {
			t.Fatal(err)
		}
		if got := clock.Pending(); !reflect.DeepEqual(got, []time.Duration{want}) {
			t.Fatalf("backoff %d: pending timers %v, want [%v]", i+1, got, want)
		}
		clock.Advance(want)
	}
	out := <-done
	if out.err != nil {
		t.Fatal(out.err)
	}
	s, _ := out.res.Step("flaky")
	if s.Status != StepSucceeded || s.Attempts != 3 || s.Outputs["attempts"] != 3.0 {
		t.Fatalf("step = %+v", s)
	}
	retries := events.of(EventStepRetrying)
	if len(retries) != 2 || retries[0].Delay != time.Second || retries[1].Delay != 2*time.Second || retries[0].Class != ClassTransient {
		t.Fatalf("retry events = %v", retries)
	}
	if d := s.Duration(); d != 3*time.Second {
		t.Fatalf("duration on fake clock = %v, want 3s", d)
	}
}

func TestRetryClasses(t *testing.T) {
	cases := []struct {
		class, retryOn string
		attempts       int
	}{
		{"transient", `[]`, 3},
		{"error", `[]`, 1},
		{"error", `["error"]`, 3},
		{"error", `["*"]`, 3},
		{"permanent", `["*"]`, 1},
	}
	for _, c := range cases {
		t.Run(c.class+c.retryOn, func(t *testing.T) {
			spec := mustSpec(t, `{"steps": [{"id": "f", "type": "fail", "inputs": {"class": "`+c.class+`"},
				"retry": {"max_attempts": 3, "initial_backoff": "1ms", "retry_on": `+c.retryOn+`}}]}`)
			res, err := newEngine(t, NewBuiltinRegistry(BuiltinOptions{}), func(o *Options) { o.Rand = func() float64 { return 0 } }).
				Run(context.Background(), spec, nil)
			if !errors.Is(err, ErrRunFailed) {
				t.Fatalf("err = %v", err)
			}
			s, _ := res.Step("f")
			if s.Attempts != c.attempts || string(s.Class) != c.class {
				t.Fatalf("attempts = %d class = %s, want %d %s", s.Attempts, s.Class, c.attempts, c.class)
			}
		})
	}
}

func TestStepTimeoutWithFakeClock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	clock := NewFakeClock(time.Unix(0, 0))
	e := newEngine(t, NewBuiltinRegistry(BuiltinOptions{}), func(o *Options) { o.Clock = clock })
	spec := mustSpec(t, `{"steps": [{"id": "slow", "type": "sleep", "inputs": {"duration": "1h"}, "timeout": "5s"}]}`)
	done := runAsync(e, ctx, spec, "")
	if err := clock.WaitForTimers(ctx, 2); err != nil { // the sleep and the timeout
		t.Fatal(err)
	}
	clock.Advance(5 * time.Second)
	out := <-done
	if !errors.Is(out.err, ErrRunFailed) {
		t.Fatalf("err = %v", out.err)
	}
	s, _ := out.res.Step("slow")
	if s.Status != StepFailed || s.Class != ClassTimeout || !strings.Contains(s.Err.Error(), "timed out after 5s") {
		t.Fatalf("step = %+v", s)
	}
}

func TestRunTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	clock := NewFakeClock(time.Unix(0, 0))
	e := newEngine(t, NewBuiltinRegistry(BuiltinOptions{}), func(o *Options) { o.Clock = clock })
	spec := mustSpec(t, `{"timeout": "1m", "steps": [
		{"id": "slow", "type": "sleep", "inputs": {"duration": "1h"}},
		{"id": "next", "type": "noop", "depends_on": ["slow"]}]}`)
	done := runAsync(e, ctx, spec, "")
	if err := clock.WaitForTimers(ctx, 2); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	out := <-done
	if !errors.Is(out.err, ErrRunTimeout) || !errors.Is(out.err, ErrRunFailed) {
		t.Fatalf("err = %v", out.err)
	}
	if st := statuses(out.res); st["slow"] != StepCancelled || st["next"] != StepCancelled {
		t.Fatalf("statuses = %v", st)
	}
}

func TestFailFastCancelsRunningSteps(t *testing.T) {
	started := make(chan struct{})
	var sawCancel bool
	reg := NewBuiltinRegistry(BuiltinOptions{})
	reg.MustRegister("block", ExecutorFunc(func(ctx context.Context, _ StepContext) (Outputs, error) {
		close(started)
		<-ctx.Done()
		sawCancel = true
		return nil, ctx.Err()
	}))
	reg.MustRegister("fail_after_start", ExecutorFunc(func(context.Context, StepContext) (Outputs, error) {
		<-started
		return nil, errors.New("boom")
	}))
	spec := mustSpec(t, `{"steps": [
		{"id": "long", "type": "block"},
		{"id": "bad", "type": "fail_after_start"},
		{"id": "child", "type": "noop", "depends_on": ["long"]}]}`)
	res, err := newEngine(t, reg, nil).Run(context.Background(), spec, nil)
	if !errors.Is(err, ErrRunFailed) || !strings.Contains(err.Error(), "step bad failed: boom") {
		t.Fatalf("err = %v", err)
	}
	if !sawCancel {
		t.Fatal("running step was not cancelled")
	}
	want := map[string]StepStatus{"long": StepCancelled, "bad": StepFailed, "child": StepCancelled}
	if got := statuses(res); !reflect.DeepEqual(got, want) {
		t.Fatalf("statuses = %v, want %v", got, want)
	}
}

func TestContinueOnFailure(t *testing.T) {
	spec := mustSpec(t, `{"continue_on_failure": true, "steps": [
		{"id": "bad", "type": "fail"},
		{"id": "tolerated", "type": "fail", "continue_on_error": true},
		{"id": "after_tolerated", "type": "noop", "depends_on": ["tolerated"]},
		{"id": "independent", "type": "noop"},
		{"id": "dependent", "type": "noop", "depends_on": ["bad"]},
		{"id": "cleanup", "type": "noop", "depends_on": ["bad"], "when": "failure()"}]}`)
	res, err := newEngine(t, NewBuiltinRegistry(BuiltinOptions{}), nil).Run(context.Background(), spec, nil)
	if !errors.Is(err, ErrRunFailed) {
		t.Fatalf("err = %v", err)
	}
	want := map[string]StepStatus{
		"bad": StepFailed, "tolerated": StepFailed, "after_tolerated": StepSucceeded,
		"independent": StepSucceeded, "dependent": StepSkipped, "cleanup": StepSucceeded,
	}
	if got := statuses(res); !reflect.DeepEqual(got, want) {
		t.Fatalf("statuses = %v, want %v", got, want)
	}
	if s, _ := res.Step("tolerated"); !s.Tolerated {
		t.Fatal("tolerated failure not marked")
	}
}

func TestSagaCompensationOrder(t *testing.T) {
	var mu sync.Mutex
	var undone []string
	reg := NewBuiltinRegistry(BuiltinOptions{})
	reg.MustRegister("undo", ExecutorFunc(func(_ context.Context, sc StepContext) (Outputs, error) {
		if !sc.Compensating {
			return nil, errors.New("not compensating")
		}
		mu.Lock()
		undone = append(undone, sc.StepID+"="+stringify(sc.Inputs["v"]))
		mu.Unlock()
		if sc.Inputs["fail"] == true {
			return nil, Permanent(errors.New("cannot undo"))
		}
		return nil, nil
	}))
	spec := mustSpec(t, `{"steps": [
		{"id": "a", "type": "noop", "inputs": {"v": "A"}, "compensate": {"type": "undo", "inputs": {"v": "${self.outputs.v}"}}},
		{"id": "b", "type": "noop", "depends_on": ["a"], "inputs": {"v": "B"}, "compensate": {"type": "undo", "inputs": {"v": "${self.outputs.v}"}}},
		{"id": "c", "type": "noop", "depends_on": ["b"], "inputs": {"v": "C"}, "compensate": {"type": "undo", "inputs": {"v": "${self.outputs.v}", "fail": true}}},
		{"id": "d", "type": "noop", "depends_on": ["a"], "inputs": {"v": "D"}, "compensate": {"type": "undo", "inputs": {"v": "${steps.a.outputs.v}+${self.outputs.v}"}}},
		{"id": "nocomp", "type": "noop", "depends_on": ["a"]},
		{"id": "e", "type": "fail", "depends_on": ["c", "d"], "compensate": {"type": "undo"}}]}`)
	events := &eventLog{}
	res, err := newEngine(t, reg, func(o *Options) { o.Observer = events.observe }).Run(context.Background(), spec, nil)
	if !errors.Is(err, ErrRunFailed) || !strings.Contains(err.Error(), "compensate c: cannot undo") {
		t.Fatalf("err = %v", err)
	}
	// Reverse topological order (a, b, c, d, nocomp, e); e failed, so it is
	// not compensated, and c's failure does not stop the others.
	if want := []string{"d=A+D", "c=C", "b=B", "a=A"}; !reflect.DeepEqual(undone, want) {
		t.Fatalf("compensation order = %q, want %q", undone, want)
	}
	if s, _ := res.Step("c"); s.Compensation != CompensationFailed {
		t.Fatalf("c compensation = %s", s.Compensation)
	}
	if s, _ := res.Step("a"); s.Compensation != CompensationSucceeded {
		t.Fatalf("a compensation = %s", s.Compensation)
	}
	if n := len(events.of(EventStepCompensated)); n != 3 {
		t.Fatalf("%d step_compensated events, want 3", n)
	}
	if res.Outputs != nil {
		t.Fatal("failed run has outputs")
	}
}

func TestPanicAndBadOutputs(t *testing.T) {
	reg := NewRegistry()
	reg.MustRegister("panic", ExecutorFunc(func(context.Context, StepContext) (Outputs, error) { panic("kaboom") }))
	reg.MustRegister("chan", ExecutorFunc(func(context.Context, StepContext) (Outputs, error) {
		return Outputs{"c": make(chan int)}, nil
	}))
	reg.MustRegister("partial", ExecutorFunc(func(context.Context, StepContext) (Outputs, error) {
		return Outputs{"a": 1}, nil
	}))
	for typ, want := range map[string]string{
		"panic":   "executor panicked: kaboom",
		"chan":    "outputs are not JSON values",
		"partial": `did not produce declared output "b"`,
	} {
		spec := mustSpec(t, `{"steps": [{"id": "s", "type": "`+typ+`", "outputs": ["a", "b"],
			"retry": {"retry_on": ["*"], "initial_backoff": "1ms"}}]}`)
		res, err := newEngine(t, reg, nil).Run(context.Background(), spec, nil)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", typ, err, want)
			continue
		}
		if s, _ := res.Step("s"); s.Attempts != 1 || s.Class != ClassPermanent {
			t.Errorf("%s: attempts %d class %s; permanent errors must not be retried", typ, s.Attempts, s.Class)
		}
	}
}

func TestEventsChannelNeverBlocks(t *testing.T) {
	ch := make(chan Event, 2)
	spec := mustSpec(t, `{"steps": [{"id": "a", "type": "noop"}, {"id": "b", "type": "noop", "depends_on": ["a"]}]}`)
	res, err := newEngine(t, NewBuiltinRegistry(BuiltinOptions{}), func(o *Options) { o.Events = ch }).Run(context.Background(), spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	// run_started, 2x(started, succeeded), run_finished = 6 events.
	if len(ch) != 2 || res.DroppedEvents != 4 {
		t.Fatalf("buffered %d, dropped %d", len(ch), res.DroppedEvents)
	}
	if first := <-ch; first.Type != EventRunStarted || first.RunID != "run-1" {
		t.Fatalf("first event = %v", first)
	}
}

// externalSystem stands in for a service that deduplicates requests by
// idempotency key, as the engine's resume contract expects.
type externalSystem struct {
	mu      sync.Mutex
	applied map[string]string // key -> step
	effects map[string]int    // step -> effects applied
	execs   map[string]int    // step -> executions
	resumed map[string]bool
}

func newExternalSystem() *externalSystem {
	return &externalSystem{applied: map[string]string{}, effects: map[string]int{}, execs: map[string]int{}, resumed: map[string]bool{}}
}

func (x *externalSystem) apply(sc StepContext) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.execs[sc.StepID]++
	if sc.Resumed {
		x.resumed[sc.StepID] = true
	}
	if _, done := x.applied[sc.IdempotencyKey]; done {
		return
	}
	x.applied[sc.IdempotencyKey] = sc.StepID
	x.effects[sc.StepID]++
}

const crashSpec = `{"concurrency": 1, "steps": [
	{"id": "a", "type": "effect", "estimate": "1s"},
	{"id": "b", "type": "effect", "depends_on": ["a"], "estimate": "1s"},
	{"id": "c", "type": "effect", "depends_on": ["b"], "estimate": "1s", "outputs": ["from"]},
	{"id": "d", "type": "effect", "depends_on": ["c"], "estimate": "1s", "inputs": {"from": "${steps.c.outputs.from}"}},
	{"id": "e", "type": "effect", "depends_on": ["a"], "estimate": "1s"}]}`

func TestCrashResume(t *testing.T) {
	for _, tc := range []struct {
		name        string
		tearBytes   int64
		wantResumed bool
	}{
		{"in-flight record survives", 0, true},
		{"torn tail drops the in-flight record", 7, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "runs", "wf.jsonl")
			ext := newExternalSystem()
			spec := mustSpec(t, crashSpec)

			// First process: c applies its effect, then hangs until killed.
			ctx, kill := context.WithCancel(context.Background())
			reg1 := NewRegistry()
			reg1.MustRegister("effect", ExecutorFunc(func(ctx context.Context, sc StepContext) (Outputs, error) {
				ext.apply(sc)
				if sc.StepID == "c" {
					kill()
					<-ctx.Done()
					return nil, ctx.Err()
				}
				return Outputs{"from": sc.StepID}, nil
			}))
			res, err := newEngine(t, reg1, nil).Start(ctx, spec, nil, path)
			if !errors.Is(err, ErrInterrupted) || !errors.Is(err, context.Canceled) || res.Status != RunInterrupted {
				t.Fatalf("first run: status %v err %v", res.Status, err)
			}
			if st := statuses(res); st["a"] != StepSucceeded || st["b"] != StepSucceeded || st["c"] != StepRunning || st["d"] != StepPending {
				t.Fatalf("first run statuses = %v", st)
			}

			// Simulate a crash in the middle of the last write.
			if tc.tearBytes > 0 {
				fi, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Truncate(path, fi.Size()-tc.tearBytes); err != nil {
					t.Fatal(err)
				}
			}

			// Second process.
			reg2 := NewRegistry()
			reg2.MustRegister("effect", ExecutorFunc(func(_ context.Context, sc StepContext) (Outputs, error) {
				ext.apply(sc)
				return Outputs{"from": sc.StepID}, nil
			}))
			events := &eventLog{}
			res, err = newEngine(t, reg2, func(o *Options) { o.Observer = events.observe }).Resume(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			if res.Status != RunSucceeded || res.RunID != "run-1" || res.Count(StepSucceeded) != 5 {
				t.Fatalf("resumed run: %s %v", res.Status, statuses(res))
			}
			for _, step := range []string{"a", "b", "c", "d", "e"} {
				if ext.effects[step] != 1 {
					t.Errorf("step %s side effect applied %d times, want exactly once", step, ext.effects[step])
				}
			}
			wantExecs := map[string]int{"a": 1, "b": 1, "c": 2, "d": 1, "e": 1}
			if !reflect.DeepEqual(ext.execs, wantExecs) {
				t.Errorf("executions = %v, want %v", ext.execs, wantExecs)
			}
			if ext.resumed["c"] != tc.wantResumed {
				t.Errorf("c resumed = %v, want %v", ext.resumed["c"], tc.wantResumed)
			}
			if len(events.of(EventRunResumed)) != 1 {
				t.Error("no run_resumed event")
			}
			if s, _ := res.Step("d"); s.Outputs["from"] != "d" {
				t.Errorf("d outputs = %v", s.Outputs)
			}

			// Resuming a finished run replays it without executing anything.
			before := len(ext.applied)
			again, err := newEngine(t, reg2, nil).Resume(context.Background(), path)
			if err != nil || again.Status != RunSucceeded || len(ext.applied) != before || ext.execs["a"] != 1 {
				t.Fatalf("re-resume: %v %v", again.Status, err)
			}
			recs, err := ReadJournal(path)
			if err != nil {
				t.Fatal(err)
			}
			if last := recs[len(recs)-1]; last.Type != RecRunFinished || last.Status != RunSucceeded {
				t.Fatalf("last record = %+v", last)
			}
		})
	}
}

func TestResumeCompensationPhase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wf.jsonl")
	var mu sync.Mutex
	var undone []string
	ctx, kill := context.WithCancel(context.Background())
	defer kill()
	undo := func(block bool) Executor {
		return ExecutorFunc(func(ctx context.Context, sc StepContext) (Outputs, error) {
			mu.Lock()
			undone = append(undone, sc.StepID)
			mu.Unlock()
			if block && sc.StepID == "b" {
				kill()
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return nil, nil
		})
	}
	spec := mustSpec(t, `{"steps": [
		{"id": "a", "type": "noop", "compensate": {"type": "undo"}},
		{"id": "b", "type": "noop", "depends_on": ["a"], "compensate": {"type": "undo"}},
		{"id": "c", "type": "fail", "depends_on": ["b"]}]}`)
	reg := NewBuiltinRegistry(BuiltinOptions{})
	reg.MustRegister("undo", undo(true))
	res, err := newEngine(t, reg, nil).Start(ctx, spec, nil, path)
	if !errors.Is(err, ErrInterrupted) || res.Status != RunInterrupted {
		t.Fatalf("first run: %v", err)
	}
	reg2 := NewBuiltinRegistry(BuiltinOptions{})
	reg2.MustRegister("undo", undo(false))
	res, err = newEngine(t, reg2, nil).Resume(context.Background(), path)
	if !errors.Is(err, ErrRunFailed) || res.Status != RunFailed {
		t.Fatalf("resumed: %v", err)
	}
	if want := []string{"b", "b", "a"}; !reflect.DeepEqual(undone, want) {
		t.Fatalf("compensations = %q, want %q", undone, want)
	}
	if s, _ := res.Step("c"); s.Attempts != 1 || s.Status != StepFailed {
		t.Fatalf("failed step re-ran: %+v", s)
	}
}

func TestStartRefusesExistingJournalAndLocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wf.jsonl")
	spec := mustSpec(t, `{"steps": [{"id": "a", "type": "noop"}]}`)
	e := newEngine(t, NewBuiltinRegistry(BuiltinOptions{}), nil)
	if _, err := e.Start(context.Background(), spec, nil, path); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Start(context.Background(), spec, nil, path); err == nil {
		t.Fatal("Start overwrote an existing journal")
	}
	j, _, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if _, err := e.Resume(context.Background(), path); !errors.Is(err, ErrJournalLocked) {
		t.Fatalf("concurrent resume: %v, want ErrJournalLocked", err)
	}
}
