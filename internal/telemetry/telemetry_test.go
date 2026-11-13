package telemetry

import (
	"context"
	"math"
	"strings"
	"testing"
)

func TestNewDisabledByDefault(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	tel, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if tel.Enabled() || tel.Description() != "off" {
		t.Fatal("telemetry on without being asked")
	}
	t.Setenv("OTEL_SDK_DISABLED", "true")
	tel, err = New(Config{Enabled: true, Endpoint: "http://localhost:4318"})
	if err != nil || tel.Enabled() {
		t.Fatal("OTEL_SDK_DISABLED ignored")
	}
	t.Setenv("OTEL_SDK_DISABLED", "")
	tel, err = New(Config{Enabled: true, Exporter: "none"})
	if err != nil || tel.Enabled() {
		t.Fatal("exporter none still enabled")
	}
}

func TestConfigValidate(t *testing.T) {
	ratio := 1.5
	for _, c := range []Config{
		{Exporter: "zipkin"},
		{Exporter: "file"},
		{SampleRatio: &ratio},
		{Temporality: "sometimes"},
		{Compression: "br"},
		{MetricIntervalSec: -1},
	} {
		if err := c.Validate(); err == nil {
			t.Fatalf("accepted %+v", c)
		}
		if _, err := New(c); err == nil {
			t.Fatalf("New accepted %+v", c)
		}
	}
}

func TestNewSignalsAndSampling(t *testing.T) {
	c := newCollector(t)
	zero := 0.0
	tel, err := New(Config{Enabled: true, Endpoint: c.srv.URL, SampleRatio: &zero, DisableMetrics: true, Temporality: "delta"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(tel.Description(), "(traces only)") {
		t.Fatalf("description %q", tel.Description())
	}
	_, s := tel.Tracer.Start(context.Background(), "unsampled")
	if s.SpanContext().IsSampled() {
		t.Fatal("ratio 0 sampled a root span")
	}
	s.End()
	if err := tel.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(c.Requests()); n != 0 {
		t.Fatalf("%d requests for an unsampled trace with metrics off", n)
	}

	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	tel, err = New(Config{Enabled: true, Endpoint: c.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(tel.Description(), "(metrics only)") {
		t.Fatalf("description %q", tel.Description())
	}
	_, s = tel.Tracer.Start(context.Background(), "x")
	if s.IsRecording() || !s.SpanContext().IsValid() {
		t.Fatal("traces off must still propagate ids without recording")
	}
	_ = tel.Shutdown(context.Background())
}

func TestSelfMetrics(t *testing.T) {
	c := newCollector(t)
	tel, err := New(Config{Enabled: true, Endpoint: c.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer tel.Shutdown(context.Background())
	_, s := tel.Tracer.Start(context.Background(), "x")
	s.End()
	if err := tel.TracerProvider.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	rm := tel.MeterProvider.Collect(context.Background())
	if pointValue(t, findMetric(t, rm, "gta.telemetry.spans"), map[string]string{"state": "exported"}) != 1 {
		t.Fatal("exported span not counted")
	}
	findMetric(t, rm, "gta.telemetry.queue.size")
	findMetric(t, rm, "gta.telemetry.export.retries")
	b, e := tel.Stats()
	if b.Exported != 1 || e.Requests != 1 {
		t.Fatalf("stats %+v %+v", b, e)
	}
}

func TestOTLPDoubleEncoding(t *testing.T) {
	for f, want := range map[float64]string{math.NaN(): `"NaN"`, math.Inf(1): `"Infinity"`, math.Inf(-1): `"-Infinity"`, 1.5: `1.5`, 1e21: `1e+21`} {
		b, _ := otlpDouble(f).MarshalJSON()
		if string(b) != want {
			t.Fatalf("%v -> %s, want %s", f, b, want)
		}
	}
}
