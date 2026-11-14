package supplychain

import (
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// A strict TOML 1.0 parser covering what lockfiles use: tables, arrays of
// tables, dotted and quoted keys, all string forms, integers, floats,
// booleans, date-times (kept as strings), arrays and inline tables.
// Values decode to map[string]any, []any, string, int64, float64 and bool.

type tomlParser struct {
	file string
	s    string
	i    int
	line int
	// tables explicitly defined with [header]; keyed by pointer identity.
	defined map[uintptr]bool
	inline  map[uintptr]bool
	depth   int
}

type tomlTable = map[string]any

func parseTOML(file string, data []byte) (tomlTable, error) {
	if !utf8.Valid(data) {
		return nil, perr(file, 0, "toml: input is not valid UTF-8")
	}
	p := &tomlParser{file: file, s: strings.TrimPrefix(string(data), "\uFEFF"), line: 1,
		defined: map[uintptr]bool{}, inline: map[uintptr]bool{}}
	root := tomlTable{}
	cur := root
	for {
		p.skipWSCommentsNewlines()
		if p.i >= len(p.s) {
			return root, nil
		}
		switch {
		case strings.HasPrefix(p.s[p.i:], "[["):
			p.i += 2
			keys, err := p.key()
			if err != nil {
				return nil, err
			}
			p.skipWS()
			if !strings.HasPrefix(p.s[p.i:], "]]") {
				return nil, p.err("expected ']]'")
			}
			p.i += 2
			if cur, err = p.arrayTable(root, keys); err != nil {
				return nil, err
			}
		case p.s[p.i] == '[':
			p.i++
			keys, err := p.key()
			if err != nil {
				return nil, err
			}
			p.skipWS()
			if p.i >= len(p.s) || p.s[p.i] != ']' {
				return nil, p.err("expected ']'")
			}
			p.i++
			if cur, err = p.stdTable(root, keys); err != nil {
				return nil, err
			}
		default:
			if err := p.keyValue(cur); err != nil {
				return nil, err
			}
		}
		if err := p.endOfLine(); err != nil {
			return nil, err
		}
	}
}

func (p *tomlParser) err(msg string) error { return perr(p.file, p.line, "toml: %s", msg) }

func (p *tomlParser) skipWS() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\t') {
		p.i++
	}
}

func (p *tomlParser) skipComment() error {
	if p.i < len(p.s) && p.s[p.i] == '#' {
		for p.i < len(p.s) && p.s[p.i] != '\n' {
			c := p.s[p.i]
			if c < 0x20 && c != '\t' && c != '\r' || c == 0x7f {
				return p.err("control character in comment")
			}
			p.i++
		}
	}
	return nil
}

func (p *tomlParser) skipWSCommentsNewlines() {
	for p.i < len(p.s) {
		switch c := p.s[p.i]; {
		case c == ' ' || c == '\t' || c == '\r':
			p.i++
		case c == '\n':
			p.i++
			p.line++
		case c == '#':
			for p.i < len(p.s) && p.s[p.i] != '\n' {
				p.i++
			}
		default:
			return
		}
	}
}

func (p *tomlParser) endOfLine() error {
	p.skipWS()
	if err := p.skipComment(); err != nil {
		return err
	}
	if p.i >= len(p.s) {
		return nil
	}
	if p.s[p.i] == '\r' && p.i+1 < len(p.s) && p.s[p.i+1] == '\n' {
		p.i++
	}
	if p.s[p.i] != '\n' {
		return p.err("expected end of line")
	}
	p.i++
	p.line++
	return nil
}

func (p *tomlParser) key() ([]string, error) {
	var keys []string
	for {
		p.skipWS()
		if p.i >= len(p.s) {
			return nil, p.err("expected key")
		}
		var k string
		var err error
		switch p.s[p.i] {
		case '"':
			if strings.HasPrefix(p.s[p.i:], `"""`) {
				return nil, p.err("multi-line string cannot be a key")
			}
			k, err = p.basicString()
		case '\'':
			if strings.HasPrefix(p.s[p.i:], "'''") {
				return nil, p.err("multi-line string cannot be a key")
			}
			k, err = p.literalString()
		default:
			start := p.i
			for p.i < len(p.s) && isBareKeyChar(p.s[p.i]) {
				p.i++
			}
			if start == p.i {
				return nil, p.err("invalid key")
			}
			k = p.s[start:p.i]
		}
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
		p.skipWS()
		if p.i < len(p.s) && p.s[p.i] == '.' {
			p.i++
			continue
		}
		return keys, nil
	}
}

func isBareKeyChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// descend walks/creates intermediate tables for a header or dotted key.
func (p *tomlParser) descend(t tomlTable, keys []string, forHeader bool) (tomlTable, error) {
	for _, k := range keys {
		switch v := t[k].(type) {
		case nil:
			nt := tomlTable{}
			t[k] = nt
			t = nt
		case tomlTable:
			if p.inline[tableID(v)] {
				return nil, p.err("cannot extend inline table " + strconv.Quote(k))
			}
			t = v
		case []any:
			if !forHeader || len(v) == 0 {
				return nil, p.err("key " + strconv.Quote(k) + " is not a table")
			}
			last, ok := v[len(v)-1].(tomlTable)
			if !ok {
				return nil, p.err("key " + strconv.Quote(k) + " is a static array")
			}
			t = last
		default:
			return nil, p.err("key " + strconv.Quote(k) + " already has a value")
		}
	}
	return t, nil
}

func (p *tomlParser) stdTable(root tomlTable, keys []string) (tomlTable, error) {
	parent, err := p.descend(root, keys[:len(keys)-1], true)
	if err != nil {
		return nil, err
	}
	last := keys[len(keys)-1]
	switch v := parent[last].(type) {
	case nil:
		nt := tomlTable{}
		parent[last] = nt
		p.markDefined(nt)
		return nt, nil
	case tomlTable:
		if p.isDefined(v) {
			return nil, p.err("table " + strconv.Quote(strings.Join(keys, ".")) + " defined twice")
		}
		p.markDefined(v)
		return v, nil
	}
	return nil, p.err("key " + strconv.Quote(last) + " is not a table")
}

func (p *tomlParser) arrayTable(root tomlTable, keys []string) (tomlTable, error) {
	parent, err := p.descend(root, keys[:len(keys)-1], true)
	if err != nil {
		return nil, err
	}
	last := keys[len(keys)-1]
	nt := tomlTable{}
	p.markDefined(nt)
	switch v := parent[last].(type) {
	case nil:
		parent[last] = []any{nt}
	case []any:
		if len(v) > 0 {
			if _, ok := v[0].(tomlTable); !ok {
				return nil, p.err("cannot append to static array " + strconv.Quote(last))
			}
		}
		parent[last] = append(v, nt)
	default:
		return nil, p.err("key " + strconv.Quote(last) + " is not an array of tables")
	}
	return nt, nil
}

// Tables are identified by their map header address; every table stays
// reachable from the root for the parser's lifetime, so addresses are
// never reused while they are tracked.
func tableID(t tomlTable) uintptr { return reflect.ValueOf(t).Pointer() }

func (p *tomlParser) markDefined(t tomlTable)    { p.defined[tableID(t)] = true }
func (p *tomlParser) isDefined(t tomlTable) bool { return p.defined[tableID(t)] }

func (p *tomlParser) keyValue(t tomlTable) error {
	keys, err := p.key()
	if err != nil {
		return err
	}
	p.skipWS()
	if p.i >= len(p.s) || p.s[p.i] != '=' {
		return p.err("expected '=' after key")
	}
	p.i++
	p.skipWS()
	v, err := p.value()
	if err != nil {
		return err
	}
	parent, err := p.descend(t, keys[:len(keys)-1], false)
	if err != nil {
		return err
	}
	last := keys[len(keys)-1]
	if _, exists := parent[last]; exists {
		return p.err("duplicate key " + strconv.Quote(strings.Join(keys, ".")))
	}
	parent[last] = v
	return nil
}

func (p *tomlParser) value() (any, error) {
	p.depth++
	defer func() { p.depth-- }()
	if p.depth > 128 {
		return nil, p.err("nesting too deep")
	}
	if p.i >= len(p.s) {
		return nil, p.err("expected value")
	}
	switch c := p.s[p.i]; {
	case strings.HasPrefix(p.s[p.i:], `"""`):
		return p.multiBasicString()
	case strings.HasPrefix(p.s[p.i:], "'''"):
		return p.multiLiteralString()
	case c == '"':
		return p.basicString()
	case c == '\'':
		return p.literalString()
	case c == '[':
		return p.array()
	case c == '{':
		return p.inlineTable()
	case strings.HasPrefix(p.s[p.i:], "true"):
		p.i += 4
		return true, nil
	case strings.HasPrefix(p.s[p.i:], "false"):
		p.i += 5
		return false, nil
	}
	start := p.i
	for p.i < len(p.s) {
		c := p.s[p.i]
		if c == ',' || c == ']' || c == '}' || c == '\n' || c == '\r' || c == '#' || c == '\t' {
			break
		}
		if c == ' ' {
			// a space is allowed between date and time in a date-time
			if p.i+1 < len(p.s) && p.s[p.i+1] >= '0' && p.s[p.i+1] <= '9' && isTOMLDate(p.s[start:p.i]) {
				p.i++
				continue
			}
			break
		}
		p.i++
	}
	tok := p.s[start:p.i]
	if tok == "" {
		return nil, p.err("expected value")
	}
	return p.scalarToken(tok)
}

