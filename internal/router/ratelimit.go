package router

import (
	"encoding/json"
	"math"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

// Bucket is a token bucket that refills continuously at rate tokens per
// second up to capacity. Reservations may drive it negative, which is how
// a caller learns how long to wait; that keeps waiting callers in order.
// It is safe for concurrent use.
type Bucket struct {
	mu       sync.Mutex
	clock    Clock
	rate     float64
	capacity float64
	tokens   float64
	last     time.Time
}

// NewBucket returns a full bucket; perMinute is both the refill rate per
// minute and the capacity.
func NewBucket(perMinute int, clock Clock) *Bucket {
	if clock == nil {
		clock = RealClock
	}
	c := float64(perMinute)
	return &Bucket{clock: clock, rate: c / 60, capacity: c, tokens: c, last: clock.Now()}
}

func (b *Bucket) refill(now time.Time) {
	if el := now.Sub(b.last).Seconds(); el > 0 {
		b.tokens = math.Min(b.capacity, b.tokens+el*b.rate)
		b.last = now
	}
}

// Reserve takes n tokens (capped at capacity, so an oversized request
// waits for a full bucket instead of forever) and returns how long the
// caller must wait before using them.
func (b *Bucket) Reserve(n float64) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock.Now()
	b.refill(now)
	n = math.Min(n, b.capacity)
	b.tokens -= n
	if b.tokens >= 0 {
		return 0
	}
	return time.Duration(-b.tokens / b.rate * float64(time.Second))
}

// Adjust adds tokens back (positive n) or takes more (negative n), for
// cancelled reservations and for reconciling an estimate with real usage.
func (b *Bucket) Adjust(n float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill(b.clock.Now())
	b.tokens = math.Min(b.capacity, b.tokens+n)
}

// Available returns the tokens available now (negative while in debt).
func (b *Bucket) Available() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill(b.clock.Now())
	return b.tokens
}

// limiter combines a route's request and token buckets.
type limiter struct {
	rpm *Bucket
	tpm *Bucket
}

func newLimiter(rpm, tpm int, clock Clock) *limiter {
	l := &limiter{}
	if rpm > 0 {
		l.rpm = NewBucket(rpm, clock)
	}
	if tpm > 0 {
		l.tpm = NewBucket(tpm, clock)
	}
	return l
}

type reservation struct {
	l      *limiter
	tokens float64
	wait   time.Duration
}

// reserve books one request of estimated tokens.
func (l *limiter) reserve(tokens int64) reservation {
	r := reservation{l: l}
	if l.rpm != nil {
		r.wait = l.rpm.Reserve(1)
	}
	if l.tpm != nil {
		r.tokens = math.Min(float64(tokens), l.tpm.capacity)
		r.wait = max(r.wait, l.tpm.Reserve(r.tokens))
	}
	return r
}

// cancel returns a reservation that was never used.
func (r reservation) cancel() {
	if r.l.rpm != nil {
		r.l.rpm.Adjust(1)
	}
	if r.l.tpm != nil {
		r.l.tpm.Adjust(r.tokens)
	}
}

// settle reconciles the token estimate with what the call really used. A
// failed call (used < 0) refunds the tokens but keeps the request counted,
// since the server saw it.
func (r reservation) settle(used int64) {
	if r.l.tpm == nil {
		return
	}
	if used < 0 {
		r.l.tpm.Adjust(r.tokens)
		return
	}
	r.l.tpm.Adjust(r.tokens - float64(used))
}

// EstimateTokens approximates a request's input tokens at four characters
// per token over the system prompt, message content, tool inputs and tool
// schemas, plus a small per-message overhead. It is deliberately simple:
// the estimate only paces requests, and is reconciled with the real usage
// after each call.
func EstimateTokens(req llm.Request) int64 {
	chars := utf8.RuneCountInString(req.System)
	for _, m := range req.Messages {
		chars += 16
		for _, b := range m.Blocks {
			chars += utf8.RuneCountInString(b.Text) + len(b.Input)
		}
	}
	for _, t := range req.Tools {
		schema, _ := json.Marshal(t.Properties)
		chars += len(t.Name) + utf8.RuneCountInString(t.Description) + len(schema) + len(t.Required)*8
	}
	return int64((chars + 3) / 4)
}
