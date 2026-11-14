package evals

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

// Cassette file format identifiers. A cassette is JSONL: one header line
// followed by one line per recorded interaction.
const (
	CassetteFormat  = "gta-cassette"
	CassetteVersion = 1
)

// MaxCassetteLine bounds a single JSONL line so a corrupt or hostile file
// cannot exhaust memory.
const MaxCassetteLine = 64 << 20

// CassetteHeader is the first line of a cassette file.
type CassetteHeader struct {
	Type     string            `json:"type"`
	Format   string            `json:"format"`
	Version  int               `json:"version"`
	Provider string            `json:"provider,omitempty"`
	Meta     map[string]string `json:"meta,omitempty"`
}

// Interaction is one recorded model call.
type Interaction struct {
	Type string `json:"type"`
	// Seq is the 0-based position of the interaction in the cassette.
	Seq int `json:"seq"`
	// Hash is the request hash under the options used when recording. It
	// is informational: matching re-hashes Request under the options in
	// force at replay time, so ignore lists can change without
	// re-recording.
	Hash string `json:"hash"`
	// Request is the scrubbed canonical request tree.
	Request map[string]any `json:"request"`
	// Events are the streamed events in the order they were delivered.
	Events []RecordedEvent `json:"events,omitempty"`
	// Response is the final response; nil when Error is set.
	Response *RecordedResponse `json:"response,omitempty"`
	// Error is the provider's error message for a recorded failure.
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

// RecordedEvent is an llm.Event with a symbolic type.
type RecordedEvent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ToolName string `json:"tool_name,omitempty"`
	ToolID   string `json:"tool_id,omitempty"`
	Bytes    int    `json:"bytes,omitempty"`
}

// RecordedResponse is an llm.Response with stable JSON field names.
type RecordedResponse struct {
	Message    llm.Message `json:"message"`
	StopReason string      `json:"stop_reason"`
	StopDetail string      `json:"stop_detail,omitempty"`
	Usage      llm.Usage   `json:"usage"`
	Model      string      `json:"model,omitempty"`
}

var eventNames = map[llm.EventType]string{
	llm.EventTextDelta:       "text_delta",
	llm.EventThinkingDelta:   "thinking_delta",
	llm.EventToolUseStart:    "tool_use_start",
	llm.EventToolInputDelta:  "tool_input_delta",
	llm.EventServerToolStart: "server_tool_start",
	llm.EventBlockStop:       "block_stop",
}

var eventTypes = func() map[string]llm.EventType {
	m := make(map[string]llm.EventType, len(eventNames))
	for t, n := range eventNames {
		m[n] = t
	}
	return m
}()

func recordEvent(ev llm.Event, fn func(string) string) RecordedEvent {
	name, ok := eventNames[ev.Type]
	if !ok {
		name = fmt.Sprintf("unknown_%d", int(ev.Type))
	}
	return RecordedEvent{Type: name, Text: fn(ev.Text), ToolName: fn(ev.ToolName), ToolID: fn(ev.ToolID), Bytes: ev.Bytes}
}

func (e RecordedEvent) event(fn func(string) string) (llm.Event, error) {
	t, ok := eventTypes[e.Type]
	if !ok {
		return llm.Event{}, fmt.Errorf("unknown event type %q", e.Type)
	}
	return llm.Event{Type: t, Text: fn(e.Text), ToolName: fn(e.ToolName), ToolID: fn(e.ToolID), Bytes: e.Bytes}, nil
}

func identity(s string) string { return s }

