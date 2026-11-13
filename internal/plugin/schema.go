package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// schema is a compiled JSON Schema subset: type, enum, const, properties,
// required, additionalProperties, items, min/max length, items and numeric
// bounds, pattern, and allOf/anyOf/oneOf/not. Plugins get their input
// checked by GTA before the executable starts, so a tool never sees input
// that does not match what it declared.
type schema struct {
	raw        map[string]any
	types      []string
	enum       []any
	hasConst   bool
	constVal   any
	props      map[string]*schema
	required   []string
	additional *schema // nil: anything allowed
	noExtra    bool
	items      *schema
	pattern    *regexp.Regexp
	minLen     *int
	maxLen     *int
	minItems   *int
	maxItems   *int
	minimum    *float64
	maximum    *float64
	exMinimum  *float64
	exMaximum  *float64
	allOf      []*schema
	anyOf      []*schema
	oneOf      []*schema
	not        *schema
}

var validTypes = map[string]bool{"object": true, "array": true, "string": true, "number": true, "integer": true, "boolean": true, "null": true}

// unsupported keywords are refused rather than silently ignored.
var unsupportedKeywords = []string{"$ref", "$dynamicRef", "if", "then", "else", "patternProperties", "dependentSchemas", "dependentRequired", "unevaluatedProperties", "prefixItems", "contains"}

// compileSchema checks and compiles a tool's input schema. An empty schema
// means a tool without parameters. The top level must describe an object.
func compileSchema(raw json.RawMessage) (*schema, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return &schema{raw: map[string]any{"type": "object", "properties": map[string]any{}}, types: []string{"object"}, props: map[string]*schema{}}, nil
	}
	var node map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&node); err != nil || node == nil {
		return nil, errors.New("must be a JSON object")
	}
	s, err := compileNode(node, "")
	if err != nil {
		return nil, err
	}
	if len(s.types) == 0 {
		s.types = []string{"object"}
	}
	if len(s.types) != 1 || s.types[0] != "object" {
		return nil, errors.New(`the top level must be {"type": "object", ...}`)
	}
	if s.props == nil {
		s.props = map[string]*schema{}
	}
	for _, r := range s.required {
		if _, ok := s.props[r]; !ok {
			return nil, fmt.Errorf("required property %q is not in properties", r)
		}
	}
	return s, nil
}

