// Package telemetry gives GTA OpenTelemetry-compatible traces and metrics
// without depending on the OpenTelemetry SDK.
//
// It implements the parts of the specification GTA needs: W3C Trace Context
// ids and propagation (HTTP headers and the TRACEPARENT environment
// variable), spans with attributes, events, links, status and limits,
// head sampling, synchronous and observable instruments with cumulative or
// delta temporality, exemplars and cardinality limits, a batching span
// processor with a bounded queue, and exporters for OTLP/HTTP (JSON
// encoding, gzip, retries honouring Retry-After), JSON-lines files in the
// Collector's otlpjsonfile format, memory and no-op.
//
// The standard OTEL_* environment variables are honoured, so an existing
// collector setup works unchanged. On top of that, WrapProvider turns every
// model call into a span carrying GenAI semantic-convention attributes and
// StartTool does the same for tool calls.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config selects and tunes telemetry. It is meant to be embedded in GTA's
// configuration file as "telemetry": {...}. Explicit fields win over the
// OTEL_* environment variables, which fill in whatever is left empty.
type Config struct {
	// Enabled turns telemetry on. It is also on when
	// OTEL_EXPORTER_OTLP_ENDPOINT (or the traces/metrics variant) is set,
	// and always off when OTEL_SDK_DISABLED=true.
	Enabled bool `json:"enabled"`
	// Exporter is "otlp" (default), "file" or "none".
	Exporter string `json:"exporter,omitempty"`
	// Endpoint is the OTLP/HTTP base URL, e.g. http://localhost:4318.
	Endpoint string `json:"endpoint,omitempty"`
	// Headers are sent with every OTLP request, e.g. an API key.
	Headers map[string]string `json:"headers,omitempty"`
	// Compression is "gzip" or "none".
	Compression string `json:"compression,omitempty"`
	// TimeoutSec bounds each OTLP request.
	TimeoutSec int `json:"timeout_sec,omitempty"`
	// File is the JSON-lines output for the file exporter.
	File string `json:"file,omitempty"`
	// FileMaxMB rotates the file at this size (0 never rotates).
	FileMaxMB int `json:"file_max_mb,omitempty"`
	// ServiceName overrides service.name (default "gta").
	ServiceName string `json:"service_name,omitempty"`
	// ResourceAttributes are added to the resource.
	ResourceAttributes map[string]string `json:"resource_attributes,omitempty"`
	// SampleRatio samples this fraction of traces (default 1). Children
	// follow their parent's decision.
	SampleRatio *float64 `json:"sample_ratio,omitempty"`
	// MetricIntervalSec is the metric export period (default 60).
	MetricIntervalSec int `json:"metric_interval_sec,omitempty"`
	// Temporality is "cumulative" (default) or "delta".
	Temporality string `json:"temporality,omitempty"`
	// DisableTraces and DisableMetrics turn one signal off.
	DisableTraces  bool `json:"disable_traces,omitempty"`
	DisableMetrics bool `json:"disable_metrics,omitempty"`
}

// Validate checks values that New would reject.
func (c Config) Validate() error {
	switch strings.ToLower(c.Exporter) {
	case "", "otlp", "file", "none":
	default:
		return fmt.Errorf("telemetry: exporter %q is not one of otlp, file, none", c.Exporter)
	}
	if strings.EqualFold(c.Exporter, "file") && strings.TrimSpace(c.File) == "" {
		return errors.New("telemetry: the file exporter needs \"file\"")
	}
	if c.SampleRatio != nil && (*c.SampleRatio < 0 || *c.SampleRatio > 1) {
		return fmt.Errorf("telemetry: sample_ratio %v is not in [0, 1]", *c.SampleRatio)
	}
	switch strings.ToLower(c.Temporality) {
	case "", "cumulative", "delta":
	default:
		return fmt.Errorf("telemetry: temporality %q is not cumulative or delta", c.Temporality)
	}
	switch strings.ToLower(c.Compression) {
	case "", "gzip", "none":
	default:
		return fmt.Errorf("telemetry: compression %q is not gzip or none", c.Compression)
	}
	if c.MetricIntervalSec < 0 || c.TimeoutSec < 0 || c.FileMaxMB < 0 {
		return errors.New("telemetry: intervals, timeouts and sizes must not be negative")
	}
	return nil
}

// Telemetry is GTA's telemetry pipeline. A nil or disabled *Telemetry is
// valid: its helpers do nothing and WrapProvider returns the provider
// unchanged.
type Telemetry struct {
	TracerProvider *TracerProvider
	MeterProvider  *MeterProvider
	Tracer         *Tracer
	Instruments    *Instruments

	enabled     bool
	description string
	spans       *BatchSpanProcessor
	otlp        *OTLPExporter
	closer      func(context.Context) error
	warnings    []error
}

// The two pipelines share one exporter, which must outlive both of their
// shutdowns; Telemetry.Shutdown closes it last.
type keepOpenSpans struct{ SpanExporter }

