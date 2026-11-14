package evals

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

// AgentFunc runs an agent once: it must work inside workspace, answer
// prompt, and use only provider for model calls. It returns what happened.
// The harness enforces limits through ctx and the provider it passes in.
type AgentFunc func(ctx context.Context, workspace, prompt string, provider llm.Provider) (Transcript, error)

// ToolCall is one tool invocation observed in a run.
type ToolCall struct {
	ID      string          `json:"id,omitempty"`
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input,omitempty"`
	Output  string          `json:"output,omitempty"`
	IsError bool            `json:"is_error,omitempty"`
}

// Transcript is the record of one agent run. An agent may fill only
// Messages; Normalize derives the rest.
type Transcript struct {
	Messages  []llm.Message `json:"messages,omitempty"`
	Output    string        `json:"output,omitempty"`
	ToolCalls []ToolCall    `json:"tool_calls,omitempty"`
	Turns     int           `json:"turns,omitempty"`
	Usage     llm.Usage     `json:"usage"`
	Model     string        `json:"model,omitempty"`
	CostUSD   float64       `json:"cost_usd,omitempty"`
}

// Normalize derives ToolCalls, Output and Turns from Messages when the
// agent did not set them.
func (t *Transcript) Normalize() {
	if len(t.ToolCalls) == 0 {
		t.ToolCalls = toolCallsFrom(t.Messages)
	}
	if t.Output == "" {
		for i := len(t.Messages) - 1; i >= 0; i-- {
			if t.Messages[i].Role == llm.RoleAssistant {
				if txt := strings.TrimSpace(t.Messages[i].Text()); txt != "" {
					t.Output = txt
					break
				}
			}
		}
	}
	if t.Turns == 0 {
		for _, m := range t.Messages {
			if m.Role == llm.RoleAssistant {
				t.Turns++
			}
		}
	}
}

// toolCallsFrom pairs tool_use blocks with their tool_result blocks.
func toolCallsFrom(msgs []llm.Message) []ToolCall {
	var calls []ToolCall
	byID := map[string]int{}
	for _, m := range msgs {
		for _, b := range m.Blocks {
			switch b.Type {
			case llm.BlockToolUse:
				if b.ToolUseID != "" {
					byID[b.ToolUseID] = len(calls)
				}
				calls = append(calls, ToolCall{ID: b.ToolUseID, Name: b.ToolName, Input: b.Input})
			case llm.BlockToolResult:
				if i, ok := byID[b.ToolUseID]; ok {
					calls[i].Output = b.Text
					calls[i].IsError = b.IsError
				}
			}
		}
	}
	return calls
}
