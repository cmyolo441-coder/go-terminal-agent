package router

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm/openai"
)

// FuzzClassify checks that classification never panics and stays
// self-consistent for any error text, status and in-stream error.
func FuzzClassify(f *testing.F) {
	for _, s := range []string{"rate limited (429) after retries", "overloaded (529)", "insufficient_quota", "EOF", "context length exceeded",
		"(999)", "(000)", "", "model_not_found", "stream stalled", "x509: certificate signed by unknown authority"} {
		f.Add(s, 429, "rate_limit", true)
	}
	f.Fuzz(func(t *testing.T, msg string, status int, typ string, streaming bool) {
		check := func(c Classification) {
			if c.Kind < KindNone || int(c.Kind) >= len(kindNames) {
				t.Fatalf("kind out of range: %d", c.Kind)
			}
			if c.Kind.String() == "" {
				t.Fatal("empty kind name")
			}
			if c.RetryAfter < 0 {
				t.Fatalf("negative RetryAfter %v", c.RetryAfter)
			}
			_ = c.HealthFailure()
		}
		check(Classify(errors.New(msg)))
		check(classifyMessage(msg))
		check(fromStatus(status, typ))
		check(Classify(&openai.APIError{Provider: "p", StatusCode: status, Type: typ, Message: msg, Streaming: streaming}))
		_ = Kind(status).String()
	})
}

// FuzzParseRetryAfter checks that any Retry-After header gives a wait that
// is never negative and never absurdly long.
func FuzzParseRetryAfter(f *testing.F) {
	for _, s := range []string{"1", "0.5", "-3", "NaN", "Inf", "1e300", "Wed, 21 Oct 2099 07:28:00 GMT", "", " 7 "} {
		f.Add(s, "250")
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, ra, ms string) {
		h := http.Header{}
		h.Set("Retry-After", ra)
		h.Set("Retry-After-Ms", ms)
		for _, hh := range []http.Header{h, {"Retry-After": {ra}}} {
			d := parseRetryAfter(hh, now)
			if d < 0 || d > maxRetryAfter {
				t.Fatalf("Retry-After %q / ms %q gave %v", ra, ms, d)
			}
		}
	})
}
