// Package router makes model calls reliable. A Router is an llm.Provider
// over an ordered list of routes (provider and model pairs). On a
// retryable failure (rate limits, overload, 5xx, connection errors,
// timeouts before the first token) it retries with exponential backoff and
// jitter, honouring Retry-After, and then falls back to the next route. A
// per-route circuit breaker skips a dead route for a cooldown, and
// client-side token buckets keep each route under its requests-per-minute
// and tokens-per-minute limits.
//
// Output is never duplicated: once any part of a response has streamed to
// the caller, a failure is returned as-is instead of retried, unless
// Options.RetryAfterStream opts in. Errors that retrying cannot fix (400,
// 401, 403, cancellation) surface at once.
package router

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

// Route is one way to serve a request.
type Route struct {
	// Name labels the route in events and errors; default
	// "provider/model".
	Name     string
	Provider llm.Provider
	// Model replaces the request's model; empty keeps it, so the first
	// route can follow whatever model the user picked.
	Model string
	// MaxTokens caps the request's max tokens on this route (many
	// OpenAI-compatible models reject Claude-sized limits). Zero keeps it.
	MaxTokens int64
	// Effort replaces the request's effort level; "none" clears it, empty
	// keeps it.
	Effort string
	// RPM and TPM are client-side limits in requests and estimated tokens
	// per minute; zero means unlimited.
	RPM int
	TPM int
	// Prepare, if set, adjusts the request for this route last. It gets a
	// copy of the request, but the messages are shared and must not be
	// modified in place.
	Prepare func(*llm.Request)
}

// Options tune a Router. Zero fields take the defaults.
type Options struct {
	// Name is what Name() returns; default the first route's provider
	// name.
	Name string
	// MaxRetries is how many times a route is retried after its first
	// attempt before falling back (default 2; negative means none).
	MaxRetries int
	// BaseBackoff and MaxBackoff bound the exponential backoff (defaults
	// 500ms and 20s).
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	// MaxRetryAfter is the longest server-requested wait honoured on the
	// same route; a longer Retry-After falls back at once (default 60s).
	MaxRetryAfter time.Duration
	// FirstTokenTimeout cancels an attempt that has streamed nothing
	// within it and treats it as a retryable timeout; zero disables it.
	FirstTokenTimeout time.Duration
	// RetryAfterStream allows retrying and falling back even after part
	// of a response reached the caller, who must then discard the partial
	// output (EventRetry and EventFallback carry Streamed=true).
	RetryAfterStream bool
	// MaxRateWait is the longest a request waits for a route's client-side
	// rate limit before trying the next route instead (default 30s).
	MaxRateWait time.Duration
	// Breaker tunes every route's circuit breaker.
	Breaker BreakerOptions
	// Clock and Jitter make timing deterministic in tests. Jitter returns
	// a value in [0, 1).
	Clock  Clock
	Jitter func() float64
	// Classifier overrides Classify.
	Classifier func(error) Classification
	// Estimator overrides EstimateTokens for TPM limits.
	Estimator func(llm.Request) int64
	// Observer receives routing events (attempts, retries, fallbacks,
	// skips, breaker changes, the serving route). It is called
	// synchronously and must be quick.
	Observer func(ctx context.Context, ev Event)
}

// EventKind identifies a routing event.
type EventKind int

// Routing events.
const (
	// EventAttempt: a request is being sent on a route.
	EventAttempt EventKind = iota
	// EventRetry: the route failed and will be retried after Delay.
	EventRetry
	// EventFallback: the route is given up and Next will be tried.
	EventFallback
	// EventSkip: the route was not tried (open breaker or rate limit).
	EventSkip
	// EventRateWait: the request waits Delay for the route's rate limit.
	EventRateWait
	// EventBreaker: the route's breaker changed state From -> To.
	EventBreaker
	// EventServed: the route served the request.
	EventServed
)

