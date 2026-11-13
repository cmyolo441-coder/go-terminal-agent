package telemetry

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func readLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("line is not JSON: %v", err)
		}
		out = append(out, m)
	}
	return out
}

func TestFileExporterJSONLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "telemetry.jsonl")
	fe, err := NewFileExporter(path, FileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := fe.ExportSpans(context.Background(), []SpanData{testSpanData(), testSpanData()}); err != nil {
		t.Fatal(err)
	}
	mp := NewMeterProvider()
	c, _ := mp.Meter("m").Int64Counter("c")
	c.Add(context.Background(), 3)
	if err := fe.ExportMetrics(context.Background(), mp.Collect(context.Background())); err != nil {
		t.Fatal(err)
	}
	if err := fe.ExportSpans(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if err := fe.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	lines := readLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("%d lines", len(lines))
	}
	spans := arr(t, arr(t, lines[0], "resourceSpans")[0], "scopeSpans")[0]
	if len(arr(t, spans, "spans")) != 2 {
		t.Fatal("spans line")
	}
	if _, ok := lines[1]["resourceMetrics"]; !ok {
		t.Fatal("metrics line")
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v err %v", st.Mode(), err)
	}
	if err := fe.ExportSpans(context.Background(), []SpanData{testSpanData()}); !errors.Is(err, ErrExporterShutdown) {
		t.Fatalf("export after shutdown: %v", err)
	}
	if err := fe.Shutdown(context.Background()); err != nil {
		t.Fatal("second shutdown")
	}
}

func TestFileExporterRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	fe, err := NewFileExporter(path, FileOptions{MaxBytes: 600, MaxBackups: 2})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if err := fe.ExportSpans(context.Background(), []SpanData{testSpanData()}); err != nil {
			t.Fatal(err)
		}
	}
	_ = fe.Shutdown(context.Background())
	for _, p := range []string{path, path + ".1", path + ".2"} {
		if len(readLines(t, p)) == 0 {
			t.Fatalf("%s empty", p)
		}
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatal("more backups than MaxBackups")
	}
	total := 0
	for _, p := range []string{path, path + ".1", path + ".2"} {
		total += len(readLines(t, p))
	}
	if total > 6 || total < 3 {
		t.Fatalf("%d lines kept", total)
	}
}

func TestFileExporterEndToEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gta.jsonl")
	tel, err := New(Config{Enabled: true, File: path, ServiceName: "gta-file", ResourceAttributes: map[string]string{"team": "core"}})
	if err != nil {
		t.Fatal(err)
	}
	if tel.Description() != "file "+path {
		t.Fatalf("description %q", tel.Description())
	}
	_, s := tel.Tracer.Start(context.Background(), "op")
	s.End()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tel.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	lines := readLines(t, path)
	var sawSpans, sawMetrics bool
	for _, l := range lines {
		if rs, ok := l["resourceSpans"]; ok {
			sawSpans = true
			ra := attrMap(t, obj(t, rs.([]any)[0], "resource"))
			if strAttr(t, ra, "service.name") != "gta-file" || strAttr(t, ra, "team") != "core" {
				t.Fatalf("resource %v", ra)
			}
		}
		if _, ok := l["resourceMetrics"]; ok {
			sawMetrics = true
		}
	}
	if !sawSpans || !sawMetrics {
		t.Fatalf("spans=%v metrics=%v in %d lines", sawSpans, sawMetrics, len(lines))
	}
}
