package router

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm/openai"
)

// Kind is the category of a model-call failure.
type Kind int

// Failure kinds.
const (
	KindNone Kind = iota
	KindUnknown
	KindRateLimit
	KindQuota
	KindOverloaded
	KindServer
	KindTimeout
	KindConnection
	KindStreamInterrupted
	KindAuth
	KindPermission
	KindBadRequest
	KindNotFound
	KindCanceled
	KindCircuitOpen
	KindClientRateLimit
)

var kindNames = [...]string{
	KindNone: "none", KindUnknown: "unknown", KindRateLimit: "rate_limit", KindQuota: "quota",
	KindOverloaded: "overloaded", KindServer: "server_error", KindTimeout: "timeout",
	KindConnection: "connection", KindStreamInterrupted: "stream_interrupted", KindAuth: "auth",
	KindPermission: "permission", KindBadRequest: "bad_request", KindNotFound: "not_found",
	KindCanceled: "canceled", KindCircuitOpen: "circuit_open", KindClientRateLimit: "client_rate_limit",
}

// String returns a stable snake_case name, suitable as a metric attribute.
func (k Kind) String() string {
	if int(k) < len(kindNames) {
		return kindNames[k]
	}
	return "kind(" + strconv.Itoa(int(k)) + ")"
}

// Classification is what the router needs to know about a failure.
type Classification struct {
	Kind Kind
	// StatusCode is the HTTP status when one is known.
	StatusCode int
	// Retryable means the same route may succeed if tried again.
	Retryable bool
	// Fallback means another route may succeed even though this one
	// should not be retried (for example an exhausted quota or a model
	// this provider does not serve). Retryable failures also fall back
	// once their retries are spent.
	Fallback bool
	// RetryAfter is the server's requested wait, when it gave one.
	RetryAfter time.Duration
}

// HealthFailure reports whether the failure says the route cannot serve
// requests right now (it is down, overloaded, out of quota or does not
// serve the model), which is what the circuit breaker counts. A rejected
// request (400, 401, 403) proves the route is up, so it does not count.
func (c Classification) HealthFailure() bool {
	switch c.Kind {
	case KindRateLimit, KindQuota, KindOverloaded, KindServer, KindTimeout, KindConnection,
		KindStreamInterrupted, KindNotFound, KindUnknown:
		return true
	}
	return false
}

// RetryAfterError lets any error carry a server-requested wait.
type RetryAfterError interface {
	error
	RetryAfter() time.Duration
}

// StatusError lets any error carry an HTTP status.
type StatusError interface {
	error
	HTTPStatus() int
}

// Classify inspects err from either built-in provider (or any provider
// whose errors expose an HTTP status) and decides whether it is worth
// retrying or falling back. Errors from the OpenAI-compatible provider
// (*openai.APIError, *openai.ConnError) and the Anthropic SDK
// (*anthropic.Error) are inspected structurally; the Anthropic provider
// rewrites most SDK errors into text, so as a last resort the status and
// well-known phrases are parsed from the message.
func Classify(err error) Classification {
	if err == nil {
		return Classification{Kind: KindNone}
	}
	c := classify(err)
	var ra RetryAfterError
	if c.RetryAfter == 0 && errors.As(err, &ra) {
		c.RetryAfter = ra.RetryAfter()
	}
	return c
}

func classify(err error) Classification {
	var ftt *FirstTokenTimeoutError
	var re *RouteError
	switch {
	case errors.As(err, &re):
		return re.Class
	case errors.As(err, &ftt):
		return Classification{Kind: KindTimeout, Retryable: true, Fallback: true}
	case errors.Is(err, context.Canceled):
		return Classification{Kind: KindCanceled}
	case errors.Is(err, context.DeadlineExceeded):
		// Only reached for a deadline the provider set itself (the
		// router checks the caller's context first).
		return Classification{Kind: KindTimeout, Retryable: true, Fallback: true}
	}

	var oe *openai.APIError
	if errors.As(err, &oe) {
		return classifyOpenAI(oe)
	}
	var ce *openai.ConnError
	if errors.As(err, &ce) {
		if ce.Timeout > 0 {
			return Classification{Kind: KindTimeout, Retryable: true, Fallback: true}
		}
		if permanentTransport(ce.Err) {
			return Classification{Kind: KindConnection, Fallback: true}
		}
		return Classification{Kind: KindConnection, Retryable: true, Fallback: true}
	}
	var ae *anthropic.Error
	if errors.As(err, &ae) {
		return classifyAnthropic(ae)
	}
	var se StatusError
	if errors.As(err, &se) {
		return fromStatus(se.HTTPStatus(), "")
	}
	if permanentTransport(err) {
		return Classification{Kind: KindConnection, Fallback: true}
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return Classification{Kind: KindTimeout, Retryable: true, Fallback: true}
	}
	var oe2 *net.OpError
	if errors.As(err, &oe2) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, io.ErrUnexpectedEOF) {
		return Classification{Kind: KindConnection, Retryable: true, Fallback: true}
	}
	return classifyMessage(err.Error())
}

