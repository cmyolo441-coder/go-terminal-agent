package fleet

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestCoordinatorCrashRecovery(t *testing.T) {
	dir := t.TempDir()
	clk := NewFakeClock(t0)
	cfg := testConfig(dir, clk)
	cfg.SnapshotEvery = 25 // force several compactions mid-run
	ctx := context.Background()

	c1, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	mustRegister(t, c1, "w1", Capabilities{})
	for i := range 60 {
		spec := TaskSpec{ID: fmt.Sprintf("t%02d", i), Tenant: []string{"a", "b"}[i%2], IdempotencyKey: fmt.Sprintf("k%d", i), MaxAttempts: 2}
		if i%10 == 9 {
			spec.DependsOn = []string{fmt.Sprintf("t%02d", i-1)}
		}
		if i%15 == 0 {
			spec.Delay = Duration(time.Hour)
		}
		mustSubmit(t, c1, spec)
		c1.Tick()
	}
	leased := leaseN(t, c1, "w1", 20)
	for _, a := range leased[:8] {
		mustComplete(t, c1, "w1", a)
	}
	c1.Fail(ctx, FailRequest{WorkerID: "w1", TaskID: leased[8].Task.ID, LeaseID: leased[8].LeaseID, Error: "x", Permanent: true})
	c1.Cancel(ctx, "t30", "")
	c1.Tick()

	before := map[string]*Task{}
	all, _ := c1.List(ctx, ListFilter{Limit: 1000})
	for _, task := range all {
		before[task.ID] = task
	}
	// Crash: the process dies without a clean shutdown, leaving a torn
	// record at the end of the log.
	seg := lastSegment(t, dir)
	f, _ := os.OpenFile(seg, os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString(`3f2a1b0c {"seq":9999,"tok":0,"op":"put","task":{"id":"half-writ`)
	f.Close()
	c1.Close()

	clk.Advance(time.Second)
	c2 := openTest(t, cfg)
	after, _ := c2.List(ctx, ListFilter{Limit: 1000})
	if len(after) != len(before) {
		t.Fatalf("recovered %d tasks, want %d", len(after), len(before))
	}
	for _, got := range after {
		want := before[got.ID]
		if want == nil || got.State != want.State || got.Attempts != want.Attempts || got.Version != want.Version {
			t.Fatalf("task %s recovered as %s/%d/v%d, want %+v", got.ID, got.State, got.Attempts, got.Version, want)
		}
	}
	// Indexes are rebuilt: idempotency keys, dependencies, schedules and
	// leases all still work.
	r, _ := c2.Submit(ctx, TaskSpec{Kind: "test", Tenant: "a", IdempotencyKey: "k0"})
	if !r.Duplicate {
		t.Fatal("idempotency key lost in recovery")
	}
	// The worker re-registers with the new process and finishes its
	// in-flight leases with the lease IDs it already had.
	mustRegister(t, c2, "w1", Capabilities{})
	for _, a := range leased[9:] {
		if err := c2.Complete(ctx, CompleteRequest{WorkerID: "w1", TaskID: a.Task.ID, LeaseID: a.LeaseID}); err != nil {
			t.Fatalf("complete recovered lease %s: %v", a.Task.ID, err)
		}
	}
	// Drain everything else to completion.
	clk.Advance(2 * time.Hour)
	for range 200 {
		as := leaseN(t, c2, "w1", 50)
		if len(as) == 0 {
			break
		}
		for _, a := range as {
			mustComplete(t, c2, "w1", a)
		}
	}
	m := c2.Metrics(ctx)
	if m.States[StateReady]+m.States[StatePending]+m.States[StateScheduled]+m.States[StateLeased] != 0 {
		t.Fatalf("unfinished tasks after drain: %v", m.States)
	}
	if m.States[StateDead] != 1 || m.States[StateCanceled] < 1 {
		t.Fatalf("states = %v", m.States)
	}
}

func TestRecoveryRepairsInterruptedCascade(t *testing.T) {
	// A crash between a dependency's completion and the release of its
	// dependents is repaired when the log is replayed.
	dir := t.TempDir()
	w, _, _ := OpenWAL(dir, WALOptions{})
	w.Append(0, opPut, &Task{ID: "parent", Kind: "k", Tenant: "d", State: StateSucceeded, Seq: 1, MaxAttempts: 1, LeaseTimeout: Duration(time.Second), CompletedAt: t0}, "")
	w.Append(0, opPut, &Task{ID: "child", Kind: "k", Tenant: "d", State: StatePending, Seq: 2, DependsOn: []string{"parent"}, MaxAttempts: 1, LeaseTimeout: Duration(time.Second)}, "")
	w.Append(0, opPut, &Task{ID: "dead", Kind: "k", Tenant: "d", State: StateDead, Seq: 3, MaxAttempts: 1, LeaseTimeout: Duration(time.Second), CompletedAt: t0}, "")
	w.Append(0, opPut, &Task{ID: "orphan", Kind: "k", Tenant: "d", State: StatePending, Seq: 4, DependsOn: []string{"dead"}, MaxAttempts: 1, LeaseTimeout: Duration(time.Second)}, "")
	w.Close()
	c := openTest(t, testConfig(dir, NewFakeClock(t0)))
	mustState(t, c, "child", StateReady)
	mustState(t, c, "orphan", StateCanceled)
}
