package e2e_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeAPIKey is the obviously fake key every gta process gets; the mock
// checks that it arrives so we know requests come from the binary under test.
const fakeAPIKey = "e2e-fake-key-not-a-real-credential"

// requiredTools are tools every agent request must offer the model.
var requiredTools = []string{"read_file", "write_file", "edit_file", "bash"}

// mockAPI is a scripted, in-process stand-in for the Anthropic Messages API.
// Each request is checked for the shape the real API insists on (model,
// stream, tools, alternating roles, tool_result ids matching the previous
// tool_use ids) and answered by the next step of the script as an SSE
// stream. Problems are collected and reported when the test ends, so a
// handler goroutine never calls t.Fatal.
type mockAPI struct {
	srv *httptest.Server

	mu       sync.Mutex
	steps    []step
	handler  step // when set, answers every request instead of steps
	reqs     []*apiRequest
	problems []string
}

// step inspects one request and returns the reply. It may record problems
// with r.errorf.
type step func(r *apiRequest) reply

// apiRequest is one decoded request the mock received.
type apiRequest struct {
	m *mockAPI
	// N is the 0-based index of this request.
	N        int
	Raw      []byte
	Header   http.Header
	Model    string
	Stream   bool
	Tools    []string
	Messages []wireMessage
}

type wireRequest struct {
	Model     string          `json:"model"`
	MaxTokens int64           `json:"max_tokens"`
	Stream    bool            `json:"stream"`
	System    json.RawMessage `json:"system"`
	Messages  []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Name        string          `json:"name"`
		InputSchema json.RawMessage `json:"input_schema"`
	} `json:"tools"`
}

// wireMessage is a message with its content normalized to blocks.
type wireMessage struct {
	Role   string
	Blocks []wireBlock
}

type wireBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// ResultText is a tool_result block's text, whatever form its content has.
func (b wireBlock) ResultText() string {
	var s string
	if json.Unmarshal(b.Content, &s) == nil {
		return s
	}
	var parts []wireBlock
	_ = json.Unmarshal(b.Content, &parts)
	var out []string
	for _, p := range parts {
		if p.Type == "text" {
			out = append(out, p.Text)
		}
	}
	return strings.Join(out, "\n")
}

// errorf records a problem with this request.
func (r *apiRequest) errorf(format string, args ...any) {
	r.m.addProblem(fmt.Sprintf("request %d: ", r.N) + fmt.Sprintf(format, args...))
}

// Last is the last message of the request.
func (r *apiRequest) Last() wireMessage {
	if len(r.Messages) == 0 {
		return wireMessage{}
	}
	return r.Messages[len(r.Messages)-1]
}

// ToolResult returns the tool_result for id in the last message.
func (r *apiRequest) ToolResult(id string) (wireBlock, bool) {
	for _, b := range r.Last().Blocks {
		if b.Type == "tool_result" && b.ToolUseID == id {
			return b, true
		}
	}
	return wireBlock{}, false
}

// UserTexts are the text blocks of every user message, in order.
func (r *apiRequest) UserTexts() []string {
	var out []string
	for _, m := range r.Messages {
		if m.Role != "user" {
			continue
		}
		for _, b := range m.Blocks {
			if b.Type == "text" {
				out = append(out, b.Text)
			}
		}
	}
	return out
}

// reply is what the mock answers: an HTTP error, or a streamed message of
// text and tool_use blocks.
type reply struct {
	status  int
	errType string
	errMsg  string
	blocks  []replyBlock
}

type replyBlock struct {
	text  string
	id    string
	name  string
	input any
}

// Usage the mock reports for every successful call.
const (
	mockInputTokens  = 120
	mockOutputTokens = 30
)

func textReply(text string) reply { return reply{blocks: []replyBlock{{text: text}}} }

func toolReply(text string, uses ...replyBlock) reply {
	var r reply
	if text != "" {
		r.blocks = append(r.blocks, replyBlock{text: text})
	}
	r.blocks = append(r.blocks, uses...)
	return r
}

func toolUse(id, name string, input any) replyBlock {
	return replyBlock{id: id, name: name, input: input}
}

func errorReply(status int, typ, msg string) reply {
	return reply{status: status, errType: typ, errMsg: msg}
}

// newMock starts a mock that answers requests with steps, in order.
func newMock(t *testing.T, steps ...step) *mockAPI {
	t.Helper()
	m := &mockAPI{steps: steps}
	m.srv = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(func() {
		m.srv.Close()
		for _, p := range m.Problems() {
			t.Error("mock API: " + p)
		}
	})
	return m
}

// setHandler answers every request with h instead of the steps.
func (m *mockAPI) setHandler(h step) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handler = h
}

// addSteps appends steps to the script.
func (m *mockAPI) addSteps(steps ...step) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.steps = append(m.steps, steps...)
}

// URL is the base URL to give gta as ANTHROPIC_BASE_URL.
func (m *mockAPI) URL() string { return m.srv.URL }

func (m *mockAPI) addProblem(p string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.problems = append(m.problems, p)
}

// Problems are the problems found so far.
func (m *mockAPI) Problems() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.problems...)
}

// Requests are the requests received so far.
func (m *mockAPI) Requests() []*apiRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*apiRequest(nil), m.reqs...)
}

