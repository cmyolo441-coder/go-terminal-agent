package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

type builtinFunc struct {
	name     string
	min, max int // max < 0: variadic
	status   bool
	fn       func(env *evalEnv, args []any) (any, error)
}

const (
	maxRegexpLen  = 1024
	maxJSONLen    = 1 << 20
	maxResultSize = 1 << 20
)

var builtinFuncs map[string]*builtinFunc

func init() {
	fs := []*builtinFunc{
		{name: "len", min: 1, max: 1, fn: fnLen},
		{name: "lower", min: 1, max: 1, fn: strFunc(strings.ToLower)},
		{name: "upper", min: 1, max: 1, fn: strFunc(strings.ToUpper)},
		{name: "trim", min: 1, max: 1, fn: strFunc(strings.TrimSpace)},
		{name: "trimPrefix", min: 2, max: 2, fn: str2Func(strings.TrimPrefix)},
		{name: "trimSuffix", min: 2, max: 2, fn: str2Func(strings.TrimSuffix)},
		{name: "startsWith", min: 2, max: 2, fn: str2Bool(strings.HasPrefix)},
		{name: "endsWith", min: 2, max: 2, fn: str2Bool(strings.HasSuffix)},
		{name: "contains", min: 2, max: 2, fn: fnContains},
		{name: "replace", min: 3, max: 3, fn: fnReplace},
		{name: "split", min: 2, max: 2, fn: fnSplit},
		{name: "join", min: 1, max: 2, fn: fnJoin},
		{name: "matches", min: 2, max: 2, fn: fnMatches},
		{name: "format", min: 1, max: -1, fn: fnFormat},
		{name: "toJSON", min: 1, max: 1, fn: fnToJSON},
		{name: "fromJSON", min: 1, max: 1, fn: fnFromJSON},
		{name: "string", min: 1, max: 1, fn: func(_ *evalEnv, a []any) (any, error) { return stringify(a[0]), nil }},
		{name: "number", min: 1, max: 1, fn: fnNumber},
		{name: "bool", min: 1, max: 1, fn: func(_ *evalEnv, a []any) (any, error) { return truthy(a[0]), nil }},
		{name: "default", min: 2, max: 2, fn: fnDefault},
		{name: "keys", min: 1, max: 1, fn: fnKeys},
		{name: "success", min: 0, max: 0, status: true, fn: fnStatus("success")},
		{name: "failure", min: 0, max: 0, status: true, fn: fnStatus("failure")},
		{name: "always", min: 0, max: 0, status: true, fn: fnStatus("always")},
	}
	builtinFuncs = make(map[string]*builtinFunc, len(fs))
	for _, f := range fs {
		builtinFuncs[f.name] = f
	}
}

func argString(a any, i int) (string, error) {
	s, ok := a.(string)
	if !ok {
		return "", fmt.Errorf("argument %d must be a string, not a %s", i+1, typeName(a))
	}
	return s, nil
}

func strFunc(f func(string) string) func(*evalEnv, []any) (any, error) {
	return func(_ *evalEnv, a []any) (any, error) {
		s, err := argString(a[0], 0)
		if err != nil {
			return nil, err
		}
		return f(s), nil
	}
}

func str2Func(f func(string, string) string) func(*evalEnv, []any) (any, error) {
	return func(_ *evalEnv, a []any) (any, error) {
		s, err := argString(a[0], 0)
		if err != nil {
			return nil, err
		}
		t, err := argString(a[1], 1)
		if err != nil {
			return nil, err
		}
		return f(s, t), nil
	}
}

func str2Bool(f func(string, string) bool) func(*evalEnv, []any) (any, error) {
	return func(_ *evalEnv, a []any) (any, error) {
		s, err := argString(a[0], 0)
		if err != nil {
			return nil, err
		}
		t, err := argString(a[1], 1)
		if err != nil {
			return nil, err
		}
		return f(s, t), nil
	}
}

func fnLen(_ *evalEnv, a []any) (any, error) {
	switch x := a[0].(type) {
	case nil:
		return 0.0, nil
	case string:
		return float64(utf8.RuneCountInString(x)), nil
	case []any:
		return float64(len(x)), nil
	case map[string]any:
		return float64(len(x)), nil
	}
	return nil, fmt.Errorf("cannot take the length of a %s", typeName(a[0]))
}

func fnContains(_ *evalEnv, a []any) (any, error) {
	switch x := a[0].(type) {
	case nil:
		return false, nil
	case string:
		s, err := argString(a[1], 1)
		if err != nil {
			return nil, err
		}
		return strings.Contains(x, s), nil
	case []any:
		for _, e := range x {
			if valuesEqual(e, a[1]) {
				return true, nil
			}
		}
		return false, nil
	}
	return nil, fmt.Errorf("argument 1 must be a string or list, not a %s", typeName(a[0]))
}

func fnReplace(_ *evalEnv, a []any) (any, error) {
	var ss [3]string
	for i := range ss {
		s, err := argString(a[i], i)
		if err != nil {
			return nil, err
		}
		ss[i] = s
	}
	if ss[1] == "" {
		return nil, errors.New("the string to replace must not be empty")
	}
	if n := strings.Count(ss[0], ss[1]); n > 0 && len(ss[0])+n*(len(ss[2])-len(ss[1])) > maxResultSize {
		return nil, fmt.Errorf("result exceeds %d bytes", maxResultSize)
	}
	return strings.ReplaceAll(ss[0], ss[1], ss[2]), nil
}

