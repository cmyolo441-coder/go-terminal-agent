package fleet

import (
	"math"
	"sort"
	"time"
)

// Counters are monotonic totals since this coordinator became leader.
type Counters struct {
	Submitted     uint64 `json:"submitted"`
	Duplicates    uint64 `json:"duplicates"`
	Leased        uint64 `json:"leased"`
	Redeliveries  uint64 `json:"redeliveries"`
	Succeeded     uint64 `json:"succeeded"`
	Failures      uint64 `json:"failures"`
	Retries       uint64 `json:"retries"`
	DeadLettered  uint64 `json:"dead_lettered"`
	Canceled      uint64 `json:"canceled"`
	LeaseExpiries uint64 `json:"lease_expiries"`
	Released      uint64 `json:"released"`
	Redriven      uint64 `json:"redriven"`
	Quarantines   uint64 `json:"quarantines"`
	WorkersLost   uint64 `json:"workers_lost"`
	FenceRejects  uint64 `json:"fence_rejects"`
	Purged        uint64 `json:"purged"`
	Snapshots     uint64 `json:"snapshots"`
}

// LatencySummary summarises a latency distribution.
type LatencySummary struct {
	Count uint64   `json:"count"`
	P50   Duration `json:"p50"`
	P95   Duration `json:"p95"`
	P99   Duration `json:"p99"`
	Max   Duration `json:"max"`
}

// QueueDepth counts a tenant's unfinished tasks at one priority.
type QueueDepth struct {
	Tenant    string   `json:"tenant"`
	Priority  Priority `json:"priority"`
	Pending   int      `json:"pending"`
	Scheduled int      `json:"scheduled"`
	Ready     int      `json:"ready"`
	Leased    int      `json:"leased"`
}

// Total is the number of unfinished tasks in the row.
func (q QueueDepth) Total() int { return q.Pending + q.Scheduled + q.Ready + q.Leased }

// Throughput is completions per second over trailing windows.
type Throughput struct {
	Last10s float64 `json:"last_10s"`
	Last1m  float64 `json:"last_1m"`
}

// Snapshot is a point-in-time view of a coordinator.
type Snapshot struct {
	Time          time.Time         `json:"time"`
	NodeID        string            `json:"node_id"`
	Leader        bool              `json:"leader"`
	FencingToken  uint64            `json:"fencing_token"`
	LeaderSince   time.Time         `json:"leader_since,omitzero"`
	States        map[TaskState]int `json:"states"`
	Queues        []QueueDepth      `json:"queues"`
	DLQ           int               `json:"dlq"`
	Counters      Counters          `json:"counters"`
	Throughput    Throughput        `json:"throughput"`
	QueueWait     LatencySummary    `json:"queue_wait"`
	RunTime       LatencySummary    `json:"run_time"`
	EndToEnd      LatencySummary    `json:"end_to_end"`
	Workers       []WorkerStatus    `json:"workers"`
	WALSeq        uint64            `json:"wal_seq"`
	SinceSnapshot int               `json:"since_snapshot"`
}

type depthKey struct {
	tenant   string
	priority Priority
}

// metrics is guarded by the coordinator's mutex, except the digests,
// which lock themselves.
type metrics struct {
	c         Counters
	depth     map[depthKey]*QueueDepth
	states    map[TaskState]int
	queueWait *TDigest
	runTime   *TDigest
	endToEnd  *TDigest
	rate      rateWindow
}

func newMetrics() *metrics {
	return &metrics{
		depth:     map[depthKey]*QueueDepth{},
		states:    map[TaskState]int{},
		queueWait: NewTDigest(DefaultCompression),
		runTime:   NewTDigest(DefaultCompression),
		endToEnd:  NewTDigest(DefaultCompression),
	}
}

// account adds delta (+1/-1) for t's current state.
func (m *metrics) account(t *Task, delta int) {
	m.states[t.State] += delta
	if t.State.Terminal() {
		return
	}
	k := depthKey{t.Tenant, t.Priority}
	d := m.depth[k]
	if d == nil {
		d = &QueueDepth{Tenant: t.Tenant, Priority: t.Priority}
		m.depth[k] = d
	}
	switch t.State {
	case StatePending:
		d.Pending += delta
	case StateScheduled:
		d.Scheduled += delta
	case StateReady:
		d.Ready += delta
	case StateLeased:
		d.Leased += delta
	}
	if d.Total() == 0 {
		delete(m.depth, k)
	}
}

func (m *metrics) queues() []QueueDepth {
	out := make([]QueueDepth, 0, len(m.depth))
	for _, d := range m.depth {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tenant != out[j].Tenant {
			return out[i].Tenant < out[j].Tenant
		}
		return out[i].Priority > out[j].Priority
	})
	return out
}

func summarize(d *TDigest) LatencySummary {
	n := d.Count()
	if n == 0 {
		return LatencySummary{}
	}
	q := d.Quantiles(0.5, 0.95, 0.99)
	toD := func(sec float64) Duration {
		if math.IsNaN(sec) {
			return 0
		}
		return Duration(sec * float64(time.Second))
	}
	return LatencySummary{Count: uint64(n), P50: toD(q[0]), P95: toD(q[1]), P99: toD(q[2]), Max: toD(d.Max())}
}

// rateWindow counts events in one-second buckets over the last minute.
type rateWindow struct {
	buckets [60]uint64
	stamps  [60]int64
}

func (r *rateWindow) add(now time.Time, n uint64) {
	s := now.Unix()
	i := int(uint64(s) % 60)
	if r.stamps[i] != s {
		r.stamps[i], r.buckets[i] = s, 0
	}
	r.buckets[i] += n
}

// rate returns events per second over the last window seconds, excluding
// the current partial second.
func (r *rateWindow) rate(now time.Time, window int) float64 {
	window = min(max(window, 1), 59)
	s := now.Unix()
	var sum uint64
	for k := int64(1); k <= int64(window); k++ {
		i := int(uint64(s-k) % 60)
		if r.stamps[i] == s-k {
			sum += r.buckets[i]
		}
	}
	return float64(sum) / float64(window)
}
