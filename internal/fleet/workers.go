package fleet

import (
	"math"
	"slices"
	"time"
)

// WorkerState is a worker's administrative state.
type WorkerState string

// Worker states.
const (
	WorkerActive      WorkerState = "active"
	WorkerDraining    WorkerState = "draining"
	WorkerQuarantined WorkerState = "quarantined"
	WorkerLost        WorkerState = "lost"
)

// HealthPolicy decides when a worker is quarantined.
//
// Each worker has a health score in [0,1]: an exponentially weighted moving
// average of its attempt outcomes (success 1, failure 0; a lease that
// expires in the worker's hands counts as two failures, since it also cost
// a visibility timeout). Permanent failures blame the task, not the worker,
// and are not scored. A worker is quarantined when its score drops below
// QuarantineBelow after MinSamples outcomes, or when it flaps (is declared
// lost, or re-registers as a new process) MaxFlaps times within
// FlapWindow. Quarantine lasts QuarantineBase, doubling with each repeat up
// to QuarantineMax; afterwards the worker is on probation with a neutral
// score.
type HealthPolicy struct {
	Alpha           float64
	MinSamples      int
	QuarantineBelow float64
	FlapWindow      time.Duration
	MaxFlaps        int
	QuarantineBase  time.Duration
	QuarantineMax   time.Duration
	// Disabled turns quarantine off (scores are still kept).
	Disabled bool
}

// DefaultHealthPolicy returns the policy used when Config.Health is zero.
func DefaultHealthPolicy() HealthPolicy {
	return HealthPolicy{
		Alpha:           0.2,
		MinSamples:      5,
		QuarantineBelow: 0.3,
		FlapWindow:      10 * time.Minute,
		MaxFlaps:        3,
		QuarantineBase:  time.Minute,
		QuarantineMax:   time.Hour,
	}
}

func (p HealthPolicy) withDefaults() HealthPolicy {
	d := DefaultHealthPolicy()
	if p.Alpha <= 0 || p.Alpha > 1 {
		p.Alpha = d.Alpha
	}
	if p.MinSamples <= 0 {
		p.MinSamples = d.MinSamples
	}
	if p.QuarantineBelow <= 0 {
		p.QuarantineBelow = d.QuarantineBelow
	}
	if p.FlapWindow <= 0 {
		p.FlapWindow = d.FlapWindow
	}
	if p.MaxFlaps <= 0 {
		p.MaxFlaps = d.MaxFlaps
	}
	if p.QuarantineBase <= 0 {
		p.QuarantineBase = d.QuarantineBase
	}
	if p.QuarantineMax < p.QuarantineBase {
		p.QuarantineMax = max(d.QuarantineMax, p.QuarantineBase)
	}
	return p
}

type outcome int

const (
	outcomeSuccess outcome = iota
	outcomeFailure
	outcomeExpired
)

// workerEntry is the coordinator's record of a worker. Worker records are
// soft state: they are not logged, and workers re-register with a new
// leader when their heartbeats are refused.
type workerEntry struct {
	id          string
	incarnation string
	version     string
	caps        Capabilities
	state       WorkerState
	registered  time.Time
	lastSeen    time.Time
	score       float64
	samples     int
	flaps       []time.Time
	quarantines int
	quarUntil   time.Time
	stateBefore WorkerState // state to restore after quarantine
	leases      map[string]struct{}
	succeeded   uint64
	failed      uint64
	expired     uint64
}

func newWorkerEntry(id string, now time.Time) *workerEntry {
	return &workerEntry{id: id, state: WorkerActive, registered: now, lastSeen: now, score: 1, leases: map[string]struct{}{}}
}

func (w *workerEntry) free() int { return max(w.caps.MaxConcurrency-len(w.leases), 0) }

// record scores an outcome; it reports whether the worker was just
// quarantined.
func (w *workerEntry) record(p HealthPolicy, o outcome, now time.Time) bool {
	switch o {
	case outcomeSuccess:
		w.succeeded++
		w.score += p.Alpha * (1 - w.score)
	case outcomeFailure:
		w.failed++
		w.score -= p.Alpha * w.score
	case outcomeExpired:
		w.expired++
		w.score -= p.Alpha * w.score
		w.score -= p.Alpha * w.score
	}
	w.samples++
	if p.Disabled || w.state == WorkerQuarantined || w.state == WorkerLost {
		return false
	}
	if w.samples >= p.MinSamples && w.score < p.QuarantineBelow {
		w.quarantine(p, now)
		return true
	}
	return false
}