func (keepOpenSpans) Shutdown(context.Context) error { return nil }

type keepOpenMetrics struct{ MetricExporter }

func (keepOpenMetrics) Shutdown(context.Context) error { return nil }

// Disabled returns a Telemetry that records nothing.
func Disabled() *Telemetry {
	tp := NewTracerProvider(WithSampler(NeverSample()))
	return &Telemetry{
		TracerProvider: tp,
		Tracer:         tp.Tracer(instrumentationName),
		Instruments:    &Instruments{},
		description:    "off",
	}
}

func envTrue(name string) bool {
	b, _ := strconv.ParseBool(strings.TrimSpace(os.Getenv(name)))
	return b
}

// EnvEndpointSet reports whether an OTLP endpoint is set in the
// environment (OTEL_EXPORTER_OTLP_ENDPOINT or its traces or metrics
// variant), which turns telemetry on without "enabled" in the config.
func EnvEndpointSet() bool {
	for _, k := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"} {
		if os.Getenv(k) != "" {
			return true
		}
	}
	return false
}

// New builds the pipeline described by cfg and the OTEL_* environment.
// Recoverable problems with environment variables (malformed lists, an
// unsupported protocol) do not fail New; Warnings reports them.
func New(cfg Config) (*Telemetry, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	envEndpoint := EnvEndpointSet()
	if envTrue("OTEL_SDK_DISABLED") || !(cfg.Enabled || envEndpoint) {
		return Disabled(), nil
	}
	exporter := strings.ToLower(cfg.Exporter)
	if exporter == "" {
		exporter = "otlp"
		if cfg.File != "" && cfg.Endpoint == "" && !envEndpoint {
			exporter = "file"
		}
	}
	if exporter == "none" {
		return Disabled(), nil
	}
	tracesOn := !cfg.DisableTraces && !strings.EqualFold(os.Getenv("OTEL_TRACES_EXPORTER"), "none")
	metricsOn := !cfg.DisableMetrics && !strings.EqualFold(os.Getenv("OTEL_METRICS_EXPORTER"), "none")

	t := &Telemetry{enabled: true}

	res := defaultResource()
	envRes, err := resourceFromEnv()
	if err != nil {
		t.warnings = append(t.warnings, err)
	}
	res = res.Merge(envRes)
	var extra []KeyValue
	for k, v := range cfg.ResourceAttributes {
		extra = append(extra, String(k, v))
	}
	if cfg.ServiceName != "" {
		extra = append(extra, String("service.name", cfg.ServiceName))
	}
	res = res.Merge(NewResource(extra...))

	var spanExp SpanExporter = NoopExporter{}
	var metricExp MetricExporter = NoopExporter{}
	switch exporter {
	case "file":
		fe, err := NewFileExporter(cfg.File, FileOptions{MaxBytes: int64(cfg.FileMaxMB) << 20, MaxBackups: 3})
		if err != nil {
			return nil, err
		}
		spanExp, metricExp, t.closer = keepOpenSpans{fe}, keepOpenMetrics{fe}, fe.Shutdown
		t.description = "file " + fe.Path()
	default:
		o, err := OTLPOptionsFromEnv()
		if err != nil {
			t.warnings = append(t.warnings, err)
		}
		if cfg.Endpoint != "" {
			o.Endpoint, o.TracesURL, o.MetricsURL = cfg.Endpoint, "", ""
		}
		if len(cfg.Headers) > 0 {
			o.Headers = merge(o.Headers, cfg.Headers)
		}
		if cfg.Compression != "" {
			o.Compression = strings.ToLower(cfg.Compression)
		}
		if cfg.TimeoutSec > 0 {
			o.Timeout = time.Duration(cfg.TimeoutSec) * time.Second
		}
		oe, err := NewOTLPExporter(o)
		if err != nil {
			return nil, err
		}
		t.otlp = oe
		spanExp, metricExp, t.closer = keepOpenSpans{oe}, keepOpenMetrics{oe}, oe.Shutdown
		t.description = "otlp " + strings.TrimSuffix(oe.TracesURL(), "/v1/traces")
	}

	sampler := ParentBased(AlwaysSample())
	if cfg.SampleRatio != nil {
		sampler = ParentBased(TraceIDRatioBased(*cfg.SampleRatio))
	} else if s, ok, err := SamplerFromEnv(); err != nil {
		t.warnings = append(t.warnings, err)
	} else if ok {
		sampler = s
	}
	if !tracesOn {
		// Spans still carry ids for propagation but record nothing.
		sampler = NeverSample()
	}
	tpOpts := []TracerProviderOption{WithResource(res), WithSampler(sampler), WithSpanLimits(SpanLimitsFromEnv())}
	if tracesOn {
		t.spans = NewBatchSpanProcessor(spanExp, BatchOptionsFromEnv())
		tpOpts = append(tpOpts, WithSpanProcessor(t.spans))
	}
	t.TracerProvider = NewTracerProvider(tpOpts...)
	t.Tracer = t.TracerProvider.Tracer(instrumentationName, WithSchemaURL(genAISchemaURL))

	temporality := TemporalityFromEnv()
	if cfg.Temporality != "" {
		temporality = CumulativeTemporality
		if strings.EqualFold(cfg.Temporality, "delta") {
			temporality = DeltaTemporality
		}
	}
	interval := time.Duration(envInt("OTEL_METRIC_EXPORT_INTERVAL")) * time.Millisecond
	if cfg.MetricIntervalSec > 0 {
		interval = time.Duration(cfg.MetricIntervalSec) * time.Second
	}
	if interval <= 0 {
		interval = defaultMetricIntervalSeconds * time.Second
	}
	mpOpts := []MeterProviderOption{
		WithMetricResource(res), WithTemporality(temporality), WithExemplarFilter(ExemplarFilterFromEnv()),
	}
	if metricsOn {
		mpOpts = append(mpOpts, WithPeriodicReader(metricExp, ReaderOptions{
			Interval: interval,
			Timeout:  time.Duration(envInt("OTEL_METRIC_EXPORT_TIMEOUT")) * time.Millisecond,
		}))
	}
	t.MeterProvider = NewMeterProvider(mpOpts...)
	meter := t.MeterProvider.Meter(instrumentationName, WithSchemaURL(genAISchemaURL))
	in, err := NewInstruments(meter)
	if err != nil {
		return nil, err
	}
	t.Instruments = in
	if err := t.registerSelfMetrics(meter); err != nil {
		return nil, err
	}
	if !tracesOn {
		t.description += " (metrics only)"
	} else if !metricsOn {
		t.description += " (traces only)"
	}
	return t, nil
}

