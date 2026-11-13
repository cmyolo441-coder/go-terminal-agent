package telemetry

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// Scope identifies the instrumentation library that produced telemetry.
type Scope struct {
	Name      string
	Version   string
	SchemaURL string
}

// SpanProcessor receives span lifecycle callbacks.
type SpanProcessor interface {
	OnStart(ctx context.Context, s *Span)
	OnEnd(s SpanData)
	ForceFlush(ctx context.Context) error
	Shutdown(ctx context.Context) error
}

// TracerProvider creates tracers and owns the span pipeline.
type TracerProvider struct {
	resource *Resource
	sampler  Sampler
	limits   SpanLimits
	ids      *idGenerator

	mu         sync.Mutex
	processors atomic.Pointer[[]SpanProcessor]
	tracers    map[Scope]*Tracer
	shutdown   atomic.Bool
}

// TracerProviderOption configures NewTracerProvider.
type TracerProviderOption func(*TracerProvider)

// WithResource sets the resource attached to every span.
func WithResource(r *Resource) TracerProviderOption {
	return func(tp *TracerProvider) { tp.resource = r }
}

// WithSampler sets the sampler (default ParentBased(AlwaysSample())).
func WithSampler(s Sampler) TracerProviderOption {
	return func(tp *TracerProvider) { tp.sampler = s }
}

// WithSpanLimits sets the per-span limits.
func WithSpanLimits(l SpanLimits) TracerProviderOption {
	return func(tp *TracerProvider) { tp.limits = l.normalized() }
}

// WithSpanProcessor registers a processor.
func WithSpanProcessor(p SpanProcessor) TracerProviderOption {
	return func(tp *TracerProvider) { tp.RegisterSpanProcessor(p) }
}

// NewTracerProvider builds a provider. With no options it samples every
// trace and exports nowhere until a processor is registered.
func NewTracerProvider(opts ...TracerProviderOption) *TracerProvider {
	tp := &TracerProvider{
		resource: defaultResource(),
		sampler:  ParentBased(AlwaysSample()),
		limits:   SpanLimits{}.normalized(),
		ids:      newIDGenerator(),
		tracers:  map[Scope]*Tracer{},
	}
	empty := []SpanProcessor{}
	tp.processors.Store(&empty)
	for _, o := range opts {
		o(tp)
	}
	return tp
}

// Resource returns the provider's resource.
func (tp *TracerProvider) Resource() *Resource { return tp.resource }

// RegisterSpanProcessor adds a processor.
func (tp *TracerProvider) RegisterSpanProcessor(p SpanProcessor) {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	cur := *tp.processors.Load()
	next := append(append([]SpanProcessor(nil), cur...), p)
	tp.processors.Store(&next)
}

func (tp *TracerProvider) processorList() []SpanProcessor {
	if tp == nil {
		return nil
	}
	return *tp.processors.Load()
}

// TracerOption configures Tracer.
type TracerOption func(*Scope)

// WithInstrumentationVersion sets the scope version.
func WithInstrumentationVersion(v string) TracerOption { return func(s *Scope) { s.Version = v } }

// WithSchemaURL sets the scope schema URL.
func WithSchemaURL(u string) TracerOption { return func(s *Scope) { s.SchemaURL = u } }

// Tracer returns the tracer for an instrumentation scope; the same scope
// always yields the same tracer.
func (tp *TracerProvider) Tracer(name string, opts ...TracerOption) *Tracer {
	sc := Scope{Name: name}
	for _, o := range opts {
		o(&sc)
	}
	tp.mu.Lock()
	defer tp.mu.Unlock()
	if t, ok := tp.tracers[sc]; ok {
		return t
	}
	t := &Tracer{provider: tp, scope: sc}
	tp.tracers[sc] = t
	return t
}

// ForceFlush flushes every processor.
func (tp *TracerProvider) ForceFlush(ctx context.Context) error {
	var errs []error
	for _, p := range tp.processorList() {
		if err := p.ForceFlush(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Shutdown flushes and stops every processor. Spans started afterwards are
// non-recording. Only the first call does work.
func (tp *TracerProvider) Shutdown(ctx context.Context) error {
	if !tp.shutdown.CompareAndSwap(false, true) {
		return nil
	}
	var errs []error
	for _, p := range tp.processorList() {
		if err := p.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Tracer starts spans for one instrumentation scope.
type Tracer struct {
	provider *TracerProvider
	scope    Scope
}

// Scope returns the tracer's instrumentation scope.
func (t *Tracer) Scope() Scope { return t.scope }

type spanConfig struct {
	kind    SpanKind
	attrs   []KeyValue
	links   []Link
	start   time.Time
	newRoot bool
}

// SpanOption configures Start.
type SpanOption func(*spanConfig)

// WithSpanKind sets the span kind (default Internal).
func WithSpanKind(k SpanKind) SpanOption { return func(c *spanConfig) { c.kind = k } }

// WithAttributes sets initial attributes, visible to the sampler.
func WithAttributes(attrs ...KeyValue) SpanOption {
	return func(c *spanConfig) { c.attrs = append(c.attrs, attrs...) }
}

// WithLinks adds links.
func WithLinks(links ...Link) SpanOption {
	return func(c *spanConfig) { c.links = append(c.links, links...) }
}

// WithTimestamp sets an explicit start time.
func WithTimestamp(t time.Time) SpanOption { return func(c *spanConfig) { c.start = t } }

// WithNewRoot ignores any parent in the context and starts a new trace.
func WithNewRoot() SpanOption { return func(c *spanConfig) { c.newRoot = true } }

// Start begins a span as a child of the span in ctx (if any) and returns a
// context carrying it. The span is never nil; when the sampler drops it, it
// is non-recording but still propagates its trace id.
func (t *Tracer) Start(ctx context.Context, name string, opts ...SpanOption) (context.Context, *Span) {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg := spanConfig{kind: SpanKindInternal}
	for _, o := range opts {
		o(&cfg)
	}
	var parent SpanContext
	if !cfg.newRoot {
		parent = SpanContextFromContext(ctx)
	}
	tp := t.provider
	sc := SpanContext{SpanID: tp.ids.spanID()}
	if parent.IsValid() {
		sc.TraceID = parent.TraceID
	} else {
		sc.TraceID = tp.ids.traceID()
		parent = SpanContext{}
	}
	res := tp.sampler.ShouldSample(SamplingParameters{
		ParentContext: parent, TraceID: sc.TraceID, Name: name, Kind: cfg.kind, Attributes: cfg.attrs,
	})
	sc.TraceState = res.TraceState
	if res.Decision == RecordAndSample {
		sc.TraceFlags |= FlagsSampled
	}
	if res.Decision == Drop || tp.shutdown.Load() {
		sc.TraceFlags &^= FlagsSampled
		s := &Span{sc: sc, parent: parent}
		return ContextWithSpan(ctx, s), s
	}
	start := cfg.start
	if start.IsZero() {
		start = time.Now()
	}
	s := &Span{
		tracer: t, limits: tp.limits, sc: sc, parent: parent,
		name: name, kind: cfg.kind, start: start, recording: true,
	}
	for _, kv := range cfg.attrs {
		s.setAttrLocked(kv)
	}
	for _, kv := range res.Attributes {
		s.setAttrLocked(kv)
	}
	for _, l := range cfg.links {
		s.addLinkLocked(l)
	}
	ctx = ContextWithSpan(ctx, s)
	for _, p := range tp.processorList() {
		p.OnStart(ctx, s)
	}
	return ctx, s
}