var eventKindNames = [...]string{"attempt", "retry", "fallback", "skip", "rate_wait", "breaker", "served"}

func (k EventKind) String() string {
	if k >= 0 && int(k) < len(eventKindNames) {
		return eventKindNames[k]
	}
	return "event(" + strconv.Itoa(int(k)) + ")"
}

// Event describes one routing step.
type Event struct {
	Kind       EventKind
	Route      string
	RouteIndex int
	Model      string
	// Attempt counts attempts on this route, from 1.
	Attempt int
	Err     error
	Class   Classification
	// Delay is the backoff (EventRetry) or rate-limit wait (EventRateWait).
	Delay time.Duration
	// Streamed is true when part of the failed response reached the
	// caller.
	Streamed bool
	// Next is the route tried after an EventFallback, when there is one.
	Next string
	// FellBack is true on EventServed when a route other than the first
	// served the request.
	FellBack bool
	// From and To are the breaker states of an EventBreaker.
	From, To BreakerState
}

// AttemptError is one failed or skipped attempt.
type AttemptError struct {
	Route   string
	Model   string
	Attempt int
	Err     error
	Class   Classification
}

// RouteError reports a request that failed after more than one attempt or
// after falling back. Unwrap exposes every attempt's error, so errors.As
// still finds provider error types.
type RouteError struct {
	Attempts []AttemptError
	// Class is the final failure's classification.
	Class Classification
	// Streamed is true when part of a response reached the caller.
	Streamed bool
}

// Error summarises each route's last failure.
func (e *RouteError) Error() string {
	type agg struct {
		name  string
		last  error
		count int
	}
	var order []*agg
	idx := map[string]*agg{}
	for _, a := range e.Attempts {
		g, ok := idx[a.Route]
		if !ok {
			g = &agg{name: a.Route}
			idx[a.Route] = g
			order = append(order, g)
		}
		g.last = a.Err
		if a.Class.Kind != KindCircuitOpen && a.Class.Kind != KindClientRateLimit {
			g.count++
		}
	}
	part := func(g *agg) string {
		s := g.last.Error()
		if g.count > 1 {
			s += fmt.Sprintf(" (%d attempts)", g.count)
		}
		return s
	}
	if len(order) == 1 {
		return part(order[0])
	}
	parts := make([]string, len(order))
	for i, g := range order {
		parts[i] = g.name + ": " + part(g)
	}
	return fmt.Sprintf("all %d model routes failed: %s", len(order), strings.Join(parts, "; "))
}

// Unwrap returns every attempt's error, last first.
func (e *RouteError) Unwrap() []error {
	out := make([]error, 0, len(e.Attempts))
	for i := len(e.Attempts) - 1; i >= 0; i-- {
		out = append(out, e.Attempts[i].Err)
	}
	return out
}

// CircuitOpenError is recorded when a route was skipped by its breaker.
type CircuitOpenError struct {
	Route   string
	RetryAt time.Time
}

func (e *CircuitOpenError) Error() string {
	return fmt.Sprintf("%s: circuit open after repeated failures, skipped until %s", e.Route, e.RetryAt.Format("15:04:05"))
}

// RateLimitedError is recorded when a route was skipped by its client-side
// rate limit.
type RateLimitedError struct {
	Route string
	Wait  time.Duration
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("%s: client-side rate limit would delay the request by %s", e.Route, e.Wait.Round(time.Millisecond))
}

// FirstTokenTimeoutError is an attempt cancelled for streaming nothing
// within Options.FirstTokenTimeout.
type FirstTokenTimeoutError struct {
	Route string
	After time.Duration
}

func (e *FirstTokenTimeoutError) Error() string {
	return fmt.Sprintf("%s: no output within %s", e.Route, e.After)
}

var errFirstToken = errors.New("first token timeout")

