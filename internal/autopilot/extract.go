package autopilot

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// fence is one fenced code block in a model reply.
type fence struct {
	lang string
	body string
}

// fencedBlocks returns the fenced code blocks of text in order. An
// unterminated final block (a reply cut off mid-JSON) is included so the
// error message can point at the real problem.
func fencedBlocks(text string) []fence {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var out []fence
	for i := 0; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(t, "```") {
			continue
		}
		ticks := len(t) - len(strings.TrimLeft(t, "`"))
		lang := strings.ToLower(strings.TrimSpace(t[ticks:]))
		var body []string
		j := i + 1
		for ; j < len(lines); j++ {
			c := strings.TrimSpace(lines[j])
			if strings.HasPrefix(c, strings.Repeat("`", ticks)) && strings.Trim(c, "`") == "" {
				break
			}
			body = append(body, lines[j])
		}
		out = append(out, fence{lang: lang, body: strings.Join(body, "\n")})
		i = j
	}
	return out
}

// extractObject finds the JSON object in a model reply that has all the
// required top-level keys. It prefers the last ```json fenced block, then
// any fenced block, then a bare object. The error explains precisely what
// is wrong so the model can correct it.
func extractObject(text string, required ...string) (json.RawMessage, error) {
	var candidates []string
	blocks := fencedBlocks(text)
	for i := len(blocks) - 1; i >= 0; i-- {
		if strings.HasPrefix(blocks[i].lang, "json") {
			candidates = append(candidates, blocks[i].body)
		}
	}
	for i := len(blocks) - 1; i >= 0; i-- {
		if blocks[i].lang == "" {
			candidates = append(candidates, blocks[i].body)
		}
	}
	if len(blocks) == 0 {
		t := strings.TrimSpace(text)
		if strings.HasPrefix(t, "{") {
			candidates = append(candidates, t)
		} else if a, b := strings.Index(t, "{"), strings.LastIndex(t, "}"); a >= 0 && b > a {
			candidates = append(candidates, t[a:b+1])
		}
	}
	if len(candidates) == 0 {
		if strings.TrimSpace(text) == "" {
			return nil, errors.New("the reply was empty; it must end with a ```json fenced block")
		}
		return nil, errors.New("no ```json fenced block was found in the reply")
	}
	var firstErr error
	for _, c := range candidates {
		raw, err := decodeObject(c)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		var keys map[string]json.RawMessage
		_ = json.Unmarshal(raw, &keys)
		var missing []string
		for _, k := range required {
			if _, ok := keys[k]; !ok {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			if firstErr == nil {
				firstErr = fmt.Errorf("the JSON object is missing the required field(s) %s", quoteList(missing))
			}
			continue
		}
		return raw, nil
	}
	return nil, firstErr
}

// decodeObject validates that s is one JSON object. On a syntax error it
// retries once with trailing commas and // comments removed, which models
// commonly emit, before giving up.
func decodeObject(s string) (json.RawMessage, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("the JSON block is empty")
	}
	raw, err := strictObject(s)
	if err == nil {
		return raw, nil
	}
	if relaxed := relaxJSON(s); relaxed != s {
		if raw, rerr := strictObject(relaxed); rerr == nil {
			return raw, nil
		}
	}
	return nil, describeJSONError(s, err)
}

func strictObject(s string) (json.RawMessage, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	var v json.RawMessage
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("unexpected data after the JSON object")
	}
	if t := bytes.TrimSpace(v); len(t) == 0 || t[0] != '{' {
		return nil, errors.New("the JSON value must be an object")
	}
	return v, nil
}

// relaxJSON removes trailing commas before } or ] and // line comments,
// outside of strings.
func relaxJSON(s string) string {
	var b strings.Builder
	inStr, esc := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			b.WriteByte(c)
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch {
		case c == '"':
			inStr = true
			b.WriteByte(c)
		case c == '/' && i+1 < len(s) && s[i+1] == '/':
			for i < len(s) && s[i] != '\n' {
				i++
			}
			if i < len(s) {
				b.WriteByte('\n')
			}
		case c == ',':
			j := i + 1
			for j < len(s) && strings.IndexByte(" \t\r\n", s[j]) >= 0 {
				j++
			}
			if j < len(s) && (s[j] == '}' || s[j] == ']') {
				continue
			}
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// describeJSONError adds a line and column to syntax errors.
func describeJSONError(s string, err error) error {
	var se *json.SyntaxError
	if errors.As(err, &se) {
		off := int(min(se.Offset, int64(len(s))))
		line := strings.Count(s[:off], "\n") + 1
		col := off - strings.LastIndex(s[:off], "\n")
		return fmt.Errorf("the JSON block is not valid JSON: %v (line %d, column %d)", se, line, col)
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return errors.New("the JSON block is not valid JSON: it ends before the object is complete (was the reply cut off?)")
	}
	return fmt.Errorf("the JSON block is not valid JSON: %v", err)
}

func quoteList(xs []string) string {
	q := make([]string, len(xs))
	for i, x := range xs {
		q[i] = fmt.Sprintf("%q", x)
	}
	return strings.Join(q, ", ")
}
