package workflow

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Expression limits. They bound parse and evaluation cost for untrusted
// specs.
const (
	MaxExprLen   = 8192
	maxExprDepth = 64
)

// ExprError is a syntax or evaluation error in an expression.
type ExprError struct {
	Expr string
	Pos  int
	Msg  string
}

func (e *ExprError) Error() string {
	src := e.Expr
	if len(src) > 120 {
		src = src[:117] + "..."
	}
	return fmt.Sprintf("expression %q: at offset %d: %s", src, e.Pos, e.Msg)
}

// Expr is a parsed expression. It is immutable and safe for concurrent use.
type Expr struct {
	src  string
	root node
}

// Reference is a variable path an expression reads, such as
// steps.build.outputs.bin. Path holds the static member names after Root up
// to the first dynamic index.
type Reference struct {
	Root string
	Path []string
	Pos  int
}

func (r Reference) String() string {
	if len(r.Path) == 0 {
		return r.Root
	}
	return r.Root + "." + strings.Join(r.Path, ".")
}

// ParseExpr parses an expression.
//
// Grammar, loosest binding first:
//
//	or      = and { "||" and }
//	and     = eq { "&&" eq }
//	eq      = rel { ("==" | "!=") rel }
//	rel     = add { ("<" | "<=" | ">" | ">=" | "in") add }
//	add     = mul { ("+" | "-") mul }
//	mul     = unary { ("*" | "/" | "%") unary }
//	unary   = ("!" | "-") unary | postfix
//	postfix = primary { "." ident | "[" or "]" }
//	primary = number | string | true | false | null | ident
//	        | ident "(" [ or { "," or } ] ")" | "(" or ")"
//	        | "[" [ or { "," or } ] "]"
//
// Strings are single- or double-quoted with \n \t \r \\ \' \" and \uXXXX
// escapes. Functions are a fixed set; see the package documentation.
func ParseExpr(src string) (*Expr, error) {
	if len(src) > MaxExprLen {
		return nil, &ExprError{Expr: src, Msg: fmt.Sprintf("expression is %d bytes; the limit is %d", len(src), MaxExprLen)}
	}
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{src: src, toks: toks}
	root, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t.kind != tkEOF {
		return nil, p.errf(t.pos, "unexpected %s", t.describe())
	}
	return &Expr{src: src, root: root}, nil
}

// String returns the source text.
func (e *Expr) String() string { return e.src }

// Eval evaluates the expression against vars, whose keys are the root
// identifiers. Status functions (success, failure, always) are unavailable.
func (e *Expr) Eval(vars map[string]any) (any, error) {
	return e.eval(&evalEnv{vars: vars})
}

func (e *Expr) eval(env *evalEnv) (v any, err error) {
	env.src = e.src
	return e.root.eval(env)
}

// Refs returns the variable paths the expression reads, in source order.
func (e *Expr) Refs() []Reference {
	var out []Reference
	collectRefs(e.root, &out)
	return out
}

// Calls returns the names of the functions the expression calls, in source
// order, with duplicates.
func (e *Expr) Calls() []string {
	var out []string
	walk(e.root, func(n node) {
		if c, ok := n.(*callNode); ok {
			out = append(out, c.fn.name)
		}
	})
	return out
}

// ---- lexer ----

type tokKind uint8

const (
	tkEOF tokKind = iota
	tkIdent
	tkNumber
	tkString
	tkLParen
	tkRParen
	tkLBrack
	tkRBrack
	tkComma
	tkDot
	tkNot
	tkAnd
	tkOr
	tkEq
	tkNe
	tkLt
	tkLe
	tkGt
	tkGe
	tkPlus
	tkMinus
	tkStar
	tkSlash
	tkPercent
)

type token struct {
	kind tokKind
	text string
	num  float64
	pos  int
}

func (t token) describe() string {
	switch t.kind {
	case tkEOF:
		return "end of expression"
	case tkString:
		return "string " + strconv.Quote(t.text)
	}
	return strconv.Quote(t.text)
}

