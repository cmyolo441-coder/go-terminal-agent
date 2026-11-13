package router

import (
	"net/http"
	"testing"
	"time"
)

func TestKindStringOutOfRange(t *testing.T) {
	if s := Kind(-1).String(); s != "kind(-1)" {
		t.Fatalf("Kind(-1) = %q", s)
	}
}

// Retry-After values that do not fit a time.Duration used to convert to
// arbitrary (often negative) durations.
func TestParseRetryAfterClamps(t *testing.T) {
	now := time.Now()
	for v, want := range map[string]time.Duration{
		"1e300": maxRetryAfter, "Inf": maxRetryAfter, "NaN": 0, "-Inf": 0, "-1": 0, "2.5": 2500 * time.Millisecond,
		"9999999999999": maxRetryAfter,
	} {
		if d := parseRetryAfter(http.Header{"Retry-After": {v}}, now); d != want {
			t.Errorf("Retry-After %q = %v, want %v", v, d, want)
		}
	}
	if d := parseRetryAfter(http.Header{"Retry-After-Ms": {"1e300"}}, now); d != maxRetryAfter {
		t.Errorf("Retry-After-Ms 1e300 = %v", d)
	}
	far := now.Add(30 * 24 * time.Hour).UTC().Format(http.TimeFormat)
	if d := parseRetryAfter(http.Header{"Retry-After": {far}}, now); d != maxRetryAfter {
		t.Errorf("far date = %v", d)
	}
}
