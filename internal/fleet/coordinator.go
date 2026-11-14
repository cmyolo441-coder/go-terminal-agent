package fleet

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	mrand "math/rand/v2"
	"slices"
	"sync"
	"time"
)

// Defaults for Config fields left zero.
const (
	DefaultLeaseTimeout      = 30 * time.Second
	DefaultMaxLeaseTimeout   = 6 * time.Hour
	DefaultMaxAttempts       = 3
	DefaultRetryBackoffBase  = time.Second
	DefaultRetryBackoffMax   = 5 * time.Minute
	DefaultMaxScan           = 1024
	DefaultSnapshotEvery     = 10000
	DefaultRetention         = 24 * time.Hour
	DefaultWorkerTTL         = 30 * time.Second
	DefaultHeartbeatInterval = 5 * time.Second
	DefaultMaxLongPoll       = 30 * time.Second
	DefaultTickInterval      = 200 * time.Millisecond
	DefaultMaxPayloadBytes   = 1 << 20
	DefaultMaxDependencies   = 256

	minLeaseTimeout  = 10 * time.Millisecond
	maxAttemptsLimit = 1000
	maxConcurrency   = 4096
	maxListLimit     = 1000
	purgeBatch       = 1000
)

// Config configures a Coordinator.
type Config struct {
	// NodeID names this coordinator in logs, metrics and elections.
	NodeID string
	// Dir holds the write-ahead log and snapshots. Empty means no
	// durability: state lives only in memory.
	Dir string
	// SyncWrites fsyncs every log record before acknowledging it.
	SyncWrites bool
	Clock      Clock
	Logger     *slog.Logger

	DefaultLeaseTimeout time.Duration
	MaxLeaseTimeout     time.Duration
	DefaultMaxAttempts  int
	// RetryBackoffBase is the delay before the first retry, doubling per
	// attempt up to RetryBackoffMax, with jitter. Negative disables
	// backoff: failed tasks are ready again at once.
	RetryBackoffBase time.Duration
	RetryBackoffMax  time.Duration

	// TenantWeights sets each tenant's share within a priority band
	// (default 1).
	TenantWeights map[string]int
	// MaxScan bounds how far into a tenant's queue dispatch looks for a
	// task the polling worker can run.
	MaxScan int

	// SnapshotEvery compacts the log after this many records.
	SnapshotEvery int
	// Retention keeps finished tasks (and so their idempotency keys) this
	// long before purging them.
	Retention time.Duration

	// WorkerTTL declares a worker lost, and reclaims its leases, after
	// this long without contact.
	WorkerTTL         time.Duration
	HeartbeatInterval time.Duration
	MaxLongPoll       time.Duration
	TickInterval      time.Duration
	Health            HealthPolicy

	MaxPayloadBytes int
	MaxDependencies int
}

