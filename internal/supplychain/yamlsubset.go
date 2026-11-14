package supplychain

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// A small, strict YAML subset parser sufficient for pnpm lockfiles: block
// mappings and sequences, plain/single-/double-quoted scalars, flow
// mappings and sequences, literal and folded block scalars, and comments.
// Anchors, aliases, tags and multi-document streams are rejected rather
// than mis-parsed.

type yKind int

const (
	yScalar yKind = iota
	yMap
	ySeq
)

type yNode struct {
	kind   yKind
	value  string
	null   bool
	keys   []string
	fields map[string]*yNode
	items  []*yNode
	line   int
}

func (n *yNode) get(k string) *yNode {
	if n == nil || n.kind != yMap {
		return nil
	}
	return n.fields[k]
}

func (n *yNode) str() string {
	if n == nil || n.kind != yScalar {
		return ""
	}
	return n.value
}

type yLine struct {
	indent int
	text   string // content without indentation or trailing comment
	num    int
}

type yParser struct {
	file  string
	lines []yLine
	pos   int
	depth int
}

const maxYAMLDepth = 200

func parseYAMLSubset(file string, data []byte) (*yNode, error) {
	if !utf8.Valid(data) {
		return nil, perr(file, 0, "yaml: input is not valid UTF-8")
	}
	src := strings.ReplaceAll(string(data), "\r\n", "\n")
	src = strings.TrimPrefix(src, "\uFEFF")
	raw := strings.Split(src, "\n")
	p := &yParser{file: file}
	docs := 0
	for i := 0; i < len(raw); i++ {
		line := raw[i]
		num := i + 1
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		if strings.HasPrefix(trimmed, "\t") {
			return nil, perr(file, num, "yaml: tab indentation is not allowed")
		}
		content := stripYAMLComment(trimmed)
		content = strings.TrimRight(content, " \t")
		if content == "" {
			continue
		}
		if indent == 0 && (content == "---" || strings.HasPrefix(content, "--- ")) {
			docs++
			if docs > 1 || len(p.lines) > 0 {
				return nil, perr(file, num, "yaml: multi-document streams are not supported")
			}
			continue
		}
		if indent == 0 && content == "..." {
			break
		}
		if indent == 0 && strings.HasPrefix(content, "%") {
			return nil, perr(file, num, "yaml: directives are not supported")
		}
		// block scalars: capture following more-indented lines verbatim
		if isBlockScalarHeader(content) {
			p.lines = append(p.lines, yLine{indent, content, num})
			var body []string
			blockIndent := -1
			for i+1 < len(raw) {
				nl := raw[i+1]
				nt := strings.TrimLeft(nl, " ")
				if strings.TrimSpace(nl) == "" {
					body = append(body, "")
					i++
					continue
				}
				ni := len(nl) - len(nt)
				if ni <= indent {
					break
				}
				if blockIndent < 0 {
					blockIndent = ni
				}
				if ni < blockIndent {
					return nil, perr(file, i+2, "yaml: inconsistent block scalar indentation")
				}
				body = append(body, nl[blockIndent:])
				i++
			}
			for len(body) > 0 && body[len(body)-1] == "" {
				body = body[:len(body)-1]
			}
			p.lines[len(p.lines)-1].text = content + "\x00" + strings.Join(body, "\n")
			continue
		}
		p.lines = append(p.lines, yLine{indent, content, num})
	}
	if len(p.lines) == 0 {
		return &yNode{kind: yMap, fields: map[string]*yNode{}}, nil
	}
	n, err := p.parseBlock(p.lines[0].indent)
	if err != nil {
		return nil, err
	}
	if p.pos < len(p.lines) {
		return nil, perr(file, p.lines[p.pos].num, "yaml: unexpected content (bad indentation?)")
	}
	return n, nil
}

func isBlockScalarHeader(content string) bool {
	_, v, ok := splitYAMLKey(content)
	if strings.HasPrefix(content, "- ") {
		v = strings.TrimSpace(content[2:])
		if _, vv, ok2 := splitYAMLKey(v); ok2 {
			v = vv
		}
		ok = true
	}
	if !ok {
		return false
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	if v[0] != '|' && v[0] != '>' {
		return false
	}
	for _, c := range v[1:] {
		if c != '-' && c != '+' && !(c >= '1' && c <= '9') {
			return false
		}
	}
	return true
}

// stripYAMLComment removes a trailing "# comment" that is outside quotes.
func stripYAMLComment(s string) string {
	inSingle, inDouble := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inDouble:
			if c == '\\' {
				i++
			} else if c == '"' {
				inDouble = false
			}
		case inSingle:
			if c == '\'' {
				inSingle = false
			}
		case c == '"':
			inDouble = true
		case c == '\'':
			inSingle = true
		case c == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t'):
			return s[:i]
		}
	}
	return s
}

