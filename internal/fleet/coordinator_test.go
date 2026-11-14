package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestLeaseExpiryRedelivers(t *testing.T) {
	clk := NewFakeClock(t0)
	c := openTest(t, testConfig("", clk))
	mustRegister(t, c, "w1", Capabilities{})
	mustRegister(t, c, "w2", Capabilities{})
	task := mustSubmit(t, c, TaskSpec{ID: "job", MaxAttempts: 3})

	a1 := leaseOne(t, c, "w1")
	if a1.Attempt != 1 || a1.Task.ID != "job" {
		t.Fatalf("first lease = %+v", a1)
	}
	if as := leaseN(t, c, "w2", 1); len(as) != 0 {
		t.Fatalf("leased task was delivered twice while its lease was live")
	}

	// Heartbeats keep the lease alive past its timeout.
	clk.Advance(8 * time.Second)
	hb, err := c.Heartbeat(context.Background(), HeartbeatRequest{WorkerID: "w1", Leases: []LeaseRef{{task.ID, a1.LeaseID}}})
	if err != nil || hb.Extended != 1 || len(hb.Revoked) != 0 {
		t.Fatalf("heartbeat = %+v, %v", hb, err)
	}
	clk.Advance(8 * time.Second)
	c.Tick()
	mustState(t, c, "job", StateLeased)

	// Without heartbeats the visibility timeout passes and the task is
	// redelivered to another worker.
	clk.Advance(3 * time.Second)
	c.Tick()
	a2 := leaseOne(t, c, "w2")
	if a2.Task.ID != "job" || a2.Attempt != 2 || a2.LeaseID == a1.LeaseID {
		t.Fatalf("redelivery = %+v", a2)
	}
	// The original holder learns it lost the lease and cannot complete.
	hb, err = c.Heartbeat(context.Background(), HeartbeatRequest{WorkerID: "w1", Leases: []LeaseRef{{task.ID, a1.LeaseID}}})
	if err != nil || len(hb.Revoked) != 1 {
		t.Fatalf("heartbeat after expiry = %+v, %v", hb, err)
	}
	wantErr(t, c.Complete(context.Background(), CompleteRequest{WorkerID: "w1", TaskID: "job", LeaseID: a1.LeaseID}), ErrLeaseLost)
	mustComplete(t, c, "w2", a2)
	got := mustState(t, c, "job", StateSucceeded)
	if got.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", got.Attempts)
	}
	m := c.Metrics(context.Background())
	if m.Counters.LeaseExpiries != 1 || m.Counters.Redeliveries != 1 || m.Counters.Succeeded != 1 {
		t.Fatalf("counters = %+v", m.Counters)
	}
}

