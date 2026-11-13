package openai

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// APIError is a failure the server reported, either as an HTTP error status
// before the stream started or as an error object inside the stream. Its
// Error method gives a short, actionable message; the fields keep the raw
// details for callers that want them.
type APIError struct {
	// Provider is the provider label (Options.Name).
	Provider string
	// StatusCode is the HTTP status, or the error's numeric code for an
	// in-stream error (0 when the server gave none).
	StatusCode int
	// Type, Code and Message come from the server's error object.
	Type    string
	Code    string
	Message string
	// Attempts is how many requests were made in total.
	Attempts int
	// Streaming is true when the error arrived after the stream started.
	Streaming bool

	keyEnv  string
	baseURL string
	model   string
}

// Error implements error.
func (e *APIError) Error() string {
	detail := oneLine(e.Message)
	with := func(s string) string {
		if detail == "" {
			return s
		}
		return s + ": " + detail
	}
	after := ""
	if e.Attempts > 1 {
		after = fmt.Sprintf(" after %d attempts", e.Attempts)
	}
	var msg string
	switch {
	case e.Streaming:
		msg = with("the server reported an error mid-stream")
		if e.StatusCode != 0 {
			msg = fmt.Sprintf("the server reported an error mid-stream (%d)", e.StatusCode)
			if detail != "" {
				msg += ": " + detail
			}
		}
	case e.StatusCode == http.StatusUnauthorized:
		hint := "check the API key"
		if e.keyEnv != "" {
			hint = "set " + e.keyEnv + " to a valid API key"
		}
		msg = with("authentication failed (401): " + hint)
	case e.StatusCode == http.StatusPaymentRequired:
		msg = with("payment required (402): the account has run out of credits")
	case e.StatusCode == http.StatusForbidden:
		msg = with("permission denied (403): the key cannot use this model or endpoint")
	case e.StatusCode == http.StatusNotFound:
		msg = with(fmt.Sprintf("not found (404): check the model name %q and the base URL %s", e.model, e.baseURL))
	case e.StatusCode == http.StatusRequestTimeout:
		msg = with("the server timed out the request (408)" + after)
	case e.StatusCode == http.StatusConflict:
		msg = with("request conflict (409)" + after + ", try again")
	case e.StatusCode == http.StatusRequestEntityTooLarge:
		msg = with("request too large (413): the conversation is too long, compact or clear it")
	case e.StatusCode == http.StatusTooManyRequests && isQuotaCode(e.Code, e.Type):
		msg = with("quota exceeded (429): check the account's plan and billing")
	case e.StatusCode == http.StatusTooManyRequests:
		msg = with("rate limited (429)" + after + ", try again shortly")
	case e.StatusCode == http.StatusBadRequest || e.StatusCode == http.StatusUnprocessableEntity:
		msg = with(fmt.Sprintf("request rejected (%d)", e.StatusCode))
		if hint := badRequestHint(e.Message); hint != "" {
			msg += " (" + hint + ")"
		}
	case e.StatusCode >= 500:
		msg = with(fmt.Sprintf("server error (%d)%s, try again", e.StatusCode, after))
	default:
		msg = with(fmt.Sprintf("unexpected response (%d)", e.StatusCode))
	}
	return e.Provider + ": " + msg
}

// badRequestHint adds advice for the 400s users can fix from config.
func badRequestHint(message string) string {
	m := strings.ToLower(message)
	switch {
	case strings.Contains(m, "context length") || strings.Contains(m, "context_length") ||
		strings.Contains(m, "context window") || strings.Contains(m, "too many tokens") ||
		strings.Contains(m, "prompt is too long"):
		return "the conversation exceeds the model's context window, compact or clear it"
	case strings.Contains(m, "max_tokens") || strings.Contains(m, "max_completion_tokens") ||
		strings.Contains(m, "max tokens"):
		return "lower max_tokens in the config for this model"
	case strings.Contains(m, "reasoning_effort"):
		return "this model does not take an effort level, clear effort in the config"
	case strings.Contains(m, "tool") && (strings.Contains(m, "not support") || strings.Contains(m, "unsupported")):
		return "this model does not support tool calling, pick one that does"
	}
	return ""
}

func isQuotaCode(code, typ string) bool {
	return code == "insufficient_quota" || typ == "insufficient_quota"
}

// errorEnvelope matches the error bodies of the compatible servers:
// {"error":{"message":..,"type":..,"code":..}} (OpenAI, Groq, OpenRouter),
// {"error":"text"} (Ollama), {"message":..,"type":..,"code":..} (vLLM) and
// {"detail":"text"} (FastAPI-based servers). Gemini may wrap the first form
// in a one-element array.
type errorEnvelope struct {
	Error   json.RawMessage `json:"error"`
	Message string          `json:"message"`
	Type    string          `json:"type"`
	Code    json.RawMessage `json:"code"`
	Detail  json.RawMessage `json:"detail"`
}