func compileNode(node map[string]any, path string) (*schema, error) {
	at := func(err error) error {
		if path == "" {
			return err
		}
		return fmt.Errorf("%s: %w", path, err)
	}
	for _, k := range unsupportedKeywords {
		if _, ok := node[k]; ok {
			return nil, at(fmt.Errorf("%q is not supported; inline the definition instead", k))
		}
	}
	s := &schema{raw: node}
	switch t := node["type"].(type) {
	case nil:
	case string:
		s.types = []string{t}
	case []any:
		for _, v := range t {
			str, ok := v.(string)
			if !ok {
				return nil, at(errors.New(`"type" entries must be strings`))
			}
			s.types = append(s.types, str)
		}
	default:
		return nil, at(errors.New(`"type" must be a string or an array of strings`))
	}
	for _, t := range s.types {
		if !validTypes[t] {
			return nil, at(fmt.Errorf("unknown type %q", t))
		}
	}
	if e, ok := node["enum"]; ok {
		list, ok := e.([]any)
		if !ok || len(list) == 0 {
			return nil, at(errors.New(`"enum" must be a non-empty array`))
		}
		s.enum = list
	}
	if c, ok := node["const"]; ok {
		s.hasConst, s.constVal = true, c
	}
	if p, ok := node["properties"]; ok {
		pm, ok := p.(map[string]any)
		if !ok {
			return nil, at(errors.New(`"properties" must be an object`))
		}
		s.props = map[string]*schema{}
		for name, sub := range pm {
			subNode, ok := sub.(map[string]any)
			if !ok {
				return nil, at(fmt.Errorf("property %q must be a schema object", name))
			}
			cs, err := compileNode(subNode, joinPath(path, name))
			if err != nil {
				return nil, err
			}
			s.props[name] = cs
		}
	}
	if r, ok := node["required"]; ok {
		list, ok := r.([]any)
		if !ok {
			return nil, at(errors.New(`"required" must be an array of strings`))
		}
		for _, v := range list {
			str, ok := v.(string)
			if !ok {
				return nil, at(errors.New(`"required" must be an array of strings`))
			}
			s.required = append(s.required, str)
		}
	}
	switch a := node["additionalProperties"].(type) {
	case nil:
	case bool:
		s.noExtra = !a
	case map[string]any:
		cs, err := compileNode(a, joinPath(path, "additionalProperties"))
		if err != nil {
			return nil, err
		}
		s.additional = cs
	default:
		return nil, at(errors.New(`"additionalProperties" must be a boolean or a schema`))
	}
	if it, ok := node["items"]; ok {
		in, ok := it.(map[string]any)
		if !ok {
			return nil, at(errors.New(`"items" must be a schema object`))
		}
		cs, err := compileNode(in, path+"[]")
		if err != nil {
			return nil, err
		}
		s.items = cs
	}
	if p, ok := node["pattern"]; ok {
		str, ok := p.(string)
		if !ok {
			return nil, at(errors.New(`"pattern" must be a string`))
		}
		re, err := regexp.Compile(str)
		if err != nil {
			return nil, at(fmt.Errorf(`"pattern": %v`, err))
		}
		s.pattern = re
	}
	for key, dst := range map[string]**int{"minLength": &s.minLen, "maxLength": &s.maxLen, "minItems": &s.minItems, "maxItems": &s.maxItems} {
		if v, ok := node[key]; ok {
			n, err := nonNegInt(v)
			if err != nil {
				return nil, at(fmt.Errorf("%q %v", key, err))
			}
			*dst = &n
		}
	}
	for key, dst := range map[string]**float64{"minimum": &s.minimum, "maximum": &s.maximum, "exclusiveMinimum": &s.exMinimum, "exclusiveMaximum": &s.exMaximum} {
		if v, ok := node[key]; ok {
			f, ok := toFloat(v)
			if !ok {
				return nil, at(fmt.Errorf("%q must be a number", key))
			}
			*dst = &f
		}
	}
	for key, dst := range map[string]*[]*schema{"allOf": &s.allOf, "anyOf": &s.anyOf, "oneOf": &s.oneOf} {
		v, ok := node[key]
		if !ok {
			continue
		}
		list, ok := v.([]any)
		if !ok || len(list) == 0 {
			return nil, at(fmt.Errorf("%q must be a non-empty array of schemas", key))
		}
		for i, item := range list {
			in, ok := item.(map[string]any)
			if !ok {
				return nil, at(fmt.Errorf("%q must be a non-empty array of schemas", key))
			}
			cs, err := compileNode(in, fmt.Sprintf("%s(%s %d)", path, key, i))
			if err != nil {
				return nil, err
			}
			*dst = append(*dst, cs)
		}
	}
	if n, ok := node["not"]; ok {
		in, ok := n.(map[string]any)
		if !ok {
			return nil, at(errors.New(`"not" must be a schema object`))
		}
		cs, err := compileNode(in, path+"(not)")
		if err != nil {
			return nil, err
		}
		s.not = cs
	}
	return s, nil
}

func joinPath(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

func nonNegInt(v any) (int, error) {
	f, ok := toFloat(v)
	if !ok || f < 0 || f != math.Trunc(f) || f > math.MaxInt32 {
		return 0, errors.New("must be a non-negative integer")
	}
	return int(f), nil
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case float64:
		return n, true
	}
	return 0, false
}

// properties returns the top-level properties as plain JSON-able values for
// the model-facing tool spec.
func (s *schema) properties() map[string]any {
	out := map[string]any{}
	if p, ok := s.raw["properties"].(map[string]any); ok {
		for k, v := range p {
			out[k] = plainJSON(v)
		}
	}
	return out
}

// plainJSON converts json.Number values to float64 or int64 so the schema
// marshals the same way through any encoder.
func plainJSON(v any) any {
	switch t := v.(type) {
	case json.Number:
		if i, err := strconv.ParseInt(string(t), 10, 64); err == nil {
			return i
		}
		f, _ := t.Float64()
		return f
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = plainJSON(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = plainJSON(x)
		}
		return out
	}
	return v
}

