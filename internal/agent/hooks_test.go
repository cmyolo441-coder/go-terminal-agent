//go:build !windows

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/hooks"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permission"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

func withHooks(t *testing.T, a *Agent, root string, cfg hooks.Config) {
	t.Helper()
	r, err := hooks.NewRunner(cfg, hooks.Options{Root: root, SessionID: "test"})
	if err != nil {
		t.Fatal(err)
	}
	a.Hooks = r
}

func TestPreToolUseHookBlocksCall(t *testing.T) {
	p := &scripted{responses: []*llm.Response{
		reply(llm.StopToolUse, toolUse("t1", "write_file", map[string]any{"path": "x.txt", "content": "x"})),
		reply(llm.StopEndTurn, llm.Block{Type: llm.BlockText, Text: "ok"}),
	}}
	a, root := newAgent(t, p, permission.ModeYolo)
	withHooks(t, a, root, hooks.Config{hooks.PreToolUse: {{Matcher: "write_file", Command: "echo no writes here >&2; exit 2"}}})
	s := &sink{decision: permission.DecisionAllowOnce}
	if err := a.Run(context.Background(), llm.UserText("go"), s); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "x.txt")); err == nil {
		t.Fatal("blocked write still ran")
	}
	res := a.History()[2].Blocks[0]
	if !res.IsError || !strings.Contains(res.Text, "no writes here") {
		t.Fatalf("model did not get the hook reason: %+v", res)
	}
}

func TestPreToolUseHookApprovalSkipsPrompt(t *testing.T) {
	p := &scripted{responses: []*llm.Response{
		reply(llm.StopToolUse, toolUse("t1", "write_file", map[string]any{"path": "y.txt", "content": "y"})),
		reply(llm.StopEndTurn),
	}}
	a, root := newAgent(t, p, permission.ModeAsk)
	withHooks(t, a, root, hooks.Config{hooks.PreToolUse: {{Command: `echo '{"decision":"allow"}'`}}})
	s := &sink{decision: permission.DecisionDeny}
	if err := a.Run(context.Background(), llm.UserText("go"), s); err != nil {
		t.Fatal(err)
	}
	if len(s.asked) != 0 {
		t.Fatalf("hook approval should skip the prompt, asked %v", s.asked)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "y.txt")); string(b) != "y" {
		t.Fatal("approved write did not run")
	}
}

func TestPromptHookAddsContextAndStopHookContinues(t *testing.T) {
	p := &scripted{responses: []*llm.Response{
		reply(llm.StopEndTurn, llm.Block{Type: llm.BlockText, Text: "first"}),
		reply(llm.StopEndTurn, llm.Block{Type: llm.BlockText, Text: "second"}),
	}}
	a, root := newAgent(t, p, permission.ModeAsk)
	marker := filepath.Join(root, "stopped-once")
	withHooks(t, a, root, hooks.Config{
		hooks.UserPromptSubmit: {{Command: "echo branch is main"}},
		hooks.Stop:             {{Command: "if [ -e " + marker + " ]; then exit 0; fi; touch " + marker + "; echo run the tests >&2; exit 2"}},
	})
	if err := a.Run(context.Background(), llm.UserText("hi"), &sink{}); err != nil {
		t.Fatal(err)
	}
	first := p.requests[0].Messages[0]
	if !strings.Contains(first.Text(), "branch is main") {
		t.Fatalf("prompt hook context missing: %q", first.Text())
	}
	if len(p.requests) != 2 || !strings.Contains(p.requests[1].Messages[2].Text(), "run the tests") {
		t.Fatalf("stop hook did not send the model back to work: %d requests", len(p.requests))
	}
}

func TestPromptHookBlocks(t *testing.T) {
	p := &scripted{}
	a, root := newAgent(t, p, permission.ModeAsk)
	withHooks(t, a, root, hooks.Config{hooks.UserPromptSubmit: {{Command: "echo secrets detected >&2; exit 2"}}})
	err := a.Run(context.Background(), llm.UserText("my key is sk-123"), &sink{})
	if err == nil || !strings.Contains(err.Error(), "secrets detected") || len(p.requests) != 0 {
		t.Fatalf("prompt not blocked: %v", err)
	}
}

func TestBeforeWriteCalledForWritesOnly(t *testing.T) {
	p := &scripted{responses: []*llm.Response{
		reply(llm.StopToolUse,
			toolUse("t1", "list_dir", map[string]any{}),
			toolUse("t2", "write_file", map[string]any{"path": "z.txt", "content": "z"})),
		reply(llm.StopEndTurn),
	}}
	a, _ := newAgent(t, p, permission.ModeYolo)
	var seen []string
	a.BeforeWrite = func(c *tools.Call) { seen = append(seen, c.Tool+":"+filepath.Base(c.Path)) }
	if err := a.Run(context.Background(), llm.UserText("go"), &sink{}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != "write_file:z.txt" {
		t.Fatalf("BeforeWrite calls = %v", seen)
	}
}

func TestUsageByModelSplitsSubagentUsage(t *testing.T) {
	p := &scripted{responses: []*llm.Response{reply(llm.StopEndTurn, llm.Block{Type: llm.BlockText, Text: "hi"})}}
	a, _ := newAgent(t, p, permission.ModeAsk)
	if err := a.Run(context.Background(), llm.UserText("hi"), &sink{}); err != nil {
		t.Fatal(err)
	}
	a.AddUsage("claude-haiku-5-5", llm.Usage{InputTokens: 50, OutputTokens: 5})
	by := a.UsageByModel()
	if len(by) != 2 || a.Calls() != 1 {
		t.Fatalf("by model = %+v, calls %d", by, a.Calls())
	}
	total, _, _ := a.Stats()
	if total.InputTokens != 150 {
		t.Fatalf("total input %d", total.InputTokens)
	}
	a.Reset()
	if len(a.UsageByModel()) != 0 {
		t.Fatal("reset kept per-model usage")
	}
}
