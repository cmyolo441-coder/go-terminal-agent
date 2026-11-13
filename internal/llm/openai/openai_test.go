package openai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

func TestPresetResolution(t *testing.T) {
	for _, n := range PresetNames() {
		for _, env := range presets[n].KeyEnvs {
			t.Setenv(env, "")
		}
	}
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("OLLAMA_HOST", "")
	t.Setenv("OPENAI_API_KEY", "sk-openai")
	t.Setenv("NVIDIA_API_KEY", "nvapi-test")

	cases := []struct {
		name, base, key, env string
		require              bool
	}{
		{"openai", "https://api.openai.com/v1", "sk-openai", "OPENAI_API_KEY", true},
		{"OpenRouter", "https://openrouter.ai/api/v1", "", "OPENROUTER_API_KEY", true},
		{"ollama", "http://localhost:11434/v1", "", "OLLAMA_API_KEY", false},
		{"gemini", "https://generativelanguage.googleapis.com/v1beta/openai", "", "GEMINI_API_KEY", true},
		{"groq", "https://api.groq.com/openai/v1", "", "GROQ_API_KEY", true},
		{"deepseek", "https://api.deepseek.com/v1", "", "DEEPSEEK_API_KEY", true},
		{"together", "https://api.together.xyz/v1", "", "TOGETHER_API_KEY", true},
		{"lmstudio", "http://localhost:1234/v1", "", "LMSTUDIO_API_KEY", false},
		{"vllm", "http://localhost:8000/v1", "", "VLLM_API_KEY", false},
		{" nvidia ", "https://integrate.api.nvidia.com/v1", "nvapi-test", "NVIDIA_API_KEY", true},
	}
	for _, c := range cases {
		opts, err := Preset(c.name)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		want := strings.ToLower(strings.TrimSpace(c.name))
		if opts.Name != want || opts.BaseURL != c.base || opts.APIKey != c.key || opts.APIKeyEnv != c.env || opts.RequireAPIKey != c.require {
			t.Errorf("%s: got %+v", c.name, opts)
		}
		if _, err := New(opts); err != nil {
			t.Errorf("%s: New: %v", c.name, err)
		}
	}
	if _, err := Preset("nope"); err == nil || !strings.Contains(err.Error(), "nvidia") {
		t.Fatalf("unknown preset error should list known names: %v", err)
	}
	if len(PresetNames()) != len(cases) {
		t.Fatalf("preset names: %v", PresetNames())
	}
}

func TestPresetEnvOverrides(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "g-key")
	opts, _ := Preset("gemini")
	if opts.APIKey != "g-key" || opts.APIKeyEnv != "GOOGLE_API_KEY" {
		t.Fatalf("gemini fallback key: %+v", opts)
	}
	t.Setenv("OPENAI_BASE_URL", "https://gateway.example/v1")
	if opts, _ := Preset("openai"); opts.BaseURL != "https://gateway.example/v1" {
		t.Fatalf("OPENAI_BASE_URL ignored: %s", opts.BaseURL)
	}
	for in, want := range map[string]string{
		"127.0.0.1":                  "http://127.0.0.1:11434/v1",
		"0.0.0.0:9999":               "http://0.0.0.0:9999/v1",
		"https://ollama.internal/":   "https://ollama.internal:11434/v1",
		"http://box:11434/v1":        "http://box:11434/v1",
		"http://[::1]:11434/ollama/": "http://[::1]:11434/ollama/v1",
	} {
		t.Setenv("OLLAMA_HOST", in)
		if opts, _ := Preset("ollama"); opts.BaseURL != want {
			t.Errorf("OLLAMA_HOST=%q: got %s want %s", in, opts.BaseURL, want)
		}
	}
}

