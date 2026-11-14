package evals

import (
	"encoding/json"
	"fmt"
	"math"
	"net/mail"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Schema is a compiled JSON Schema. It implements the practical subset
// used to check structured agent output:
//
//	type (string or list), enum, const,
//	properties, required, additionalProperties, patternProperties,
//	propertyNames, minProperties, maxProperties, dependentRequired,
//	items (schema or draft-07 tuple array), prefixItems, additionalItems,
//	contains, minItems, maxItems, uniqueItems,
//	minLength, maxLength, pattern, format (date-time, date, time, email,
//	uri, uuid, ipv4, ipv6, hostname),
//	minimum, maximum, exclusiveMinimum, exclusiveMaximum (draft-04
//	boolean and draft-06+ numeric forms), multipleOf,
//	allOf, anyOf, oneOf, not, if/then/else,
//	$ref to "#" or a "#/..." JSON pointer (definitions, $defs), and the
//	boolean schemas true and false.
//
// Unknown keywords are ignored, as the specification requires. Patterns
// use Go's RE2 syntax.
type Schema struct {
	root *schemaNode
}

// SchemaError is one validation failure at a JSON pointer.
type SchemaError struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func (e SchemaError) Error() string {
	p := e.Path
	if p == "" {
		p = "(root)"
	}
	return p + ": " + e.Message
}

type patternProp struct {
	re   *regexp.Regexp
	node *schemaNode
}

type schemaNode struct {
	boolean *bool

	types    []string
	enum     []any
	hasConst bool
	constVal any

	properties        map[string]*schemaNode
	required          []string
	additionalProps   *schemaNode
	patternProps      []patternProp
	propertyNames     *schemaNode
	minProps          *int
	maxProps          *int
	dependentRequired map[string][]string

	items           *schemaNode
	prefixItems     []*schemaNode
	additionalItems *schemaNode
	contains        *schemaNode
	minItems        *int
	maxItems        *int
	uniqueItems     bool

	minLength *int
	maxLength *int
	pattern   *regexp.Regexp
	format    string

	minimum    *float64
	maximum    *float64
	exclMin    *float64
	exclMax    *float64
	multipleOf *float64

	allOf []*schemaNode
	anyOf []*schemaNode
	oneOf []*schemaNode
	not   *schemaNode
	ifS   *schemaNode
	thenS *schemaNode
	elseS *schemaNode

	ref *schemaNode
}

type schemaCompiler struct {
	root  any
	cache map[string]*schemaNode
	depth int
}

const maxSchemaDepth = 128

// CompileSchema compiles a JSON Schema document.
func CompileSchema(raw []byte) (*Schema, error) {
	doc, err := decodeJSONValue(raw)
	if err != nil {
		return nil, fmt.Errorf("schema is not valid JSON: %w", err)
	}
	c := &schemaCompiler{root: doc, cache: map[string]*schemaNode{}}
	n, err := c.compileRef("#")
	if err != nil {
		return nil, err
	}
	return &Schema{root: n}, nil
}

func (c *schemaCompiler) compileRef(ref string) (*schemaNode, error) {
	if n, ok := c.cache[ref]; ok {
		return n, nil
	}
	if ref != "#" && !strings.HasPrefix(ref, "#/") {
		return nil, fmt.Errorf("unsupported $ref %q: only local references (#/...) are supported", ref)
	}
	target, err := jsonPointer(c.root, strings.TrimPrefix(ref, "#"))
	if err != nil {
		return nil, fmt.Errorf("$ref %q: %w", ref, err)
	}
	n := &schemaNode{}
	c.cache[ref] = n // registered before compiling so cycles terminate
	if err := c.fill(n, target, ref); err != nil {
		return nil, err
	}
	return n, nil
}

// jsonPointer resolves an RFC 6901 pointer.
func jsonPointer(doc any, ptr string) (any, error) {
	if ptr == "" {
		return doc, nil
	}
	if !strings.HasPrefix(ptr, "/") {
		return nil, fmt.Errorf("invalid JSON pointer %q", ptr)
	}
	cur := doc
	for _, raw := range strings.Split(ptr[1:], "/") {
		tok, err := url.PathUnescape(raw)
		if err != nil {
			return nil, err
		}
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		switch x := cur.(type) {
		case map[string]any:
			v, ok := x[tok]
			if !ok {
				return nil, fmt.Errorf("no member %q", tok)
			}
			cur = v
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(x) {
				return nil, fmt.Errorf("bad array index %q", tok)
			}
			cur = x[i]
		default:
			return nil, fmt.Errorf("cannot index into %T with %q", cur, tok)
		}
	}
	return cur, nil
}

func (c *schemaCompiler) compile(v any, at string) (*schemaNode, error) {
	n := &schemaNode{}
	if err := c.fill(n, v, at); err != nil {
		return nil, err
	}
	return n, nil
}

func (c *schemaCompiler) fill(n *schemaNode, v any, at string) error {
	c.depth++
	defer func() { c.depth-- }()
	if c.depth > maxSchemaDepth {
		return fmt.Errorf("%s: schema nested too deeply", at)
	}
	if b, ok := v.(bool); ok {
		n.boolean = &b
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: a schema must be an object or a boolean, got %s", at, jsonKind(v))
	}
	errf := func(kw, format string, args ...any) error {
		return fmt.Errorf("%s/%s: %s", at, kw, fmt.Sprintf(format, args...))
	}
	sub := func(kw string) (*schemaNode, error) {
		x, ok := m[kw]
		if !ok {
			return nil, nil
		}
		return c.compile(x, at+"/"+kw)
	}
	list := func(kw string) ([]*schemaNode, error) {
		x, ok := m[kw]
		if !ok {
			return nil, nil
		}
		arr, ok := x.([]any)
		if !ok || len(arr) == 0 {
			return nil, errf(kw, "must be a non-empty array of schemas")
		}
		out := make([]*schemaNode, len(arr))
		for i, e := range arr {
			s, err := c.compile(e, fmt.Sprintf("%s/%s/%d", at, kw, i))
			if err != nil {
				return nil, err
			}
			out[i] = s
		}
		return out, nil
	}
	nonNegInt := func(kw string) (*int, error) {
		x, ok := m[kw]
		if !ok {
			return nil, nil
		}
		f, ok := numberOf(x)
		if !ok || f < 0 || f != math.Trunc(f) || f > math.MaxInt32 {
			return nil, errf(kw, "must be a non-negative integer")
		}
		i := int(f)
		return &i, nil
	}
	num := func(kw string) (*float64, error) {
		x, ok := m[kw]
		if !ok {
			return nil, nil
		}
		f, ok := numberOf(x)
		if !ok {
			return nil, errf(kw, "must be a number")
		}
		return &f, nil
	}
	var err error

	if r, ok := m["$ref"]; ok {
		s, ok := r.(string)
		if !ok {
			return errf("$ref", "must be a string")
		}
		if n.ref, err = c.compileRef(s); err != nil {
			return err
		}
	}

	switch t := m["type"].(type) {
	case nil:
	case string:
		n.types = []string{t}
	case []any:
		for _, e := range t {
			s, ok := e.(string)
			if !ok {
				return errf("type", "entries must be strings")
			}
			n.types = append(n.types, s)
		}
	default:
		return errf("type", "must be a string or an array of strings")
	}
	for _, t := range n.types {
		switch t {
		case "null", "boolean", "object", "array", "number", "integer", "string":
		default:
			return errf("type", "unknown type %q", t)
		}
	}
	if e, ok := m["enum"]; ok {
		arr, ok := e.([]any)
		if !ok {
			return errf("enum", "must be an array")
		}
		n.enum = arr
	}
	if cv, ok := m["const"]; ok {
		n.hasConst, n.constVal = true, cv
	}

	if p, ok := m["properties"]; ok {
		pm, ok := p.(map[string]any)
		if !ok {
			return errf("properties", "must be an object")
		}
		n.properties = make(map[string]*schemaNode, len(pm))
		for _, k := range sortedKeys(pm) {
			s, err := c.compile(pm[k], at+"/properties/"+k)
			if err != nil {
				return err
			}
			n.properties[k] = s
		}
	}
	if r, ok := m["required"]; ok {
		arr, ok := r.([]any)
		if !ok {
			return errf("required", "must be an array of strings")
		}
		for _, e := range arr {
			s, ok := e.(string)
			if !ok {
				return errf("required", "must be an array of strings")
			}
			n.required = append(n.required, s)
		}
	}
	if n.additionalProps, err = sub("additionalProperties"); err != nil {
		return err
	}
	if p, ok := m["patternProperties"]; ok {
		pm, ok := p.(map[string]any)
		if !ok {
			return errf("patternProperties", "must be an object")
		}
		for _, k := range sortedKeys(pm) {
			re, err := regexp.Compile(k)
			if err != nil {
				return errf("patternProperties", "invalid pattern %q: %v", k, err)
			}
			s, err := c.compile(pm[k], at+"/patternProperties/"+k)
			if err != nil {
				return err
			}
			n.patternProps = append(n.patternProps, patternProp{re: re, node: s})
		}
	}
	if n.propertyNames, err = sub("propertyNames"); err != nil {
		return err
	}
	if n.minProps, err = nonNegInt("minProperties"); err != nil {
		return err
	}
	if n.maxProps, err = nonNegInt("maxProperties"); err != nil {
		return err
	}
	if d, ok := m["dependentRequired"]; ok {
		dm, ok := d.(map[string]any)
		if !ok {
			return errf("dependentRequired", "must be an object")
		}
		n.dependentRequired = map[string][]string{}
		for k, v := range dm {
			arr, ok := v.([]any)
			if !ok {
				return errf("dependentRequired", "%q must map to an array of strings", k)
			}
			for _, e := range arr {
				s, ok := e.(string)
				if !ok {
					return errf("dependentRequired", "%q must map to an array of strings", k)
				}
				n.dependentRequired[k] = append(n.dependentRequired[k], s)
			}
		}
	}

	switch it := m["items"].(type) {
	case nil:
	case []any:
		// Draft-07 tuple form.
		if n.prefixItems, err = list("items"); err != nil {
			return err
		}
		if n.additionalItems, err = sub("additionalItems"); err != nil {
			return err
		}
	default:
		if n.items, err = c.compile(it, at+"/items"); err != nil {
			return err
		}
	}
	if _, ok := m["prefixItems"]; ok {
		if n.prefixItems, err = list("prefixItems"); err != nil {
			return err
		}
	}
	if n.contains, err = sub("contains"); err != nil {
		return err
	}
	if n.minItems, err = nonNegInt("minItems"); err != nil {
		return err
	}
	if n.maxItems, err = nonNegInt("maxItems"); err != nil {
		return err
	}
	if u, ok := m["uniqueItems"]; ok {
		b, ok := u.(bool)
		if !ok {
			return errf("uniqueItems", "must be a boolean")
		}
		n.uniqueItems = b
	}

	if n.minLength, err = nonNegInt("minLength"); err != nil {
		return err
	}
	if n.maxLength, err = nonNegInt("maxLength"); err != nil {
		return err
	}
	if p, ok := m["pattern"]; ok {
		s, ok := p.(string)
		if !ok {
			return errf("pattern", "must be a string")
		}
		if n.pattern, err = regexp.Compile(s); err != nil {
			return errf("pattern", "invalid regular expression: %v", err)
		}
	}
	if f, ok := m["format"]; ok {
		s, ok := f.(string)
		if !ok {
			return errf("format", "must be a string")
		}
		n.format = s
	}

	if n.minimum, err = num("minimum"); err != nil {
		return err
	}
	if n.maximum, err = num("maximum"); err != nil {
		return err
	}
	// exclusiveMinimum/Maximum: numeric (draft-06+) or boolean modifiers
	// of minimum/maximum (draft-04).
	for _, kw := range []string{"exclusiveMinimum", "exclusiveMaximum"} {
		x, ok := m[kw]
		if !ok {
			continue
		}
		var target *float64
		if b, isBool := x.(bool); isBool {
			if b {
				if kw == "exclusiveMinimum" {
					target, n.minimum = n.minimum, nil
				} else {
					target, n.maximum = n.maximum, nil
				}
				if target == nil {
					return errf(kw, "boolean form needs %s", strings.ToLower(kw[9:10])+kw[10:])
				}
			}
		} else {
			f, ok := numberOf(x)
			if !ok {
				return errf(kw, "must be a number or a boolean")
			}
			target = &f
		}
		if kw == "exclusiveMinimum" {
			n.exclMin = target
		} else {
			n.exclMax = target
		}
	}
	if n.multipleOf, err = num("multipleOf"); err != nil {
		return err
	}
	if n.multipleOf != nil && *n.multipleOf <= 0 {
		return errf("multipleOf", "must be greater than 0")
	}

	if n.allOf, err = list("allOf"); err != nil {
		return err
	}
	if n.anyOf, err = list("anyOf"); err != nil {
		return err
	}
	if n.oneOf, err = list("oneOf"); err != nil {
		return err
	}
	if n.not, err = sub("not"); err != nil {
		return err
	}
	if n.ifS, err = sub("if"); err != nil {
		return err
	}
	if n.thenS, err = sub("then"); err != nil {
		return err
	}
	if n.elseS, err = sub("else"); err != nil {
		return err
	}
	return nil
}

func numberOf(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil && !math.IsInf(f, 0)
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}

func jsonKind(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case json.Number:
		if isInteger(x) {
			return "integer"
		}
		return "number"
	case float64:
		if x == math.Trunc(x) && !math.IsInf(x, 0) {
			return "integer"
		}
		return "number"
	}
	return fmt.Sprintf("%T", v)
}

