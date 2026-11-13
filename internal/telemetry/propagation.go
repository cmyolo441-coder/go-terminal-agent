package telemetry

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// W3C Trace Context header names. The environment-variable carrier uses the
// upper-case forms (TRACEPARENT, TRACESTATE), as the OpenTelemetry
// specification for environment propagation defines.
const (
	TraceparentHeader = "traceparent"
	TracestateHeader  = "tracestate"
)

// FlagsSampled is the W3C "sampled" trace flag.
const FlagsSampled byte = 0x01

// SpanContext identifies a span across process boundaries.
type SpanContext struct {
	TraceID    TraceID
	SpanID     SpanID
	TraceFlags byte
	TraceState string
	// Remote is true when the context was extracted from a carrier.
	Remote bool
}

// IsValid reports whether both ids are set.
func (sc SpanContext) IsValid() bool { return sc.TraceID.IsValid() && sc.SpanID.IsValid() }

// IsSampled reports whether the sampled flag is set.
func (sc SpanContext) IsSampled() bool { return sc.TraceFlags&FlagsSampled != 0 }

// Traceparent renders the W3C traceparent value (version 00), or "" for an
// invalid context.
func (sc SpanContext) Traceparent() string {
	if !sc.IsValid() {
		return ""
	}
	return fmt.Sprintf("00-%s-%s-%02x", sc.TraceID, sc.SpanID, sc.TraceFlags)
}

// ParseTraceparent decodes a W3C traceparent value. Unknown future versions
// are accepted as long as their first four fields are well formed, as the
// specification requires; version ff is invalid.
func ParseTraceparent(v string) (SpanContext, error) {
	v = strings.TrimSpace(v)
	parts := strings.Split(v, "-")
	if len(parts) < 4 || len(parts[0]) != 2 || len(parts[3]) != 2 {
		return SpanContext{}, fmt.Errorf("telemetry: malformed traceparent %q", v)
	}
	var ver [1]byte
	if decodeLowerHex(ver[:], parts[0]) != nil || ver[0] == 0xff {
		return SpanContext{}, fmt.Errorf("telemetry: invalid traceparent version %q", parts[0])
	}
	if ver[0] == 0 && len(parts) != 4 {
		return SpanContext{}, fmt.Errorf("telemetry: malformed traceparent %q", v)
	}
	tid, err := ParseTraceID(parts[1])
	if err != nil {
		return SpanContext{}, fmt.Errorf("telemetry: invalid trace id in traceparent %q", v)
	}
	sid, err := ParseSpanID(parts[2])
	if err != nil {
		return SpanContext{}, fmt.Errorf("telemetry: invalid parent id in traceparent %q", v)
	}
	var flags [1]byte
	if decodeLowerHex(flags[:], parts[3]) != nil {
		return SpanContext{}, fmt.Errorf("telemetry: invalid trace flags in traceparent %q", v)
	}
	return SpanContext{TraceID: tid, SpanID: sid, TraceFlags: flags[0], Remote: true}, nil
}

// validTracestate checks the tracestate list syntax loosely: at most 32
// comma-separated key=value members with no control characters. A value
// that fails is dropped rather than propagated.
func validTracestate(s string) bool {
	if len(s) > 512 {
		return false
	}
	members := 0
	for _, m := range strings.Split(s, ",") {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		members++
		k, val, ok := strings.Cut(m, "=")
		if !ok || k == "" || val == "" || members > 32 {
			return false
		}
		for _, c := range m {
			if c < 0x20 || c > 0x7e {
				return false
			}
		}
	}
	return true
}

// Inject writes the active span's context to h as traceparent and
// tracestate headers. It does nothing when ctx carries no valid span.
func Inject(ctx context.Context, h http.Header) {
	sc := SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return
	}
	h.Set(TraceparentHeader, sc.Traceparent())
	if sc.TraceState != "" {
		h.Set(TracestateHeader, sc.TraceState)
	}
}

// Extract returns ctx carrying the remote span context found in h, so spans
// started from it continue the caller's trace. An absent or malformed
// header leaves ctx unchanged.
func Extract(ctx context.Context, h http.Header) context.Context {
	return extract(ctx, h.Get(TraceparentHeader), h.Get(TracestateHeader))
}

// InjectEnv appends TRACEPARENT and TRACESTATE to env (as "KEY=value"
// entries) for a child process, replacing earlier entries of the same name.
func InjectEnv(ctx context.Context, env []string) []string {
	sc := SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return env
	}
	out := make([]string, 0, len(env)+2)
	for _, e := range env {
		if strings.HasPrefix(e, "TRACEPARENT=") || strings.HasPrefix(e, "TRACESTATE=") {
			continue
		}
		out = append(out, e)
	}
	out = append(out, "TRACEPARENT="+sc.Traceparent())
	if sc.TraceState != "" {
		out = append(out, "TRACESTATE="+sc.TraceState)
	}
	return out
}

// ExtractEnv continues a trace started by a parent process that exported
// TRACEPARENT (and optionally TRACESTATE), for example a CI pipeline.
func ExtractEnv(ctx context.Context) context.Context {
	return extract(ctx, os.Getenv("TRACEPARENT"), os.Getenv("TRACESTATE"))
}

func extract(ctx context.Context, traceparent, tracestate string) context.Context {
	if traceparent == "" {
		return ctx
	}
	sc, err := ParseTraceparent(traceparent)
	if err != nil {
		return ctx
	}
	if tracestate != "" && validTracestate(tracestate) {
		sc.TraceState = strings.TrimSpace(tracestate)
	}
	return ContextWithRemoteSpanContext(ctx, sc)
}