func (c Config) withDefaults() Config {
	setD := func(p *time.Duration, d time.Duration) {
		if *p == 0 {
			*p = d
		}
	}
	setI := func(p *int, d int) {
		if *p <= 0 {
			*p = d
		}
	}
	if c.NodeID == "" {
		c.NodeID = "coordinator-" + randHex(4)
	}
	if c.Clock == nil {
		c.Clock = RealClock()
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
	setD(&c.DefaultLeaseTimeout, DefaultLeaseTimeout)
	setD(&c.MaxLeaseTimeout, DefaultMaxLeaseTimeout)
	setD(&c.RetryBackoffBase, DefaultRetryBackoffBase)
	setD(&c.RetryBackoffMax, DefaultRetryBackoffMax)
	setD(&c.Retention, DefaultRetention)
	setD(&c.WorkerTTL, DefaultWorkerTTL)
	setD(&c.HeartbeatInterval, DefaultHeartbeatInterval)
	setD(&c.MaxLongPoll, DefaultMaxLongPoll)
	setD(&c.TickInterval, DefaultTickInterval)
	setI(&c.DefaultMaxAttempts, DefaultMaxAttempts)
	setI(&c.MaxScan, DefaultMaxScan)
	setI(&c.SnapshotEvery, DefaultSnapshotEvery)
	setI(&c.MaxPayloadBytes, DefaultMaxPayloadBytes)
	setI(&c.MaxDependencies, DefaultMaxDependencies)
	c.Health = c.Health.withDefaults()
	c.TenantWeights = maps.Clone(c.TenantWeights)
	return c
}

// Coordinator owns the task queue and the worker registry. All methods
// are safe for concurrent use. Only a leader serves requests; others
// return ErrNotLeader.
type Coordinator struct {
	cfg   Config
	clock Clock
	log   *slog.Logger

	mu          sync.Mutex
	closed      bool
	leader      bool
	leaderSince time.Time
	fencer      Fencer
	token       uint64
	wal         *WAL
	hint        func(context.Context) string

	tasks     map[string]*Task
	idem      map[string]string
	ready     *readyQueues
	delayed   deadlineHeap
	leases    deadlineHeap
	finished  deadlineHeap
	waiters   map[string]map[string]struct{}
	dlq       map[string]struct{}
	workers   map[string]*workerEntry
	nextSeq   uint64
	sinceSnap int
	m         *metrics
	notify    chan struct{}
	rng       *mrand.Rand
}

// New returns a coordinator in the follower state. Call Promote (or use
// Open, or RunWithElection) to make it serve.
func New(cfg Config) (*Coordinator, error) {
	cfg = cfg.withDefaults()
	if cfg.DefaultLeaseTimeout < minLeaseTimeout || cfg.MaxLeaseTimeout < cfg.DefaultLeaseTimeout {
		return nil, fmt.Errorf("%w: lease timeouts", ErrInvalid)
	}
	for t, w := range cfg.TenantWeights {
		if !validTenant(t) || w <= 0 {
			return nil, fmt.Errorf("%w: tenant weight %q=%d", ErrInvalid, t, w)
		}
	}
	var seed [16]byte
	_, _ = rand.Read(seed[:])
	c := &Coordinator{
		cfg:    cfg,
		clock:  cfg.Clock,
		log:    cfg.Logger.With("node", cfg.NodeID),
		notify: make(chan struct{}),
		rng:    mrand.New(mrand.NewPCG(leUint64(seed[:8]), leUint64(seed[8:]))),
	}
	c.resetLocked()
	return c, nil
}

// Open returns a standalone coordinator that leads without election
// (fencing token 0), recovering state from cfg.Dir.
func Open(ctx context.Context, cfg Config) (*Coordinator, error) {
	c, err := New(cfg)
	if err != nil {
		return nil, err
	}
	if err := c.Promote(ctx, nil); err != nil {
		return nil, err
	}
	return c, nil
}

func leUint64(b []byte) uint64 {
	var v uint64
	for i := range 8 {
		v |= uint64(b[i]) << (8 * i)
	}
	return v
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newID(prefix string, now time.Time) string {
	// Time-ordered prefix keeps IDs roughly sortable by creation.
	return fmt.Sprintf("%s%012x%s", prefix, now.UnixMilli(), randHex(6))
}

func (c *Coordinator) resetLocked() {
	c.tasks = map[string]*Task{}
	c.idem = map[string]string{}
	c.ready = newReadyQueues(c.weight, c.cfg.MaxScan)
	c.delayed, c.leases, c.finished = nil, nil, nil
	c.waiters = map[string]map[string]struct{}{}
	c.dlq = map[string]struct{}{}
	c.workers = map[string]*workerEntry{}
	c.nextSeq = 1
	c.sinceSnap = 0
	c.m = newMetrics()
}

func (c *Coordinator) weight(tenant string) int {
	if w, ok := c.cfg.TenantWeights[tenant]; ok {
		return w
	}
	return 1
}

// SetTenantWeight changes a tenant's fair share (not persisted).
func (c *Coordinator) SetTenantWeight(tenant string, weight int) error {
	if !validTenant(tenant) || weight <= 0 {
		return fmt.Errorf("%w: tenant weight %q=%d", ErrInvalid, tenant, weight)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfg.TenantWeights[tenant] = weight
	return nil
}

// NodeID returns the configured node ID.
func (c *Coordinator) NodeID() string { return c.cfg.NodeID }

// IsLeader reports whether this coordinator is serving, and its token.
func (c *Coordinator) IsLeader() (bool, uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.leader, c.token
}

// LeaderHint returns the address of the current leader if known.
func (c *Coordinator) LeaderHint(ctx context.Context) string {
	c.mu.Lock()
	h := c.hint
	c.mu.Unlock()
	if h == nil {
		return ""
	}
	return h(ctx)
}

// SetLeaderHint installs the function LeaderHint uses.
func (c *Coordinator) SetLeaderHint(h func(context.Context) string) {
	c.mu.Lock()
	c.hint = h
	c.mu.Unlock()
}

// Promote makes the coordinator leader under fencer's token (nil fencer:
// standalone, token 0). It recovers the task set from the log; a log that
// already holds records from a newer leader makes promotion fail with
// ErrStaleFencingToken.
func (c *Coordinator) Promote(ctx context.Context, fencer Fencer) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}
	if c.leader {
		c.demoteLocked("re-promotion")
	}
	var token uint64
	if fencer != nil {
		token = fencer.Token()
		if err := fencer.Check(ctx); err != nil {
			return err
		}
	}
	c.resetLocked()
	now := c.clock.Now()
	var rec *Recovery
	if c.cfg.Dir != "" {
		w, r, err := OpenWAL(c.cfg.Dir, WALOptions{SyncWrites: c.cfg.SyncWrites, Logger: c.log})
		if err != nil {
			return err
		}
		if r.MaxToken > token {
			w.Close()
			return fmt.Errorf("%w: log written by token %d, ours is %d", ErrStaleFencingToken, r.MaxToken, token)
		}
		c.wal, rec = w, r
	}
	c.leader, c.token, c.fencer, c.leaderSince = true, token, fencer, now
	if rec != nil {
		c.rebuildLocked(rec.Tasks, now)
		c.log.Info("fleet: recovered", "tasks", len(rec.Tasks), "records", rec.Records,
			"stale_records", rec.StaleRecords, "truncated_bytes", rec.TruncatedBytes, "token", token)
		if err := c.repairLocked(now); err != nil {
			c.demoteLocked("repair failed")
			return err
		}
		if rec.Records > 0 || rec.StaleRecords > 0 {
			if err := c.snapshotLocked(now); err != nil {
				c.demoteLocked("snapshot failed")
				return err
			}
		}
	}
	c.wakeLocked()
	return nil
}

// rebuildLocked indexes recovered tasks. Leases are given a fresh
// visibility timeout, since heartbeats are not logged: workers that still
// hold them keep them by heartbeating the new leader.
func (c *Coordinator) rebuildLocked(tasks map[string]*Task, now time.Time) {
	all := slices.SortedFunc(maps.Values(tasks), func(a, b *Task) int { return cmp.Compare(a.Seq, b.Seq) })
	for _, t := range all {
		c.tasks[t.ID] = t
		c.nextSeq = max(c.nextSeq, t.Seq+1)
	}
	for _, t := range all {
		if t.State == StateLeased {
			if t.Lease == nil {
				t.State, t.ReadyAt = StateReady, now
			} else {
				t.Lease.Expires = now.Add(t.LeaseTimeout.D())
				if c.workers[t.Lease.WorkerID] == nil {
					// Placeholder until the worker re-registers; if it
					// never does, Tick declares it lost after WorkerTTL.
					w := newWorkerEntry(t.Lease.WorkerID, now)
					w.version = "recovered"
					c.workers[w.id] = w
				}
			}
		}
		if !t.State.valid() {
			c.log.Warn("fleet: task with invalid state in log; marking dead", "task", t.ID, "state", t.State)
			t.State, t.DeadReason, t.CompletedAt = StateDead, "invalid state in log", now
		}
		c.indexLocked(t)
	}
}

// repairLocked finishes dependency transitions a crash may have cut short.
func (c *Coordinator) repairLocked(now time.Time) error {
	ids := slices.Sorted(maps.Keys(c.tasks))
	for _, id := range ids {
		t := c.tasks[id]
		if t == nil || t.State != StatePending {
			continue
		}
		switch ok, failed := c.depsStatusLocked(t); {
		case failed != "":
			if err := c.cancelTreeLocked(t, "dependency "+failed+" did not succeed", now); err != nil {
				return err
			}
		case ok:
			nt := t.clone()
			c.makeRunnable(nt, now)
			if err := c.commitLocked(t, nt); err != nil {
				return err
			}
		}
	}
	return nil
}

// Demote stops serving and drops in-memory state; the log stays on disk
// for the next leader.
func (c *Coordinator) Demote() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.demoteLocked("demoted")
}

func (c *Coordinator) demoteLocked(reason string) {
	if !c.leader {
		return
	}
	c.log.Info("fleet: stepping down", "reason", reason, "token", c.token)
	c.leader = false
	if c.wal != nil {
		if err := c.wal.Close(); err != nil {
			c.log.Warn("fleet: wal close", "err", err)
		}
		c.wal = nil
	}
	c.fencer = nil
	c.resetLocked()
	c.wakeLocked()
}

