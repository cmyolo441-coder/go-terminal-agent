package telemetry

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// NoopExporter discards everything. It implements SpanExporter and
// MetricExporter.
type NoopExporter struct{}

// ExportSpans implements SpanExporter.
func (NoopExporter) ExportSpans(context.Context, []SpanData) error { return nil }

// ExportMetrics implements MetricExporter.
func (NoopExporter) ExportMetrics(context.Context, *ResourceMetrics) error { return nil }

// Shutdown implements SpanExporter and MetricExporter.
func (NoopExporter) Shutdown(context.Context) error { return nil }

// FileOptions configure a FileExporter.
type FileOptions struct {
	// MaxBytes rotates the file once it would grow past this size; zero
	// never rotates.
	MaxBytes int64
	// MaxBackups is how many rotated files (path.1, path.2, ...) are kept
	// (default 1).
	MaxBackups int
}

// FileExporter appends one OTLP/JSON export request per line, the format
// the OpenTelemetry Collector's otlpjsonfile receiver reads. It implements
// SpanExporter and MetricExporter, and both may share one file.
type FileExporter struct {
	path string
	o    FileOptions

	mu     sync.Mutex
	f      *os.File
	w      *bufio.Writer
	size   int64
	closed bool
}

// NewFileExporter opens (or creates) path for appending. The file is
// private to the user (0600) because spans carry model, tool and file metadata.
func NewFileExporter(path string, o FileOptions) (*FileExporter, error) {
	if path == "" {
		return nil, fmt.Errorf("telemetry: file exporter needs a path")
	}
	if o.MaxBackups <= 0 {
		o.MaxBackups = 1
	}
	e := &FileExporter{path: path, o: o}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("telemetry: create directory for %s: %w", path, err)
	}
	if err := e.open(); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *FileExporter) open() error {
	f, err := os.OpenFile(e.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("telemetry: open %s: %w", e.path, err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("telemetry: stat %s: %w", e.path, err)
	}
	e.f, e.w, e.size = f, bufio.NewWriterSize(f, 64<<10), st.Size()
	return nil
}

// Path is the file being written.
func (e *FileExporter) Path() string { return e.path }

// ExportSpans implements SpanExporter.
func (e *FileExporter) ExportSpans(_ context.Context, spans []SpanData) error {
	if len(spans) == 0 {
		return nil
	}
	line, err := encodeTraces(spans)
	if err != nil {
		return fmt.Errorf("telemetry: encode spans: %w", err)
	}
	return e.writeLine(line)
}

// ExportMetrics implements MetricExporter.
func (e *FileExporter) ExportMetrics(_ context.Context, rm *ResourceMetrics) error {
	if rm == nil || len(rm.ScopeMetrics) == 0 {
		return nil
	}
	line, err := encodeMetrics(rm)
	if err != nil {
		return fmt.Errorf("telemetry: encode metrics: %w", err)
	}
	return e.writeLine(line)
}

func (e *FileExporter) writeLine(line []byte) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrExporterShutdown
	}
	n := int64(len(line) + 1)
	if e.o.MaxBytes > 0 && e.size > 0 && e.size+n > e.o.MaxBytes {
		if err := e.rotate(); err != nil {
			return err
		}
	}
	if _, err := e.w.Write(line); err != nil {
		return err
	}
	if err := e.w.WriteByte('\n'); err != nil {
		return err
	}
	e.size += n
	// Flush per export so a crash loses at most the batch in flight.
	return e.w.Flush()
}

// rotate shifts path.N-1 to path.N ... path to path.1 and reopens path.
func (e *FileExporter) rotate() error {
	if err := e.w.Flush(); err != nil {
		return err
	}
	if err := e.f.Close(); err != nil {
		return err
	}
	for i := e.o.MaxBackups; i > 1; i-- {
		from := fmt.Sprintf("%s.%d", e.path, i-1)
		if _, err := os.Stat(from); err == nil {
			if err := os.Rename(from, fmt.Sprintf("%s.%d", e.path, i)); err != nil {
				return fmt.Errorf("telemetry: rotate %s: %w", from, err)
			}
		}
	}
	if err := os.Rename(e.path, e.path+".1"); err != nil {
		return fmt.Errorf("telemetry: rotate %s: %w", e.path, err)
	}
	return e.open()
}

// Shutdown flushes, syncs and closes the file. Later calls do nothing.
func (e *FileExporter) Shutdown(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	err := e.w.Flush()
	if serr := e.f.Sync(); err == nil {
		err = serr
	}
	if cerr := e.f.Close(); err == nil {
		err = cerr
	}
	return err
}

// InMemoryExporter keeps everything it receives, for tests and for showing
// recent telemetry inside the application. It implements SpanExporter and
// MetricExporter.
type InMemoryExporter struct {
	mu      sync.Mutex
	spans   []SpanData
	metrics []*ResourceMetrics
}

// ExportSpans implements SpanExporter.
func (e *InMemoryExporter) ExportSpans(_ context.Context, spans []SpanData) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = append(e.spans, spans...)
	return nil
}

// ExportMetrics implements MetricExporter.
func (e *InMemoryExporter) ExportMetrics(_ context.Context, rm *ResourceMetrics) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.metrics = append(e.metrics, rm)
	return nil
}

// Shutdown implements SpanExporter and MetricExporter.
func (e *InMemoryExporter) Shutdown(context.Context) error { return nil }

// Spans returns a copy of the exported spans.
func (e *InMemoryExporter) Spans() []SpanData {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]SpanData(nil), e.spans...)
}

// Metrics returns the exported collections.
func (e *InMemoryExporter) Metrics() []*ResourceMetrics {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]*ResourceMetrics(nil), e.metrics...)
}

// Reset forgets everything received.
func (e *InMemoryExporter) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans, e.metrics = nil, nil
}
