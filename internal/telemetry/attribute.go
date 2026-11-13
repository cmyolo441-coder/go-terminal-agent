package telemetry

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ValueType identifies what an attribute Value holds.
type ValueType int

// Attribute value types. They map one-to-one onto the OTLP AnyValue union
// (stringValue, boolValue, intValue, doubleValue, arrayValue).
const (
	TypeInvalid ValueType = iota
	TypeString
	TypeBool
	TypeInt64
	TypeFloat64
	TypeStringSlice
	TypeBoolSlice
	TypeInt64Slice
	TypeFloat64Slice
)

// Value is an immutable attribute value.
type Value struct {
	typ ValueType
	s   string
	n   int64
	f   float64
	b   bool
	ss  []string
	bs  []bool
	ns  []int64
	fs  []float64
}

// Type reports the kind of value held.
func (v Value) Type() ValueType { return v.typ }

// AsString returns the string held by a TypeString value.
func (v Value) AsString() string { return v.s }

// AsBool returns the bool held by a TypeBool value.
func (v Value) AsBool() bool { return v.b }

// AsInt64 returns the integer held by a TypeInt64 value.
func (v Value) AsInt64() int64 { return v.n }

// AsFloat64 returns the float held by a TypeFloat64 value.
func (v Value) AsFloat64() float64 { return v.f }

// AsStringSlice returns a copy of the strings held by a TypeStringSlice value.
func (v Value) AsStringSlice() []string { return append([]string(nil), v.ss...) }

// AsInterface returns the value as a plain Go value.
func (v Value) AsInterface() any {
	switch v.typ {
	case TypeString:
		return v.s
	case TypeBool:
		return v.b
	case TypeInt64:
		return v.n
	case TypeFloat64:
		return v.f
	case TypeStringSlice:
		return append([]string(nil), v.ss...)
	case TypeBoolSlice:
		return append([]bool(nil), v.bs...)
	case TypeInt64Slice:
		return append([]int64(nil), v.ns...)
	case TypeFloat64Slice:
		return append([]float64(nil), v.fs...)
	}
	return nil
}

// Emit renders the value as text, for logs and attribute-set keys.
func (v Value) Emit() string {
	switch v.typ {
	case TypeString:
		return v.s
	case TypeBool:
		return strconv.FormatBool(v.b)
	case TypeInt64:
		return strconv.FormatInt(v.n, 10)
	case TypeFloat64:
		return strconv.FormatFloat(v.f, 'g', -1, 64)
	case TypeStringSlice:
		return "[" + strings.Join(v.ss, ",") + "]"
	case TypeBoolSlice:
		parts := make([]string, len(v.bs))
		for i, b := range v.bs {
			parts[i] = strconv.FormatBool(b)
		}
		return "[" + strings.Join(parts, ",") + "]"
	case TypeInt64Slice:
		parts := make([]string, len(v.ns))
		for i, n := range v.ns {
			parts[i] = strconv.FormatInt(n, 10)
		}
		return "[" + strings.Join(parts, ",") + "]"
	case TypeFloat64Slice:
		parts := make([]string, len(v.fs))
		for i, f := range v.fs {
			parts[i] = strconv.FormatFloat(f, 'g', -1, 64)
		}
		return "[" + strings.Join(parts, ",") + "]"
	}
	return ""
}

// truncate shortens string values to at most limit runes (limit <= 0 keeps
// them whole), as OTEL_ATTRIBUTE_VALUE_LENGTH_LIMIT requires.
func (v Value) truncate(limit int) Value {
	if limit <= 0 {
		return v
	}
	switch v.typ {
	case TypeString:
		v.s = truncateRunes(v.s, limit)
	case TypeStringSlice:
		out := make([]string, len(v.ss))
		for i, s := range v.ss {
			out[i] = truncateRunes(s, limit)
		}
		v.ss = out
	}
	return v
}

func truncateRunes(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	n := 0
	for i := range s {
		if n == limit {
			return s[:i]
		}
		n++
	}
	return s
}

// KeyValue is one attribute.
type KeyValue struct {
	Key   string
	Value Value
}

// Valid reports whether the attribute has a key and a typed value.
func (kv KeyValue) Valid() bool { return kv.Key != "" && kv.Value.typ != TypeInvalid }

// String builds a string attribute.
func String(k, v string) KeyValue { return KeyValue{k, Value{typ: TypeString, s: v}} }

// Bool builds a bool attribute.
func Bool(k string, v bool) KeyValue { return KeyValue{k, Value{typ: TypeBool, b: v}} }

// Int builds an integer attribute.
func Int(k string, v int) KeyValue { return Int64(k, int64(v)) }

// Int64 builds an integer attribute.
func Int64(k string, v int64) KeyValue { return KeyValue{k, Value{typ: TypeInt64, n: v}} }

// Float64 builds a floating-point attribute.
func Float64(k string, v float64) KeyValue { return KeyValue{k, Value{typ: TypeFloat64, f: v}} }

// StringSlice builds a string-array attribute. The slice is copied.
func StringSlice(k string, v []string) KeyValue {
	return KeyValue{k, Value{typ: TypeStringSlice, ss: append([]string(nil), v...)}}
}

// BoolSlice builds a bool-array attribute. The slice is copied.
func BoolSlice(k string, v []bool) KeyValue {
	return KeyValue{k, Value{typ: TypeBoolSlice, bs: append([]bool(nil), v...)}}
}

// Int64Slice builds an integer-array attribute. The slice is copied.
func Int64Slice(k string, v []int64) KeyValue {
	return KeyValue{k, Value{typ: TypeInt64Slice, ns: append([]int64(nil), v...)}}
}

// Float64Slice builds a float-array attribute. The slice is copied.
func Float64Slice(k string, v []float64) KeyValue {
	return KeyValue{k, Value{typ: TypeFloat64Slice, fs: append([]float64(nil), v...)}}
}

// attrSet is a de-duplicated, key-sorted attribute list with a canonical
// encoding used to key metric data points.
type attrSet struct {
	kvs []KeyValue
	key string
}

// newAttrSet de-duplicates attrs (the last value of a key wins), drops
// invalid ones and sorts by key.
func newAttrSet(attrs []KeyValue) attrSet {
	if len(attrs) == 0 {
		return attrSet{}
	}
	idx := make(map[string]int, len(attrs))
	out := make([]KeyValue, 0, len(attrs))
	for _, kv := range attrs {
		if !kv.Valid() {
			continue
		}
		if i, ok := idx[kv.Key]; ok {
			out[i] = kv
			continue
		}
		idx[kv.Key] = len(out)
		out = append(out, kv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	var b strings.Builder
	for _, kv := range out {
		// Length-prefix every part so no key or value can forge a boundary.
		b.WriteString(strconv.Itoa(len(kv.Key)))
		b.WriteByte(':')
		b.WriteString(kv.Key)
		b.WriteByte(byte('0' + kv.Value.typ))
		e := kv.Value.Emit()
		if kv.Value.typ == TypeFloat64 && math.IsNaN(kv.Value.f) {
			e = "NaN"
		}
		b.WriteString(strconv.Itoa(len(e)))
		b.WriteByte(':')
		b.WriteString(e)
	}
	return attrSet{kvs: out, key: b.String()}
}
