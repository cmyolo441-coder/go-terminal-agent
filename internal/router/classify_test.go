package router

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm/openai"
)

func anthropicErr(t *testing.T, status int, typ string, header http.Header) *anthropic.Error {
	t.Helper()
	e := &anthropic.Error{StatusCode: status}
	if header != nil {
		e.Response = &http.Response{StatusCode: status, Header: header}
	}
	body := fmt.Sprintf(`{"type":"error","error":{"type":%q,"message":"test"}}`, typ)
	if err := e.UnmarshalJSON([]byte(body)); err != nil {
		t.Fatal(err)
	}
	return e
}

type netTimeout struct{}

func (netTimeout) Error() string   { return "i/o timeout" }
func (netTimeout) Timeout() bool   { return true }
func (netTimeout) Temporary() bool { return true }

type waitErr struct{ d time.Duration }

func (w waitErr) Error() string             { return "slow down (429)" }
func (w waitErr) RetryAfter() time.Duration { return w.d }

type statusErr struct{ code int }

func (s statusErr) Error() string   { return "custom" }
func (s statusErr) HTTPStatus() int { return s.code }

func TestClassify(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		kind      Kind
		retryable bool
		fallback  bool
		status    int
	}{
		{"nil", nil, KindNone, false, false, 0},
		{"openai 429", &openai.APIError{StatusCode: 429}, KindRateLimit, true, true, 429},
		{"openai quota", &openai.APIError{StatusCode: 429, Code: "insufficient_quota"}, KindQuota, false, true, 429},
		{"openai 402", &openai.APIError{StatusCode: 402}, KindQuota, false, true, 402},
		{"openai 500", &openai.APIError{StatusCode: 500}, KindServer, true, true, 500},
		{"openai 502", &openai.APIError{StatusCode: 502}, KindServer, true, true, 502},
		{"openai 503", &openai.APIError{StatusCode: 503}, KindOverloaded, true, true, 503},
		{"openai 501", &openai.APIError{StatusCode: 501}, KindServer, false, true, 501},
		{"openai 408", &openai.APIError{StatusCode: 408}, KindTimeout, true, true, 408},
		{"openai 400", &openai.APIError{StatusCode: 400}, KindBadRequest, false, false, 400},
		{"openai 413", &openai.APIError{StatusCode: 413}, KindBadRequest, false, false, 413},
		{"openai 401", &openai.APIError{StatusCode: 401}, KindAuth, false, false, 401},
		{"openai 403", &openai.APIError{StatusCode: 403}, KindPermission, false, false, 403},
		{"openai 404", &openai.APIError{StatusCode: 404}, KindNotFound, false, true, 404},
		{"openai wrapped", fmt.Errorf("turn: %w", &openai.APIError{StatusCode: 429}), KindRateLimit, true, true, 429},
		{"openai stream overloaded", &openai.APIError{Streaming: true, Type: "overloaded_error", Message: "Overloaded"}, KindOverloaded, true, true, 0},
		{"openai stream unknown", &openai.APIError{Streaming: true, Message: "boom"}, KindStreamInterrupted, true, true, 0},
		{"openai stream 500", &openai.APIError{Streaming: true, StatusCode: 500, Message: "x"}, KindServer, true, true, 500},
		{"conn refused", &openai.ConnError{Err: syscall.ECONNREFUSED}, KindConnection, true, true, 0},
		{"conn header timeout", &openai.ConnError{Timeout: time.Minute, Err: errors.New("response header timeout")}, KindTimeout, true, true, 0},
		{"conn bad cert", &openai.ConnError{Err: x509.UnknownAuthorityError{}}, KindConnection, false, true, 0},
		{"anthropic overloaded mid-stream", anthropicErr(t, 200, "overloaded_error", nil), KindOverloaded, true, true, 200},
		{"anthropic 529", anthropicErr(t, 529, "overloaded_error", nil), KindOverloaded, true, true, 529},
		{"anthropic rate limit", anthropicErr(t, 429, "rate_limit_error", nil), KindRateLimit, true, true, 429},
		{"anthropic api_error", anthropicErr(t, 500, "api_error", nil), KindServer, true, true, 500},
		{"anthropic billing", anthropicErr(t, 400, "billing_error", nil), KindQuota, false, true, 400},
		{"anthropic auth", anthropicErr(t, 401, "authentication_error", nil), KindAuth, false, false, 401},
		{"anthropic invalid", anthropicErr(t, 400, "invalid_request_error", nil), KindBadRequest, false, false, 400},
		{"anthropic unknown type 503", anthropicErr(t, 503, "something_new", nil), KindOverloaded, true, true, 503},
		{"anthropic provider text 429", errors.New("rate limited (429) after retries, try again shortly"), KindRateLimit, true, true, 429},
		{"anthropic provider text 529", errors.New("Claude API server error (529), try again"), KindOverloaded, true, true, 529},
		{"anthropic provider text 401", errors.New("authentication failed (401): set ANTHROPIC_API_KEY"), KindAuth, false, false, 401},
		{"anthropic provider text 400", errors.New("request rejected (400): bad"), KindBadRequest, false, false, 400},
		{"anthropic provider text 404", errors.New("not found (404), check the model name: x"), KindNotFound, false, true, 404},
		{"stream interrupted text", errors.New("nvidia: the stream was interrupted: EOF"), KindStreamInterrupted, true, true, 0},
		{"sdk stream text", errors.New(`received error while streaming: {"type":"overloaded_error"}`), KindOverloaded, true, true, 0},
		{"canceled", context.Canceled, KindCanceled, false, false, 0},
		{"wrapped canceled", fmt.Errorf("x: %w", context.Canceled), KindCanceled, false, false, 0},
		{"deadline", context.DeadlineExceeded, KindTimeout, true, true, 0},
		{"net timeout", &net.OpError{Op: "dial", Err: netTimeout{}}, KindTimeout, true, true, 0},
		{"net op", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, KindConnection, true, true, 0},
		{"unexpected eof", fmt.Errorf("read: %w", io.ErrUnexpectedEOF), KindConnection, true, true, 0},
		{"dns not found", &net.DNSError{Err: "no such host", Name: "x.invalid", IsNotFound: true}, KindConnection, false, true, 0},
		{"status interface", statusErr{503}, KindOverloaded, true, true, 503},
		{"first token", &FirstTokenTimeoutError{Route: "a", After: time.Second}, KindTimeout, true, true, 0},
		{"missing key", errors.New("nvidia: no API key: export NVIDIA_API_KEY=<your key> and try again"), KindAuth, false, false, 0},
		{"mystery", errors.New("the model sent malformed streamed content"), KindUnknown, false, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Classify(tc.err)
			if c.Kind != tc.kind || c.Retryable != tc.retryable || c.Fallback != tc.fallback || c.StatusCode != tc.status {
				t.Fatalf("Classify(%v) = %+v (kind %s), want kind %s retryable=%v fallback=%v status=%d",
					tc.err, c, c.Kind, tc.kind, tc.retryable, tc.fallback, tc.status)
			}
		})
	}
}