func isTOMLDate(s string) bool {
	return len(s) == 10 && s[4] == '-' && s[7] == '-' && isDigits(s[:4]) && isDigits(s[5:7]) && isDigits(s[8:])
}

func (p *tomlParser) scalarToken(tok string) (any, error) {
	switch tok {
	case "inf", "+inf", "-inf", "nan", "+nan", "-nan":
		f, _ := strconv.ParseFloat(strings.TrimPrefix(tok, "+"), 64)
		return f, nil
	}
	if len(tok) >= 10 && isTOMLDate(tok[:10]) || len(tok) >= 8 && tok[2] == ':' && tok[5] == ':' {
		for i := 0; i < len(tok); i++ {
			c := tok[i]
			if !(c >= '0' && c <= '9' || strings.IndexByte("-:.TtZz+ ", c) >= 0) {
				return nil, p.err("invalid date-time " + strconv.Quote(tok))
			}
		}
		return tok, nil
	}
	clean := tok
	if strings.Contains(tok, "_") {
		if strings.HasPrefix(tok, "_") || strings.HasSuffix(tok, "_") || strings.Contains(tok, "__") {
			return nil, p.err("invalid underscore in number " + strconv.Quote(tok))
		}
		clean = strings.ReplaceAll(tok, "_", "")
	}
	if len(clean) > 2 && clean[0] == '0' && (clean[1] == 'x' || clean[1] == 'o' || clean[1] == 'b') {
		base := map[byte]int{'x': 16, 'o': 8, 'b': 2}[clean[1]]
		n, err := strconv.ParseInt(clean[2:], base, 64)
		if err != nil {
			return nil, p.err("invalid integer " + strconv.Quote(tok))
		}
		return n, nil
	}
	digits := strings.TrimLeft(clean, "+-")
	if strings.ContainsAny(clean, ".eE") {
		if strings.HasPrefix(digits, ".") || strings.Contains(digits, ".e") || strings.HasSuffix(digits, ".") {
			return nil, p.err("invalid float " + strconv.Quote(tok))
		}
		f, err := strconv.ParseFloat(clean, 64)
		if err != nil {
			return nil, p.err("invalid float " + strconv.Quote(tok))
		}
		return f, nil
	}
	if len(digits) > 1 && digits[0] == '0' {
		return nil, p.err("leading zero in integer " + strconv.Quote(tok))
	}
	n, err := strconv.ParseInt(clean, 10, 64)
	if err != nil {
		return nil, p.err("invalid value " + strconv.Quote(tok))
	}
	return n, nil
}

func (p *tomlParser) basicString() (string, error) {
	p.i++ // opening quote
	var b strings.Builder
	for {
		if p.i >= len(p.s) {
			return "", p.err("unterminated string")
		}
		c := p.s[p.i]
		switch {
		case c == '"':
			p.i++
			return b.String(), nil
		case c == '\n':
			return "", p.err("newline in basic string")
		case c == '\\':
			if err := p.escape(&b); err != nil {
				return "", err
			}
		case c < 0x20 && c != '\t' || c == 0x7f:
			return "", p.err("control character in string")
		default:
			b.WriteByte(c)
			p.i++
		}
	}
}

func (p *tomlParser) escape(b *strings.Builder) error {
	p.i++ // backslash
	if p.i >= len(p.s) {
		return p.err("unterminated escape")
	}
	c := p.s[p.i]
	p.i++
	switch c {
	case 'b':
		b.WriteByte('\b')
	case 't':
		b.WriteByte('\t')
	case 'n':
		b.WriteByte('\n')
	case 'f':
		b.WriteByte('\f')
	case 'r':
		b.WriteByte('\r')
	case 'e':
		b.WriteByte(0x1b)
	case '"':
		b.WriteByte('"')
	case '\\':
		b.WriteByte('\\')
	case 'u', 'U':
		n := 4
		if c == 'U' {
			n = 8
		}
		if p.i+n > len(p.s) {
			return p.err("truncated unicode escape")
		}
		r, err := strconv.ParseUint(p.s[p.i:p.i+n], 16, 32)
		if err != nil || !utf8.ValidRune(rune(r)) {
			return p.err("invalid unicode escape")
		}
		b.WriteRune(rune(r))
		p.i += n
	default:
		return p.err("invalid escape \\" + string(c))
	}
	return nil
}

