package router

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm/openai"
)

func req() llm.Request {
	return llm.Request{Model: "primary-model", MaxTokens: 32000, Effort: "high", Messages: []llm.Message{llm.UserText("hello")}}
}

func status(code int) error {
	return &openai.APIError{Provider: "p", StatusCode: code, Message: "boom"}
}

func newRouter(t *testing.T, clock *fakeClock, rec *recorder, o Options, routes ...Route) *Router {
	t.Helper()
	o.Clock = clock
	if o.Jitter == nil {
		o.Jitter = func() float64 { return 0 }
	}
	if rec != nil {
		o.Observer = rec.observe
	}
	r, err := New(routes, o)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestNewValidates(t *testing.T) {
	if _, err := New(nil, Options{}); err == nil {
		t.Fatal("no routes accepted")
	}
	if _, err := New([]Route{{}}, Options{}); err == nil {
		t.Fatal("nil provider accepted")
	}
	clock := newFakeClock()
	if _, err := New([]Route{{Provider: newFake("a", clock), RPM: -1}}, Options{}); err == nil {
		t.Fatal("negative limit accepted")
	}
	r, err := New([]Route{{Provider: newFake("a", clock), Model: "m"}, {Provider: newFake("a", clock), Model: "m"}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	st := r.Routes()
	if st[0].Name != "a/m" || st[1].Name != "a/m#2" || r.Name() != "a" {
		t.Fatalf("names: %+v, Name()=%q", st, r.Name())
	}
}

func TestRetryThenSuccess(t *testing.T) {
	clock := newFakeClock()
	rec := &recorder{}
	p := newFake("anthropic", clock, step{err: status(503)}, step{err: status(500)}, step{})
	r := newRouter(t, clock, rec, Options{}, Route{Provider: p})
	resp, err := r.Stream(context.Background(), req(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Calls() != 3 || resp.Model != "primary-model" {
		t.Fatalf("calls=%d model=%q", p.Calls(), resp.Model)
	}
	// Equal jitter with Jitter()=0 waits half the exponential step.
	if got, want := clock.Sleeps(), []time.Duration{250 * time.Millisecond, 500 * time.Millisecond}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sleeps = %v, want %v", got, want)
	}
	want := []EventKind{EventAttempt, EventRetry, EventAttempt, EventRetry, EventAttempt, EventServed}
	if got := rec.kinds(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if served := rec.of(EventServed)[0]; served.FellBack || served.Attempt != 3 {
		t.Fatalf("served event %+v", served)
	}
	st := r.Routes()[0]
	if st.Requests != 3 || st.Successes != 1 || st.Failures != 2 || st.Breaker != StateClosed {
		t.Fatalf("status %+v", st)
	}
}

func TestBackoffGrowsAndCaps(t *testing.T) {
	r, err := New([]Route{{Provider: newFake("a", newFakeClock())}}, Options{
		BaseBackoff: time.Second, MaxBackoff: 5 * time.Second, Jitter: func() float64 { return 0.999999 },
	})
	if err != nil {
		t.Fatal(err)
	}
	for n, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second} {
		got := r.backoff(n, 0)
		if got > want || got < want*99/100 {
			t.Fatalf("backoff(%d) = %s, want ~%s", n, got, want)
		}
	}
	if got := r.backoff(0, 9*time.Second); got != 9*time.Second {
		t.Fatalf("Retry-After not honoured: %s", got)
	}
	if got := r.backoff(1000, 0); got > 5*time.Second {
		t.Fatalf("huge n overflowed: %s", got)
	}
}

func TestRetryAfterHonoured(t *testing.T) {
	clock := newFakeClock()
	p := newFake("a", clock, step{err: waitErr{3 * time.Second}}, step{})
	r := newRouter(t, clock, nil, Options{}, Route{Provider: p})
	if _, err := r.Stream(context.Background(), req(), nil); err != nil {
		t.Fatal(err)
	}
	if got := clock.Sleeps(); len(got) != 1 || got[0] != 3*time.Second {
		t.Fatalf("sleeps = %v, want [3s]", got)
	}
}

func TestLongRetryAfterFallsBackImmediately(t *testing.T) {
	clock := newFakeClock()
	rec := &recorder{}
	a := newFake("a", clock, step{err: waitErr{10 * time.Minute}})
	b := newFake("b", clock, step{})
	r := newRouter(t, clock, rec, Options{}, Route{Provider: a}, Route{Provider: b, Model: "fallback-model"})
	resp, err := r.Stream(context.Background(), req(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.Calls() != 1 || b.Calls() != 1 || len(clock.Sleeps()) != 0 {
		t.Fatalf("a=%d b=%d sleeps=%v", a.Calls(), b.Calls(), clock.Sleeps())
	}
	if resp.Model != "fallback-model" {
		t.Fatalf("model %q", resp.Model)
	}
}

func TestFallbackAfterRetriesExhausted(t *testing.T) {
	clock := newFakeClock()
	rec := &recorder{}
	primary := newFake("anthropic", clock, step{err: errors.New("Claude API server error (529), try again")})
	fallback := newFake("nvidia", clock, step{events: []llm.Event{{Type: llm.EventTextDelta, Text: "hi"}}})
	r := newRouter(t, clock, rec, Options{MaxRetries: 1},
		Route{Provider: primary},
		Route{Provider: fallback, Model: "mistralai/mistral-large-4-1t-a52b", MaxTokens: 8192, Effort: "none"})
	ctx, rep := WithReport(context.Background())
	var got []llm.Event
	resp, err := r.Stream(ctx, req(), func(ev llm.Event) { got = append(got, ev) })
	if err != nil {
		t.Fatal(err)
	}
	if primary.Calls() != 2 || fallback.Calls() != 1 {
		t.Fatalf("primary=%d fallback=%d", primary.Calls(), fallback.Calls())
	}
	freq := fallback.Requests()[0]
	if freq.Model != "mistralai/mistral-large-4-1t-a52b" || freq.MaxTokens != 8192 || freq.Effort != "" {
		t.Fatalf("fallback request not prepared: model=%q max=%d effort=%q", freq.Model, freq.MaxTokens, freq.Effort)
	}
	if preq := primary.Requests()[0]; preq.Model != "primary-model" || preq.MaxTokens != 32000 || preq.Effort != "high" {
		t.Fatalf("primary request changed: %+v", preq)
	}
	if resp.Model != "mistralai/mistral-large-4-1t-a52b" || len(got) != 1 {
		t.Fatalf("resp.Model=%q events=%v", resp.Model, got)
	}
	if !rep.FellBack || rep.RouteIndex != 1 || rep.Route != "nvidia/mistralai/mistral-large-4-1t-a52b" || rep.Attempts != 3 || len(rep.Failures) != 2 {
		t.Fatalf("report %+v", rep)
	}
	fb := rec.of(EventFallback)
	if len(fb) != 1 || fb[0].Route != "anthropic" || fb[0].Next != "nvidia/mistralai/mistral-large-4-1t-a52b" || fb[0].Class.Kind != KindOverloaded {
		t.Fatalf("fallback events %+v", fb)
	}
	if s := rec.of(EventServed); len(s) != 1 || !s[0].FellBack || s[0].RouteIndex != 1 {
		t.Fatalf("served events %+v", s)
	}
}

func TestNonRetryableSurfacesAtOnce(t *testing.T) {
	for _, code := range []int{400, 401, 403} {
		clock := newFakeClock()
		a := newFake("a", clock, step{err: status(code)})
		b := newFake("b", clock, step{})
		r := newRouter(t, clock, nil, Options{}, Route{Provider: a}, Route{Provider: b})
		_, err := r.Stream(context.Background(), req(), nil)
		var ae *openai.APIError
		if !errors.As(err, &ae) || ae.StatusCode != code {
			t.Fatalf("%d: got %v, want the raw provider error", code, err)
		}
		if a.Calls() != 1 || b.Calls() != 0 || len(clock.Sleeps()) != 0 {
			t.Fatalf("%d: a=%d b=%d sleeps=%v", code, a.Calls(), b.Calls(), clock.Sleeps())
		}
		if st := r.Routes()[0]; st.Breaker != StateClosed {
			t.Fatalf("%d: breaker %s", code, st.Breaker)
		}
	}
}

func TestQuotaFallsBackWithoutRetry(t *testing.T) {
	clock := newFakeClock()
	a := newFake("a", clock, step{err: &openai.APIError{StatusCode: 429, Code: "insufficient_quota"}})
	b := newFake("b", clock, step{})
	r := newRouter(t, clock, nil, Options{}, Route{Provider: a}, Route{Provider: b})
	if _, err := r.Stream(context.Background(), req(), nil); err != nil {
		t.Fatal(err)
	}
	if a.Calls() != 1 || b.Calls() != 1 {
		t.Fatalf("a=%d b=%d", a.Calls(), b.Calls())
	}
}

func TestAllRoutesFailRouteError(t *testing.T) {
	clock := newFakeClock()
	a := newFake("a", clock, step{err: status(503)})
	b := newFake("b", clock, step{err: &openai.ConnError{Provider: "b", BaseURL: "http://b", Err: errors.New("connection refused")}})
	r := newRouter(t, clock, nil, Options{MaxRetries: 1}, Route{Provider: a}, Route{Provider: b})
	_, err := r.Stream(context.Background(), req(), nil)
	var re *RouteError
	if !errors.As(err, &re) {
		t.Fatalf("got %T %v", err, err)
	}
	if len(re.Attempts) != 4 || re.Class.Kind != KindConnection {
		t.Fatalf("route error %+v", re)
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "all 2 model routes failed: a: ") || !strings.Contains(msg, "(2 attempts)") || !strings.Contains(msg, "b: ") {
		t.Fatalf("message %q", msg)
	}
	var ae *openai.APIError
	var ce *openai.ConnError
	if !errors.As(err, &ae) || !errors.As(err, &ce) {
		t.Fatal("provider errors not reachable through RouteError")
	}
	if Classify(err).Kind != KindConnection {
		t.Fatalf("Classify(RouteError) = %v", Classify(err).Kind)
	}
}

func TestSingleRouteRetriesExhaustedMessage(t *testing.T) {
	clock := newFakeClock()
	a := newFake("a", clock, step{err: status(503)})
	r := newRouter(t, clock, nil, Options{}, Route{Provider: a})
	_, err := r.Stream(context.Background(), req(), nil)
	if err == nil || !strings.HasSuffix(err.Error(), "(3 attempts)") || strings.Contains(err.Error(), "routes failed") {
		t.Fatalf("message %q", err)
	}
}

func TestNoRetryAfterStream(t *testing.T) {
	clock := newFakeClock()
	rec := &recorder{}
	a := newFake("a", clock, step{events: []llm.Event{{Type: llm.EventTextDelta, Text: "partial"}}, err: status(503)})
	b := newFake("b", clock, step{})
	r := newRouter(t, clock, rec, Options{}, Route{Provider: a}, Route{Provider: b})
	var got []string
	_, err := r.Stream(context.Background(), req(), func(ev llm.Event) { got = append(got, ev.Text) })
	var ae *openai.APIError
	if !errors.As(err, &ae) || ae.StatusCode != 503 {
		t.Fatalf("got %v", err)
	}
	if a.Calls() != 1 || b.Calls() != 0 || !reflect.DeepEqual(got, []string{"partial"}) {
		t.Fatalf("a=%d b=%d output=%v", a.Calls(), b.Calls(), got)
	}
	if len(rec.of(EventRetry)) != 0 || len(rec.of(EventFallback)) != 0 {
		t.Fatalf("retried after streaming: %v", rec.kinds())
	}
}

func TestRetryAfterStreamOptIn(t *testing.T) {
	clock := newFakeClock()
	rec := &recorder{}
	a := newFake("a", clock,
		step{events: []llm.Event{{Type: llm.EventTextDelta, Text: "partial"}}, err: status(503)},
		step{events: []llm.Event{{Type: llm.EventTextDelta, Text: "full"}}})
	r := newRouter(t, clock, rec, Options{RetryAfterStream: true}, Route{Provider: a})
	var got []string
	if _, err := r.Stream(context.Background(), req(), func(ev llm.Event) { got = append(got, ev.Text) }); err != nil {
		t.Fatal(err)
	}
	if a.Calls() != 2 || !reflect.DeepEqual(got, []string{"partial", "full"}) {
		t.Fatalf("calls=%d output=%v", a.Calls(), got)
	}
	if rt := rec.of(EventRetry); len(rt) != 1 || !rt[0].Streamed {
		t.Fatalf("retry event %+v", rt)
	}
}

func TestFirstTokenTimeout(t *testing.T) {
	clock := newFakeClock()
	rec := &recorder{}
	a := newFake("a", clock, step{wait: 10 * time.Second}, step{events: []llm.Event{{Type: llm.EventTextDelta, Text: "x"}}, wait: 5 * time.Second})
	r := newRouter(t, clock, rec, Options{FirstTokenTimeout: 2 * time.Second}, Route{Provider: a})
	if _, err := r.Stream(context.Background(), req(), nil); err != nil {
		t.Fatal(err)
	}
	if a.Calls() != 2 {
		t.Fatalf("calls=%d", a.Calls())
	}
	rt := rec.of(EventRetry)
	var ft *FirstTokenTimeoutError
	if len(rt) != 1 || !errors.As(rt[0].Err, &ft) || ft.After != 2*time.Second || rt[0].Class.Kind != KindTimeout {
		t.Fatalf("retry events %+v", rt)
	}
	// The second attempt streamed before its slow finish, so the timer
	// was stopped and did not cut it off.
}

func TestFirstTokenTimeoutRealClock(t *testing.T) {
	a := newFake("a", newFakeClock(), step{block: true})
	r, err := New([]Route{{Provider: a}}, Options{FirstTokenTimeout: 20 * time.Millisecond, MaxRetries: -1})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = r.Stream(context.Background(), req(), nil)
	var ft *FirstTokenTimeoutError
	if !errors.As(err, &ft) {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("timeout did not fire promptly")
	}
}

func TestCancelDuringBackoff(t *testing.T) {
	clock := newFakeClock()
	ctx, cancel := context.WithCancel(context.Background())
	a := newFake("a", clock, step{err: status(503)})
	b := newFake("b", clock, step{})
	r := newRouter(t, clock, nil, Options{Observer: func(_ context.Context, ev Event) {
		if ev.Kind == EventRetry {
			cancel()
		}
	}}, Route{Provider: a}, Route{Provider: b})
	_, err := r.Stream(ctx, req(), nil)
	if !errors.Is(err, context.Canceled) || a.Calls() != 1 || b.Calls() != 0 {
		t.Fatalf("err=%v a=%d b=%d", err, a.Calls(), b.Calls())
	}
}

func TestCancelDuringStream(t *testing.T) {
	clock := newFakeClock()
	a := newFake("a", clock, step{block: true})
	b := newFake("b", clock, step{})
	r := newRouter(t, clock, nil, Options{Breaker: BreakerOptions{FailureThreshold: 1}}, Route{Provider: a}, Route{Provider: b})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for a.Calls() == 0 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	_, err := r.Stream(ctx, req(), nil)
	if !errors.Is(err, context.Canceled) || b.Calls() != 0 {
		t.Fatalf("err=%v b=%d", err, b.Calls())
	}
	if st := r.Routes()[0]; st.Breaker != StateClosed {
		t.Fatalf("cancellation tripped the breaker: %s", st.Breaker)
	}
	if _, err := r.Stream(ctx, req(), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("already-cancelled ctx: %v", err)
	}
}

func TestDeadlineTooCloseFallsBack(t *testing.T) {
	clock := newFakeClock()
	a := newFake("a", clock, step{err: status(503)})
	b := newFake("b", clock, step{})
	r := newRouter(t, clock, nil, Options{BaseBackoff: time.Hour, MaxBackoff: time.Hour}, Route{Provider: a}, Route{Provider: b})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := r.Stream(ctx, req(), nil); err != nil {
		t.Fatal(err)
	}
	if a.Calls() != 1 || b.Calls() != 1 || len(clock.Sleeps()) != 0 {
		t.Fatalf("a=%d b=%d sleeps=%v", a.Calls(), b.Calls(), clock.Sleeps())
	}
}

func TestBreakerSkipsDeadRoute(t *testing.T) {
	clock := newFakeClock()
	rec := &recorder{}
	a := newFake("a", clock, step{err: status(503)}, step{err: status(503)}, step{})
	b := newFake("b", clock, step{})
	r := newRouter(t, clock, rec, Options{MaxRetries: -1, Breaker: BreakerOptions{FailureThreshold: 2, Cooldown: 30 * time.Second}},
		Route{Provider: a}, Route{Provider: b})
	for i := 0; i < 2; i++ {
		if _, err := r.Stream(context.Background(), req(), nil); err != nil {
			t.Fatal(err)
		}
	}
	if r.Routes()[0].Breaker != StateOpen {
		t.Fatalf("breaker %s after 2 failures", r.Routes()[0].Breaker)
	}
	// While open the dead route is not called at all.
	if _, err := r.Stream(context.Background(), req(), nil); err != nil {
		t.Fatal(err)
	}
	if a.Calls() != 2 || b.Calls() != 3 {
		t.Fatalf("a=%d b=%d", a.Calls(), b.Calls())
	}
	skips := rec.of(EventSkip)
	var co *CircuitOpenError
	if len(skips) != 1 || skips[0].Class.Kind != KindCircuitOpen || !errors.As(skips[0].Err, &co) {
		t.Fatalf("skip events %+v", skips)
	}
	// After the cooldown one probe goes through and closes the breaker.
	clock.Advance(30 * time.Second)
	if r.Routes()[0].Breaker != StateHalfOpen {
		t.Fatalf("breaker %s after cooldown", r.Routes()[0].Breaker)
	}
	resp, err := r.Stream(context.Background(), req(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.Calls() != 3 || resp.Message.Text() != "ok from a" || r.Routes()[0].Breaker != StateClosed {
		t.Fatalf("a=%d text=%q breaker=%s", a.Calls(), resp.Message.Text(), r.Routes()[0].Breaker)
	}
	var transitions []string
	for _, ev := range rec.of(EventBreaker) {
		transitions = append(transitions, ev.From.String()+">"+ev.To.String())
	}
	if want := []string{"closed>open", "open>half-open", "half-open>closed"}; !reflect.DeepEqual(transitions, want) {
		t.Fatalf("transitions %v, want %v", transitions, want)
	}
}

func TestAllCircuitsOpenStillTriesOnce(t *testing.T) {
	clock := newFakeClock()
	a := newFake("a", clock, step{err: status(503)}, step{})
	r := newRouter(t, clock, nil, Options{MaxRetries: -1, Breaker: BreakerOptions{FailureThreshold: 1, Cooldown: time.Minute}}, Route{Provider: a})
	if _, err := r.Stream(context.Background(), req(), nil); err == nil {
		t.Fatal("first call should fail")
	}
	if r.Routes()[0].Breaker != StateOpen {
		t.Fatal("breaker not open")
	}
	if _, err := r.Stream(context.Background(), req(), nil); err != nil {
		t.Fatalf("forced attempt failed: %v", err)
	}
	if a.Calls() != 2 || r.Routes()[0].Breaker != StateClosed {
		t.Fatalf("calls=%d breaker=%s", a.Calls(), r.Routes()[0].Breaker)
	}
}

func TestRPMWaits(t *testing.T) {
	clock := newFakeClock()
	rec := &recorder{}
	a := newFake("a", clock, step{})
	r := newRouter(t, clock, rec, Options{MaxRateWait: time.Minute}, Route{Provider: a, RPM: 2})
	for i := 0; i < 3; i++ {
		if _, err := r.Stream(context.Background(), req(), nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := clock.Sleeps(); len(got) != 1 || got[0] != 30*time.Second {
		t.Fatalf("sleeps = %v, want [30s]", got)
	}
	if w := rec.of(EventRateWait); len(w) != 1 || w[0].Delay != 30*time.Second {
		t.Fatalf("rate wait events %+v", w)
	}
}

func TestTPMSkipsToNextRouteAndSettles(t *testing.T) {
	clock := newFakeClock()
	rec := &recorder{}
	a := newFake("a", clock, step{usage: llm.Usage{InputTokens: 70, OutputTokens: 10}})
	b := newFake("b", clock, step{})
	r := newRouter(t, clock, rec, Options{MaxRateWait: 10 * time.Second, Estimator: func(llm.Request) int64 { return 80 }},
		Route{Provider: a, TPM: 100}, Route{Provider: b})
	// First call: 80 reserved, 80 used: 20 left.
	if _, err := r.Stream(context.Background(), req(), nil); err != nil {
		t.Fatal(err)
	}
	// Second call needs 80 with 20 left: a 36s wait exceeds MaxRateWait,
	// so the next route serves it without delay.
	if _, err := r.Stream(context.Background(), req(), nil); err != nil {
		t.Fatal(err)
	}
	if a.Calls() != 1 || b.Calls() != 1 || len(clock.Sleeps()) != 0 {
		t.Fatalf("a=%d b=%d sleeps=%v", a.Calls(), b.Calls(), clock.Sleeps())
	}
	var rl *RateLimitedError
	if s := rec.of(EventSkip); len(s) != 1 || s[0].Class.Kind != KindClientRateLimit || !errors.As(s[0].Err, &rl) || rl.Wait != 36*time.Second {
		t.Fatalf("skip events %+v", s)
	}
	// The skipped booking was handed back, so after 18s of refill the
	// bucket holds exactly 20 + 18*100/60 = 50 tokens.
	clock.Advance(18 * time.Second)
	if got := r.routes[0].limiter.tpm.Available(); got < 49.99 || got > 50.01 {
		t.Fatalf("tokens available = %v, want 50", got)
	}
}

func TestTPMSettleRefundsOverestimate(t *testing.T) {
	clock := newFakeClock()
	a := newFake("a", clock, step{usage: llm.Usage{InputTokens: 5, OutputTokens: 5}})
	r := newRouter(t, clock, nil, Options{MaxRateWait: time.Second, Estimator: func(llm.Request) int64 { return 80 }},
		Route{Provider: a, TPM: 100})
	for i := 0; i < 3; i++ {
		if _, err := r.Stream(context.Background(), req(), nil); err != nil {
			t.Fatal(err)
		}
	}
	if a.Calls() != 3 || len(clock.Sleeps()) != 0 {
		t.Fatalf("calls=%d sleeps=%v: overestimates were not refunded", a.Calls(), clock.Sleeps())
	}
	if got := r.routes[0].limiter.tpm.Available(); got != 70 {
		t.Fatalf("available = %v, want 70", got)
	}
}

func TestConcurrentStreams(t *testing.T) {
	clock := newFakeClock()
	a := newFake("a", clock, step{err: status(503)}, step{})
	b := newFake("b", clock, step{})
	r := newRouter(t, clock, &recorder{}, Options{MaxRetries: 1}, Route{Provider: a, RPM: 1000, TPM: 1 << 20}, Route{Provider: b})
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.Stream(context.Background(), req(), nil); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	st := r.Routes()
	if st[0].Successes+st[1].Successes != 32 {
		t.Fatalf("status %+v", st)
	}
}

func TestPrepareHookAndNilResponse(t *testing.T) {
	clock := newFakeClock()
	a := newFake("a", clock, step{})
	r := newRouter(t, clock, nil, Options{}, Route{Provider: a, Prepare: func(r *llm.Request) { r.System = "route system" }})
	orig := req()
	if _, err := r.Stream(context.Background(), orig, nil); err != nil {
		t.Fatal(err)
	}
	if a.Requests()[0].System != "route system" || orig.System != "" {
		t.Fatal("Prepare not applied to a copy")
	}
	nilp := nilProvider{}
	r2 := newRouter(t, clock, nil, Options{MaxRetries: -1}, Route{Provider: nilp})
	if _, err := r2.Stream(context.Background(), req(), nil); err == nil || !strings.Contains(err.Error(), "no response") {
		t.Fatalf("nil response: %v", err)
	}
}

type nilProvider struct{}

func (nilProvider) Name() string { return "nil" }
func (nilProvider) Stream(context.Context, llm.Request, func(llm.Event)) (*llm.Response, error) {
	return nil, nil
}