// Close demotes the coordinator for good.
func (c *Coordinator) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.demoteLocked("closed")
	c.closed = true
	return nil
}

// Run drives timers (lease expiry, delays, worker liveness, purging and
// snapshots) until ctx ends.
func (c *Coordinator) Run(ctx context.Context) error {
	for {
		if err := sleepCtx(ctx, c.clock, c.cfg.TickInterval); err != nil {
			return nil
		}
		c.Tick()
	}
}

// RunWithElection campaigns through e and serves while elected, until ctx
// ends.
func (c *Coordinator) RunWithElection(ctx context.Context, e *Elector) error {
	c.SetLeaderHint(e.LeaderHint)
	tctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go c.Run(tctx)
	return e.Run(ctx,
		func(lctx context.Context, l Lease) error {
			return c.Promote(lctx, NewStoreFencer(e.cfg.Store, l, c.clock))
		},
		func(Lease) { c.Demote() })
}

// Tick processes everything due now. Run calls it periodically; tests with
// a fake clock call it directly.
func (c *Coordinator) Tick() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.leader {
		return
	}
	now := c.clock.Now()
	if err := c.advanceLocked(now); err != nil {
		c.log.Warn("fleet: tick", "err", err)
		return
	}
	if err := c.checkWorkersLocked(now); err != nil {
		c.log.Warn("fleet: worker check", "err", err)
		return
	}
	if err := c.purgeLocked(now); err != nil {
		c.log.Warn("fleet: purge", "err", err)
		return
	}
	if c.wal != nil && c.sinceSnap >= c.cfg.SnapshotEvery {
		if err := c.snapshotLocked(now); err != nil {
			c.log.Warn("fleet: snapshot", "err", err)
		}
	}
}

// Snapshot compacts the log now.
func (c *Coordinator) Snapshot() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.leadLocked(); err != nil {
		return err
	}
	if c.wal == nil {
		return nil
	}
	return c.snapshotLocked(c.clock.Now())
}

func (c *Coordinator) snapshotLocked(now time.Time) error {
	if err := c.fenceLocked(); err != nil {
		return err
	}
	tasks := slices.SortedFunc(maps.Values(c.tasks), func(a, b *Task) int { return cmp.Compare(a.Seq, b.Seq) })
	if err := c.wal.Snapshot(c.token, tasks, now); err != nil {
		return err
	}
	c.sinceSnap = 0
	c.m.c.Snapshots++
	return nil
}

func (c *Coordinator) leadLocked() error {
	if c.closed {
		return ErrClosed
	}
	if !c.leader {
		return ErrNotLeader
	}
	return nil
}

// fenceLocked verifies leadership before a write; a stale token demotes
// this coordinator.
func (c *Coordinator) fenceLocked() error {
	if err := c.leadLocked(); err != nil {
		return err
	}
	if c.fencer == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.fencer.Check(ctx); err != nil {
		c.m.c.FenceRejects++
		if errors.Is(err, ErrStaleFencingToken) {
			c.log.Warn("fleet: write rejected with stale fencing token", "token", c.token, "err", err)
			c.demoteLocked("stale fencing token")
			return errors.Join(ErrNotLeader, err)
		}
		return fmt.Errorf("fleet: fence check: %w", err)
	}
	return nil
}

// commitLocked logs nt and then makes it current, replacing old (nil for a
// new task). Nothing changes if logging fails.
func (c *Coordinator) commitLocked(old, nt *Task) error {
	if err := c.fenceLocked(); err != nil {
		return err
	}
	if old != nil {
		nt.Version = old.Version + 1
	} else {
		nt.Version = 1
	}
	nt.UpdatedAt = c.clock.Now()
	if c.wal != nil {
		if _, err := c.wal.Append(c.token, opPut, nt, ""); err != nil {
			return err
		}
		c.sinceSnap++
	}
	if old != nil {
		c.unindexLocked(old)
	}
	c.tasks[nt.ID] = nt
	c.indexLocked(nt)
	return nil
}

func idemKey(tenant, key string) string { return tenant + "\x00" + key }

func (c *Coordinator) indexLocked(t *Task) {
	c.m.account(t, +1)
	if t.IdempotencyKey != "" {
		c.idem[idemKey(t.Tenant, t.IdempotencyKey)] = t.ID
	}
	switch t.State {
	case StatePending:
		for _, d := range t.DependsOn {
			if dep := c.tasks[d]; dep != nil && dep.State == StateSucceeded {
				continue
			}
			ws := c.waiters[d]
			if ws == nil {
				ws = map[string]struct{}{}
				c.waiters[d] = ws
			}
			ws[t.ID] = struct{}{}
		}
	case StateScheduled:
		c.delayed.add(t.ID, t.NotBefore)
	case StateReady:
		c.ready.push(t)
		c.wakeLocked()
	case StateLeased:
		c.leases.add(t.ID, t.Lease.Expires)
		if w := c.workers[t.Lease.WorkerID]; w != nil {
			w.leases[t.ID] = struct{}{}
		}
	case StateDead:
		c.dlq[t.ID] = struct{}{}
	}
	if t.State.Terminal() {
		c.finished.add(t.ID, t.CompletedAt)
	}
}

func (c *Coordinator) unindexLocked(t *Task) {
	c.m.account(t, -1)
	switch t.State {
	case StatePending:
		for _, d := range t.DependsOn {
			if ws := c.waiters[d]; ws != nil {
				delete(ws, t.ID)
				if len(ws) == 0 {
					delete(c.waiters, d)
				}
			}
		}
	case StateReady:
		c.ready.remove(t)
	case StateLeased:
		if w := c.workers[t.Lease.WorkerID]; w != nil {
			delete(w.leases, t.ID)
		}
	case StateDead:
		delete(c.dlq, t.ID)
	}
}

// wakeLocked releases long-polling lease requests to look again.
func (c *Coordinator) wakeLocked() {
	close(c.notify)
	c.notify = make(chan struct{})
}

func (c *Coordinator) makeRunnable(t *Task, now time.Time) {
	if t.NotBefore.After(now) {
		t.State = StateScheduled
		return
	}
	t.State, t.ReadyAt, t.NotBefore = StateReady, now, time.Time{}
}

// depsStatusLocked reports whether all of t's dependencies succeeded, or
// the ID of one that never will. Purged dependencies count as succeeded:
// only finished tasks are purged, and failures cascade before that.
func (c *Coordinator) depsStatusLocked(t *Task) (ok bool, failed string) {
	ok = true
	for _, d := range t.DependsOn {
		dep := c.tasks[d]
		switch {
		case dep == nil, dep.State == StateSucceeded:
		case dep.State == StateDead || dep.State == StateCanceled:
			return false, d
		default:
			ok = false
		}
	}
	return ok, ""
}

