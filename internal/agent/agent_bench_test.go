package agent

import (
	"context"
	"fmt"
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permission"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

// Benchmarks for the agent loop. They only run with -bench:
//
//	go test ./internal/agent -run '^$' -bench . -benchmem

// deltaProvider streams a fixed reply as many small deltas.
type deltaProvider struct {
	deltas []llm.Event
	resp   *llm.Response
}

func (p *deltaProvider) Name() string { return "bench" }

func (p *deltaProvider) Stream(_ context.Context, _ llm.Request, on func(llm.Event)) (*llm.Response, error) {
	for _, ev := range p.deltas {
		on(ev)
	}
	return p.resp, nil
}

type countSink struct {
	sink
	events, bytes int
}

func (s *countSink) Stream(ev llm.Event) { s.events++; s.bytes += len(ev.Text) }

// BenchmarkRunDeltas is one model turn of 2000 text deltas through the
// agent to its sink, on top of a 400-message conversation. The provider
// and sink do no work of their own, so this is the agent's per-turn and
// per-delta overhead.
func BenchmarkRunDeltas(b *testing.B) {
	const n = 2000
	deltas := make([]llm.Event, n)
	for i := range deltas {
		deltas[i] = llm.Event{Type: llm.EventTextDelta, Text: fmt.Sprintf("w%d ", i%17)}
	}
	p := &deltaProvider{deltas: deltas, resp: reply(llm.StopEndTurn, llm.Block{Type: llm.BlockText, Text: "done"})}
	env, err := tools.NewEnv(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	a := New(p, tools.Default(), env, permission.NewGate(permission.ModeAsk, nil), "sys", Settings{Model: "claude-opus-5-5", MaxTokens: 4096})
	var hist []llm.Message
	for i := 0; i < 200; i++ {
		hist = append(hist,
			llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: fmt.Sprintf("question %d", i)}}},
			llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: fmt.Sprintf("answer %d", i)}}})
	}
	s := &countSink{}
	user := llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "go"}}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		a.SetHistory(hist, llm.Usage{}, 0)
		b.StartTimer()
		if err := a.Run(context.Background(), user, s); err != nil {
			b.Fatal(err)
		}
	}
	if s.events != n*b.N {
		b.Fatalf("events %d, want %d", s.events, n*b.N)
	}
}