// mapMessage returns a deep copy of m with fn applied to every string that
// can carry user or model content.
func mapMessage(m llm.Message, fn func(string) string) llm.Message {
	out := llm.Message{Role: m.Role, Provider: m.Provider}
	if m.Blocks != nil {
		out.Blocks = make([]llm.Block, len(m.Blocks))
	}
	for i, b := range m.Blocks {
		nb := llm.Block{
			Type:      b.Type,
			Text:      fn(b.Text),
			ToolUseID: fn(b.ToolUseID),
			ToolName:  fn(b.ToolName),
			IsError:   b.IsError,
		}
		if len(b.Input) > 0 {
			nb.Input = transformJSONStrings(append(json.RawMessage(nil), b.Input...), fn)
		}
		if b.Image != nil {
			img := mapImage(*b.Image, fn)
			nb.Image = &img
		}
		if b.Images != nil {
			nb.Images = make([]llm.Image, len(b.Images))
			for j, img := range b.Images {
				nb.Images[j] = mapImage(img, fn)
			}
		}
		out.Blocks[i] = nb
	}
	if len(m.Native) > 0 {
		out.Native = transformJSONStrings(append(json.RawMessage(nil), m.Native...), fn)
	}
	return out
}

func mapImage(img llm.Image, fn func(string) string) llm.Image {
	// Image data is base64 and never scrubbed; paths may carry workspace
	// locations and are.
	return llm.Image{MediaType: img.MediaType, Data: img.Data, Path: fn(img.Path)}
}

func recordResponse(r *llm.Response, fn func(string) string) *RecordedResponse {
	return &RecordedResponse{
		Message:    mapMessage(r.Message, fn),
		StopReason: string(r.StopReason),
		StopDetail: fn(r.StopDetail),
		Usage:      r.Usage,
		Model:      r.Model,
	}
}

func (r *RecordedResponse) response(fn func(string) string) *llm.Response {
	return &llm.Response{
		Message:    mapMessage(r.Message, fn),
		StopReason: llm.StopReason(r.StopReason),
		StopDetail: fn(r.StopDetail),
		Usage:      r.Usage,
		Model:      r.Model,
	}
}

// Cassette is an in-memory set of recorded interactions, optionally backed
// by an append-only writer. It is safe for concurrent use.
type Cassette struct {
	mu           sync.RWMutex
	header       CassetteHeader
	interactions []*Interaction
	sink         io.Writer
}

// NewCassette returns an empty cassette for the named provider.
func NewCassette(provider string) *Cassette {
	return &Cassette{header: CassetteHeader{Type: "header", Format: CassetteFormat, Version: CassetteVersion, Provider: provider}}
}

// Header returns a copy of the cassette header.
func (c *Cassette) Header() CassetteHeader {
	c.mu.RLock()
	defer c.mu.RUnlock()
	h := c.header
	if h.Meta != nil {
		m := make(map[string]string, len(h.Meta))
		for k, v := range h.Meta {
			m[k] = v
		}
		h.Meta = m
	}
	return h
}

// Len is the number of recorded interactions.
func (c *Cassette) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.interactions)
}

// Interactions returns the recorded interactions in order. The returned
// values must be treated as read-only.
func (c *Cassette) Interactions() []*Interaction {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]*Interaction(nil), c.interactions...)
}

func (c *Cassette) interaction(i int) *Interaction {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.interactions[i]
}

// add appends an interaction, assigning its sequence number, and writes it
// to the sink when one is attached. The write happens under the lock so
// lines from concurrent recorders never interleave.
func (c *Cassette) add(it *Interaction) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	it.Type = "interaction"
	it.Seq = len(c.interactions)
	if c.sink != nil {
		line, err := encodeLine(it)
		if err != nil {
			return 0, err
		}
		if _, err := c.sink.Write(line); err != nil {
			return 0, fmt.Errorf("evals: write cassette: %w", err)
		}
	}
	c.interactions = append(c.interactions, it)
	return it.Seq, nil
}

func encodeLine(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("evals: encode cassette line: %w", err)
	}
	return buf.Bytes(), nil
}

