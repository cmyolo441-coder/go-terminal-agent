package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func newRecordingProvider(opts ...TracerProviderOption) (*TracerProvider, *InMemoryExporter) {
	exp := &InMemoryExporter{}
	opts = append(opts, WithSpanProcessor(NewSimpleSpanProcessor(exp)))
	return NewTracerProvider(opts...), exp
}

func TestParentChildAndContext(t *testing.T) {
	tp, exp := newRecordingProvider()
	tr := tp.Tracer("t")
	if s := SpanFromContext(context.Background()); s == nil || s.IsRecording() || s.SpanContext().IsValid() {
		t.Fatal("empty context must yield a non-nil, non-recording, invalid span")
	}
	ctx, root := tr.Start(context.Background(), "root")
	if SpanFromContext(ctx) != root || !root.IsRecording() {
		t.Fatal("context does not carry the started span")
	}
	cctx, child := tr.Start(ctx, "child")
	_, grandchild := tr.Start(cctx, "grandchild")
	_, other := tr.Start(ctx, "fresh", WithNewRoot())
	for _, s := range []*Span{grandchild, child, other, root} {
		s.End()
	}
	got := exp.Spans()
	if len(got) != 4 {
		t.Fatalf("%d spans", len(got))
	}
	byName := map[string]SpanData{}
	for _, s := range got {
		byName[s.Name] = s
	}
	r, c, g, f := byName["root"], byName["child"], byName["grandchild"], byName["fresh"]
	if c.SpanContext.TraceID != r.SpanContext.TraceID || g.SpanContext.TraceID != r.SpanContext.TraceID {
		t.Fatal("trace id not inherited")
	}
	if c.Parent.SpanID != r.SpanContext.SpanID || g.Parent.SpanID != c.SpanContext.SpanID || r.Parent.IsValid() {
		t.Fatal("parent links wrong")
	}
	if f.SpanContext.TraceID == r.SpanContext.TraceID || f.Parent.IsValid() {
		t.Fatal("WithNewRoot did not start a new trace")
	}
	if c.Duration() < 0 || r.End.Before(r.Start) {
		t.Fatal("negative duration")
	}
	if r.Resource == nil || r.Scope.Name != "t" {
		t.Fatal("resource or scope missing")
	}
	if v, ok := r.Resource.Get("service.name"); !ok || v.AsString() != DefaultServiceName {
		t.Fatal("default service.name missing")
	}
}

func TestSpanSemantics(t *testing.T) {
	tp, exp := newRecordingProvider(WithSpanLimits(SpanLimits{
		AttributeCountLimit: 2, AttributeValueLengthLimit: 3, EventCountLimit: 1, AttributePerEventCountLimit: 1, LinkCountLimit: 1,
	}))
	_, s := tp.Tracer("t").Start(context.Background(), "op")
	s.SetAttributes(String("a", "abcdef"), Int("b", 1), Int("c", 2), Int("b", 9), KeyValue{})
	s.AddEvent("e1", []KeyValue{Int("x", 1), Int("y", 2)})
	s.AddEvent("e2", nil)
	s.AddLink(Link{SpanContext: SpanContext{TraceID: TraceID{1}, SpanID: SpanID{1}}})
	s.AddLink(Link{SpanContext: SpanContext{TraceID: TraceID{2}, SpanID: SpanID{2}}})
	s.AddLink(Link{}) // invalid: ignored, not counted
	s.SetName("renamed")
	s.SetStatus(StatusError, "first")
	s.SetStatus(StatusUnset, "")
	if got := s.Snapshot().Status; got.Code != StatusError || got.Description != "first" {
		t.Fatalf("unset overrode error: %+v", got)
	}
	s.SetStatus(StatusOK, "desc dropped")
	s.SetStatus(StatusError, "after ok")
	s.End()
	s.End() // idempotent
	s.SetAttributes(String("late", "x"))

	spans := exp.Spans()
	if len(spans) != 1 {
		t.Fatalf("%d spans exported", len(spans))
	}
	d := spans[0]
	if d.Name != "renamed" {
		t.Fatalf("name %q", d.Name)
	}
	if a, _ := d.Attr("a"); a.AsString() != "abc" {
		t.Fatalf("value not truncated: %q", a.AsString())
	}
	if b, _ := d.Attr("b"); b.AsInt64() != 9 {
		t.Fatal("existing key not updated past the limit")
	}
	if _, ok := d.Attr("c"); ok || d.DroppedAttributes != 1 {
		t.Fatalf("attribute limit: dropped=%d", d.DroppedAttributes)
	}
	if _, ok := d.Attr("late"); ok {
		t.Fatal("attribute set after End")
	}
	if len(d.Events) != 1 || d.DroppedEvents != 1 || d.Events[0].DroppedAttributes != 1 {
		t.Fatalf("event limits: %+v dropped=%d", d.Events, d.DroppedEvents)
	}
	if len(d.Links) != 1 || d.DroppedLinks != 1 {
		t.Fatalf("link limits: %d dropped=%d", len(d.Links), d.DroppedLinks)
	}
	if d.Status.Code != StatusOK || d.Status.Description != "" {
		t.Fatalf("status %+v: OK must be final and carry no description", d.Status)
	}
}