func (p *yParser) parseBlock(indent int) (*yNode, error) {
	p.depth++
	defer func() { p.depth-- }()
	if p.depth > maxYAMLDepth {
		return nil, perr(p.file, p.lines[p.pos].num, "yaml: nesting too deep")
	}
	first := p.lines[p.pos]
	if first.text == "-" || strings.HasPrefix(first.text, "- ") {
		return p.parseSeq(indent)
	}
	return p.parseMap(indent)
}

func (p *yParser) parseSeq(indent int) (*yNode, error) {
	n := &yNode{kind: ySeq, line: p.lines[p.pos].num}
	for p.pos < len(p.lines) {
		l := p.lines[p.pos]
		if l.indent < indent {
			break
		}
		if l.indent > indent {
			return nil, perr(p.file, l.num, "yaml: unexpected indentation in sequence")
		}
		if !(l.text == "-" || strings.HasPrefix(l.text, "- ")) {
			break
		}
		rest := strings.TrimLeft(strings.TrimPrefix(l.text, "-"), " ")
		if rest == "" {
			p.pos++
			if p.pos < len(p.lines) && p.lines[p.pos].indent > indent {
				child, err := p.parseBlock(p.lines[p.pos].indent)
				if err != nil {
					return nil, err
				}
				n.items = append(n.items, child)
			} else {
				n.items = append(n.items, &yNode{kind: yScalar, null: true, line: l.num})
			}
			continue
		}
		// Re-interpret "- key: v" as a mapping indented past the dash.
		childIndent := indent + (len(l.text) - len(rest))
		if _, _, isKey := splitYAMLKey(rest); isKey && !strings.ContainsRune("{[|>", rune(rest[0])) {
			p.lines[p.pos] = yLine{childIndent, rest, l.num}
			child, err := p.parseMap(childIndent)
			if err != nil {
				return nil, err
			}
			n.items = append(n.items, child)
			continue
		}
		v, err := p.parseInlineValue(rest, l.num, indent)
		if err != nil {
			return nil, err
		}
		p.pos++
		n.items = append(n.items, v)
	}
	return n, nil
}

func (p *yParser) parseMap(indent int) (*yNode, error) {
	n := &yNode{kind: yMap, fields: map[string]*yNode{}, line: p.lines[p.pos].num}
	for p.pos < len(p.lines) {
		l := p.lines[p.pos]
		if l.indent < indent {
			break
		}
		if l.indent > indent {
			return nil, perr(p.file, l.num, "yaml: unexpected indentation")
		}
		if l.text == "-" || strings.HasPrefix(l.text, "- ") {
			break
		}
		key, val, ok := splitYAMLKey(l.text)
		if !ok {
			return nil, perr(p.file, l.num, "yaml: expected 'key: value'")
		}
		k, err := yamlScalar(key, l.num, p.file)
		if err != nil {
			return nil, err
		}
		if _, dup := n.fields[k]; dup {
			return nil, perr(p.file, l.num, "yaml: duplicate key %q", k)
		}
		var child *yNode
		val = strings.TrimSpace(val)
		if val == "" {
			p.pos++
			// A nested block, or a sequence at the same indentation
			// (permitted by YAML for mapping values).
			if p.pos < len(p.lines) && (p.lines[p.pos].indent > indent ||
				(p.lines[p.pos].indent == indent && (p.lines[p.pos].text == "-" || strings.HasPrefix(p.lines[p.pos].text, "- ")))) {
				ci := p.lines[p.pos].indent
				if ci == indent {
					child, err = p.parseSeq(ci)
				} else {
					child, err = p.parseBlock(ci)
				}
				if err != nil {
					return nil, err
				}
			} else {
				child = &yNode{kind: yScalar, null: true, line: l.num}
			}
		} else {
			child, err = p.parseInlineValue(val, l.num, indent)
			if err != nil {
				return nil, err
			}
			p.pos++
		}
		n.keys = append(n.keys, k)
		n.fields[k] = child
	}
	return n, nil
}