// WriteTo writes the whole cassette as JSONL.
func (c *Cassette) WriteTo(w io.Writer) (int64, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var n int64
	line, err := encodeLine(c.header)
	if err != nil {
		return 0, err
	}
	m, err := w.Write(line)
	n += int64(m)
	if err != nil {
		return n, err
	}
	for _, it := range c.interactions {
		line, err := encodeLine(it)
		if err != nil {
			return n, err
		}
		m, err := w.Write(line)
		n += int64(m)
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// CassetteError reports a malformed cassette line.
type CassetteError struct {
	Line int
	Err  error
}

func (e *CassetteError) Error() string {
	return fmt.Sprintf("evals: cassette line %d: %v", e.Line, e.Err)
}

func (e *CassetteError) Unwrap() error { return e.Err }

// ReadCassette parses a JSONL cassette. Blank lines are ignored. The first
// non-blank line must be a header of a supported version; every other line
// must be a well-formed interaction.
func ReadCassette(r io.Reader) (*Cassette, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	c := &Cassette{}
	sawHeader := false
	for lineNo := 1; ; lineNo++ {
		line, err := readLine(br)
		if len(bytes.TrimSpace(line)) > 0 {
			if perr := c.parseLine(line, &sawHeader); perr != nil {
				return nil, &CassetteError{Line: lineNo, Err: perr}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, &CassetteError{Line: lineNo, Err: err}
		}
	}
	if !sawHeader {
		return nil, &CassetteError{Line: 1, Err: errors.New("missing header")}
	}
	return c, nil
}

// readLine reads one line without its terminator, enforcing MaxCassetteLine.
func readLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(line)+len(chunk) > MaxCassetteLine {
			return nil, fmt.Errorf("line exceeds %d bytes", MaxCassetteLine)
		}
		line = append(line, chunk...)
		if err == bufio.ErrBufferFull {
			continue
		}
		return bytes.TrimRight(line, "\r\n"), err
	}
}

func (c *Cassette) parseLine(line []byte, sawHeader *bool) error {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &probe); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	if !*sawHeader {
		if probe.Type != "header" {
			return fmt.Errorf("first line must be a header, got type %q", probe.Type)
		}
		var h CassetteHeader
		if err := decodeLine(line, &h); err != nil {
			return fmt.Errorf("header: %w", err)
		}
		if h.Format != CassetteFormat {
			return fmt.Errorf("unsupported format %q", h.Format)
		}
		if h.Version < 1 || h.Version > CassetteVersion {
			return fmt.Errorf("unsupported cassette version %d (this build reads up to %d)", h.Version, CassetteVersion)
		}
		c.header = h
		*sawHeader = true
		return nil
	}
	if probe.Type != "interaction" {
		return fmt.Errorf("unexpected line type %q", probe.Type)
	}
	var it Interaction
	if err := decodeLine(line, &it); err != nil {
		return fmt.Errorf("interaction: %w", err)
	}
	if err := it.validate(); err != nil {
		return err
	}
	it.Seq = len(c.interactions)
	c.interactions = append(c.interactions, &it)
	return nil
}

// decodeLine decodes one JSON object, keeping numbers as json.Number so
// stored request trees re-hash exactly.
func decodeLine(line []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data after JSON object")
	}
	return nil
}

func (it *Interaction) validate() error {
	if it.Request == nil {
		return errors.New("interaction has no request")
	}
	if (it.Response == nil) == (it.Error == "") {
		return errors.New("interaction must have exactly one of response or error")
	}
	for i, ev := range it.Events {
		if _, ok := eventTypes[ev.Type]; !ok {
			return fmt.Errorf("event %d: unknown type %q", i, ev.Type)
		}
		if ev.Bytes < 0 {
			return fmt.Errorf("event %d: negative byte count", i)
		}
	}
	return nil
}

// LoadCassette reads a cassette file.
func LoadCassette(path string) (*Cassette, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	c, err := ReadCassette(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// SaveCassette writes the cassette to path atomically (write to a temporary
// file in the same directory, then rename).
func SaveCassette(path string, c *Cassette) error {
	return writeFileAtomic(path, func(w io.Writer) error {
		_, err := c.WriteTo(w)
		return err
	})
}