func TestRecordErrorNamesWrappedType(t *testing.T) {
	tp, exp := newRecordingProvider()
	_, s := tp.Tracer("t").Start(context.Background(), "op")
	s.RecordError(fmt.Errorf("context: %w", &customErr{}))
	s.RecordError(nil)
	s.End()
	ev := exp.Spans()[0].Events
	if len(ev) != 1 {
		t.Fatalf("%d events", len(ev))
	}
	var typ string
	for _, kv := range ev[0].Attributes {
		if kv.Key == "exception.type" {
			typ = kv.Value.AsString()
		}
	}
	if typ != "*telemetry.customErr" {
		t.Fatalf("exception.type %q", typ)
	}
}

type customErr struct{}

func (*customErr) Error() string { return "custom" }

func TestNilAndNonRecordingSpansAreSafe(t *testing.T) {
	var s *Span
	s.SetAttributes(String("a", "b"))
	s.AddEvent("e", nil)
	s.RecordError(errors.New("x"))
	s.SetStatus(StatusError, "x")
	s.SetName("x")
	s.AddLink(Link{})
	s.End()
	if s.IsRecording() || s.SpanContext().IsValid() {
		t.Fatal("nil span recording")
	}
	tp, exp := newRecordingProvider(WithSampler(NeverSample()))
	ctx, dropped := tp.Tracer("t").Start(context.Background(), "x")
	if dropped.IsRecording() || !dropped.SpanContext().IsValid() || dropped.SpanContext().IsSampled() {
		t.Fatal("dropped span must be valid, non-recording and unsampled")
	}
	_, child := tp.Tracer("t").Start(ctx, "child")
	if child.SpanContext().TraceID != dropped.SpanContext().TraceID {
		t.Fatal("dropped span does not propagate its trace id")
	}
	dropped.End()
	child.End()
	if len(exp.Spans()) != 0 {
		t.Fatal("unsampled spans exported")
	}
}

