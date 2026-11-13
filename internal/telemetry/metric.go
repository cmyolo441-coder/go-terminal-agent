package telemetry

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Temporality is the OTLP aggregation temporality; the numbers are the OTLP
// enum values.
type Temporality int

// Temporalities.
const (
	DeltaTemporality      Temporality = 1
	CumulativeTemporality Temporality = 2
)

// ResourceMetrics is one collection of every instrument.
type ResourceMetrics struct {
	Resource     *Resource
	ScopeMetrics []ScopeMetrics
}

// ScopeMetrics groups the metrics of one meter.
type ScopeMetrics struct {
	Scope   Scope
	Metrics []Metric
}

// Metric is one instrument's collected data. Data is a Sum, Gauge or
// Histogram.
type Metric struct {
	Name        string
	Description string
	Unit        string
	Data        any
}

// Sum is the data of counters and up-down counters.
type Sum struct {
	Temporality Temporality
	IsMonotonic bool
	DataPoints  []NumberDataPoint
}

// Gauge is the data of gauges.
type Gauge struct {
	DataPoints []NumberDataPoint
}

// Histogram is the data of histograms with explicit buckets.
type Histogram struct {
	Temporality Temporality
	DataPoints  []HistogramDataPoint
}

// NumberDataPoint is one attribute set's value.
type NumberDataPoint struct {
	Attributes []KeyValue
	Start      time.Time
	Time       time.Time
	IsInt      bool
	Int        int64
	Float      float64
	Exemplars  []Exemplar
}

// Value returns the point's value as a float.
func (p NumberDataPoint) Value() float64 {
	if p.IsInt {
		return float64(p.Int)
	}
	return p.Float
}

// HistogramDataPoint is one attribute set's distribution.
type HistogramDataPoint struct {
	Attributes   []KeyValue
	Start        time.Time
	Time         time.Time
	Count        uint64
	Sum          float64
	Bounds       []float64
	BucketCounts []uint64
	Min, Max     float64
	Exemplars    []Exemplar
}

// Exemplar ties a measurement to the span that was active when it was made.
type Exemplar struct {
	Time       time.Time
	Value      float64
	IsInt      bool
	IntValue   int64
	TraceID    TraceID
	SpanID     SpanID
	Attributes []KeyValue
}

// ExemplarFilter selects which measurements may become exemplars.
type ExemplarFilter int

// Exemplar filters, as named by OTEL_METRICS_EXEMPLAR_FILTER.
const (
	ExemplarTraceBased ExemplarFilter = iota
	ExemplarAlwaysOn
	ExemplarAlwaysOff
)

// DefaultCardinalityLimit caps the attribute sets per instrument; further
// sets aggregate into one point marked otel.metric.overflow=true.
const DefaultCardinalityLimit = 2000

type instrumentKind int

const (
	kindCounter instrumentKind = iota
	kindUpDownCounter
	kindHistogram
	kindObservableGauge
	kindObservableCounter
	kindObservableUpDownCounter
)

func (k instrumentKind) String() string {
	return [...]string{"counter", "updowncounter", "histogram", "observable gauge", "observable counter", "observable updowncounter"}[k]
}

func (k instrumentKind) observable() bool { return k >= kindObservableGauge }

// MeterProvider creates meters and collects their instruments.
type MeterProvider struct {
	resource    *Resource
	start       time.Time
	temporality Temporality
	cardinality int
	exemplars   ExemplarFilter

	mu     sync.Mutex
	meters []*Meter
	byName map[Scope]*Meter

	collectMu   sync.Mutex
	lastCollect time.Time

	reader   *periodicReader
	shutdown atomic.Bool
}

// MeterProviderOption configures NewMeterProvider.
type MeterProviderOption func(*MeterProvider)

// WithMetricResource sets the resource attached to every collection.
func WithMetricResource(r *Resource) MeterProviderOption {
	return func(mp *MeterProvider) { mp.resource = r }
}

// WithTemporality sets cumulative (default) or delta temporality for sums
// and histograms.
func WithTemporality(t Temporality) MeterProviderOption {
	return func(mp *MeterProvider) { mp.temporality = t }
}

