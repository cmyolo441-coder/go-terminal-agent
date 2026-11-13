package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

// sse renders chunks as an SSE body terminated by [DONE].
func sse(chunks ...string) string {
	var b strings.Builder
	for _, c := range chunks {
		fmt.Fprintf(&b, "data: %s\n\n", c)
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// recorder collects stream events in order.
type recorder struct {
	mu     sync.Mutex
	events []llm.Event
}

func (r *recorder) on(ev llm.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

// trace renders events compactly: T(text) R(thinking) S(name) D(bytes) |.
func (r *recorder) trace() string {
	var parts []string
	for _, ev := range r.events {
		switch ev.Type {
		case llm.EventTextDelta:
			parts = append(parts, "T("+ev.Text+")")
		case llm.EventThinkingDelta:
			parts = append(parts, "R("+ev.Text+")")
		case llm.EventToolUseStart:
			parts = append(parts, "S("+ev.ToolName+"#"+ev.ToolID+")")
		case llm.EventToolInputDelta:
			parts = append(parts, fmt.Sprintf("D(%s:%d)", ev.ToolID, ev.Bytes))
		case llm.EventBlockStop:
			parts = append(parts, "|")
		}
	}
	return strings.Join(parts, " ")
}

// newTest builds a provider for srv whose retry sleeps are recorded, not slept.
func newTest(t *testing.T, srv *httptest.Server, opts Options) (*Provider, *[]time.Duration) {
	t.Helper()
	opts.BaseURL = srv.URL + "/v1"
	if opts.Name == "" {
		opts.Name = "test"
	}
	p, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	var sleeps []time.Duration
	p.sleep = func(ctx context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return ctx.Err()
	}
	p.jitter = func() float64 { return 0 }
	return p, &sleeps
}

func hi(model string) llm.Request {
	return llm.Request{Model: model, MaxTokens: 1000, Messages: []llm.Message{llm.UserText("hi")}}
}

func TestStreamText(t *testing.T) {
	var gotBody []byte
	var gotHeaders http.Header
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotHeaders, gotPath = r.Header.Clone(), r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse(
			`{"id":"c1","model":"gpt-x-2026","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
			`{"id":"c1","model":"gpt-x-2026","choices":[{"index":0,"delta":{"content":"Hello "},"finish_reason":null}]}`,
			`{"id":"c1","model":"gpt-x-2026","choices":[{"index":0,"delta":{"content":"world."},"finish_reason":null}]}`,
			`{"id":"c1","model":"gpt-x-2026","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`{"id":"c1","model":"gpt-x-2026","choices":[],"usage":{"prompt_tokens":1200,"completion_tokens":7,"total_tokens":1207,"prompt_tokens_details":{"cached_tokens":1000}}}`,
		))
	}))
	defer srv.Close()

	p, _ := newTest(t, srv, Options{Name: "openrouter", APIKey: "sk-test", ExtraHeaders: map[string]string{"X-Team": "core"}})
	rec := &recorder{}
	resp, err := p.Stream(context.Background(), hi("gpt-x"), rec.on)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.trace(); got != "T(Hello ) T(world.) |" {
		t.Fatalf("events: %s", got)
	}
	if resp.Message.Text() != "Hello world." || resp.Message.Provider != "openrouter" || resp.Message.Role != llm.RoleAssistant {
		t.Fatalf("message: %+v", resp.Message)
	}
	if resp.StopReason != llm.StopEndTurn || resp.Model != "gpt-x-2026" {
		t.Fatalf("resp: %+v", resp)
	}
	if resp.Usage != (llm.Usage{InputTokens: 200, OutputTokens: 7, CacheReadTokens: 1000}) {
		t.Fatalf("usage: %+v", resp.Usage)
	}
	if gotPath != "/v1/chat/completions" || gotHeaders.Get("Authorization") != "Bearer sk-test" ||
		gotHeaders.Get("X-Team") != "core" || gotHeaders.Get("Accept") != "text/event-stream" ||
		!strings.HasPrefix(gotHeaders.Get("User-Agent"), "gta/") {
		t.Fatalf("request: path=%s headers=%v", gotPath, gotHeaders)
	}
	var body map[string]any
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatal(err)
	}
	if body["stream"] != true || body["max_tokens"] != float64(1000) || body["model"] != "gpt-x" {
		t.Fatalf("body: %s", gotBody)
	}
}