// validate checks a decoded value (decoded with UseNumber) against s.
func (s *schema) validate(v any, path string) error {
	where := path
	if where == "" {
		where = "input"
	}
	if len(s.types) > 0 {
		ok := false
		for _, t := range s.types {
			if typeMatches(t, v) {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("%s must be %s, got %s", where, strings.Join(s.types, " or "), jsonType(v))
		}
	}
	if s.enum != nil {
		ok := false
		for _, e := range s.enum {
			if jsonEqual(e, v) {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("%s must be one of %s", where, renderList(s.enum))
		}
	}
	if s.hasConst && !jsonEqual(s.constVal, v) {
		return fmt.Errorf("%s must be %s", where, renderValue(s.constVal))
	}
	switch val := v.(type) {
	case string:
		n := utf8.RuneCountInString(val)
		if s.minLen != nil && n < *s.minLen {
			return fmt.Errorf("%s must be at least %d characters", where, *s.minLen)
		}
		if s.maxLen != nil && n > *s.maxLen {
			return fmt.Errorf("%s must be at most %d characters", where, *s.maxLen)
		}
		if s.pattern != nil && !s.pattern.MatchString(val) {
			return fmt.Errorf("%s must match the pattern %s", where, s.pattern)
		}
	case json.Number:
		f, _ := val.Float64()
		switch {
		case s.minimum != nil && f < *s.minimum:
			return fmt.Errorf("%s must be >= %v", where, *s.minimum)
		case s.maximum != nil && f > *s.maximum:
			return fmt.Errorf("%s must be <= %v", where, *s.maximum)
		case s.exMinimum != nil && f <= *s.exMinimum:
			return fmt.Errorf("%s must be > %v", where, *s.exMinimum)
		case s.exMaximum != nil && f >= *s.exMaximum:
			return fmt.Errorf("%s must be < %v", where, *s.exMaximum)
		}
	case []any:
		if s.minItems != nil && len(val) < *s.minItems {
			return fmt.Errorf("%s must have at least %d items", where, *s.minItems)
		}
		if s.maxItems != nil && len(val) > *s.maxItems {
			return fmt.Errorf("%s must have at most %d items", where, *s.maxItems)
		}
		if s.items != nil {
			for i, item := range val {
				if err := s.items.validate(item, fmt.Sprintf("%s[%d]", where, i)); err != nil {
					return err
				}
			}
		}
	case map[string]any:
		for _, r := range s.required {
			if _, ok := val[r]; !ok {
				return fmt.Errorf("%q is required", joinPath(path, r))
			}
		}
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if ps, ok := s.props[k]; ok {
				if err := ps.validate(val[k], joinPath(path, k)); err != nil {
					return err
				}
				continue
			}
			if s.noExtra {
				return fmt.Errorf("unknown property %q", joinPath(path, k))
			}
			if s.additional != nil {
				if err := s.additional.validate(val[k], joinPath(path, k)); err != nil {
					return err
				}
			}
		}
	}
	for _, sub := range s.allOf {
		if err := sub.validate(v, path); err != nil {
			return err
		}
	}
	if len(s.anyOf) > 0 {
		var first error
		ok := false
		for _, sub := range s.anyOf {
			err := sub.validate(v, path)
			if err == nil {
				ok = true
				break
			}
			if first == nil {
				first = err
			}
		}
		if !ok {
			return fmt.Errorf("%s matches none of the allowed forms (first: %v)", where, first)
		}
	}
	if len(s.oneOf) > 0 {
		n := 0
		for _, sub := range s.oneOf {
			if sub.validate(v, path) == nil {
				n++
			}
		}
		if n != 1 {
			return fmt.Errorf("%s must match exactly one of the allowed forms (matches %d)", where, n)
		}
	}
	if s.not != nil && s.not.validate(v, path) == nil {
		return fmt.Errorf("%s is not allowed", where)
	}
	return nil
}

func typeMatches(t string, v any) bool {
	switch t {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "null":
		return v == nil
	case "number":
		_, ok := v.(json.Number)
		return ok
	case "integer":
		n, ok := v.(json.Number)
		if !ok {
			return false
		}
		if _, err := n.Int64(); err == nil {
			return true
		}
		f, err := n.Float64()
		return err == nil && f == math.Trunc(f) && !math.IsInf(f, 0)
	}
	return false
}

func jsonType(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case json.Number:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

// jsonEqual compares decoded JSON values, treating numbers by value.
func jsonEqual(a, b any) bool {
	fa, aNum := toFloat(a)
	fb, bNum := toFloat(b)
	if aNum || bNum {
		return aNum && bNum && fa == fb
	}
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			if w, ok := y[k]; !ok || !jsonEqual(v, w) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !jsonEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	}
	return a == b
}

func renderValue(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func renderList(vs []any) string {
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = renderValue(v)
	}
	return strings.Join(parts, ", ")
}
