package telemetry

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

var (
	hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)
	hex16 = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

func newTestExporter(t *testing.T, c *collector, o OTLPOptions) *OTLPExporter {
	t.Helper()
	o.Endpoint = c.srv.URL
	e, err := NewOTLPExporter(o)
	if err != nil {
		t.Fatal(err)
	}
	e.sleep = func(context.Context, time.Duration) error { return nil }
	e.jitter = func() float64 { return 0 }
	return e
}

func TestOTLPTracesWireFormat(t *testing.T) {
	c := newCollector(t)
	exp := newTestExporter(t, c, OTLPOptions{Headers: map[string]string{"x-api-key": "secret"}})
	tp := NewTracerProvider(
		WithResource(NewResource(String("service.name", "gta-test"), Int("pid", 7))),
		WithSpanProcessor(NewBatchSpanProcessor(exp, BatchOptions{ScheduledDelay: time.Hour})),
	)
	tr := tp.Tracer("gta/test", WithInstrumentationVersion("1.2.3"))

	remote := SpanContext{TraceID: TraceID{9, 9}, SpanID: SpanID{8, 8}, TraceFlags: FlagsSampled}
	ctx, parent := tr.Start(context.Background(), "parent", WithSpanKind(SpanKindServer),
		WithLinks(Link{SpanContext: remote, Attributes: []KeyValue{String("why", "retry")}}))
	parent.SetAttributes(
		String("s", "v"), Bool("b", true), Int64("i", 42), Float64("f", 1.5),
		StringSlice("ss", []string{"a", "b"}), Int64Slice("is", []int64{1, -2}),
		Float64Slice("fs", []float64{0.5}), BoolSlice("bs", []bool{false}),
	)
	_, child := tr.Start(ctx, "child", WithSpanKind(SpanKindClient))
	child.AddEvent("note", []KeyValue{Int("n", 3)})
	child.RecordError(errors.New("kaput"))
	child.SetStatus(StatusError, "it broke")
	child.End()
	parent.SetStatus(StatusOK, "ignored for ok")
	parent.End()
	if err := tp.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}

	reqs := c.ByPath("/v1/traces")
	if len(reqs) != 1 {
		t.Fatalf("got %d trace requests", len(reqs))
	}
	r := reqs[0]
	if ct := r.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type %q", ct)
	}
	if r.Header.Get("x-api-key") != "secret" {
		t.Fatal("configured header not sent")
	}
	rs := arr(t, r.Body, "resourceSpans")
	if len(rs) != 1 {
		t.Fatalf("%d resourceSpans", len(rs))
	}
	res := obj(t, rs[0], "resource")
	ra := attrMap(t, res)
	if strAttr(t, ra, "service.name") != "gta-test" || ra["pid"]["intValue"] != "7" {
		t.Fatalf("resource attributes %v", ra)
	}
	ss := arr(t, rs[0], "scopeSpans")
	scope := obj(t, ss[0], "scope")
	if str(t, scope, "name") != "gta/test" || str(t, scope, "version") != "1.2.3" {
		t.Fatalf("scope %v", scope)
	}
	spans := arr(t, ss[0], "spans")
	if len(spans) != 2 {
		t.Fatalf("%d spans", len(spans))
	}
	byName := map[string]map[string]any{}
	for _, s := range spans {
		m := s.(map[string]any)
		byName[m["name"].(string)] = m
	}
	p, ch := byName["parent"], byName["child"]
	for _, s := range []map[string]any{p, ch} {
		if !hex32.MatchString(str(t, s, "traceId")) || !hex16.MatchString(str(t, s, "spanId")) {
			t.Fatalf("ids not lower-case hex: %v / %v", s["traceId"], s["spanId"])
		}
		start, end := int64Str(t, s, "startTimeUnixNano"), int64Str(t, s, "endTimeUnixNano")
		if start <= 0 || end < start {
			t.Fatalf("times %d..%d", start, end)
		}
	}
	if str(t, ch, "traceId") != str(t, p, "traceId") || str(t, ch, "parentSpanId") != str(t, p, "spanId") {
		t.Fatal("child not linked to parent")
	}
	if _, ok := p["parentSpanId"]; ok {
		t.Fatal("root span has a parentSpanId")
	}
	if num(t, p, "kind") != 2 || num(t, ch, "kind") != 3 {
		t.Fatalf("kinds %v %v", p["kind"], ch["kind"])
	}
	// flags: sampled (0x01) plus "is-remote known" (0x100), parent local.
	if num(t, ch, "flags") != 0x101 {
		t.Fatalf("flags %v", ch["flags"])
	}
	pa := attrMap(t, p)
	if pa["s"]["stringValue"] != "v" || pa["b"]["boolValue"] != true || pa["i"]["intValue"] != "42" || pa["f"]["doubleValue"] != 1.5 {
		t.Fatalf("scalar attributes %v", pa)
	}
	ssv := pa["ss"]["arrayValue"].(map[string]any)["values"].([]any)
	if len(ssv) != 2 || ssv[1].(map[string]any)["stringValue"] != "b" {
		t.Fatalf("string array %v", pa["ss"])
	}
	isv := pa["is"]["arrayValue"].(map[string]any)["values"].([]any)
	if isv[1].(map[string]any)["intValue"] != "-2" {
		t.Fatalf("int array %v", pa["is"])
	}
	if st := obj(t, p, "status"); st["code"] != 1.0 || st["message"] != nil {
		t.Fatalf("ok status %v", st)
	}
	if st := obj(t, ch, "status"); st["code"] != 2.0 || st["message"] != "it broke" {
		t.Fatalf("error status %v", st)
	}
	links := arr(t, p, "links")
	if str(t, links[0], "traceId") != remote.TraceID.String() || str(t, links[0], "spanId") != remote.SpanID.String() {
		t.Fatalf("link %v", links[0])
	}
	events := arr(t, ch, "events")
	if len(events) != 2 || str(t, events[0], "name") != "note" || str(t, events[1], "name") != "exception" {
		t.Fatalf("events %v", events)
	}
	if int64Str(t, events[0], "timeUnixNano") <= 0 {
		t.Fatal("event time")
	}
	ex := attrMap(t, events[1])
	if strAttr(t, ex, "exception.message") != "kaput" || strAttr(t, ex, "exception.type") != "*errors.errorString" {
		t.Fatalf("exception attributes %v", ex)
	}
}