// Two parallel calls whose argument fragments interleave by index.
func TestStreamParallelToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		io.WriteString(w, sse(
			`{"choices":[{"index":0,"delta":{"content":"Checking."}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read_file","arguments":""}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"bash","arguments":"{\"comm"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"and\":\"go test\"}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":" \"main.go\"}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		))
	}))
	defer srv.Close()

	p, _ := newTest(t, srv, Options{})
	rec := &recorder{}
	resp, err := p.Stream(context.Background(), hi("m"), rec.on)
	if err != nil {
		t.Fatal(err)
	}
	want := "T(Checking.) | S(read_file#call_a) | S(bash#call_b) D(call_b:6) D(call_a:8) D(call_b:15) D(call_a:11) |"
	if got := rec.trace(); got != want {
		t.Fatalf("events\n got: %s\nwant: %s", got, want)
	}
	if resp.StopReason != llm.StopToolUse {
		t.Fatalf("stop: %s", resp.StopReason)
	}
	uses := resp.Message.ToolUses()
	if len(uses) != 2 ||
		uses[0].ToolUseID != "call_a" || uses[0].ToolName != "read_file" || string(uses[0].Input) != `{"path":"main.go"}` ||
		uses[1].ToolUseID != "call_b" || uses[1].ToolName != "bash" || string(uses[1].Input) != `{"command":"go test"}` {
		t.Fatalf("tool uses: %+v", uses)
	}
	if resp.Message.Blocks[0].Type != llm.BlockText || resp.Usage != (llm.Usage{}) {
		t.Fatalf("blocks/usage: %+v %+v", resp.Message.Blocks, resp.Usage)
	}
	// The produced message replays cleanly through this provider.
	msgs, err := toMessages("", []llm.Message{llm.UserText("go"), resp.Message})
	if err != nil || len(msgs) != 4 || len(msgs[1].ToolCalls) != 2 {
		t.Fatalf("replay: %+v %v", msgs, err)
	}
}

// Servers that omit the index (or reuse index 0) and send whole calls at once,
// and that report "stop" despite tool calls.
func TestStreamToolCallsWithoutIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse(
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"id":"g1","function":{"name":"ls","arguments":"{}"}},{"id":"g2","function":{"name":"pwd","arguments":{"deep":true}}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"g3","function":{"name":"cat","arguments":"{\"f\":1"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"}"}}]},"finish_reason":"stop"}]}`,
		))
	}))
	defer srv.Close()
	p, _ := newTest(t, srv, Options{})
	resp, err := p.Stream(context.Background(), hi("m"), nil)
	if err != nil {
		t.Fatal(err)
	}
	uses := resp.Message.ToolUses()
	if resp.StopReason != llm.StopToolUse || len(uses) != 3 ||
		uses[0].ToolName != "ls" || uses[1].ToolName != "pwd" || string(uses[1].Input) != `{"deep":true}` ||
		uses[2].ToolUseID != "g3" || string(uses[2].Input) != `{"f":1}` {
		t.Fatalf("resp: %+v uses: %+v", resp, uses)
	}
}

func TestStreamReasoningAndFinishReasons(t *testing.T) {
	cases := []struct {
		name, finish string
		want         llm.StopReason
		reasoning    string // field name carrying reasoning
	}{
		{"stop", "stop", llm.StopEndTurn, "reasoning_content"},
		{"length", "length", llm.StopMaxTokens, "reasoning"},
		{"filter", "content_filter", llm.StopRefusal, "reasoning_content"},
		{"other", "something_new", llm.StopOther, "reasoning"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, sse(
					`{"choices":[{"index":0,"delta":{"`+c.reasoning+`":"Think "}}]}`,
					`{"choices":[{"index":0,"delta":{"`+c.reasoning+`":"hard."}}]}`,
					`{"choices":[{"index":0,"delta":{"content":"Done."},"finish_reason":"`+c.finish+`"}]}`,
				))
			}))
			defer srv.Close()
			p, _ := newTest(t, srv, Options{})
			rec := &recorder{}
			resp, err := p.Stream(context.Background(), hi("m"), rec.on)
			if err != nil {
				t.Fatal(err)
			}
			if got := rec.trace(); got != "R(Think ) R(hard.) | T(Done.) |" {
				t.Fatalf("events: %s", got)
			}
			if resp.StopReason != c.want {
				t.Fatalf("stop: %s want %s", resp.StopReason, c.want)
			}
			if c.want == llm.StopRefusal && resp.StopDetail == "" {
				t.Fatal("content filter needs a stop detail")
			}
			b := resp.Message.Blocks
			if len(b) != 2 || b[0].Type != llm.BlockThinking || b[0].Text != "Think hard." || b[1].Text != "Done." {
				t.Fatalf("blocks: %+v", b)
			}
		})
	}
}

func TestStreamRefusalAndDeepSeekUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse(
			`{"choices":[{"index":0,"delta":{"refusal":"I can't help with that."},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":50,"completion_tokens":5,"prompt_cache_hit_tokens":30,"prompt_cache_miss_tokens":20}}`,
		))
	}))
	defer srv.Close()
	p, _ := newTest(t, srv, Options{})
	resp, err := p.Stream(context.Background(), hi("m"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != llm.StopRefusal || resp.StopDetail != "I can't help with that." {
		t.Fatalf("refusal: %+v", resp)
	}
	if resp.Usage != (llm.Usage{InputTokens: 20, OutputTokens: 5, CacheReadTokens: 30}) {
		t.Fatalf("usage: %+v", resp.Usage)
	}
}

// NVIDIA NIM end to end: Bearer key from NVIDIA_API_KEY, reasoning_content,
// text and a tool call, and no usage chunk.
func TestNvidiaPresetStream(t *testing.T) {
	t.Setenv("NVIDIA_API_KEY", "nvapi-unit-test")
	var auth, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, path = r.Header.Get("Authorization"), r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse(
			`{"model":"kimi-k3.1-pro","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"Need the file."}}]}`,
			`{"model":"kimi-k3.1-pro","choices":[{"index":0,"delta":{"content":"Reading it."}}]}`,
			`{"model":"kimi-k3.1-pro","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"chatcmpl-tool-1","type":"function","function":{"name":"read_file","arguments":"{\"path\": \"go.mod\"}"}}]}}]}`,
			`{"model":"kimi-k3.1-pro","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		))
	}))
	defer srv.Close()

	opts, err := Preset("nvidia")
	if err != nil {
		t.Fatal(err)
	}
	opts.BaseURL = srv.URL + "/v1" // config base_url override
	p, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "nvidia" {
		t.Fatalf("name: %s", p.Name())
	}
	rec := &recorder{}
	resp, err := p.Stream(context.Background(), hi("kimi-k3.1-pro"), rec.on)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer nvapi-unit-test" || path != "/v1/chat/completions" {
		t.Fatalf("auth=%q path=%q", auth, path)
	}
	if got := rec.trace(); got != "R(Need the file.) | T(Reading it.) | S(read_file#chatcmpl-tool-1) D(chatcmpl-tool-1:18) |" {
		t.Fatalf("events: %s", got)
	}
	uses := resp.Message.ToolUses()
	if resp.StopReason != llm.StopToolUse || len(uses) != 1 || string(uses[0].Input) != `{"path":"go.mod"}` ||
		resp.Message.Provider != "nvidia" || resp.Model != "kimi-k3.1-pro" || resp.Usage != (llm.Usage{}) {
		t.Fatalf("resp: %+v", resp)
	}
}

func TestRetryOn429ThenSuccess(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"message":"Rate limit reached","type":"requests","code":"rate_limit_exceeded"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse(`{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	p, sleeps := newTest(t, srv, Options{})
	resp, err := p.Stream(context.Background(), hi("m"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Text() != "ok" || calls.Load() != 2 || len(*sleeps) != 1 || (*sleeps)[0] != 2*time.Second {
		t.Fatalf("calls=%d sleeps=%v resp=%+v", calls.Load(), *sleeps, resp)
	}
}

func TestRetryExhaustedOn5xxWithBackoff(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "<html>upstream down</html>", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	p, sleeps := newTest(t, srv, Options{MaxRetries: 3})
	_, err := p.Stream(context.Background(), hi("m"), nil)
	var ae *APIError
	if !errors.As(err, &ae) || ae.StatusCode != 503 || ae.Attempts != 4 || !strings.Contains(err.Error(), "server error (503) after 4 attempts") {
		t.Fatalf("err: %v", err)
	}
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second}
	if calls.Load() != 4 || fmt.Sprint(*sleeps) != fmt.Sprint(want) {
		t.Fatalf("calls=%d sleeps=%v", calls.Load(), *sleeps)
	}
}

func TestNoRetryOnClientErrors(t *testing.T) {
	cases := []struct {
		status int
		body   string
		header map[string]string
		want   []string
	}{
		{401, `{"error":{"message":"Incorrect API key provided","type":"invalid_request_error","code":"invalid_api_key"}}`, nil,
			[]string{"authentication failed (401)", "set OPENAI_API_KEY", "Incorrect API key"}},
		{404, `{"error":{"message":"The model does not exist","code":"model_not_found"}}`, nil,
			[]string{"not found (404)", `"gpt-nope"`, "/v1"}},
		{429, `{"error":{"message":"You exceeded your current quota","type":"insufficient_quota","code":"insufficient_quota"}}`, nil,
			[]string{"quota exceeded (429)"}},
		{500, `{"error":{"message":"boom"}}`, map[string]string{"X-Should-Retry": "false"},
			[]string{"server error (500)"}},
	}
	for _, c := range cases {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			for k, v := range c.header {
				w.Header().Set(k, v)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(c.status)
			io.WriteString(w, c.body)
		}))
		p, sleeps := newTest(t, srv, Options{Name: "openai", APIKey: "bad", APIKeyEnv: "OPENAI_API_KEY"})
		_, err := p.Stream(context.Background(), hi("gpt-nope"), nil)
		srv.Close()
		if err == nil {
			t.Fatalf("%d: expected error", c.status)
		}
		for _, w := range c.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%d: %q missing %q", c.status, err, w)
			}
		}
		if calls.Load() != 1 || len(*sleeps) != 0 {
			t.Errorf("%d: must not retry (calls=%d)", c.status, calls.Load())
		}
	}
}