// fromStatus maps an HTTP status (plus an optional error type or code) to
// a classification.
func fromStatus(code int, typ string) Classification {
	c := Classification{StatusCode: code}
	t := strings.ToLower(typ)
	switch {
	case code == http.StatusTooManyRequests && (strings.Contains(t, "quota") || strings.Contains(t, "billing")):
		c.Kind, c.Fallback = KindQuota, true
	case code == http.StatusTooManyRequests:
		c.Kind, c.Retryable, c.Fallback = KindRateLimit, true, true
	case code == http.StatusPaymentRequired:
		c.Kind, c.Fallback = KindQuota, true
	case code == http.StatusUnauthorized:
		c.Kind = KindAuth
	case code == http.StatusForbidden:
		c.Kind = KindPermission
	case code == http.StatusNotFound:
		c.Kind, c.Fallback = KindNotFound, true
	case code == http.StatusRequestTimeout || code == http.StatusGatewayTimeout:
		c.Kind, c.Retryable, c.Fallback = KindTimeout, true, true
	case code == http.StatusConflict:
		c.Kind, c.Retryable, c.Fallback = KindServer, true, true
	case code == http.StatusServiceUnavailable || code == 529:
		c.Kind, c.Retryable, c.Fallback = KindOverloaded, true, true
	case code == http.StatusNotImplemented || code == http.StatusHTTPVersionNotSupported:
		c.Kind, c.Fallback = KindServer, true
	case code >= 500:
		c.Kind, c.Retryable, c.Fallback = KindServer, true, true
	case code >= 400:
		c.Kind = KindBadRequest
	default:
		c.Kind = KindUnknown
	}
	if strings.Contains(t, "overloaded") && code >= 500 {
		c.Kind = KindOverloaded
	}
	return c
}

func classifyOpenAI(e *openai.APIError) Classification {
	typ := e.Type + " " + e.Code
	if e.Streaming && (e.StatusCode < 400) {
		// An error object inside a stream that began successfully: judge
		// it by its type, code and message.
		c := classifyMessage(typ + " " + e.Message)
		if c.Kind == KindUnknown {
			c = Classification{Kind: KindStreamInterrupted, Retryable: true, Fallback: true}
		}
		c.StatusCode = e.StatusCode
		return c
	}
	return fromStatus(e.StatusCode, typ)
}

func classifyAnthropic(e *anthropic.Error) Classification {
	typ := string(e.Type())
	switch typ {
	case "overloaded_error":
		return withRetryAfter(Classification{Kind: KindOverloaded, StatusCode: e.StatusCode, Retryable: true, Fallback: true}, e)
	case "rate_limit_error":
		return withRetryAfter(Classification{Kind: KindRateLimit, StatusCode: e.StatusCode, Retryable: true, Fallback: true}, e)
	case "api_error":
		return withRetryAfter(Classification{Kind: KindServer, StatusCode: e.StatusCode, Retryable: true, Fallback: true}, e)
	case "timeout_error":
		return Classification{Kind: KindTimeout, StatusCode: e.StatusCode, Retryable: true, Fallback: true}
	case "billing_error":
		return Classification{Kind: KindQuota, StatusCode: e.StatusCode, Fallback: true}
	case "authentication_error":
		return Classification{Kind: KindAuth, StatusCode: e.StatusCode}
	case "permission_error":
		return Classification{Kind: KindPermission, StatusCode: e.StatusCode}
	case "not_found_error":
		return Classification{Kind: KindNotFound, StatusCode: e.StatusCode, Fallback: true}
	case "invalid_request_error", "request_too_large":
		return Classification{Kind: KindBadRequest, StatusCode: e.StatusCode}
	}
	if e.StatusCode >= 400 {
		return withRetryAfter(fromStatus(e.StatusCode, typ), e)
	}
	// An in-stream error event without a recognised type.
	return Classification{Kind: KindStreamInterrupted, StatusCode: e.StatusCode, Retryable: true, Fallback: true}
}

