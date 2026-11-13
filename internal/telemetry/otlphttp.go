package telemetry

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/version"
)

// DefaultOTLPEndpoint is the OTLP/HTTP base URL used when none is set.
const DefaultOTLPEndpoint = "http://localhost:4318"

// RetryOptions tune export retries. Zero fields take the defaults: first
// wait 1s, longest wait 30s, give up after 1 minute in total.
type RetryOptions struct {
	// Disabled turns retries off.
	Disabled        bool
	InitialInterval time.Duration
	MaxInterval     time.Duration
	MaxElapsedTime  time.Duration
}

func (r RetryOptions) normalized() RetryOptions {
	if r.InitialInterval <= 0 {
		r.InitialInterval = time.Second
	}
	if r.MaxInterval <= 0 {
		r.MaxInterval = 30 * time.Second
	}
	if r.MaxElapsedTime <= 0 {
		r.MaxElapsedTime = time.Minute
	}
	return r
}

// OTLPOptions configure an OTLPExporter.
type OTLPOptions struct {
	// Endpoint is the OTLP/HTTP base URL; signals post to
	// {Endpoint}/v1/traces and {Endpoint}/v1/metrics.
	Endpoint string
	// TracesURL and MetricsURL, when set, are used as-is for that signal.
	TracesURL  string
	MetricsURL string
	// Headers go on every request (for example an API key header);
	// TracesHeaders and MetricsHeaders add to or override them per signal.
	Headers        map[string]string
	TracesHeaders  map[string]string
	MetricsHeaders map[string]string
	// Timeout bounds each HTTP request (default 10s).
	Timeout time.Duration
	// Compression is "gzip" or "none" (default "none").
	Compression string
	Retry       RetryOptions
	HTTPClient  *http.Client
}

// OTLPOptionsFromEnv reads the standard OTEL_EXPORTER_OTLP_* variables:
// ENDPOINT, TRACES_ENDPOINT, METRICS_ENDPOINT, HEADERS, TRACES_HEADERS,
// METRICS_HEADERS, TIMEOUT, COMPRESSION and PROTOCOL. Only the HTTP
// protocols are supported; both are served with JSON, which every OTLP/HTTP
// receiver accepts.
func OTLPOptionsFromEnv() (OTLPOptions, error) {
	var o OTLPOptions
	var errs []error
	o.Endpoint = strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	o.TracesURL = strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"))
	o.MetricsURL = strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"))
	headers := func(name string) map[string]string {
		v := os.Getenv(name)
		if strings.TrimSpace(v) == "" {
			return nil
		}
		pairs, err := parseKeyValueList(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s ignored: %w", name, err))
			return nil
		}
		m := make(map[string]string, len(pairs))
		for _, p := range pairs {
			m[p[0]] = p[1]
		}
		return m
	}
	o.Headers = headers("OTEL_EXPORTER_OTLP_HEADERS")
	o.TracesHeaders = headers("OTEL_EXPORTER_OTLP_TRACES_HEADERS")
	o.MetricsHeaders = headers("OTEL_EXPORTER_OTLP_METRICS_HEADERS")
	if v := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_TIMEOUT")); v != "" {
		ms, err := strconv.Atoi(v)
		if err != nil || ms <= 0 {
			errs = append(errs, fmt.Errorf("OTEL_EXPORTER_OTLP_TIMEOUT %q ignored: not a positive number of milliseconds", v))
		} else {
			o.Timeout = time.Duration(ms) * time.Millisecond
		}
	}
	o.Compression = strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_COMPRESSION")))
	switch p := strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL"))); p {
	case "", "http/json", "http/protobuf":
	default:
		errs = append(errs, fmt.Errorf("OTEL_EXPORTER_OTLP_PROTOCOL %q is not supported (use http/json)", p))
	}
	return o, errors.Join(errs...)
}

// ExportError is a failed OTLP export.
type ExportError struct {
	Signal     string
	URL        string
	StatusCode int
	Message    string
	Attempts   int
	Err        error
}

// Error implements error.
func (e *ExportError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "otlp %s export to %s failed", e.Signal, e.URL)
	if e.StatusCode != 0 {
		fmt.Fprintf(&b, " (HTTP %d)", e.StatusCode)
	}
	if e.Attempts > 1 {
		fmt.Fprintf(&b, " after %d attempts", e.Attempts)
	}
	if e.Message != "" {
		b.WriteString(": " + e.Message)
	} else if e.Err != nil {
		b.WriteString(": " + e.Err.Error())
	}
	return b.String()
}

// Unwrap returns the transport error, if any.
func (e *ExportError) Unwrap() error { return e.Err }