func (c *Coordinator) backoff(attempts int) time.Duration {
	base := c.cfg.RetryBackoffBase
	if base < 0 {
		return 0
	}
	d := float64(base) * math.Pow(2, float64(max(attempts-1, 0)))
	if d > float64(c.cfg.RetryBackoffMax) || math.IsInf(d, 0) {
		d = float64(c.cfg.RetryBackoffMax)
	}
	half := time.Duration(d / 2)
	return half + time.Duration(c.rng.Int64N(int64(half)+1))
}

// ---- Submission -----------------------------------------------------

func (c *Coordinator) normalize(spec TaskSpec, now time.Time) (*Task, error) {
	if spec.Tenant == "" {
		spec.Tenant = "default"
	}
	if !validTenant(spec.Tenant) {
		return nil, fmt.Errorf("%w: tenant %q", ErrInvalid, spec.Tenant)
	}
	if spec.ID != "" && !validID(spec.ID) {
		return nil, fmt.Errorf("%w: task id %q", ErrInvalid, spec.ID)
	}
	if spec.Priority < PriorityLow || spec.Priority >= numPriorities {
		return nil, fmt.Errorf("%w: priority %d", ErrInvalid, spec.Priority)
	}
	if spec.Kind == "" || len(spec.Kind) > 128 {
		return nil, fmt.Errorf("%w: kind must be 1..128 bytes", ErrInvalid)
	}
	if len(spec.Payload) > c.cfg.MaxPayloadBytes {
		return nil, fmt.Errorf("%w: payload of %d bytes exceeds %d", ErrInvalid, len(spec.Payload), c.cfg.MaxPayloadBytes)
	}
	if len(spec.IdempotencyKey) > 256 {
		return nil, fmt.Errorf("%w: idempotency key too long", ErrInvalid)
	}
	if len(spec.DependsOn) > c.cfg.MaxDependencies {
		return nil, fmt.Errorf("%w: %d dependencies exceed %d", ErrInvalid, len(spec.DependsOn), c.cfg.MaxDependencies)
	}
	if spec.MaxAttempts < 0 || spec.MaxAttempts > maxAttemptsLimit {
		return nil, fmt.Errorf("%w: max_attempts %d", ErrInvalid, spec.MaxAttempts)
	}
	if spec.MaxAttempts == 0 {
		spec.MaxAttempts = c.cfg.DefaultMaxAttempts
	}
	lt := spec.LeaseTimeout.D()
	if lt == 0 {
		lt = c.cfg.DefaultLeaseTimeout
	}
	if lt < minLeaseTimeout || lt > c.cfg.MaxLeaseTimeout {
		return nil, fmt.Errorf("%w: lease_timeout %v outside [%v, %v]", ErrInvalid, lt, minLeaseTimeout, c.cfg.MaxLeaseTimeout)
	}
	if spec.Delay < 0 {
		return nil, fmt.Errorf("%w: negative delay", ErrInvalid)
	}
	deps := slices.Compact(slices.Sorted(slices.Values(spec.DependsOn)))
	for _, d := range deps {
		if !validID(d) {
			return nil, fmt.Errorf("%w: dependency id %q", ErrInvalid, d)
		}
	}
	nb := spec.NotBefore
	if at := now.Add(spec.Delay.D()); spec.Delay > 0 && at.After(nb) {
		nb = at
	}
	if !nb.After(now) {
		nb = time.Time{}
	}
	return &Task{
		ID: spec.ID, Tenant: spec.Tenant, Priority: spec.Priority, Kind: spec.Kind,
		Payload: spec.Payload, Requirements: spec.Requirements, IdempotencyKey: spec.IdempotencyKey,
		DependsOn: deps, Labels: spec.Labels, MaxAttempts: spec.MaxAttempts,
		LeaseTimeout: Duration(lt), CreatedAt: now, NotBefore: nb,
	}, nil
}

// Submit enqueues a task. A repeated idempotency key (per tenant) returns
// the original task with Duplicate set, whatever its state.
func (c *Coordinator) Submit(ctx context.Context, spec TaskSpec) (SubmitResult, error) {
	res, err := c.SubmitBatch(ctx, []TaskSpec{spec})
	if err != nil {
		return SubmitResult{}, err
	}
	return res[0], nil
}

// SubmitBatch enqueues tasks in order; later tasks may depend on earlier
// ones. It stops at the first invalid task, returning the results so far
// together with the error.
func (c *Coordinator) SubmitBatch(ctx context.Context, specs []TaskSpec) ([]SubmitResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.leadLocked(); err != nil {
		return nil, err
	}
	out := make([]SubmitResult, 0, len(specs))
	for i, spec := range specs {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		r, err := c.submitLocked(spec)
		if err != nil {
			if len(specs) > 1 {
				err = fmt.Errorf("task %d: %w", i, err)
			}
			return out, err
		}
		out = append(out, r)
	}
	return out, nil
}

func (c *Coordinator) submitLocked(spec TaskSpec) (SubmitResult, error) {
	now := c.clock.Now()
	t, err := c.normalize(spec, now)
	if err != nil {
		return SubmitResult{}, err
	}
	if t.IdempotencyKey != "" {
		if id, ok := c.idem[idemKey(t.Tenant, t.IdempotencyKey)]; ok {
			if ex := c.tasks[id]; ex != nil {
				c.m.c.Duplicates++
				return SubmitResult{Task: ex.clone(), Duplicate: true}, nil
			}
		}
	}
	if t.ID == "" {
		t.ID = newID("t_", now)
	} else if _, exists := c.tasks[t.ID]; exists {
		return SubmitResult{}, fmt.Errorf("%w: task %s already exists", ErrConflict, t.ID)
	}
	for _, d := range t.DependsOn {
		if d == t.ID {
			return SubmitResult{}, fmt.Errorf("%w: task depends on itself", ErrInvalid)
		}
		if _, ok := c.tasks[d]; !ok {
			return SubmitResult{}, fmt.Errorf("%w: %s", ErrUnknownDependency, d)
		}
	}
	switch ok, failed := c.depsStatusLocked(t); {
	case failed != "":
		return SubmitResult{}, fmt.Errorf("%w: %s is %s", ErrDependencyFailed, failed, c.tasks[failed].State)
	case ok:
		c.makeRunnable(t, now)
	default:
		t.State = StatePending
	}
	t.Seq = c.nextSeq
	if err := c.commitLocked(nil, t); err != nil {
		return SubmitResult{}, err
	}
	c.nextSeq++
	c.m.c.Submitted++
	return SubmitResult{Task: t.clone()}, nil
}

