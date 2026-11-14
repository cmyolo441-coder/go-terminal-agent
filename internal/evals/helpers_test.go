package evals

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

// scriptProvider answers each request with whatever reply returns,
// streaming the events first. It counts calls.
type scriptProvider struct {
	name  string
	reply func(req llm.Request) ([]llm.Event, *llm.Response, error)
	calls atomic.Int64
}

func (p *scriptProvider) Name() string { return p.name }

func (p *scriptProvider) Stream(ctx context.Context, req llm.Request, onEvent func(llm.Event)) (*llm.Response, error) {
	p.calls.Add(1)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	evs, resp, err := p.reply(req)
	for _, ev := range evs {
		onEvent(ev)
	}
	return resp, err
}

// textReply streams text in chunks and finishes the turn.
func textReply(chunks ...string) ([]llm.Event, *llm.Response) {
	var evs []llm.Event
	for _, c := range chunks {
		evs = append(evs, llm.Event{Type: llm.EventTextDelta, Text: c})
	}
	evs = append(evs, llm.Event{Type: llm.EventBlockStop})
	return evs, &llm.Response{
		Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: strings.Join(chunks, "")}}},
		StopReason: llm.StopEndTurn,
		Usage:      llm.Usage{InputTokens: 100, OutputTokens: 20},
		Model:      "claude-sonnet-5",
	}
}

// toolReply asks for one tool call, streaming its input in two fragments.
func toolReply(id, name string, input any) ([]llm.Event, *llm.Response) {
	raw, _ := json.Marshal(input)
	half := len(raw) / 2
	evs := []llm.Event{
		{Type: llm.EventToolUseStart, ToolName: name, ToolID: id},
		{Type: llm.EventToolInputDelta, Bytes: half},
		{Type: llm.EventToolInputDelta, Bytes: len(raw) - half},
		{Type: llm.EventBlockStop},
	}
	return evs, &llm.Response{
		Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Type: llm.BlockText, Text: "Working on it."},
			{Type: llm.BlockToolUse, ToolUseID: id, ToolName: name, Input: raw},
		}},
		StopReason: llm.StopToolUse,
		Usage:      llm.Usage{InputTokens: 120, OutputTokens: 30},
		Model:      "claude-sonnet-5",
	}
}

// codingModel is a deterministic fake model: on the first turn it asks to
// write the file named in the prompt ("write <path>: <content>"), then it
// answers with a summary. Random tool ids mimic a real provider.
func codingModel() *scriptProvider {
	var seq atomic.Int64
	p := &scriptProvider{name: "fake"}
	p.reply = func(req llm.Request) ([]llm.Event, *llm.Response, error) {
		last := req.Messages[len(req.Messages)-1]
		for _, b := range last.Blocks {
			if b.Type == llm.BlockToolResult {
				evs, resp := textReply("Done. ", "Wrote the file.")
				return evs, resp, nil
			}
		}
		prompt := last.Text()
		var path, content string
		if rest, ok := strings.CutPrefix(prompt, "write "); ok {
			path, content, _ = strings.Cut(rest, ": ")
		}
		if path == "" {
			evs, resp := textReply(`{"answer": 42, "ok": true}`)
			return evs, resp, nil
		}
		id := fmt.Sprintf("toolu_%d_%d", seq.Add(1), len(prompt))
		evs, resp := toolReply(id, "write_file", map[string]any{"path": path, "content": content})
		return evs, resp, nil
	}
	return p
}

// fakeAgent is a minimal agent loop: it runs write_file tool calls inside
// the workspace and stops when the model ends its turn.
func fakeAgent(ctx context.Context, workspace, prompt string, p llm.Provider) (Transcript, error) {
	tr := Transcript{}
	msgs := []llm.Message{llm.UserText(prompt)}
	for turn := 0; turn < 10; turn++ {
		resp, err := p.Stream(ctx, llm.Request{
			Model:     "claude-sonnet-5",
			System:    "You are working in " + workspace,
			Messages:  msgs,
			Tools:     []llm.ToolSpec{{Name: "write_file", Properties: map[string]any{"path": map[string]any{"type": "string"}}}},
			MaxTokens: 1024,
		}, func(llm.Event) {})
		if err != nil {
			tr.Messages = msgs
			return tr, err
		}
		msgs = append(msgs, resp.Message)
		uses := resp.Message.ToolUses()
		if len(uses) == 0 {
			break
		}
		var results []llm.Block
		for _, u := range uses {
			var in struct{ Path, Content string }
			_ = json.Unmarshal(u.Input, &in)
			out, isErr := "wrote "+in.Path, false
			full, err := resolveInside(workspace, in.Path)
			if err == nil {
				err = os.MkdirAll(filepath.Dir(full), 0o755)
			}
			if err == nil {
				err = os.WriteFile(full, []byte(in.Content), 0o644)
			}
			if err != nil {
				out, isErr = err.Error(), true
			}
			results = append(results, llm.Block{Type: llm.BlockToolResult, ToolUseID: u.ToolUseID, Text: out, IsError: isErr})
		}
		msgs = append(msgs, llm.Message{Role: llm.RoleUser, Blocks: results})
	}
	tr.Messages = msgs
	return tr, nil
}

// eventLog collects streamed events safely.
type eventLog struct {
	mu  sync.Mutex
	evs []llm.Event
}

func (l *eventLog) add(ev llm.Event) {
	l.mu.Lock()
	l.evs = append(l.evs, ev)
	l.mu.Unlock()
}

func (l *eventLog) all() []llm.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]llm.Event(nil), l.evs...)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func ptr[T any](v T) *T { return &v }