func isInteger(n json.Number) bool {
	if _, err := strconv.ParseInt(string(n), 10, 64); err == nil {
		return true
	}
	f, err := n.Float64()
	return err == nil && f == math.Trunc(f) && !math.IsInf(f, 0)
}

// ValidateJSON parses raw JSON and validates it.
func (s *Schema) ValidateJSON(raw []byte) ([]SchemaError, error) {
	v, err := decodeJSONValue(raw)
	if err != nil {
		return nil, err
	}
	return s.Validate(v), nil
}

// maxValidateDepth stops runaway recursion through self-referencing
// schemas that consume no input (for example {"$ref": "#"}).
const maxValidateDepth = 512

// Validate checks a decoded JSON value (as produced by encoding/json, with
// or without UseNumber) and returns every violation found.
func (s *Schema) Validate(v any) []SchemaError {
	norm, err := normalizeAny(v, identity)
	if err != nil {
		return []SchemaError{{Message: "value is not JSON-compatible: " + err.Error()}}
	}
	var errs []SchemaError
	validateNode(s.root, norm, "", 0, &errs)
	return errs
}

func validateNode(n *schemaNode, v any, path string, depth int, errs *[]SchemaError) {
	add := func(format string, args ...any) {
		*errs = append(*errs, SchemaError{Path: path, Message: fmt.Sprintf(format, args...)})
	}
	if depth > maxValidateDepth {
		add("schema recursion too deep")
		return
	}
	if n.boolean != nil {
		if !*n.boolean {
			add("no value is allowed here")
		}
		return
	}
	if n.ref != nil {
		validateNode(n.ref, v, path, depth+1, errs)
	}
	kind := jsonKind(v)
	if len(n.types) > 0 {
		ok := false
		for _, t := range n.types {
			if t == kind || (t == "number" && kind == "integer") {
				ok = true
				break
			}
		}
		if !ok {
			add("expected %s, got %s", strings.Join(n.types, " or "), kind)
			return
		}
	}
	if n.enum != nil {
		found := false
		for _, e := range n.enum {
			if jsonEqual(e, v) {
				found = true
				break
			}
		}
		if !found {
			add("value %s is not one of %s", summarize(v), summarize(n.enum))
		}
	}
	if n.hasConst && !jsonEqual(n.constVal, v) {
		add("value %s must equal %s", summarize(v), summarize(n.constVal))
	}

	switch x := v.(type) {
	case map[string]any:
		validateObject(n, x, path, depth, errs, add)
	case []any:
		validateArray(n, x, path, depth, errs, add)
	case string:
		l := utf8.RuneCountInString(x)
		if n.minLength != nil && l < *n.minLength {
			add("string is shorter than %d characters", *n.minLength)
		}
		if n.maxLength != nil && l > *n.maxLength {
			add("string is longer than %d characters", *n.maxLength)
		}
		if n.pattern != nil && !n.pattern.MatchString(x) {
			add("string does not match pattern %q", n.pattern.String())
		}
		if n.format != "" {
			if msg := checkFormat(n.format, x); msg != "" {
				add("%s", msg)
			}
		}
	case json.Number:
		f, _ := x.Float64()
		if n.minimum != nil && f < *n.minimum {
			add("%s is less than the minimum %v", x, *n.minimum)
		}
		if n.maximum != nil && f > *n.maximum {
			add("%s is greater than the maximum %v", x, *n.maximum)
		}
		if n.exclMin != nil && f <= *n.exclMin {
			add("%s must be greater than %v", x, *n.exclMin)
		}
		if n.exclMax != nil && f >= *n.exclMax {
			add("%s must be less than %v", x, *n.exclMax)
		}
		if n.multipleOf != nil {
			q := f / *n.multipleOf
			if math.Abs(q-math.Round(q)) > 1e-9*math.Max(1, math.Abs(q)) {
				add("%s is not a multiple of %v", x, *n.multipleOf)
			}
		}
	}

	for _, s := range n.allOf {
		validateNode(s, v, path, depth+1, errs)
	}
	if len(n.anyOf) > 0 {
		ok := false
		for _, s := range n.anyOf {
			if validQuiet(s, v, path, depth) {
				ok = true
				break
			}
		}
		if !ok {
			add("value matches none of the anyOf schemas")
		}
	}
	if len(n.oneOf) > 0 {
		matches := 0
		for _, s := range n.oneOf {
			if validQuiet(s, v, path, depth) {
				matches++
			}
		}
		if matches != 1 {
			add("value must match exactly one oneOf schema, matched %d", matches)
		}
	}
	if n.not != nil && validQuiet(n.not, v, path, depth) {
		add("value must not match the \"not\" schema")
	}
	if n.ifS != nil {
		if validQuiet(n.ifS, v, path, depth) {
			if n.thenS != nil {
				validateNode(n.thenS, v, path, depth+1, errs)
			}
		} else if n.elseS != nil {
			validateNode(n.elseS, v, path, depth+1, errs)
		}
	}
}

