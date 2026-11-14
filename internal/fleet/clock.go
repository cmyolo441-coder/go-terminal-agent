package fleet

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Clock abstracts time so that lease expiry, delays and heartbeats can be
// driven deterministically in tests.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
}

// Timer is the subset of *time.Timer that fleet uses.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

// RealClock returns a Clock backed by the time package.
func RealClock() Clock { return realClock{} }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) NewTimer(d time.Duration) Timer { return realTimer{time.NewTimer(d)} }

type realTimer struct{ t *time.Timer }

func (r realTimer) C() <-chan time.Time { return r.t.C }
func (r realTimer) Stop() bool          { return r.t.Stop() }

// FakeClock is a manually advanced Clock for tests. Timers fire when
// Advance or Set moves the clock to or past their deadline.
type FakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
	added  chan struct{}
}

// NewFakeClock returns a FakeClock reading start.
func NewFakeClock(start time.Time) *FakeClock {
	return &FakeClock{now: start, added: make(chan struct{})}
}

// Now returns the fake time.
func (f *FakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// NewTimer returns a timer that fires once the fake time reaches now+d.
func (f *FakeClock) NewTimer(d time.Duration) Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &fakeTimer{clock: f, deadline: f.now.Add(d), ch: make(chan time.Time, 1), active: true}
	if d <= 0 {
		t.active = false
		t.ch <- f.now
		return t
	}
	f.timers = append(f.timers, t)
	close(f.added)
	f.added = make(chan struct{})
	return t
}

// Advance moves the fake time forward by d and fires due timers.
func (f *FakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.fireLocked()
	f.mu.Unlock()
}

// Set moves the fake time to t (which may not go backwards) and fires due
// timers.
func (f *FakeClock) Set(t time.Time) {
	f.mu.Lock()
	if t.After(f.now) {
		f.now = t
	}
	f.fireLocked()
	f.mu.Unlock()
}

// Timers reports how many timers are waiting to fire.
func (f *FakeClock) Timers() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.timers)
}

// WaitForTimers blocks until at least n timers are pending or ctx ends,
// which lets a test advance time only after the code under test is
// waiting on it.
func (f *FakeClock) WaitForTimers(ctx context.Context, n int) error {
	for {
		f.mu.Lock()
		if len(f.timers) >= n {
			f.mu.Unlock()
			return nil
		}
		ch := f.added
		f.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (f *FakeClock) fireLocked() {
	sort.SliceStable(f.timers, func(i, j int) bool { return f.timers[i].deadline.Before(f.timers[j].deadline) })
	kept := f.timers[:0]
	for _, t := range f.timers {
		if !t.deadline.After(f.now) {
			t.active = false
			select {
			case t.ch <- f.now:
			default:
			}
			continue
		}
		kept = append(kept, t)
	}
	for i := len(kept); i < len(f.timers); i++ {
		f.timers[i] = nil
	}
	f.timers = kept
}

type fakeTimer struct {
	clock    *FakeClock
	deadline time.Time
	ch       chan time.Time
	active   bool
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) Stop() bool {
	f := t.clock
	f.mu.Lock()
	defer f.mu.Unlock()
	if !t.active {
		return false
	}
	t.active = false
	for i, x := range f.timers {
		if x == t {
			f.timers = append(f.timers[:i], f.timers[i+1:]...)
			break
		}
	}
	return true
}

// sleepCtx waits for d on clk, returning ctx.Err() if ctx ends first.
func sleepCtx(ctx context.Context, clk Clock, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := clk.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C():
		return nil
	}
}
