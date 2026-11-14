package fleet

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testLeaseStore(t *testing.T, name string, mk func(Clock) LeaseStore) {
	t.Run(name, func(t *testing.T) {
		ctx := context.Background()
		clk := NewFakeClock(t0)
		s := mk(clk)
		a, err := s.Acquire(ctx, "fleet", "a", "http://a", 10*time.Second)
		if err != nil || a.Token != 1 {
			t.Fatalf("acquire: %+v %v", a, err)
		}
		if _, err := s.Acquire(ctx, "fleet", "b", "http://b", 10*time.Second); err != ErrLeaseHeld {
			t.Fatalf("second holder: %v", err)
		}
		a2, err := s.Acquire(ctx, "fleet", "a", "http://a", 10*time.Second)
		if err != nil || a2.Token != 1 {
			t.Fatalf("re-acquire by holder changed token: %+v %v", a2, err)
		}
		clk.Advance(5 * time.Second)
		if a, err = s.Renew(ctx, a, 10*time.Second); err != nil || !a.Expires.Equal(t0.Add(15*time.Second)) {
			t.Fatalf("renew: %+v %v", a, err)
		}
		if _, err := s.CheckFence(ctx, "fleet", 1); err != nil {
			t.Fatalf("fence: %v", err)
		}
		clk.Advance(11 * time.Second)
		if _, err := s.CheckFence(ctx, "fleet", 1); err == nil {
			t.Fatal("expired lease passed the fence")
		}
		b, err := s.Acquire(ctx, "fleet", "b", "http://b", 10*time.Second)
		if err != nil || b.Token != 2 {
			t.Fatalf("takeover: %+v %v", b, err)
		}
		if _, err := s.Renew(ctx, a, 10*time.Second); err != ErrLeaseLost {
			t.Fatalf("stale renew: %v", err)
		}
		_, err = s.CheckFence(ctx, "fleet", 1)
		wantErr(t, err, ErrStaleFencingToken)
		if err := s.Release(ctx, b); err != nil {
			t.Fatal(err)
		}
		if g, _ := s.Get(ctx, "fleet"); g.Holder != "" || g.Token != 2 {
			t.Fatalf("after release: %+v", g)
		}
		c, _ := s.Acquire(ctx, "fleet", "a", "http://a", 10*time.Second)
		if c.Token != 3 {
			t.Fatalf("tokens must stay monotonic across release: %d", c.Token)
		}

		// Concurrent campaigns: exactly one winner.
		var wins atomic.Int32
		var wg sync.WaitGroup
		for i := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := s.Acquire(ctx, "race", fmt.Sprint(i), "", time.Minute); err == nil {
					wins.Add(1)
				}
			}()
		}
		wg.Wait()
		if wins.Load() != 1 {
			t.Fatalf("%d concurrent winners", wins.Load())
		}
	})
}

