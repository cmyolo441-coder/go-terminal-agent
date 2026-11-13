package telemetry

import (
	"context"
	"errors"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// SpanExporter sends finished spans to a backend.
type SpanExporter interface {
	ExportSpans(ctx context.Context, spans []SpanData) error
	Shutdown(ctx context.Context) error
}

// BatchOptions tune a BatchSpanProcessor. Zero fields take the
// OpenTelemetry defaults.
type BatchOptions struct {
	// MaxQueueSize bounds the spans waiting for export; spans that arrive
	// while it is full are dropped and counted (default 2048).
	MaxQueueSize int
	// MaxExportBatchSize caps one export call (default 512).
	MaxExportBatchSize int
	// ScheduledDelay is the longest a span waits before export (default 5s).
	ScheduledDelay time.Duration
	// ExportTimeout bounds one export call, retries included (default 30s).
	ExportTimeout time.Duration
}

func (o BatchOptions) normalized() BatchOptions {
	if o.MaxQueueSize <= 0 {
		o.MaxQueueSize = 2048
	}
	if o.MaxExportBatchSize <= 0 {
		o.MaxExportBatchSize = 512
	}
	if o.MaxExportBatchSize > o.MaxQueueSize {
		o.MaxExportBatchSize = o.MaxQueueSize
	}
	if o.ScheduledDelay <= 0 {
		o.ScheduledDelay = 5 * time.Second
	}
	if o.ExportTimeout <= 0 {
		o.ExportTimeout = 30 * time.Second
	}
	return o
}

// BatchOptionsFromEnv reads OTEL_BSP_SCHEDULE_DELAY, OTEL_BSP_EXPORT_TIMEOUT
// (milliseconds), OTEL_BSP_MAX_QUEUE_SIZE and OTEL_BSP_MAX_EXPORT_BATCH_SIZE.
func BatchOptionsFromEnv() BatchOptions {
	return BatchOptions{
		MaxQueueSize:       envInt("OTEL_BSP_MAX_QUEUE_SIZE"),
		MaxExportBatchSize: envInt("OTEL_BSP_MAX_EXPORT_BATCH_SIZE"),
		ScheduledDelay:     time.Duration(envInt("OTEL_BSP_SCHEDULE_DELAY")) * time.Millisecond,
		ExportTimeout:      time.Duration(envInt("OTEL_BSP_EXPORT_TIMEOUT")) * time.Millisecond,
	}
}

func envInt(name string) int {
	v, err := strconv.Atoi(os.Getenv(name))
	if err != nil || v < 0 {
		return 0
	}
	return v
}

// BatchStats are a processor's self-observability counters.
type BatchStats struct {
	Queued   int
	Exported uint64
	Dropped  uint64
	Failed   uint64
	Batches  uint64
}

type flushRequest struct {
	ctx   context.Context
	reply chan error
}

// BatchSpanProcessor queues sampled spans and exports them in batches from
// one background goroutine, so ending a span never blocks on the network.
type BatchSpanProcessor struct {
	exp   SpanExporter
	o     BatchOptions
	queue chan SpanData

	flushCh chan flushRequest
	stopCh  chan flushRequest
	done    chan struct{}

	stopped  atomic.Bool
	stopOnce sync.Once
	stopErr  error

	exported, dropped, failed, batches atomic.Uint64
}

// NewBatchSpanProcessor starts a processor exporting to exp.
func NewBatchSpanProcessor(exp SpanExporter, o BatchOptions) *BatchSpanProcessor {
	o = o.normalized()
	b := &BatchSpanProcessor{
		exp:     exp,
		o:       o,
		queue:   make(chan SpanData, o.MaxQueueSize),
		flushCh: make(chan flushRequest),
		stopCh:  make(chan flushRequest),
		done:    make(chan struct{}),
	}
	go b.run()
	return b
}

// OnStart implements SpanProcessor.
func (b *BatchSpanProcessor) OnStart(context.Context, *Span) {}

// OnEnd implements SpanProcessor. It never blocks: when the queue is full the
// span is dropped and counted.
func (b *BatchSpanProcessor) OnEnd(s SpanData) {
	if !s.SpanContext.IsSampled() || b.stopped.Load() {
		return
	}
	select {
	case b.queue <- s:
	default:
		b.dropped.Add(1)
	}
}

// Stats returns the processor's counters.
func (b *BatchSpanProcessor) Stats() BatchStats {
	return BatchStats{
		Queued:   len(b.queue),
		Exported: b.exported.Load(),
		Dropped:  b.dropped.Load(),
		Failed:   b.failed.Load(),
		Batches:  b.batches.Load(),
	}
}

// ForceFlush exports everything queued so far and waits for it.
func (b *BatchSpanProcessor) ForceFlush(ctx context.Context) error {
	if b.stopped.Load() {
		return nil
	}
	return b.request(ctx, b.flushCh)
}

// Shutdown flushes the queue, stops the goroutine and shuts the exporter
// down. Later calls return the first call's result.
func (b *BatchSpanProcessor) Shutdown(ctx context.Context) error {
	b.stopOnce.Do(func() {
		b.stopped.Store(true)
		err := b.request(ctx, b.stopCh)
		if serr := b.exp.Shutdown(ctx); serr != nil {
			err = errors.Join(err, serr)
		}
		b.stopErr = err
	})
	return b.stopErr
}

func (b *BatchSpanProcessor) request(ctx context.Context, ch chan flushRequest) error {
	req := flushRequest{ctx: ctx, reply: make(chan error, 1)}
	select {
	case ch <- req:
	case <-b.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-req.reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *BatchSpanProcessor) run() {
	defer close(b.done)
	timer := time.NewTimer(b.o.ScheduledDelay)
	defer timer.Stop()
	batch := make([]SpanData, 0, b.o.MaxExportBatchSize)
	resetTimer := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(b.o.ScheduledDelay)
	}
	for {
		select {
		case s := <-b.queue:
			batch = append(batch, s)
			if len(batch) >= b.o.MaxExportBatchSize {
				_ = b.export(context.Background(), batch)
				batch = batch[:0]
				resetTimer()
			}
		case <-timer.C:
			if len(batch) > 0 {
				_ = b.export(context.Background(), batch)
				batch = batch[:0]
			}
			timer.Reset(b.o.ScheduledDelay)
		case req := <-b.flushCh:
			req.reply <- b.drain(req.ctx, &batch)
			resetTimer()
		case req := <-b.stopCh:
			req.reply <- b.drain(req.ctx, &batch)
			return
		}
	}
}

// drain exports the pending batch and everything queued, in batches.
func (b *BatchSpanProcessor) drain(ctx context.Context, batch *[]SpanData) error {
	var errs []error
	for {
		for len(*batch) < b.o.MaxExportBatchSize {
			select {
			case s := <-b.queue:
				*batch = append(*batch, s)
				continue
			default:
			}
			break
		}
		if len(*batch) == 0 {
			return errors.Join(errs...)
		}
		if err := b.export(ctx, *batch); err != nil {
			errs = append(errs, err)
		}
		*batch = (*batch)[:0]
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			return errors.Join(errs...)
		}
	}
}

