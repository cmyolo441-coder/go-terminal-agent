package sentinel

import (
	"fmt"
	"sync"
	"time"
)

// BreakerState is a circuit breaker state.
type BreakerState uint8

// Breaker states.
const (
	// BreakerClosed lets calls through and counts consecutive failures.
	BreakerClosed BreakerState = iota
	// BreakerOpen rejects calls until its open period ends.
	BreakerOpen
	// BreakerHalfOpen lets a limited number of probe calls through.
	BreakerHalfOpen
)

func (s BreakerState) String() string {
	switch s {
	case BreakerClosed:
		return "closed"
	case BreakerOpen:
		return "open"
	case BreakerHalfOpen:
		return "half-open"
	}
	return fmt.Sprintf("breaker(%d)", uint8(s))
}

// MarshalText implements encoding.TextMarshaler.
func (s BreakerState) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// Breaker is a circuit breaker for one tool.
//
//	closed    --Failures consecutive failures-->            open
//	open      --OpenFor elapsed, next Allow-->              half-open (probe)
//	half-open --Successes probe successes-->                closed
//	half-open --any probe failure-->                        open (OpenFor doubled, ≤ MaxOpenFor)
//
// Closing resets the open period to OpenFor.
type Breaker struct {
	cfg   BreakerConfig
	clock Clock

	mu         sync.Mutex
	state      BreakerState
	failures   int // consecutive, while closed
	successes  int // probe successes, while half-open
	inflight   int // probes running, while half-open
	lastProbe  int64
	openUntil  int64
	openFor    time.Duration
	trips      uint64
	lastChange int64
}

// NewBreaker returns a closed breaker. Zero config fields take defaults.
func NewBreaker(cfg BreakerConfig, clock Clock) *Breaker {
	full, _ := Config{Breaker: cfg, Clock: clock}.withDefaults()
	return &Breaker{cfg: full.Breaker, clock: full.Clock, openFor: full.Breaker.OpenFor}
}

// Allow reports whether a call may run now. When it returns false,
// retryAfter says when the circuit will next admit a probe. A true result
// in half-open state reserves a probe slot that Record or Release frees.
func (b *Breaker) Allow() (ok bool, retryAfter time.Duration) {
	now := b.clock.Now().UnixNano()
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case BreakerClosed:
		return true, 0
	case BreakerOpen:
		if now < b.openUntil {
			return false, time.Duration(b.openUntil - now)
		}
		b.set(BreakerHalfOpen, now)
		b.successes, b.inflight = 0, 0
		fallthrough
	default: // half-open
		if b.inflight > 0 && now-b.lastProbe > int64(b.cfg.ProbeTimeout) {
			b.inflight = 0 // the probes' results never arrived
		}
		if b.inflight >= b.cfg.Probes {
			return false, time.Duration(max(int64(b.cfg.ProbeTimeout)-(now-b.lastProbe), 0))
		}
		b.inflight++
		b.lastProbe = now
		return true, 0
	}
}

// Release frees a probe slot reserved by Allow for a call that did not run.
func (b *Breaker) Release() {
	b.mu.Lock()
	if b.state == BreakerHalfOpen && b.inflight > 0 {
		b.inflight--
	}
	b.mu.Unlock()
}

// Record reports a call's outcome. It returns true when this outcome
// opened the circuit.
func (b *Breaker) Record(success bool) (tripped bool) {
	now := b.clock.Now().UnixNano()
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case BreakerClosed:
		if success {
			b.failures = 0
			return false
		}
		b.failures++
		if b.failures >= b.cfg.Failures {
			b.open(now)
			return true
		}
	case BreakerHalfOpen:
		if b.inflight > 0 {
			b.inflight--
		}
		if !success {
			b.openFor = min(b.openFor*2, b.cfg.MaxOpenFor)
			b.open(now)
			return true
		}
		b.successes++
		if b.successes >= b.cfg.Successes {
			b.set(BreakerClosed, now)
			b.failures, b.successes, b.inflight = 0, 0, 0
			b.openFor = b.cfg.OpenFor
		}
	case BreakerOpen:
		// A call admitted before the trip finished; it changes nothing.
	}
	return false
}

func (b *Breaker) open(now int64) {
	b.set(BreakerOpen, now)
	b.openUntil = now + int64(b.openFor)
	b.failures, b.successes, b.inflight = 0, 0, 0
	b.trips++
}

func (b *Breaker) set(s BreakerState, now int64) {
	b.state = s
	b.lastChange = now
}

// State returns the current state. An open breaker whose period has ended
// still reports open until the next Allow moves it to half-open.
func (b *Breaker) State() BreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// BreakerStatus is a snapshot of a breaker.
type BreakerStatus struct {
	Tool      string        `json:"tool"`
	State     BreakerState  `json:"state"`
	Failures  int           `json:"consecutive_failures,omitempty"`
	Trips     uint64        `json:"trips"`
	OpenUntil time.Time     `json:"open_until,omitzero"`
	OpenFor   time.Duration `json:"open_for"`
	Since     time.Time     `json:"since,omitzero"`
}

// Status returns a snapshot.
func (b *Breaker) Status() BreakerStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := BreakerStatus{State: b.state, Failures: b.failures, Trips: b.trips, OpenFor: b.openFor}
	if b.state == BreakerOpen {
		st.OpenUntil = time.Unix(0, b.openUntil).UTC()
	}
	if b.lastChange != 0 {
		st.Since = time.Unix(0, b.lastChange).UTC()
	}
	return st
}

// Reset closes the breaker.
func (b *Breaker) Reset() {
	now := b.clock.Now().UnixNano()
	b.mu.Lock()
	b.set(BreakerClosed, now)
	b.failures, b.successes, b.inflight = 0, 0, 0
	b.openFor = b.cfg.OpenFor
	b.mu.Unlock()
}