type errorObject struct {
	Message  string          `json:"message"`
	Type     string          `json:"type"`
	Code     json.RawMessage `json:"code"`
	Status   string          `json:"status"`
	Metadata struct {
		Raw          string `json:"raw"`
		ProviderName string `json:"provider_name"`
	} `json:"metadata"`
}

// parseErrorBody extracts message, type and code from an error body. It
// reports ok=false when the body holds no recognisable error.
func parseErrorBody(body []byte) (message, typ, code string, ok bool) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return "", "", "", false
	}
	if body[0] == '[' {
		var arr []json.RawMessage
		if json.Unmarshal(body, &arr) == nil && len(arr) > 0 {
			body = arr[0]
		}
	}
	var env errorEnvelope
	if json.Unmarshal(body, &env) != nil {
		return "", "", "", false
	}
	if len(env.Error) > 0 && !bytes.Equal(env.Error, []byte("null")) {
		var s string
		if json.Unmarshal(env.Error, &s) == nil {
			return s, "", "", s != ""
		}
		var obj errorObject
		if json.Unmarshal(env.Error, &obj) == nil {
			msg := obj.Message
			if obj.Metadata.Raw != "" && !strings.Contains(msg, obj.Metadata.Raw) {
				prefix := "upstream"
				if obj.Metadata.ProviderName != "" {
					prefix = obj.Metadata.ProviderName
				}
				msg = strings.TrimSpace(msg + " (" + prefix + ": " + obj.Metadata.Raw + ")")
			}
			typ := obj.Type
			if typ == "" {
				typ = obj.Status
			}
			return msg, typ, rawCode(obj.Code), msg != "" || typ != ""
		}
	}
	if env.Message != "" {
		return env.Message, env.Type, rawCode(env.Code), true
	}
	if len(env.Detail) > 0 {
		var s string
		if json.Unmarshal(env.Detail, &s) == nil {
			return s, "", "", s != ""
		}
		return string(env.Detail), "", "", true
	}
	return "", "", "", false
}

