package router

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

// fakeClock is a manual clock. Sleep advances time instantly, firing any
// AfterFunc timers that fall due on the way.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
	timers []*fakeTimer
}

type fakeTimer struct {
	when    time.Time
	f       func()
	stopped bool
	fired   bool
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.sleeps = append(c.sleeps, d)
	c.mu.Unlock()
	return c.advance(ctx, d)
}

// Advance moves time forward without recording a sleep.
func (c *fakeClock) Advance(d time.Duration) { _ = c.advance(context.Background(), d) }

func (c *fakeClock) advance(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		var due []*fakeTimer
		for _, t := range c.timers {
			if !t.stopped && !t.fired && !t.when.After(target) {
				due = append(due, t)
			}
		}
		sort.Slice(due, func(i, j int) bool { return due[i].when.Before(due[j].when) })
		if len(due) == 0 {
			c.now = target
			c.mu.Unlock()
			return ctx.Err()
		}
		t := due[0]
		t.fired = true
		if t.when.After(c.now) {
			c.now = t.when
		}
		c.mu.Unlock()
		t.f()
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{when: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		was := !t.stopped && !t.fired
		t.stopped = true
		return was
	}
}

func (c *fakeClock) Sleeps() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.sleeps...)
}

// step scripts one call to a fakeProvider.
type step struct {
	events []llm.Event
	// wait is spent on the clock before the outcome (after events).
	wait time.Duration
	// block waits for the context to end.
	block bool
	err   error
	usage llm.Usage
}

type fakeProvider struct {
	name  string
	clock *fakeClock

	mu    sync.Mutex
	steps []step
	reqs  []llm.Request
}

func newFake(name string, clock *fakeClock, steps ...step) *fakeProvider {
	return &fakeProvider{name: name, clock: clock, steps: steps}
}

func (p *fakeProvider) Name() string { return p.name }

func (p *fakeProvider) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.reqs)
}

func (p *fakeProvider) Requests() []llm.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]llm.Request(nil), p.reqs...)
}

func (p *fakeProvider) Stream(ctx context.Context, req llm.Request, onEvent func(llm.Event)) (*llm.Response, error) {
	p.mu.Lock()
	n := len(p.reqs)
	p.reqs = append(p.reqs, req)
	var s step
	if len(p.steps) > 0 {
		if n < len(p.steps) {
			s = p.steps[n]
		} else {
			s = p.steps[len(p.steps)-1]
		}
	}
	p.mu.Unlock()
	for _, ev := range s.events {
		onEvent(ev)
	}
	if s.wait > 0 {
		if err := p.clock.Sleep(ctx, s.wait); err != nil {
			return nil, err
		}
	}
	if s.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if s.err != nil {
		return nil, s.err
	}
	return &llm.Response{
		Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "ok from " + p.name}}},
		StopReason: llm.StopEndTurn,
		Usage:      s.usage,
		Model:      req.Model,
	}, nil
}

// recorder collects observer events.
type recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *recorder) observe(_ context.Context, ev Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recorder) kinds() []EventKind {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]EventKind, len(r.events))
	for i, e := range r.events {
		out[i] = e.Kind
	}
	return out
}

func (r *recorder) of(k EventKind) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Event
	for _, e := range r.events {
		if e.Kind == k {
			out = append(out, e)
		}
	}
	return out
}