// WithCardinalityLimit caps attribute sets per instrument.
func WithCardinalityLimit(n int) MeterProviderOption {
	return func(mp *MeterProvider) {
		if n > 1 {
			mp.cardinality = n
		}
	}
}

// WithExemplarFilter selects exemplar sampling (default trace-based).
func WithExemplarFilter(f ExemplarFilter) MeterProviderOption {
	return func(mp *MeterProvider) { mp.exemplars = f }
}

// ReaderOptions tune periodic metric export. Zero fields take the
// OpenTelemetry defaults (60s interval, 30s timeout).
type ReaderOptions struct {
	Interval time.Duration
	Timeout  time.Duration
}

// WithPeriodicReader exports a collection to exp every interval and once
// more on Shutdown.
func WithPeriodicReader(exp MetricExporter, o ReaderOptions) MeterProviderOption {
	return func(mp *MeterProvider) {
		if o.Interval <= 0 {
			o.Interval = 60 * time.Second
		}
		if o.Timeout <= 0 {
			o.Timeout = 30 * time.Second
		}
		mp.reader = &periodicReader{exp: exp, o: o, stop: make(chan struct{}), done: make(chan struct{})}
	}
}

// TemporalityFromEnv reads OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE
// (cumulative, delta or lowmemory; the last two select delta here).
func TemporalityFromEnv() Temporality {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE"))) {
	case "delta", "lowmemory":
		return DeltaTemporality
	}
	return CumulativeTemporality
}

// ExemplarFilterFromEnv reads OTEL_METRICS_EXEMPLAR_FILTER.
func ExemplarFilterFromEnv() ExemplarFilter {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_METRICS_EXEMPLAR_FILTER"))) {
	case "always_on":
		return ExemplarAlwaysOn
	case "always_off":
		return ExemplarAlwaysOff
	}
	return ExemplarTraceBased
}

// NewMeterProvider builds a provider and starts its periodic reader, if
// one was configured.
func NewMeterProvider(opts ...MeterProviderOption) *MeterProvider {
	mp := &MeterProvider{
		resource:    defaultResource(),
		start:       time.Now(),
		temporality: CumulativeTemporality,
		cardinality: DefaultCardinalityLimit,
		byName:      map[Scope]*Meter{},
	}
	for _, o := range opts {
		o(mp)
	}
	if mp.temporality != DeltaTemporality {
		mp.temporality = CumulativeTemporality
	}
	mp.lastCollect = mp.start
	if mp.reader != nil {
		go mp.reader.run(mp)
	}
	return mp
}

// Meter returns the meter for an instrumentation scope.
func (mp *MeterProvider) Meter(name string, opts ...TracerOption) *Meter {
	sc := Scope{Name: name}
	for _, o := range opts {
		o(&sc)
	}
	mp.mu.Lock()
	defer mp.mu.Unlock()
	if m, ok := mp.byName[sc]; ok {
		return m
	}
	m := &Meter{provider: mp, scope: sc, byName: map[string]*instrument{}}
	mp.byName[sc] = m
	mp.meters = append(mp.meters, m)
	return m
}

// Collect gathers every instrument's current data. With delta temporality
// each call reports the change since the previous one.
func (mp *MeterProvider) Collect(ctx context.Context) *ResourceMetrics {
	mp.collectMu.Lock()
	defer mp.collectMu.Unlock()
	return mp.collectLocked(ctx)
}

func (mp *MeterProvider) collectLocked(ctx context.Context) *ResourceMetrics {
	now := time.Now()
	start := mp.start
	if mp.temporality == DeltaTemporality {
		start = mp.lastCollect
	}
	mp.lastCollect = now
	mp.mu.Lock()
	meters := append([]*Meter(nil), mp.meters...)
	mp.mu.Unlock()
	rm := &ResourceMetrics{Resource: mp.resource}
	for _, m := range meters {
		sm := ScopeMetrics{Scope: m.scope}
		for _, inst := range m.instruments() {
			if metric, ok := inst.collect(ctx, start, now); ok {
				sm.Metrics = append(sm.Metrics, metric)
			}
		}
		if len(sm.Metrics) > 0 {
			rm.ScopeMetrics = append(rm.ScopeMetrics, sm)
		}
	}
	return rm
}