func TestOTLPMetricsWireFormat(t *testing.T) {
	c := newCollector(t)
	exp := newTestExporter(t, c, OTLPOptions{})
	mp := NewMeterProvider(WithMetricResource(NewResource(String("service.name", "m"))), WithPeriodicReader(exp, ReaderOptions{Interval: time.Hour}))
	defer mp.Shutdown(context.Background())
	tp := NewTracerProvider()
	m := mp.Meter("gta/metrics")
	cnt, _ := m.Int64Counter("requests", WithUnit("{request}"), WithDescription("reqs"))
	fc, _ := m.Float64Counter("cost", WithUnit("USD"))
	h, _ := m.Float64Histogram("latency", WithUnit("s"), WithExplicitBuckets(1, 2))
	if err := m.Float64ObservableGauge("temp", func(_ context.Context, o Observe) { o(21.5, String("room", "a")) }); err != nil {
		t.Fatal(err)
	}
	ctx, span := tp.Tracer("t").Start(context.Background(), "op")
	cnt.Add(ctx, 2, String("route", "a"))
	cnt.Add(ctx, 3, String("route", "a"))
	fc.Add(ctx, 0.25)
	for _, v := range []float64{0.5, 1, 1.5, 3} {
		h.Record(ctx, v)
	}
	span.End()
	if err := mp.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	reqs := c.ByPath("/v1/metrics")
	if len(reqs) != 1 {
		t.Fatalf("%d metric requests", len(reqs))
	}
	rm := arr(t, reqs[0].Body, "resourceMetrics")
	if strAttr(t, attrMap(t, obj(t, rm[0], "resource")), "service.name") != "m" {
		t.Fatal("resource")
	}
	sm := arr(t, rm[0], "scopeMetrics")
	if str(t, obj(t, sm[0], "scope"), "name") != "gta/metrics" {
		t.Fatal("scope")
	}
	metrics := map[string]map[string]any{}
	for _, mm := range arr(t, sm[0], "metrics") {
		o := mm.(map[string]any)
		metrics[o["name"].(string)] = o
	}
	req := metrics["requests"]
	if req["unit"] != "{request}" || req["description"] != "reqs" {
		t.Fatalf("metadata %v", req)
	}
	sum := obj(t, req, "sum")
	if num(t, sum, "aggregationTemporality") != 2 || sum["isMonotonic"] != true {
		t.Fatalf("sum %v", sum)
	}
	dp := arr(t, sum, "dataPoints")[0]
	if str(t, dp, "asInt") != "5" || strAttr(t, attrMap(t, dp), "route") != "a" {
		t.Fatalf("data point %v", dp)
	}
	if int64Str(t, dp, "startTimeUnixNano") > int64Str(t, dp, "timeUnixNano") {
		t.Fatal("start after time")
	}
	exs := arr(t, dp, "exemplars")
	if str(t, exs[0], "traceId") != span.SpanContext().TraceID.String() || str(t, exs[0], "asInt") != "3" {
		t.Fatalf("exemplar %v", exs[0])
	}
	if cdp := arr(t, obj(t, metrics["cost"], "sum"), "dataPoints")[0]; num(t, cdp, "asDouble") != 0.25 {
		t.Fatalf("double sum %v", cdp)
	}
	hist := obj(t, metrics["latency"], "histogram")
	if num(t, hist, "aggregationTemporality") != 2 {
		t.Fatal("histogram temporality")
	}
	hdp := arr(t, hist, "dataPoints")[0]
	if str(t, hdp, "count") != "4" || num(t, hdp, "sum") != 6 || num(t, hdp, "min") != 0.5 || num(t, hdp, "max") != 3 {
		t.Fatalf("histogram point %v", hdp)
	}
	// Buckets are upper-inclusive: (-inf,1] holds 0.5 and 1.
	bc := arr(t, hdp, "bucketCounts")
	if len(bc) != 3 || bc[0] != "2" || bc[1] != "1" || bc[2] != "1" {
		t.Fatalf("bucketCounts %v", bc)
	}
	if eb := arr(t, hdp, "explicitBounds"); len(eb) != 2 || eb[0] != 1.0 || eb[1] != 2.0 {
		t.Fatalf("explicitBounds %v", eb)
	}
	gdp := arr(t, obj(t, metrics["temp"], "gauge"), "dataPoints")[0]
	if num(t, gdp, "asDouble") != 21.5 {
		t.Fatalf("gauge %v", gdp)
	}
	if _, ok := gdp.(map[string]any)["startTimeUnixNano"]; ok {
		t.Fatal("gauge carries a start time")
	}
}