// RouteStatus is a route's health snapshot, for status displays.
type RouteStatus struct {
	Name      string
	Provider  string
	Model     string
	Breaker   BreakerState
	Requests  uint64
	Successes uint64
	Failures  uint64
	LastError string
	LastAt    time.Time
}

type routeState struct {
	Route
	name    string
	breaker *Breaker
	limiter *limiter

	requests, successes, failures atomic.Uint64
	mu                            sync.Mutex
	lastErr                       string
	lastAt                        time.Time
}

// Router is an llm.Provider that routes over several providers. It is
// safe for concurrent use.
type Router struct {
	o      Options
	routes []*routeState
}

// New validates the routes and builds a router.
func New(routes []Route, o Options) (*Router, error) {
	if len(routes) == 0 {
		return nil, errors.New("router: at least one route is required")
	}
	if o.MaxRetries == 0 {
		o.MaxRetries = 2
	} else if o.MaxRetries < 0 {
		o.MaxRetries = 0
	}
	if o.BaseBackoff <= 0 {
		o.BaseBackoff = 500 * time.Millisecond
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = 20 * time.Second
	}
	if o.MaxBackoff < o.BaseBackoff {
		o.MaxBackoff = o.BaseBackoff
	}
	if o.MaxRetryAfter <= 0 {
		o.MaxRetryAfter = 60 * time.Second
	}
	if o.MaxRateWait <= 0 {
		o.MaxRateWait = 30 * time.Second
	}
	if o.Clock == nil {
		o.Clock = RealClock
	}
	if o.Jitter == nil {
		o.Jitter = rand.Float64
	}
	if o.Classifier == nil {
		o.Classifier = Classify
	}
	if o.Estimator == nil {
		o.Estimator = EstimateTokens
	}
	r := &Router{o: o}
	seen := map[string]bool{}
	for i, rt := range routes {
		if rt.Provider == nil {
			return nil, fmt.Errorf("router: route %d has no provider", i+1)
		}
		if rt.RPM < 0 || rt.TPM < 0 || rt.MaxTokens < 0 {
			return nil, fmt.Errorf("router: route %d has a negative limit", i+1)
		}
		name := rt.Name
		if name == "" {
			name = rt.Provider.Name()
			if rt.Model != "" {
				name += "/" + rt.Model
			}
		}
		if seen[name] {
			name = fmt.Sprintf("%s#%d", name, i+1)
		}
		seen[name] = true
		rs := &routeState{Route: rt, name: name, limiter: newLimiter(rt.RPM, rt.TPM, o.Clock)}
		idx := i
		rs.breaker = NewBreaker(o.Breaker, o.Clock, func(from, to BreakerState) {
			r.emit(context.Background(), Event{Kind: EventBreaker, Route: name, RouteIndex: idx, Model: rt.Model, From: from, To: to})
		})
		r.routes = append(r.routes, rs)
	}
	return r, nil
}

// Name implements llm.Provider.
func (r *Router) Name() string {
	if r.o.Name != "" {
		return r.o.Name
	}
	return r.routes[0].Provider.Name()
}

// Routes returns each route's health.
func (r *Router) Routes() []RouteStatus {
	out := make([]RouteStatus, len(r.routes))
	for i, rs := range r.routes {
		rs.mu.Lock()
		out[i] = RouteStatus{
			Name: rs.name, Provider: rs.Provider.Name(), Model: rs.Model,
			Breaker: rs.breaker.State(), Requests: rs.requests.Load(),
			Successes: rs.successes.Load(), Failures: rs.failures.Load(),
			LastError: rs.lastErr, LastAt: rs.lastAt,
		}
		rs.mu.Unlock()
	}
	return out
}

// Report describes how one request was served; see WithReport.
type Report struct {
	// Route, RouteIndex and Model identify the serving route.
	Route      string
	RouteIndex int
	Model      string
	// FellBack is true when a route other than the first served it.
	FellBack bool
	// Attempts counts requests actually sent.
	Attempts int
	// Failures lists the failed and skipped attempts before the outcome.
	Failures []AttemptError
}