func TestIdempotency(t *testing.T) {
	clk := NewFakeClock(t0)
	c := openTest(t, testConfig("", clk))
	ctx := context.Background()
	r1, err := c.Submit(ctx, TaskSpec{Tenant: "acme", Kind: "migrate", IdempotencyKey: "repo-42"})
	if err != nil || r1.Duplicate {
		t.Fatalf("first submit = %+v, %v", r1, err)
	}
	r2, err := c.Submit(ctx, TaskSpec{Tenant: "acme", Kind: "migrate", IdempotencyKey: "repo-42", Payload: json.RawMessage(`{"other":true}`)})
	if err != nil || !r2.Duplicate || r2.Task.ID != r1.Task.ID {
		t.Fatalf("duplicate submit = %+v, %v", r2, err)
	}
	// Keys are scoped per tenant.
	r3, err := c.Submit(ctx, TaskSpec{Tenant: "globex", Kind: "migrate", IdempotencyKey: "repo-42"})
	if err != nil || r3.Duplicate || r3.Task.ID == r1.Task.ID {
		t.Fatalf("other tenant submit = %+v, %v", r3, err)
	}
	// An explicit ID cannot be reused without the same key.
	mustSubmit(t, c, TaskSpec{ID: "fixed"})
	_, err = c.Submit(ctx, TaskSpec{ID: "fixed", Kind: "x"})
	wantErr(t, err, ErrConflict)

	// Acknowledgements are idempotent per lease.
	mustRegister(t, c, "w", Capabilities{})
	a := leaseOne(t, c, "w")
	req := CompleteRequest{WorkerID: "w", TaskID: a.Task.ID, LeaseID: a.LeaseID, Result: json.RawMessage(`1`)}
	for range 3 {
		if err := c.Complete(ctx, req); err != nil {
			t.Fatalf("repeated complete: %v", err)
		}
	}
	wantErr(t, c.Fail(ctx, FailRequest{WorkerID: "w", TaskID: a.Task.ID, LeaseID: a.LeaseID, Error: "late"}), ErrLeaseLost)
	m := c.Metrics(ctx)
	if m.Counters.Succeeded != 1 || m.Counters.Duplicates != 1 {
		t.Fatalf("counters = %+v", m.Counters)
	}
	// A duplicate of a finished task still returns it.
	r4, _ := c.Submit(ctx, TaskSpec{Tenant: "acme", Kind: "migrate", IdempotencyKey: "repo-42"})
	if !r4.Duplicate {
		t.Fatalf("duplicate after completion not detected")
	}
}

func TestDeadLetterQueue(t *testing.T) {
	clk := NewFakeClock(t0)
	c := openTest(t, testConfig("", clk))
	ctx := context.Background()
	mustRegister(t, c, "w", Capabilities{})
	mustSubmit(t, c, TaskSpec{ID: "flaky", MaxAttempts: 2})
	mustSubmit(t, c, TaskSpec{ID: "child", DependsOn: []string{"flaky"}})

	for i := range 2 {
		a := leaseOne(t, c, "w")
		if a.Attempt != i+1 {
			t.Fatalf("attempt = %d, want %d", a.Attempt, i+1)
		}
		if err := c.Fail(ctx, FailRequest{WorkerID: "w", TaskID: a.Task.ID, LeaseID: a.LeaseID, Error: "boom"}); err != nil {
			t.Fatal(err)
		}
	}
	dead := mustState(t, c, "flaky", StateDead)
	if !strings.Contains(dead.DeadReason, "after 2 attempts") {
		t.Fatalf("dead reason = %q", dead.DeadReason)
	}
	child := mustState(t, c, "child", StateCanceled)
	if !strings.Contains(child.LastError, "flaky") {
		t.Fatalf("child error = %q", child.LastError)
	}
	dlq, err := c.DeadLetters(ctx, "", 10)
	if err != nil || len(dlq) != 1 || dlq[0].ID != "flaky" {
		t.Fatalf("dlq = %v, %v", dlq, err)
	}
	if leaseN(t, c, "w", 1) != nil {
		t.Fatalf("dead task was delivered")
	}

	// Redrive gives it a fresh budget.
	if _, err := c.Redrive(ctx, "flaky"); err != nil {
		t.Fatal(err)
	}
	a := leaseOne(t, c, "w")
	if a.Attempt != 1 {
		t.Fatalf("attempt after redrive = %d", a.Attempt)
	}
	mustComplete(t, c, "w", a)
	_, err = c.Redrive(ctx, "flaky")
	wantErr(t, err, ErrConflict)

	// Permanent failures skip the retries; expiries count as attempts.
	mustSubmit(t, c, TaskSpec{ID: "bad-input", MaxAttempts: 5})
	a = leaseOne(t, c, "w")
	if err := c.Fail(ctx, FailRequest{WorkerID: "w", TaskID: a.Task.ID, LeaseID: a.LeaseID, Error: "malformed", Permanent: true}); err != nil {
		t.Fatal(err)
	}
	if d := mustState(t, c, "bad-input", StateDead); !strings.HasPrefix(d.DeadReason, "permanent") {
		t.Fatalf("dead reason = %q", d.DeadReason)
	}
	mustSubmit(t, c, TaskSpec{ID: "hangs", MaxAttempts: 1, LeaseTimeout: Duration(time.Second)})
	leaseOne(t, c, "w")
	clk.Advance(2 * time.Second)
	c.Tick()
	mustState(t, c, "hangs", StateDead)
	if m := c.Metrics(ctx); m.DLQ != 2 || m.Counters.DeadLettered != 3 {
		t.Fatalf("dlq=%d counters=%+v", m.DLQ, m.Counters)
	}
}

