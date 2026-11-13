package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

// Wire types for streamed chunks (and, reused, for a non-streamed reply).

type chunk struct {
	ID      string                      `json:"id"`
	Model   string                      `json:"model"`
	Choices []chunkChoice               `json:"choices"`
	Usage   *wireUsage                  `json:"usage"`
	XGroq   *struct{ Usage *wireUsage } `json:"x_groq"`
	Error   json.RawMessage             `json:"error"`
}

type chunkChoice struct {
	Index        int    `json:"index"`
	Delta        delta  `json:"delta"`
	Message      *delta `json:"message"` // non-streamed replies
	FinishReason string `json:"finish_reason"`
}

type delta struct {
	Content          string          `json:"content"`
	ReasoningContent string          `json:"reasoning_content"`
	Reasoning        string          `json:"reasoning"`
	Refusal          string          `json:"refusal"`
	ToolCalls        []deltaToolCall `json:"tool_calls"`
}

type deltaToolCall struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type wireUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	// DeepSeek reports cache hits separately.
	PromptCacheHitTokens int64 `json:"prompt_cache_hit_tokens"`
}

// argumentText decodes a tool-call arguments fragment. The standard form is
// a JSON string; a few servers send the arguments as a JSON object.
func argumentText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	}
	return string(raw)
}

type blockKind int

const (
	kindNone blockKind = iota
	kindThinking
	kindText
	kindTool
)

type toolSlot struct {
	id, name     string
	args         strings.Builder
	started      bool
	pendingBytes int
}

// part is one ordered piece of the reply: a text or thinking run, or a tool call.
type part struct {
	kind blockKind
	text *strings.Builder
	tool *toolSlot
}

// accumulator rebuilds the reply from chunks and reports progress.
type accumulator struct {
	onEvent func(llm.Event)
	parts   []part
	open    blockKind
	tools   []*toolSlot
	byIndex map[int]*toolSlot
	refusal strings.Builder
	finish  string
	usage   *wireUsage
	model   string
}

func newAccumulator(onEvent func(llm.Event)) *accumulator {
	return &accumulator{onEvent: onEvent, byIndex: map[int]*toolSlot{}}
}

// enter makes kind the open block, closing the previous one. Every tool call
// is its own block, so entering kindTool always closes what was open.
func (a *accumulator) enter(kind blockKind) {
	if a.open != kindNone && (a.open != kind || kind == kindTool) {
		a.onEvent(llm.Event{Type: llm.EventBlockStop})
	}
	a.open = kind
}

// appendRun adds text to the trailing run of kind, starting one if needed.
func (a *accumulator) appendRun(kind blockKind, s string) {
	if n := len(a.parts); n > 0 && a.parts[n-1].kind == kind && a.open == kind {
		a.parts[n-1].text.WriteString(s)
		return
	}
	b := &strings.Builder{}
	b.WriteString(s)
	a.parts = append(a.parts, part{kind: kind, text: b})
}

func (a *accumulator) addChunk(c *chunk) {
	if c.Model != "" {
		a.model = c.Model
	}
	if c.Usage != nil {
		a.usage = c.Usage
	} else if c.XGroq != nil && c.XGroq.Usage != nil {
		a.usage = c.XGroq.Usage
	}
	for i := range c.Choices {
		ch := &c.Choices[i]
		if ch.Index != 0 {
			continue // only one choice is requested
		}
		d := ch.Delta
		if ch.Message != nil {
			d = *ch.Message
		}
		a.addDelta(d)
		if ch.FinishReason != "" {
			a.finish = ch.FinishReason
		}
	}
}