// ---- Queries --------------------------------------------------------

// Get returns a copy of a task.
func (c *Coordinator) Get(_ context.Context, id string) (*Task, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.leadLocked(); err != nil {
		return nil, err
	}
	t := c.tasks[id]
	if t == nil {
		return nil, fmt.Errorf("%w: task %s", ErrNotFound, id)
	}
	return t.clone(), nil
}

// List returns tasks matching f in submission order.
func (c *Coordinator) List(_ context.Context, f ListFilter) ([]*Task, error) {
	if f.State != "" && !f.State.valid() {
		return nil, fmt.Errorf("%w: state %q", ErrInvalid, f.State)
	}
	limit := f.Limit
	if limit <= 0 || limit > maxListLimit {
		limit = min(max(limit, 100), maxListLimit)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.leadLocked(); err != nil {
		return nil, err
	}
	var src []*Task
	if f.State == StateDead {
		for id := range c.dlq {
			src = append(src, c.tasks[id])
		}
	} else {
		src = slices.Collect(maps.Values(c.tasks))
	}
	var out []*Task
	for _, t := range src {
		if t.Seq <= f.After || (f.Tenant != "" && t.Tenant != f.Tenant) || (f.State != "" && t.State != f.State) {
			continue
		}
		out = append(out, t)
	}
	slices.SortFunc(out, func(a, b *Task) int { return cmp.Compare(a.Seq, b.Seq) })
	if len(out) > limit {
		out = out[:limit]
	}
	for i, t := range out {
		out[i] = t.clone()
	}
	return out, nil
}

// DeadLetters lists the dead-letter queue (optionally for one tenant).
func (c *Coordinator) DeadLetters(ctx context.Context, tenant string, limit int) ([]*Task, error) {
	return c.List(ctx, ListFilter{Tenant: tenant, State: StateDead, Limit: limit})
}

// ---- Administrative transitions ------------------------------------

// Cancel cancels a task that has not finished, revoking its lease if it
// has one, and cancels every task that depends on it. Canceling a
// canceled task is a no-op; canceling a succeeded or dead task is a
// conflict.
func (c *Coordinator) Cancel(_ context.Context, id, reason string) (*Task, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.leadLocked(); err != nil {
		return nil, err
	}
	t := c.tasks[id]
	if t == nil {
		return nil, fmt.Errorf("%w: task %s", ErrNotFound, id)
	}
	switch t.State {
	case StateCanceled:
		return t.clone(), nil
	case StateSucceeded, StateDead:
		return nil, fmt.Errorf("%w: task %s is %s", ErrConflict, id, t.State)
	}
	if reason == "" {
		reason = "canceled"
	}
	if err := c.cancelTreeLocked(t, reason, c.clock.Now()); err != nil {
		return nil, err
	}
	return c.tasks[id].clone(), nil
}

// cancelTreeLocked cancels root and, transitively, its pending
// dependents.
func (c *Coordinator) cancelTreeLocked(root *Task, reason string, now time.Time) error {
	type item struct {
		t      *Task
		reason string
	}
	stack := []item{{root, reason}}
	for len(stack) > 0 {
		it := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		t := c.tasks[it.t.ID]
		if t == nil || t.State.Terminal() {
			continue
		}
		nt := t.clone()
		if nt.Lease != nil {
			nt.Closure = "revoked:" + nt.Lease.ID
			nt.Lease = nil
		}
		nt.State, nt.CompletedAt, nt.LastError = StateCanceled, now, it.reason
		if err := c.commitLocked(t, nt); err != nil {
			return err
		}
		c.m.c.Canceled++
		stack = pushDependents(c, stack, nt.ID, func(dep *Task) item {
			return item{dep, "dependency " + nt.ID + " was canceled"}
		})
	}
	return nil
}

// pushDependents appends the pending dependents of id to stack, in a
// deterministic order.
func pushDependents[T any](c *Coordinator, stack []T, id string, mk func(*Task) T) []T {
	ws := c.waiters[id]
	for _, did := range slices.Sorted(maps.Keys(ws)) {
		if d := c.tasks[did]; d != nil && d.State == StatePending {
			stack = append(stack, mk(d))
		}
	}
	return stack
}

// Redrive moves a dead-lettered task back to ready with a fresh attempt
// budget. Dependents canceled when it died stay canceled.
func (c *Coordinator) Redrive(_ context.Context, id string) (*Task, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.leadLocked(); err != nil {
		return nil, err
	}
	t := c.tasks[id]
	if t == nil {
		return nil, fmt.Errorf("%w: task %s", ErrNotFound, id)
	}
	if t.State != StateDead {
		return nil, fmt.Errorf("%w: task %s is %s, not dead", ErrConflict, id, t.State)
	}
	now := c.clock.Now()
	nt := t.clone()
	nt.State, nt.Attempts, nt.ReadyAt = StateReady, 0, now
	nt.DeadReason, nt.CompletedAt = "", time.Time{}
	if err := c.commitLocked(t, nt); err != nil {
		return nil, err
	}
	c.m.c.Redriven++
	return nt.clone(), nil
}

// ---- Time-driven transitions ---------------------------------------

func (c *Coordinator) advanceLocked(now time.Time) error {
	for {
		e, ok := c.delayed.peek()
		if !ok || e.at.After(now) {
			break
		}
		c.delayed.pop()
		t := c.tasks[e.id]
		if t == nil || t.State != StateScheduled || !t.NotBefore.Equal(e.at) {
			continue
		}
		nt := t.clone()
		nt.State, nt.ReadyAt, nt.NotBefore = StateReady, now, time.Time{}
		if err := c.commitLocked(t, nt); err != nil {
			return err
		}
	}
	for {
		e, ok := c.leases.peek()
		if !ok || e.at.After(now) {
			break
		}
		c.leases.pop()
		t := c.tasks[e.id]
		if t == nil || t.State != StateLeased || !t.Lease.Expires.Equal(e.at) {
			continue
		}
		c.m.c.LeaseExpiries++
		if err := c.endAttemptLocked(t, "expired", "lease expired", false, 0, outcomeExpired, now); err != nil {
			return err
		}
	}
	live := 0
	for _, w := range c.workers {
		live += len(w.leases)
	}
	c.leases.compact(live, func(e deadlineEntry) bool {
		t := c.tasks[e.id]
		return t != nil && t.State == StateLeased && t.Lease.Expires.Equal(e.at)
	})
	return nil
}

// endAttemptLocked closes t's current lease and retries the task, or
// dead-letters it when it is out of attempts or the failure is permanent.
func (c *Coordinator) endAttemptLocked(t *Task, closure, reason string, permanent bool, retryAfter time.Duration, o outcome, now time.Time) error {
	nt := t.clone()
	wid := ""
	if nt.Lease != nil {
		wid = nt.Lease.WorkerID
		nt.Closure = closure + ":" + nt.Lease.ID
		nt.Lease = nil
	}
	nt.LastError = reason
	dead := permanent || nt.Attempts >= nt.MaxAttempts
	if dead {
		nt.State, nt.CompletedAt = StateDead, now
		if permanent {
			nt.DeadReason = "permanent failure: " + reason
		} else {
			nt.DeadReason = fmt.Sprintf("%s after %d attempts", reason, nt.Attempts)
		}
	} else {
		d := c.backoff(nt.Attempts)
		if retryAfter > 0 {
			d = min(retryAfter, c.cfg.RetryBackoffMax)
		}
		nt.NotBefore = now.Add(d)
		c.makeRunnable(nt, now)
	}
	if err := c.commitLocked(t, nt); err != nil {
		return err
	}
	if w := c.workers[wid]; w != nil && !(permanent && o == outcomeFailure) {
		if w.record(c.cfg.Health, o, now) {
			c.m.c.Quarantines++
			c.log.Warn("fleet: worker quarantined", "worker", wid, "health", w.score, "until", w.quarUntil)
		}
	}
	if dead {
		c.m.c.DeadLettered++
		return c.cascadeFailureLocked(nt, now)
	}
	c.m.c.Retries++
	return nil
}

func (c *Coordinator) cascadeFailureLocked(t *Task, now time.Time) error {
	var deps []*Task
	deps = pushDependents(c, deps, t.ID, func(d *Task) *Task { return d })
	for _, d := range deps {
		if err := c.cancelTreeLocked(d, "dependency "+t.ID+" is "+string(t.State), now); err != nil {
			return err
		}
	}
	return nil
}

func (c *Coordinator) checkWorkersLocked(now time.Time) error {
	for _, id := range slices.Sorted(maps.Keys(c.workers)) {
		w := c.workers[id]
		if w.maybeRelease(now) {
			c.log.Info("fleet: worker quarantine ended", "worker", id)
			c.wakeLocked()
		}
		if w.lost() {
			if now.Sub(w.lastSeen) > 10*c.cfg.WorkerTTL {
				delete(c.workers, id)
			}
			continue
		}
		if now.Sub(w.lastSeen) <= c.cfg.WorkerTTL {
			continue
		}
		c.log.Warn("fleet: worker lost", "worker", id, "last_seen", w.lastSeen, "leases", len(w.leases))
		c.m.c.WorkersLost++
		if err := c.reclaimLocked(w, "worker lost", outcomeExpired, now); err != nil {
			return err
		}
		if w.flap(c.cfg.Health, now) {
			c.m.c.Quarantines++
		}
		w.markLost()
	}
	return nil
}

// reclaimLocked ends every lease a worker holds as an expiry.
func (c *Coordinator) reclaimLocked(w *workerEntry, reason string, o outcome, now time.Time) error {
	for _, tid := range slices.Sorted(maps.Keys(w.leases)) {
		t := c.tasks[tid]
		if t == nil || t.State != StateLeased || t.Lease.WorkerID != w.id {
			delete(w.leases, tid)
			continue
		}
		c.m.c.LeaseExpiries++
		if err := c.endAttemptLocked(t, "expired", reason, false, 0, o, now); err != nil {
			return err
		}
	}
	return nil
}

func (c *Coordinator) purgeLocked(now time.Time) error {
	for n := 0; n < purgeBatch; n++ {
		e, ok := c.finished.peek()
		if !ok || now.Sub(e.at) < c.cfg.Retention {
			return nil
		}
		c.finished.pop()
		t := c.tasks[e.id]
		if t == nil || !t.State.Terminal() || !t.CompletedAt.Equal(e.at) {
			continue
		}
		if err := c.fenceLocked(); err != nil {
			return err
		}
		if c.wal != nil {
			if _, err := c.wal.Append(c.token, opDelete, nil, t.ID); err != nil {
				return err
			}
			c.sinceSnap++
		}
		c.unindexLocked(t)
		delete(c.tasks, t.ID)
		if k := idemKey(t.Tenant, t.IdempotencyKey); t.IdempotencyKey != "" && c.idem[k] == t.ID {
			delete(c.idem, k)
		}
		delete(c.waiters, t.ID)
		c.m.c.Purged++
	}
	return nil
}

// ---- Worker protocol ------------------------------------------------

// Register implements WorkerAPI.
func (c *Coordinator) Register(_ context.Context, req RegisterRequest) (RegisterResponse, error) {
	caps := req.Capabilities
	if caps.MaxConcurrency == 0 {
		caps.MaxConcurrency = 1
	}
	if caps.MaxConcurrency < 0 || caps.MaxConcurrency > maxConcurrency {
		return RegisterResponse{}, fmt.Errorf("%w: max_concurrency %d", ErrInvalid, caps.MaxConcurrency)
	}
	if req.WorkerID != "" && !validID(req.WorkerID) {
		return RegisterResponse{}, fmt.Errorf("%w: worker id %q", ErrInvalid, req.WorkerID)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.leadLocked(); err != nil {
		return RegisterResponse{}, err
	}
	now := c.clock.Now()
	id := req.WorkerID
	if id == "" {
		id = newID("w_", now)
	}
	w := c.workers[id]
	if w == nil {
		w = newWorkerEntry(id, now)
		c.workers[id] = w
	} else {
		restarted := w.incarnation != "" && req.Incarnation != w.incarnation
		if restarted {
			// The previous process died: its leases are orphans.
			if err := c.reclaimLocked(w, "worker restarted", outcomeExpired, now); err != nil {
				return RegisterResponse{}, err
			}
		}
		wasLost := w.lost()
		w.revive()
		if restarted || wasLost {
			if w.flap(c.cfg.Health, now) {
				c.m.c.Quarantines++
				c.log.Warn("fleet: flapping worker quarantined", "worker", id, "until", w.quarUntil)
			}
		}
	}
	w.incarnation, w.caps, w.version, w.lastSeen = req.Incarnation, caps, req.Version, now
	c.wakeLocked()
	return RegisterResponse{
		WorkerID:          id,
		HeartbeatInterval: Duration(c.cfg.HeartbeatInterval),
		MaxLongPoll:       Duration(c.cfg.MaxLongPoll),
	}, nil
}

func (c *Coordinator) liveWorkerLocked(id string, now time.Time) (*workerEntry, error) {
	w := c.workers[id]
	if w == nil || w.lost() {
		return nil, fmt.Errorf("%w: %s", ErrUnknownWorker, id)
	}
	w.lastSeen = now
	if w.maybeRelease(now) {
		c.wakeLocked()
	}
	return w, nil
}

// Heartbeat implements WorkerAPI: it extends the worker's live leases by
// their visibility timeouts and reports the ones it no longer holds.
// Extensions are not logged; after a failover workers keep their leases
// by heartbeating the new leader, which grants recovered leases a fresh
// timeout.
func (c *Coordinator) Heartbeat(_ context.Context, req HeartbeatRequest) (HeartbeatResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.leadLocked(); err != nil {
		return HeartbeatResponse{}, err
	}
	now := c.clock.Now()
	if err := c.advanceLocked(now); err != nil {
		return HeartbeatResponse{}, err
	}
	w, err := c.liveWorkerLocked(req.WorkerID, now)
	if err != nil {
		return HeartbeatResponse{}, err
	}
	var resp HeartbeatResponse
	for _, ref := range req.Leases {
		t := c.tasks[ref.TaskID]
		if t == nil || t.State != StateLeased || t.Lease.ID != ref.LeaseID || t.Lease.WorkerID != w.id {
			resp.Revoked = append(resp.Revoked, ref)
			continue
		}
		t.Lease.Expires = now.Add(t.LeaseTimeout.D())
		c.leases.add(t.ID, t.Lease.Expires)
		w.leases[t.ID] = struct{}{}
		resp.Extended++
	}
	resp.State = w.state
	resp.Quarantined = w.state == WorkerQuarantined
	resp.Draining = w.state == WorkerDraining || (resp.Quarantined && w.stateBefore == WorkerDraining)
	return resp, nil
}

// Lease implements WorkerAPI. It long-polls up to min(req.Wait,
// MaxLongPoll) for tasks the worker can run, granting up to req.Max (and
// no more than the worker's free capacity).
func (c *Coordinator) Lease(ctx context.Context, req LeaseRequest) (LeaseResponse, error) {
	if req.Max <= 0 {
		req.Max = 1
	}
	wait := min(max(req.Wait.D(), 0), c.cfg.MaxLongPoll)
	deadline := c.clock.Now().Add(wait)
	for {
		c.mu.Lock()
		resp, err := c.tryLeaseLocked(req)
		notify := c.notify
		var nextDue time.Time
		if e, ok := c.delayed.peek(); ok {
			nextDue = e.at
		}
		if e, ok := c.leases.peek(); ok && (nextDue.IsZero() || e.at.Before(nextDue)) {
			nextDue = e.at
		}
		c.mu.Unlock()
		if err != nil || len(resp.Assignments) > 0 || resp.Draining || resp.Quarantined {
			return resp, err
		}
		now := c.clock.Now()
		if !now.Before(deadline) {
			return resp, nil
		}
		until := deadline
		if !nextDue.IsZero() && nextDue.Before(until) {
			until = nextDue
		}
		timer := c.clock.NewTimer(until.Sub(now))
		select {
		case <-ctx.Done():
			timer.Stop()
			return LeaseResponse{}, ctx.Err()
		case <-notify:
		case <-timer.C():
		}
		timer.Stop()
	}
}

func (c *Coordinator) tryLeaseLocked(req LeaseRequest) (LeaseResponse, error) {
	if err := c.leadLocked(); err != nil {
		return LeaseResponse{}, err
	}
	now := c.clock.Now()
	if err := c.advanceLocked(now); err != nil {
		return LeaseResponse{}, err
	}
	w, err := c.liveWorkerLocked(req.WorkerID, now)
	if err != nil {
		return LeaseResponse{}, err
	}
	switch w.state {
	case WorkerDraining:
		return LeaseResponse{Draining: true}, nil
	case WorkerQuarantined:
		return LeaseResponse{Quarantined: true, RetryAfter: Duration(w.quarUntil.Sub(now))}, nil
	}
	n := min(req.Max, w.free())
	var resp LeaseResponse
	match := func(t *Task) bool { return w.caps.Satisfies(t.Requirements) }
	for range n {
		t := c.ready.next(match)
		if t == nil {
			break
		}
		nt := t.clone()
		lease := &LeaseInfo{ID: newID("l_", now), WorkerID: w.id, Granted: now, Expires: now.Add(nt.LeaseTimeout.D())}
		nt.State, nt.Lease, nt.LeasedAt = StateLeased, lease, now
		nt.Attempts++
		if err := c.commitLocked(t, nt); err != nil {
			if len(resp.Assignments) > 0 {
				break
			}
			return LeaseResponse{}, err
		}
		c.m.c.Leased++
		if nt.Attempts > 1 {
			c.m.c.Redeliveries++
		}
		c.m.queueWait.Add(now.Sub(t.ReadyAt).Seconds())
		resp.Assignments = append(resp.Assignments, Assignment{
			Task: nt.clone(), LeaseID: lease.ID, Expires: lease.Expires, Attempt: nt.Attempts, FencingToken: c.token,
		})
	}
	if len(resp.Assignments) == 0 {
		resp.RetryAfter = Duration(time.Second)
	}
	return resp, nil
}

// leasedTaskLocked finds the task for a worker acknowledgement. It
// returns the task when ref is its live lease; done=true when the same
// lease was already closed with closure (a retried acknowledgement).
func (c *Coordinator) leasedTaskLocked(workerID, taskID, leaseID, closure string) (t *Task, done bool, err error) {
	t = c.tasks[taskID]
	if t == nil {
		return nil, false, fmt.Errorf("%w: task %s", ErrNotFound, taskID)
	}
	if t.State == StateLeased && t.Lease.ID == leaseID && (workerID == "" || t.Lease.WorkerID == workerID) {
		return t, false, nil
	}
	if leaseID != "" && t.Closure == closure+":"+leaseID {
		return t, true, nil
	}
	return nil, false, fmt.Errorf("%w: task %s lease %s", ErrLeaseLost, taskID, leaseID)
}

// Complete implements WorkerAPI. Only the live lease holder can complete
// a task, so its result is committed once even when a redelivered copy
// also runs; retrying an acknowledged completion is a no-op.
func (c *Coordinator) Complete(_ context.Context, req CompleteRequest) error {
	if len(req.Result) > c.cfg.MaxPayloadBytes {
		return fmt.Errorf("%w: result of %d bytes exceeds %d", ErrInvalid, len(req.Result), c.cfg.MaxPayloadBytes)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.leadLocked(); err != nil {
		return err
	}
	now := c.clock.Now()
	if err := c.advanceLocked(now); err != nil {
		return err
	}
	t, done, err := c.leasedTaskLocked(req.WorkerID, req.TaskID, req.LeaseID, "complete")
	if err != nil || done {
		return err
	}
	wid := t.Lease.WorkerID
	nt := t.clone()
	nt.Closure = "complete:" + nt.Lease.ID
	nt.Lease = nil
	nt.State, nt.Result, nt.CompletedAt, nt.LastError = StateSucceeded, req.Result, now, ""
	if err := c.commitLocked(t, nt); err != nil {
		return err
	}
	c.m.c.Succeeded++
	c.m.rate.add(now, 1)
	c.m.runTime.Add(now.Sub(t.LeasedAt).Seconds())
	c.m.endToEnd.Add(now.Sub(t.CreatedAt).Seconds())
	if w := c.workers[wid]; w != nil {
		w.lastSeen = now
		w.record(c.cfg.Health, outcomeSuccess, now)
	}
	return c.releaseDependentsLocked(nt.ID, now)
}

func (c *Coordinator) releaseDependentsLocked(id string, now time.Time) error {
	var deps []*Task
	deps = pushDependents(c, deps, id, func(d *Task) *Task { return d })
	for _, d := range deps {
		ok, failed := c.depsStatusLocked(d)
		if failed != "" {
			if err := c.cancelTreeLocked(d, "dependency "+failed+" did not succeed", now); err != nil {
				return err
			}
			continue
		}
		if !ok {
			continue
		}
		nd := d.clone()
		c.makeRunnable(nd, now)
		if err := c.commitLocked(d, nd); err != nil {
			return err
		}
	}
	return nil
}

// Fail implements WorkerAPI.
func (c *Coordinator) Fail(_ context.Context, req FailRequest) error {
	if len(req.Error) > 64<<10 {
		req.Error = req.Error[:64<<10]
	}
	if req.Error == "" {
		req.Error = "failed"
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.leadLocked(); err != nil {
		return err
	}
	now := c.clock.Now()
	if err := c.advanceLocked(now); err != nil {
		return err
	}
	t, done, err := c.leasedTaskLocked(req.WorkerID, req.TaskID, req.LeaseID, "fail")
	if err != nil || done {
		return err
	}
	if w := c.workers[t.Lease.WorkerID]; w != nil {
		w.lastSeen = now
	}
	c.m.c.Failures++
	return c.endAttemptLocked(t, "fail", req.Error, req.Permanent, req.RetryAfter.D(), outcomeFailure, now)
}

// Release implements WorkerAPI: the task goes back to the front of its
// queue and the attempt is not counted.
func (c *Coordinator) Release(_ context.Context, req ReleaseRequest) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.leadLocked(); err != nil {
		return err
	}
	now := c.clock.Now()
	t, done, err := c.leasedTaskLocked(req.WorkerID, req.TaskID, req.LeaseID, "release")
	if err != nil || done {
		return err
	}
	return c.releaseLocked(t, now)
}

func (c *Coordinator) releaseLocked(t *Task, now time.Time) error {
	nt := t.clone()
	nt.Closure = "release:" + nt.Lease.ID
	nt.Lease = nil
	nt.Attempts = max(nt.Attempts-1, 0)
	nt.State, nt.ReadyAt = StateReady, now
	if err := c.commitLocked(t, nt); err != nil {
		return err
	}
	c.m.c.Released++
	return nil
}

// Drain implements WorkerAPI: the worker gets no new leases but keeps the
// ones it has.
func (c *Coordinator) Drain(_ context.Context, workerID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.leadLocked(); err != nil {
		return err
	}
	w, err := c.liveWorkerLocked(workerID, c.clock.Now())
	if err != nil {
		return err
	}
	if w.state == WorkerQuarantined {
		w.stateBefore = WorkerDraining
	} else {
		w.state = WorkerDraining
	}
	return nil
}

// Deregister implements WorkerAPI: the worker is forgotten and its
// remaining leases are released without counting the attempts.
func (c *Coordinator) Deregister(_ context.Context, workerID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.leadLocked(); err != nil {
		return err
	}
	w := c.workers[workerID]
	if w == nil {
		return nil
	}
	now := c.clock.Now()
	for _, tid := range slices.Sorted(maps.Keys(w.leases)) {
		t := c.tasks[tid]
		if t == nil || t.State != StateLeased || t.Lease.WorkerID != w.id {
			continue
		}
		if err := c.releaseLocked(t, now); err != nil {
			return err
		}
	}
	delete(c.workers, workerID)
	return nil
}

// Workers lists registered workers.
func (c *Coordinator) Workers(_ context.Context) ([]WorkerStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.leadLocked(); err != nil {
		return nil, err
	}
	return c.workersLocked(), nil
}

func (c *Coordinator) workersLocked() []WorkerStatus {
	out := make([]WorkerStatus, 0, len(c.workers))
	for _, id := range slices.Sorted(maps.Keys(c.workers)) {
		out = append(out, c.workers[id].status())
	}
	return out
}

// Metrics returns a snapshot of queue depths, counters, throughput,
// latency quantiles and workers. Followers report only their identity.
func (c *Coordinator) Metrics(_ context.Context) Snapshot {
	c.mu.Lock()
	now := c.clock.Now()
	s := Snapshot{Time: now, NodeID: c.cfg.NodeID, Leader: c.leader, FencingToken: c.token}
	if !c.leader {
		c.mu.Unlock()
		return s
	}
	s.LeaderSince = c.leaderSince
	s.States = maps.Clone(c.m.states)
	for st, n := range s.States {
		if n == 0 {
			delete(s.States, st)
		}
	}
	s.Queues = c.m.queues()
	s.DLQ = len(c.dlq)
	s.Counters = c.m.c
	s.Throughput = Throughput{Last10s: c.m.rate.rate(now, 10), Last1m: c.m.rate.rate(now, 59)}
	s.Workers = c.workersLocked()
	s.SinceSnapshot = c.sinceSnap
	if c.wal != nil {
		s.WALSeq = c.wal.LastSeq()
	}
	qw, rt, e2e := c.m.queueWait, c.m.runTime, c.m.endToEnd
	c.mu.Unlock()
	s.QueueWait, s.RunTime, s.EndToEnd = summarize(qw), summarize(rt), summarize(e2e)
	return s
}

var _ WorkerAPI = (*Coordinator)(nil)
