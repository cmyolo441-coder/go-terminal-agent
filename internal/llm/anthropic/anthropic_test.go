package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go/v2"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

const respJSON = `{
  "id": "msg_1", "type": "message", "role": "assistant", "model": "claude-opus-5-5",
  "stop_reason": "tool_use",
  "usage": {"input_tokens": 12, "output_tokens": 34, "cache_read_input_tokens": 56, "cache_creation_input_tokens": 7},
  "content": [
    {"type": "thinking", "thinking": "Let me look.", "signature": "sig-abc"},
    {"type": "text", "text": "Checking the file."},
    {"type": "server_tool_use", "id": "srvtoolu_1", "name": "web_search", "input": {"query": "go 1.25"}},
    {"type": "web_search_tool_result", "tool_use_id": "srvtoolu_1", "content": [
      {"type": "web_search_result", "url": "https://go.dev", "title": "Go", "encrypted_content": "enc", "page_age": null}
    ]},
    {"type": "tool_use", "id": "toolu_1", "name": "read_file", "input": {"path": "main.go"}}
  ]
}`

func decodeMessage(t *testing.T) sdk.BetaMessage {
	t.Helper()
	var m sdk.BetaMessage
	if err := json.Unmarshal([]byte(respJSON), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestFromMessageNormalizes(t *testing.T) {
	resp, err := FromMessage(decodeMessage(t))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != llm.StopToolUse || resp.Usage.CacheReadTokens != 56 || resp.Usage.OutputTokens != 34 {
		t.Fatalf("bad response: %+v", resp)
	}
	uses := resp.Message.ToolUses()
	if len(uses) != 1 || uses[0].ToolName != "read_file" || string(uses[0].Input) != `{"path": "main.go"}` && string(uses[0].Input) != `{"path":"main.go"}` {
		t.Fatalf("bad tool use: %+v", uses)
	}
	if resp.Message.Text() != "Checking the file." {
		t.Fatalf("text: %q", resp.Message.Text())
	}
}

// Replay must send thinking signatures and server tool results back unchanged.
func TestNativeReplayIsLossless(t *testing.T) {
	resp, err := FromMessage(decodeMessage(t))
	if err != nil {
		t.Fatal(err)
	}
	// Round-trip through session JSON, as a resumed session would.
	raw, _ := json.Marshal(resp.Message)
	var restored llm.Message
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	params, err := ToParams([]llm.Message{llm.UserText("hi"), restored})
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := json.Marshal(params[1])
	s := string(wire)
	for _, want := range []string{`"signature":"sig-abc"`, `"thinking":"Let me look."`, `"type":"web_search_tool_result"`, `"encrypted_content":"enc"`, `"type":"server_tool_use"`, `"id":"toolu_1"`, `"role":"assistant"`} {
		if !strings.Contains(s, want) {
			t.Errorf("replayed message missing %s:\n%s", want, s)
		}
	}
}

func TestToParamsFallbackAndToolResults(t *testing.T) {
	hist := []llm.Message{
		llm.UserText("go"),
		{Role: llm.RoleAssistant, Provider: "other", Blocks: []llm.Block{
			{Type: llm.BlockThinking, Text: "hidden"},
			{Type: llm.BlockText, Text: "ok"},
			{Type: llm.BlockToolUse, ToolUseID: "t1", ToolName: "bash", Input: json.RawMessage(`{"command":"ls"}`)},
		}},
		{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockToolResult, ToolUseID: "t1", Text: "a\nb", IsError: true}}},
	}
	params, err := ToParams(hist)
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := json.Marshal(params)
	s := string(wire)
	if strings.Contains(s, "hidden") {
		t.Fatal("foreign thinking must not be replayed")
	}
	for _, want := range []string{`"type":"tool_use"`, `"command":"ls"`, `"type":"tool_result"`, `"tool_use_id":"t1"`, `"is_error":true`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in %s", want, s)
		}
	}
}

func TestBuildParams(t *testing.T) {
	p := New(Options{Fallbacks: true, WebSearch: true})
	params, err := p.BuildParams(llm.Request{
		Model: "claude-opus-5-5", System: "sys", MaxTokens: 64000, Effort: "high", ShowThinking: true,
		Messages: []llm.Message{llm.UserText("hi")},
		Tools:    []llm.ToolSpec{{Name: "read_file", Description: "d", Properties: map[string]any{"path": map[string]any{"type": "string"}}, Required: []string{"path"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := json.Marshal(params)
	s := string(wire)
	for _, want := range []string{
		`"model":"claude-opus-5-5"`, `"eager_input_streaming":true`, `"type":"adaptive"`, `"display":"summarized"`,
		`"effort":"high"`, `"fallbacks":"default"`, `"cache_control":{"type":"ephemeral"}`, `"type":"web_search_20260209"`, `"required":["path"]`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("params missing %s:\n%s", want, s)
		}
	}
	if len(params.Betas) != 1 || params.Betas[0] != sdk.AnthropicBetaServerSideFallback2026_07_01 {
		t.Fatalf("betas: %v", params.Betas)
	}
}