type reportKey struct{}

// WithReport returns a context that makes Stream fill in the returned
// Report, so a caller can show which route served a call ("fell back to
// nvidia/kimi-k3.1-pro").
func WithReport(ctx context.Context) (context.Context, *Report) {
	rep := &Report{RouteIndex: -1}
	return context.WithValue(ctx, reportKey{}, rep), rep
}

type outcome int

const (
	outServed outcome = iota
	outStop
	outNext
	outSkipped
)

// result is the outcome of trying one route.
type result struct {
	resp    *llm.Response
	err     error
	out     outcome
	readyAt time.Time
	class   Classification
}

type call struct {
	attempts []AttemptError
	sent     int
	streamed bool
	report   *Report
}

func (c *call) fail(a AttemptError) {
	c.attempts = append(c.attempts, a)
	if c.report != nil {
		c.report.Failures = append(c.report.Failures, a)
	}
}

// Stream implements llm.Provider.
func (r *Router) Stream(ctx context.Context, req llm.Request, onEvent func(llm.Event)) (*llm.Response, error) {
	if onEvent == nil {
		onEvent = func(llm.Event) {}
	}
	c := &call{}
	c.report, _ = ctx.Value(reportKey{}).(*Report)
	type deferred struct {
		idx     int
		readyAt time.Time
	}
	var skipped []deferred
	for i := range r.routes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		res := r.runRoute(ctx, i, req, onEvent, c, false)
		switch res.out {
		case outServed:
			return res.resp, nil
		case outStop:
			return nil, r.finalError(ctx, c, res.err)
		case outSkipped:
			skipped = append(skipped, deferred{i, res.readyAt})
		}
		if i+1 < len(r.routes) {
			r.emit(ctx, Event{
				Kind: EventFallback, Route: r.routes[i].name, RouteIndex: i, Model: r.routes[i].Model,
				Err: res.err, Class: res.class, Streamed: c.streamed, Next: r.routes[i+1].name,
			})
		}
	}
	if c.sent == 0 && len(skipped) > 0 {
		// Nothing was even tried: rather than fail without a request,
		// force the route that becomes available first.
		best := skipped[0]
		for _, d := range skipped[1:] {
			if d.readyAt.Before(best.readyAt) {
				best = d
			}
		}
		res := r.runRoute(ctx, best.idx, req, onEvent, c, true)
		if res.out == outServed {
			return res.resp, nil
		}
		return nil, r.finalError(ctx, c, res.err)
	}
	return nil, r.finalError(ctx, c, nil)
}

// finalError returns ctx's error on cancellation, the bare error when only
// one attempt was made, and a RouteError otherwise.
func (r *Router) finalError(ctx context.Context, c *call, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	switch len(c.attempts) {
	case 0:
		if err != nil {
			return err
		}
		return errors.New("router: no route could serve the request")
	case 1:
		return c.attempts[0].Err
	}
	last := c.attempts[len(c.attempts)-1]
	return &RouteError{Attempts: append([]AttemptError(nil), c.attempts...), Class: last.Class, Streamed: c.streamed}
}

// prepare builds the request for one route.
func (rs *routeState) prepare(req llm.Request) llm.Request {
	out := req
	out.Messages = append([]llm.Message(nil), req.Messages...)
	out.Tools = append([]llm.ToolSpec(nil), req.Tools...)
	if rs.Model != "" {
		out.Model = rs.Model
	}
	if rs.MaxTokens > 0 && (out.MaxTokens <= 0 || out.MaxTokens > rs.MaxTokens) {
		out.MaxTokens = rs.MaxTokens
	}
	switch rs.Effort {
	case "":
	case "none":
		out.Effort = ""
	default:
		out.Effort = rs.Effort
	}
	if rs.Prepare != nil {
		rs.Prepare(&out)
	}
	return out
}