func TestLeaseStores(t *testing.T) {
	testLeaseStore(t, "memory", func(c Clock) LeaseStore { return NewMemLeaseStore(c) })
	testLeaseStore(t, "file", func(c Clock) LeaseStore {
		s, err := NewFileLeaseStore(t.TempDir(), c)
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

func TestFileLeaseStoreBreaksStaleLock(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewFileLeaseStore(dir, nil)
	lock := filepath.Join(dir, "fleet.lock")
	os.WriteFile(lock, []byte("crashed"), 0o600)
	old := time.Now().Add(-time.Minute)
	os.Chtimes(lock, old, old)
	if _, err := s.Acquire(context.Background(), "fleet", "a", "", time.Second); err != nil {
		t.Fatalf("stale lock not broken: %v", err)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatal("lock file left behind")
	}
}

// TestLeaderFailoverFencing: leader A pauses past its lease, B takes over
// with a higher token, and A's late writes are rejected and never reach
// the shared log.
func TestLeaderFailoverFencing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	clk := NewFakeClock(t0)
	store, err := NewFileLeaseStore(filepath.Join(dir, "leases"), clk)
	if err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(dir, "state")

	la, _ := store.Acquire(ctx, "fleet", "a", "http://a", 10*time.Second)
	cfgA := testConfig(stateDir, clk)
	cfgA.NodeID = "a"
	a, _ := New(cfgA)
	defer a.Close()
	if err := a.Promote(ctx, NewStoreFencer(store, la, clk)); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		mustSubmit(t, a, TaskSpec{ID: fmt.Sprintf("a%d", i)})
	}

	// A stalls (GC pause, partition); its lease lapses and B wins.
	clk.Advance(11 * time.Second)
	lb, err := store.Acquire(ctx, "fleet", "b", "http://b", 10*time.Second)
	if err != nil || lb.Token != 2 {
		t.Fatalf("b acquire: %+v %v", lb, err)
	}
	cfgB := cfgA
	cfgB.NodeID = "b"
	b, _ := New(cfgB)
	defer b.Close()
	if err := b.Promote(ctx, NewStoreFencer(store, lb, clk)); err != nil {
		t.Fatal(err)
	}
	mustState(t, b, "a2", StateReady)
	mustSubmit(t, b, TaskSpec{ID: "b0"})

	// A wakes up and tries to write.
	_, err = a.Submit(ctx, TaskSpec{ID: "zombie", Kind: "k"})
	wantErr(t, err, ErrStaleFencingToken)
	wantErr(t, err, ErrNotLeader)
	if leader, _ := a.IsLeader(); leader {
		t.Fatal("stale leader still believes it leads")
	}
	if a.Metrics(ctx).Leader {
		t.Fatal("stale leader reports leadership")
	}
	// Re-promoting with the old lease is refused too.
	wantErr(t, a.Promote(ctx, NewStoreFencer(store, la, clk)), ErrStaleFencingToken)

	// The shared log holds exactly B's view.
	b.Close()
	lc, _ := store.Acquire(ctx, "fleet", "c", "", 10*time.Second)
	_ = lc
	_, rec, err := OpenWAL(stateDir, WALOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Tasks["zombie"] != nil || rec.Tasks["b0"] == nil || len(rec.Tasks) != 4 || rec.MaxToken != 2 {
		t.Fatalf("log after failover: %d tasks, max token %d", len(rec.Tasks), rec.MaxToken)
	}
	// A coordinator with an older token cannot promote over the log.
	old, _ := New(cfgA)
	wantErr(t, old.Promote(ctx, staticFencer(1)), ErrStaleFencingToken)
}

type staticFencer uint64

func (s staticFencer) Token() uint64                 { return uint64(s) }
func (s staticFencer) Check(_ context.Context) error { return nil }

// TestElectionFailover runs two coordinators with real elections: when the
// leader stops, the follower takes over with the queue intact.
func TestElectionFailover(t *testing.T) {
	dir := t.TempDir()
	store := NewMemLeaseStore(nil)
	type node struct {
		c      *Coordinator
		cancel context.CancelFunc
		done   chan struct{}
	}
	start := func(id string) node {
		cfg := testConfig(dir, nil)
		cfg.NodeID = id
		cfg.TickInterval = 10 * time.Millisecond
		c, _ := New(cfg)
		e, err := NewElector(ElectorConfig{Store: store, Name: "fleet", Holder: id, Address: "http://" + id, TTL: 300 * time.Millisecond, RetryEvery: 20 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			c.RunWithElection(ctx, e)
		}()
		return node{c, cancel, done}
	}
	n1 := start("n1")
	waitFor(t, 5*time.Second, "n1 leads", func() bool { l, _ := n1.c.IsLeader(); return l })
	n2 := start("n2")
	defer func() { n2.cancel(); <-n2.done; n2.c.Close() }()
	if hint := n2.c.LeaderHint(context.Background()); hint != "http://n1" {
		t.Fatalf("leader hint = %q", hint)
	}
	for i := range 5 {
		mustSubmit(t, n1.c, TaskSpec{ID: fmt.Sprintf("t%d", i)})
	}
	n1.cancel()
	<-n1.done
	n1.c.Close()
	waitFor(t, 5*time.Second, "n2 leads", func() bool { l, _ := n2.c.IsLeader(); return l })
	_, tok := n2.c.IsLeader()
	if tok != 2 {
		t.Fatalf("n2 token = %d", tok)
	}
	got, err := n2.c.List(context.Background(), ListFilter{})
	if err != nil || len(got) != 5 {
		t.Fatalf("n2 sees %d tasks: %v", len(got), err)
	}
}