func TestNvidiaPresetKeyOnlyFromNvidiaEnv(t *testing.T) {
	t.Setenv("NVIDIA_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "sk-must-not-be-used")
	opts, err := Preset("nvidia")
	if err != nil {
		t.Fatal(err)
	}
	if opts.APIKey != "" || opts.APIKeyEnv != "NVIDIA_API_KEY" || !opts.RequireAPIKey {
		t.Fatalf("nvidia must read only NVIDIA_API_KEY: %+v", opts)
	}
	err = MissingKeyError(opts)
	if err == nil || !strings.Contains(err.Error(), "export NVIDIA_API_KEY") {
		t.Fatalf("missing key error: %v", err)
	}
	// Stream fails fast, before any network call.
	p, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	p.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("no request may be sent without a key")
		return nil, nil
	})}
	_, err = p.Stream(context.Background(), llm.Request{Model: "kimi-k3.1-pro", Messages: []llm.Message{llm.UserText("hi")}}, nil)
	if err == nil || !strings.Contains(err.Error(), "export NVIDIA_API_KEY") {
		t.Fatalf("stream without key: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPresetRegistry(t *testing.T) {
	want := []string{"mistralai/mistral-large-4-1t-a52b", "kimi-k3.1-pro"}
	if got := KnownModels("NVIDIA"); !reflect.DeepEqual(got, want) {
		t.Fatalf("known models: %v", got)
	}
	info, ok := LookupPreset("nvidia")
	if !ok || info.Name != "nvidia" || info.Description != "NVIDIA NIM" || info.BaseURL != "https://integrate.api.nvidia.com/v1" {
		t.Fatalf("lookup: %+v", info)
	}
	info.KnownModels[0] = "mutated"
	if KnownModels("nvidia")[0] != want[0] {
		t.Fatal("LookupPreset must return copies")
	}
	all := Presets()
	if len(all) != len(PresetNames()) || all[0].Name != PresetNames()[0] {
		t.Fatalf("presets: %+v", all)
	}
	if _, ok := LookupPreset("missing"); ok || KnownModels("missing") != nil {
		t.Fatal("unknown preset must not resolve")
	}
}

func TestNewValidates(t *testing.T) {
	bad := []Options{
		{},
		{BaseURL: "ftp://x"},
		{BaseURL: "localhost:11434"},
		{BaseURL: "http://"},
		{BaseURL: "http://x", ExtraHeaders: map[string]string{"Bad Header": "v"}},
		{BaseURL: "http://x", ExtraHeaders: map[string]string{"X-Ok": "a\r\nb"}},
	}
	for _, o := range bad {
		if _, err := New(o); err == nil {
			t.Errorf("expected error for %+v", o)
		}
	}
	p, err := New(Options{BaseURL: " http://host:1/v1/ "})
	if err != nil || p.Endpoint() != "http://host:1/v1/chat/completions" || p.Name() != DefaultName {
		t.Fatalf("p=%+v err=%v", p, err)
	}
	p, _ = New(Options{BaseURL: "http://host/v1/chat/completions", MaxRetries: -1, Timeout: -1, IdleTimeout: -1})
	if p.Endpoint() != "http://host/v1/chat/completions" || p.retries != 0 || p.timeout != 0 || p.idle != 0 {
		t.Fatalf("p=%+v", p)
	}
	p, _ = New(Options{BaseURL: "http://host"})
	if p.retries != DefaultMaxRetries || p.timeout != DefaultTimeout || p.idle != DefaultIdleTimeout {
		t.Fatalf("defaults not applied: %+v", p)
	}
	if _, err := p.Stream(context.Background(), llm.Request{}, nil); err == nil || !strings.Contains(err.Error(), "no model") {
		t.Fatalf("empty model: %v", err)
	}
}

func TestToMessages(t *testing.T) {
	hist := []llm.Message{
		llm.UserText("fix the bug"),
		{Role: llm.RoleAssistant, Provider: "anthropic", Native: json.RawMessage(`[{"type":"thinking","thinking":"secret","signature":"sig"}]`), Blocks: []llm.Block{
			{Type: llm.BlockThinking, Text: "secret plan"},
			{Type: llm.BlockText, Text: "Looking."},
			{Type: llm.BlockServerTool, ToolUseID: "srv_1", ToolName: "web_search", Input: json.RawMessage(`{"q":"x"}`)},
			{Type: llm.BlockToolUse, ToolUseID: "t1", ToolName: "read_file", Input: json.RawMessage(`{ "path" : "a.go" }`)},
			{Type: llm.BlockToolUse, ToolUseID: "t2", ToolName: "bash", Input: json.RawMessage(`{"command":"go test"}`)},
		}},
		{Role: llm.RoleUser, Blocks: []llm.Block{
			{Type: llm.BlockToolResult, ToolUseID: "t1", Text: "package a"},
			{Type: llm.BlockToolResult, ToolUseID: "t2", Text: "exit status 1", IsError: true},
		}},
		{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockToolUse, ToolUseID: "t3", ToolName: "ls"}}},
		// t3 was interrupted: no result before the next user text.
		{Role: llm.RoleUser, Blocks: []llm.Block{
			{Type: llm.BlockToolResult, ToolUseID: "stale", Text: "old"},
			{Type: llm.BlockText, Text: "carry on"},
		}},
		{Role: llm.RoleAssistant},
		{Role: llm.RoleUser},
	}
	msgs, err := toMessages("be careful", hist)
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := json.Marshal(msgs)
	got := string(wire)
	want := `[` +
		`{"role":"system","content":"be careful"},` +
		`{"role":"user","content":"fix the bug"},` +
		`{"role":"assistant","content":"Looking.","tool_calls":[` +
		`{"id":"t1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.go\"}"}},` +
		`{"id":"t2","type":"function","function":{"name":"bash","arguments":"{\"command\":\"go test\"}"}}]},` +
		`{"role":"tool","content":"package a","tool_call_id":"t1"},` +
		`{"role":"tool","content":"Error: exit status 1","tool_call_id":"t2"},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"t3","type":"function","function":{"name":"ls","arguments":"{}"}}]},` +
		`{"role":"tool","content":"` + interruptedTool + `","tool_call_id":"t3"},` +
		`{"role":"user","content":"Result of an earlier tool call (stale):\nold\n\ncarry on"},` +
		`{"role":"assistant","content":"(no content)"}` +
		`]`
	if got != want {
		t.Fatalf("history mismatch\n got: %s\nwant: %s", got, want)
	}
	if strings.Contains(got, "secret") || strings.Contains(got, "web_search") {
		t.Fatal("thinking, native and server-tool content must be dropped")
	}

	// A trailing unanswered call is closed too.
	msgs, _ = toMessages("", []llm.Message{{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockToolUse, ToolUseID: "x", ToolName: "ls"}}}})
	if len(msgs) != 2 || msgs[1].Role != "tool" || msgs[1].ToolCallID != "x" {
		t.Fatalf("trailing call: %+v", msgs)
	}
	if _, err := toMessages("", []llm.Message{{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockToolUse, ToolUseID: "x", ToolName: "ls", Input: json.RawMessage(`{bad`)}}}}); err == nil {
		t.Fatal("invalid tool input must be reported")
	}
}

