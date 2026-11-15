package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/chzyer/readline"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/agent"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permissions"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/ui"
)

// Term is the interactive terminal UI.
type Term struct {
	mu       sync.Mutex // guards output state
	askMu    sync.Mutex // one question at a time
	out      io.Writer
	spin     ui.Spinner
	md       ui.Markdown
	think    strings.Builder
	thinking bool
	lastTool string
	rl       *readline.Instance
	verbose  bool
}

// NewTerm creates the terminal UI writing to stdout.
func NewTerm(verbose bool) *Term {
	t := &Term{out: os.Stdout, verbose: verbose}
	t.md = ui.Markdown{W: t.out, Prefix: ui.Bold("● "), Indent: "  "}
	return t
}

func (t *Term) Status(label string) {
	if label == "" {
		t.spin.Stop()
		return
	}
	t.spin.Start(label)
}

func (t *Term) Text(delta string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.spin.Stop()
	t.md.Write(delta)
}

func (t *Term) Thinking(delta string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if delta == "" {
		return
	}
	t.spin.Stop()
	t.think.WriteString(delta)
	all := t.think.String()
	i := strings.LastIndexByte(all, '\n')
	if i < 0 {
		return
	}
	t.think.Reset()
	t.think.WriteString(all[i+1:])
	t.thinkLines(all[:i])
}

func (t *Term) thinkLines(s string) {
	for _, l := range strings.Split(s, "\n") {
		if !t.thinking {
			if strings.TrimSpace(l) == "" {
				continue
			}
			t.thinking = true
			fmt.Fprintln(t.out, ui.Dim(ui.Italic("✻ "+l)))
			continue
		}
		fmt.Fprintln(t.out, ui.Dim(ui.Italic("  "+l)))
	}
}

func (t *Term) BlockEnd() {
	t.mu.Lock()
	defer t.mu.Unlock()
	printed := false
	if t.think.Len() > 0 || t.thinking {
		t.spin.Stop()
		if t.think.Len() > 0 {
			rest := t.think.String()
			t.think.Reset()
			t.thinkLines(rest)
		}
		printed = t.thinking
		t.thinking = false
	}
	if t.md.Started() {
		t.spin.Stop()
		t.md.Flush()
		printed = true
	}
	if printed {
		fmt.Fprintln(t.out)
		t.spin.Start("Working…")
	}
}

func header(name, summary string) string {
	h := ui.Bold(name)
	if summary != "" {
		h += ui.Dim("(") + ui.Truncate(strings.ReplaceAll(summary, "\n", " ⏎ "), 110) + ui.Dim(")")
	}
	return h
}

func (t *Term) ToolStart(id, name, summary string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.spin.Stop()
	fmt.Fprintln(t.out, ui.Accent("● ")+header(name, summary))
	t.lastTool = id
}

func (t *Term) ToolEnd(id, name, summary string, res tools.Result, dur time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.spin.Stop()
	body := res.Display
	limit := 40
	if body == "" || res.IsError {
		body = res.Text
		limit = 6
		if res.IsError {
			limit = 10
		}
	}
	if t.verbose {
		body, limit = res.Text, 400
	}
	shown, more := ui.FirstLines(body, limit)
	lines := strings.Split(shown, "\n")
	if more > 0 {
		lines = append(lines, ui.Dim(fmt.Sprintf("… +%d lines", more)))
	}
	if t.lastTool != id { // results of a parallel batch arrive out of order
		lines = append([]string{ui.Dim(name + "(" + ui.Truncate(summary, 60) + ")")}, lines...)
	}
	for i, l := range lines {
		l = ui.Truncate(l, 400)
		if res.IsError {
			l = ui.Red(l)
		} else if res.Display == "" || t.verbose {
			l = ui.Dim(l)
		}
		lead := "    "
		if i == 0 {
			lead = ui.Dim("  ⎿ ")
		}
		fmt.Fprintln(t.out, lead+l)
	}
	if dur > 5*time.Second {
		fmt.Fprintln(t.out, ui.Dim(fmt.Sprintf("    (%s)", dur.Round(100*time.Millisecond))))
	}
	fmt.Fprintln(t.out)
	t.lastTool = ""
}

func (t *Term) ServerTool(name, summary string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.spin.Stop()
	if name == "web_search" {
		name = "WebSearch"
	}
	fmt.Fprintln(t.out, ui.Accent("● ")+header(name, summary))
	fmt.Fprintln(t.out)
}

func (t *Term) Notice(msg string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.spin.Stop()
	fmt.Fprintln(t.out, ui.Dim(msg))
}

// Println prints a line outside of a turn.
func (t *Term) Println(a ...any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.spin.Stop()
	fmt.Fprintln(t.out, a...)
}