// runRoute tries one route with retries.
func (r *Router) runRoute(ctx context.Context, i int, req llm.Request, onEvent func(llm.Event), c *call, force bool) result {
	rs := r.routes[i]
	rreq := rs.prepare(req)
	base := Event{Route: rs.name, RouteIndex: i, Model: rreq.Model}
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return result{err: err, out: outStop, class: Classification{Kind: KindCanceled}}
		}
		ev := base
		ev.Attempt = attempt

		var permit Permit
		if force {
			permit = rs.breaker.Force()
		} else if permit = rs.breaker.Allow(); !permit.OK {
			cls := Classification{Kind: KindCircuitOpen, Fallback: true}
			err := &CircuitOpenError{Route: rs.name, RetryAt: permit.RetryAt}
			c.fail(AttemptError{Route: rs.name, Model: rreq.Model, Attempt: attempt, Err: err, Class: cls})
			ev.Kind, ev.Err, ev.Class = EventSkip, err, cls
			r.emit(ctx, ev)
			return result{err: err, out: outSkipped, readyAt: permit.RetryAt, class: cls}
		}

		rsv := rs.limiter.reserve(r.o.Estimator(rreq))
		if rsv.wait > 0 {
			now := r.o.Clock.Now()
			if !force && (rsv.wait > r.o.MaxRateWait || !fitsDeadline(ctx, rsv.wait)) {
				rsv.cancel()
				rs.breaker.Release(permit)
				cls := Classification{Kind: KindClientRateLimit, Fallback: true}
				err := &RateLimitedError{Route: rs.name, Wait: rsv.wait}
				c.fail(AttemptError{Route: rs.name, Model: rreq.Model, Attempt: attempt, Err: err, Class: cls})
				ev.Kind, ev.Err, ev.Class, ev.Delay = EventSkip, err, cls, rsv.wait
				r.emit(ctx, ev)
				return result{err: err, out: outSkipped, readyAt: now.Add(rsv.wait), class: cls}
			}
			wev := ev
			wev.Kind, wev.Delay = EventRateWait, rsv.wait
			r.emit(ctx, wev)
			if err := r.o.Clock.Sleep(ctx, rsv.wait); err != nil {
				rsv.cancel()
				rs.breaker.Release(permit)
				return result{err: err, out: outStop, class: Classification{Kind: KindCanceled}}
			}
		}

		ev.Kind = EventAttempt
		r.emit(ctx, ev)
		c.sent++
		rs.requests.Add(1)
		if c.report != nil {
			c.report.Attempts++
		}
		resp, streamed, err := r.try(ctx, rs, rreq, onEvent)
		if err == nil {
			rsv.settle(resp.Usage.ContextTokens() + resp.Usage.OutputTokens)
			rs.breaker.Success(permit)
			rs.successes.Add(1)
			if resp.Model == "" {
				resp.Model = rreq.Model
			}
			sev := ev
			sev.Kind, sev.FellBack, sev.Model = EventServed, i > 0, resp.Model
			r.emit(ctx, sev)
			if c.report != nil {
				c.report.Route, c.report.RouteIndex, c.report.Model, c.report.FellBack = rs.name, i, resp.Model, i > 0
			}
			return result{resp: resp, out: outServed}
		}
		rsv.settle(-1)
		if ctx.Err() != nil {
			rs.breaker.Release(permit)
			return result{err: ctx.Err(), out: outStop, class: Classification{Kind: KindCanceled}}
		}
		cls := r.o.Classifier(err)
		switch {
		case cls.HealthFailure():
			rs.breaker.Failure(permit)
		case cls.Kind == KindCanceled:
			rs.breaker.Release(permit)
		default:
			// The route answered; the request itself was the problem.
			rs.breaker.Success(permit)
		}
		rs.failures.Add(1)
		rs.mu.Lock()
		rs.lastErr, rs.lastAt = err.Error(), r.o.Clock.Now()
		rs.mu.Unlock()
		c.fail(AttemptError{Route: rs.name, Model: rreq.Model, Attempt: attempt, Err: err, Class: cls})
		if streamed {
			c.streamed = true
			if !r.o.RetryAfterStream {
				return result{err: err, out: outStop, class: cls}
			}
		}
		if !cls.Retryable && !cls.Fallback {
			return result{err: err, out: outStop, class: cls}
		}
		if cls.Retryable && attempt <= r.o.MaxRetries && cls.RetryAfter <= r.o.MaxRetryAfter {
			d := r.backoff(attempt-1, cls.RetryAfter)
			if fitsDeadline(ctx, d) {
				rev := ev
				rev.Kind, rev.Err, rev.Class, rev.Delay, rev.Streamed = EventRetry, err, cls, d, streamed
				r.emit(ctx, rev)
				if serr := r.o.Clock.Sleep(ctx, d); serr != nil {
					return result{err: serr, out: outStop, class: Classification{Kind: KindCanceled}}
				}
				continue
			}
		}
		return result{err: err, out: outNext, class: cls}
	}
}

