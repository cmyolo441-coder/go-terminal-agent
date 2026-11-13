package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permission"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

func TestAfterWriteAddsDiagnosticsToResult(t *testing.T) {
	p := &scripted{responses: []*llm.Response{
		reply(llm.StopToolUse, toolUse("t1", "write_file", map[string]any{"path": "a.go", "content": "package a\n"})),
		reply(llm.StopToolUse, toolUse("t2", "read_file", map[string]any{"path": "a.go"})),
		reply(llm.StopEndTurn, llm.Block{Type: llm.BlockText, Text: "done"}),
	}}
	a, root := newAgent(t, p, permission.ModeYolo)
	var paths []string
	a.AfterWrite = func(ctx context.Context, c *tools.Call) string {
		paths = append(paths, c.Path)
		return "<diagnostics file=\"a.go\">a.go:1:1 error: boom</diagnostics>"
	}
	if err := a.Run(context.Background(), llm.UserText("go"), &sink{}); err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(filepath.Join(root, "a.go"))
	if len(paths) != 1 {
		t.Fatalf("AfterWrite ran %d times, want once (writes only): %v", len(paths), paths)
	}
	if got, _ := filepath.EvalSymlinks(paths[0]); got != want {
		t.Fatalf("AfterWrite path %q, want %q", paths[0], want)
	}
	res := a.History()[2].Blocks[0]
	if res.IsError || !strings.Contains(res.Text, "error: boom") || !strings.Contains(res.Text, "a.go") {
		t.Fatalf("diagnostics not in the write result: %+v", res)
	}
	if strings.Contains(a.History()[4].Blocks[0].Text, "boom") {
		t.Fatal("diagnostics leaked into a read result")
	}
}
