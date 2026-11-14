package workflow

import (
	"context"
	"slices"
	"sync"
	"time"
)

// Clock is the engine's source of time. The engine never calls time.Now or
// time.NewTimer directly, so tests can drive backoffs and timeouts with a
// FakeClock.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
}

// Timer is a one-shot timer created by a Clock.
type Timer interface {
	// C returns the channel the fire time is delivered on.
	C() <-chan time.Time
	// Stop prevents the timer from firing. It reports whether it did so.
	Stop() bool
}

// SystemClock returns the wall clock.
func SystemClock() Clock { return systemClock{} }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) NewTimer(d time.Duration) Timer { return systemTimer{time.NewTimer(d)} }

type systemTimer struct{ t *time.Timer }

func (t systemTimer) C() <-chan time.Time { return t.t.C }
func (t systemTimer) Stop() bool          { return t.t.Stop() }

// FakeClock is a manually advanced Clock for tests. It is safe for
// concurrent use.
type FakeClock struct {
	mu      sync.Mutex
	now     time.Time
	timers  []*fakeTimer
	changed chan struct{}
}

// NewFakeClock returns a FakeClock reading start.
func NewFakeClock(start time.Time) *FakeClock {
	return &FakeClock{now: start, changed: make(chan struct{})}
}

type fakeTimer struct {
	c    *FakeClock
	when time.Time
	ch   chan time.Time
}

// Now returns the clock's current time.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// NewTimer returns a timer that fires once the clock has been advanced by d.
// A timer with d <= 0 fires immediately.
func (c *FakeClock) NewTimer(d time.Duration) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{c: c, when: c.now.Add(d), ch: make(chan time.Time, 1)}
	if d <= 0 {
		t.ch <- c.now
		return t
	}
	c.timers = append(c.timers, t)
	c.notifyLocked()
	return t
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) Stop() bool {
	c := t.c
	c.mu.Lock()
	defer c.mu.Unlock()
	i := slices.Index(c.timers, t)
	if i < 0 {
		return false
	}
	c.timers = slices.Delete(c.timers, i, i+1)
	c.notifyLocked()
	return true
}

// Advance moves the clock forward by d and fires, in deadline order, every
// timer that is due.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	var due []*fakeTimer
	kept := c.timers[:0]
	for _, t := range c.timers {
		if !t.when.After(c.now) {
			due = append(due, t)
		} else {
			kept = append(kept, t)
		}
	}
	clear(c.timers[len(kept):])
	c.timers = kept
	slices.SortStableFunc(due, func(a, b *fakeTimer) int { return a.when.Compare(b.when) })
	for _, t := range due {
		t.ch <- c.now
	}
	if len(due) > 0 {
		c.notifyLocked()
	}
}

// Pending returns the remaining duration of every pending timer, shortest
// first.
func (c *FakeClock) Pending() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]time.Duration, len(c.timers))
	for i, t := range c.timers {
		out[i] = t.when.Sub(c.now)
	}
	slices.Sort(out)
	return out
}

// WaitForTimers blocks until at least n timers are pending or ctx is done.
func (c *FakeClock) WaitForTimers(ctx context.Context, n int) error {
	for {
		c.mu.Lock()
		if len(c.timers) >= n {
			c.mu.Unlock()
			return nil
		}
		ch := c.changed
		c.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (c *FakeClock) notifyLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}