// ErrExporterShutdown is returned by exports after Shutdown.
var ErrExporterShutdown = errors.New("telemetry: exporter is shut down")

// ExporterStats are an exporter's counters.
type ExporterStats struct {
	Requests           uint64
	Retries            uint64
	Failures           uint64
	BytesSent          uint64
	RejectedSpans      uint64
	RejectedDataPoints uint64
}

// OTLPExporter sends traces and metrics over OTLP/HTTP with JSON encoding.
// It implements SpanExporter and MetricExporter.
type OTLPExporter struct {
	o          OTLPOptions
	tracesURL  string
	metricsURL string
	client     *http.Client
	shutdown   atomic.Bool

	// sleep and jitter are replaceable in tests.
	sleep  func(ctx context.Context, d time.Duration) error
	jitter func() float64
	now    func() time.Time

	requests, retries, failures, bytes, rejSpans, rejPoints atomic.Uint64
}

// NewOTLPExporter validates o and builds an exporter.
func NewOTLPExporter(o OTLPOptions) (*OTLPExporter, error) {
	base := strings.TrimSpace(o.Endpoint)
	if base == "" {
		base = DefaultOTLPEndpoint
	}
	signalURL := func(explicit, path string) (string, error) {
		u := strings.TrimSpace(explicit)
		if u == "" {
			u = strings.TrimRight(base, "/") + path
		}
		parsed, err := url.Parse(u)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return "", fmt.Errorf("telemetry: OTLP endpoint %q must be an absolute http(s) URL", u)
		}
		return u, nil
	}
	tu, err := signalURL(o.TracesURL, "/v1/traces")
	if err != nil {
		return nil, err
	}
	mu, err := signalURL(o.MetricsURL, "/v1/metrics")
	if err != nil {
		return nil, err
	}
	switch o.Compression {
	case "", "none", "gzip":
	default:
		return nil, fmt.Errorf("telemetry: unsupported OTLP compression %q (use gzip or none)", o.Compression)
	}
	for _, hs := range []map[string]string{o.Headers, o.TracesHeaders, o.MetricsHeaders} {
		for k, v := range hs {
			if k == "" || strings.ContainsAny(k, " \t\r\n:") || strings.ContainsAny(v, "\r\n\x00") {
				return nil, fmt.Errorf("telemetry: invalid OTLP header %q", k)
			}
		}
	}
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	o.Retry = o.Retry.normalized()
	client := o.HTTPClient
	if client == nil {
		client = &http.Client{Transport: http.DefaultTransport}
	}
	return &OTLPExporter{
		o: o, tracesURL: tu, metricsURL: mu, client: client,
		sleep: sleepCtx, jitter: rand.Float64, now: time.Now,
	}, nil
}

// TracesURL is where spans are posted.
func (e *OTLPExporter) TracesURL() string { return e.tracesURL }

// MetricsURL is where metrics are posted.
func (e *OTLPExporter) MetricsURL() string { return e.metricsURL }

// Stats returns the exporter's counters.
func (e *OTLPExporter) Stats() ExporterStats {
	return ExporterStats{
		Requests: e.requests.Load(), Retries: e.retries.Load(), Failures: e.failures.Load(),
		BytesSent: e.bytes.Load(), RejectedSpans: e.rejSpans.Load(), RejectedDataPoints: e.rejPoints.Load(),
	}
}

// ExportSpans implements SpanExporter.
func (e *OTLPExporter) ExportSpans(ctx context.Context, spans []SpanData) error {
	if len(spans) == 0 {
		return nil
	}
	body, err := encodeTraces(spans)
	if err != nil {
		return fmt.Errorf("telemetry: encode spans: %w", err)
	}
	return e.export(ctx, "traces", e.tracesURL, merge(e.o.Headers, e.o.TracesHeaders), body)
}

// ExportMetrics implements MetricExporter.
func (e *OTLPExporter) ExportMetrics(ctx context.Context, rm *ResourceMetrics) error {
	if rm == nil || len(rm.ScopeMetrics) == 0 {
		return nil
	}
	body, err := encodeMetrics(rm)
	if err != nil {
		return fmt.Errorf("telemetry: encode metrics: %w", err)
	}
	return e.export(ctx, "metrics", e.metricsURL, merge(e.o.Headers, e.o.MetricsHeaders), body)
}

// Shutdown stops further exports and releases idle connections.
func (e *OTLPExporter) Shutdown(context.Context) error {
	if e.shutdown.CompareAndSwap(false, true) {
		e.client.CloseIdleConnections()
	}
	return nil
}

