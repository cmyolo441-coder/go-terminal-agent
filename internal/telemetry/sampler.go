package telemetry

import (
	"encoding/binary"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// SamplingDecision is a sampler's verdict.
type SamplingDecision int

// Sampling decisions.
const (
	// Drop creates a non-recording span that still propagates context.
	Drop SamplingDecision = iota
	// RecordOnly records the span for local processors but does not set
	// the sampled flag, so it is not exported.
	RecordOnly
	// RecordAndSample records and exports the span.
	RecordAndSample
)

// SamplingParameters are the inputs to a sampling decision.
type SamplingParameters struct {
	ParentContext SpanContext
	TraceID       TraceID
	Name          string
	Kind          SpanKind
	Attributes    []KeyValue
}

// SamplingResult is a sampler's output.
type SamplingResult struct {
	Decision   SamplingDecision
	Attributes []KeyValue
	TraceState string
}

// Sampler decides whether a new span is recorded and exported.
type Sampler interface {
	ShouldSample(p SamplingParameters) SamplingResult
	Description() string
}

type alwaysSampler struct{ on bool }

func (s alwaysSampler) ShouldSample(p SamplingParameters) SamplingResult {
	d := Drop
	if s.on {
		d = RecordAndSample
	}
	return SamplingResult{Decision: d, TraceState: p.ParentContext.TraceState}
}

func (s alwaysSampler) Description() string {
	if s.on {
		return "AlwaysOnSampler"
	}
	return "AlwaysOffSampler"
}

// AlwaysSample samples every span.
func AlwaysSample() Sampler { return alwaysSampler{on: true} }

// NeverSample samples no span.
func NeverSample() Sampler { return alwaysSampler{on: false} }

type ratioSampler struct {
	bound uint64
	desc  string
}

// TraceIDRatioBased samples the given fraction of traces, deciding from the
// trace id alone so every service that sees the trace makes the same call.
func TraceIDRatioBased(fraction float64) Sampler {
	switch {
	case fraction >= 1:
		return AlwaysSample()
	case fraction <= 0 || fraction != fraction:
		return NeverSample()
	}
	return ratioSampler{
		bound: uint64(fraction * (1 << 63)),
		desc:  fmt.Sprintf("TraceIDRatioBased{%g}", fraction),
	}
}

func (s ratioSampler) ShouldSample(p SamplingParameters) SamplingResult {
	x := binary.BigEndian.Uint64(p.TraceID[8:16]) >> 1
	d := Drop
	if x < s.bound {
		d = RecordAndSample
	}
	return SamplingResult{Decision: d, TraceState: p.ParentContext.TraceState}
}

func (s ratioSampler) Description() string { return s.desc }

type parentBased struct{ root Sampler }

// ParentBased follows the parent's sampled flag and uses root for spans that
// start a trace.
func ParentBased(root Sampler) Sampler { return parentBased{root: root} }

func (s parentBased) ShouldSample(p SamplingParameters) SamplingResult {
	if p.ParentContext.IsValid() {
		if p.ParentContext.IsSampled() {
			return AlwaysSample().ShouldSample(p)
		}
		return NeverSample().ShouldSample(p)
	}
	return s.root.ShouldSample(p)
}

func (s parentBased) Description() string {
	return "ParentBased{root:" + s.root.Description() + "}"
}

// SamplerFromEnv builds the sampler named by OTEL_TRACES_SAMPLER and
// OTEL_TRACES_SAMPLER_ARG. It reports false when the variable is unset;
// an unknown name is an error.
func SamplerFromEnv() (Sampler, bool, error) {
	name := strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_TRACES_SAMPLER")))
	if name == "" {
		return nil, false, nil
	}
	ratio := 1.0
	if arg := strings.TrimSpace(os.Getenv("OTEL_TRACES_SAMPLER_ARG")); arg != "" {
		f, err := strconv.ParseFloat(arg, 64)
		if err != nil || f < 0 || f > 1 {
			return nil, false, fmt.Errorf("telemetry: OTEL_TRACES_SAMPLER_ARG %q is not a ratio in [0, 1]", arg)
		}
		ratio = f
	}
	switch name {
	case "always_on":
		return AlwaysSample(), true, nil
	case "always_off":
		return NeverSample(), true, nil
	case "traceidratio":
		return TraceIDRatioBased(ratio), true, nil
	case "parentbased_always_on":
		return ParentBased(AlwaysSample()), true, nil
	case "parentbased_always_off":
		return ParentBased(NeverSample()), true, nil
	case "parentbased_traceidratio":
		return ParentBased(TraceIDRatioBased(ratio)), true, nil
	}
	return nil, false, fmt.Errorf("telemetry: unsupported OTEL_TRACES_SAMPLER %q", name)
}