// registerSelfMetrics exposes the pipeline's own health, so a dashboard can
// alert on dropped spans or failing exports.
func (t *Telemetry) registerSelfMetrics(m *Meter) error {
	if t.spans != nil {
		err := m.Int64ObservableCounter("gta.telemetry.spans", func(_ context.Context, observe Observe) {
			st := t.spans.Stats()
			observe(float64(st.Exported), String("state", "exported"))
			observe(float64(st.Dropped), String("state", "dropped"))
			observe(float64(st.Failed), String("state", "failed"))
		}, WithUnit("{span}"), WithDescription("Spans handled by the batch processor, by state."))
		if err != nil {
			return err
		}
		err = m.Int64ObservableGauge("gta.telemetry.queue.size", func(_ context.Context, observe Observe) {
			observe(float64(t.spans.Stats().Queued))
		}, WithUnit("{span}"), WithDescription("Spans waiting in the export queue."))
		if err != nil {
			return err
		}
	}
	if t.otlp != nil {
		return m.Int64ObservableCounter("gta.telemetry.export.retries", func(_ context.Context, observe Observe) {
			observe(float64(t.otlp.Stats().Retries))
		}, WithUnit("{retry}"), WithDescription("OTLP export retries."))
	}
	return nil
}

func (t *Telemetry) instruments() *Instruments {
	if t == nil || t.Instruments == nil {
		return &Instruments{}
	}
	return t.Instruments
}

// Enabled reports whether anything is recorded.
func (t *Telemetry) Enabled() bool { return t != nil && t.enabled }

// Description summarises the pipeline for a status line, e.g.
// "otlp http://localhost:4318" or "off".
func (t *Telemetry) Description() string {
	if t == nil {
		return "off"
	}
	return t.description
}

// Warnings lists recoverable configuration problems found by New.
func (t *Telemetry) Warnings() []error {
	if t == nil {
		return nil
	}
	return append([]error(nil), t.warnings...)
}

// Stats returns span-pipeline and exporter counters.
func (t *Telemetry) Stats() (BatchStats, ExporterStats) {
	var b BatchStats
	var e ExporterStats
	if t == nil {
		return b, e
	}
	if t.spans != nil {
		b = t.spans.Stats()
	}
	if t.otlp != nil {
		e = t.otlp.Stats()
	}
	return b, e
}

// ForceFlush exports everything recorded so far.
func (t *Telemetry) ForceFlush(ctx context.Context) error {
	if t == nil || !t.enabled {
		return nil
	}
	return errors.Join(t.TracerProvider.ForceFlush(ctx), t.MeterProvider.ForceFlush(ctx))
}

// Shutdown flushes and stops both pipelines. Call it once before exit with
// a bounded context so a dead collector cannot hang the process.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	if t == nil || !t.enabled {
		return nil
	}
	err := errors.Join(t.TracerProvider.Shutdown(ctx), t.MeterProvider.Shutdown(ctx))
	if t.closer != nil {
		err = errors.Join(err, t.closer(ctx))
	}
	return err
}