var punct = []struct {
	s string
	k tokKind
}{
	{"&&", tkAnd}, {"||", tkOr}, {"==", tkEq}, {"!=", tkNe}, {"<=", tkLe}, {">=", tkGe},
	{"<", tkLt}, {">", tkGt}, {"!", tkNot}, {"+", tkPlus}, {"-", tkMinus}, {"*", tkStar},
	{"/", tkSlash}, {"%", tkPercent}, {"(", tkLParen}, {")", tkRParen}, {"[", tkLBrack},
	{"]", tkRBrack}, {",", tkComma}, {".", tkDot},
}

func isIdentStart(c byte) bool { return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isDigit(c byte) bool      { return c >= '0' && c <= '9' }
func isIdentChar(c byte) bool  { return isIdentStart(c) || isDigit(c) }

func lex(src string) ([]token, error) {
	var toks []token
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case isIdentStart(c):
			j := i + 1
			for j < len(src) && isIdentChar(src[j]) {
				j++
			}
			toks = append(toks, token{kind: tkIdent, text: src[i:j], pos: i})
			i = j
		case isDigit(c):
			j := i
			for j < len(src) && isDigit(src[j]) {
				j++
			}
			if j+1 < len(src) && src[j] == '.' && isDigit(src[j+1]) {
				j++
				for j < len(src) && isDigit(src[j]) {
					j++
				}
			}
			if j < len(src) && (src[j] == 'e' || src[j] == 'E') {
				k := j + 1
				if k < len(src) && (src[k] == '+' || src[k] == '-') {
					k++
				}
				if k < len(src) && isDigit(src[k]) {
					for k < len(src) && isDigit(src[k]) {
						k++
					}
					j = k
				}
			}
			if j < len(src) && isIdentStart(src[j]) {
				return nil, &ExprError{Expr: src, Pos: j, Msg: "invalid number literal"}
			}
			f, err := strconv.ParseFloat(src[i:j], 64)
			if err != nil || math.IsInf(f, 0) {
				return nil, &ExprError{Expr: src, Pos: i, Msg: "invalid number literal " + strconv.Quote(src[i:j])}
			}
			toks = append(toks, token{kind: tkNumber, text: src[i:j], num: f, pos: i})
			i = j
		case c == '"' || c == '\'':
			s, n, err := lexString(src, i)
			if err != nil {
				return nil, err
			}
			toks = append(toks, token{kind: tkString, text: s, pos: i})
			i += n
		default:
			matched := false
			for _, p := range punct {
				if strings.HasPrefix(src[i:], p.s) {
					toks = append(toks, token{kind: p.k, text: p.s, pos: i})
					i += len(p.s)
					matched = true
					break
				}
			}
			if !matched {
				r, _ := utf8.DecodeRuneInString(src[i:])
				msg := fmt.Sprintf("unexpected character %q", r)
				switch c {
				case '=':
					msg += " (use == to compare)"
				case '&', '|':
					msg += " (use && or ||)"
				}
				return nil, &ExprError{Expr: src, Pos: i, Msg: msg}
			}
		}
	}
	return append(toks, token{kind: tkEOF, pos: len(src)}), nil
}

// lexString scans a quoted string starting at src[start] and returns its
// value and the number of bytes consumed.
func lexString(src string, start int) (string, int, error) {
	q := src[start]
	var b strings.Builder
	i := start + 1
	for i < len(src) {
		c := src[i]
		switch {
		case c == q:
			return b.String(), i + 1 - start, nil
		case c == '\\':
			if i+1 >= len(src) {
				return "", 0, &ExprError{Expr: src, Pos: i, Msg: "unterminated escape"}
			}
			switch e := src[i+1]; e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '\\', '\'', '"':
				b.WriteByte(e)
			case 'u':
				if i+6 > len(src) {
					return "", 0, &ExprError{Expr: src, Pos: i, Msg: "short \\u escape"}
				}
				n, err := strconv.ParseUint(src[i+2:i+6], 16, 32)
				if err != nil {
					return "", 0, &ExprError{Expr: src, Pos: i, Msg: "invalid \\u escape"}
				}
				b.WriteRune(rune(n))
				i += 4
			default:
				return "", 0, &ExprError{Expr: src, Pos: i, Msg: fmt.Sprintf("unknown escape \\%c", e)}
			}
			i += 2
		default:
			b.WriteByte(c)
			i++
		}
	}
	return "", 0, &ExprError{Expr: src, Pos: start, Msg: "unterminated string"}
}