// rawCode renders a code that may be a string, a number or null.
func rawCode(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// maxErrorBody caps how much of an error response is read.
const maxErrorBody = 1 << 20

// newHTTPError builds an APIError from a non-2xx response and closes its body.
func (p *Provider) newHTTPError(resp *http.Response, model string) *APIError {
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	e := &APIError{
		Provider: p.opts.Name, StatusCode: resp.StatusCode,
		keyEnv: p.opts.APIKeyEnv, baseURL: p.opts.BaseURL, model: model,
	}
	if msg, typ, code, ok := parseErrorBody(body); ok {
		e.Message, e.Type, e.Code = msg, typ, code
	} else if text := oneLine(string(body)); text != "" && !strings.HasPrefix(text, "<") {
		e.Message = text
	} else {
		e.Message = http.StatusText(resp.StatusCode)
	}
	return e
}

// ConnError is a failure to get a response at all: the server could not be
// reached, the connection dropped, or no headers arrived in time.
type ConnError struct {
	Provider string
	BaseURL  string
	Attempts int
	// Timeout is set when no response headers arrived within it.
	Timeout time.Duration
	Err     error
}

// Error implements error.
func (e *ConnError) Error() string {
	after := ""
	if e.Attempts > 1 {
		after = fmt.Sprintf(" (%d attempts)", e.Attempts)
	}
	if e.Timeout > 0 {
		return fmt.Sprintf("%s: no response from %s within %s%s", e.Provider, e.BaseURL, e.Timeout, after)
	}
	msg := fmt.Sprintf("%s: cannot reach %s%s: %v", e.Provider, e.BaseURL, after, unwrapURLError(e.Err))
	if errors.Is(e.Err, syscall.ECONNREFUSED) {
		msg += " (is the server running?)"
	}
	return msg
}

// Unwrap exposes the underlying network error.
func (e *ConnError) Unwrap() error { return e.Err }

func unwrapURLError(err error) error {
	var ue interface{ Unwrap() error }
	if errors.As(err, &ue) {
		if inner := ue.Unwrap(); inner != nil {
			return inner
		}
	}
	return err
}

var errHeaderTimeout = errors.New("response header timeout")

// retryableStatus reports whether an HTTP status is worth retrying.
func retryableStatus(code int) bool {
	return code == http.StatusRequestTimeout || code == http.StatusConflict ||
		code == http.StatusTooManyRequests || code >= 500
}

// retryableTransport reports whether a transport error is worth retrying.
// Certificate problems and malformed URLs will not fix themselves.
func retryableTransport(err error) bool {
	var (
		unknownAuth x509.UnknownAuthorityError
		hostErr     x509.HostnameError
		certErr     x509.CertificateInvalidError
		verifyErr   *tls.CertificateVerificationError
		dnsErr      *net.DNSError
	)
	switch {
	case errors.As(err, &unknownAuth), errors.As(err, &hostErr), errors.As(err, &certErr), errors.As(err, &verifyErr):
		return false
	case errors.As(err, &dnsErr):
		return dnsErr.IsTemporary || dnsErr.IsTimeout
	}
	return true
}

// Backoff tuning.
const (
	baseBackoff   = 500 * time.Millisecond
	maxBackoff    = 8 * time.Second
	maxRetryAfter = 60 * time.Second
)

// backoff returns the wait before retry number n (0-based). A server
// Retry-After within maxRetryAfter wins; otherwise the wait doubles from
// baseBackoff up to maxBackoff with up to 25% jitter subtracted, so many
// clients do not retry in lockstep.
func (p *Provider) backoff(n int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 && retryAfter <= maxRetryAfter {
		return retryAfter
	}
	d := time.Duration(float64(baseBackoff) * math.Pow(2, float64(n)))
	if d > maxBackoff || d <= 0 {
		d = maxBackoff
	}
	return d - time.Duration(float64(d)*0.25*p.jitter())
}

// parseRetryAfter reads retry-after-ms (OpenAI) or Retry-After as seconds
// or an HTTP date.
func parseRetryAfter(h http.Header, now time.Time) time.Duration {
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

// send posts body, retrying transient failures, and returns the first 2xx
// response. The returned context governs the stream; cancel must be called
// once the body is consumed. Nothing is retried after this returns.
func (p *Provider) send(ctx context.Context, body []byte, model string) (*http.Response, context.Context, context.CancelCauseFunc, error) {
	for attempt := 0; ; attempt++ {
		resp, actx, cancel, err := p.attempt(ctx, body)
		if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, actx, cancel, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			if resp != nil {
				resp.Body.Close()
			}
			cancel(nil)
			return nil, nil, nil, ctxErr
		}
		var (
			failure    error
			retry      bool
			retryAfter time.Duration
		)
		if err != nil {
			ce := &ConnError{Provider: p.opts.Name, BaseURL: p.opts.BaseURL, Attempts: attempt + 1, Err: err}
			if errors.Is(context.Cause(actx), errHeaderTimeout) {
				ce.Timeout, ce.Err = p.timeout, errHeaderTimeout
			}
			failure, retry = ce, retryableTransport(err)
		} else {
			retryAfter = parseRetryAfter(resp.Header, time.Now())
			ae := p.newHTTPError(resp, model)
			ae.Attempts = attempt + 1
			failure = ae
			retry = retryableStatus(resp.StatusCode) && !isQuotaCode(ae.Code, ae.Type)
			// An explicit x-should-retry header (OpenAI) overrides the status.
			switch resp.Header.Get("X-Should-Retry") {
			case "true":
				retry = true
			case "false":
				retry = false
			}
		}
		cancel(nil)
		if !retry || attempt >= p.retries {
			return nil, nil, nil, failure
		}
		if err := p.sleep(ctx, p.backoff(attempt, retryAfter)); err != nil {
			return nil, nil, nil, err
		}
	}
}

// attempt makes one request. Its context is cancelled with errHeaderTimeout
// if headers do not arrive within the configured timeout.
func (p *Provider) attempt(ctx context.Context, body []byte) (*http.Response, context.Context, context.CancelCauseFunc, error) {
	actx, cancel := context.WithCancelCause(ctx)
	hreq, err := http.NewRequestWithContext(actx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		cancel(nil)
		return nil, actx, cancel, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "text/event-stream")
	hreq.Header.Set("User-Agent", userAgent())
	for k, v := range p.opts.ExtraHeaders {
		hreq.Header.Set(k, v)
	}
	if p.opts.APIKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+p.opts.APIKey)
	}
	var timer *time.Timer
	if p.timeout > 0 {
		timer = time.AfterFunc(p.timeout, func() { cancel(errHeaderTimeout) })
	}
	resp, err := p.client.Do(hreq)
	if timer != nil {
		timer.Stop()
	}
	return resp, actx, cancel, err
}

// oneLine collapses whitespace and caps length for error messages.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 400 {
		cut := 400
		for cut > 0 && !utf8Start(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