func TestRetryBackoffAndRetryAfter(t *testing.T) {
	clk := NewFakeClock(t0)
	cfg := testConfig("", clk)
	cfg.RetryBackoffBase = 10 * time.Second
	cfg.RetryBackoffMax = time.Minute
	c := openTest(t, cfg)
	ctx := context.Background()
	mustRegister(t, c, "w", Capabilities{})
	mustSubmit(t, c, TaskSpec{ID: "r", MaxAttempts: 5})
	a := leaseOne(t, c, "w")
	c.Fail(ctx, FailRequest{WorkerID: "w", TaskID: "r", LeaseID: a.LeaseID, Error: "x"})
	mustState(t, c, "r", StateScheduled)
	clk.Advance(4 * time.Second)
	if leaseN(t, c, "w", 1) != nil {
		t.Fatal("retried before the backoff's lower bound")
	}
	clk.Advance(7 * time.Second)
	a = leaseOne(t, c, "w")
	c.Fail(ctx, FailRequest{WorkerID: "w", TaskID: "r", LeaseID: a.LeaseID, Error: "x", RetryAfter: Duration(30 * time.Second)})
	clk.Advance(29 * time.Second)
	if leaseN(t, c, "w", 1) != nil {
		t.Fatal("retried before RetryAfter")
	}
	clk.Advance(time.Second)
	leaseOne(t, c, "w")
}

func TestFairQueuingProportions(t *testing.T) {
	clk := NewFakeClock(t0)
	cfg := testConfig("", clk)
	cfg.TenantWeights = map[string]int{"big": 3}
	c := openTest(t, cfg)
	mustRegister(t, c, "w", Capabilities{MaxConcurrency: 4096})
	var specs []TaskSpec
	for i := range 400 {
		for _, tenant := range []string{"big", "small", "tiny"} {
			specs = append(specs, TaskSpec{Tenant: tenant, Kind: "k", ID: fmt.Sprintf("%s-%d", tenant, i)})
		}
	}
	if _, err := c.SubmitBatch(context.Background(), specs); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for range 500 {
		a := leaseOne(t, c, "w")
		counts[a.Task.Tenant]++
	}
	// Weights 3:1:1 over 500 dispatches: exactly 300/100/100 for DRR
	// with unit costs, allowing one round of slack.
	for tenant, want := range map[string]int{"big": 300, "small": 100, "tiny": 100} {
		if d := counts[tenant] - want; d < -5 || d > 5 {
			t.Fatalf("tenant %s got %d of 500, want ~%d (all: %v)", tenant, counts[tenant], want, counts)
		}
	}

	// A tenant whose tasks no worker in the poll can run does not hold up
	// the others, and keeps its turn for capable workers.
	c2 := openTest(t, testConfig("", clk))
	mustRegister(t, c2, "cpu", Capabilities{MaxConcurrency: 1000})
	mustRegister(t, c2, "gpu", Capabilities{MaxConcurrency: 1000, Labels: map[string]string{"gpu": "a100"}})
	for i := range 50 {
		mustSubmit(t, c2, TaskSpec{Tenant: "ml", Requirements: Requirements{Labels: map[string]string{"gpu": ""}}, ID: fmt.Sprintf("ml-%d", i)})
		mustSubmit(t, c2, TaskSpec{Tenant: "web", ID: fmt.Sprintf("web-%d", i)})
	}
	for range 50 {
		if a := leaseOne(t, c2, "cpu"); a.Task.Tenant != "web" {
			t.Fatalf("cpu worker got %s", a.Task.ID)
		}
	}
	if as := leaseN(t, c2, "cpu", 1); len(as) != 0 {
		t.Fatalf("cpu worker got gpu task %s", as[0].Task.ID)
	}
	if as := leaseN(t, c2, "gpu", 100); len(as) != 50 {
		t.Fatalf("gpu worker got %d tasks, want 50", len(as))
	}
}

