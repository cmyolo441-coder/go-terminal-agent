package openai

import (
	"bytes"
	"encoding/json"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

// chunkDecoder decodes streamed chunks with few allocations. It handles
// the shapes servers actually send, exactly as encoding/json would; for
// anything unusual (keys differing only in case, duplicate keys, invalid
// UTF-8, type mismatches, usage objects, x_groq, errors, full messages) it
// gives up and the caller falls back to encoding/json, so the result is
// always what json.Unmarshal produces.
//
// Strings that repeat in every chunk (id, model) are reused, and the
// decoded chunk aliases data (raw JSON fields) and decoder scratch space:
// it is valid only until the next decode.
type chunkDecoder struct {
	c       chunk
	choices [1]chunkChoice
	calls   [4]deltaToolCall
	ints    [4]int
	nints   int
	// last decoded id and model, reused when they repeat
	id, model string
}

// decode decodes data into the decoder's chunk. ok is false when the
// fast path does not apply; the chunk is then unspecified.
func (d *chunkDecoder) decode(data []byte) (c *chunk, ok bool) {
	d.c = chunk{}
	d.nints = 0
	p := jsonParser{b: data}
	p.ws()
	if !p.object(func(key []byte) bool {
		switch string(key) {
		case "id":
			s, ok := p.str(d.id)
			d.c.ID, d.id = s, s
			return ok
		case "model":
			s, ok := p.str(d.model)
			d.c.Model, d.model = s, s
			return ok
		case "choices":
			return d.choicesValue(&p)
		case "usage":
			return p.null() // anything else goes to encoding/json
		case "error":
			if p.null() {
				d.c.Error = json.RawMessage("null") // as json.RawMessage decodes null
				return true
			}
			return false
		case "x_groq":
			return false
		}
		return p.unknown(key, "id", "model", "choices", "usage", "x_groq", "error")
	}) {
		return nil, false
	}
	p.ws()
	if p.i != len(p.b) || p.bad {
		return nil, false
	}
	return &d.c, true
}

func (d *chunkDecoder) choicesValue(p *jsonParser) bool {
	if p.null() {
		return true // a null slice stays nil
	}
	d.c.Choices = d.choices[:0]
	return p.array(func() bool {
		if len(d.c.Choices) == cap(d.c.Choices) {
			return false // more than one choice: not worth a fast path
		}
		d.c.Choices = append(d.c.Choices, chunkChoice{})
		ch := &d.c.Choices[len(d.c.Choices)-1]
		if p.null() {
			return true
		}
		return p.object(func(key []byte) bool {
			switch string(key) {
			case "index":
				n, ok := p.int()
				ch.Index = n
				return ok
			case "delta":
				return d.deltaValue(p, &ch.Delta)
			case "finish_reason":
				s, ok := p.str("")
				ch.FinishReason = s
				return ok
			case "message":
				return false
			}
			return p.unknown(key, "index", "delta", "message", "finish_reason")
		})
	})
}

func (d *chunkDecoder) deltaValue(p *jsonParser, dl *delta) bool {
	if p.null() {
		return true
	}
	return p.object(func(key []byte) bool {
		var ok bool
		switch string(key) {
		case "content":
			dl.Content, ok = p.str("")
		case "reasoning_content":
			dl.ReasoningContent, ok = p.str("")
		case "reasoning":
			dl.Reasoning, ok = p.str("")
		case "refusal":
			dl.Refusal, ok = p.str("")
		case "tool_calls":
			ok = d.toolCallsValue(p, dl)
		default:
			ok = p.unknown(key, "content", "reasoning_content", "reasoning", "refusal", "tool_calls")
		}
		return ok
	})
}

func (d *chunkDecoder) toolCallsValue(p *jsonParser, dl *delta) bool {
	if p.null() {
		return true
	}
	dl.ToolCalls = d.calls[:0]
	return p.array(func() bool {
		if len(dl.ToolCalls) == cap(dl.ToolCalls) {
			dl.ToolCalls = append([]deltaToolCall(nil), dl.ToolCalls...) // rare: many calls in one chunk
		}
		dl.ToolCalls = append(dl.ToolCalls, deltaToolCall{})
		tc := &dl.ToolCalls[len(dl.ToolCalls)-1]
		if p.null() {
			return true
		}
		return p.object(func(key []byte) bool {
			switch string(key) {
			case "index":
				if p.null() {
					tc.Index = nil
					return true
				}
				n, ok := p.int()
				if d.nints < len(d.ints) {
					d.ints[d.nints] = n
					tc.Index = &d.ints[d.nints]
					d.nints++
				} else {
					v := n
					tc.Index = &v
				}
				return ok
			case "id":
				var ok bool
				tc.ID, ok = p.str("")
				return ok
			case "function":
				if p.null() {
					return true
				}
				return p.object(func(key []byte) bool {
					switch string(key) {
					case "name":
						var ok bool
						tc.Function.Name, ok = p.str("")
						return ok
					case "arguments":
						raw, ok := p.raw()
						tc.Function.Arguments = raw
						return ok
					}
					return p.unknown(key, "name", "arguments")
				})
			}
			return p.unknown(key, "index", "id", "function")
		})
	})
}

// jsonParser is a strict JSON reader over a byte slice. Every method
// returns false on input it does not accept; bad records a syntax error.
type jsonParser struct {
	b     []byte
	i     int
	bad   bool
	depth int // nesting of objects and arrays
}

func (p *jsonParser) ws() {
	for p.i < len(p.b) {
		switch p.b[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

// lit consumes a literal such as null.
func (p *jsonParser) lit(s string) bool {
	if len(p.b)-p.i >= len(s) && string(p.b[p.i:p.i+len(s)]) == s {
		p.i += len(s)
		return true
	}
	return false
}

// null consumes a null value if one is next.
func (p *jsonParser) null() bool {
	p.ws()
	if p.lit("null") {
		return true
	}
	return false
}

// object reads an object, calling field for each key with the parser at
// the value. Keys with escapes and duplicate keys are refused.
func (p *jsonParser) object(field func(key []byte) bool) bool {
	p.ws()
	if p.i >= len(p.b) || p.b[p.i] != '{' {
		return false
	}
	p.depth++
	if p.depth > 64 {
		return false
	}
	p.i++
	var seen [16][]byte
	nseen := 0
	p.ws()
	if p.i < len(p.b) && p.b[p.i] == '}' {
		p.i++
		p.depth--
		return true
	}
	for {
		p.ws()
		key, ok := p.rawKey()
		if !ok {
			return false
		}
		for _, k := range seen[:nseen] {
			if bytes.Equal(k, key) {
				return false // duplicate key: encoding/json decides
			}
		}
		if nseen == len(seen) {
			return false
		}
		seen[nseen] = key
		nseen++
		p.ws()
		if p.i >= len(p.b) || p.b[p.i] != ':' {
			return false
		}
		p.i++
		p.ws()
		if !field(key) {
			return false
		}
		p.ws()
		if p.i >= len(p.b) {
			return false
		}
		switch p.b[p.i] {
		case ',':
			p.i++
		case '}':
			p.i++
			p.depth--
			return true
		default:
			return false
		}
	}
}

// array reads an array, calling elem with the parser at each element.
func (p *jsonParser) array(elem func() bool) bool {
	p.ws()
	if p.i >= len(p.b) || p.b[p.i] != '[' {
		return false
	}
	p.depth++
	if p.depth > 64 {
		return false
	}
	p.i++
	p.ws()
	if p.i < len(p.b) && p.b[p.i] == ']' {
		p.i++
		p.depth--
		return true
	}
	for {
		p.ws()
		if !elem() {
			return false
		}
		p.ws()
		if p.i >= len(p.b) {
			return false
		}
		switch p.b[p.i] {
		case ',':
			p.i++
		case ']':
			p.i++
			p.depth--
			return true
		default:
			return false
		}
	}
}

// rawKey reads an object key without escapes.
func (p *jsonParser) rawKey() ([]byte, bool) {
	if p.i >= len(p.b) || p.b[p.i] != '"' {
		return nil, false
	}
	start := p.i + 1
	for j := start; j < len(p.b); j++ {
		c := p.b[j]
		if c == '"' {
			p.i = j + 1
			return p.b[start:j], true
		}
		if c == '\\' || c < ' ' || c >= utf8.RuneSelf {
			return nil, false
		}
	}
	return nil, false
}

// unknown skips the value of a key that is not a field, provided the key
// cannot match any of fields case-insensitively, as encoding/json would.
func (p *jsonParser) unknown(key []byte, fields ...string) bool {
	for _, f := range fields {
		if bytes.EqualFold(key, []byte(f)) {
			return false
		}
	}
	return p.skip()
}

// skip validates and skips any value.
func (p *jsonParser) skip() bool {
	p.ws()
	if p.i >= len(p.b) {
		return false
	}
	switch c := p.b[p.i]; {
	case c == '{':
		return p.object(func([]byte) bool { return p.skip() })
	case c == '[':
		return p.array(p.skip)
	case c == '"':
		_, ok := p.str("")
		return ok
	case c == 't':
		return p.lit("true")
	case c == 'f':
		return p.lit("false")
	case c == 'n':
		return p.lit("null")
	default:
		_, ok := p.number()
		return ok
	}
}

// raw returns the bytes of the next value, as json.RawMessage would hold
// them.
func (p *jsonParser) raw() (json.RawMessage, bool) {
	p.ws()
	start := p.i
	if !p.skip() {
		return nil, false
	}
	return json.RawMessage(p.b[start:p.i]), true
}

// number reads a JSON number and returns its text.
func (p *jsonParser) number() ([]byte, bool) {
	start := p.i
	if p.i < len(p.b) && p.b[p.i] == '-' {
		p.i++
	}
	digits := func() int {
		n := 0
		for p.i < len(p.b) && p.b[p.i] >= '0' && p.b[p.i] <= '9' {
			p.i++
			n++
		}
		return n
	}
	if p.i < len(p.b) && p.b[p.i] == '0' {
		p.i++
	} else if digits() == 0 {
		return nil, false
	}
	if p.i < len(p.b) && p.b[p.i] == '.' {
		p.i++
		if digits() == 0 {
			return nil, false
		}
	}
	if p.i < len(p.b) && (p.b[p.i] == 'e' || p.b[p.i] == 'E') {
		p.i++
		if p.i < len(p.b) && (p.b[p.i] == '+' || p.b[p.i] == '-') {
			p.i++
		}
		if digits() == 0 {
			return nil, false
		}
	}
	return p.b[start:p.i], true
}

// int reads a number into an int field; null leaves it zero, as for a
// fresh value.
func (p *jsonParser) int() (int, bool) {
	if p.null() {
		return 0, true
	}
	text, ok := p.number()
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(string(text), 10, strconv.IntSize)
	if err != nil {
		return 0, false // fractions, exponents, overflow: encoding/json decides
	}
	return int(n), true
}

// str reads a string field; null leaves it empty, as for a fresh value.
// When the text equals reuse, reuse is returned without allocating.
func (p *jsonParser) str(reuse string) (string, bool) {
	if p.null() {
		return "", true
	}
	if p.i >= len(p.b) || p.b[p.i] != '"' {
		return "", false
	}
	p.i++
	start := p.i
	// Fast path: no escapes, valid UTF-8.
	for j := start; j < len(p.b); j++ {
		c := p.b[j]
		switch {
		case c == '"':
			p.i = j + 1
			raw := p.b[start:j]
			if string(raw) == reuse {
				return reuse, true
			}
			if !utf8.Valid(raw) {
				return "", false // encoding/json replaces invalid bytes
			}
			return string(raw), true
		case c == '\\':
			return p.unescape(start)
		case c < ' ':
			p.bad = true
			return "", false
		}
	}
	return "", false
}

// unescape reads the rest of a string that has escapes, decoding them as
// encoding/json does.
func (p *jsonParser) unescape(start int) (string, bool) {
	out := make([]byte, 0, 64)
	j := start
	for j < len(p.b) {
		c := p.b[j]
		switch {
		case c == '"':
			p.i = j + 1
			if !utf8.Valid(out) {
				return "", false
			}
			return string(out), true
		case c < ' ':
			return "", false
		case c == '\\':
			if j+1 >= len(p.b) {
				return "", false
			}
			switch e := p.b[j+1]; e {
			case '"', '\\', '/':
				out = append(out, e)
				j += 2
			case 'b':
				out = append(out, '\b')
				j += 2
			case 'f':
				out = append(out, '\f')
				j += 2
			case 'n':
				out = append(out, '\n')
				j += 2
			case 'r':
				out = append(out, '\r')
				j += 2
			case 't':
				out = append(out, '\t')
				j += 2
			case 'u':
				r := hex4(p.b, j+2)
				if r < 0 {
					return "", false
				}
				j += 6
				if utf16.IsSurrogate(r) {
					r2 := rune(-1)
					if j+1 < len(p.b) && p.b[j] == '\\' && p.b[j+1] == 'u' {
						r2 = hex4(p.b, j+2)
					}
					if dec := utf16.DecodeRune(r, r2); dec != utf8.RuneError {
						j += 6
						r = dec
					} else {
						r = utf8.RuneError
					}
				}
				out = utf8.AppendRune(out, r)
			default:
				return "", false
			}
		default:
			out = append(out, c)
			j++
		}
	}
	return "", false
}

// hex4 decodes four hex digits at b[i:], or returns -1.
func hex4(b []byte, i int) rune {
	if i+4 > len(b) {
		return -1
	}
	var r rune
	for _, c := range b[i : i+4] {
		switch {
		case '0' <= c && c <= '9':
			c -= '0'
		case 'a' <= c && c <= 'f':
			c = c - 'a' + 10
		case 'A' <= c && c <= 'F':
			c = c - 'A' + 10
		default:
			return -1
		}
		r = r*16 + rune(c)
	}
	return r
}
