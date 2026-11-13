package telemetry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"
)

// SpanKind is the OTLP span kind; the numbers are the OTLP enum values.
type SpanKind int

// Span kinds.
const (
	SpanKindUnspecified SpanKind = 0
	SpanKindInternal    SpanKind = 1
	SpanKindServer      SpanKind = 2
	SpanKindClient      SpanKind = 3
	SpanKindProducer    SpanKind = 4
	SpanKindConsumer    SpanKind = 5
)

// StatusCode is the OTLP span status code; the numbers are the OTLP enum
// values.
type StatusCode int

// Status codes.
const (
	StatusUnset StatusCode = 0
	StatusOK    StatusCode = 1
	StatusError StatusCode = 2
)

// Status is a span's outcome.
type Status struct {
	Code        StatusCode
	Description string
}

// Event is a timestamped annotation on a span.
type Event struct {
	Name              string
	Time              time.Time
	Attributes        []KeyValue
	DroppedAttributes int
}

// Link points from a span to another span, possibly in another trace.
type Link struct {
	SpanContext       SpanContext
	Attributes        []KeyValue
	DroppedAttributes int
}

// SpanLimits bound what a single span may hold, so a runaway caller cannot
// grow memory or export payloads without limit. Zero fields take the
// specification defaults; a negative AttributeValueLengthLimit means no
// limit.
type SpanLimits struct {
	AttributeCountLimit         int
	AttributeValueLengthLimit   int
	EventCountLimit             int
	LinkCountLimit              int
	AttributePerEventCountLimit int
	AttributePerLinkCountLimit  int
}

// Default span limits from the OpenTelemetry specification.
const defaultSpanCountLimit = 128

func (l SpanLimits) normalized() SpanLimits {
	def := func(v int) int {
		if v <= 0 {
			return defaultSpanCountLimit
		}
		return v
	}
	l.AttributeCountLimit = def(l.AttributeCountLimit)
	l.EventCountLimit = def(l.EventCountLimit)
	l.LinkCountLimit = def(l.LinkCountLimit)
	l.AttributePerEventCountLimit = def(l.AttributePerEventCountLimit)
	l.AttributePerLinkCountLimit = def(l.AttributePerLinkCountLimit)
	if l.AttributeValueLengthLimit == 0 {
		l.AttributeValueLengthLimit = -1
	}
	return l
}

// SpanLimitsFromEnv reads the OTEL_*_LIMIT variables. Unset or malformed
// variables leave the default in place.
func SpanLimitsFromEnv() SpanLimits {
	get := func(names ...string) int {
		for _, n := range names {
			if v := os.Getenv(n); v != "" {
				if i, err := strconv.Atoi(v); err == nil && i >= 0 {
					return i
				}
			}
		}
		return 0
	}
	l := SpanLimits{
		AttributeCountLimit:         get("OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT", "OTEL_ATTRIBUTE_COUNT_LIMIT"),
		AttributeValueLengthLimit:   get("OTEL_SPAN_ATTRIBUTE_VALUE_LENGTH_LIMIT", "OTEL_ATTRIBUTE_VALUE_LENGTH_LIMIT"),
		EventCountLimit:             get("OTEL_SPAN_EVENT_COUNT_LIMIT"),
		LinkCountLimit:              get("OTEL_SPAN_LINK_COUNT_LIMIT"),
		AttributePerEventCountLimit: get("OTEL_EVENT_ATTRIBUTE_COUNT_LIMIT"),
		AttributePerLinkCountLimit:  get("OTEL_LINK_ATTRIBUTE_COUNT_LIMIT"),
	}
	return l.normalized()
}

// SpanData is an immutable snapshot of an ended span, handed to processors
// and exporters.
type SpanData struct {
	Name              string
	Kind              SpanKind
	SpanContext       SpanContext
	Parent            SpanContext
	Start             time.Time
	End               time.Time
	Attributes        []KeyValue
	DroppedAttributes int
	Events            []Event
	DroppedEvents     int
	Links             []Link
	DroppedLinks      int
	Status            Status
	Resource          *Resource
	Scope             Scope
}

// Duration is End minus Start.
func (d SpanData) Duration() time.Duration { return d.End.Sub(d.Start) }