func TestPriorityBands(t *testing.T) {
	clk := NewFakeClock(t0)
	c := openTest(t, testConfig("", clk))
	mustRegister(t, c, "w", Capabilities{})
	mustSubmit(t, c, TaskSpec{ID: "low", Priority: PriorityLow})
	mustSubmit(t, c, TaskSpec{ID: "normal-1", Priority: PriorityNormal})
	mustSubmit(t, c, TaskSpec{ID: "normal-2", Priority: PriorityNormal})
	mustSubmit(t, c, TaskSpec{ID: "crit", Priority: PriorityCritical})
	mustSubmit(t, c, TaskSpec{ID: "high", Priority: PriorityHigh})
	var order []string
	for range 5 {
		order = append(order, leaseOne(t, c, "w").Task.ID)
	}
	if got := strings.Join(order, ","); got != "crit,high,normal-1,normal-2,low" {
		t.Fatalf("dispatch order = %s", got)
	}
	if p, err := ParsePriority("HIGH"); err != nil || p != PriorityHigh {
		t.Fatalf("ParsePriority = %v, %v", p, err)
	}
}

func TestCapabilityMatching(t *testing.T) {
	caps := Capabilities{OS: "linux", Arch: "amd64", Tools: []string{"git", "go"}, Labels: map[string]string{"region": "eu", "tier": "prod"}}
	for _, tc := range []struct {
		req  Requirements
		want bool
	}{
		{Requirements{}, true},
		{Requirements{OS: "Linux"}, true},
		{Requirements{OS: "darwin"}, false},
		{Requirements{Arch: "arm64"}, false},
		{Requirements{Tools: []string{"git"}}, true},
		{Requirements{Tools: []string{"git", "docker"}}, false},
		{Requirements{Labels: map[string]string{"region": "eu"}}, true},
		{Requirements{Labels: map[string]string{"region": "us"}}, false},
		{Requirements{Labels: map[string]string{"tier": ""}}, true},
		{Requirements{Labels: map[string]string{"gpu": ""}}, false},
		{Requirements{Labels: map[string]string{"tier": "!prod"}}, false},
		{Requirements{Labels: map[string]string{"tier": "!dev"}}, true},
		{Requirements{Labels: map[string]string{"gpu": "!yes"}}, true},
	} {
		if got := caps.Satisfies(tc.req); got != tc.want {
			t.Errorf("Satisfies(%+v) = %v, want %v", tc.req, got, tc.want)
		}
	}

	clk := NewFakeClock(t0)
	c := openTest(t, testConfig("", clk))
	mustRegister(t, c, "mac", Capabilities{OS: "darwin", Arch: "arm64", Tools: []string{"git"}})
	mustRegister(t, c, "bare", Capabilities{OS: "linux", Arch: "amd64"})
	mustRegister(t, c, "full", caps)
	mustSubmit(t, c, TaskSpec{ID: "needs-linux-git", Requirements: Requirements{OS: "linux", Tools: []string{"git"}}})
	if leaseN(t, c, "mac", 1) != nil || leaseN(t, c, "bare", 1) != nil {
		t.Fatal("task leased to an incapable worker")
	}
	if a := leaseOne(t, c, "full"); a.Task.ID != "needs-linux-git" {
		t.Fatalf("got %s", a.Task.ID)
	}
	// Concurrency limits are enforced.
	mustRegister(t, c, "single", Capabilities{MaxConcurrency: 1})
	mustSubmit(t, c, TaskSpec{ID: "a"})
	mustSubmit(t, c, TaskSpec{ID: "b"})
	if as := leaseN(t, c, "single", 5); len(as) != 1 {
		t.Fatalf("single-slot worker got %d tasks", len(as))
	}
	if as := leaseN(t, c, "single", 5); len(as) != 0 {
		t.Fatalf("worker over its concurrency limit")
	}
}