func TestOTLPRetriesOn429And5xx(t *testing.T) {
	c := newCollector(t)
	c.respond = func(n int, w http.ResponseWriter) bool {
		switch n {
		case 0:
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			return true
		case 1:
			w.WriteHeader(http.StatusServiceUnavailable)
			return true
		}
		return false
	}
	exp := newTestExporter(t, c, OTLPOptions{Retry: RetryOptions{InitialInterval: 100 * time.Millisecond}})
	var sleeps []time.Duration
	exp.sleep = func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }
	if err := exp.ExportSpans(context.Background(), []SpanData{testSpanData()}); err != nil {
		t.Fatal(err)
	}
	if len(c.Requests()) != 3 {
		t.Fatalf("%d requests", len(c.Requests()))
	}
	// Retry-After wins over backoff; then backoff 2^1 * 100ms with
	// jitter 0 = half of 200ms.
	if len(sleeps) != 2 || sleeps[0] != 2*time.Second || sleeps[1] != 100*time.Millisecond {
		t.Fatalf("sleeps %v", sleeps)
	}
	if st := exp.Stats(); st.Requests != 3 || st.Retries != 2 || st.Failures != 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestOTLPGivesUpAfterMaxElapsed(t *testing.T) {
	c := newCollector(t)
	c.respond = func(_ int, w http.ResponseWriter) bool { w.WriteHeader(http.StatusBadGateway); return true }
	exp := newTestExporter(t, c, OTLPOptions{Retry: RetryOptions{InitialInterval: time.Second, MaxInterval: time.Second, MaxElapsedTime: 3 * time.Second}})
	now := time.Now()
	exp.now = func() time.Time { return now }
	exp.sleep = func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil }
	err := exp.ExportSpans(context.Background(), []SpanData{testSpanData()})
	var ee *ExportError
	if !errors.As(err, &ee) || ee.StatusCode != 502 || ee.Attempts < 3 {
		t.Fatalf("got %v", err)
	}
	if exp.Stats().Failures != 1 {
		t.Fatal("failure not counted")
	}
}

func TestOTLPPermanentErrorNotRetried(t *testing.T) {
	c := newCollector(t)
	c.respond = func(_ int, w http.ResponseWriter) bool {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":3,"message":"invalid span"}`))
		return true
	}
	exp := newTestExporter(t, c, OTLPOptions{})
	err := exp.ExportSpans(context.Background(), []SpanData{testSpanData()})
	var ee *ExportError
	if !errors.As(err, &ee) || ee.StatusCode != 400 || ee.Message != "invalid span" || ee.Attempts != 1 {
		t.Fatalf("got %#v", err)
	}
	if len(c.Requests()) != 1 {
		t.Fatalf("400 was retried: %d requests", len(c.Requests()))
	}
	if !strings.Contains(err.Error(), "HTTP 400") {
		t.Fatalf("message %q", err)
	}
}