func merge(a, b map[string]string) map[string]string {
	out := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func (e *OTLPExporter) export(ctx context.Context, signal, target string, headers map[string]string, body []byte) error {
	if e.shutdown.Load() {
		return ErrExporterShutdown
	}
	gzipped := e.o.Compression == "gzip"
	if gzipped {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write(body); err != nil {
			return err
		}
		if err := zw.Close(); err != nil {
			return err
		}
		body = buf.Bytes()
	}
	started := e.now()
	for attempt := 1; ; attempt++ {
		status, retryAfter, msg, respBody, err := e.post(ctx, target, headers, body, gzipped)
		if err == nil && status >= 200 && status < 300 {
			e.recordPartial(signal, respBody)
			return nil
		}
		if ctx.Err() != nil {
			e.failures.Add(1)
			return &ExportError{Signal: signal, URL: target, Attempts: attempt, Err: ctx.Err()}
		}
		failure := &ExportError{Signal: signal, URL: target, StatusCode: status, Message: msg, Attempts: attempt, Err: err}
		if e.o.Retry.Disabled || !retryableExport(status, err) {
			e.failures.Add(1)
			return failure
		}
		wait := e.backoff(attempt-1, retryAfter)
		if e.now().Add(wait).Sub(started) > e.o.Retry.MaxElapsedTime {
			e.failures.Add(1)
			return failure
		}
		if dl, ok := ctx.Deadline(); ok && e.now().Add(wait).After(dl) {
			e.failures.Add(1)
			return failure
		}
		e.retries.Add(1)
		if err := e.sleep(ctx, wait); err != nil {
			e.failures.Add(1)
			failure.Err = err
			return failure
		}
	}
}

// retryableExport follows the OTLP/HTTP specification (429, 502, 503, 504)
// and also retries other transient 5xx statuses and transport failures.
func retryableExport(status int, err error) bool {
	if err != nil {
		return true
	}
	switch status {
	case http.StatusTooManyRequests, http.StatusRequestTimeout:
		return true
	case http.StatusNotImplemented, http.StatusHTTPVersionNotSupported:
		return false
	}
	return status >= 500
}

func (e *OTLPExporter) backoff(n int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return retryAfter
	}
	d := float64(e.o.Retry.InitialInterval) * math.Pow(2, float64(n))
	if d > float64(e.o.Retry.MaxInterval) {
		d = float64(e.o.Retry.MaxInterval)
	}
	// Full jitter in [d/2, d) spreads clients out without starving them.
	return time.Duration(d/2 + d/2*e.jitter())
}

func (e *OTLPExporter) post(ctx context.Context, target string, headers map[string]string, body []byte, gzipped bool) (int, time.Duration, string, []byte, error) {
	rctx, cancel := context.WithTimeout(ctx, e.o.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, 0, "", nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "gta-otlp-exporter/"+version.Version)
	if gzipped {
		req.Header.Set("Content-Encoding", "gzip")
	}
	e.requests.Add(1)
	e.bytes.Add(uint64(len(body)))
	resp, err := e.client.Do(req)
	if err != nil {
		return 0, 0, "", nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var msg string
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg = statusMessage(respBody, resp.StatusCode)
	}
	return resp.StatusCode, parseRetryAfter(resp.Header.Get("Retry-After"), e.now()), msg, respBody, nil
}

// statusMessage extracts google.rpc.Status.message from an error body, or a
// short plain-text excerpt.
func statusMessage(body []byte, code int) string {
	var st struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &st) == nil && st.Message != "" {
		return st.Message
	}
	text := strings.Join(strings.Fields(string(body)), " ")
	if text == "" || strings.HasPrefix(text, "<") {
		return http.StatusText(code)
	}
	if len(text) > 200 {
		text = text[:200] + "…"
	}
	return text
}

func (e *OTLPExporter) recordPartial(signal string, body []byte) {
	if len(bytes.TrimSpace(body)) == 0 {
		return
	}
	var ps partialSuccess
	if json.Unmarshal(body, &ps) != nil || ps.PartialSuccess == nil {
		return
	}
	p := ps.PartialSuccess
	spans, _ := p.RejectedSpans.Int64()
	points, _ := p.RejectedDataPoints.Int64()
	if spans > 0 {
		e.rejSpans.Add(uint64(spans))
	}
	if points > 0 {
		e.rejPoints.Add(uint64(points))
	}
	if spans > 0 || points > 0 || p.ErrorMessage != "" {
		reportError(fmt.Errorf("otlp %s export partially rejected (%d spans, %d data points): %s", signal, spans, points, p.ErrorMessage))
	}
}

// parseRetryAfter reads Retry-After as seconds or an HTTP date.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
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

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
