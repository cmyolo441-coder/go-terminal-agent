package telemetry

import (
	"context"
	"math"
	"strings"
	"sync"
	"testing"
)

func findMetric(t *testing.T, rm *ResourceMetrics, name string) Metric {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m
			}
		}
	}
	t.Fatalf("metric %q not collected", name)
	return Metric{}
}

func TestCounterCumulativeAndDelta(t *testing.T) {
	for _, temp := range []Temporality{CumulativeTemporality, DeltaTemporality} {
		mp := NewMeterProvider(WithTemporality(temp))
		c, err := mp.Meter("m").Int64Counter("calls")
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		c.Add(ctx, 2, String("k", "a"))
		c.Add(ctx, -5, String("k", "a")) // ignored
		c.Add(ctx, 1, String("k", "b"))
		first := findMetric(t, mp.Collect(ctx), "calls").Data.(Sum)
		c.Add(ctx, 4, String("k", "a"))
		second := findMetric(t, mp.Collect(ctx), "calls").Data.(Sum)
		if first.Temporality != temp || !first.IsMonotonic || len(first.DataPoints) != 2 {
			t.Fatalf("%v: first %+v", temp, first)
		}
		if first.DataPoints[0].Int != 2 || first.DataPoints[1].Int != 1 {
			t.Fatalf("%v: first values %+v", temp, first.DataPoints)
		}
		switch temp {
		case CumulativeTemporality:
			if len(second.DataPoints) != 2 || second.DataPoints[0].Int != 6 || !second.DataPoints[0].Start.Equal(first.DataPoints[0].Start) {
				t.Fatalf("cumulative second %+v", second.DataPoints)
			}
		case DeltaTemporality:
			if len(second.DataPoints) != 1 || second.DataPoints[0].Int != 4 || !second.DataPoints[0].Start.Equal(first.DataPoints[0].Time) {
				t.Fatalf("delta second %+v", second.DataPoints)
			}
		}
	}
}

func TestHistogramAggregation(t *testing.T) {
	mp := NewMeterProvider()
	h, _ := mp.Meter("m").Float64Histogram("lat", WithExplicitBuckets(10, 1, 5, 5, math.NaN()))
	ctx := context.Background()
	for _, v := range []float64{0.5, 1, 3, 5, 7, 100, math.NaN(), math.Inf(1)} {
		h.Record(ctx, v)
	}
	dp := findMetric(t, mp.Collect(ctx), "lat").Data.(Histogram).DataPoints[0]
	if want := []float64{1, 5, 10}; len(dp.Bounds) != 3 || dp.Bounds[0] != want[0] || dp.Bounds[2] != want[2] {
		t.Fatalf("bounds %v", dp.Bounds)
	}
	want := []uint64{2, 2, 1, 1}
	for i, c := range dp.BucketCounts {
		if c != want[i] {
			t.Fatalf("bucket counts %v, want %v", dp.BucketCounts, want)
		}
	}
	if dp.Count != 6 || dp.Sum != 116.5 || dp.Min != 0.5 || dp.Max != 100 {
		t.Fatalf("point %+v", dp)
	}
	ih, _ := mp.Meter("m").Int64Histogram("sizes")
	ih.Record(ctx, 7)
	if d := findMetric(t, mp.Collect(ctx), "sizes").Data.(Histogram).DataPoints[0]; len(d.Bounds) != len(DefaultHistogramBuckets) || d.Count != 1 {
		t.Fatalf("default buckets %+v", d)
	}
}

