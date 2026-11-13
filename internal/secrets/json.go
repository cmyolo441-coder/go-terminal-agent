package secrets

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"unicode/utf8"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

// jnode is an order-preserving JSON value, so redaction changes only the
// strings it must and keeps the key order a reader expects.
type jnode struct {
	kind byte // 'o' object, 'a' array, 's' string, 'l' literal (number, bool, null)
	keys []string
	vals []*jnode
	s    string
}

// maxJSONDepth bounds recursion on hostile input.
const maxJSONDepth = 512

func parseJSON(data []byte) (*jnode, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	n, err := parseValue(dec, 0)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("secrets: trailing data after JSON value")
	}
	return n, nil
}

func parseValue(dec *json.Decoder, depth int) (*jnode, error) {
	if depth > maxJSONDepth {
		return nil, errors.New("secrets: JSON nested too deeply")
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			n := &jnode{kind: 'o'}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, ok := kt.(string)
				if !ok {
					return nil, errors.New("secrets: object key is not a string")
				}
				v, err := parseValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				n.keys = append(n.keys, k)
				n.vals = append(n.vals, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return n, nil
		case '[':
			n := &jnode{kind: 'a'}
			for dec.More() {
				v, err := parseValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				n.vals = append(n.vals, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return n, nil
		}
		return nil, fmt.Errorf("secrets: unexpected delimiter %q", t)
	case string:
		return &jnode{kind: 's', s: t}, nil
	case json.Number:
		return &jnode{kind: 'l', s: t.String()}, nil
	case bool:
		return &jnode{kind: 'l', s: strconv.FormatBool(t)}, nil
	case nil:
		return &jnode{kind: 'l', s: "null"}, nil
	}
	return nil, fmt.Errorf("secrets: unexpected JSON token %T", tok)
}

func (n *jnode) encode(b *bytes.Buffer) {
	switch n.kind {
	case 'o':
		b.WriteByte('{')
		for i, k := range n.keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSONString(b, k)
			b.WriteByte(':')
			n.vals[i].encode(b)
		}
		b.WriteByte('}')
	case 'a':
		b.WriteByte('[')
		for i, v := range n.vals {
			if i > 0 {
				b.WriteByte(',')
			}
			v.encode(b)
		}
		b.WriteByte(']')
	case 's':
		writeJSONString(b, n.s)
	default:
		b.WriteString(n.s)
	}
}

// writeJSONString quotes s as JSON without HTML escaping, so redacted
// output stays as close to the input as possible.
func writeJSONString(b *bytes.Buffer, s string) {
	const hexd = "0123456789abcdef"
	b.WriteByte('"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch {
			case c == '"':
				b.WriteString(`\"`)
			case c == '\\':
				b.WriteString(`\\`)
			case c == '\n':
				b.WriteString(`\n`)
			case c == '\r':
				b.WriteString(`\r`)
			case c == '\t':
				b.WriteString(`\t`)
			case c < 0x20:
				b.WriteString(`\u00`)
				b.WriteByte(hexd[c>>4])
				b.WriteByte(hexd[c&0xf])
			default:
				b.WriteByte(c)
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			b.WriteString(`�`)
		case r == ' ' || r == ' ':
			b.WriteString(`\u202`)
			b.WriteByte(hexd[r&0xf])
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	b.WriteByte('"')
}

// jsonWalk carries state while redacting a JSON tree.
type jsonWalk struct {
	r        *Redactor
	findings []Finding
	// skip reports objects that must not be modified (signed content).
	skip func(n *jnode) bool
	// skipKeys are fields whose string values are left alone.
	skipKeys map[string]bool
	// skipped counts secrets seen inside skipped objects.
	skipped int
}

func (w *jsonWalk) walk(n *jnode) {
	switch n.kind {
	case 'o':
		if w.skip != nil && w.skip(n) {
			w.countSkipped(n)
			return
		}
		// {"name": "DB_PASSWORD", "value": "..."} as in Kubernetes env lists.
		pairSecret := false
		for i, k := range n.keys {
			if (k == "name" || k == "key") && n.vals[i].kind == 's' && IsSecretKey(n.vals[i].s) {
				pairSecret = true
			}
		}
		for i, k := range n.keys {
			v := n.vals[i]
			if v.kind == 's' && w.skipKeys[k] {
				continue
			}
			w.walk(v)
			if v.kind == 's' && (IsSecretKey(k) || (pairSecret && k == "value")) && plausibleSecretValue(v.s) && !hasPlaceholder(v.s) {
				f := Finding{RuleID: RuleGenericSecret, Line: 1, Column: 1, End: len(v.s),
					Fingerprint: w.r.fingerprint(v.s), Placeholder: w.r.Placeholder(RuleGenericSecret, v.s)}
				if !w.r.allow[v.s] {
					w.findings = append(w.findings, f)
					v.s = f.Placeholder
				}
			}
		}
	case 'a':
		for _, v := range n.vals {
			w.walk(v)
		}
	case 's':
		red, fs := w.r.Redact(n.s)
		if len(fs) > 0 {
			n.s = red
			w.findings = append(w.findings, fs...)
		}
	}
}

func (w *jsonWalk) countSkipped(n *jnode) {
	switch n.kind {
	case 'o', 'a':
		for _, v := range n.vals {
			w.countSkipped(v)
		}
	case 's':
		w.skipped += len(w.r.Scan(n.s))
	}
}

func hasPlaceholder(s string) bool { return bytes.Contains([]byte(s), []byte("[REDACTED:")) }

// RedactJSON redacts every string in a JSON document, and the values of
// fields named like credentials ("password", "api_key", ...). Key order is
// kept and the result is always valid JSON. When nothing is found the input
// is returned unchanged. Invalid JSON is an error and is returned as is.
func (r *Redactor) RedactJSON(data []byte) ([]byte, []Finding, error) {
	out, fs, _, err := r.redactJSON(data, nil, nil)
	return out, fs, err
}

// redactJSON also returns how many secrets were left in skipped objects.
func (r *Redactor) redactJSON(data []byte, skip func(*jnode) bool, skipKeys map[string]bool) ([]byte, []Finding, int, error) {
	n, err := parseJSON(data)
	if err != nil {
		return data, nil, 0, err
	}
	w := &jsonWalk{r: r, skip: skip, skipKeys: skipKeys}
	w.walk(n)
	if len(w.findings) == 0 {
		return data, nil, w.skipped, nil
	}
	var b bytes.Buffer
	b.Grow(len(data))
	n.encode(&b)
	return b.Bytes(), w.findings, w.skipped, nil
}

// MessageFinding locates a finding in a conversation.
type MessageFinding struct {
	// Message and Block index the conversation; Block is -1 for the
	// provider-native payload.
	Message int `json:"message"`
	Block   int `json:"block"`
	// Field is "text", "input" or "native".
	Field string `json:"field"`
	Finding
}

// MessageReport summarises a RedactMessages call.
type MessageReport struct {
	Findings []MessageFinding
	// DroppedNative counts native payloads that held secrets but could not
	// be parsed, so they were removed and the provider falls back to Blocks.
	DroppedNative int
	// SignedUnredacted counts secrets left inside signed provider content
	// (thinking blocks), which cannot change without breaking replay. They
	// are the model's own output, so the model has already seen them.
	SignedUnredacted int
}

// Count is the number of secrets redacted.
func (m MessageReport) Count() int { return len(m.Findings) }

// RedactMessages redacts the default Redactor's way; see
// Redactor.RedactMessages.
func RedactMessages(msgs []llm.Message) ([]llm.Message, MessageReport) {
	return Default().RedactMessages(msgs)
}

// nativeSkipKeys are provider fields that are identifiers or opaque data.
var nativeSkipKeys = map[string]bool{
	"id": true, "type": true, "signature": true, "tool_use_id": true, "data": true,
	"encrypted_content": true, "encrypted_index": true, "media_type": true,
}

func skipSigned(n *jnode) bool {
	for i, k := range n.keys {
		if k == "type" && n.vals[i].kind == 's' {
			switch n.vals[i].s {
			case "thinking", "redacted_thinking":
				return true
			}
		}
	}
	return false
}

// RedactMessages returns a copy of msgs with secrets removed from text,
// thinking text, tool inputs (kept valid JSON), tool results and the
// provider-native replay payload. The input slice is not modified.
func (r *Redactor) RedactMessages(msgs []llm.Message) ([]llm.Message, MessageReport) {
	var rep MessageReport
	out := make([]llm.Message, len(msgs))
	for i, m := range msgs {
		nm := m
		if len(m.Blocks) > 0 {
			nm.Blocks = make([]llm.Block, len(m.Blocks))
			copy(nm.Blocks, m.Blocks)
		}
		for j := range nm.Blocks {
			b := &nm.Blocks[j]
			if b.Text != "" {
				red, fs := r.Redact(b.Text)
				if len(fs) > 0 {
					b.Text = red
					for _, f := range fs {
						rep.Findings = append(rep.Findings, MessageFinding{Message: i, Block: j, Field: "text", Finding: f})
					}
				}
			}
			if len(b.Input) > 0 {
				red, fs := r.redactInput(b.Input)
				if len(fs) > 0 {
					b.Input = red
					for _, f := range fs {
						rep.Findings = append(rep.Findings, MessageFinding{Message: i, Block: j, Field: "input", Finding: f})
					}
				}
			}
		}
		if len(m.Native) > 0 {
			red, fs, skipped, err := r.redactJSON(m.Native, skipSigned, nativeSkipKeys)
			rep.SignedUnredacted += skipped
			switch {
			case err != nil:
				if r.Contains(string(m.Native)) {
					nm.Native, nm.Provider = nil, ""
					rep.DroppedNative++
				}
			case len(fs) > 0:
				nm.Native = red
				for _, f := range fs {
					rep.Findings = append(rep.Findings, MessageFinding{Message: i, Block: -1, Field: "native", Finding: f})
				}
			}
		}
		out[i] = nm
	}
	return out, rep
}

// redactInput redacts a tool input. Valid JSON stays valid JSON; input that
// is not JSON (for example truncated by max_tokens) is redacted as text.
func (r *Redactor) redactInput(in json.RawMessage) (json.RawMessage, []Finding) {
	red, fs, err := r.RedactJSON(in)
	if err == nil {
		return red, fs
	}
	text, fs := r.Redact(string(in))
	return json.RawMessage(text), fs
}