func TestDelayedAndScheduledTasks(t *testing.T) {
	clk := NewFakeClock(t0)
	c := openTest(t, testConfig("", clk))
	mustRegister(t, c, "w", Capabilities{})
	mustSubmit(t, c, TaskSpec{ID: "later", Delay: Duration(time.Minute)})
	mustSubmit(t, c, TaskSpec{ID: "at", NotBefore: t0.Add(2 * time.Minute)})
	mustState(t, c, "later", StateScheduled)
	if leaseN(t, c, "w", 5) != nil {
		t.Fatal("delayed task dispatched early")
	}
	clk.Advance(time.Minute)
	a := leaseOne(t, c, "w")
	if a.Task.ID != "later" {
		t.Fatalf("got %s", a.Task.ID)
	}
	mustComplete(t, c, "w", a)
	clk.Advance(time.Minute)
	if a := leaseOne(t, c, "w"); a.Task.ID != "at" {
		t.Fatalf("got %s", a.Task.ID)
	}
}

func TestLongPollWakesOnSubmit(t *testing.T) {
	c := openTest(t, testConfig("", RealClock()))
	mustRegister(t, c, "w", Capabilities{})
	done := make(chan LeaseResponse, 1)
	go func() {
		r, err := c.Lease(context.Background(), LeaseRequest{WorkerID: "w", Max: 1, Wait: Duration(10 * time.Second)})
		if err != nil {
			t.Error(err)
		}
		done <- r
	}()
	time.Sleep(20 * time.Millisecond)
	start := time.Now()
	mustSubmit(t, c, TaskSpec{ID: "wake"})
	select {
	case r := <-done:
		if len(r.Assignments) != 1 || time.Since(start) > 2*time.Second {
			t.Fatalf("long poll returned %+v after %v", r, time.Since(start))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("long poll did not wake on submit")
	}

	// A long poll times out empty, and honours cancellation.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := c.Lease(ctx, LeaseRequest{WorkerID: "w", Max: 1, Wait: Duration(10 * time.Second)})
	wantErr(t, err, context.DeadlineExceeded)
}

func TestDependencies(t *testing.T) {
	clk := NewFakeClock(t0)
	c := openTest(t, testConfig("", clk))
	ctx := context.Background()
	mustRegister(t, c, "w", Capabilities{})
	if _, err := c.SubmitBatch(ctx, []TaskSpec{
		{ID: "schema", Kind: "k"},
		{ID: "backfill", Kind: "k", DependsOn: []string{"schema"}},
		{ID: "verify", Kind: "k", DependsOn: []string{"backfill", "schema"}},
		{ID: "cleanup", Kind: "k", DependsOn: []string{"verify"}},
	}); err != nil {
		t.Fatal(err)
	}
	mustState(t, c, "backfill", StatePending)
	a := leaseOne(t, c, "w")
	if a.Task.ID != "schema" || leaseN(t, c, "w", 5) != nil {
		t.Fatal("dependent dispatched before its dependency finished")
	}
	mustComplete(t, c, "w", a)
	mustState(t, c, "backfill", StateReady)
	mustState(t, c, "verify", StatePending)
	a = leaseOne(t, c, "w")
	mustComplete(t, c, "w", a)
	a = leaseOne(t, c, "w")
	if a.Task.ID != "verify" {
		t.Fatalf("got %s", a.Task.ID)
	}
	if err := c.Fail(ctx, FailRequest{WorkerID: "w", TaskID: "verify", LeaseID: a.LeaseID, Error: "mismatch", Permanent: true}); err != nil {
		t.Fatal(err)
	}
	mustState(t, c, "cleanup", StateCanceled)

	_, err := c.Submit(ctx, TaskSpec{Kind: "k", DependsOn: []string{"nope"}})
	wantErr(t, err, ErrUnknownDependency)
	_, err = c.Submit(ctx, TaskSpec{Kind: "k", DependsOn: []string{"verify"}})
	wantErr(t, err, ErrDependencyFailed)
	_, err = c.Submit(ctx, TaskSpec{ID: "self", Kind: "k", DependsOn: []string{"self"}})
	wantErr(t, err, ErrInvalid)
}

func TestCancellation(t *testing.T) {
	clk := NewFakeClock(t0)
	c := openTest(t, testConfig("", clk))
	ctx := context.Background()
	mustRegister(t, c, "w", Capabilities{})
	mustSubmit(t, c, TaskSpec{ID: "run"})
	mustSubmit(t, c, TaskSpec{ID: "after", DependsOn: []string{"run"}})
	mustSubmit(t, c, TaskSpec{ID: "queued", Priority: PriorityLow})
	a := leaseOne(t, c, "w")

	if _, err := c.Cancel(ctx, "run", "operator abort"); err != nil {
		t.Fatal(err)
	}
	mustState(t, c, "run", StateCanceled)
	mustState(t, c, "after", StateCanceled)
	hb, err := c.Heartbeat(ctx, HeartbeatRequest{WorkerID: "w", Leases: []LeaseRef{{"run", a.LeaseID}}})
	if err != nil || len(hb.Revoked) != 1 {
		t.Fatalf("heartbeat = %+v, %v", hb, err)
	}
	wantErr(t, c.Complete(ctx, CompleteRequest{WorkerID: "w", TaskID: "run", LeaseID: a.LeaseID}), ErrLeaseLost)
	if _, err := c.Cancel(ctx, "run", ""); err != nil {
		t.Fatalf("second cancel: %v", err)
	}
	if _, err := c.Cancel(ctx, "queued", ""); err != nil {
		t.Fatal(err)
	}
	if leaseN(t, c, "w", 5) != nil {
		t.Fatal("canceled task dispatched")
	}
	mustSubmit(t, c, TaskSpec{ID: "fin"})
	mustComplete(t, c, "w", leaseOne(t, c, "w"))
	_, err = c.Cancel(ctx, "fin", "")
	wantErr(t, err, ErrConflict)
	_, err = c.Cancel(ctx, "missing", "")
	wantErr(t, err, ErrNotFound)
}

func TestReleaseDoesNotCountAttempt(t *testing.T) {
	clk := NewFakeClock(t0)
	c := openTest(t, testConfig("", clk))
	mustRegister(t, c, "w", Capabilities{})
	mustSubmit(t, c, TaskSpec{ID: "x", MaxAttempts: 1})
	for range 3 {
		a := leaseOne(t, c, "w")
		if err := c.Release(context.Background(), ReleaseRequest{WorkerID: "w", TaskID: "x", LeaseID: a.LeaseID}); err != nil {
			t.Fatal(err)
		}
	}
	if got := mustState(t, c, "x", StateReady); got.Attempts != 0 {
		t.Fatalf("attempts = %d", got.Attempts)
	}
}

func TestWorkerLostDrainAndDeregister(t *testing.T) {
	clk := NewFakeClock(t0)
	cfg := testConfig("", clk)
	cfg.WorkerTTL = 30 * time.Second
	c := openTest(t, cfg)
	ctx := context.Background()
	mustRegister(t, c, "w1", Capabilities{})
	mustRegister(t, c, "w2", Capabilities{})
	mustSubmit(t, c, TaskSpec{ID: "t1", LeaseTimeout: Duration(time.Hour)})
	leaseOne(t, c, "w1")

	// w1 goes silent: after WorkerTTL its lease is reclaimed without
	// waiting for the hour-long visibility timeout.
	clk.Advance(20 * time.Second)
	c.Heartbeat(ctx, HeartbeatRequest{WorkerID: "w2"})
	clk.Advance(15 * time.Second)
	c.Tick()
	mustState(t, c, "t1", StateReady)
	_, err := c.Heartbeat(ctx, HeartbeatRequest{WorkerID: "w1"})
	wantErr(t, err, ErrUnknownWorker)
	ws, _ := c.Workers(ctx)
	if ws[0].State != WorkerLost || ws[0].Flaps != 1 {
		t.Fatalf("w1 status = %+v", ws[0])
	}
	mustRegister(t, c, "w1", Capabilities{})

	// Drain stops new leases but keeps existing ones alive.
	a := leaseOne(t, c, "w2")
	if err := c.Drain(ctx, "w2"); err != nil {
		t.Fatal(err)
	}
	mustSubmit(t, c, TaskSpec{ID: "t2"})
	r, err := c.Lease(ctx, LeaseRequest{WorkerID: "w2", Max: 1})
	if err != nil || !r.Draining || len(r.Assignments) != 0 {
		t.Fatalf("lease while draining = %+v, %v", r, err)
	}
	hb, err := c.Heartbeat(ctx, HeartbeatRequest{WorkerID: "w2", Leases: []LeaseRef{{"t1", a.LeaseID}}})
	if err != nil || hb.Extended != 1 || !hb.Draining {
		t.Fatalf("heartbeat while draining = %+v, %v", hb, err)
	}
	// Deregistering returns the lease without spending the attempt.
	if err := c.Deregister(ctx, "w2"); err != nil {
		t.Fatal(err)
	}
	if got := mustState(t, c, "t1", StateReady); got.Attempts != 1 {
		t.Fatalf("attempts after deregister = %d, want 1", got.Attempts)
	}

	// A new incarnation of a worker reclaims the old process's leases.
	a = leaseOne(t, c, "w1")
	if _, err := c.Register(ctx, RegisterRequest{WorkerID: "w1", Incarnation: "rebooted", Capabilities: Capabilities{MaxConcurrency: 4}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get(ctx, a.Task.ID); got.State == StateLeased {
		t.Fatalf("lease of the dead incarnation survived re-registration")
	}
}

func TestQuarantine(t *testing.T) {
	clk := NewFakeClock(t0)
	cfg := testConfig("", clk)
	cfg.Health = HealthPolicy{Alpha: 0.5, MinSamples: 3, QuarantineBelow: 0.3, MaxFlaps: 3, FlapWindow: time.Hour, QuarantineBase: time.Minute}
	c := openTest(t, cfg)
	ctx := context.Background()
	mustRegister(t, c, "bad", Capabilities{})
	mustRegister(t, c, "good", Capabilities{})
	for i := range 10 {
		mustSubmit(t, c, TaskSpec{ID: fmt.Sprintf("t%d", i), MaxAttempts: 10})
	}
	for range 3 {
		a := leaseOne(t, c, "bad")
		if err := c.Fail(ctx, FailRequest{WorkerID: "bad", TaskID: a.Task.ID, LeaseID: a.LeaseID, Error: "disk full"}); err != nil {
			t.Fatal(err)
		}
	}
	r, err := c.Lease(ctx, LeaseRequest{WorkerID: "bad", Max: 1})
	if err != nil || !r.Quarantined || len(r.Assignments) != 0 {
		t.Fatalf("lease for failing worker = %+v, %v", r, err)
	}
	leaseOne(t, c, "good")
	if m := c.Metrics(ctx); m.Counters.Quarantines != 1 {
		t.Fatalf("quarantines = %d", m.Counters.Quarantines)
	}
	clk.Advance(time.Minute)
	leaseOne(t, c, "bad") // probation

	// Permanent failures are the task's fault and do not count.
	mustRegister(t, c, "strict", Capabilities{})
	for range 4 {
		mustSubmit(t, c, TaskSpec{Kind: "k"})
		a := leaseOne(t, c, "strict")
		c.Fail(ctx, FailRequest{WorkerID: "strict", TaskID: a.Task.ID, LeaseID: a.LeaseID, Error: "bad input", Permanent: true})
	}
	if r, _ := c.Lease(ctx, LeaseRequest{WorkerID: "strict"}); r.Quarantined {
		t.Fatal("worker quarantined for permanent task failures")
	}

	// A worker that keeps restarting is quarantined as flapping, with
	// doubling durations.
	for i := range 4 {
		if _, err := c.Register(ctx, RegisterRequest{WorkerID: "flappy", Incarnation: fmt.Sprint(i), Capabilities: Capabilities{MaxConcurrency: 1}}); err != nil {
			t.Fatal(err)
		}
	}
	r, _ = c.Lease(ctx, LeaseRequest{WorkerID: "flappy"})
	if !r.Quarantined {
		t.Fatalf("flapping worker not quarantined: %+v", r)
	}
	ws, _ := c.Workers(ctx)
	for _, w := range ws {
		if w.ID == "flappy" && w.State != WorkerQuarantined {
			t.Fatalf("flappy state = %s", w.State)
		}
	}
}

func TestRetentionPurge(t *testing.T) {
	clk := NewFakeClock(t0)
	cfg := testConfig(t.TempDir(), clk)
	cfg.Retention = time.Hour
	c := openTest(t, cfg)
	ctx := context.Background()
	mustRegister(t, c, "w", Capabilities{})
	mustSubmit(t, c, TaskSpec{ID: "old", IdempotencyKey: "k"})
	mustComplete(t, c, "w", leaseOne(t, c, "w"))
	clk.Advance(2 * time.Hour)
	c.Tick()
	_, err := c.Get(ctx, "old")
	wantErr(t, err, ErrNotFound)
	r, err := c.Submit(ctx, TaskSpec{Kind: "test", IdempotencyKey: "k"})
	if err != nil || r.Duplicate {
		t.Fatalf("idempotency key not released after retention: %+v %v", r, err)
	}
	// Purges are durable.
	c.Close()
	c2 := openTest(t, cfg)
	_, err = c2.Get(ctx, "old")
	wantErr(t, err, ErrNotFound)
}

func TestValidation(t *testing.T) {
	c := openTest(t, testConfig("", NewFakeClock(t0)))
	ctx := context.Background()
	for _, spec := range []TaskSpec{
		{},
		{Kind: "k", Tenant: "bad tenant"},
		{Kind: "k", ID: "has space"},
		{Kind: "k", Priority: 9},
		{Kind: "k", MaxAttempts: -1},
		{Kind: "k", LeaseTimeout: Duration(time.Nanosecond)},
		{Kind: "k", Delay: -1},
		{Kind: "k", Payload: make([]byte, DefaultMaxPayloadBytes+1)},
	} {
		_, err := c.Submit(ctx, spec)
		wantErr(t, err, ErrInvalid)
	}
	_, err := c.Lease(ctx, LeaseRequest{WorkerID: "nobody"})
	wantErr(t, err, ErrUnknownWorker)
	_, err = c.Register(ctx, RegisterRequest{Capabilities: Capabilities{MaxConcurrency: -1}})
	wantErr(t, err, ErrInvalid)
	f, _ := New(testConfig("", nil))
	_, err = f.Submit(ctx, TaskSpec{Kind: "k"})
	wantErr(t, err, ErrNotLeader)
}