func TestSamplers(t *testing.T) {
	if !strings.Contains(ParentBased(TraceIDRatioBased(0.25)).Description(), "TraceIDRatioBased{0.25}") {
		t.Fatal("description")
	}
	if TraceIDRatioBased(1).Description() != "AlwaysOnSampler" || TraceIDRatioBased(0).Description() != "AlwaysOffSampler" {
		t.Fatal("ratio extremes not simplified")
	}
	g := newIDGenerator()
	s := TraceIDRatioBased(0.25)
	sampled := 0
	const n = 20000
	for i := 0; i < n; i++ {
		tid := g.traceID()
		r1 := s.ShouldSample(SamplingParameters{TraceID: tid}).Decision
		r2 := s.ShouldSample(SamplingParameters{TraceID: tid}).Decision
		if r1 != r2 {
			t.Fatal("ratio sampler is not deterministic per trace id")
		}
		if r1 == RecordAndSample {
			sampled++
		}
	}
	if frac := float64(sampled) / n; frac < 0.22 || frac > 0.28 {
		t.Fatalf("sampled fraction %.3f, want about 0.25", frac)
	}
	pb := ParentBased(NeverSample())
	sampledParent := SpanContext{TraceID: TraceID{1}, SpanID: SpanID{1}, TraceFlags: FlagsSampled, Remote: true}
	if pb.ShouldSample(SamplingParameters{ParentContext: sampledParent}).Decision != RecordAndSample {
		t.Fatal("parent-based ignored a sampled parent")
	}
	sampledParent.TraceFlags = 0
	if ParentBased(AlwaysSample()).ShouldSample(SamplingParameters{ParentContext: sampledParent}).Decision != Drop {
		t.Fatal("parent-based ignored an unsampled parent")
	}
}

func TestSamplerFromEnv(t *testing.T) {
	t.Setenv("OTEL_TRACES_SAMPLER", "parentbased_traceidratio")
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "0.5")
	s, ok, err := SamplerFromEnv()
	if err != nil || !ok || s.Description() != "ParentBased{root:TraceIDRatioBased{0.5}}" {
		t.Fatalf("%v %v %v", s, ok, err)
	}
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "2")
	if _, _, err := SamplerFromEnv(); err == nil {
		t.Fatal("ratio 2 accepted")
	}
	t.Setenv("OTEL_TRACES_SAMPLER", "jaeger_remote")
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "")
	if _, _, err := SamplerFromEnv(); err == nil {
		t.Fatal("unknown sampler accepted")
	}
	t.Setenv("OTEL_TRACES_SAMPLER", "")
	if _, ok, err := SamplerFromEnv(); ok || err != nil {
		t.Fatal("unset variable reported a sampler")
	}
}