func (a *accumulator) addDelta(d delta) {
	// Some servers mirror reasoning_content into reasoning; take one.
	reasoning := d.ReasoningContent
	if reasoning == "" {
		reasoning = d.Reasoning
	}
	if reasoning != "" {
		a.enter(kindThinking)
		a.appendRun(kindThinking, reasoning)
		a.onEvent(llm.Event{Type: llm.EventThinkingDelta, Text: reasoning})
	}
	for _, text := range []string{d.Content, d.Refusal} {
		if text == "" {
			continue
		}
		a.enter(kindText)
		a.appendRun(kindText, text)
		a.onEvent(llm.Event{Type: llm.EventTextDelta, Text: text})
	}
	if d.Refusal != "" {
		a.refusal.WriteString(d.Refusal)
	}
	for i := range d.ToolCalls {
		a.addToolDelta(&d.ToolCalls[i])
	}
}

// slotFor finds the tool call a fragment belongs to. Calls are keyed by
// index; a fragment carrying a different id under a known index (servers
// that reuse index 0) starts a new call, and servers that omit the index
// are matched by id or appended to the latest call.
func (a *accumulator) slotFor(tc *deltaToolCall) *toolSlot {
	newSlot := func() *toolSlot {
		s := &toolSlot{}
		a.tools = append(a.tools, s)
		a.parts = append(a.parts, part{kind: kindTool, tool: s})
		return s
	}
	if tc.Index != nil {
		if s, ok := a.byIndex[*tc.Index]; ok && (tc.ID == "" || s.id == "" || s.id == tc.ID) {
			return s
		}
		s := newSlot()
		a.byIndex[*tc.Index] = s
		return s
	}
	if tc.ID != "" {
		for _, s := range a.tools {
			if s.id == tc.ID {
				return s
			}
		}
		return newSlot()
	}
	if n := len(a.tools); n > 0 {
		return a.tools[n-1]
	}
	return newSlot()
}

func (a *accumulator) addToolDelta(tc *deltaToolCall) {
	s := a.slotFor(tc)
	if s.id == "" && tc.ID != "" {
		s.id = tc.ID
	}
	if s.name == "" && tc.Function.Name != "" {
		s.name = tc.Function.Name
	}
	frag := argumentText(tc.Function.Arguments)
	s.args.WriteString(frag)
	if !s.started {
		s.pendingBytes += len(frag)
		if s.name == "" {
			return
		}
		s.started = true
		a.enter(kindTool)
		a.onEvent(llm.Event{Type: llm.EventToolUseStart, ToolName: s.name, ToolID: s.id})
		if s.pendingBytes > 0 {
			a.onEvent(llm.Event{Type: llm.EventToolInputDelta, ToolName: s.name, ToolID: s.id, Bytes: s.pendingBytes})
		}
		s.pendingBytes = 0
		return
	}
	if len(frag) > 0 {
		a.onEvent(llm.Event{Type: llm.EventToolInputDelta, ToolName: s.name, ToolID: s.id, Bytes: len(frag)})
	}
}

// finish closes the last block and assembles the response.
func (a *accumulator) result(provider, requestModel string) (*llm.Response, error) {
	if a.open != kindNone {
		a.onEvent(llm.Event{Type: llm.EventBlockStop})
		a.open = kindNone
	}
	msg := llm.Message{Role: llm.RoleAssistant, Provider: provider}
	calls := 0
	for _, pt := range a.parts {
		switch pt.kind {
		case kindThinking:
			if s := pt.text.String(); s != "" {
				msg.Blocks = append(msg.Blocks, llm.Block{Type: llm.BlockThinking, Text: s})
			}
		case kindText:
			if s := pt.text.String(); s != "" {
				msg.Blocks = append(msg.Blocks, llm.Block{Type: llm.BlockText, Text: s})
			}
		case kindTool:
			s := pt.tool
			calls++
			if s.name == "" {
				return nil, fmt.Errorf("%s: the model sent a tool call without a function name", provider)
			}
			id := s.id
			if id == "" {
				id = "call_" + strconv.Itoa(calls)
			}
			msg.Blocks = append(msg.Blocks, llm.Block{
				Type: llm.BlockToolUse, ToolUseID: id, ToolName: s.name, Input: toolInput(s.args.String()),
			})
		}
	}
	resp := &llm.Response{Message: msg, Model: a.model, StopReason: mapFinish(a.finish)}
	if resp.Model == "" {
		resp.Model = requestModel
	}
	// Several servers report "stop" even when the turn ends in tool calls.
	if calls > 0 && resp.StopReason == llm.StopEndTurn {
		resp.StopReason = llm.StopToolUse
	}
	switch {
	case a.refusal.Len() > 0:
		resp.StopReason = llm.StopRefusal
		resp.StopDetail = strings.TrimSpace(a.refusal.String())
	case resp.StopReason == llm.StopRefusal:
		resp.StopDetail = "the provider's content filter stopped the response"
	}
	if u := a.usage; u != nil {
		cached := int64(0)
		if u.PromptTokensDetails != nil {
			cached = u.PromptTokensDetails.CachedTokens
		}
		if cached == 0 {
			cached = u.PromptCacheHitTokens
		}
		input := u.PromptTokens - cached
		if input < 0 {
			input = 0
		}
		resp.Usage = llm.Usage{InputTokens: input, OutputTokens: u.CompletionTokens, CacheReadTokens: cached}
	}
	return resp, nil
}

