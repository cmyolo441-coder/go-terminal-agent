package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	mrand "math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingAPI records which (task, lease) completions the coordinator
// accepted, so the test can prove each task's effect was committed once.
type countingAPI struct {
	WorkerAPI
	mu       sync.Mutex
	accepted map[string]map[string]bool // task -> lease IDs accepted
}

func (c *countingAPI) Complete(ctx context.Context, req CompleteRequest) error {
	err := c.WorkerAPI.Complete(ctx, req)
	if err == nil {
		c.mu.Lock()
		if c.accepted[req.TaskID] == nil {
			c.accepted[req.TaskID] = map[string]bool{}
		}
		c.accepted[req.TaskID][req.LeaseID] = true
		c.mu.Unlock()
	}
	return err
}

// TestSoakWorkerCrashes runs 2000 tasks across 8 workers while randomly
// killing workers (no drain, no acknowledgements) and restarting them.
// Every task must end succeeded with exactly one accepted completion,
// however many times it actually ran.
func TestSoakWorkerCrashes(t *testing.T) {
	const (
		nTasks   = 2000
		nWorkers = 8
	)
	cfg := testConfig(t.TempDir(), RealClock())
	cfg.DefaultLeaseTimeout = 300 * time.Millisecond
	cfg.HeartbeatInterval = 40 * time.Millisecond
	cfg.WorkerTTL = 500 * time.Millisecond
	cfg.TickInterval = 10 * time.Millisecond
	cfg.DefaultMaxAttempts = 100
	cfg.SnapshotEvery = 1000
	cfg.Health = HealthPolicy{MaxFlaps: 1000, MinSamples: 1000}
	c := openTest(t, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	api := &countingAPI{WorkerAPI: c, accepted: map[string]map[string]bool{}}
	var specs []TaskSpec
	for i := range nTasks {
		s := TaskSpec{
			ID: fmt.Sprintf("s%04d", i), Kind: "apply-migration", Tenant: []string{"alpha", "beta", "gamma"}[i%3],
			Priority: Priority(i % 3), IdempotencyKey: fmt.Sprintf("repo-%d", i),
		}
		switch i % 4 {
		case 0:
			s.Requirements.OS = "linux"
		case 1:
			s.Requirements.OS = "darwin"
		}
		specs = append(specs, s)
	}
	if _, err := c.SubmitBatch(ctx, specs); err != nil {
		t.Fatal(err)
	}

	var executions sync.Map // task -> *atomic.Int32
	exec := ExecutorFunc(func(ctx context.Context, a Assignment) (json.RawMessage, error) {
		v, _ := executions.LoadOrStore(a.Task.ID, new(atomic.Int32))
		v.(*atomic.Int32).Add(1)
		select {
		case <-time.After(time.Duration(mrand.IntN(3000)) * time.Microsecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return json.RawMessage(`"ok"`), nil
	})

	type slot struct {
		w    *Worker
		done chan struct{}
	}
	var slotsMu sync.Mutex
	slots := make([]slot, nWorkers)
	startWorker := func(i int) {
		os := "linux"
		if i%2 == 1 {
			os = "darwin"
		}
		w, err := NewWorker(WorkerConfig{
			ID: fmt.Sprintf("soak-%d", i), API: api, Executor: exec,
			Capabilities: Capabilities{OS: os, Arch: "amd64", MaxConcurrency: 4},
			PollWait:     100 * time.Millisecond, RetryMin: 5 * time.Millisecond, RetryMax: 50 * time.Millisecond,
			DrainTimeout: time.Second,
		})
		if err != nil {
			t.Error(err)
			return
		}
		done := make(chan struct{})
		go func() { defer close(done); w.Run(ctx) }()
		slotsMu.Lock()
		slots[i] = slot{w, done}
		slotsMu.Unlock()
	}
	for i := range nWorkers {
		startWorker(i)
	}

	// Chaos: crash a random worker every few milliseconds and bring it
	// back as a new process.
	var crashes atomic.Int32
	chaosDone := make(chan struct{})
	stopChaos := make(chan struct{})
	go func() {
		defer close(chaosDone)
		rng := mrand.New(mrand.NewPCG(7, 11))
		for {
			select {
			case <-stopChaos:
				return
			case <-time.After(time.Duration(20+rng.IntN(40)) * time.Millisecond):
			}
			i := rng.IntN(nWorkers)
			slotsMu.Lock()
			s := slots[i]
			slotsMu.Unlock()
			s.w.crash()
			<-s.done
			crashes.Add(1)
			startWorker(i)
		}
	}()

	deadline := time.Now().Add(90 * time.Second)
	for c.Metrics(ctx).States[StateSucceeded] < nTasks {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %v", c.Metrics(ctx).States)
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(stopChaos)
	<-chaosDone
	cancel()
	for _, s := range slots {
		<-s.done
	}

	if crashes.Load() < 5 {
		t.Fatalf("only %d crashes; the soak test did not exercise failures", crashes.Load())
	}
	m := c.Metrics(context.Background())
	if m.Counters.Succeeded != nTasks || m.States[StateSucceeded] != nTasks {
		t.Fatalf("succeeded counter %d, states %v", m.Counters.Succeeded, m.States)
	}
	ranTwice := 0
	for i := range nTasks {
		id := fmt.Sprintf("s%04d", i)
		task := mustState(t, c, id, StateSucceeded)
		if n := len(api.accepted[id]); n != 1 {
			t.Fatalf("task %s had %d accepted completions", id, n)
		}
		if task.Closure != "complete:"+firstKey(api.accepted[id]) {
			t.Fatalf("task %s committed by %s, accepted %v", id, task.Closure, api.accepted[id])
		}
		v, ok := executions.Load(id)
		if !ok {
			t.Fatalf("task %s never executed", id)
		}
		if v.(*atomic.Int32).Load() > 1 {
			ranTwice++
		}
	}
	// Duplicate submission is detected for every task.
	res, err := c.SubmitBatch(context.Background(), specs)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if !r.Duplicate {
			t.Fatalf("resubmission of %s not detected as duplicate", r.Task.ID)
		}
	}
	t.Logf("crashes=%d redeliveries=%d expiries=%d executed-more-than-once=%d snapshots=%d run p99=%v",
		crashes.Load(), m.Counters.Redeliveries, m.Counters.LeaseExpiries, ranTwice, m.Counters.Snapshots, m.RunTime.P99.D())
}

func firstKey(m map[string]bool) string {
	for k := range m {
		return k
	}
	return ""
}