// readLine asks a one-line question. ok is false on ctrl+c or end of input.
func (t *Term) readLine(prompt string) (string, bool) {
	t.spin.Stop()
	if t.rl == nil {
		return "", false
	}
	old := t.rl.Config.Prompt
	t.rl.SetPrompt(prompt)
	defer t.rl.SetPrompt(old)
	t.rl.HistoryDisable()
	defer t.rl.HistoryEnable()
	line, err := t.rl.Readline()
	if err != nil {
		if !errors.Is(err, readline.ErrInterrupt) && !errors.Is(err, io.EOF) {
			fmt.Fprintln(t.out, ui.Red(err.Error()))
		}
		return "", false
	}
	return strings.TrimSpace(line), true
}

func (t *Term) box(title, body string) {
	fmt.Fprintln(t.out, ui.Yellow("╭─ "+title))
	for _, l := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		fmt.Fprintln(t.out, ui.Yellow("│ ")+l)
	}
	fmt.Fprintln(t.out, ui.Yellow("╰─"))
}

func (t *Term) AskPermission(p agent.PermissionPrompt) agent.PermissionAnswer {
	t.askMu.Lock()
	defer t.askMu.Unlock()
	t.mu.Lock()
	t.spin.Stop()
	detail, more := ui.FirstLines(p.Detail, 30)
	if more > 0 {
		detail += "\n" + ui.Dim(fmt.Sprintf("… +%d lines", more))
	}
	body := header(p.Tool, p.Summary)
	if detail != "" && strings.HasPrefix(p.Detail, p.Summary) {
		body = ui.Bold(p.Tool) + "\n" + detail // the detail already shows the full command
	} else if detail != "" {
		body += "\n" + detail
	}
	t.box("Permission required", body)
	always := "Yes, and don't ask again for " + ui.Bold(p.Rule) + " in this project"
	if p.IsEdit {
		always = "Yes, and allow all edits for the rest of this session"
	}
	fmt.Fprintln(t.out, "  1. Yes")
	fmt.Fprintln(t.out, "  2. "+always)
	fmt.Fprintln(t.out, "  3. No, and tell the agent what to do instead")
	t.mu.Unlock()

	for {
		ans, ok := t.readLine(ui.Yellow("  Choose [1/2/3] ❯ "))
		if !ok {
			return agent.PermissionAnswer{Interrupt: true}
		}
		switch strings.ToLower(ans) {
		case "", "1", "y", "yes":
			return agent.PermissionAnswer{Allow: true}
		case "2", "a", "always":
			return agent.PermissionAnswer{Allow: true, Always: true}
		case "3", "n", "no":
			fb, ok := t.readLine("  What should it do instead? (enter to skip) ❯ ")
			if !ok {
				return agent.PermissionAnswer{Interrupt: true}
			}
			return agent.PermissionAnswer{Feedback: fb}
		default:
			if len(ans) > 3 { // free text counts as "no, do this instead"
				return agent.PermissionAnswer{Feedback: ans}
			}
			t.Println(ui.Dim("  Type 1, 2 or 3."))
		}
	}
}

func (t *Term) ApprovePlan(plan string) (bool, permissions.Mode, string) {
	t.askMu.Lock()
	defer t.askMu.Unlock()
	t.mu.Lock()
	t.spin.Stop()
	fmt.Fprintln(t.out, ui.Cyan("╭─ Proposed plan"))
	md := ui.Markdown{W: t.out, Prefix: ui.Cyan("│ "), Indent: ui.Cyan("│ ")}
	md.Write(plan + "\n")
	md.Flush()
	fmt.Fprintln(t.out, ui.Cyan("╰─"))
	fmt.Fprintln(t.out, "  1. Yes, and auto-accept edits")
	fmt.Fprintln(t.out, "  2. Yes, and ask before each edit")
	fmt.Fprintln(t.out, "  3. No, keep planning")
	t.mu.Unlock()
	for {
		ans, ok := t.readLine(ui.Cyan("  Choose [1/2/3] ❯ "))
		if !ok {
			return false, permissions.ModePlan, "the user interrupted"
		}
		switch strings.ToLower(ans) {
		case "", "1", "y", "yes":
			return true, permissions.ModeAcceptEdits, ""
		case "2":
			return true, permissions.ModeDefault, ""
		case "3", "n", "no":
			fb, _ := t.readLine("  What should change? (enter to skip) ❯ ")
			return false, permissions.ModePlan, fb
		default:
			if len(ans) > 3 {
				return false, permissions.ModePlan, ans
			}
			t.Println(ui.Dim("  Type 1, 2 or 3."))
		}
	}
}