func (m *mockAPI) serve(w http.ResponseWriter, hr *http.Request) {
	body, err := io.ReadAll(hr.Body)
	if err != nil {
		m.addProblem("reading body: " + err.Error())
		return
	}
	m.mu.Lock()
	r := &apiRequest{m: m, N: len(m.reqs), Raw: body, Header: hr.Header.Clone()}
	m.reqs = append(m.reqs, r)
	var next step
	switch {
	case m.handler != nil:
		next = m.handler
	case r.N < len(m.steps):
		next = m.steps[r.N]
	}
	m.mu.Unlock()

	if hr.Method != http.MethodPost || hr.URL.Path != "/v1/messages" {
		r.errorf("unexpected %s %s", hr.Method, hr.URL.Path)
		writeAPIError(w, 404, "not_found_error", "no such endpoint")
		return
	}
	// The SDK sends the key as x-api-key or as a bearer token.
	if hr.Header.Get("X-Api-Key") != fakeAPIKey && hr.Header.Get("Authorization") != "Bearer "+fakeAPIKey {
		r.errorf("the request does not carry the fake test key")
		writeAPIError(w, 401, "authentication_error", "invalid x-api-key")
		return
	}
	if msg := r.decode(); msg != "" {
		r.errorf("%s", msg)
		writeAPIError(w, 400, "invalid_request_error", msg)
		return
	}
	if next == nil {
		r.errorf("unexpected extra request (script has %d steps)", len(m.steps))
		writeAPIError(w, 500, "api_error", "the e2e script has no step for this request")
		return
	}
	rep := next(r)
	if rep.status != 0 {
		writeAPIError(w, rep.status, rep.errType, rep.errMsg)
		return
	}
	writeStream(w, r.Model, rep)
}

// decode parses the body and validates it like the real API would. It
// returns a description of the first problem, or "".
func (r *apiRequest) decode() string {
	var wr wireRequest
	dec := json.NewDecoder(bytes.NewReader(r.Raw))
	if err := dec.Decode(&wr); err != nil {
		return "body is not JSON: " + err.Error()
	}
	r.Model, r.Stream = wr.Model, wr.Stream
	if wr.Model == "" {
		return "model is empty"
	}
	if !wr.Stream {
		return "stream is not true"
	}
	if wr.MaxTokens <= 0 {
		return "max_tokens is missing"
	}
	if len(wr.System) == 0 {
		return "system prompt is missing"
	}
	for _, tl := range wr.Tools {
		r.Tools = append(r.Tools, tl.Name)
		if tl.Name != "" && len(tl.InputSchema) == 0 {
			return "tool " + tl.Name + " has no input_schema"
		}
	}
	for _, want := range requiredTools {
		if !contains(r.Tools, want) {
			return fmt.Sprintf("tool %s is not offered (tools: %v)", want, r.Tools)
		}
	}
	if len(wr.Messages) == 0 {
		return "messages is empty"
	}
	for i, wm := range wr.Messages {
		msg := wireMessage{Role: wm.Role}
		var s string
		if json.Unmarshal(wm.Content, &s) == nil {
			msg.Blocks = []wireBlock{{Type: "text", Text: s}}
		} else if err := json.Unmarshal(wm.Content, &msg.Blocks); err != nil {
			return fmt.Sprintf("messages[%d].content: %v", i, err)
		}
		want := "user"
		if i%2 == 1 {
			want = "assistant"
		}
		if wm.Role != want {
			return fmt.Sprintf("messages[%d].role = %q, want %q (roles must alternate)", i, wm.Role, want)
		}
		if len(msg.Blocks) == 0 {
			return fmt.Sprintf("messages[%d] has no content", i)
		}
		r.Messages = append(r.Messages, msg)
	}
	// Every tool_use must be answered by a tool_result with its id in the
	// next message, and every tool_result must answer one.
	for i, msg := range r.Messages {
		if msg.Role != "user" {
			continue
		}
		pending := map[string]bool{}
		if i > 0 {
			for _, b := range r.Messages[i-1].Blocks {
				if b.Type == "tool_use" {
					if b.ID == "" || b.Name == "" {
						return fmt.Sprintf("messages[%d] has a tool_use without id or name", i-1)
					}
					pending[b.ID] = true
				}
			}
		}
		for _, b := range msg.Blocks {
			if b.Type != "tool_result" {
				continue
			}
			if !pending[b.ToolUseID] {
				return fmt.Sprintf("messages[%d] has a tool_result for %q, which the previous message did not ask for", i, b.ToolUseID)
			}
			delete(pending, b.ToolUseID)
		}
		for id := range pending {
			return fmt.Sprintf("messages[%d] lacks the tool_result for %q", i, id)
		}
	}
	if r.Last().Role != "user" {
		return "the last message is not from the user"
	}
	return ""
}

func writeAPIError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": typ, "message": msg}})
	_, _ = w.Write(b)
}

// writeStream answers with a Messages API event stream for rep.
func writeStream(w http.ResponseWriter, model string, rep reply) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	send := func(v map[string]any) {
		data, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", v["type"], data)
		if flusher != nil {
			flusher.Flush()
		}
	}
	send(map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_e2e", "type": "message", "role": "assistant", "model": model,
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": mockInputTokens, "output_tokens": 1},
	}})
	stop := "end_turn"
	for i, b := range rep.blocks {
		if b.name == "" {
			send(map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "text", "text": ""}})
			// Two deltas, to exercise accumulation.
			half := len(b.text) / 2
			for _, part := range []string{b.text[:half], b.text[half:]} {
				send(map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "text_delta", "text": part}})
			}
		} else {
			stop = "tool_use"
			send(map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "tool_use", "id": b.id, "name": b.name, "input": map[string]any{}}})
			in, _ := json.Marshal(b.input)
			half := len(in) / 2
			for _, part := range []string{string(in[:half]), string(in[half:])} {
				send(map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": part}})
			}
		}
		send(map[string]any{"type": "content_block_stop", "index": i})
	}
	send(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": mockOutputTokens}})
	send(map[string]any{"type": "message_stop"})
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
