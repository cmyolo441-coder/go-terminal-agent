package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

var toolNames = map[string]string{
	"read_file": "Read", "write_file": "Write", "edit_file": "Edit", "list_dir": "List",
	"glob": "Glob", "grep": "Grep", "bash": "Bash", "todo_write": "Tasks",
	"web_search": "Web search", "web_fetch": "Web fetch", "plan": "Plan", "verify": "Verify",
}

func displayName(tool string) string {
	if n, ok := toolNames[tool]; ok {
		return n
	}
	return tool
}

// toolArg is the short argument shown next to a tool's name.
func toolArg(c *tools.Call) string {
	switch c.Tool {
	case "bash":
		return oneLine(c.Command, 90)
	case "todo_write":
		return ""
	}
	// Summaries read "Verb target"; show the target.
	if i := strings.IndexByte(c.Summary, ' '); i > 0 {
		return c.Summary[i+1:]
	}
	return c.Summary
}

func toolHeader(c *tools.Call, ok bool) string {
	bullet := sGreen.Render(gBullet)
	if !ok {
		bullet = sRed.Render(gBullet)
	}
	h := bullet + " " + sBold.Render(displayName(c.Tool))
	if arg := toolArg(c); arg != "" {
		h += " " + sMuted.Render(arg)
	}
	return h
}

// toolLine is the focus-density rendering of a finished call: a ✓ or ✗,
// then the command itself for shell calls (as typed, clipped with …) or
// the tool and its target, and a +/− stat for edits.
func toolLine(c *tools.Call, output string, err error, width int) string {
	mark := sGreen.Render(gCheck)
	if err != nil {
		mark = sRed.Render(gCross)
	}
	avail := max(20, width-4)
	var body string
	switch c.Tool {
	case "bash":
		body = sText.Render(oneLine(c.Command, avail))
	default:
		name := displayName(c.Tool)
		arg := toolArg(c)
		stat := ""
		if c.Tool == "write_file" || c.Tool == "edit_file" {
			add, del := diffStat(c.Preview)
			stat = " " + sGreen.Render(fmt.Sprintf("+%d", add)) + " " + sRed.Render(fmt.Sprintf("−%d", del))
		}
		room := avail - runewidth.StringWidth(name) - 1 - lipgloss.Width(stat)
		body = sBold.Render(name)
		if arg != "" && room > 4 {
			body += " " + sMuted.Render(oneLine(arg, room))
		}
		body += stat
	}
	line := mark + " " + body
	if err != nil {
		line += "\n" + resultBlock([]string{sRed.Render(oneLine(err.Error(), max(20, width-8)))})
	}
	return line
}

// resultLines renders the indented result under a tool header.
func resultBlock(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	var b strings.Builder
	for i, l := range lines {
		if i == 0 {
			b.WriteString("  " + sFaint.Render(gResult) + "  " + l)
		} else {
			b.WriteString("\n     " + l)
		}
	}
	return b.String()
}

// renderToolDone renders a finished tool call with its result, collapsed
// or expanded according to the layout density.
func renderToolDone(c *tools.Call, output string, err error, width int, d density) string {
	if c.Tool == "todo_write" {
		return "" // the task list renders on its own
	}
	if d.oneLine {
		return toolLine(c, output, err, width)
	}
	head := toolHeader(c, err == nil)
	inner := width - 6
	if inner < 20 {
		inner = 20
	}
	var lines []string
	switch {
	case err != nil && c.Tool != "bash":
		lines = append(lines, sRed.Render(oneLine(err.Error(), inner)))
	case c.Tool == "read_file":
		n := strings.Count(output, "\n")
		lines = append(lines, sMuted.Render(fmt.Sprintf("Read %d line%s", n, plural(n))))
	case c.Tool == "write_file" || c.Tool == "edit_file":
		add, del := diffStat(c.Preview)
		stat := fmt.Sprintf("+%d −%d", add, del)
		msg := oneLine(output, max(8, inner-runewidth.StringWidth(stat)-2))
		lines = append(lines, sMuted.Render(msg+"  ")+sGreen.Render(fmt.Sprintf("+%d", add))+" "+sRed.Render(fmt.Sprintf("−%d", del)))
		lines = append(lines, renderDiff(c.Preview, inner, d.diffMax)...)
	case c.Tool == "bash":
		lines = append(lines, tailLines(output, d.bashTail, inner, err != nil)...)
	case c.Tool == "todo_write":
		return "" // the task list renders on its own
	default:
		lines = append(lines, headLines(output, d.toolHead, inner)...)
	}
	if rb := resultBlock(lines); rb != "" {
		return head + "\n" + rb
	}
	return head
}

func renderToolFailed(name, reason string, width int) string {
	head := sRed.Render(gBullet) + " " + sBold.Render(displayName(name))
	return head + "\n" + resultBlock([]string{sRed.Render(oneLine(reason, width-6))})
}

// headLines shows the first n lines of s, then a "+K lines" hint.
func headLines(s string, n, width int) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return []string{sMuted.Render("(no output)")}
	}
	kept, hidden := collapse(strings.Split(s, "\n"), n, false)
	var out []string
	for _, l := range kept {
		out = append(out, sMuted.Render(clip(sanitize(stripANSI(l)), width)))
	}
	if hidden > 0 {
		out = append(out, sFaint.Render(fmt.Sprintf("… +%d lines", hidden)))
	}
	return out
}

