package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/agent"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permissions"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

// PrintUI is the non-interactive UI used by -p. It never prompts.
type PrintUI struct {
	mu      sync.Mutex
	format  string // text | json | stream-json
	verbose bool
	out     io.Writer
	text    strings.Builder
}

func (p *PrintUI) emit(v map[string]any) {
	if p.format != "stream-json" {
		return
	}
	b, _ := json.Marshal(v)
	p.mu.Lock()
	fmt.Fprintln(p.out, string(b))
	p.mu.Unlock()
}

func (p *PrintUI) log(msg string) {
	if p.verbose && p.format == "text" {
		fmt.Fprintln(os.Stderr, msg)
	}
}

func (p *PrintUI) Status(string)   {}
func (p *PrintUI) Thinking(string) {}
func (p *PrintUI) Text(delta string) {
	p.mu.Lock()
	p.text.WriteString(delta)
	p.mu.Unlock()
}
func (p *PrintUI) BlockEnd() {
	p.mu.Lock()
	t := p.text.String()
	p.text.Reset()
	p.mu.Unlock()
	if strings.TrimSpace(t) != "" {
		p.emit(map[string]any{"type": "assistant", "text": t})
	}
}
func (p *PrintUI) ToolStart(id, name, summary string) {
	p.log(fmt.Sprintf("● %s(%s)", name, summary))
	p.emit(map[string]any{"type": "tool_use", "id": id, "name": name, "summary": summary})
}
func (p *PrintUI) ToolEnd(id, name, _ string, res tools.Result, dur time.Duration) {
	p.emit(map[string]any{"type": "tool_result", "id": id, "name": name, "is_error": res.IsError,
		"content": res.Text, "duration_ms": dur.Milliseconds()})
}
func (p *PrintUI) ServerTool(name, summary string) {
	p.log(fmt.Sprintf("● %s(%s)", name, summary))
	p.emit(map[string]any{"type": "server_tool_use", "name": name, "summary": summary})
}
func (p *PrintUI) Notice(msg string) {
	p.log(msg)
	p.emit(map[string]any{"type": "system", "message": msg})
}
func (p *PrintUI) AskPermission(q agent.PermissionPrompt) agent.PermissionAnswer {
	p.emit(map[string]any{"type": "permission_denied", "tool": q.Tool, "summary": q.Summary})
	return agent.PermissionAnswer{Feedback: "This session is non-interactive, so nobody can approve the call. " +
		"It needs an allow rule (--allowedTools), --permission-mode acceptEdits, or --dangerously-skip-permissions. Continue without it if you can."}
}
func (p *PrintUI) ApprovePlan(string) (bool, permissions.Mode, string) {
	return false, permissions.ModePlan, "This session is non-interactive. Give the plan as your final answer and stop."
}

// RunPrint executes a single prompt and writes the result in the chosen format.
// It returns the process exit code.
func RunPrint(opts Options) int {
	if opts.OutputFormat == "" {
		opts.OutputFormat = "text"
	}
	switch opts.OutputFormat {
	case "text", "json", "stream-json":
	default:
		fmt.Fprintf(os.Stderr, "invalid --output-format %q (use text, json or stream-json)\n", opts.OutputFormat)
		return 2
	}
	pui := &PrintUI{format: opts.OutputFormat, verbose: opts.Verbose, out: os.Stdout}
	app, err := NewApp(opts, pui)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer app.Close()
	for _, w := range app.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	prompt := opts.Prompt
	if strings.HasPrefix(prompt, "/") {
		if expanded, ok := app.expandCustom(prompt); ok {
			prompt = expanded
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	pui.emit(map[string]any{"type": "system", "subtype": "init", "session_id": app.Agent.Session.ID,
		"model": app.Agent.Model, "cwd": app.Cwd, "permission_mode": string(app.Agent.Perm.Mode())})

	start := time.Now()
	blocks, _ := expandMentions(app.Cwd, prompt)
	result, err := app.Agent.Run(ctx, prompt, blocks...)
	in, out, cr, cc, reqs, cost := app.Agent.Usage.Snapshot()
	final := map[string]any{
		"type": "result", "is_error": err != nil, "result": result, "session_id": app.Agent.Session.ID,
		"duration_ms": time.Since(start).Milliseconds(), "num_requests": reqs, "total_cost_usd": cost,
		"usage": map[string]any{"input_tokens": in, "output_tokens": out, "cache_read_input_tokens": cr, "cache_creation_input_tokens": cc},
	}
	if err != nil {
		final["error"] = err.Error()
	}
	switch opts.OutputFormat {
	case "text":
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
		} else {
			fmt.Println(result)
		}
	default:
		b, _ := json.Marshal(final)
		fmt.Println(string(b))
	}
	switch {
	case errors.Is(err, agent.ErrInterrupted):
		return 130
	case err != nil:
		return 1
	}
	return 0
}
