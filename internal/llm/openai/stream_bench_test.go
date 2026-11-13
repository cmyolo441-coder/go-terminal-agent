package openai

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

// Benchmarks for streaming. They only run with -bench:
//
//	go test ./internal/llm/openai -run '^$' -bench . -benchmem

const (
	benchTextDeltas = 2000
	benchArgDeltas  = 400
)

// benchStream is a realistic reply: reasoning, 2000 small text deltas, a
// tool call streamed in 400 argument fragments, a finish chunk and usage.
func benchStream() []byte {
	var b bytes.Buffer
	words := strings.Fields("the quick brown fox jumps over a lazy dog while reading main.go and fixing tests")
	ev := func(s string) { fmt.Fprintf(&b, "data: %s\n\n", s) }
	ev(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-x-2026","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`)
	for i := 0; i < 50; i++ {
		ev(fmt.Sprintf(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-x-2026","choices":[{"index":0,"delta":{"reasoning_content":"%s "},"finish_reason":null}]}`, words[i%len(words)]))
	}
	for i := 0; i < benchTextDeltas; i++ {
		ev(fmt.Sprintf(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-x-2026","choices":[{"index":0,"delta":{"content":"%s "},"finish_reason":null}]}`, words[i%len(words)]))
	}
	ev(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-x-2026","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"write_file","arguments":""}}]},"finish_reason":null}]}`)
	ev(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-x-2026","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"main.go\",\"content\":\""}}]},"finish_reason":null}]}`)
	for i := 0; i < benchArgDeltas; i++ {
		ev(fmt.Sprintf(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-x-2026","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"%s "}}]},"finish_reason":null}]}`, words[i%len(words)]))
	}
	ev(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-x-2026","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"}"}}]},"finish_reason":null}]}`)
	ev(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-x-2026","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
	ev(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-x-2026","choices":[],"usage":{"prompt_tokens":12000,"completion_tokens":2500,"prompt_tokens_details":{"cached_tokens":10000}}}`)
	b.WriteString("data: [DONE]\n\n")
	return b.Bytes()
}

// BenchmarkReadStream parses and assembles a whole streamed reply; divide
// ns/op and allocs/op by about 2450 for the cost per delta.
func BenchmarkReadStream(b *testing.B) {
	body := benchStream()
	p, err := New(Options{Name: "bench", BaseURL: "http://127.0.0.1:1/v1", APIKey: "x"})
	if err != nil {
		b.Fatal(err)
	}
	p.idle = time.Minute
	events := 0
	onEvent := func(llm.Event) { events++ }
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ctx := context.Background()
		actx, cancel := context.WithCancelCause(ctx)
		resp := &http.Response{
			Header: http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:   io.NopCloser(bytes.NewReader(body)),
		}
		r, err := p.read(ctx, actx, cancel, resp, "gpt-x", onEvent)
		cancel(nil)
		if err != nil {
			b.Fatal(err)
		}
		if len(r.Message.Blocks) != 3 {
			b.Fatalf("blocks: %+v", r.Message.Blocks)
		}
	}
	b.ReportMetric(float64(events)/float64(b.N), "events/op")
}

// BenchmarkReadSSE is the event framing alone.
func BenchmarkReadSSE(b *testing.B) {
	body := benchStream()
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n := 0
		err := readSSE(bytes.NewReader(body), func() {}, func(data []byte) (bool, error) {
			n += len(data)
			return false, nil
		})
		if err != nil || n == 0 {
			b.Fatal(err)
		}
	}
}