// ForceFlush collects and exports now when a reader is configured.
func (mp *MeterProvider) ForceFlush(ctx context.Context) error {
	if mp.reader == nil || mp.shutdown.Load() {
		return nil
	}
	return mp.reader.export(ctx, mp)
}

// Shutdown stops the reader after a final export and shuts the exporter
// down. Only the first call does work.
func (mp *MeterProvider) Shutdown(ctx context.Context) error {
	if !mp.shutdown.CompareAndSwap(false, true) || mp.reader == nil {
		return nil
	}
	r := mp.reader
	close(r.stop)
	select {
	case <-r.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	err := r.export(ctx, mp)
	return errors.Join(err, r.exp.Shutdown(ctx))
}

// MetricExporter sends collected metrics to a backend.
type MetricExporter interface {
	ExportMetrics(ctx context.Context, rm *ResourceMetrics) error
	Shutdown(ctx context.Context) error
}

type periodicReader struct {
	exp  MetricExporter
	o    ReaderOptions
	stop chan struct{}
	done chan struct{}
}

func (r *periodicReader) run(mp *MeterProvider) {
	defer close(r.done)
	t := time.NewTicker(r.o.Interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			_ = r.export(context.Background(), mp)
		case <-r.stop:
			return
		}
	}
}

func (r *periodicReader) export(parent context.Context, mp *MeterProvider) error {
	ctx, cancel := context.WithTimeout(parent, r.o.Timeout)
	defer cancel()
	// Hold collectMu through the export so delta collections reach the
	// exporter in order.
	mp.collectMu.Lock()
	defer mp.collectMu.Unlock()
	rm := mp.collectLocked(ctx)
	if len(rm.ScopeMetrics) == 0 {
		return nil
	}
	if err := r.exp.ExportMetrics(ctx, rm); err != nil {
		reportError(err)
		return err
	}
	return nil
}

// Meter creates instruments for one scope.
type Meter struct {
	provider *MeterProvider
	scope    Scope

	mu     sync.Mutex
	insts  []*instrument
	byName map[string]*instrument
}

func (m *Meter) instruments() []*instrument {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*instrument(nil), m.insts...)
}

// InstrumentOption configures an instrument.
type InstrumentOption func(*instrument)

// WithDescription sets the instrument description.
func WithDescription(d string) InstrumentOption { return func(i *instrument) { i.desc = d } }

// WithUnit sets the UCUM unit, for example "s", "By" or "{token}".
func WithUnit(u string) InstrumentOption { return func(i *instrument) { i.unit = u } }

// WithExplicitBuckets sets histogram bucket boundaries; they are sorted and
// de-duplicated.
func WithExplicitBuckets(bounds ...float64) InstrumentOption {
	return func(i *instrument) {
		b := append([]float64(nil), bounds...)
		sort.Float64s(b)
		out := b[:0]
		for j, v := range b {
			if math.IsNaN(v) || math.IsInf(v, 0) || (j > 0 && v == b[j-1]) {
				continue
			}
			out = append(out, v)
		}
		i.bounds = out
	}
}

// DefaultHistogramBuckets are the OpenTelemetry default boundaries.
var DefaultHistogramBuckets = []float64{0, 5, 10, 25, 50, 75, 100, 250, 500, 750, 1000, 2500, 5000, 7500, 10000}

var instrumentNameRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_./-]{0,254}$`)

func (m *Meter) register(name string, kind instrumentKind, isInt bool, opts []InstrumentOption) (*instrument, error) {
	if !instrumentNameRE.MatchString(name) {
		return nil, fmt.Errorf("telemetry: invalid instrument name %q", name)
	}
	inst := &instrument{
		name: name, kind: kind, isInt: isInt, meter: m,
		points: map[string]*point{}, prevObs: map[string]float64{},
	}
	for _, o := range opts {
		o(inst)
	}
	if kind == kindHistogram && inst.bounds == nil {
		inst.bounds = DefaultHistogramBuckets
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := strings.ToLower(name)
	if prev, ok := m.byName[key]; ok {
		if prev.kind != kind || prev.isInt != isInt || prev.unit != inst.unit {
			return nil, fmt.Errorf("telemetry: instrument %q already registered as %s (unit %q)", name, prev.kind, prev.unit)
		}
		if kind.observable() {
			// Each registration adds its callback to the shared instrument.
			prev.callbacks = append(prev.callbacks, inst.callbacks...)
		}
		return prev, nil
	}
	m.byName[key] = inst
	m.insts = append(m.insts, inst)
	return inst, nil
}

// Int64Counter is a monotonic integer sum.
type Int64Counter struct{ inst *instrument }

// Float64Counter is a monotonic floating-point sum.
type Float64Counter struct{ inst *instrument }

// Int64UpDownCounter is a non-monotonic integer sum.
type Int64UpDownCounter struct{ inst *instrument }

// Float64Histogram records a distribution.
type Float64Histogram struct{ inst *instrument }

// Int64Histogram records a distribution of integers.
type Int64Histogram struct{ inst *instrument }

// Int64Counter creates or returns a counter.
func (m *Meter) Int64Counter(name string, opts ...InstrumentOption) (*Int64Counter, error) {
	i, err := m.register(name, kindCounter, true, opts)
	if err != nil {
		return nil, err
	}
	return &Int64Counter{i}, nil
}

// Float64Counter creates or returns a counter.
func (m *Meter) Float64Counter(name string, opts ...InstrumentOption) (*Float64Counter, error) {
	i, err := m.register(name, kindCounter, false, opts)
	if err != nil {
		return nil, err
	}
	return &Float64Counter{i}, nil
}

// Int64UpDownCounter creates or returns an up-down counter.
func (m *Meter) Int64UpDownCounter(name string, opts ...InstrumentOption) (*Int64UpDownCounter, error) {
	i, err := m.register(name, kindUpDownCounter, true, opts)
	if err != nil {
		return nil, err
	}
	return &Int64UpDownCounter{i}, nil
}

// Float64Histogram creates or returns a histogram.
func (m *Meter) Float64Histogram(name string, opts ...InstrumentOption) (*Float64Histogram, error) {
	i, err := m.register(name, kindHistogram, false, opts)
	if err != nil {
		return nil, err
	}
	return &Float64Histogram{i}, nil
}

// Int64Histogram creates or returns a histogram of integers.
func (m *Meter) Int64Histogram(name string, opts ...InstrumentOption) (*Int64Histogram, error) {
	i, err := m.register(name, kindHistogram, true, opts)
	if err != nil {
		return nil, err
	}
	return &Int64Histogram{i}, nil
}

// Observe reports one value of an observable instrument.
type Observe func(value float64, attrs ...KeyValue)

// Callback is invoked on every collection of an observable instrument.
type Callback func(ctx context.Context, observe Observe)

func withCallback(cb Callback) InstrumentOption {
	return func(i *instrument) {
		if cb != nil {
			i.callbacks = append(i.callbacks, cb)
		}
	}
}

// Float64ObservableGauge registers a gauge read by cb at collection time.
func (m *Meter) Float64ObservableGauge(name string, cb Callback, opts ...InstrumentOption) error {
	_, err := m.register(name, kindObservableGauge, false, append(opts, withCallback(cb)))
	return err
}

// Int64ObservableGauge registers an integer gauge read by cb at collection
// time; observed values are truncated to integers.
func (m *Meter) Int64ObservableGauge(name string, cb Callback, opts ...InstrumentOption) error {
	_, err := m.register(name, kindObservableGauge, true, append(opts, withCallback(cb)))
	return err
}

// Int64ObservableCounter registers a monotonic counter whose cumulative
// value cb reports at collection time.
func (m *Meter) Int64ObservableCounter(name string, cb Callback, opts ...InstrumentOption) error {
	_, err := m.register(name, kindObservableCounter, true, append(opts, withCallback(cb)))
	return err
}

// Int64ObservableUpDownCounter registers a non-monotonic sum whose current
// value cb reports at collection time.
func (m *Meter) Int64ObservableUpDownCounter(name string, cb Callback, opts ...InstrumentOption) error {
	_, err := m.register(name, kindObservableUpDownCounter, true, append(opts, withCallback(cb)))
	return err
}

// Add increments the counter; negative values are ignored.
func (c *Int64Counter) Add(ctx context.Context, n int64, attrs ...KeyValue) {
	if c == nil || n < 0 {
		return
	}
	c.inst.recordSum(ctx, float64(n), n, attrs)
}

// Add increments the counter; negative, NaN and infinite values are ignored.
func (c *Float64Counter) Add(ctx context.Context, v float64, attrs ...KeyValue) {
	if c == nil || v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return
	}
	c.inst.recordSum(ctx, v, 0, attrs)
}

// Add changes the sum by n.
func (c *Int64UpDownCounter) Add(ctx context.Context, n int64, attrs ...KeyValue) {
	if c == nil {
		return
	}
	c.inst.recordSum(ctx, float64(n), n, attrs)
}

// Record adds a measurement; NaN and infinite values are ignored.
func (h *Float64Histogram) Record(ctx context.Context, v float64, attrs ...KeyValue) {
	if h == nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return
	}
	h.inst.recordHist(ctx, v, attrs)
}

// Record adds a measurement.
func (h *Int64Histogram) Record(ctx context.Context, v int64, attrs ...KeyValue) {
	if h == nil {
		return
	}
	h.inst.recordHist(ctx, float64(v), attrs)
}

type instrument struct {
	name, desc, unit string
	kind             instrumentKind
	isInt            bool
	bounds           []float64
	callbacks        []Callback
	meter            *Meter

	mu       sync.Mutex
	points   map[string]*point
	prevObs  map[string]float64
	overflow attrSet
}

type point struct {
	attrs    attrSet
	ival     int64
	fval     float64
	count    uint64
	sum      float64
	buckets  []uint64
	min, max float64
	exemplar *Exemplar
}

var overflowSet = newAttrSet([]KeyValue{Bool("otel.metric.overflow", true)})

// pointFor returns the point for attrs, creating it within the cardinality
// limit or diverting to the overflow point beyond it. Caller holds mu.
func (i *instrument) pointFor(attrs []KeyValue) *point {
	set := newAttrSet(attrs)
	if p, ok := i.points[set.key]; ok {
		return p
	}
	if len(i.points) >= i.meter.provider.cardinality-1 {
		set = overflowSet
		if p, ok := i.points[set.key]; ok {
			return p
		}
	}
	p := &point{attrs: set}
	if i.kind == kindHistogram {
		p.buckets = make([]uint64, len(i.bounds)+1)
		p.min, p.max = math.Inf(1), math.Inf(-1)
	}
	i.points[set.key] = p
	return p
}

func (i *instrument) exemplarFor(ctx context.Context, v float64, n int64) *Exemplar {
	filter := i.meter.provider.exemplars
	if filter == ExemplarAlwaysOff {
		return nil
	}
	sc := SpanContextFromContext(ctx)
	if filter == ExemplarTraceBased && !sc.IsSampled() {
		return nil
	}
	e := &Exemplar{Time: time.Now(), Value: v, IsInt: i.isInt, IntValue: n}
	if sc.IsValid() {
		e.TraceID, e.SpanID = sc.TraceID, sc.SpanID
	}
	return e
}

func (i *instrument) recordSum(ctx context.Context, v float64, n int64, attrs []KeyValue) {
	ex := i.exemplarFor(ctx, v, n)
	i.mu.Lock()
	defer i.mu.Unlock()
	p := i.pointFor(attrs)
	p.ival += n
	p.fval += v
	if ex != nil {
		p.exemplar = ex
	}
}

func (i *instrument) recordHist(ctx context.Context, v float64, attrs []KeyValue) {
	ex := i.exemplarFor(ctx, v, int64(v))
	i.mu.Lock()
	defer i.mu.Unlock()
	p := i.pointFor(attrs)
	idx := sort.SearchFloat64s(i.bounds, v) // first bound >= v: buckets are upper-inclusive
	p.buckets[idx]++
	p.count++
	p.sum += v
	p.min = math.Min(p.min, v)
	p.max = math.Max(p.max, v)
	if ex != nil {
		p.exemplar = ex
	}
}

func (i *instrument) collect(ctx context.Context, start, now time.Time) (Metric, bool) {
	m := Metric{Name: i.name, Description: i.desc, Unit: i.unit}
	delta := i.meter.provider.temporality == DeltaTemporality
	if i.kind.observable() {
		return i.collectObservable(ctx, m, start, now, delta)
	}
	i.mu.Lock()
	pts := i.sortedPoints()
	if delta {
		i.points = map[string]*point{}
	}
	i.mu.Unlock()
	if len(pts) == 0 {
		return m, false
	}
	temporality := CumulativeTemporality
	if delta {
		temporality = DeltaTemporality
	}
	switch i.kind {
	case kindHistogram:
		h := Histogram{Temporality: temporality}
		for _, p := range pts {
			dp := HistogramDataPoint{
				Attributes: p.attrs.kvs, Start: start, Time: now,
				Count: p.count, Sum: p.sum, Bounds: append([]float64(nil), i.bounds...),
				BucketCounts: append([]uint64(nil), p.buckets...), Min: p.min, Max: p.max,
			}
			if p.exemplar != nil {
				dp.Exemplars = []Exemplar{*p.exemplar}
			}
			h.DataPoints = append(h.DataPoints, dp)
		}
		m.Data = h
	default:
		s := Sum{Temporality: temporality, IsMonotonic: i.kind == kindCounter}
		for _, p := range pts {
			dp := NumberDataPoint{Attributes: p.attrs.kvs, Start: start, Time: now, IsInt: i.isInt, Int: p.ival, Float: p.fval}
			if p.exemplar != nil {
				dp.Exemplars = []Exemplar{*p.exemplar}
			}
			s.DataPoints = append(s.DataPoints, dp)
		}
		m.Data = s
	}
	return m, true
}

// sortedPoints returns copies of the points in attribute-key order so
// collections are deterministic. Caller holds mu.
func (i *instrument) sortedPoints() []point {
	keys := make([]string, 0, len(i.points))
	for k := range i.points {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]point, 0, len(keys))
	for _, k := range keys {
		p := *i.points[k]
		p.buckets = append([]uint64(nil), p.buckets...)
		out = append(out, p)
	}
	return out
}

func (i *instrument) collectObservable(ctx context.Context, m Metric, start, now time.Time, delta bool) (Metric, bool) {
	i.mu.Lock()
	cbs := append([]Callback(nil), i.callbacks...)
	i.mu.Unlock()
	observed := map[string]*point{}
	var order []string
	for _, cb := range cbs {
		cb(ctx, func(v float64, attrs ...KeyValue) {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return
			}
			set := newAttrSet(attrs)
			diverted := false
			if _, ok := observed[set.key]; !ok && len(observed) >= i.meter.provider.cardinality-1 {
				set, diverted = overflowSet, true
			}
			p, ok := observed[set.key]
			if !ok {
				p = &point{attrs: set}
				observed[set.key] = p
				order = append(order, set.key)
			}
			if diverted {
				p.fval += v // the overflow point aggregates every diverted set
			} else {
				p.fval = v
			}
		})
	}
	if len(order) == 0 {
		return m, false
	}
	sort.Strings(order)
	var dps []NumberDataPoint
	i.mu.Lock()
	for _, k := range order {
		p := observed[k]
		v := p.fval
		if i.kind == kindObservableCounter && delta {
			prev := i.prevObs[k]
			i.prevObs[k] = v
			v -= prev
			if v < 0 { // the source restarted; report its new total
				v = p.fval
			}
		}
		dp := NumberDataPoint{Attributes: p.attrs.kvs, Time: now, IsInt: i.isInt}
		switch {
		case i.kind == kindObservableCounter && delta:
			dp.Start = start
		case i.kind != kindObservableGauge:
			// Observed sums other than delta counters are cumulative
			// since the provider started.
			dp.Start = i.meter.provider.start
		}
		if i.isInt {
			dp.Int = int64(v)
		} else {
			dp.Float = v
		}
		dps = append(dps, dp)
	}
	i.mu.Unlock()
	switch i.kind {
	case kindObservableGauge:
		m.Data = Gauge{DataPoints: dps}
	default:
		t := CumulativeTemporality
		if delta && i.kind == kindObservableCounter {
			t = DeltaTemporality
		}
		m.Data = Sum{Temporality: t, IsMonotonic: i.kind == kindObservableCounter, DataPoints: dps}
	}
	return m, true
}