func TestTraceparent(t *testing.T) {
	sc := SpanContext{TraceID: TraceID{0xab, 1}, SpanID: SpanID{0xcd, 2}, TraceFlags: FlagsSampled}
	tp := sc.Traceparent()
	if tp != "00-ab010000000000000000000000000000-cd02000000000000-01" {
		t.Fatalf("traceparent %q", tp)
	}
	got, err := ParseTraceparent(tp)
	if err != nil || got.TraceID != sc.TraceID || got.SpanID != sc.SpanID || !got.IsSampled() || !got.Remote {
		t.Fatalf("round trip %+v %v", got, err)
	}
	if _, err := ParseTraceparent("cc-ab010000000000000000000000000000-cd02000000000000-01-extra"); err != nil {
		t.Fatalf("future version with extra fields rejected: %v", err)
	}
	for _, bad := range []string{
		"",
		"00-ab010000000000000000000000000000-cd02000000000000-01-extra",
		"ff-ab010000000000000000000000000000-cd02000000000000-01",
		"00-AB010000000000000000000000000000-cd02000000000000-01",
		"00-00000000000000000000000000000000-cd02000000000000-01",
		"00-ab010000000000000000000000000000-0000000000000000-01",
		"00-ab01-cd02000000000000-01",
		"00-ab010000000000000000000000000000-cd02000000000000-zz",
	} {
		if _, err := ParseTraceparent(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestHTTPAndEnvPropagation(t *testing.T) {
	tp, exp := newRecordingProvider()
	tr := tp.Tracer("t")
	ctx, span := tr.Start(context.Background(), "client")
	h := http.Header{}
	Inject(ctx, h)
	if h.Get("traceparent") != span.SpanContext().Traceparent() {
		t.Fatalf("injected %q", h.Get("traceparent"))
	}
	h.Set("tracestate", "vendor=abc")
	sctx := Extract(context.Background(), h)
	_, server := tr.Start(sctx, "server", WithSpanKind(SpanKindServer))
	server.End()
	span.End()
	var srv SpanData
	for _, s := range exp.Spans() {
		if s.Name == "server" {
			srv = s
		}
	}
	if srv.SpanContext.TraceID != span.SpanContext().TraceID || srv.Parent.SpanID != span.SpanContext().SpanID || !srv.Parent.Remote {
		t.Fatalf("server span not continued from the header: %+v", srv)
	}
	if srv.SpanContext.TraceState != "vendor=abc" {
		t.Fatalf("tracestate %q", srv.SpanContext.TraceState)
	}
	// Bad headers leave the context alone.
	h.Set("traceparent", "garbage")
	if SpanContextFromContext(Extract(context.Background(), h)).IsValid() {
		t.Fatal("garbage traceparent extracted")
	}
	Inject(context.Background(), http.Header{}) // no span: no panic, no header

	env := InjectEnv(ctx, []string{"PATH=/bin", "TRACEPARENT=old"})
	if len(env) != 2 || env[1] != "TRACEPARENT="+span.SpanContext().Traceparent() {
		t.Fatalf("env %v", env)
	}
	t.Setenv("TRACEPARENT", span.SpanContext().Traceparent())
	t.Setenv("TRACESTATE", "bad\x01state")
	ectx := ExtractEnv(context.Background())
	if got := SpanContextFromContext(ectx); got.TraceID != span.SpanContext().TraceID || got.TraceState != "" {
		t.Fatalf("env extract %+v", got)
	}
}

func TestResourceFromEnvAndMerge(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "a=1,b=hello%2Cworld")
	t.Setenv("OTEL_SERVICE_NAME", "svc")
	r, err := resourceFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := r.Get("b"); v.AsString() != "hello,world" {
		t.Fatalf("percent-decoding: %q", v.AsString())
	}
	if v, _ := r.Get("service.name"); v.AsString() != "svc" {
		t.Fatal("OTEL_SERVICE_NAME ignored")
	}
	m := NewResource(String("a", "0"), String("keep", "y")).Merge(r)
	if v, _ := m.Get("a"); v.AsString() != "1" {
		t.Fatal("merge precedence")
	}
	if v, ok := m.Get("keep"); !ok || v.AsString() != "y" {
		t.Fatal("merge lost attribute")
	}
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "a=1,broken")
	if _, err := resourceFromEnv(); err == nil {
		t.Fatal("malformed list accepted")
	}
}

func TestConcurrentSpans(t *testing.T) {
	tp, exp := newRecordingProvider()
	tr := tp.Tracer("t")
	ctx, root := tr.Start(context.Background(), "root")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, s := tr.Start(ctx, "child")
			s.SetAttributes(Int("i", i))
			root.AddEvent("child started", nil)
			s.End()
		}(i)
	}
	wg.Wait()
	root.End()
	if len(exp.Spans()) != 51 {
		t.Fatalf("%d spans", len(exp.Spans()))
	}
	ids := map[SpanID]bool{}
	for _, s := range exp.Spans() {
		if ids[s.SpanContext.SpanID] {
			t.Fatal("duplicate span id")
		}
		ids[s.SpanContext.SpanID] = true
	}
}

func TestShutdownStopsRecording(t *testing.T) {
	tp, exp := newRecordingProvider()
	if err := tp.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = tp.Shutdown(context.Background())
	_, s := tp.Tracer("t").Start(context.Background(), "late", WithTimestamp(time.Now()))
	s.End()
	if s.IsRecording() || len(exp.Spans()) != 0 {
		t.Fatal("span recorded after shutdown")
	}
}

func TestSpanLimitsFromEnv(t *testing.T) {
	t.Setenv("OTEL_ATTRIBUTE_COUNT_LIMIT", "7")
	t.Setenv("OTEL_SPAN_ATTRIBUTE_VALUE_LENGTH_LIMIT", "12")
	t.Setenv("OTEL_SPAN_EVENT_COUNT_LIMIT", "x")
	l := SpanLimitsFromEnv()
	if l.AttributeCountLimit != 7 || l.AttributeValueLengthLimit != 12 || l.EventCountLimit != defaultSpanCountLimit {
		t.Fatalf("%+v", l)
	}
}
