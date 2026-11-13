package router

import (
	"reflect"
	"testing"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

func TestBreakerTransitions(t *testing.T) {
	clock := newFakeClock()
	var changes []string
	b := NewBreaker(BreakerOptions{FailureThreshold: 2, Cooldown: 10 * time.Second, MaxCooldown: 25 * time.Second}, clock,
		func(from, to BreakerState) { changes = append(changes, from.String()+">"+to.String()) })

	p := b.Allow()
	b.Failure(p)
	if b.State() != StateClosed || b.ConsecutiveFailures() != 1 {
		t.Fatalf("after 1 failure: %s/%d", b.State(), b.ConsecutiveFailures())
	}
	// A success resets the streak.
	b.Success(b.Allow())
	if b.ConsecutiveFailures() != 0 {
		t.Fatal("success did not reset the streak")
	}
	b.Failure(b.Allow())
	b.Failure(b.Allow())
	if b.State() != StateOpen {
		t.Fatalf("state %s, want open", b.State())
	}
	p = b.Allow()
	if p.OK || !p.RetryAt.Equal(clock.Now().Add(10*time.Second)) {
		t.Fatalf("open breaker admitted a request: %+v", p)
	}

	clock.Advance(10 * time.Second)
	probe := b.Allow()
	if !probe.OK {
		t.Fatal("half-open breaker refused the probe")
	}
	if second := b.Allow(); second.OK {
		t.Fatal("half-open breaker admitted a second concurrent probe")
	}
	// A failed probe re-opens with a doubled cooldown.
	b.Failure(probe)
	clock.Advance(10 * time.Second)
	if b.Allow().OK {
		t.Fatal("cooldown did not double")
	}
	clock.Advance(10 * time.Second)
	probe = b.Allow()
	if !probe.OK {
		t.Fatal("no probe after the doubled cooldown")
	}
	// Released probes free the slot without a verdict.
	b.Release(probe)
	probe = b.Allow()
	if !probe.OK {
		t.Fatal("released probe slot not reusable")
	}
	b.Failure(probe) // third open: 40s capped at 25s
	clock.Advance(25 * time.Second)
	probe = b.Allow()
	if !probe.OK {
		t.Fatal("cooldown not capped at MaxCooldown")
	}
	b.Success(probe)
	if b.State() != StateClosed {
		t.Fatalf("state %s after probe success", b.State())
	}
	want := []string{"closed>open", "open>half-open", "half-open>open", "open>half-open", "half-open>open", "open>half-open", "half-open>closed"}
	if !reflect.DeepEqual(changes, want) {
		t.Fatalf("changes %v\nwant    %v", changes, want)
	}
}

func TestBreakerDisabledAndForced(t *testing.T) {
	clock := newFakeClock()
	off := NewBreaker(BreakerOptions{Disabled: true, FailureThreshold: 1}, clock, nil)
	for i := 0; i < 5; i++ {
		off.Failure(off.Allow())
	}
	if off.State() != StateClosed || !off.Allow().OK {
		t.Fatal("disabled breaker opened")
	}
	b := NewBreaker(BreakerOptions{FailureThreshold: 1, Cooldown: time.Hour}, clock, nil)
	b.Failure(b.Allow())
	f := b.Force()
	if !f.OK {
		t.Fatal("Force refused")
	}
	b.Failure(f)
	if b.State() != StateOpen {
		t.Fatal("forced failure closed the breaker")
	}
	b.Success(b.Force())
	if b.State() != StateClosed {
		t.Fatal("forced success did not close the breaker")
	}
}

func TestBucket(t *testing.T) {
	clock := newFakeClock()
	b := NewBucket(60, clock) // 1 token per second, capacity 60
	if w := b.Reserve(60); w != 0 {
		t.Fatalf("full bucket wait %s", w)
	}
	if w := b.Reserve(3); w != 3*time.Second {
		t.Fatalf("wait %s, want 3s", w)
	}
	// Waiters queue: the next one waits behind the debt.
	if w := b.Reserve(2); w != 5*time.Second {
		t.Fatalf("wait %s, want 5s", w)
	}
	b.Adjust(5) // both cancelled
	if got := b.Available(); got != 0 {
		t.Fatalf("available %v, want 0", got)
	}
	clock.Advance(90 * time.Second)
	if got := b.Available(); got != 60 {
		t.Fatalf("refill exceeded capacity: %v", got)
	}
	// An oversized request is capped at capacity, not starved.
	if w := b.Reserve(1000); w != 0 {
		t.Fatalf("oversized wait %s", w)
	}
	if w := b.Reserve(1); w != time.Second {
		t.Fatalf("wait %s, want 1s", w)
	}
}

func TestEstimateTokens(t *testing.T) {
	small := EstimateTokens(llm.Request{Messages: []llm.Message{llm.UserText("hi")}})
	big := EstimateTokens(llm.Request{
		System:   string(make([]byte, 4000)),
		Messages: []llm.Message{llm.UserText(string(make([]byte, 4000)))},
		Tools:    []llm.ToolSpec{{Name: "bash", Description: "run", Properties: map[string]any{"command": map[string]any{"type": "string"}}}},
	})
	if small <= 0 || small > 10 {
		t.Fatalf("small estimate %d", small)
	}
	if big < 2000 || big > 2100 {
		t.Fatalf("big estimate %d, want about 2000", big)
	}
}

func TestEventKindString(t *testing.T) {
	if EventServed.String() != "served" || EventRateWait.String() != "rate_wait" || EventKind(42).String() != "event(42)" {
		t.Fatal("event kind names")
	}
	if StateHalfOpen.String() != "half-open" || BreakerState(9).String() != "unknown" {
		t.Fatal("breaker state names")
	}
}
