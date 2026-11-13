// Package llm defines the provider-neutral conversation model used by the
// agent. Providers translate these types to and from their wire formats.
package llm

import (
	"context"
	"encoding/json"
)

// Role is the author of a message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// BlockType identifies the kind of content in a Block.
type BlockType string

const (
	BlockText       BlockType = "text"
	BlockThinking   BlockType = "thinking"
	BlockToolUse    BlockType = "tool_use"
	BlockToolResult BlockType = "tool_result"
	// BlockServerTool is a tool the provider ran on its side (for example web
	// search). It is kept for display only; replay uses Message.Native.
	BlockServerTool BlockType = "server_tool"
)

// Block is one piece of message content.
type Block struct {
	Type      BlockType       `json:"type"`
	Text      string          `json:"text,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	ToolName  string          `json:"tool_name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// Message is one conversation turn.
type Message struct {
	Role   Role    `json:"role"`
	Blocks []Block `json:"blocks"`
	// Provider and Native carry the provider's exact content for lossless
	// replay (thinking signatures, server tool results, compaction blocks).
	// A provider that does not recognise Provider falls back to Blocks.
	Provider string          `json:"provider,omitempty"`
	Native   json.RawMessage `json:"native,omitempty"`
}

// Text concatenates the text blocks of a message.
func (m Message) Text() string {
	var s string
	for _, b := range m.Blocks {
		if b.Type == BlockText {
			s += b.Text
		}
	}
	return s
}

// ToolUses returns the client-side tool calls in a message.
func (m Message) ToolUses() []Block {
	var out []Block
	for _, b := range m.Blocks {
		if b.Type == BlockToolUse {
			out = append(out, b)
		}
	}
	return out
}

// UserText builds a plain user message.
func UserText(text string) Message {
	return Message{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: text}}}
}

// ToolSpec describes a client-side tool to the model.
type ToolSpec struct {
	Name        string
	Description string
	// Properties is the JSON Schema "properties" object.
	Properties map[string]any
	Required   []string
}

// Request is one model call.
type Request struct {
	Model     string
	System    string
	Messages  []Message
	Tools     []ToolSpec
	MaxTokens int64
	// Effort is one of low, medium, high, xhigh, max. Empty uses the model default.
	Effort string
	// ShowThinking asks for summarized reasoning instead of omitted.
	ShowThinking bool
}

// StopReason is a normalized reason the model stopped.
type StopReason string

const (
	StopEndTurn   StopReason = "end_turn"
	StopToolUse   StopReason = "tool_use"
	StopMaxTokens StopReason = "max_tokens"
	StopRefusal   StopReason = "refusal"
	StopPauseTurn StopReason = "pause_turn"
	StopOther     StopReason = "other"
)

// Usage counts tokens for one call.
type Usage struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
}

// Add accumulates another usage record.
func (u *Usage) Add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CacheReadTokens += o.CacheReadTokens
	u.CacheWriteTokens += o.CacheWriteTokens
}

// ContextTokens is the size of the prompt the model last saw.
func (u Usage) ContextTokens() int64 {
	return u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

// Response is the completed result of a model call.
type Response struct {
	Message    Message
	StopReason StopReason
	// StopDetail explains a refusal when the provider gives one.
	StopDetail string
	Usage      Usage
	Model      string
}

// EventType identifies a streaming event.
type EventType int

const (
	EventTextDelta EventType = iota
	EventThinkingDelta
	EventToolUseStart
	EventToolInputDelta
	EventServerToolStart
	EventBlockStop
)

// Event is an incremental update while a response streams.
type Event struct {
	Type     EventType
	Text     string
	ToolName string
	ToolID   string
	// Bytes is the size of a tool-input fragment.
	Bytes int
}

// Provider is a model backend.
type Provider interface {
	Name() string
	Stream(ctx context.Context, req Request, onEvent func(Event)) (*Response, error)
}
