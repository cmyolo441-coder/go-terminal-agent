package workflow

import (
	"fmt"
	"math"
	"time"
)

// Retry policy defaults, applied when a step has a retry block.
const (
	DefaultMaxAttempts    = 3
	DefaultInitialBackoff = time.Second
	DefaultMaxBackoff     = time.Minute
	DefaultMultiplier     = 2.0
)

// retryPolicy is a resolved RetrySpec.
type retryPolicy struct {
	maxAttempts int
	initial     time.Duration
	max         time.Duration
	multiplier  float64
	retryOn     map[ErrorClass]bool
}

var noRetry = retryPolicy{maxAttempts: 1}

// compileRetry resolves defaults and validates a retry block, reporting
// problems through add.
func compileRetry(r *RetrySpec, add func(field, msg string)) retryPolicy {
	if r == nil {
		return noRetry
	}
	p := retryPolicy{
		maxAttempts: r.MaxAttempts,
		initial:     r.InitialBackoff.D(),
		max:         r.MaxBackoff.D(),
		multiplier:  r.Multiplier,
		retryOn:     map[ErrorClass]bool{},
	}
	if p.maxAttempts == 0 {
		p.maxAttempts = DefaultMaxAttempts
	}
	if p.maxAttempts < 1 || p.maxAttempts > MaxRetryAttempts {
		add("max_attempts", fmt.Sprintf("must be between 1 and %d", MaxRetryAttempts))
	}
	if p.initial < 0 || p.max < 0 {
		add("initial_backoff", "backoffs must not be negative")
	}
	if p.initial == 0 {
		p.initial = DefaultInitialBackoff
	}
	if p.max == 0 {
		p.max = max(DefaultMaxBackoff, p.initial)
	}
	if p.max < p.initial {
		add("max_backoff", "must not be less than initial_backoff")
	}
	if p.multiplier == 0 {
		p.multiplier = DefaultMultiplier
	}
	if p.multiplier < 1 || math.IsNaN(p.multiplier) || math.IsInf(p.multiplier, 0) {
		add("multiplier", "must be a finite number >= 1")
	}
	if len(r.RetryOn) == 0 {
		p.retryOn[ClassTransient] = true
		p.retryOn[ClassTimeout] = true
	}
	for _, s := range r.RetryOn {
		if s == "*" {
			p.retryOn[ClassTransient] = true
			p.retryOn[ClassTimeout] = true
			p.retryOn[ClassError] = true
			continue
		}
		c, ok := parseErrorClass(s)
		switch {
		case !ok:
			add("retry_on", fmt.Sprintf("unknown error class %q (want transient, timeout, error or *)", s))
		case c == ClassPermanent:
			add("retry_on", "permanent errors are never retried")
		default:
			p.retryOn[c] = true
		}
	}
	return p
}

// shouldRetry reports whether a failure of the given attempt (1-based) with
// the given class is retried.
func (p retryPolicy) shouldRetry(attempt int, class ErrorClass) bool {
	return attempt < p.maxAttempts && class != ClassPermanent && p.retryOn[class]
}

// backoff returns the delay before the attempt after the given one, using
// exponential backoff with full jitter: a uniformly random duration between
// zero and min(max, initial * multiplier^(attempt-1)). rnd returns a value
// in [0, 1].
func (p retryPolicy) backoff(attempt int, rnd func() float64) time.Duration {
	ceil := float64(p.initial) * math.Pow(p.multiplier, float64(attempt-1))
	if ceil > float64(p.max) || math.IsInf(ceil, 0) || math.IsNaN(ceil) {
		ceil = float64(p.max)
	}
	f := rnd()
	if f < 0 || math.IsNaN(f) {
		f = 0
	}
	if f > 1 {
		f = 1
	}
	return time.Duration(f * ceil)
}