// Attr returns the value of the named attribute.
func (d SpanData) Attr(key string) (Value, bool) {
	for _, kv := range d.Attributes {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return Value{}, false
}

// Span is one timed operation. All methods are safe for concurrent use and
// on a nil or non-recording span, where they do nothing.
type Span struct {
	mu        sync.Mutex
	tracer    *Tracer
	limits    SpanLimits
	sc        SpanContext
	parent    SpanContext
	name      string
	kind      SpanKind
	start     time.Time
	end       time.Time
	attrs     []KeyValue
	attrIndex map[string]int
	dropAttrs int
	events    []Event
	dropEvent int
	links     []Link
	dropLinks int
	status    Status
	ended     bool
	recording bool
}

// SpanContext returns the span's identity.
func (s *Span) SpanContext() SpanContext {
	if s == nil {
		return SpanContext{}
	}
	return s.sc
}

// IsRecording reports whether the span records data (it was sampled or
// recorded and has not ended).
func (s *Span) IsRecording() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recording && !s.ended
}

// SetName replaces the span name.
func (s *Span) SetName(name string) {
	if !s.lock() {
		return
	}
	defer s.mu.Unlock()
	s.name = name
}

// lock takes the mutex and reports true when the span still records; on
// false the mutex is not held.
func (s *Span) lock() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	if !s.recording || s.ended {
		s.mu.Unlock()
		return false
	}
	return true
}

// SetAttributes adds or replaces attributes. Past the count limit new keys
// are dropped and counted; existing keys are still updated.
func (s *Span) SetAttributes(attrs ...KeyValue) {
	if !s.lock() {
		return
	}
	defer s.mu.Unlock()
	for _, kv := range attrs {
		s.setAttrLocked(kv)
	}
}

func (s *Span) setAttrLocked(kv KeyValue) {
	if !kv.Valid() {
		return
	}
	kv.Value = kv.Value.truncate(s.limits.AttributeValueLengthLimit)
	if i, ok := s.attrIndex[kv.Key]; ok {
		s.attrs[i] = kv
		return
	}
	if len(s.attrs) >= s.limits.AttributeCountLimit {
		s.dropAttrs++
		return
	}
	if s.attrIndex == nil {
		s.attrIndex = make(map[string]int)
	}
	s.attrIndex[kv.Key] = len(s.attrs)
	s.attrs = append(s.attrs, kv)
}

// limitAttrs de-duplicates attrs, truncates values and keeps at most limit.
func limitAttrs(attrs []KeyValue, limit, valueLimit int) ([]KeyValue, int) {
	if len(attrs) == 0 {
		return nil, 0
	}
	seen := make(map[string]int, len(attrs))
	out := make([]KeyValue, 0, len(attrs))
	dropped := 0
	for _, kv := range attrs {
		if !kv.Valid() {
			continue
		}
		kv.Value = kv.Value.truncate(valueLimit)
		if i, ok := seen[kv.Key]; ok {
			out[i] = kv
			continue
		}
		if len(out) >= limit {
			dropped++
			continue
		}
		seen[kv.Key] = len(out)
		out = append(out, kv)
	}
	return out, dropped
}

// EventOption configures AddEvent.
type EventOption func(*Event)

// WithEventTime sets an explicit event timestamp.
func WithEventTime(t time.Time) EventOption { return func(e *Event) { e.Time = t } }

// AddEvent records a named event with attributes.
func (s *Span) AddEvent(name string, attrs []KeyValue, opts ...EventOption) {
	if !s.lock() {
		return
	}
	defer s.mu.Unlock()
	s.addEventLocked(name, attrs, opts...)
}

func (s *Span) addEventLocked(name string, attrs []KeyValue, opts ...EventOption) {
	if len(s.events) >= s.limits.EventCountLimit {
		s.dropEvent++
		return
	}
	ev := Event{Name: name}
	for _, o := range opts {
		o(&ev)
	}
	if ev.Time.IsZero() {
		ev.Time = s.now()
	}
	ev.Attributes, ev.DroppedAttributes = limitAttrs(attrs, s.limits.AttributePerEventCountLimit, s.limits.AttributeValueLengthLimit)
	s.events = append(s.events, ev)
}

// now returns a wall time derived from the span's monotonic start, so
// durations stay correct even if the wall clock jumps mid-span.
func (s *Span) now() time.Time {
	return s.start.Add(time.Since(s.start))
}

// AddLink links this span to another span context.
func (s *Span) AddLink(l Link) {
	if !s.lock() {
		return
	}
	defer s.mu.Unlock()
	s.addLinkLocked(l)
}