func TestBuildRequest(t *testing.T) {
	req := llm.Request{
		Model: "gpt-5", System: "sys", MaxTokens: 4096, Effort: "xhigh",
		Messages: []llm.Message{llm.UserText("hi")},
		Tools: []llm.ToolSpec{
			{Name: "read_file", Description: "Read a file", Properties: map[string]any{"path": map[string]any{"type": "string"}}, Required: []string{"path"}},
			{Name: "pwd"},
		},
	}
	openai, _ := New(Options{Name: "openai", BaseURL: "http://x"})
	body, err := openai.buildBody(req)
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	for _, want := range []string{
		`"model":"gpt-5"`, `"stream":true`, `"stream_options":{"include_usage":true}`,
		`"max_completion_tokens":4096`, `"reasoning_effort":"high"`,
		`{"type":"function","function":{"name":"read_file","description":"Read a file","parameters":{"properties":{"path":{"type":"string"}},"required":["path"],"type":"object"}}}`,
		`{"type":"function","function":{"name":"pwd","parameters":{"properties":{},"type":"object"}}}`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("openai body missing %s\n%s", want, s)
		}
	}
	if strings.Contains(s, `"max_tokens"`) {
		t.Errorf("openai must not send max_tokens: %s", s)
	}

	groq, _ := New(Options{Name: "groq", BaseURL: "http://x"})
	body, _ = groq.buildBody(req)
	s = string(body)
	if !strings.Contains(s, `"max_tokens":4096`) || strings.Contains(s, "max_completion_tokens") || strings.Contains(s, "reasoning_effort") {
		t.Errorf("groq body: %s", s)
	}

	for in, want := range map[string]string{"low": "low", "medium": "medium", "high": "high", "xhigh": "high", "max": "high", "": "", "turbo": ""} {
		if got := reasoningEffort(in); got != want {
			t.Errorf("effort %q: got %q want %q", in, got, want)
		}
	}
	if _, err := groq.buildBody(llm.Request{Model: "m", Tools: []llm.ToolSpec{{Name: " "}}}); err == nil {
		t.Error("empty tool name must be rejected")
	}
}

func TestMapFinish(t *testing.T) {
	for in, want := range map[string]llm.StopReason{
		"stop": llm.StopEndTurn, "": llm.StopEndTurn, "eos": llm.StopEndTurn,
		"tool_calls": llm.StopToolUse, "function_call": llm.StopToolUse,
		"length": llm.StopMaxTokens, "content_filter": llm.StopRefusal, "weird": llm.StopOther,
	} {
		if got := mapFinish(in); got != want {
			t.Errorf("%q: got %s want %s", in, got, want)
		}
	}
}

