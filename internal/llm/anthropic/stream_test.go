package anthropic

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

// sse builds a Messages API event stream.
func sse(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		typ := e[strings.Index(e, `"type":"`)+8:]
		typ = typ[:strings.Index(typ, `"`)]
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", typ, e)
	}
	return b.String()
}

func TestStreamEndToEnd(t *testing.T) {
	var gotBody, gotBeta string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		gotBeta = r.Header.Get("anthropic-beta")
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse(
			`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Plan."}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hello "}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"there."}}`,
			`{"type":"content_block_stop","index":1}`,
			`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_9","name":"read_file","input":{}}}`,
			`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"path\": "}}`,
			`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"go.mod\"}"}}`,
			`{"type":"content_block_stop","index":2}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":42}}`,
			`{"type":"message_stop"}`,
		))
	}))
	defer srv.Close()

	p := New(Options{APIKey: "test", BaseURL: srv.URL, Fallbacks: true})
	var text, thinking strings.Builder
	var started []string
	resp, err := p.Stream(context.Background(), llm.Request{
		Model: "claude-opus-5-5", MaxTokens: 1024, Messages: []llm.Message{llm.UserText("hi")},
	}, func(ev llm.Event) {
		switch ev.Type {
		case llm.EventTextDelta:
			text.WriteString(ev.Text)
		case llm.EventThinkingDelta:
			thinking.WriteString(ev.Text)
		case llm.EventToolUseStart:
			started = append(started, ev.ToolName)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if text.String() != "Hello there." || thinking.String() != "Plan." || len(started) != 1 {
		t.Fatalf("events: text=%q thinking=%q started=%v", text.String(), thinking.String(), started)
	}
	if resp.StopReason != llm.StopToolUse || resp.Usage.OutputTokens != 42 {
		t.Fatalf("resp: %+v", resp)
	}
	uses := resp.Message.ToolUses()
	if len(uses) != 1 || !strings.Contains(string(uses[0].Input), "go.mod") {
		t.Fatalf("tool use: %+v", uses)
	}
	if !strings.Contains(string(resp.Message.Native), `"signature":"sig"`) {
		t.Fatalf("native content lost signature: %s", resp.Message.Native)
	}
	if !strings.Contains(gotBody, `"stream":true`) || !strings.Contains(gotBeta, "server-side-fallback-2026-07-01") {
		t.Fatalf("request: beta=%q body=%s", gotBeta, gotBody)
	}
}

func TestStreamAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
	}))
	defer srv.Close()
	p := New(Options{APIKey: "bad", BaseURL: srv.URL})
	_, err := p.Stream(context.Background(), llm.Request{Model: "m", MaxTokens: 1024, Messages: []llm.Message{llm.UserText("hi")}}, nil)
	if err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("got %v", err)
	}
}
