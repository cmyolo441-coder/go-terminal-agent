// Package mockapi is a scripted stand-in for the Messages API used by tests.
package mockapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
)

// Block is one content block of a scripted response.
type Block struct {
	Type  string // text | thinking | tool_use
	Text  string
	ID    string
	Name  string
	Input string // JSON for tool_use
}

// Response is one scripted assistant message.
type Response struct {
	Blocks     []Block
	StopReason string // defaults to end_turn, or tool_use when a tool block is present
	Status     int    // non-zero returns an HTTP error instead
}

// Server replays responses in order and records request bodies.
type Server struct {
	*httptest.Server
	mu        sync.Mutex
	responses []Response
	Requests  []map[string]any
}

// New starts a server that will answer with the given responses.
func New(responses ...Response) *Server {
	s := &Server{responses: responses}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	_ = json.Unmarshal(body, &req)
	s.mu.Lock()
	s.Requests = append(s.Requests, req)
	var resp Response
	if len(s.responses) > 0 {
		resp, s.responses = s.responses[0], s.responses[1:]
	} else {
		resp = Response{Blocks: []Block{{Type: "text", Text: "(mock: no more scripted responses)"}}}
	}
	s.mu.Unlock()

	if resp.Status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.Status)
		fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"scripted failure"}}`)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fl, _ := w.(http.Flusher)
	send := func(event string, data any) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if fl != nil {
			fl.Flush()
		}
	}
	model, _ := req["model"].(string)
	send("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_mock", "type": "message", "role": "assistant", "model": model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": 100, "output_tokens": 1, "cache_read_input_tokens": 50, "cache_creation_input_tokens": 0},
	}})
	stop := resp.StopReason
	for i, b := range resp.Blocks {
		var start, delta map[string]any
		switch b.Type {
		case "thinking":
			start = map[string]any{"type": "thinking", "thinking": "", "signature": ""}
			delta = map[string]any{"type": "thinking_delta", "thinking": b.Text}
		case "tool_use":
			start = map[string]any{"type": "tool_use", "id": b.ID, "name": b.Name, "input": map[string]any{}}
			delta = map[string]any{"type": "input_json_delta", "partial_json": b.Input}
			if stop == "" {
				stop = "tool_use"
			}
		default:
			start = map[string]any{"type": "text", "text": ""}
			delta = map[string]any{"type": "text_delta", "text": b.Text}
		}
		send("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": start})
		send("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": delta})
		if b.Type == "thinking" {
			send("content_block_delta", map[string]any{"type": "content_block_delta", "index": i,
				"delta": map[string]any{"type": "signature_delta", "signature": "sig-mock"}})
		}
		send("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
	}
	if stop == "" {
		stop = "end_turn"
	}
	send("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 25}})
	send("message_stop", map[string]any{"type": "message_stop"})
}

// Request returns the i-th recorded request body.
func (s *Server) Request(i int) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.Requests) {
		return nil
	}
	return s.Requests[i]
}

// Count returns how many requests were received.
func (s *Server) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Requests)
}
