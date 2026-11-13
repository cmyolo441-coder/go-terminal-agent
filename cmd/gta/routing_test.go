package main

import (
	"context"
	"testing"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/config"
)

// Telemetry turns on for any of the OTLP endpoint variables that
// internal/telemetry honours, not only OTEL_EXPORTER_OTLP_ENDPOINT.
func TestStartTelemetryHonoursSignalEndpoints(t *testing.T) {
	for _, k := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"} {
		t.Run(k, func(t *testing.T) {
			for _, other := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "OTEL_SDK_DISABLED"} {
				t.Setenv(other, "")
			}
			cfg := config.Default()
			if tel, err := startTelemetry(cfg); err != nil || tel != nil {
				t.Fatalf("telemetry on without an endpoint: %v %v", tel, err)
			}
			t.Setenv(k, "http://127.0.0.1:1/v1/x")
			tel, err := startTelemetry(cfg)
			if err != nil || tel == nil || !tel.Enabled() {
				t.Fatalf("%s did not turn telemetry on: %v %v", k, tel, err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = tel.Shutdown(ctx)
		})
	}
}
