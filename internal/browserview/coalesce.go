package browserview

import (
	"context"
	"hash/maphash"
	"sync"
	"time"
)

// Latest is a single-slot, latest-value-wins mailbox. Put never blocks and
// replaces any value that has not been taken yet, so a slow consumer only
// ever observes the newest value and stale values are never queued.
//
// The zero value is not usable; create one with NewLatest.
type Latest[T any] struct {
	mu      sync.Mutex
	val     T
	has     bool
	dropped uint64
	notify  chan struct{}
}

// NewLatest returns an empty mailbox.
func NewLatest[T any]() *Latest[T] {
	return &Latest[T]{notify: make(chan struct{}, 1)}
}

// Put stores v, replacing (and dropping) any value not yet taken.
func (l *Latest[T]) Put(v T) {
	l.mu.Lock()
	if l.has {
		l.dropped++
	}
	l.val, l.has = v, true
	l.mu.Unlock()
	select {
	case l.notify <- struct{}{}:
	default:
	}
}

// Ready returns a channel that receives after Put. A receive is only a hint;
// Take may still report false if another consumer won the race.
func (l *Latest[T]) Ready() <-chan struct{} { return l.notify }

// Take removes and returns the newest value.
func (l *Latest[T]) Take() (T, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	v, ok := l.val, l.has
	var zero T
	l.val, l.has = zero, false
	return v, ok
}

// Peek returns the newest value without removing it.
func (l *Latest[T]) Peek() (T, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.val, l.has
}

// Dropped reports how many values were overwritten before being taken.
func (l *Latest[T]) Dropped() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.dropped
}

// Wait blocks until a value is available or ctx is done.
func (l *Latest[T]) Wait(ctx context.Context) (T, bool) {
	for {
		if v, ok := l.Take(); ok {
			return v, true
		}
		select {
		case <-ctx.Done():
			var zero T
			return zero, false
		case <-l.notify:
		}
	}
}

// pump drains ch into l until ch closes or ctx ends. It never blocks the
// producer for longer than one Put. done is closed when pump returns.
func pump[T any](ctx context.Context, ch <-chan T, l *Latest[T], done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case <-ctx.Done():
			return
		case v, ok := <-ch:
			if !ok {
				return
			}
			l.Put(v)
		}
	}
}

// Pacer enforces a minimum interval between events (an FPS cap).
// It is not safe for concurrent use.
type Pacer struct {
	interval time.Duration
	last     time.Time
	now      func() time.Time
}

// NewPacer returns a pacer for fps events per second. fps <= 0 disables
// pacing.
func NewPacer(fps int) *Pacer {
	p := &Pacer{now: time.Now}
	if fps > 0 {
		p.interval = time.Second / time.Duration(fps)
	}
	return p
}

// Delay returns how long to wait before the next event may fire.
func (p *Pacer) Delay() time.Duration {
	if p.interval == 0 || p.last.IsZero() {
		return 0
	}
	d := p.interval - p.now().Sub(p.last)
	if d < 0 {
		return 0
	}
	return d
}

// Mark records that an event fired now.
func (p *Pacer) Mark() { p.last = p.now() }

// Wait sleeps until the next event may fire. It returns false if ctx ended.
func (p *Pacer) Wait(ctx context.Context) bool {
	d := p.Delay()
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

var hashSeed = maphash.MakeSeed()

// hashBytes returns a fast, process-local 64-bit hash of b.
func hashBytes(b []byte) uint64 { return maphash.Bytes(hashSeed, b) }

// fpsMeter computes a smoothed frames-per-second estimate.
type fpsMeter struct {
	last time.Time
	ewma float64
}

func (m *fpsMeter) tick(now time.Time) {
	if !m.last.IsZero() {
		dt := now.Sub(m.last).Seconds()
		if dt > 0 {
			inst := 1 / dt
			if m.ewma == 0 {
				m.ewma = inst
			} else {
				m.ewma = m.ewma*0.8 + inst*0.2
			}
		}
	}
	m.last = now
}

// value returns the estimate, decaying to zero when frames stop arriving.
func (m *fpsMeter) value(now time.Time) float64 {
	if m.last.IsZero() {
		return 0
	}
	idle := now.Sub(m.last).Seconds()
	if idle > 2 {
		return 0
	}
	if m.ewma > 0 && idle > 1/m.ewma*2 {
		return 1 / idle
	}
	return m.ewma
}