// fitsDeadline reports whether waiting d still leaves time before ctx's
// deadline. Context deadlines are wall-clock times, so this uses the real
// clock even when Options.Clock is a test clock.
func fitsDeadline(ctx context.Context, d time.Duration) bool {
	dl, ok := ctx.Deadline()
	return !ok || time.Until(dl) > d
}

// try makes one attempt, enforcing the first-token timeout and noting
// whether anything reached the caller.
func (r *Router) try(ctx context.Context, rs *routeState, req llm.Request, onEvent func(llm.Event)) (*llm.Response, bool, error) {
	actx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var streamed atomic.Bool
	var stop func() bool
	if r.o.FirstTokenTimeout > 0 {
		stop = afterFunc(r.o.Clock, r.o.FirstTokenTimeout, func() {
			if !streamed.Load() {
				cancel(errFirstToken)
			}
		})
	}
	wrapped := func(ev llm.Event) {
		if streamed.CompareAndSwap(false, true) && stop != nil {
			stop()
		}
		onEvent(ev)
	}
	resp, err := rs.Provider.Stream(actx, req, wrapped)
	if stop != nil {
		stop()
	}
	if err == nil && resp == nil {
		err = fmt.Errorf("%s: the provider returned no response", rs.name)
	}
	if err != nil && ctx.Err() == nil && errors.Is(context.Cause(actx), errFirstToken) && !streamed.Load() {
		err = &FirstTokenTimeoutError{Route: rs.name, After: r.o.FirstTokenTimeout}
	}
	if err != nil {
		return nil, streamed.Load(), err
	}
	return resp, streamed.Load(), nil
}

// TimerClock is a Clock that can also schedule callbacks; the first-token
// timeout uses it when the configured clock provides it.
type TimerClock interface {
	Clock
	AfterFunc(d time.Duration, f func()) (stop func() bool)
}

func afterFunc(c Clock, d time.Duration, f func()) func() bool {
	if tc, ok := c.(TimerClock); ok {
		return tc.AfterFunc(d, f)
	}
	return time.AfterFunc(d, f).Stop
}

// backoff is the wait before retry n (0-based): a server Retry-After wins;
// otherwise BaseBackoff doubles up to MaxBackoff, with "equal jitter"
// (half fixed, half random) so clients spread out without waiting too
// little.
func (r *Router) backoff(n int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return retryAfter
	}
	d := float64(r.o.BaseBackoff) * math.Pow(2, float64(n))
	if d > float64(r.o.MaxBackoff) || math.IsInf(d, 0) {
		d = float64(r.o.MaxBackoff)
	}
	return time.Duration(d/2 + d/2*r.o.Jitter())
}

func (r *Router) emit(ctx context.Context, ev Event) {
	if r.o.Observer != nil {
		r.o.Observer(ctx, ev)
	}
}