// toolInput turns streamed arguments into tool input. Empty arguments mean
// no parameters; arguments cut short (for example at max_tokens) cannot be
// trusted and become an empty object, which the tool's own validation
// rejects with a clear message.
func toolInput(args string) json.RawMessage {
	args = strings.TrimSpace(args)
	if args == "" || !json.Valid([]byte(args)) {
		return json.RawMessage("{}")
	}
	var buf bytes.Buffer
	if json.Compact(&buf, []byte(args)) != nil {
		return json.RawMessage("{}")
	}
	return json.RawMessage(buf.Bytes())
}

// mapFinish normalizes finish_reason.
func mapFinish(reason string) llm.StopReason {
	switch reason {
	case "stop", "eos", "end_turn", "stop_sequence":
		return llm.StopEndTurn
	case "tool_calls", "function_call", "tool_use":
		return llm.StopToolUse
	case "length", "max_tokens", "model_length":
		return llm.StopMaxTokens
	case "content_filter", "safety", "refusal":
		return llm.StopRefusal
	case "":
		return llm.StopEndTurn
	}
	return llm.StopOther
}

var errIdleTimeout = errors.New("stream idle timeout")

// read consumes a successful response: an SSE stream, or a plain JSON body
// from servers that ignore stream:true.
func (p *Provider) read(ctx, actx context.Context, cancel context.CancelCauseFunc, resp *http.Response, model string, onEvent func(llm.Event)) (*llm.Response, error) {
	acc := newAccumulator(onEvent)
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt == "application/json" {
		return p.readJSON(ctx, resp.Body, acc, model)
	}

	var idle *time.Timer
	touch := func() {}
	if p.idle > 0 {
		idle = time.AfterFunc(p.idle, func() { cancel(errIdleTimeout) })
		defer idle.Stop()
		touch = func() { idle.Reset(p.idle) }
	}

	sawData, done := false, false
	err := readSSE(resp.Body, touch, func(data []byte) (bool, error) {
		if err := ctx.Err(); err != nil {
			return true, err
		}
		if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
			done = true
			return true, nil
		}
		var c chunk
		if err := json.Unmarshal(data, &c); err != nil {
			return true, fmt.Errorf("%s: the server sent a malformed stream chunk: %w", p.opts.Name, err)
		}
		if err := p.streamError(&c, model); err != nil {
			return true, err
		}
		sawData = true
		acc.addChunk(&c)
		return false, nil
	})
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) || !isReadErr(err) {
			return nil, err
		}
		if errors.Is(context.Cause(actx), errIdleTimeout) {
			return nil, fmt.Errorf("%s: the stream stalled: no data for %s", p.opts.Name, p.idle)
		}
		return nil, fmt.Errorf("%s: the stream was interrupted: %w", p.opts.Name, err)
	}
	if !done && acc.finish == "" {
		if !sawData {
			return nil, fmt.Errorf("%s: the server returned an empty stream", p.opts.Name)
		}
		return nil, fmt.Errorf("%s: the stream ended before the model finished", p.opts.Name)
	}
	return acc.result(p.opts.Name, model)
}