func TestOTLPGzipAndPartialSuccess(t *testing.T) {
	c := newCollector(t)
	c.respond = func(_ int, w http.ResponseWriter) bool {
		_, _ = w.Write([]byte(`{"partialSuccess":{"rejectedSpans":"2","errorMessage":"too old"}}`))
		return true
	}
	var got error
	SetErrorHandler(func(err error) { got = err })
	defer SetErrorHandler(nil)
	exp := newTestExporter(t, c, OTLPOptions{Compression: "gzip"})
	if err := exp.ExportSpans(context.Background(), []SpanData{testSpanData()}); err != nil {
		t.Fatal(err)
	}
	r := c.Requests()[0]
	if !r.Gzipped || len(arr(t, r.Body, "resourceSpans")) != 1 {
		t.Fatalf("gzip request %+v", r)
	}
	if exp.Stats().RejectedSpans != 2 || got == nil || !strings.Contains(got.Error(), "too old") || LastError() == nil {
		t.Fatalf("partial success not reported: %v %+v", got, exp.Stats())
	}
}

func TestOTLPEnvironment(t *testing.T) {
	c := newCollector(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", c.srv.URL+"/")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "x-api-key=abc%20def, team = gta")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS", "team=traces")
	t.Setenv("OTEL_SERVICE_NAME", "svc-from-env")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment=ci,service.name=ignored")
	t.Setenv("OTEL_EXPORTER_OTLP_COMPRESSION", "gzip")
	tel, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if !tel.Enabled() || !strings.HasPrefix(tel.Description(), "otlp "+c.srv.URL) || len(tel.Warnings()) != 0 {
		t.Fatalf("enabled=%v description=%q warnings=%v", tel.Enabled(), tel.Description(), tel.Warnings())
	}
	_, span := tel.Tracer.Start(context.Background(), "hello")
	span.End()
	tel.Instruments.RecordError(context.Background(), "model", "rate_limit")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tel.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	traces := c.ByPath("/v1/traces")
	if len(traces) != 1 {
		t.Fatalf("%d trace requests", len(traces))
	}
	h := traces[0].Header
	if h.Get("x-api-key") != "abc def" || h.Get("team") != "traces" || !traces[0].Gzipped {
		t.Fatalf("headers %v gzip=%v", h, traces[0].Gzipped)
	}
	ra := attrMap(t, obj(t, arr(t, traces[0].Body, "resourceSpans")[0], "resource"))
	if strAttr(t, ra, "service.name") != "svc-from-env" || strAttr(t, ra, "deployment.environment") != "ci" ||
		strAttr(t, ra, "telemetry.sdk.language") != "go" {
		t.Fatalf("resource %v", ra)
	}
	metrics := c.ByPath("/v1/metrics")
	if len(metrics) != 1 {
		t.Fatalf("%d metric requests (Shutdown must flush metrics)", len(metrics))
	}
	if metrics[0].Header.Get("team") != "gta" {
		t.Fatal("signal headers leaked across signals")
	}
}

func TestOTLPSignalEndpointsAndValidation(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "https://traces.example/custom")
	t.Setenv("OTEL_EXPORTER_OTLP_TIMEOUT", "250")
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "grpc")
	o, err := OTLPOptionsFromEnv()
	if err == nil || !strings.Contains(err.Error(), "grpc") {
		t.Fatalf("grpc accepted: %v", err)
	}
	e, err := NewOTLPExporter(o)
	if err != nil {
		t.Fatal(err)
	}
	if e.TracesURL() != "https://traces.example/custom" || e.MetricsURL() != DefaultOTLPEndpoint+"/v1/metrics" || e.o.Timeout != 250*time.Millisecond {
		t.Fatalf("urls %q %q timeout %s", e.TracesURL(), e.MetricsURL(), e.o.Timeout)
	}
	for _, bad := range []OTLPOptions{
		{Endpoint: "localhost:4318"},
		{Compression: "zstd"},
		{Headers: map[string]string{"bad header": "x"}},
		{Headers: map[string]string{"x": "a\r\nb"}},
	} {
		if _, err := NewOTLPExporter(bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "novalue")
	if _, err := OTLPOptionsFromEnv(); err == nil {
		t.Fatal("malformed headers accepted")
	}
}

func TestOTLPShutdown(t *testing.T) {
	c := newCollector(t)
	exp := newTestExporter(t, c, OTLPOptions{})
	_ = exp.Shutdown(context.Background())
	if err := exp.ExportSpans(context.Background(), []SpanData{testSpanData()}); !errors.Is(err, ErrExporterShutdown) {
		t.Fatalf("got %v", err)
	}
	if len(c.Requests()) != 0 {
		t.Fatal("exported after shutdown")
	}
}

func testSpanData() SpanData {
	now := time.Now()
	return SpanData{
		Name:        "x",
		Kind:        SpanKindInternal,
		SpanContext: SpanContext{TraceID: TraceID{1}, SpanID: SpanID{2}, TraceFlags: FlagsSampled},
		Start:       now,
		End:         now.Add(time.Millisecond),
		Resource:    NewResource(String("service.name", "t")),
		Scope:       Scope{Name: "t"},
	}
}
