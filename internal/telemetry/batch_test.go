package telemetry

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// gateExporter blocks every export until released.
type gateExporter struct {
	InMemoryExporter
	started chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	batches []int
}

func newGate() *gateExporter {
	return &gateExporter{started: make(chan struct{}), release: make(chan struct{})}
}

func (g *gateExporter) ExportSpans(ctx context.Context, spans []SpanData) error {
	g.once.Do(func() { close(g.started) })
	select {
	case <-g.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	g.mu.Lock()
	g.batches = append(g.batches, len(spans))
	g.mu.Unlock()
	return g.InMemoryExporter.ExportSpans(ctx, spans)
}

func sampledSpan(i int) SpanData {
	return SpanData{Name: "s", SpanContext: SpanContext{TraceID: TraceID{1}, SpanID: SpanID{byte(i + 1)}, TraceFlags: FlagsSampled}}
}

func TestBatchDropsWhenQueueFull(t *testing.T) {
	g := newGate()
	b := NewBatchSpanProcessor(g, BatchOptions{MaxQueueSize: 2, MaxExportBatchSize: 1, ScheduledDelay: time.Hour})
	b.OnEnd(sampledSpan(0)) // taken by the loop, which then blocks exporting it
	<-g.started
	for i := 1; i <= 5; i++ {
		b.OnEnd(sampledSpan(i))
	}
	if st := b.Stats(); st.Dropped != 3 || st.Queued != 2 {
		t.Fatalf("stats %+v, want 3 dropped and 2 queued", st)
	}
	// Unsampled spans are ignored, not dropped.
	b.OnEnd(SpanData{SpanContext: SpanContext{TraceID: TraceID{1}, SpanID: SpanID{9}}})
	if b.Stats().Dropped != 3 {
		t.Fatal("unsampled span counted")
	}
	close(g.release)
	if err := b.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := b.Stats(); st.Exported != 3 || st.Queued != 0 {
		t.Fatalf("after shutdown %+v", st)
	}
	b.OnEnd(sampledSpan(7)) // after shutdown: ignored
	if len(g.Spans()) != 3 {
		t.Fatalf("%d spans exported", len(g.Spans()))
	}
}

func TestBatchPeriodicFlush(t *testing.T) {
	exp := &InMemoryExporter{}
	b := NewBatchSpanProcessor(exp, BatchOptions{ScheduledDelay: 10 * time.Millisecond})
	defer b.Shutdown(context.Background())
	b.OnEnd(sampledSpan(0))
	deadline := time.Now().Add(5 * time.Second)
	for len(exp.Spans()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("scheduled flush never happened")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestBatchSizesAndForceFlush(t *testing.T) {
	g := newGate()
	close(g.release)
	b := NewBatchSpanProcessor(g, BatchOptions{MaxExportBatchSize: 3, ScheduledDelay: time.Hour})
	for i := 0; i < 7; i++ {
		b.OnEnd(sampledSpan(i))
	}
	if err := b.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	batches := append([]int(nil), g.batches...)
	g.mu.Unlock()
	total := 0
	for _, n := range batches {
		if n > 3 {
			t.Fatalf("batch of %d exceeds the limit", n)
		}
		total += n
	}
	if total != 7 || b.Stats().Batches != uint64(len(batches)) {
		t.Fatalf("batches %v, stats %+v", batches, b.Stats())
	}
	if err := b.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := b.ForceFlush(context.Background()); err != nil {
		t.Fatal("ForceFlush after Shutdown must be a no-op")
	}
}

func TestBatchShutdownHonoursContext(t *testing.T) {
	g := newGate() // never released
	b := NewBatchSpanProcessor(g, BatchOptions{MaxExportBatchSize: 1, ScheduledDelay: time.Hour, ExportTimeout: time.Hour})
	b.OnEnd(sampledSpan(0))
	<-g.started
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := b.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("Shutdown ignored its context")
	}
	close(g.release)
}

type failingExporter struct{ NoopExporter }

func (failingExporter) ExportSpans(context.Context, []SpanData) error {
	return errors.New("collector down")
}

func TestBatchCountsFailures(t *testing.T) {
	var got error
	SetErrorHandler(func(err error) { got = err })
	defer SetErrorHandler(nil)
	b := NewBatchSpanProcessor(failingExporter{}, BatchOptions{ScheduledDelay: time.Hour})
	b.OnEnd(sampledSpan(0))
	b.OnEnd(sampledSpan(1))
	if err := b.ForceFlush(context.Background()); err == nil {
		t.Fatal("export failure not returned")
	}
	if st := b.Stats(); st.Failed != 2 || got == nil {
		t.Fatalf("stats %+v handler=%v", st, got)
	}
	_ = b.Shutdown(context.Background())
}

func TestBatchOptionsFromEnv(t *testing.T) {
	t.Setenv("OTEL_BSP_SCHEDULE_DELAY", "250")
	t.Setenv("OTEL_BSP_MAX_QUEUE_SIZE", "10")
	t.Setenv("OTEL_BSP_MAX_EXPORT_BATCH_SIZE", "50")
	o := BatchOptionsFromEnv().normalized()
	if o.ScheduledDelay != 250*time.Millisecond || o.MaxQueueSize != 10 || o.MaxExportBatchSize != 10 {
		t.Fatalf("%+v", o)
	}
}