// readErr marks errors from the body reader, as opposed to chunk handling.
type readErr struct{ err error }

func (e readErr) Error() string { return e.err.Error() }
func (e readErr) Unwrap() error { return e.err }

func isReadErr(err error) bool {
	var re readErr
	return errors.As(err, &re)
}

// readJSON handles a non-streamed completion body.
func (p *Provider) readJSON(ctx context.Context, body io.Reader, acc *accumulator, model string) (*llm.Response, error) {
	data, err := io.ReadAll(io.LimitReader(body, 64<<20))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("%s: read response: %w", p.opts.Name, err)
	}
	var c chunk
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("%s: the server sent a malformed response: %w", p.opts.Name, err)
	}
	if err := p.streamError(&c, model); err != nil {
		return nil, err
	}
	// A full message carries complete tool calls without indexes; number
	// them so each becomes its own call.
	for i := range c.Choices {
		if m := c.Choices[i].Message; m != nil {
			for j := range m.ToolCalls {
				if m.ToolCalls[j].Index == nil {
					idx := j
					m.ToolCalls[j].Index = &idx
				}
			}
		}
	}
	acc.addChunk(&c)
	if len(c.Choices) == 0 {
		return nil, fmt.Errorf("%s: the server returned no choices", p.opts.Name)
	}
	return acc.result(p.opts.Name, model)
}

// streamError converts an in-band error object into an APIError.
func (p *Provider) streamError(c *chunk, model string) error {
	raw := bytes.TrimSpace(c.Error)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	msg, typ, code, _ := parseErrorBody([]byte(`{"error":` + string(raw) + `}`))
	e := &APIError{
		Provider: p.opts.Name, Type: typ, Code: code, Message: msg, Streaming: true,
		keyEnv: p.opts.APIKeyEnv, baseURL: p.opts.BaseURL, model: model,
	}
	if n, err := strconv.Atoi(code); err == nil {
		e.StatusCode = n
	}
	if e.Message == "" {
		e.Message = oneLine(string(raw))
	}
	return e
}

// readSSE parses a Server-Sent Events stream, calling handle with each
// event's data until handle reports done, the body ends or reading fails.
// Comment lines (keep-alives such as ": OPENROUTER PROCESSING") and fields
// other than data are ignored. touch is called whenever bytes arrive.
func readSSE(r io.Reader, touch func(), handle func(data []byte) (bool, error)) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var buf bytes.Buffer
	hasData := false
	dispatch := func() (bool, error) {
		if !hasData {
			return false, nil
		}
		data := append([]byte(nil), buf.Bytes()...)
		buf.Reset()
		hasData = false
		return handle(data)
	}
	for {
		line, rerr := br.ReadBytes('\n')
		if len(line) > 0 {
			touch()
			line = bytes.TrimRight(line, "\r\n")
			switch {
			case len(line) == 0:
				if stop, err := dispatch(); stop || err != nil {
					return err
				}
			case line[0] == ':':
			default:
				field, value, _ := bytes.Cut(line, []byte(":"))
				value = bytes.TrimPrefix(value, []byte(" "))
				if string(field) != "data" {
					continue
				}
				// Tolerate servers that omit the blank line between events:
				// a complete JSON payload followed by another data line is
				// two events, not one multi-line event.
				if hasData && json.Valid(buf.Bytes()) {
					if stop, err := dispatch(); stop || err != nil {
						return err
					}
				}
				if hasData {
					buf.WriteByte('\n')
				}
				buf.Write(value)
				hasData = true
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				_, err := dispatch()
				return err
			}
			return readErr{rerr}
		}
	}
}