// ---- parser ----

type parser struct {
	src   string
	toks  []token
	i     int
	depth int
}

func (p *parser) peek() token { return p.toks[p.i] }

func (p *parser) next() token {
	t := p.toks[p.i]
	if t.kind != tkEOF {
		p.i++
	}
	return t
}

func (p *parser) errf(pos int, format string, args ...any) error {
	return &ExprError{Expr: p.src, Pos: pos, Msg: fmt.Sprintf(format, args...)}
}

func (p *parser) enter(pos int) error {
	p.depth++
	if p.depth > maxExprDepth {
		return p.errf(pos, "expression nested deeper than %d levels", maxExprDepth)
	}
	return nil
}

func (p *parser) leave() { p.depth-- }

func (p *parser) parseExpr() (node, error) {
	if err := p.enter(p.peek().pos); err != nil {
		return nil, err
	}
	defer p.leave()
	return p.parseOr()
}

func (p *parser) parseBinary(ops map[tokKind]string, kw string, next func(*parser) (node, error)) (node, error) {
	l, err := next(p)
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		op, ok := ops[t.kind]
		if !ok && kw != "" && t.kind == tkIdent && t.text == kw {
			op, ok = kw, true
		}
		if !ok {
			return l, nil
		}
		p.next()
		r, err := next(p)
		if err != nil {
			return nil, err
		}
		l = &binaryNode{pos: t.pos, op: op, l: l, r: r}
	}
}

var (
	orOps  = map[tokKind]string{tkOr: "||"}
	andOps = map[tokKind]string{tkAnd: "&&"}
	eqOps  = map[tokKind]string{tkEq: "==", tkNe: "!="}
	relOps = map[tokKind]string{tkLt: "<", tkLe: "<=", tkGt: ">", tkGe: ">="}
	addOps = map[tokKind]string{tkPlus: "+", tkMinus: "-"}
	mulOps = map[tokKind]string{tkStar: "*", tkSlash: "/", tkPercent: "%"}
)

func (p *parser) parseOr() (node, error)  { return p.parseBinary(orOps, "", (*parser).parseAnd) }
func (p *parser) parseAnd() (node, error) { return p.parseBinary(andOps, "", (*parser).parseEq) }
func (p *parser) parseEq() (node, error)  { return p.parseBinary(eqOps, "", (*parser).parseRel) }
func (p *parser) parseRel() (node, error) { return p.parseBinary(relOps, "in", (*parser).parseAdd) }
func (p *parser) parseAdd() (node, error) { return p.parseBinary(addOps, "", (*parser).parseMul) }
func (p *parser) parseMul() (node, error) {
	return p.parseBinary(mulOps, "", (*parser).parseUnary)
}

func (p *parser) parseUnary() (node, error) {
	t := p.peek()
	if t.kind != tkNot && t.kind != tkMinus {
		return p.parsePostfix()
	}
	p.next()
	if err := p.enter(t.pos); err != nil {
		return nil, err
	}
	defer p.leave()
	x, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	if t.kind == tkMinus {
		if lit, ok := x.(*litNode); ok {
			if f, ok := lit.v.(float64); ok {
				return &litNode{pos: t.pos, v: -f}, nil
			}
		}
	}
	return &unaryNode{pos: t.pos, op: t.text, x: x}, nil
}