func TestInstrumentRegistration(t *testing.T) {
	m := NewMeterProvider().Meter("m")
	a, err := m.Int64Counter("Requests", WithUnit("1"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Int64Counter("requests", WithUnit("1"))
	if err != nil || a.inst != b.inst {
		t.Fatal("same instrument (case-insensitive) not reused")
	}
	if _, err := m.Float64Histogram("requests"); err == nil {
		t.Fatal("conflicting kind accepted")
	}
	for _, bad := range []string{"", "1abc", "has space", strings.Repeat("a", 256)} {
		if _, err := m.Int64Counter(bad); err == nil {
			t.Fatalf("name %q accepted", bad)
		}
	}
	var nilCounter *Int64Counter
	nilCounter.Add(context.Background(), 1) // safe
	var nilHist *Float64Histogram
	nilHist.Record(context.Background(), 1)
}

func TestCardinalityOverflow(t *testing.T) {
	mp := NewMeterProvider(WithCardinalityLimit(3))
	c, _ := mp.Meter("m").Int64Counter("c")
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		c.Add(ctx, 1, Int("i", i))
	}
	c.Add(ctx, 1, Int("i", 0)) // existing set still aggregates normally
	pts := findMetric(t, mp.Collect(ctx), "c").Data.(Sum).DataPoints
	if len(pts) != 3 {
		t.Fatalf("%d points, want 2 + overflow", len(pts))
	}
	var overflow, zero int64
	for _, p := range pts {
		for _, kv := range p.Attributes {
			if kv.Key == "otel.metric.overflow" {
				overflow = p.Int
			}
			if kv.Key == "i" && kv.Value.AsInt64() == 0 {
				zero = p.Int
			}
		}
	}
	if overflow != 3 || zero != 2 {
		t.Fatalf("overflow=%d zero=%d", overflow, zero)
	}
}

func TestObservableInstruments(t *testing.T) {
	for _, temp := range []Temporality{CumulativeTemporality, DeltaTemporality} {
		mp := NewMeterProvider(WithTemporality(temp))
		m := mp.Meter("m")
		var total float64
		if err := m.Int64ObservableCounter("total", func(_ context.Context, o Observe) { o(total, String("k", "v")) }); err != nil {
			t.Fatal(err)
		}
		if err := m.Float64ObservableGauge("g", func(_ context.Context, o Observe) { o(1.5); o(math.NaN()) }); err != nil {
			t.Fatal(err)
		}
		if err := m.Int64ObservableUpDownCounter("ud", func(_ context.Context, o Observe) { o(-3) }); err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		total = 10
		mp.Collect(ctx)
		total = 25
		rm := mp.Collect(ctx)
		s := findMetric(t, rm, "total").Data.(Sum)
		want := int64(25)
		if temp == DeltaTemporality {
			want = 15
		}
		if s.DataPoints[0].Int != want || !s.IsMonotonic || s.Temporality != temp {
			t.Fatalf("%v: observable counter %+v", temp, s)
		}
		g := findMetric(t, rm, "g").Data.(Gauge)
		if len(g.DataPoints) != 1 || g.DataPoints[0].Float != 1.5 || !g.DataPoints[0].Start.IsZero() {
			t.Fatalf("gauge %+v", g)
		}
		ud := findMetric(t, rm, "ud").Data.(Sum)
		if ud.IsMonotonic || ud.Temporality != CumulativeTemporality || ud.DataPoints[0].Int != -3 {
			t.Fatalf("updown %+v", ud)
		}
	}
}

func TestExemplarFilters(t *testing.T) {
	tp := NewTracerProvider()
	sctx, span := tp.Tracer("t").Start(context.Background(), "op")
	defer span.End()
	for _, tc := range []struct {
		filter      ExemplarFilter
		ctx         context.Context
		wantTraceID bool
		wantAny     bool
	}{
		{ExemplarTraceBased, sctx, true, true},
		{ExemplarTraceBased, context.Background(), false, false},
		{ExemplarAlwaysOn, context.Background(), false, true},
		{ExemplarAlwaysOff, sctx, false, false},
	} {
		mp := NewMeterProvider(WithExemplarFilter(tc.filter))
		h, _ := mp.Meter("m").Float64Histogram("h")
		h.Record(tc.ctx, 2)
		ex := findMetric(t, mp.Collect(context.Background()), "h").Data.(Histogram).DataPoints[0].Exemplars
		if (len(ex) == 1) != tc.wantAny {
			t.Fatalf("filter %v: exemplars %+v", tc.filter, ex)
		}
		if tc.wantAny && ex[0].TraceID.IsValid() != tc.wantTraceID {
			t.Fatalf("filter %v: exemplar trace id %v", tc.filter, ex[0].TraceID)
		}
	}
}

func TestConcurrentRecording(t *testing.T) {
	mp := NewMeterProvider()
	m := mp.Meter("m")
	c, _ := m.Int64Counter("c")
	h, _ := m.Float64Histogram("h")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				c.Add(context.Background(), 1, Int("w", i%4))
				h.Record(context.Background(), float64(j))
				if j%25 == 0 {
					mp.Collect(context.Background())
				}
			}
		}(i)
	}
	wg.Wait()
	var total int64
	for _, p := range findMetric(t, mp.Collect(context.Background()), "c").Data.(Sum).DataPoints {
		total += p.Int
	}
	if total != 2000 {
		t.Fatalf("total %d", total)
	}
}

func TestEnvSelectors(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE", "Delta")
	t.Setenv("OTEL_METRICS_EXEMPLAR_FILTER", "always_off")
	if TemporalityFromEnv() != DeltaTemporality || ExemplarFilterFromEnv() != ExemplarAlwaysOff {
		t.Fatal("env selectors ignored")
	}
}