// flap records a flap; it reports whether the worker was just quarantined.
func (w *workerEntry) flap(p HealthPolicy, now time.Time) bool {
	cut := now.Add(-p.FlapWindow)
	w.flaps = slices.DeleteFunc(w.flaps, func(t time.Time) bool { return t.Before(cut) })
	w.flaps = append(w.flaps, now)
	if p.Disabled || w.state == WorkerQuarantined {
		return false
	}
	if len(w.flaps) >= p.MaxFlaps {
		w.quarantine(p, now)
		return true
	}
	return false
}

func (w *workerEntry) quarantine(p HealthPolicy, now time.Time) {
	w.quarantines++
	d := time.Duration(float64(p.QuarantineBase) * math.Pow(2, float64(w.quarantines-1)))
	if d > p.QuarantineMax || d <= 0 {
		d = p.QuarantineMax
	}
	w.stateBefore = w.state
	if w.stateBefore == WorkerLost || w.stateBefore == WorkerQuarantined {
		w.stateBefore = WorkerActive
	}
	w.state = WorkerQuarantined
	w.quarUntil = now.Add(d)
}

// lost reports whether the worker was declared lost (possibly while also
// quarantined); it must re-register before it is served again.
func (w *workerEntry) lost() bool {
	return w.state == WorkerLost || (w.state == WorkerQuarantined && w.stateBefore == WorkerLost)
}

// markLost records the worker as lost, preserving a running quarantine.
func (w *workerEntry) markLost() {
	if w.state == WorkerQuarantined {
		w.stateBefore = WorkerLost
		return
	}
	w.state = WorkerLost
}

// revive undoes markLost on re-registration.
func (w *workerEntry) revive() {
	switch {
	case w.state == WorkerLost:
		w.state = WorkerActive
	case w.state == WorkerQuarantined && w.stateBefore == WorkerLost:
		w.stateBefore = WorkerActive
	}
}

// maybeRelease ends an elapsed quarantine; it reports whether it did.
func (w *workerEntry) maybeRelease(now time.Time) bool {
	if w.state != WorkerQuarantined || now.Before(w.quarUntil) {
		return false
	}
	w.state = w.stateBefore
	if w.state == "" {
		w.state = WorkerActive
	}
	w.score, w.samples, w.flaps = 0.5, 0, nil
	return true
}

// WorkerStatus is a worker as reported in metrics and listings.
type WorkerStatus struct {
	ID              string       `json:"id"`
	State           WorkerState  `json:"state"`
	Version         string       `json:"version,omitempty"`
	Capabilities    Capabilities `json:"capabilities"`
	Leases          int          `json:"leases"`
	Health          float64      `json:"health"`
	Succeeded       uint64       `json:"succeeded"`
	Failed          uint64       `json:"failed"`
	Expired         uint64       `json:"expired"`
	Flaps           int          `json:"flaps"`
	Quarantines     int          `json:"quarantines"`
	QuarantineUntil time.Time    `json:"quarantine_until,omitzero"`
	RegisteredAt    time.Time    `json:"registered_at"`
	LastSeen        time.Time    `json:"last_seen"`
}

func (w *workerEntry) status() WorkerStatus {
	caps := w.caps
	caps.Tools = slices.Clone(caps.Tools)
	if caps.Labels != nil {
		m := make(map[string]string, len(caps.Labels))
		for k, v := range caps.Labels {
			m[k] = v
		}
		caps.Labels = m
	}
	st := WorkerStatus{
		ID: w.id, State: w.state, Version: w.version, Capabilities: caps,
		Leases: len(w.leases), Health: w.score,
		Succeeded: w.succeeded, Failed: w.failed, Expired: w.expired,
		Flaps: len(w.flaps), Quarantines: w.quarantines,
		RegisteredAt: w.registered, LastSeen: w.lastSeen,
	}
	if w.state == WorkerQuarantined {
		st.QuarantineUntil = w.quarUntil
	}
	return st
}
