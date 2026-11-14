package sentinel

import (
	"context"
	"time"
)

// DecisionFunc is an existing gate (policy, permissions, hooks) expressed as
// a verdict on an event.
type DecisionFunc func(Event) Verdict

// Guard combines the sentinel with an existing decision callback. Before
// asks the sentinel first and, unless it already refuses, the wrapped
// callback; the stricter verdict wins. After records the outcome.
type Guard struct {
	s    *Sentinel
	next DecisionFunc
}

// NewGuard wraps next (which may be nil) with s.
func NewGuard(s *Sentinel, next DecisionFunc) *Guard { return &Guard{s: s, next: next} }

// Sentinel returns the guard's sentinel.
func (g *Guard) Sentinel() *Sentinel { return g.s }

// Before decides whether ev may proceed. Call it once per action, before
// it runs, with the most specific kind (exec, file_write, file_read,
// file_delete, net, else tool_call), and once per model request with
// KindLLMRequest.
func (g *Guard) Before(ev Event) Verdict {
	v := g.s.Observe(ev)
	if v.MayRun() && g.next != nil {
		v = v.Stricter(g.next(ev))
	}
	if !v.MayRun() {
		// The call will not run now; free any breaker probe it reserved.
		g.s.release(&ev)
	}
	return v
}

// After records the outcome of an event Before allowed: a KindToolResult
// (with Err, Bytes and, for writes, Digest) after a tool runs, or a
// KindLLMResponse (with Tokens, Cost and Err) after a model call. Events of
// other kinds are converted to KindToolResult.
func (g *Guard) After(ev Event) {
	if !ev.Kind.isResult() {
		ev.Kind = KindToolResult
	}
	g.s.Observe(ev)
}

// Abandon reports that an event Before allowed did not run after all (for
// example the user declined it), freeing any breaker probe it reserved.
func (g *Guard) Abandon(ev Event) { g.s.release(&ev) }

// Wait is Before that waits out throttling: while the verdict is a
// throttle it sleeps for RetryAfter and re-checks the session posture
// (without recording the event again, so waiting never looks like a loop),
// until the event is admitted, ctx ends, or maxWait would be exceeded, in
// which case the throttle verdict is returned.
func (g *Guard) Wait(ctx context.Context, ev Event, maxWait time.Duration) Verdict {
	deadline := time.Now().Add(maxWait)
	v := g.s.Observe(ev)
	for v.Decision == DecisionThrottle {
		wait := max(v.RetryAfter, 10*time.Millisecond)
		if time.Now().Add(wait).After(deadline) {
			return v
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return v
		case <-t.C:
		}
		v = g.s.Recheck(ev)
	}
	if v.MayRun() && g.next != nil {
		v = v.Stricter(g.next(ev))
	}
	if !v.MayRun() {
		g.s.release(&ev)
	}
	return v
}