func fnSplit(_ *evalEnv, a []any) (any, error) {
	s, err := argString(a[0], 0)
	if err != nil {
		return nil, err
	}
	sep, err := argString(a[1], 1)
	if err != nil {
		return nil, err
	}
	if s == "" {
		return []any{}, nil
	}
	parts := strings.Split(s, sep)
	out := make([]any, len(parts))
	for i, p := range parts {
		out[i] = p
	}
	return out, nil
}

func fnJoin(_ *evalEnv, a []any) (any, error) {
	list, ok := a[0].([]any)
	if !ok {
		if a[0] == nil {
			return "", nil
		}
		return nil, fmt.Errorf("argument 1 must be a list, not a %s", typeName(a[0]))
	}
	sep := ","
	if len(a) == 2 {
		s, err := argString(a[1], 1)
		if err != nil {
			return nil, err
		}
		sep = s
	}
	parts := make([]string, len(list))
	for i, e := range list {
		parts[i] = stringify(e)
	}
	return strings.Join(parts, sep), nil
}

var regexpCache = struct {
	sync.Mutex
	m map[string]*regexp.Regexp
}{m: map[string]*regexp.Regexp{}}

func compileRegexp(p string) (*regexp.Regexp, error) {
	if len(p) > maxRegexpLen {
		return nil, fmt.Errorf("pattern is longer than %d bytes", maxRegexpLen)
	}
	regexpCache.Lock()
	defer regexpCache.Unlock()
	if re, ok := regexpCache.m[p]; ok {
		return re, nil
	}
	re, err := regexp.Compile(p) // RE2: linear time, no catastrophic backtracking
	if err != nil {
		return nil, err
	}
	if len(regexpCache.m) >= 256 {
		clear(regexpCache.m)
	}
	regexpCache.m[p] = re
	return re, nil
}

func fnMatches(_ *evalEnv, a []any) (any, error) {
	s, err := argString(a[0], 0)
	if err != nil {
		return nil, err
	}
	p, err := argString(a[1], 1)
	if err != nil {
		return nil, err
	}
	re, err := compileRegexp(p)
	if err != nil {
		return nil, err
	}
	return re.MatchString(s), nil
}

// fnFormat substitutes {0}, {1}, ... with the stringified arguments; {{ and
// }} are literal braces.
func fnFormat(_ *evalEnv, a []any) (any, error) {
	f, err := argString(a[0], 0)
	if err != nil {
		return nil, err
	}
	args := a[1:]
	var b strings.Builder
	for i := 0; i < len(f); i++ {
		c := f[i]
		switch {
		case c == '{' && i+1 < len(f) && f[i+1] == '{':
			b.WriteByte('{')
			i++
		case c == '}' && i+1 < len(f) && f[i+1] == '}':
			b.WriteByte('}')
			i++
		case c == '{':
			end := strings.IndexByte(f[i:], '}')
			if end < 0 {
				return nil, errors.New("unterminated placeholder")
			}
			n, err := strconv.Atoi(f[i+1 : i+end])
			if err != nil || n < 0 {
				return nil, fmt.Errorf("invalid placeholder %q", f[i:i+end+1])
			}
			if n >= len(args) {
				return nil, fmt.Errorf("placeholder {%d} has no argument", n)
			}
			b.WriteString(stringify(args[n]))
			if b.Len() > maxResultSize {
				return nil, fmt.Errorf("result exceeds %d bytes", maxResultSize)
			}
			i += end
		case c == '}':
			return nil, errors.New("unmatched '}'; write }} for a literal brace")
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), nil
}

func fnToJSON(_ *evalEnv, a []any) (any, error) {
	b, err := json.Marshal(a[0])
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

func fnFromJSON(_ *evalEnv, a []any) (any, error) {
	s, err := argString(a[0], 0)
	if err != nil {
		return nil, err
	}
	if len(s) > maxJSONLen {
		return nil, fmt.Errorf("input is longer than %d bytes", maxJSONLen)
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil, err
	}
	return v, nil
}

func fnNumber(_ *evalEnv, a []any) (any, error) {
	switch x := a[0].(type) {
	case nil:
		return 0.0, nil
	case bool:
		if x {
			return 1.0, nil
		}
		return 0.0, nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number", x)
		}
		return f, nil
	}
	if f, ok := toNumber(a[0]); ok {
		return f, nil
	}
	return nil, fmt.Errorf("cannot convert a %s to a number", typeName(a[0]))
}

// fnDefault returns its first argument unless it is null or empty.
func fnDefault(_ *evalEnv, a []any) (any, error) {
	if a[0] == nil || a[0] == "" {
		return a[1], nil
	}
	return a[0], nil
}

func fnKeys(_ *evalEnv, a []any) (any, error) {
	m, ok := a[0].(map[string]any)
	if !ok {
		if a[0] == nil {
			return []any{}, nil
		}
		return nil, fmt.Errorf("argument must be a map, not a %s", typeName(a[0]))
	}
	keys := sortedKeys(m)
	out := make([]any, len(keys))
	for i, k := range keys {
		out[i] = k
	}
	return out, nil
}

func fnStatus(kind string) func(*evalEnv, []any) (any, error) {
	return func(env *evalEnv, _ []any) (any, error) {
		d := env.deps
		if d == nil {
			return nil, errors.New("status functions are only available in step `when` conditions")
		}
		switch kind {
		case "success":
			return d.allSucceeded(), nil
		case "failure":
			return d.failed > 0, nil
		}
		return true, nil
	}
}
