package workflow

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// Expression values are JSON-shaped: nil, bool, float64, string, []any and
// map[string]any. normalizeValue converts host values into that shape at
// every boundary (inputs, outputs, env), so the evaluator only ever sees
// these types.

func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case float64, int, int64, int32, uint, uint64, uint32, float32:
		return "number"
	case string:
		return "string"
	case []any:
		return "list"
	case map[string]any:
		return "map"
	}
	return fmt.Sprintf("%T", v)
}

func describeValue(v any) string {
	s := stringify(v)
	if len(s) > 40 {
		s = s[:37] + "..."
	}
	return typeName(v) + " " + strconv.Quote(s)
}

func toNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint64:
		return float64(n), true
	case uint32:
		return float64(n), true
	case float32:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	if f, ok := toNumber(v); ok {
		return f != 0 && !math.IsNaN(f)
	}
	return true
}

// valuesEqual is strict, deep equality: values of different types are never
// equal.
func valuesEqual(a, b any) bool {
	if af, ok := toNumber(a); ok {
		bf, ok := toNumber(b)
		return ok && af == bf
	}
	switch x := a.(type) {
	case nil:
		return b == nil
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case string:
		y, ok := b.(string)
		return ok && x == y
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !valuesEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, xv := range x {
			yv, ok := y[k]
			if !ok || !valuesEqual(xv, yv) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(a, b)
}

func compareOrdered(a, b any) (int, error) {
	if af, ok := toNumber(a); ok {
		if bf, ok := toNumber(b); ok {
			switch {
			case af < bf:
				return -1, nil
			case af > bf:
				return 1, nil
			}
			return 0, nil
		}
	}
	if as, ok := a.(string); ok {
		if bs, ok := b.(string); ok {
			return strings.Compare(as, bs), nil
		}
	}
	return 0, fmt.Errorf("cannot compare a %s with a %s", typeName(a), typeName(b))
}

// stringify renders a value for string interpolation: null is empty,
// integral numbers have no decimal point, lists and maps are JSON.
func stringify(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	}
	if f, ok := toNumber(v); ok {
		return formatNumber(f)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func formatNumber(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// normalizeValue converts a host value into the JSON value model. Common
// types are converted directly; anything else goes through a JSON round
// trip. It fails for values JSON cannot represent.
func normalizeValue(v any) (any, error) {
	switch x := v.(type) {
	case nil, bool, string:
		return x, nil
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil, fmt.Errorf("number %v is not representable in JSON", x)
		}
		return x, nil
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			n, err := normalizeValue(e)
			if err != nil {
				return nil, err
			}
			out[i] = n
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			n, err := normalizeValue(e)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			out[k] = n
		}
		return out, nil
	case []string:
		out := make([]any, len(x))
		for i, s := range x {
			out[i] = s
		}
		return out, nil
	case map[string]string:
		out := make(map[string]any, len(x))
		for k, s := range x {
			out[k] = s
		}
		return out, nil
	}
	if f, ok := toNumber(v); ok {
		return normalizeValue(f)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func normalizeMap(m map[string]any) (map[string]any, error) {
	if m == nil {
		return map[string]any{}, nil
	}
	v, err := normalizeValue(m)
	if err != nil {
		return nil, err
	}
	return v.(map[string]any), nil
}

// deepCopy copies a normalized value so a consumer cannot mutate shared
// state.
func deepCopy(v any) any {
	switch x := v.(type) {
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = deepCopy(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = deepCopy(e)
		}
		return out
	}
	return v
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