func (p *parser) parsePostfix() (node, error) {
	x, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		switch t.kind {
		case tkDot:
			p.next()
			name := p.next()
			if name.kind != tkIdent {
				return nil, p.errf(name.pos, "expected a field name after '.', found %s", name.describe())
			}
			x = &memberNode{pos: name.pos, x: x, name: name.text}
		case tkLBrack:
			p.next()
			idx, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if c := p.next(); c.kind != tkRBrack {
				return nil, p.errf(c.pos, "expected ']', found %s", c.describe())
			}
			x = &indexNode{pos: t.pos, x: x, idx: idx}
		default:
			return x, nil
		}
	}
}

func (p *parser) parsePrimary() (node, error) {
	t := p.next()
	switch t.kind {
	case tkNumber:
		return &litNode{pos: t.pos, v: t.num}, nil
	case tkString:
		return &litNode{pos: t.pos, v: t.text}, nil
	case tkLParen:
		x, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if c := p.next(); c.kind != tkRParen {
			return nil, p.errf(c.pos, "expected ')', found %s", c.describe())
		}
		return x, nil
	case tkLBrack:
		elems, err := p.parseList(tkRBrack)
		if err != nil {
			return nil, err
		}
		return &listNode{pos: t.pos, elems: elems}, nil
	case tkIdent:
		switch t.text {
		case "true":
			return &litNode{pos: t.pos, v: true}, nil
		case "false":
			return &litNode{pos: t.pos, v: false}, nil
		case "null":
			return &litNode{pos: t.pos, v: nil}, nil
		case "in":
			return nil, p.errf(t.pos, "unexpected keyword \"in\"")
		}
		if p.peek().kind == tkLParen {
			p.next()
			fn, ok := builtinFuncs[t.text]
			if !ok {
				return nil, p.errf(t.pos, "unknown function %q", t.text)
			}
			args, err := p.parseList(tkRParen)
			if err != nil {
				return nil, err
			}
			if len(args) < fn.min || fn.max >= 0 && len(args) > fn.max {
				return nil, p.errf(t.pos, "%s() takes %s, got %d", fn.name, arityText(fn.min, fn.max), len(args))
			}
			return &callNode{pos: t.pos, fn: fn, args: args}, nil
		}
		return &identNode{pos: t.pos, name: t.text}, nil
	}
	return nil, p.errf(t.pos, "unexpected %s", t.describe())
}

// parseList parses comma-separated expressions up to the closing token,
// which it consumes. A trailing comma is not allowed.
func (p *parser) parseList(end tokKind) ([]node, error) {
	var out []node
	if p.peek().kind == end {
		p.next()
		return out, nil
	}
	for {
		x, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		out = append(out, x)
		t := p.next()
		switch t.kind {
		case end:
			return out, nil
		case tkComma:
		default:
			return nil, p.errf(t.pos, "expected ',' or closing bracket, found %s", t.describe())
		}
	}
}

func arityText(minArgs, maxArgs int) string {
	switch {
	case maxArgs < 0:
		return fmt.Sprintf("at least %d argument(s)", minArgs)
	case minArgs == maxArgs:
		return fmt.Sprintf("%d argument(s)", minArgs)
	}
	return fmt.Sprintf("%d to %d arguments", minArgs, maxArgs)
}

// ---- AST ----

// depSummary describes the terminal states of a step's dependencies, for
// the status functions in `when` conditions.
type depSummary struct {
	total, succeeded, failed, skipped, cancelled int
}

func (d *depSummary) allSucceeded() bool { return d.succeeded == d.total }

type evalEnv struct {
	vars map[string]any
	deps *depSummary
	src  string
}

func (env *evalEnv) errf(pos int, format string, args ...any) error {
	return &ExprError{Expr: env.src, Pos: pos, Msg: fmt.Sprintf(format, args...)}
}

type node interface {
	eval(env *evalEnv) (any, error)
	children() []node
}

type litNode struct {
	pos int
	v   any
}

