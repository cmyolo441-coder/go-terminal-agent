package router

import (
	"context"
	"sync"
	"time"
)

// Clock abstracts time so tests can drive backoff, cooldowns and rate
// limits deterministically.
type Clock interface {
	Now() time.Time
	// Sleep waits for d or until ctx is done, returning ctx's error then.
	Sleep(ctx context.Context, d time.Duration) error
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// RealClock is the wall clock.
var RealClock Clock = realClock{}

// BreakerState is a circuit breaker state.
type BreakerState int

// Breaker states.
const (
	// StateClosed lets every request through.
	StateClosed BreakerState = iota
	// StateOpen rejects requests until the cooldown ends.
	StateOpen
	// StateHalfOpen lets a limited number of probe requests through; a
	// success closes the breaker and a failure re-opens it.
	StateHalfOpen
)

func (s BreakerState) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	}
	return "unknown"
}

// BreakerOptions tune a Breaker. Zero fields take the defaults.
type BreakerOptions struct {
	// FailureThreshold is how many consecutive health failures open the
	// breaker (default 5).
	FailureThreshold int
	// Cooldown is how long an open breaker rejects requests (default 30s).
	// Each consecutive re-open doubles it, up to MaxCooldown.
	Cooldown time.Duration
	// MaxCooldown caps the grown cooldown (default 5m).
	MaxCooldown time.Duration
	// HalfOpenProbes is how many requests may be in flight while half-open
	// (default 1).
	HalfOpenProbes int
	// SuccessThreshold is how many probe successes close it (default 1).
	SuccessThreshold int
	// Disabled turns the breaker off: it is always closed.
	Disabled bool
}

func (o BreakerOptions) normalized() BreakerOptions {
	if o.FailureThreshold <= 0 {
		o.FailureThreshold = 5
	}
	if o.Cooldown <= 0 {
		o.Cooldown = 30 * time.Second
	}
	if o.MaxCooldown < o.Cooldown {
		o.MaxCooldown = 5 * time.Minute
		if o.MaxCooldown < o.Cooldown {
			o.MaxCooldown = o.Cooldown
		}
	}
	if o.HalfOpenProbes <= 0 {
		o.HalfOpenProbes = 1
	}
	if o.SuccessThreshold <= 0 {
		o.SuccessThreshold = 1
	}
	return o
}

// Breaker is a per-route circuit breaker. It is safe for concurrent use.
type Breaker struct {
	o        BreakerOptions
	clock    Clock
	onChange func(from, to BreakerState)

	mu        sync.Mutex
	state     BreakerState
	failures  int
	successes int
	inFlight  int
	openedAt  time.Time
	cooldown  time.Duration
	reopens   int
}

// NewBreaker builds a closed breaker. onChange, if set, is called (without
// the breaker's lock held) on every state transition.
func NewBreaker(o BreakerOptions, clock Clock, onChange func(from, to BreakerState)) *Breaker {
	if clock == nil {
		clock = RealClock
	}
	o = o.normalized()
	return &Breaker{o: o, clock: clock, onChange: onChange, cooldown: o.Cooldown}
}

// Permit is the result of Allow. A permitted request must report exactly
// one of Success, Failure or Release.
type Permit struct {
	// OK is false when the breaker rejected the request.
	OK bool
	// RetryAt is when an open breaker will next admit a probe.
	RetryAt time.Time
	probe   bool
	forced  bool
}

// Allow asks to send a request.
func (b *Breaker) Allow() Permit {
	if b.o.Disabled {
		return Permit{OK: true}
	}
	b.mu.Lock()
	from := b.state
	now := b.clock.Now()
	var p Permit
	switch b.state {
	case StateClosed:
		p = Permit{OK: true}
	case StateOpen:
		if now.Before(b.openedAt.Add(b.cooldown)) {
			p = Permit{RetryAt: b.openedAt.Add(b.cooldown)}
			break
		}
		b.state, b.successes, b.inFlight = StateHalfOpen, 0, 0
		fallthrough
	case StateHalfOpen:
		if b.inFlight < b.o.HalfOpenProbes {
			b.inFlight++
			p = Permit{OK: true, probe: true}
		} else {
			// Every probe slot is taken; admission resumes as soon as a
			// probe reports back.
			p = Permit{RetryAt: now}
		}
	}
	to := b.state
	b.mu.Unlock()
	b.notify(from, to)
	return p
}

// Force admits a request even though the breaker may be open. The router
// uses it as a last resort when every route is unavailable, so a single
// route never fails without being tried. A forced success closes the
// breaker.
func (b *Breaker) Force() Permit {
	return Permit{OK: true, forced: true}
}

// Success records a healthy response.
func (b *Breaker) Success(p Permit) {
	if b.o.Disabled || !p.OK {
		return
	}
	b.mu.Lock()
	from := b.state
	switch b.state {
	case StateClosed:
		b.failures = 0
	case StateOpen:
		if p.forced {
			b.state, b.failures, b.reopens, b.cooldown = StateClosed, 0, 0, b.o.Cooldown
		}
	case StateHalfOpen:
		if p.probe {
			b.inFlight--
		}
		b.successes++
		if b.successes >= b.o.SuccessThreshold {
			b.state, b.failures, b.reopens, b.cooldown = StateClosed, 0, 0, b.o.Cooldown
		}
	}
	to := b.state
	b.mu.Unlock()
	b.notify(from, to)
}

// Failure records a health failure (see Classification.HealthFailure).
func (b *Breaker) Failure(p Permit) {
	if b.o.Disabled || !p.OK {
		return
	}
	b.mu.Lock()
	from := b.state
	now := b.clock.Now()
	switch b.state {
	case StateClosed:
		b.failures++
		if b.failures >= b.o.FailureThreshold {
			b.state, b.openedAt, b.cooldown = StateOpen, now, b.o.Cooldown
		}
	case StateHalfOpen:
		if p.probe {
			b.inFlight--
		}
		b.reopens++
		next := b.o.Cooldown << min(b.reopens, 20)
		if next > b.o.MaxCooldown || next <= 0 {
			next = b.o.MaxCooldown
		}
		b.state, b.openedAt, b.cooldown = StateOpen, now, next
	case StateOpen:
		// A request admitted before the breaker opened (or a forced one)
		// failed too: restart the cooldown from now.
		b.openedAt = now
	}
	to := b.state
	b.mu.Unlock()
	b.notify(from, to)
}

// Release returns a permit without a verdict (the request was cancelled or
// the error said nothing about the route's health).
func (b *Breaker) Release(p Permit) {
	if b.o.Disabled || !p.OK || !p.probe {
		return
	}
	b.mu.Lock()
	if b.state == StateHalfOpen && b.inFlight > 0 {
		b.inFlight--
	}
	b.mu.Unlock()
}

// State returns the current state, moving open to half-open in the report
// when the cooldown has passed.
func (b *Breaker) State() BreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == StateOpen && !b.clock.Now().Before(b.openedAt.Add(b.cooldown)) {
		return StateHalfOpen
	}
	return b.state
}

// ConsecutiveFailures is the current failure streak while closed.
func (b *Breaker) ConsecutiveFailures() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.failures
}

func (b *Breaker) notify(from, to BreakerState) {
	if from != to && b.onChange != nil {
		b.onChange(from, to)
	}
}