func TestClassifyRetryAfter(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "7")
	if c := Classify(anthropicErr(t, 429, "rate_limit_error", h)); c.RetryAfter != 7*time.Second {
		t.Fatalf("anthropic Retry-After = %s, want 7s", c.RetryAfter)
	}
	h = http.Header{}
	h.Set("Retry-After-Ms", "1500")
	if c := Classify(anthropicErr(t, 529, "overloaded_error", h)); c.RetryAfter != 1500*time.Millisecond {
		t.Fatalf("retry-after-ms = %s, want 1.5s", c.RetryAfter)
	}
	if c := Classify(fmt.Errorf("wrapped: %w", waitErr{3 * time.Second})); c.RetryAfter != 3*time.Second || c.Kind != KindRateLimit {
		t.Fatalf("RetryAfterError: %+v", c)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h = http.Header{}
	h.Set("Retry-After", now.Add(4*time.Second).Format(http.TimeFormat))
	if d := parseRetryAfter(h, now); d != 4*time.Second {
		t.Fatalf("HTTP-date Retry-After = %s, want 4s", d)
	}
}

func TestKindString(t *testing.T) {
	if KindRateLimit.String() != "rate_limit" || KindClientRateLimit.String() != "client_rate_limit" || Kind(99).String() != "kind(99)" {
		t.Fatal("unexpected kind names")
	}
	if !(Classification{Kind: KindOverloaded}).HealthFailure() || (Classification{Kind: KindBadRequest}).HealthFailure() {
		t.Fatal("HealthFailure misclassifies")
	}
}