type identNode struct {
	pos  int
	name string
}

type memberNode struct {
	pos  int
	x    node
	name string
}

type indexNode struct {
	pos    int
	x, idx node
}

type unaryNode struct {
	pos int
	op  string
	x   node
}

type binaryNode struct {
	pos  int
	op   string
	l, r node
}

type callNode struct {
	pos  int
	fn   *builtinFunc
	args []node
}

type listNode struct {
	pos   int
	elems []node
}

func (n *litNode) children() []node    { return nil }
func (n *identNode) children() []node  { return nil }
func (n *memberNode) children() []node { return []node{n.x} }
func (n *indexNode) children() []node  { return []node{n.x, n.idx} }
func (n *unaryNode) children() []node  { return []node{n.x} }
func (n *binaryNode) children() []node { return []node{n.l, n.r} }
func (n *callNode) children() []node   { return n.args }
func (n *listNode) children() []node   { return n.elems }

func walk(n node, f func(node)) {
	f(n)
	for _, c := range n.children() {
		walk(c, f)
	}
}

// collectRefs records the variable paths n reads.
func collectRefs(n node, out *[]Reference) {
	switch n.(type) {
	case *identNode, *memberNode, *indexNode:
	default:
		for _, c := range n.children() {
			collectRefs(c, out)
		}
		return
	}
	// Walk the access chain from the outermost access inward. Names are
	// collected outermost first; a dynamic index discards the names
	// collected so far because they sit behind it.
	var names []string
	cur := n
	for {
		switch c := cur.(type) {
		case *memberNode:
			names = append(names, c.name)
			cur = c.x
			continue
		case *indexNode:
			collectRefs(c.idx, out)
			if lit, ok := c.idx.(*litNode); ok {
				if s, ok := lit.v.(string); ok {
					names = append(names, s)
					cur = c.x
					continue
				}
			}
			names = names[:0]
			cur = c.x
			continue
		case *identNode:
			path := make([]string, len(names))
			for i, s := range names {
				path[len(names)-1-i] = s
			}
			*out = append(*out, Reference{Root: c.name, Path: path, Pos: c.pos})
		default:
			collectRefs(cur, out)
		}
		return
	}
}

func (n *litNode) eval(*evalEnv) (any, error) { return n.v, nil }

func (n *identNode) eval(env *evalEnv) (any, error) {
	v, ok := env.vars[n.name]
	if !ok {
		return nil, env.errf(n.pos, "unknown identifier %q", n.name)
	}
	return v, nil
}

func (n *memberNode) eval(env *evalEnv) (any, error) {
	x, err := n.x.eval(env)
	if err != nil {
		return nil, err
	}
	return member(env, n.pos, x, n.name)
}

func member(env *evalEnv, pos int, x any, name string) (any, error) {
	switch m := x.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		return m[name], nil
	}
	return nil, env.errf(pos, "cannot read field %q of a %s", name, typeName(x))
}

func (n *indexNode) eval(env *evalEnv) (any, error) {
	x, err := n.x.eval(env)
	if err != nil {
		return nil, err
	}
	idx, err := n.idx.eval(env)
	if err != nil {
		return nil, err
	}
	switch c := x.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		s, ok := idx.(string)
		if !ok {
			return nil, env.errf(n.pos, "a map index must be a string, not a %s", typeName(idx))
		}
		return c[s], nil
	case []any:
		f, ok := toNumber(idx)
		if !ok || f != math.Trunc(f) {
			return nil, env.errf(n.pos, "a list index must be an integer, not %s", describeValue(idx))
		}
		if f < 0 || f >= float64(len(c)) {
			return nil, nil
		}
		return c[int(f)], nil
	}
	return nil, env.errf(n.pos, "cannot index a %s", typeName(x))
}