func validQuiet(n *schemaNode, v any, path string, depth int) bool {
	var e []SchemaError
	validateNode(n, v, path, depth+1, &e)
	return len(e) == 0
}

func pointerJoin(base, tok string) string {
	return base + "/" + strings.ReplaceAll(strings.ReplaceAll(tok, "~", "~0"), "/", "~1")
}

func validateObject(n *schemaNode, x map[string]any, path string, depth int, errs *[]SchemaError, add func(string, ...any)) {
	if n.minProps != nil && len(x) < *n.minProps {
		add("object has fewer than %d properties", *n.minProps)
	}
	if n.maxProps != nil && len(x) > *n.maxProps {
		add("object has more than %d properties", *n.maxProps)
	}
	for _, r := range n.required {
		if _, ok := x[r]; !ok {
			add("missing required property %q", r)
		}
	}
	for _, k := range sortedKeys(n.dependentRequired) {
		if _, ok := x[k]; !ok {
			continue
		}
		for _, r := range n.dependentRequired[k] {
			if _, ok := x[r]; !ok {
				add("property %q requires property %q", k, r)
			}
		}
	}
	keys := sortedKeys(x)
	for _, k := range keys {
		p := pointerJoin(path, k)
		if n.propertyNames != nil {
			validateNode(n.propertyNames, k, p, depth+1, errs)
		}
		matched := false
		if s, ok := n.properties[k]; ok {
			matched = true
			validateNode(s, x[k], p, depth+1, errs)
		}
		for _, pp := range n.patternProps {
			if pp.re.MatchString(k) {
				matched = true
				validateNode(pp.node, x[k], p, depth+1, errs)
			}
		}
		if !matched && n.additionalProps != nil {
			if n.additionalProps.boolean != nil && !*n.additionalProps.boolean {
				*errs = append(*errs, SchemaError{Path: p, Message: "additional property is not allowed"})
			} else {
				validateNode(n.additionalProps, x[k], p, depth+1, errs)
			}
		}
	}
}

