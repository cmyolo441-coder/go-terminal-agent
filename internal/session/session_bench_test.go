package session

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

// Benchmarks for saving. They only run with -bench:
//
//	go test ./internal/session -run '^$' -bench . -benchmem

// longConversation is n turns of a coding session: a user prompt, an
// assistant reply with a tool call, and a tool result of a few KB.
func longConversation(n int) []llm.Message {
	var msgs []llm.Message
	out := strings.Repeat("func handler(w http.ResponseWriter, r *http.Request) { // <b>&amp;</b>\n", 40)
	for i := 0; i < n; i++ {
		input, _ := json.Marshal(map[string]any{"path": fmt.Sprintf("internal/pkg%d/file.go", i), "offset": i})
		msgs = append(msgs,
			llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: fmt.Sprintf("step %d: read the file and fix the bug", i)}}},
			llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
				{Type: llm.BlockText, Text: "Let me look at the file first."},
				{Type: llm.BlockToolUse, ToolUseID: fmt.Sprintf("call_%d", i), ToolName: "read_file", Input: input},
			}},
			llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockToolResult, ToolUseID: fmt.Sprintf("call_%d", i), Text: out}}},
		)
	}
	return msgs
}

// BenchmarkSaveLong saves a 603-message (about 0.85 MB) conversation after
// a new turn (three new messages), as the UI does after every turn.
func BenchmarkSaveLong(b *testing.B) {
	st := &Store{Dir: b.TempDir()}
	msgs := longConversation(201)
	turns := [2][]llm.Message{longConversation(202)[603:], longConversation(203)[606:]}
	sess := New("/w", "m")
	sess.Messages = msgs
	if err := st.Save(sess); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		copy(msgs[600:], turns[i%2])
		if err := st.Save(sess); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSaveLongUnchanged saves the same conversation again.
func BenchmarkSaveLongUnchanged(b *testing.B) {
	st := &Store{Dir: b.TempDir()}
	sess := New("/w", "m")
	sess.Messages = longConversation(200)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := st.Save(sess); err != nil {
			b.Fatal(err)
		}
	}
}
