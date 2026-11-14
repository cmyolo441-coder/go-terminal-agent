package workflow

import (
	"fmt"
	"strings"
)

// Template is a string with embedded ${...} expressions. Write $${ for a
// literal "${".
type Template struct {
	src   string
	parts []tplPart
}

type tplPart struct {
	lit  string
	expr *Expr
}

// ParseTemplate parses s. A '}' inside a quoted string within an expression
// does not end the expression.
func ParseTemplate(s string) (*Template, error) {
	t := &Template{src: s}
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			t.parts = append(t.parts, tplPart{lit: lit.String()})
			lit.Reset()
		}
	}
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], "$${") {
			lit.WriteString("${")
			i += 3
			continue
		}
		if !strings.HasPrefix(s[i:], "${") {
			lit.WriteByte(s[i])
			i++
			continue
		}
		end := -1
		var quote byte
		for j := i + 2; j < len(s); j++ {
			c := s[j]
			if quote != 0 {
				if c == '\\' {
					j++
				} else if c == quote {
					quote = 0
				}
				continue
			}
			if c == '\'' || c == '"' {
				quote = c
			} else if c == '}' {
				end = j
				break
			}
		}
		if end < 0 {
			return nil, fmt.Errorf("template %q: unterminated ${ at offset %d", clip(s, 80), i)
		}
		body := strings.TrimSpace(s[i+2 : end])
		if body == "" {
			return nil, fmt.Errorf("template %q: empty ${} at offset %d", clip(s, 80), i)
		}
		e, err := ParseExpr(body)
		if err != nil {
			return nil, err
		}
		flush()
		t.parts = append(t.parts, tplPart{expr: e})
		i = end + 1
	}
	flush()
	return t, nil
}

// String returns the source text.
func (t *Template) String() string { return t.src }

// IsLiteral reports whether the template contains no expressions.
func (t *Template) IsLiteral() bool {
	for _, p := range t.parts {
		if p.expr != nil {
			return false
		}
	}
	return true
}

// Exprs returns the embedded expressions in order.
func (t *Template) Exprs() []*Expr {
	var out []*Expr
	for _, p := range t.parts {
		if p.expr != nil {
			out = append(out, p.expr)
		}
	}
	return out
}

// Eval expands the template. A template that is exactly one ${...} yields
// the expression's value with its type; otherwise the result is a string.
func (t *Template) Eval(vars map[string]any) (any, error) {
	return t.eval(&evalEnv{vars: vars})
}

func (t *Template) eval(env *evalEnv) (any, error) {
	if len(t.parts) == 1 && t.parts[0].expr != nil {
		return t.parts[0].expr.eval(env)
	}
	var b strings.Builder
	for _, p := range t.parts {
		if p.expr == nil {
			b.WriteString(p.lit)
			continue
		}
		v, err := p.expr.eval(env)
		if err != nil {
			return nil, err
		}
		b.WriteString(stringify(v))
		if b.Len() > maxResultSize {
			return nil, fmt.Errorf("template %q expands to more than %d bytes", clip(t.src, 80), maxResultSize)
		}
	}
	return b.String(), nil
}

// parseCondition parses a `when` condition written either as a bare
// expression or as a single ${...}.
func parseCondition(s string) (*Expr, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "${") && strings.HasSuffix(s, "}") {
		t, err := ParseTemplate(s)
		if err != nil {
			return nil, err
		}
		if len(t.parts) == 1 && t.parts[0].expr != nil {
			return t.parts[0].expr, nil
		}
	}
	return ParseExpr(s)
}

// valueTemplate is a compiled JSON value tree whose strings may be
// templates.
type valueTemplate interface {
	eval(env *evalEnv) (any, error)
	exprs(path string, f func(path string, e *Expr))
}

type litValue struct{ v any }

type strValue struct{ t *Template }

type listValue []valueTemplate

type mapValue struct {
	keys []string
	vals []valueTemplate
}

func compileValue(v any) (valueTemplate, error) {
	switch x := v.(type) {
	case string:
		if !strings.Contains(x, "${") {
			return litValue{x}, nil
		}
		t, err := ParseTemplate(x)
		if err != nil {
			return nil, err
		}
		if t.IsLiteral() {
			lit, _ := t.eval(&evalEnv{})
			return litValue{lit}, nil
		}
		return strValue{t}, nil
	case []any:
		out := make(listValue, len(x))
		for i, e := range x {
			c, err := compileValue(e)
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			out[i] = c
		}
		return out, nil
	case map[string]any:
		m := mapValue{keys: sortedKeys(x)}
		for _, k := range m.keys {
			c, err := compileValue(x[k])
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			m.vals = append(m.vals, c)
		}
		return m, nil
	}
	n, err := normalizeValue(v)
	if err != nil {
		return nil, err
	}
	return litValue{n}, nil
}

func (l litValue) eval(*evalEnv) (any, error)        { return deepCopy(l.v), nil }
func (l litValue) exprs(string, func(string, *Expr)) {}

func (s strValue) eval(env *evalEnv) (any, error) { return s.t.eval(env) }
func (s strValue) exprs(path string, f func(string, *Expr)) {
	for _, e := range s.t.Exprs() {
		f(path, e)
	}
}

func (l listValue) eval(env *evalEnv) (any, error) {
	out := make([]any, len(l))
	for i, e := range l {
		v, err := e.eval(env)
		if err != nil {
			return nil, fmt.Errorf("[%d]: %w", i, err)
		}
		out[i] = v
	}
	return out, nil
}

func (l listValue) exprs(path string, f func(string, *Expr)) {
	for i, e := range l {
		e.exprs(fmt.Sprintf("%s[%d]", path, i), f)
	}
}

func (m mapValue) eval(env *evalEnv) (any, error) {
	out := make(map[string]any, len(m.keys))
	for i, k := range m.keys {
		v, err := m.vals[i].eval(env)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		out[k] = v
	}
	return out, nil
}

func (m mapValue) exprs(path string, f func(string, *Expr)) {
	for i, k := range m.keys {
		m.vals[i].exprs(path+"."+k, f)
	}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