func withRetryAfter(c Classification, e *anthropic.Error) Classification {
	if e.Response != nil {
		c.RetryAfter = parseRetryAfter(e.Response.Header, time.Now())
	}
	return c
}

// statusInParens finds "(429)" style statuses, as written by the Anthropic
// provider's error messages ("rate limited (429) after retries").
var statusInParens = regexp.MustCompile(`\((\d{3})\)`)

// classifyMessage is the last resort for errors that lost their type.
func classifyMessage(msg string) Classification {
	m := strings.ToLower(msg)
	if sm := statusInParens.FindStringSubmatch(m); sm != nil {
		code, _ := strconv.Atoi(sm[1])
		if code >= 400 && code <= 599 {
			c := fromStatus(code, m)
			if strings.Contains(m, "overloaded") && c.Retryable {
				c.Kind = KindOverloaded
			}
			return c
		}
	}
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(m, w) {
				return true
			}
		}
		return false
	}
	switch {
	case has("insufficient_quota", "quota exceeded", "billing", "credit balance", "payment required"):
		return Classification{Kind: KindQuota, Fallback: true}
	case has("overloaded"):
		return Classification{Kind: KindOverloaded, Retryable: true, Fallback: true}
	case has("rate limit", "rate_limit", "too many requests"):
		return Classification{Kind: KindRateLimit, Retryable: true, Fallback: true}
	case has("authentication", "unauthorized", "invalid api key", "invalid x-api-key", "no api key"):
		return Classification{Kind: KindAuth}
	case has("permission denied", "forbidden"):
		return Classification{Kind: KindPermission}
	case has("timed out", "timeout", "deadline exceeded", "no response from"):
		return Classification{Kind: KindTimeout, Retryable: true, Fallback: true}
	case has("stream was interrupted", "stream stalled", "stream ended before", "empty stream", "error mid-stream",
		"received error while streaming"):
		return Classification{Kind: KindStreamInterrupted, Retryable: true, Fallback: true}
	case has("connection refused", "connection reset", "broken pipe", "no such host", "cannot reach",
		"unexpected eof", "eof", "tls handshake", "network is unreachable"):
		return Classification{Kind: KindConnection, Retryable: true, Fallback: true}
	case has("server error", "internal error", "bad gateway", "service unavailable", "server_error", "api_error"):
		return Classification{Kind: KindServer, Retryable: true, Fallback: true}
	case has("context window", "context length", "prompt is too long", "request rejected", "invalid_request"):
		return Classification{Kind: KindBadRequest}
	case has("not found", "does not exist", "model_not_found"):
		return Classification{Kind: KindNotFound, Fallback: true}
	}
	return Classification{Kind: KindUnknown}
}

// permanentTransport reports transport errors that retrying cannot fix:
// certificate problems and hosts that do not exist.
func permanentTransport(err error) bool {
	var (
		unknownAuth x509.UnknownAuthorityError
		hostErr     x509.HostnameError
		certErr     x509.CertificateInvalidError
		verifyErr   *tls.CertificateVerificationError
		dnsErr      *net.DNSError
	)
	switch {
	case errors.As(err, &unknownAuth), errors.As(err, &hostErr), errors.As(err, &certErr), errors.As(err, &verifyErr):
		return true
	case errors.As(err, &dnsErr):
		return dnsErr.IsNotFound
	}
	return false
}

// parseRetryAfter reads retry-after-ms or Retry-After (seconds or an HTTP
// date).
func parseRetryAfter(h http.Header, now time.Time) time.Duration {
	if h == nil {
		return 0
	}
	if v := strings.TrimSpace(h.Get("Retry-After-Ms")); v != "" {
		if ms, err := strconv.ParseFloat(v, 64); err == nil && ms > 0 {
			return time.Duration(ms * float64(time.Millisecond))
		}
	}
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}
