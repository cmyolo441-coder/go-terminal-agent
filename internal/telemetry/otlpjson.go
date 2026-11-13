package telemetry

import (
	"encoding/json"
	"math"
	"strconv"
	"time"
)

// This file maps the SDK's data onto the OTLP/JSON encoding defined by the
// OTLP specification (protobuf JSON mapping with OTLP's deviations): field
// names are lowerCamelCase, trace and span ids are hex strings rather than
// base64, enums are integers, and 64-bit integers (timestamps, counts, int
// values) are decimal strings.

type otlpTracesRequest struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

type otlpMetricsRequest struct {
	ResourceMetrics []otlpResourceMetrics `json:"resourceMetrics"`
}

type otlpResource struct {
	Attributes []otlpKeyValue `json:"attributes"`
}

type otlpScope struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type otlpResourceSpans struct {
	Resource   otlpResource     `json:"resource"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
}

type otlpScopeSpans struct {
	Scope     otlpScope  `json:"scope"`
	Spans     []otlpSpan `json:"spans"`
	SchemaURL string     `json:"schemaUrl,omitempty"`
}

type otlpSpan struct {
	TraceID                string         `json:"traceId"`
	SpanID                 string         `json:"spanId"`
	TraceState             string         `json:"traceState,omitempty"`
	ParentSpanID           string         `json:"parentSpanId,omitempty"`
	Flags                  uint32         `json:"flags,omitempty"`
	Name                   string         `json:"name"`
	Kind                   int            `json:"kind"`
	StartTimeUnixNano      string         `json:"startTimeUnixNano"`
	EndTimeUnixNano        string         `json:"endTimeUnixNano"`
	Attributes             []otlpKeyValue `json:"attributes,omitempty"`
	DroppedAttributesCount uint32         `json:"droppedAttributesCount,omitempty"`
	Events                 []otlpEvent    `json:"events,omitempty"`
	DroppedEventsCount     uint32         `json:"droppedEventsCount,omitempty"`
	Links                  []otlpLink     `json:"links,omitempty"`
	DroppedLinksCount      uint32         `json:"droppedLinksCount,omitempty"`
	Status                 otlpStatus     `json:"status"`
}

type otlpEvent struct {
	TimeUnixNano           string         `json:"timeUnixNano"`
	Name                   string         `json:"name"`
	Attributes             []otlpKeyValue `json:"attributes,omitempty"`
	DroppedAttributesCount uint32         `json:"droppedAttributesCount,omitempty"`
}

type otlpLink struct {
	TraceID                string         `json:"traceId"`
	SpanID                 string         `json:"spanId"`
	TraceState             string         `json:"traceState,omitempty"`
	Attributes             []otlpKeyValue `json:"attributes,omitempty"`
	DroppedAttributesCount uint32         `json:"droppedAttributesCount,omitempty"`
	Flags                  uint32         `json:"flags,omitempty"`
}

type otlpStatus struct {
	Message string `json:"message,omitempty"`
	Code    int    `json:"code,omitempty"`
}

type otlpKeyValue struct {
	Key   string       `json:"key"`
	Value otlpAnyValue `json:"value"`
}

type otlpAnyValue struct {
	StringValue *string         `json:"stringValue,omitempty"`
	BoolValue   *bool           `json:"boolValue,omitempty"`
	IntValue    *string         `json:"intValue,omitempty"`
	DoubleValue *otlpDouble     `json:"doubleValue,omitempty"`
	ArrayValue  *otlpArrayValue `json:"arrayValue,omitempty"`
}

type otlpArrayValue struct {
	Values []otlpAnyValue `json:"values"`
}

// otlpDouble encodes a float the way the protobuf JSON mapping requires,
// including the "NaN", "Infinity" and "-Infinity" string forms that plain
// encoding/json rejects.
type otlpDouble float64

func (d otlpDouble) MarshalJSON() ([]byte, error) {
	f := float64(d)
	switch {
	case math.IsNaN(f):
		return []byte(`"NaN"`), nil
	case math.IsInf(f, 1):
		return []byte(`"Infinity"`), nil
	case math.IsInf(f, -1):
		return []byte(`"-Infinity"`), nil
	}
	return strconv.AppendFloat(nil, f, 'g', -1, 64), nil
}

// OTLP span flags: the low byte carries W3C trace flags; bit 8 says whether
// the remote bit is known and bit 9 whether the parent (or link) is remote.
const (
	spanFlagsHasIsRemote = 0x100
	spanFlagsIsRemote    = 0x200
)

func nanos(t time.Time) string {
	if t.IsZero() {
		return "0"
	}
	return strconv.FormatInt(t.UnixNano(), 10)
}

func ptr[T any](v T) *T { return &v }

func encodeValue(v Value) otlpAnyValue {
	switch v.typ {
	case TypeString:
		return otlpAnyValue{StringValue: ptr(v.s)}
	case TypeBool:
		return otlpAnyValue{BoolValue: ptr(v.b)}
	case TypeInt64:
		return otlpAnyValue{IntValue: ptr(strconv.FormatInt(v.n, 10))}
	case TypeFloat64:
		return otlpAnyValue{DoubleValue: ptr(otlpDouble(v.f))}
	case TypeStringSlice:
		a := &otlpArrayValue{Values: make([]otlpAnyValue, len(v.ss))}
		for i, s := range v.ss {
			a.Values[i] = otlpAnyValue{StringValue: ptr(s)}
		}
		return otlpAnyValue{ArrayValue: a}
	case TypeBoolSlice:
		a := &otlpArrayValue{Values: make([]otlpAnyValue, len(v.bs))}
		for i, b := range v.bs {
			a.Values[i] = otlpAnyValue{BoolValue: ptr(b)}
		}
		return otlpAnyValue{ArrayValue: a}
	case TypeInt64Slice:
		a := &otlpArrayValue{Values: make([]otlpAnyValue, len(v.ns))}
		for i, n := range v.ns {
			a.Values[i] = otlpAnyValue{IntValue: ptr(strconv.FormatInt(n, 10))}
		}
		return otlpAnyValue{ArrayValue: a}
	case TypeFloat64Slice:
		a := &otlpArrayValue{Values: make([]otlpAnyValue, len(v.fs))}
		for i, f := range v.fs {
			a.Values[i] = otlpAnyValue{DoubleValue: ptr(otlpDouble(f))}
		}
		return otlpAnyValue{ArrayValue: a}
	}
	return otlpAnyValue{}
}

func encodeAttrs(kvs []KeyValue) []otlpKeyValue {
	if len(kvs) == 0 {
		return nil
	}
	out := make([]otlpKeyValue, 0, len(kvs))
	for _, kv := range kvs {
		if kv.Valid() {
			out = append(out, otlpKeyValue{Key: kv.Key, Value: encodeValue(kv.Value)})
		}
	}
	return out
}

func encodeResource(r *Resource) otlpResource {
	attrs := encodeAttrs(r.Attributes())
	if attrs == nil {
		attrs = []otlpKeyValue{}
	}
	return otlpResource{Attributes: attrs}
}

func encodeSpan(s SpanData) otlpSpan {
	out := otlpSpan{
		TraceID:                s.SpanContext.TraceID.String(),
		SpanID:                 s.SpanContext.SpanID.String(),
		TraceState:             s.SpanContext.TraceState,
		Flags:                  uint32(s.SpanContext.TraceFlags) | spanFlagsHasIsRemote,
		Name:                   s.Name,
		Kind:                   int(s.Kind),
		StartTimeUnixNano:      nanos(s.Start),
		EndTimeUnixNano:        nanos(s.End),
		Attributes:             encodeAttrs(s.Attributes),
		DroppedAttributesCount: uint32(s.DroppedAttributes),
		DroppedEventsCount:     uint32(s.DroppedEvents),
		DroppedLinksCount:      uint32(s.DroppedLinks),
		Status:                 otlpStatus{Code: int(s.Status.Code), Message: s.Status.Description},
	}
	if s.Parent.IsValid() {
		out.ParentSpanID = s.Parent.SpanID.String()
		if s.Parent.Remote {
			out.Flags |= spanFlagsIsRemote
		}
	}
	for _, e := range s.Events {
		out.Events = append(out.Events, otlpEvent{
			TimeUnixNano: nanos(e.Time), Name: e.Name,
			Attributes: encodeAttrs(e.Attributes), DroppedAttributesCount: uint32(e.DroppedAttributes),
		})
	}
	for _, l := range s.Links {
		flags := uint32(l.SpanContext.TraceFlags) | spanFlagsHasIsRemote
		if l.SpanContext.Remote {
			flags |= spanFlagsIsRemote
		}
		out.Links = append(out.Links, otlpLink{
			TraceID: l.SpanContext.TraceID.String(), SpanID: l.SpanContext.SpanID.String(),
			TraceState: l.SpanContext.TraceState, Attributes: encodeAttrs(l.Attributes),
			DroppedAttributesCount: uint32(l.DroppedAttributes), Flags: flags,
		})
	}
	return out
}

// encodeTraces groups spans by resource and scope, preserving first-seen
// order, and returns the OTLP/JSON ExportTraceServiceRequest body.
func encodeTraces(spans []SpanData) ([]byte, error) {
	req := otlpTracesRequest{ResourceSpans: []otlpResourceSpans{}}
	// Resources are grouped by content, not identity, so equal resources
	// from different providers share one resourceSpans entry.
	resIdx := map[string]int{}
	type scopeKey struct {
		r string
		s Scope
	}
	scopeIdx := map[scopeKey]int{}
	for _, s := range spans {
		rk := ""
		if s.Resource != nil {
			rk = s.Resource.set.key
		}
		ri, ok := resIdx[rk]
		if !ok {
			ri = len(req.ResourceSpans)
			resIdx[rk] = ri
			req.ResourceSpans = append(req.ResourceSpans, otlpResourceSpans{Resource: encodeResource(s.Resource)})
		}
		rs := &req.ResourceSpans[ri]
		k := scopeKey{rk, s.Scope}
		si, ok := scopeIdx[k]
		if !ok {
			si = len(rs.ScopeSpans)
			scopeIdx[k] = si
			rs.ScopeSpans = append(rs.ScopeSpans, otlpScopeSpans{
				Scope: otlpScope{Name: s.Scope.Name, Version: s.Scope.Version}, SchemaURL: s.Scope.SchemaURL,
			})
		}
		rs.ScopeSpans[si].Spans = append(rs.ScopeSpans[si].Spans, encodeSpan(s))
	}
	return json.Marshal(req)
}

type otlpResourceMetrics struct {
	Resource     otlpResource       `json:"resource"`
	ScopeMetrics []otlpScopeMetrics `json:"scopeMetrics"`
}

type otlpScopeMetrics struct {
	Scope     otlpScope    `json:"scope"`
	Metrics   []otlpMetric `json:"metrics"`
	SchemaURL string       `json:"schemaUrl,omitempty"`
}

type otlpMetric struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Unit        string         `json:"unit,omitempty"`
	Sum         *otlpSum       `json:"sum,omitempty"`
	Gauge       *otlpGauge     `json:"gauge,omitempty"`
	Histogram   *otlpHistogram `json:"histogram,omitempty"`
}

type otlpSum struct {
	DataPoints             []otlpNumberPoint `json:"dataPoints"`
	AggregationTemporality int               `json:"aggregationTemporality"`
	IsMonotonic            bool              `json:"isMonotonic,omitempty"`
}

type otlpGauge struct {
	DataPoints []otlpNumberPoint `json:"dataPoints"`
}

type otlpHistogram struct {
	DataPoints             []otlpHistogramPoint `json:"dataPoints"`
	AggregationTemporality int                  `json:"aggregationTemporality"`
}

type otlpNumberPoint struct {
	Attributes        []otlpKeyValue `json:"attributes,omitempty"`
	StartTimeUnixNano string         `json:"startTimeUnixNano,omitempty"`
	TimeUnixNano      string         `json:"timeUnixNano"`
	AsDouble          *otlpDouble    `json:"asDouble,omitempty"`
	AsInt             *string        `json:"asInt,omitempty"`
	Exemplars         []otlpExemplar `json:"exemplars,omitempty"`
}

type otlpHistogramPoint struct {
	Attributes        []otlpKeyValue `json:"attributes,omitempty"`
	StartTimeUnixNano string         `json:"startTimeUnixNano,omitempty"`
	TimeUnixNano      string         `json:"timeUnixNano"`
	Count             string         `json:"count"`
	Sum               *otlpDouble    `json:"sum,omitempty"`
	BucketCounts      []string       `json:"bucketCounts"`
	ExplicitBounds    []otlpDouble   `json:"explicitBounds"`
	Exemplars         []otlpExemplar `json:"exemplars,omitempty"`
	Min               *otlpDouble    `json:"min,omitempty"`
	Max               *otlpDouble    `json:"max,omitempty"`
}

type otlpExemplar struct {
	FilteredAttributes []otlpKeyValue `json:"filteredAttributes,omitempty"`
	TimeUnixNano       string         `json:"timeUnixNano"`
	AsDouble           *otlpDouble    `json:"asDouble,omitempty"`
	AsInt              *string        `json:"asInt,omitempty"`
	SpanID             string         `json:"spanId,omitempty"`
	TraceID            string         `json:"traceId,omitempty"`
}

func encodeExemplars(es []Exemplar) []otlpExemplar {
	var out []otlpExemplar
	for _, e := range es {
		oe := otlpExemplar{FilteredAttributes: encodeAttrs(e.Attributes), TimeUnixNano: nanos(e.Time)}
		if e.IsInt {
			oe.AsInt = ptr(strconv.FormatInt(e.IntValue, 10))
		} else {
			oe.AsDouble = ptr(otlpDouble(e.Value))
		}
		if e.TraceID.IsValid() {
			oe.TraceID = e.TraceID.String()
			oe.SpanID = e.SpanID.String()
		}
		out = append(out, oe)
	}
	return out
}

func optNanos(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return nanos(t)
}

func encodeNumberPoints(dps []NumberDataPoint) []otlpNumberPoint {
	out := make([]otlpNumberPoint, 0, len(dps))
	for _, dp := range dps {
		p := otlpNumberPoint{
			Attributes:        encodeAttrs(dp.Attributes),
			StartTimeUnixNano: optNanos(dp.Start),
			TimeUnixNano:      nanos(dp.Time),
			Exemplars:         encodeExemplars(dp.Exemplars),
		}
		if dp.IsInt {
			p.AsInt = ptr(strconv.FormatInt(dp.Int, 10))
		} else {
			p.AsDouble = ptr(otlpDouble(dp.Float))
		}
		out = append(out, p)
	}
	return out
}

func encodeMetric(m Metric) otlpMetric {
	om := otlpMetric{Name: m.Name, Description: m.Description, Unit: m.Unit}
	switch d := m.Data.(type) {
	case Sum:
		om.Sum = &otlpSum{
			DataPoints: encodeNumberPoints(d.DataPoints), AggregationTemporality: int(d.Temporality), IsMonotonic: d.IsMonotonic,
		}
	case Gauge:
		om.Gauge = &otlpGauge{DataPoints: encodeNumberPoints(d.DataPoints)}
	case Histogram:
		h := &otlpHistogram{AggregationTemporality: int(d.Temporality), DataPoints: []otlpHistogramPoint{}}
		for _, dp := range d.DataPoints {
			hp := otlpHistogramPoint{
				Attributes:        encodeAttrs(dp.Attributes),
				StartTimeUnixNano: optNanos(dp.Start),
				TimeUnixNano:      nanos(dp.Time),
				Count:             strconv.FormatUint(dp.Count, 10),
				Sum:               ptr(otlpDouble(dp.Sum)),
				BucketCounts:      make([]string, len(dp.BucketCounts)),
				ExplicitBounds:    make([]otlpDouble, len(dp.Bounds)),
				Exemplars:         encodeExemplars(dp.Exemplars),
			}
			for i, c := range dp.BucketCounts {
				hp.BucketCounts[i] = strconv.FormatUint(c, 10)
			}
			for i, b := range dp.Bounds {
				hp.ExplicitBounds[i] = otlpDouble(b)
			}
			if dp.Count > 0 {
				hp.Min, hp.Max = ptr(otlpDouble(dp.Min)), ptr(otlpDouble(dp.Max))
			}
			h.DataPoints = append(h.DataPoints, hp)
		}
		om.Histogram = h
	}
	return om
}

// encodeMetrics returns the OTLP/JSON ExportMetricsServiceRequest body.
func encodeMetrics(rm *ResourceMetrics) ([]byte, error) {
	out := otlpResourceMetrics{Resource: encodeResource(rm.Resource), ScopeMetrics: []otlpScopeMetrics{}}
	for _, sm := range rm.ScopeMetrics {
		osm := otlpScopeMetrics{
			Scope: otlpScope{Name: sm.Scope.Name, Version: sm.Scope.Version}, SchemaURL: sm.Scope.SchemaURL,
			Metrics: make([]otlpMetric, 0, len(sm.Metrics)),
		}
		for _, m := range sm.Metrics {
			osm.Metrics = append(osm.Metrics, encodeMetric(m))
		}
		out.ScopeMetrics = append(out.ScopeMetrics, osm)
	}
	return json.Marshal(otlpMetricsRequest{ResourceMetrics: []otlpResourceMetrics{out}})
}

// partialSuccess is the optional body of a successful OTLP export response.
type partialSuccess struct {
	PartialSuccess *struct {
		RejectedSpans      json.Number `json:"rejectedSpans"`
		RejectedDataPoints json.Number `json:"rejectedDataPoints"`
		ErrorMessage       string      `json:"errorMessage"`
	} `json:"partialSuccess"`
}