// parseInlineValue parses a value that starts on the current line. Flow
// collections may continue over following lines; they are joined.
func (p *yParser) parseInlineValue(val string, num, indent int) (*yNode, error) {
	if val[0] == '|' || val[0] == '>' {
		hdr, content, _ := strings.Cut(val, "\x00")
		if hdr[0] == '>' {
			content = foldYAML(content)
		}
		chomp := strings.TrimLeft(hdr[1:], "123456789")
		switch {
		case strings.Contains(hdr, "-"):
		case strings.Contains(hdr, "+") || chomp == "":
			content += "\n"
		}
		return &yNode{kind: yScalar, value: content, line: num}, nil
	}
	if strings.HasPrefix(val, "&") || strings.HasPrefix(val, "*") || strings.HasPrefix(val, "!") {
		return nil, perr(p.file, num, "yaml: anchors, aliases and tags are not supported")
	}
	if val[0] == '{' || val[0] == '[' {
		full := val
		for !flowBalanced(full) {
			if p.pos+1 >= len(p.lines) || p.lines[p.pos+1].indent <= indent {
				return nil, perr(p.file, num, "yaml: unterminated flow collection")
			}
			p.pos++
			full += " " + p.lines[p.pos].text
		}
		fp := &yFlow{s: full, file: p.file, line: num}
		n, err := fp.value(0)
		if err != nil {
			return nil, err
		}
		fp.skipWS()
		if fp.i != len(fp.s) {
			return nil, perr(p.file, num, "yaml: trailing characters after flow collection")
		}
		return n, nil
	}
	s, err := yamlScalar(val, num, p.file)
	if err != nil {
		return nil, err
	}
	return &yNode{kind: yScalar, value: s, null: isYAMLNull(val), line: num}, nil
}

func foldYAML(s string) string {
	lines := strings.Split(s, "\n")
	var b strings.Builder
	for i, l := range lines {
		if i > 0 {
			if l == "" || lines[i-1] == "" {
				b.WriteByte('\n')
			} else {
				b.WriteByte(' ')
			}
		}
		b.WriteString(l)
	}
	return b.String()
}

func isYAMLNull(v string) bool { return v == "~" || v == "null" || v == "Null" || v == "NULL" }

func flowBalanced(s string) bool {
	depth := 0
	inS, inD := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inD:
			if c == '\\' {
				i++
			} else if c == '"' {
				inD = false
			}
		case inS:
			if c == '\'' {
				inS = false
			}
		case c == '"':
			inD = true
		case c == '\'':
			inS = true
		case c == '{' || c == '[':
			depth++
		case c == '}' || c == ']':
			depth--
		}
	}
	return depth <= 0 && !inS && !inD
}

// splitYAMLKey splits "key: value" honouring quoted keys. A plain key ends
// at the first ": " or at a trailing ':'.
func splitYAMLKey(s string) (key, val string, ok bool) {
	if s == "" {
		return "", "", false
	}
	if s[0] == '"' || s[0] == '\'' {
		end := closingQuote(s)
		if end < 0 {
			return "", "", false
		}
		rest := s[end+1:]
		if rest == ":" {
			return s[:end+1], "", true
		}
		if strings.HasPrefix(rest, ": ") {
			return s[:end+1], rest[2:], true
		}
		return "", "", false
	}
	if s[0] == '{' || s[0] == '[' {
		return "", "", false
	}
	if i := strings.Index(s, ": "); i >= 0 {
		return s[:i], s[i+2:], true
	}
	if strings.HasSuffix(s, ":") {
		return s[:len(s)-1], "", true
	}
	return "", "", false
}

func closingQuote(s string) int {
	q := s[0]
	for i := 1; i < len(s); i++ {
		if q == '"' && s[i] == '\\' {
			i++
			continue
		}
		if s[i] == q {
			if q == '\'' && i+1 < len(s) && s[i+1] == '\'' {
				i++
				continue
			}
			return i
		}
	}
	return -1
}

func yamlScalar(s string, num int, file string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	switch s[0] {
	case '\'':
		if len(s) < 2 || s[len(s)-1] != '\'' || closingQuote(s) != len(s)-1 {
			return "", perr(file, num, "yaml: malformed single-quoted scalar")
		}
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'"), nil
	case '"':
		if len(s) < 2 || closingQuote(s) != len(s)-1 {
			return "", perr(file, num, "yaml: malformed double-quoted scalar")
		}
		return unescapeYAMLDouble(s[1:len(s)-1], num, file)
	case '&', '*', '!':
		return "", perr(file, num, "yaml: anchors, aliases and tags are not supported")
	case '|', '>':
		return "", perr(file, num, "yaml: unexpected block scalar indicator")
	}
	if isYAMLNull(s) {
		return "", nil
	}
	return s, nil
}

func unescapeYAMLDouble(s string, num int, file string) (string, error) {
	if !strings.Contains(s, "\\") {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		i++
		if i >= len(s) {
			return "", perr(file, num, "yaml: trailing backslash")
		}
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case '0':
			b.WriteByte(0)
		case '"', '\\', '/', ' ':
			b.WriteByte(s[i])
		case 'x', 'u', 'U':
			n := map[byte]int{'x': 2, 'u': 4, 'U': 8}[s[i]]
			if i+1+n > len(s) {
				return "", perr(file, num, "yaml: truncated escape")
			}
			r, err := strconv.ParseUint(s[i+1:i+1+n], 16, 32)
			if err != nil || !utf8.ValidRune(rune(r)) {
				return "", perr(file, num, "yaml: invalid escape")
			}
			b.WriteRune(rune(r))
			i += n
		default:
			return "", perr(file, num, "yaml: unknown escape \\%c", s[i])
		}
	}
	return b.String(), nil
}

