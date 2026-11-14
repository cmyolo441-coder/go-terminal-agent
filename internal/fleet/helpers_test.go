package fleet

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

var t0 = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

func testConfig(dir string, clk Clock) Config {
	return Config{
		NodeID:              "c1",
		Dir:                 dir,
		Clock:               clk,
		DefaultLeaseTimeout: 10 * time.Second,
		RetryBackoffBase:    -1,
		WorkerTTL:           time.Minute,
		Health:              HealthPolicy{Disabled: true},
	}
}

func openTest(t *testing.T, cfg Config) *Coordinator {
	t.Helper()
	c, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func mustRegister(t *testing.T, c *Coordinator, id string, caps Capabilities) {
	t.Helper()
	if caps.MaxConcurrency == 0 {
		caps.MaxConcurrency = 100
	}
	if _, err := c.Register(context.Background(), RegisterRequest{WorkerID: id, Incarnation: "inc-" + id, Capabilities: caps}); err != nil {
		t.Fatalf("Register %s: %v", id, err)
	}
}

func mustSubmit(t *testing.T, c *Coordinator, spec TaskSpec) *Task {
	t.Helper()
	if spec.Kind == "" {
		spec.Kind = "test"
	}
	r, err := c.Submit(context.Background(), spec)
	if err != nil {
		t.Fatalf("Submit %+v: %v", spec, err)
	}
	return r.Task
}

// leaseN leases up to n tasks without waiting.
func leaseN(t *testing.T, c *Coordinator, worker string, n int) []Assignment {
	t.Helper()
	r, err := c.Lease(context.Background(), LeaseRequest{WorkerID: worker, Max: n})
	if err != nil {
		t.Fatalf("Lease %s: %v", worker, err)
	}
	return r.Assignments
}

func leaseOne(t *testing.T, c *Coordinator, worker string) Assignment {
	t.Helper()
	as := leaseN(t, c, worker, 1)
	if len(as) != 1 {
		t.Fatalf("Lease %s: got %d assignments, want 1", worker, len(as))
	}
	return as[0]
}

func mustComplete(t *testing.T, c *Coordinator, worker string, a Assignment) {
	t.Helper()
	if err := c.Complete(context.Background(), CompleteRequest{WorkerID: worker, TaskID: a.Task.ID, LeaseID: a.LeaseID}); err != nil {
		t.Fatalf("Complete %s: %v", a.Task.ID, err)
	}
}

func mustState(t *testing.T, c *Coordinator, id string, want TaskState) *Task {
	t.Helper()
	got, err := c.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get %s: %v", id, err)
	}
	if got.State != want {
		t.Fatalf("task %s state = %s, want %s (last error %q, dead %q)", id, got.State, want, got.LastError, got.DeadReason)
	}
	return got
}

func wantErr(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("error = %v, want %v", err, target)
	}
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func ids(n int, prefix string) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s-%04d", prefix, i)
	}
	return out
}
