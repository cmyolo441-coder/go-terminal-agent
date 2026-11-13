package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

// Wire types for the Chat Completions request body.

type chatRequest struct {
	Model               string         `json:"model"`
	Messages            []chatMessage  `json:"messages"`
	Tools               []chatTool     `json:"tools,omitempty"`
	MaxTokens           int64          `json:"max_tokens,omitempty"`
	MaxCompletionTokens int64          `json:"max_completion_tokens,omitempty"`
	ReasoningEffort     string         `json:"reasoning_effort,omitempty"`
	Stream              bool           `json:"stream"`
	StreamOptions       *streamOptions `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// chatMessage is one message on the wire. Content is a pointer so that an
// assistant message carrying only tool calls sends "content": null, which is
// what every compatible server accepts.
type chatMessage struct {
	Role       string         `json:"role"`
	Content    *string        `json:"content"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type chatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function chatFunctionCall `json:"function"`
}

type chatFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatTool struct {
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

// Text sent in place of content the wire format cannot leave empty.
const (
	emptyAssistant   = "(no content)"
	emptyToolResult  = "(no output)"
	toolErrorPrefix  = "Error: "
	interruptedTool  = "(no result: the tool call was interrupted before it completed)"
	orphanResultLead = "Result of an earlier tool call"
)

// buildRequest converts a provider-neutral request to the wire request.
func (p *Provider) buildRequest(req llm.Request) (*chatRequest, error) {
	msgs, err := toMessages(req.System, req.Messages)
	if err != nil {
		return nil, err
	}
	cr := &chatRequest{
		Model:         req.Model,
		Messages:      msgs,
		Stream:        true,
		StreamOptions: &streamOptions{IncludeUsage: true},
	}
	isOpenAI := p.opts.Name == "openai"
	if req.MaxTokens > 0 {
		if isOpenAI {
			// OpenAI deprecated max_tokens and rejects it for reasoning models.
			cr.MaxCompletionTokens = req.MaxTokens
		} else {
			cr.MaxTokens = req.MaxTokens
		}
	}
	if isOpenAI {
		cr.ReasoningEffort = reasoningEffort(req.Effort)
	}
	for _, t := range req.Tools {
		if strings.TrimSpace(t.Name) == "" {
			return nil, fmt.Errorf("tool with empty name")
		}
		props := t.Properties
		if props == nil {
			props = map[string]any{}
		}
		params := map[string]any{"type": "object", "properties": props}
		if len(t.Required) > 0 {
			params["required"] = t.Required
		}
		cr.Tools = append(cr.Tools, chatTool{
			Type:     "function",
			Function: chatFunction{Name: t.Name, Description: t.Description, Parameters: params},
		})
	}
	return cr, nil
}

// buildBody encodes the wire request.
func (p *Provider) buildBody(req llm.Request) ([]byte, error) {
	cr, err := p.buildRequest(req)
	if err != nil {
		return nil, fmt.Errorf("%s: build request: %w", p.opts.Name, err)
	}
	body, err := json.Marshal(cr)
	if err != nil {
		return nil, fmt.Errorf("%s: encode request: %w", p.opts.Name, err)
	}
	return body, nil
}

// reasoningEffort maps the agent's effort scale onto OpenAI's. Levels above
// high saturate at high; unknown values send nothing so the model default
// applies.
func reasoningEffort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "low":
		return "low"
	case "medium":
		return "medium"
	case "high", "xhigh", "max":
		return "high"
	}
	return ""
}

// toMessages converts conversation history to Chat Completions messages.
//
// Thinking blocks, server-tool blocks and Native payloads are dropped: they
// belong to the provider that produced them and cannot be replayed here.
// Tool results become one "tool" message each, in order, placed directly
// after the assistant message that called them as the API requires. Any
// call left without a result (an interrupted turn) gets a placeholder
// result, and a result whose call is not pending is sent as user text, so a
// resumed or cross-provider history never trips the server's pairing check.
func toMessages(system string, history []llm.Message) ([]chatMessage, error) {
	out := make([]chatMessage, 0, len(history)+1)
	if strings.TrimSpace(system) != "" {
		out = append(out, chatMessage{Role: "system", Content: str(system)})
	}
	var pending []string // tool call ids awaiting a result, in call order
	answered := map[string]bool{}

	flushPending := func() {
		for _, id := range pending {
			if !answered[id] {
				out = append(out, chatMessage{Role: "tool", ToolCallID: id, Content: str(interruptedTool)})
			}
		}
		pending, answered = nil, map[string]bool{}
	}

	for i, m := range history {
		switch m.Role {
		case llm.RoleAssistant:
			flushPending()
			msg := chatMessage{Role: "assistant"}
			var texts []string
			for _, b := range m.Blocks {
				switch b.Type {
				case llm.BlockText:
					if b.Text != "" {
						texts = append(texts, b.Text)
					}
				case llm.BlockToolUse:
					if b.ToolUseID == "" || b.ToolName == "" {
						return nil, fmt.Errorf("message %d: tool call without id or name", i)
					}
					args, err := compactArgs(b.Input)
					if err != nil {
						return nil, fmt.Errorf("message %d: tool call %s: %w", i, b.ToolUseID, err)
					}
					msg.ToolCalls = append(msg.ToolCalls, chatToolCall{
						ID: b.ToolUseID, Type: "function",
						Function: chatFunctionCall{Name: b.ToolName, Arguments: args},
					})
					pending = append(pending, b.ToolUseID)
				}
			}
			switch {
			case len(texts) > 0:
				msg.Content = str(strings.Join(texts, "\n\n"))
			case len(msg.ToolCalls) == 0:
				msg.Content = str(emptyAssistant)
			}
			out = append(out, msg)
		default:
			var texts []string
			for _, b := range m.Blocks {
				switch b.Type {
				case llm.BlockText:
					if b.Text != "" {
						texts = append(texts, b.Text)
					}
				case llm.BlockToolResult:
					content := toolResultText(b)
					if contains(pending, b.ToolUseID) && !answered[b.ToolUseID] {
						answered[b.ToolUseID] = true
						out = append(out, chatMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: str(content)})
						continue
					}
					label := orphanResultLead
					if b.ToolUseID != "" {
						label += " (" + b.ToolUseID + ")"
					}
					texts = append(texts, label+":\n"+content)
				}
			}
			if len(texts) == 0 {
				continue
			}
			// Text after tool results starts a new user turn, so every
			// outstanding call must be answered first.
			flushPending()
			out = append(out, chatMessage{Role: "user", Content: str(strings.Join(texts, "\n\n"))})
		}
	}
	flushPending()
	return out, nil
}

// toolResultText renders a tool result, marking failures so the model can
// tell them apart from output (the wire format has no is_error flag).
func toolResultText(b llm.Block) string {
	text := b.Text
	if text == "" {
		text = emptyToolResult
	}
	if b.IsError {
		text = toolErrorPrefix + text
	}
	return text
}

// compactArgs renders tool input as the JSON string the wire format wants.
func compactArgs(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "{}", nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, trimmed); err != nil {
		return "", fmt.Errorf("invalid input JSON: %w", err)
	}
	return buf.String(), nil
}

func str(s string) *string { return &s }

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