func validateArray(n *schemaNode, x []any, path string, depth int, errs *[]SchemaError, add func(string, ...any)) {
	if n.minItems != nil && len(x) < *n.minItems {
		add("array has fewer than %d items", *n.minItems)
	}
	if n.maxItems != nil && len(x) > *n.maxItems {
		add("array has more than %d items", *n.maxItems)
	}
	for i, e := range x {
		p := path + "/" + strconv.Itoa(i)
		switch {
		case i < len(n.prefixItems):
			validateNode(n.prefixItems[i], e, p, depth+1, errs)
		case len(n.prefixItems) > 0 && n.additionalItems != nil:
			validateNode(n.additionalItems, e, p, depth+1, errs)
		case n.items != nil:
			validateNode(n.items, e, p, depth+1, errs)
		}
	}
	if n.contains != nil {
		found := false
		for _, e := range x {
			if validQuiet(n.contains, e, path, depth) {
				found = true
				break
			}
		}
		if !found {
			add("array contains no item matching the \"contains\" schema")
		}
	}
	if n.uniqueItems {
		seen := map[string]int{}
		for i, e := range x {
			k, _ := canonicalJSON(e)
			if j, dup := seen[string(k)]; dup {
				add("items %d and %d are equal", j, i)
				break
			}
			seen[string(k)] = i
		}
	}
}

