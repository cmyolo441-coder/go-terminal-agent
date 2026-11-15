package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/chzyer/readline"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/agent"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/config"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/hooks"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permissions"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/ui"
)

// REPL is the interactive session.
type REPL struct {
	app     *App
	term    *Term
	rl      *readline.Instance
	pending []string // context to attach to the next prompt (shell output, hook output)
}

// RunREPL starts the interactive session and returns the exit code.
func RunREPL(opts Options) int {
	term := NewTerm(opts.Verbose)
	app, err := NewApp(opts, term)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer app.Close()
	r := &REPL{app: app, term: term}

	_ = os.MkdirAll(config.Home(), 0o700)
	rl, err := readline.NewEx(&readline.Config{
		Prompt:          r.prompt(),
		HistoryFile:     filepath.Join(config.Home(), "history"),
		HistoryLimit:    2000,
		AutoComplete:    &completer{r: r},
		InterruptPrompt: "",
		EOFPrompt:       "",
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: cannot start the line editor:", err)
		return 1
	}
	defer rl.Close()
	r.rl, term.rl = rl, rl

	r.banner()
	if out := app.Agent.Hooks.Run(context.Background(), hooks.SessionStart, "", nil); out.Output != "" {
		r.pending = append(r.pending, "<session-start-hook>\n"+out.Output+"\n</session-start-hook>")
	}
	if strings.TrimSpace(opts.Prompt) != "" {
		term.Println(ui.Dim("> ") + opts.Prompt)
		if r.handle(opts.Prompt) {
			return 0
		}
	}

	var lastInterrupt time.Time
	for {
		rl.SetPrompt(r.prompt())
		line, err := rl.Readline()
		if errors.Is(err, readline.ErrInterrupt) {
			if line == "" {
				if time.Since(lastInterrupt) < 2*time.Second {
					break
				}
				lastInterrupt = time.Now()
				term.Println(ui.Dim("Press ctrl+c again to exit."))
			}
			continue
		}
		if err != nil { // io.EOF (ctrl+d) or a closed terminal
			if !errors.Is(err, io.EOF) {
				fmt.Fprintln(os.Stderr, "error:", err)
			}
			break
		}
		// A trailing backslash continues the prompt on the next line.
		for strings.HasSuffix(line, "\\") {
			rl.SetPrompt(ui.Dim("… "))
			more, err := rl.Readline()
			if err != nil {
				line = strings.TrimSuffix(line, "\\")
				break
			}
			line = strings.TrimSuffix(line, "\\") + "\n" + more
		}
		if r.handle(line) {
			break
		}
	}
	r.goodbye()
	return 0
}

func (r *REPL) prompt() string {
	tag := ""
	switch r.app.Agent.Perm.Mode() {
	case permissions.ModePlan:
		tag = ui.Cyan("⏸ plan ")
	case permissions.ModeAcceptEdits:
		tag = ui.Magenta("⏵⏵ accept edits ")
	case permissions.ModeBypass:
		tag = ui.Red("⚠ bypass ")
	}
	return tag + ui.Bold(ui.Accent("❯ "))
}

func (r *REPL) banner() {
	a := r.app.Agent
	home, _ := os.UserHomeDir()
	cwd := r.app.Cwd
	if home != "" && strings.HasPrefix(cwd, home) {
		cwd = "~" + strings.TrimPrefix(cwd, home)
	}
	lines := []string{
		ui.Accent("✻ ") + ui.Bold(config.AppName) + ui.Dim(" v"+config.Version),
		ui.Dim("  model: ") + a.Model + ui.Dim(" · effort: ") + a.Effort,
		ui.Dim("  cwd:   ") + cwd,
	}
	fmt.Println(ui.Accent("╭" + strings.Repeat("─", 60)))
	for _, l := range lines {
		fmt.Println(ui.Accent("│ ") + l)
	}
	fmt.Println(ui.Accent("╰" + strings.Repeat("─", 60)))
	fmt.Println(ui.Dim("  /help for commands · @path to attach files · !cmd to run a shell command"))
	fmt.Println(ui.Dim("  # to save a note to memory · end a line with \\ for multi-line · ctrl+c interrupts"))
	for _, w := range r.app.Warnings {
		fmt.Println(ui.Yellow("  ⚠ " + w))
	}
	if n := len(r.app.MCP); n > 0 {
		fmt.Println(ui.Dim(fmt.Sprintf("  %d MCP server(s) connected", n)))
	}
	if r.app.Resumed > 0 {
		fmt.Println(ui.Dim(fmt.Sprintf("  Resumed session %s (%d messages)", a.Session.ID, r.app.Resumed)))
		if tail := lastAssistantText(a.Messages); tail != "" {
			shown, more := ui.FirstLines(tail, 8)
			fmt.Println(ui.Dim("  Last reply:"))
			for _, l := range strings.Split(shown, "\n") {
				fmt.Println(ui.Dim("    " + l))
			}
			if more > 0 {
				fmt.Println(ui.Dim(fmt.Sprintf("    … +%d lines", more)))
			}
		}
	}
	fmt.Println()
}

func (r *REPL) goodbye() {
	a := r.app.Agent
	if _, _, _, _, reqs, _ := a.Usage.Snapshot(); reqs > 0 {
		fmt.Println(ui.Dim(fmt.Sprintf("Session %s saved · cost %s · resume with: %s --resume %s",
			a.Session.ID, a.Usage.CostString(), config.AppName, a.Session.ID)))
	}
}

// handle processes one line of input. It returns true when the session should end.
func (r *REPL) handle(line string) bool {
	line = strings.TrimSpace(line)
	switch {
	case line == "":
		return false
	case line == "exit" || line == "quit":
		return true
	case strings.HasPrefix(line, "/"):
		prompt, quit := r.command(line)
		if quit {
			return true
		}
		if prompt != "" {
			r.turn(prompt)
		}
	case strings.HasPrefix(line, "!"):
		r.shell(strings.TrimSpace(line[1:]))
	case strings.HasPrefix(line, "#"):
		note := strings.TrimSpace(line[1:])
		if note == "" {
			return false
		}
		if p, err := config.AppendMemory(r.app.Cwd, note); err != nil {
			r.term.Println(ui.Red("Could not save: " + err.Error()))
		} else {
			r.term.Println(ui.Dim("Saved to " + p + " (applies from the next session)"))
		}
	default:
		r.turn(line)
	}
	return false
}

// shell runs a command directly and keeps its output as context for the next prompt.
func (r *REPL) shell(cmd string) {
	if cmd == "" {
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	t, _ := r.app.Agent.Tools.Get("Bash")
	if t == nil {
		r.term.Println(ui.Red("The Bash tool is disabled."))
		return
	}
	in := fmt.Sprintf(`{"command":%q}`, cmd)
	res := t.Run(ctx, []byte(in), r.app.Agent.Env)
	out := res.Text
	if res.IsError {
		out = ui.Red(out)
	}
	r.term.Println(out)
	r.pending = append(r.pending, fmt.Sprintf("<user-shell-command>\n$ %s\n%s\n</user-shell-command>", cmd, res.Text))
}

// turn sends a prompt to the agent and waits for it to finish.
func (r *REPL) turn(prompt string) {
	a := r.app.Agent
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	go func() {
		select {
		case <-sig:
			cancel()
		case <-ctx.Done():
		}
	}()

	hook := a.Hooks.Run(ctx, hooks.UserPromptSubmit, "", map[string]any{"prompt": prompt})
	if hook.Blocked {
		r.term.Println(ui.Red("Prompt blocked by a hook: " + hook.Reason))
		return
	}
	blocks, attached := expandMentions(r.app.Cwd, prompt)
	if len(attached) > 0 {
		r.term.Println(ui.Dim("  ⎿ attached " + strings.Join(attached, ", ")))
	}
	if hook.Output != "" {
		r.pending = append(r.pending, "<user-prompt-submit-hook>\n"+hook.Output+"\n</user-prompt-submit-hook>")
	}
	for _, p := range r.pending {
		blocks = append(blocks, anthropic.NewBetaTextBlock(p))
	}
	r.pending = nil
	fmt.Println()

	start := time.Now()
	in0, out0, _, _, _, _ := a.Usage.Snapshot()
	_, err := a.Run(ctx, prompt, blocks...)
	r.term.Status("")
	switch {
	case errors.Is(err, agent.ErrInterrupted):
		r.term.Println(ui.Yellow("  ⎿ Interrupted. Tell the agent what to do next."))
	case err != nil:
		r.term.Println(ui.Red("✗ " + err.Error()))
	}
	in1, out1, _, _, _, _ := a.Usage.Snapshot()
	pct := 0
	if w := agent.ContextWindow(a.Model); w > 0 {
		pct = int(a.LastContext * 100 / w)
	}
	r.term.Println(ui.Dim(fmt.Sprintf("  %s · ↑ %s ↓ %s tokens · %s total · context %d%%",
		time.Since(start).Round(100*time.Millisecond), ui.HumanTokens(in1-in0), ui.HumanTokens(out1-out0),
		a.Usage.CostString(), pct)))
	fmt.Println()
}

// completer offers slash commands and @file paths.
type completer struct{ r *REPL }

func (c *completer) Do(line []rune, pos int) ([][]rune, int) {
	head := string(line[:pos])
	start := strings.LastIndexAny(head, " \t") + 1
	word := head[start:]
	var cands []string
	switch {
	case start == 0 && strings.HasPrefix(word, "/"):
		for _, name := range c.r.commandNames() {
			if strings.HasPrefix("/"+name, word) {
				cands = append(cands, "/"+name+" ")
			}
		}
	case strings.HasPrefix(word, "@"):
		partial := word[1:]
		dir, base := filepath.Split(partial)
		entries, _ := os.ReadDir(filepath.Join(c.r.app.Cwd, dir))
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), base) || (strings.HasPrefix(e.Name(), ".") && base == "") {
				continue
			}
			s := "@" + dir + e.Name()
			if e.IsDir() {
				s += "/"
			} else {
				s += " "
			}
			cands = append(cands, s)
		}
	}
	sort.Strings(cands)
	out := make([][]rune, 0, len(cands))
	for _, s := range cands {
		out = append(out, []rune(s[len(word):]))
	}
	return out, len([]rune(word))
}

// lastAssistantText finds the most recent assistant text in the history.
func lastAssistantText(msgs []anthropic.BetaMessageParam) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		role, blocks := decodeMessage(msgs[i])
		if role != "assistant" {
			continue
		}
		for j := len(blocks) - 1; j >= 0; j-- {
			if blocks[j].Type == "text" && strings.TrimSpace(blocks[j].Text) != "" {
				return strings.TrimSpace(blocks[j].Text)
			}
		}
	}
	return ""
}