// tailLines shows the last n lines of s after a "+K earlier lines" hint;
// command output ends with what matters most.
func tailLines(s string, n, width int, failed bool) []string {
	s = strings.TrimRight(s, "\n")
	kept, hidden := collapse(strings.Split(s, "\n"), n, true)
	var out []string
	if hidden > 0 {
		out = append(out, sFaint.Render(fmt.Sprintf("… +%d earlier lines", hidden)))
	}
	for _, l := range kept {
		st := sMuted
		if strings.HasPrefix(l, "[exit code") {
			st = sFaint
			if failed {
				st = sRed
			}
		}
		out = append(out, st.Render(clip(sanitize(stripANSI(l)), width)))
	}
	return out
}

func renderTodos(todos []tools.Todo) string {
	if len(todos) == 0 {
		return ""
	}
	var b strings.Builder
	done := 0
	for _, t := range todos {
		if t.Status == "completed" {
			done++
		}
	}
	b.WriteString(sGreen.Render(gBullet) + " " + sBold.Render("Tasks") + " " + sMuted.Render(fmt.Sprintf("%d/%d", done, len(todos))))
	for i, t := range todos {
		prefix := "     "
		if i == 0 {
			prefix = "  " + sFaint.Render(gResult) + "  "
		}
		switch t.Status {
		case "completed":
			b.WriteString("\n" + prefix + sGreen.Render(gTodoDone) + " " + sFaint.Strikethrough(true).Render(t.Content))
		case "in_progress":
			b.WriteString("\n" + prefix + sAccent.Render(gTodoNow) + " " + sAccentB.Render(t.Content))
		default:
			b.WriteString("\n" + prefix + sMuted.Render(gTodo+" "+t.Content))
		}
	}
	return b.String()
}

func renderThinking(text string, width, maxLines int) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	wrapped := lipgloss.NewStyle().Width(width - 4).Render(text)
	lines, hidden := collapse(strings.Split(wrapped, "\n"), maxLines, false)
	if hidden > 0 {
		lines = append(lines, fmt.Sprintf("… +%d lines", hidden))
	}
	var b strings.Builder
	for i, l := range lines {
		if i == 0 {
			b.WriteString(sAccent.Render(gThink) + " " + sItalic.Render(strings.TrimRight(l, " ")))
		} else {
			b.WriteString("\n  " + sItalic.Render(strings.TrimRight(l, " ")))
		}
	}
	return b.String()
}

func renderUser(text string, width int) string {
	wrapped := lipgloss.NewStyle().Width(width - 4).Render(text)
	var b strings.Builder
	for i, l := range strings.Split(wrapped, "\n") {
		if i > 0 {
			b.WriteString("\n")
		}
		mark := " "
		if i == 0 {
			mark = gPrompt
		}
		b.WriteString(sUserBar.Render(mark) + " " + sText.Render(strings.TrimRight(l, " ")))
	}
	return b.String()
}

func renderNotice(msg string) string { return sAmber.Render("! ") + sText.Render(msg) }
func renderError(msg string) string  { return sRed.Render(gCross+" ") + sRed.Render(msg) }
func renderInfo(msg string) string   { return sAccent2.Render(gDiamond+" ") + sText.Render(msg) }

func renderTurnFooter(elapsed time.Duration, u llm.Usage, cost float64, costKnown bool) string {
	parts := []string{fmt.Sprintf("%s %s", gCheck, fmtDuration(elapsed))}
	parts = append(parts, fmt.Sprintf("↑ %s ↓ %s", fmtTokens(u.ContextTokens()), fmtTokens(u.OutputTokens)))
	if u.CacheReadTokens > 0 {
		parts = append(parts, fmt.Sprintf("%s cached", fmtTokens(u.CacheReadTokens)))
	}
	if costKnown {
		parts = append(parts, fmtCost(cost))
	}
	return sFaint.Render("  " + strings.Join(parts, " · "))
}

func fmtTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func fmtCost(c float64) string {
	if c < 0.01 {
		return fmt.Sprintf("$%.4f", c)
	}
	return fmt.Sprintf("$%.2f", c)
}

func fmtDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	return clip(s, max)
}

func clip(s string, max int) string {
	if max <= 1 || runewidth.StringWidth(s) <= max {
		return s
	}
	return runewidth.Truncate(s, max-1, "") + "…"
}

func padRight(s string, w int) string {
	if n := runewidth.StringWidth(s); n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return s
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// prettyPath shortens the home directory to ~.
func prettyPath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
			if rel == "." {
				return "~"
			}
			return filepath.Join("~", rel)
		}
	}
	return p
}

// clipMiddle shortens s to at most max display cells by replacing its middle
// with an ellipsis, keeping the start and the more telling end of a path.
func clipMiddle(s string, max int) string {
	if max < 8 || runewidth.StringWidth(s) <= max {
		return s
	}
	rs := []rune(s)
	keep := max - 1
	head := keep / 3
	tail := keep - head
	for runewidth.StringWidth(string(rs[:head]))+1+runewidth.StringWidth(string(rs[len(rs)-tail:])) > max && tail > 1 {
		tail--
	}
	return string(rs[:head]) + "…" + string(rs[len(rs)-tail:])
}
