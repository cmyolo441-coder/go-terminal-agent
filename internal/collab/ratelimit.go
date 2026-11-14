package collab

import (
	"math"
	"time"
)

// tokenBucket is a classic token bucket. It is not safe for concurrent
// use; each lives inside a room actor or behind a lock.
type tokenBucket struct {
	rate   float64 // tokens per second
	burst  float64
	tokens float64
	last   time.Time
}

func newTokenBucket(rate float64, burst int, now time.Time) tokenBucket {
	return tokenBucket{rate: rate, burst: float64(burst), tokens: float64(burst), last: now}
}

// allow takes one token, or reports how long until one is available.
func (b *tokenBucket) allow(now time.Time) (bool, time.Duration) {
	if b.rate <= 0 {
		return true, 0
	}
	if now.After(b.last) {
		b.tokens = math.Min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / b.rate * float64(time.Second))
	return false, max(wait, time.Millisecond)
}
