package sentinel

import (
	"sync/atomic"
	"time"
)

// Clock tells the time. Tests use ManualClock.
type Clock interface {
	Now() time.Time
}

// SystemClock is the wall clock.
type SystemClock struct{}

// Now returns time.Now().
func (SystemClock) Now() time.Time { return time.Now() }

// ManualClock is a Clock that only moves when told to. It is safe for
// concurrent use.
type ManualClock struct {
	ns atomic.Int64
}

// NewManualClock returns a clock set to t.
func NewManualClock(t time.Time) *ManualClock {
	c := &ManualClock{}
	c.ns.Store(t.UnixNano())
	return c
}

// Now returns the clock's time.
func (c *ManualClock) Now() time.Time { return time.Unix(0, c.ns.Load()) }

// Advance moves the clock forward by d and returns the new time.
func (c *ManualClock) Advance(d time.Duration) time.Time {
	return time.Unix(0, c.ns.Add(int64(d)))
}

// Set moves the clock to t.
func (c *ManualClock) Set(t time.Time) { c.ns.Store(t.UnixNano()) }