func (s *Span) addLinkLocked(l Link) {
	if !l.SpanContext.IsValid() {
		return
	}
	if len(s.links) >= s.limits.LinkCountLimit {
		s.dropLinks++
		return
	}
	var dropped int
	l.Attributes, dropped = limitAttrs(l.Attributes, s.limits.AttributePerLinkCountLimit, s.limits.AttributeValueLengthLimit)
	l.DroppedAttributes += dropped
	s.links = append(s.links, l)
}

// RecordError adds an "exception" event following the OpenTelemetry
// exception semantic conventions. It does not change the status; call
// SetStatus for that.
func (s *Span) RecordError(err error, attrs ...KeyValue) {
	if err == nil || !s.lock() {
		return
	}
	defer s.mu.Unlock()
	all := append([]KeyValue{
		String("exception.type", errorTypeName(err)),
		String("exception.message", err.Error()),
	}, attrs...)
	s.addEventLocked("exception", all)
}

// errorTypeName names the most specific error type in the chain.
func errorTypeName(err error) string {
	for {
		u := errors.Unwrap(err)
		if u == nil {
			break
		}
		// fmt.wrapError carries no type information worth reporting.
		if t := fmt.Sprintf("%T", err); t != "*fmt.wrapError" && t != "*fmt.wrapErrors" {
			return t
		}
		err = u
	}
	return fmt.Sprintf("%T", err)
}

// SetStatus sets the outcome. Ok is final; Unset never overrides a set
// status; a description is kept only for Error, as the specification says.
func (s *Span) SetStatus(code StatusCode, description string) {
	if !s.lock() {
		return
	}
	defer s.mu.Unlock()
	if s.status.Code == StatusOK || code == StatusUnset {
		return
	}
	if code != StatusError {
		description = ""
	}
	s.status = Status{Code: code, Description: description}
}

// SpanEndOption configures End.
type SpanEndOption func(*time.Time)

// WithEndTime sets an explicit end timestamp.
func WithEndTime(t time.Time) SpanEndOption { return func(p *time.Time) { *p = t } }

// End finishes the span and hands it to the span processors. Calls after the
// first are ignored.
func (s *Span) End(opts ...SpanEndOption) {
	if !s.lock() {
		return
	}
	var end time.Time
	for _, o := range opts {
		o(&end)
	}
	if end.IsZero() {
		end = s.now()
	}
	s.end = end
	s.ended = true
	data := s.snapshotLocked()
	s.mu.Unlock()
	for _, p := range s.tracer.provider.processorList() {
		p.OnEnd(data)
	}
}

func (s *Span) snapshotLocked() SpanData {
	d := SpanData{
		Name:              s.name,
		Kind:              s.kind,
		SpanContext:       s.sc,
		Parent:            s.parent,
		Start:             s.start,
		End:               s.end,
		Attributes:        append([]KeyValue(nil), s.attrs...),
		DroppedAttributes: s.dropAttrs,
		Events:            append([]Event(nil), s.events...),
		DroppedEvents:     s.dropEvent,
		Links:             append([]Link(nil), s.links...),
		DroppedLinks:      s.dropLinks,
		Status:            s.status,
		Resource:          s.tracer.provider.resource,
		Scope:             s.tracer.scope,
	}
	return d
}

// Snapshot returns the span's current data, ended or not. It is meant for
// tests and debugging views.
func (s *Span) Snapshot() SpanData {
	if s == nil || s.tracer == nil {
		return SpanData{SpanContext: s.SpanContext()}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

type spanKey struct{}

// ContextWithSpan returns ctx carrying s as the active span.
func ContextWithSpan(ctx context.Context, s *Span) context.Context {
	return context.WithValue(ctx, spanKey{}, s)
}

// ContextWithRemoteSpanContext returns ctx carrying a non-recording span for
// sc, so new spans become its children.
func ContextWithRemoteSpanContext(ctx context.Context, sc SpanContext) context.Context {
	sc.Remote = true
	return ContextWithSpan(ctx, &Span{sc: sc})
}

// SpanFromContext returns the active span, or a non-recording span (never
// nil) when there is none.
func SpanFromContext(ctx context.Context) *Span {
	if ctx != nil {
		if s, ok := ctx.Value(spanKey{}).(*Span); ok && s != nil {
			return s
		}
	}
	return &Span{}
}

// SpanContextFromContext returns the active span's context.
func SpanContextFromContext(ctx context.Context) SpanContext {
	return SpanFromContext(ctx).SpanContext()
}