func (n *unaryNode) eval(env *evalEnv) (any, error) {
	x, err := n.x.eval(env)
	if err != nil {
		return nil, err
	}
	if n.op == "!" {
		return !truthy(x), nil
	}
	f, ok := toNumber(x)
	if !ok {
		return nil, env.errf(n.pos, "cannot negate a %s", typeName(x))
	}
	return -f, nil
}

func (n *binaryNode) eval(env *evalEnv) (any, error) {
	l, err := n.l.eval(env)
	if err != nil {
		return nil, err
	}
	switch n.op {
	case "&&":
		if !truthy(l) {
			return false, nil
		}
		r, err := n.r.eval(env)
		if err != nil {
			return nil, err
		}
		return truthy(r), nil
	case "||":
		if truthy(l) {
			return true, nil
		}
		r, err := n.r.eval(env)
		if err != nil {
			return nil, err
		}
		return truthy(r), nil
	}
	r, err := n.r.eval(env)
	if err != nil {
		return nil, err
	}
	switch n.op {
	case "==":
		return valuesEqual(l, r), nil
	case "!=":
		return !valuesEqual(l, r), nil
	case "<", "<=", ">", ">=":
		c, err := compareOrdered(l, r)
		if err != nil {
			return nil, env.errf(n.pos, "%s: %v", n.op, err)
		}
		switch n.op {
		case "<":
			return c < 0, nil
		case "<=":
			return c <= 0, nil
		case ">":
			return c > 0, nil
		}
		return c >= 0, nil
	case "in":
		return membership(env, n.pos, l, r)
	case "+":
		if ls, ok := l.(string); ok {
			if rs, ok := r.(string); ok {
				return ls + rs, nil
			}
		}
		if ll, ok := l.([]any); ok {
			if rl, ok := r.([]any); ok {
				out := make([]any, 0, len(ll)+len(rl))
				return append(append(out, ll...), rl...), nil
			}
		}
	}
	lf, lok := toNumber(l)
	rf, rok := toNumber(r)
	if !lok || !rok {
		return nil, env.errf(n.pos, "cannot apply %s to a %s and a %s", n.op, typeName(l), typeName(r))
	}
	switch n.op {
	case "+":
		return lf + rf, nil
	case "-":
		return lf - rf, nil
	case "*":
		return lf * rf, nil
	case "/":
		if rf == 0 {
			return nil, env.errf(n.pos, "division by zero")
		}
		return lf / rf, nil
	case "%":
		if rf == 0 {
			return nil, env.errf(n.pos, "modulo by zero")
		}
		return math.Mod(lf, rf), nil
	}
	return nil, env.errf(n.pos, "unknown operator %s", n.op)
}

func membership(env *evalEnv, pos int, l, r any) (any, error) {
	switch c := r.(type) {
	case nil:
		return false, nil
	case []any:
		for _, e := range c {
			if valuesEqual(l, e) {
				return true, nil
			}
		}
		return false, nil
	case string:
		s, ok := l.(string)
		if !ok {
			return nil, env.errf(pos, "in: left side must be a string to search a string, not a %s", typeName(l))
		}
		return strings.Contains(c, s), nil
	case map[string]any:
		s, ok := l.(string)
		if !ok {
			return nil, env.errf(pos, "in: left side must be a string to search a map, not a %s", typeName(l))
		}
		_, found := c[s]
		return found, nil
	}
	return nil, env.errf(pos, "in: cannot search a %s", typeName(r))
}

func (n *callNode) eval(env *evalEnv) (any, error) {
	args := make([]any, len(n.args))
	for i, a := range n.args {
		v, err := a.eval(env)
		if err != nil {
			return nil, err
		}
		args[i] = v
	}
	v, err := n.fn.fn(env, args)
	if err != nil {
		if _, ok := err.(*ExprError); ok {
			return nil, err
		}
		return nil, env.errf(n.pos, "%s(): %v", n.fn.name, err)
	}
	return v, nil
}

func (n *listNode) eval(env *evalEnv) (any, error) {
	out := make([]any, len(n.elems))
	for i, e := range n.elems {
		v, err := e.eval(env)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}