func TestNoRetryMidStream(t *testing.T) {
	t.Run("connection drops", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: "+`{"choices":[{"index":0,"delta":{"content":"partial"}}]}`+"\n\n")
			w.(http.Flusher).Flush()
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
		}))
		defer srv.Close()
		p, sleeps := newTest(t, srv, Options{MaxRetries: 5})
		rec := &recorder{}
		_, err := p.Stream(context.Background(), hi("m"), rec.on)
		if err == nil || !(strings.Contains(err.Error(), "interrupted") || strings.Contains(err.Error(), "ended before")) {
			t.Fatalf("err: %v", err)
		}
		if calls.Load() != 1 || len(*sleeps) != 0 || rec.trace() != "T(partial)" {
			t.Fatalf("calls=%d sleeps=%v events=%s", calls.Load(), *sleeps, rec.trace())
		}
	})
	t.Run("in-band error", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, sse(
				`{"choices":[{"index":0,"delta":{"content":"a"}}]}`,
				`{"error":{"message":"Upstream overloaded","code":502}}`,
			))
		}))
		defer srv.Close()
		p, _ := newTest(t, srv, Options{MaxRetries: 5})
		_, err := p.Stream(context.Background(), hi("m"), nil)
		var ae *APIError
		if !errors.As(err, &ae) || !ae.Streaming || ae.StatusCode != 502 || !strings.Contains(err.Error(), "mid-stream (502): Upstream overloaded") {
			t.Fatalf("err: %v", err)
		}
		if calls.Load() != 1 {
			t.Fatalf("calls=%d", calls.Load())
		}
	})
	t.Run("truncated without finish", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: "+`{"choices":[{"index":0,"delta":{"content":"a"}}]}`+"\n\n")
		}))
		defer srv.Close()
		p, _ := newTest(t, srv, Options{})
		if _, err := p.Stream(context.Background(), hi("m"), nil); err == nil || !strings.Contains(err.Error(), "ended before the model finished") {
			t.Fatalf("err: %v", err)
		}
	})
	t.Run("malformed chunk", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {not json\n\n")
		}))
		defer srv.Close()
		p, _ := newTest(t, srv, Options{})
		if _, err := p.Stream(context.Background(), hi("m"), nil); err == nil || !strings.Contains(err.Error(), "malformed stream chunk") {
			t.Fatalf("err: %v", err)
		}
	})
}

// blockingServer sends one chunk, then holds the stream open until the client
// goes away or the test ends.
func blockingServer(t *testing.T) *httptest.Server {
	t.Helper()
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: "+`{"choices":[{"index":0,"delta":{"content":"first"}}]}`+"\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-done:
		}
	}))
	t.Cleanup(func() { close(done); srv.Close() })
	return srv
}