type yFlow struct {
	s     string
	i     int
	file  string
	line  int
	depth int
}

func (f *yFlow) skipWS() {
	for f.i < len(f.s) && (f.s[f.i] == ' ' || f.s[f.i] == '\t') {
		f.i++
	}
}

func (f *yFlow) err(msg string) error {
	return perr(f.file, f.line, "yaml: %s at column %d", msg, f.i+1)
}

func (f *yFlow) value(_ int) (*yNode, error) {
	f.skipWS()
	if f.i >= len(f.s) {
		return nil, f.err("unexpected end of flow collection")
	}
	switch f.s[f.i] {
	case '{':
		return f.mapping()
	case '[':
		return f.sequence()
	}
	s, err := f.scalar(false)
	if err != nil {
		return nil, err
	}
	return &yNode{kind: yScalar, value: s, line: f.line}, nil
}

func (f *yFlow) enter() error {
	f.depth++
	if f.depth > maxYAMLDepth {
		return f.err("nesting too deep")
	}
	return nil
}

func (f *yFlow) mapping() (*yNode, error) {
	if err := f.enter(); err != nil {
		return nil, err
	}
	defer func() { f.depth-- }()
	f.i++ // '{'
	n := &yNode{kind: yMap, fields: map[string]*yNode{}, line: f.line}
	for {
		f.skipWS()
		if f.i < len(f.s) && f.s[f.i] == '}' {
			f.i++
			return n, nil
		}
		k, err := f.scalar(true)
		if err != nil {
			return nil, err
		}
		f.skipWS()
		var v *yNode
		if f.i < len(f.s) && f.s[f.i] == ':' {
			f.i++
			f.skipWS()
			if f.i < len(f.s) && (f.s[f.i] == ',' || f.s[f.i] == '}') {
				v = &yNode{kind: yScalar, null: true, line: f.line}
			} else if v, err = f.value(0); err != nil {
				return nil, err
			}
		} else {
			v = &yNode{kind: yScalar, null: true, line: f.line}
		}
		if _, dup := n.fields[k]; dup {
			return nil, f.err("duplicate key " + strconv.Quote(k))
		}
		n.keys = append(n.keys, k)
		n.fields[k] = v
		f.skipWS()
		if f.i >= len(f.s) {
			return nil, f.err("unterminated flow mapping")
		}
		switch f.s[f.i] {
		case ',':
			f.i++
		case '}':
		default:
			return nil, f.err("expected ',' or '}'")
		}
	}
}

func (f *yFlow) sequence() (*yNode, error) {
	if err := f.enter(); err != nil {
		return nil, err
	}
	defer func() { f.depth-- }()
	f.i++ // '['
	n := &yNode{kind: ySeq, line: f.line}
	for {
		f.skipWS()
		if f.i < len(f.s) && f.s[f.i] == ']' {
			f.i++
			return n, nil
		}
		v, err := f.value(0)
		if err != nil {
			return nil, err
		}
		n.items = append(n.items, v)
		f.skipWS()
		if f.i >= len(f.s) {
			return nil, f.err("unterminated flow sequence")
		}
		switch f.s[f.i] {
		case ',':
			f.i++
		case ']':
		default:
			return nil, f.err("expected ',' or ']'")
		}
	}
}

// scalar reads a flow scalar. Keys end at ':' followed by space or a flow
// indicator; values end at ',', '}' or ']'.
func (f *yFlow) scalar(isKey bool) (string, error) {
	f.skipWS()
	if f.i >= len(f.s) {
		return "", f.err("expected scalar")
	}
	if c := f.s[f.i]; c == '"' || c == '\'' {
		end := closingQuote(f.s[f.i:])
		if end < 0 {
			return "", f.err("unterminated quoted scalar")
		}
		raw := f.s[f.i : f.i+end+1]
		f.i += end + 1
		return yamlScalar(raw, f.line, f.file)
	}
	start := f.i
	for f.i < len(f.s) {
		c := f.s[f.i]
		if c == ',' || c == '}' || c == ']' || c == '{' || c == '[' {
			break
		}
		if c == ':' && (f.i+1 == len(f.s) || f.s[f.i+1] == ' ' || f.s[f.i+1] == ',' || f.s[f.i+1] == '}' || f.s[f.i+1] == ']') {
			break
		}
		f.i++
	}
	raw := strings.TrimSpace(f.s[start:f.i])
	if raw == "" && isKey {
		return "", f.err("empty key")
	}
	return yamlScalar(raw, f.line, f.file)
}