func (b *BatchSpanProcessor) export(parent context.Context, batch []SpanData) error {
	ctx, cancel := context.WithTimeout(parent, b.o.ExportTimeout)
	defer cancel()
	out := append([]SpanData(nil), batch...)
	b.batches.Add(1)
	if err := b.exp.ExportSpans(ctx, out); err != nil {
		b.failed.Add(uint64(len(out)))
		reportError(err)
		return err
	}
	b.exported.Add(uint64(len(out)))
	return nil
}

// SimpleSpanProcessor exports each span synchronously when it ends. It suits
// tests and short-lived tools; use BatchSpanProcessor in production.
type SimpleSpanProcessor struct {
	exp SpanExporter
	mu  sync.Mutex
}

// NewSimpleSpanProcessor builds a synchronous processor.
func NewSimpleSpanProcessor(exp SpanExporter) *SimpleSpanProcessor {
	return &SimpleSpanProcessor{exp: exp}
}

// OnStart implements SpanProcessor.
func (p *SimpleSpanProcessor) OnStart(context.Context, *Span) {}

// OnEnd implements SpanProcessor.
func (p *SimpleSpanProcessor) OnEnd(s SpanData) {
	if !s.SpanContext.IsSampled() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.exp.ExportSpans(context.Background(), []SpanData{s}); err != nil {
		reportError(err)
	}
}

// ForceFlush implements SpanProcessor.
func (p *SimpleSpanProcessor) ForceFlush(context.Context) error { return nil }

// Shutdown implements SpanProcessor.
func (p *SimpleSpanProcessor) Shutdown(ctx context.Context) error { return p.exp.Shutdown(ctx) }

// errorHandler receives export failures. Telemetry must never disturb the
// terminal UI, so the default discards them; LastError keeps the latest.
var (
	errorHandler atomic.Pointer[func(error)]
	lastError    atomic.Pointer[error]
)

// SetErrorHandler installs a function that receives telemetry pipeline
// errors (export failures, rejected data). nil restores the default.
func SetErrorHandler(f func(error)) {
	if f == nil {
		errorHandler.Store(nil)
		return
	}
	errorHandler.Store(&f)
}

// LastError returns the most recent pipeline error, or nil.
func LastError() error {
	if p := lastError.Load(); p != nil {
		return *p
	}
	return nil
}

func reportError(err error) {
	if err == nil {
		return
	}
	lastError.Store(&err)
	if h := errorHandler.Load(); h != nil {
		(*h)(err)
	}
}