func TestCancellationMidStream(t *testing.T) {
	srv := blockingServer(t)
	p, _ := newTest(t, srv, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	_, err := p.Stream(ctx, hi("m"), func(ev llm.Event) {
		if ev.Type == llm.EventTextDelta {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("cancellation was not prompt")
	}
}

func TestCancellationDuringRetryWait(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	p, _ := newTest(t, srv, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	p.sleep = func(ctx context.Context, d time.Duration) error {
		cancel()
		return sleepCtx(ctx, d)
	}
	if _, err := p.Stream(ctx, hi("m"), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err: %v", err)
	}
}

func TestIdleTimeout(t *testing.T) {
	srv := blockingServer(t)
	p, _ := newTest(t, srv, Options{IdleTimeout: 50 * time.Millisecond})
	_, err := p.Stream(context.Background(), hi("m"), nil)
	if err == nil || !strings.Contains(err.Error(), "stream stalled") {
		t.Fatalf("err: %v", err)
	}
}

func TestHeaderTimeoutIsRetriedThenReported(t *testing.T) {
	done := make(chan struct{})
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case <-r.Context().Done():
		case <-done:
		}
	}))
	defer srv.Close()
	defer close(done)
	p, sleeps := newTest(t, srv, Options{Timeout: 30 * time.Millisecond, MaxRetries: 1})
	_, err := p.Stream(context.Background(), hi("m"), nil)
	var ce *ConnError
	if !errors.As(err, &ce) || ce.Timeout != 30*time.Millisecond || !strings.Contains(err.Error(), "no response from") || !strings.Contains(err.Error(), "2 attempts") {
		t.Fatalf("err: %v", err)
	}
	if calls.Load() != 2 || len(*sleeps) != 1 {
		t.Fatalf("calls=%d sleeps=%v", calls.Load(), *sleeps)
	}
}

func TestConnectionRefusedIsRetriedThenReported(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // nothing listens on this address now
	p, sleeps := newTest(t, srv, Options{Name: "ollama"})
	_, err := p.Stream(context.Background(), hi("llama"), nil)
	var ce *ConnError
	if !errors.As(err, &ce) || !strings.Contains(err.Error(), "ollama: cannot reach") || !strings.Contains(err.Error(), "is the server running?") {
		t.Fatalf("err: %v", err)
	}
	if len(*sleeps) != DefaultMaxRetries {
		t.Fatalf("sleeps: %v", *sleeps)
	}
}

// Some servers ignore stream:true and answer with one JSON document.
func TestNonStreamingJSONReply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","model":"local-7b","choices":[{"index":0,"message":{"role":"assistant","content":"Sure.","tool_calls":[
			{"id":"c1","type":"function","function":{"name":"ls","arguments":"{}"}},
			{"id":"c2","type":"function","function":{"name":"pwd","arguments":""}}]},"finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":10,"completion_tokens":3}}`)
	}))
	defer srv.Close()
	p, _ := newTest(t, srv, Options{})
	rec := &recorder{}
	resp, err := p.Stream(context.Background(), hi("m"), rec.on)
	if err != nil {
		t.Fatal(err)
	}
	uses := resp.Message.ToolUses()
	if resp.Message.Text() != "Sure." || len(uses) != 2 || string(uses[1].Input) != "{}" || resp.Usage.InputTokens != 10 || resp.Model != "local-7b" {
		t.Fatalf("resp: %+v", resp)
	}
	if got := rec.trace(); got != "T(Sure.) | S(ls#c1) D(c1:2) | S(pwd#c2) |" {
		t.Fatalf("events: %s", got)
	}
}

func TestReadSSEEdgeCases(t *testing.T) {
	body := ": keep-alive\r\n" +
		"event: message\r\n" +
		"data: {\"a\":1}\r\n\r\n" +
		"id: 7\n" +
		"data: {\"b\":\n" + // multi-line data joins with a newline
		"data: 2}\n\n" +
		"data:{\"c\":3}\n" + // no blank line between events, no space after colon
		"data: {\"d\":4}\n\n" +
		"data: {\"e\":5}" // final event without trailing newline
	var got []string
	err := readSSE(strings.NewReader(body), func() {}, func(d []byte) (bool, error) {
		got = append(got, string(d))
		return false, nil
	})
	want := []string{`{"a":1}`, "{\"b\":\n2}", `{"c":3}`, `{"d":4}`, `{"e":5}`}
	if err != nil || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %q err %v", got, err)
	}
}