func TestParseErrorBody(t *testing.T) {
	cases := []struct{ body, msg, typ, code string }{
		{`{"error":{"message":"Invalid key","type":"invalid_request_error","code":"invalid_api_key"}}`, "Invalid key", "invalid_request_error", "invalid_api_key"},
		{`{"error":"model \"x\" not found"}`, `model "x" not found`, "", ""},
		{`[{"error":{"code":400,"message":"bad arg","status":"INVALID_ARGUMENT"}}]`, "bad arg", "INVALID_ARGUMENT", "400"},
		{`{"object":"error","message":"too long","type":"BadRequestError","code":400}`, "too long", "BadRequestError", "400"},
		{`{"detail":"Not authenticated"}`, "Not authenticated", "", ""},
		{`{"error":{"message":"Provider returned error","code":429,"metadata":{"raw":"slow down","provider_name":"Acme"}}}`, "Provider returned error (Acme: slow down)", "", "429"},
	}
	for _, c := range cases {
		msg, typ, code, ok := parseErrorBody([]byte(c.body))
		if !ok || msg != c.msg || typ != c.typ || code != c.code {
			t.Errorf("%s: got %q %q %q %v", c.body, msg, typ, code, ok)
		}
	}
	if _, _, _, ok := parseErrorBody([]byte("<html>bad gateway</html>")); ok {
		t.Error("html must not parse")
	}
}

func TestParseRetryAfterAndBackoff(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h := http.Header{}
	h.Set("Retry-After", "3")
	if d := parseRetryAfter(h, now); d != 3*time.Second {
		t.Fatalf("seconds: %v", d)
	}
	h.Set("Retry-After-Ms", "250")
	if d := parseRetryAfter(h, now); d != 250*time.Millisecond {
		t.Fatalf("ms: %v", d)
	}
	h = http.Header{}
	h.Set("Retry-After", now.Add(7*time.Second).Format(http.TimeFormat))
	if d := parseRetryAfter(h, now); d != 7*time.Second {
		t.Fatalf("date: %v", d)
	}
	h.Set("Retry-After", "soon")
	if d := parseRetryAfter(h, now); d != 0 {
		t.Fatalf("garbage: %v", d)
	}

	p, _ := New(Options{BaseURL: "http://x"})
	p.jitter = func() float64 { return 0 }
	for n, want := range []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second} {
		if got := p.backoff(n, 0); got != want {
			t.Errorf("backoff(%d) = %v want %v", n, got, want)
		}
	}
	if got := p.backoff(0, 2*time.Second); got != 2*time.Second {
		t.Errorf("retry-after must win: %v", got)
	}
	if got := p.backoff(0, 10*time.Minute); got != 500*time.Millisecond {
		t.Errorf("absurd retry-after must be ignored: %v", got)
	}
	p.jitter = func() float64 { return 0.999 }
	if got := p.backoff(1, 0); got < 750*time.Millisecond || got > time.Second {
		t.Errorf("jitter out of range: %v", got)
	}
}

func TestAPIErrorMessages(t *testing.T) {
	base := APIError{Provider: "groq", keyEnv: "GROQ_API_KEY", baseURL: "https://api.groq.com/openai/v1", model: "llama-x"}
	cases := map[int][]string{
		401: {"groq: authentication failed (401)", "set GROQ_API_KEY"},
		402: {"payment required"},
		403: {"permission denied"},
		404: {`check the model name "llama-x"`, "https://api.groq.com/openai/v1"},
		413: {"too large"},
		429: {"rate limited (429) after 3 attempts"},
		500: {"server error (500) after 3 attempts"},
		418: {"unexpected response (418)"},
	}
	for code, wants := range cases {
		e := base
		e.StatusCode, e.Attempts, e.Message = code, 3, "detail text"
		for _, w := range wants {
			if !strings.Contains(e.Error(), w) {
				t.Errorf("%d: %q missing %q", code, e.Error(), w)
			}
		}
	}
	e := base
	e.StatusCode, e.Code = 429, "insufficient_quota"
	if !strings.Contains(e.Error(), "quota exceeded") {
		t.Errorf("quota: %s", e.Error())
	}
	e = base
	e.StatusCode, e.Message = 400, "max_tokens is too large: 64000"
	if !strings.Contains(e.Error(), "lower max_tokens") {
		t.Errorf("400 hint: %s", e.Error())
	}
	e = base
	e.StatusCode, e.Message = 400, "This model's maximum context length is 8192 tokens"
	if !strings.Contains(e.Error(), "context window") {
		t.Errorf("400 context hint: %s", e.Error())
	}
	var target *APIError
	if !errors.As(error(&e), &target) {
		t.Fatal("APIError must be matchable")
	}
}