// jsonEqual compares two JSON values structurally with numbers compared by
// value.
func jsonEqual(a, b any) bool {
	na, err1 := normalizeAny(a, identity)
	nb, err2 := normalizeAny(b, identity)
	if err1 != nil || err2 != nil {
		return false
	}
	ra, _ := canonicalJSON(na)
	rb, _ := canonicalJSON(nb)
	return string(ra) == string(rb)
}

var (
	uuidRe     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	hostnameRe = regexp.MustCompile(`^(?i:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)(?:\.(?i:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?))*$`)
)

// checkFormat asserts the common formats; unknown formats pass.
func checkFormat(format, s string) string {
	bad := func() string { return fmt.Sprintf("%q is not a valid %s", s, format) }
	switch format {
	case "date-time":
		if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
			return bad()
		}
	case "date":
		if _, err := time.Parse(time.DateOnly, s); err != nil {
			return bad()
		}
	case "time":
		if _, err := time.Parse("15:04:05Z07:00", s); err != nil {
			if _, err2 := time.Parse("15:04:05.999999999Z07:00", s); err2 != nil {
				return bad()
			}
		}
	case "email":
		a, err := mail.ParseAddress(s)
		if err != nil || a.Address != s {
			return bad()
		}
	case "uri":
		u, err := url.Parse(s)
		if err != nil || u.Scheme == "" {
			return bad()
		}
	case "uuid":
		if !uuidRe.MatchString(s) {
			return bad()
		}
	case "ipv4":
		a, err := netip.ParseAddr(s)
		if err != nil || !a.Is4() {
			return bad()
		}
	case "ipv6":
		a, err := netip.ParseAddr(s)
		if err != nil || !a.Is6() {
			return bad()
		}
	case "hostname":
		if len(s) > 253 || !hostnameRe.MatchString(s) {
			return bad()
		}
	}
	return ""
}

// formatSchemaErrors renders up to limit errors on one line each, sorted.
func formatSchemaErrors(errs []SchemaError, limit int) string {
	lines := make([]string, len(errs))
	for i, e := range errs {
		lines[i] = e.Error()
	}
	sort.Strings(lines)
	if len(lines) > limit {
		lines = append(lines[:limit], fmt.Sprintf("… %d more", len(errs)-limit))
	}
	return strings.Join(lines, "; ")
}