func (p *tomlParser) literalString() (string, error) {
	p.i++
	start := p.i
	for p.i < len(p.s) {
		c := p.s[p.i]
		if c == '\'' {
			s := p.s[start:p.i]
			p.i++
			return s, nil
		}
		if c == '\n' {
			return "", p.err("newline in literal string")
		}
		if c < 0x20 && c != '\t' || c == 0x7f {
			return "", p.err("control character in string")
		}
		p.i++
	}
	return "", p.err("unterminated literal string")
}

func (p *tomlParser) multiBasicString() (string, error) {
	p.i += 3
	p.trimFirstNewline()
	var b strings.Builder
	for {
		if p.i >= len(p.s) {
			return "", p.err("unterminated multi-line string")
		}
		if strings.HasPrefix(p.s[p.i:], `"""`) {
			// up to two additional quotes may belong to the content
			extra := 0
			for extra < 2 && p.i+3+extra < len(p.s) && p.s[p.i+3+extra] == '"' {
				extra++
			}
			b.WriteString(strings.Repeat(`"`, extra))
			p.i += 3 + extra
			return b.String(), nil
		}
		c := p.s[p.i]
		switch {
		case c == '\\':
			// line-ending backslash trims following whitespace
			j := p.i + 1
			for j < len(p.s) && (p.s[j] == ' ' || p.s[j] == '\t' || p.s[j] == '\r') {
				j++
			}
			if j < len(p.s) && p.s[j] == '\n' {
				p.i = j
				for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\t' || p.s[p.i] == '\n' || p.s[p.i] == '\r') {
					if p.s[p.i] == '\n' {
						p.line++
					}
					p.i++
				}
				continue
			}
			if err := p.escape(&b); err != nil {
				return "", err
			}
		case c == '\n':
			p.line++
			b.WriteByte(c)
			p.i++
		case c < 0x20 && c != '\t' && c != '\r' || c == 0x7f:
			return "", p.err("control character in string")
		default:
			b.WriteByte(c)
			p.i++
		}
	}
}

func (p *tomlParser) multiLiteralString() (string, error) {
	p.i += 3
	p.trimFirstNewline()
	start := p.i
	for p.i < len(p.s) {
		if strings.HasPrefix(p.s[p.i:], "'''") {
			extra := 0
			for extra < 2 && p.i+3+extra < len(p.s) && p.s[p.i+3+extra] == '\'' {
				extra++
			}
			s := p.s[start : p.i+extra]
			p.i += 3 + extra
			return s, nil
		}
		if p.s[p.i] == '\n' {
			p.line++
		}
		p.i++
	}
	return "", p.err("unterminated multi-line literal string")
}

func (p *tomlParser) trimFirstNewline() {
	if strings.HasPrefix(p.s[p.i:], "\r\n") {
		p.i += 2
		p.line++
	} else if strings.HasPrefix(p.s[p.i:], "\n") {
		p.i++
		p.line++
	}
}

func (p *tomlParser) array() ([]any, error) {
	p.i++ // '['
	out := []any{}
	for {
		p.skipWSCommentsNewlines()
		if p.i >= len(p.s) {
			return nil, p.err("unterminated array")
		}
		if p.s[p.i] == ']' {
			p.i++
			return out, nil
		}
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		p.skipWSCommentsNewlines()
		if p.i >= len(p.s) {
			return nil, p.err("unterminated array")
		}
		switch p.s[p.i] {
		case ',':
			p.i++
		case ']':
		default:
			return nil, p.err("expected ',' or ']' in array")
		}
	}
}

func (p *tomlParser) inlineTable() (tomlTable, error) {
	p.i++ // '{'
	t := tomlTable{}
	p.skipWS()
	if p.i < len(p.s) && p.s[p.i] == '}' {
		p.i++
		p.inline[tableID(t)] = true
		return t, nil
	}
	for {
		p.skipWS()
		if err := p.keyValue(t); err != nil {
			return nil, err
		}
		p.skipWS()
		if p.i >= len(p.s) {
			return nil, p.err("unterminated inline table")
		}
		switch p.s[p.i] {
		case ',':
			p.i++
		case '}':
			p.i++
			p.inline[tableID(t)] = true
			return t, nil
		default:
			return nil, p.err("expected ',' or '}' in inline table")
		}
	}
}

// ---- typed accessors ------------------------------------------------------

func tomlString(t tomlTable, k string) string {
	s, _ := t[k].(string)
	return s
}

func tomlTableOf(t tomlTable, k string) tomlTable {
	v, _ := t[k].(tomlTable)
	return v
}

func tomlTables(t tomlTable, k string) []tomlTable {
	arr, _ := t[k].([]any)
	out := make([]tomlTable, 0, len(arr))
	for _, e := range arr {
		if m, ok := e.(tomlTable); ok {
			out = append(out, m)
		}
	}
	return out
}

func tomlKeys(t tomlTable) []string {
	out := make([]string, 0, len(t))
	for k := range t {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
