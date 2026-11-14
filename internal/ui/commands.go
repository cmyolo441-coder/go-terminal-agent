package ui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/agent"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/config"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permission"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/pricing"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/prompt"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/session"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

// command is a slash command.
type command struct {
	name    string
	aliases []string
	args    string
	help    string
	run     func(m *model, arg string) tea.Cmd
	// file marks an @file completion rather than a slash command.
	file bool
}

// Extra lets the application register more slash commands (git, MCP, ...).
type Extra struct {
	Name string
	Args string
	Help string
	// Run returns text to print (Markdown allowed) or an error.
	Run func(ctx context.Context, arg string) (string, error)
	// Prompt, when set, turns the command into a prompt sent to the agent.
	Prompt func(arg string) (string, error)
	// Job, when set, runs like an agent turn: it streams into the
	// transcript through sink, asks approvals in the usual dialog, can be
	// interrupted with esc, and its returned Markdown is printed at the end.
	Job func(ctx context.Context, arg string, sink agent.Sink) (string, error)
}

var extras []Extra

// Register adds application-level slash commands. Call before Run.
func Register(e ...Extra) { extras = append(extras, e...) }

var builtin []command

func init() {
	builtin = []command{
		{name: "help", aliases: []string{"?"}, help: "Show commands and keys", run: cmdHelp},
		{name: "status", help: "Dashboard: session, model, safety, integrations, workspace", run: cmdStatus},
		{name: "clear", aliases: []string{"new", "reset"}, help: "Start a new conversation", run: cmdClear},
		{name: "model", args: "[id]", help: "Show or switch the model", run: cmdModel},
		{name: "effort", args: "[level]", help: "Reasoning effort: low, medium, high, xhigh, max", run: cmdEffort},
		{name: "mode", args: "[mode]", help: "Permissions: ask, auto-edit, plan, yolo", run: cmdMode},
		{name: "thinking", help: "Toggle showing the model's reasoning summary", run: cmdThinking},
		{name: "theme", aliases: []string{"themes"}, args: "[name]", help: "List color themes, or switch and preview one", run: cmdTheme},
		{name: "density", aliases: []string{"layout"}, args: "[compact|expanded]", help: "How much tool output to show", run: cmdDensity},
		{name: "cost", aliases: []string{"usage"}, help: "Token usage and estimated cost", run: cmdCost},
		{name: "tasks", aliases: []string{"todos"}, help: "Show the agent's task list", run: cmdTodos},
		{name: "compact", args: "[focus]", help: "Summarize the conversation to free context", run: cmdCompact},
		{name: "sessions", help: "List saved sessions for this workspace", run: cmdSessions},
		{name: "resume", args: "<id>", help: "Resume a saved session", run: cmdResume},
		{name: "init", help: "Write an AGENTS.md describing this repository", run: cmdInit},
		{name: "memory", help: "Show loaded project memory files", run: cmdMemory},
		{name: "tools", help: "List the tools the agent can use", run: cmdTools},
		{name: "config", help: "Show the effective configuration", run: cmdConfig},
		{name: "exit", aliases: []string{"quit", "q"}, help: "Exit", run: func(m *model, _ string) tea.Cmd { return m.quit() }},
	}
}

func allCommands() []command {
	out := append([]command(nil), builtin...)
	for _, e := range extras {
		e := e
		out = append(out, command{name: e.Name, args: e.Args, help: e.Help, run: func(m *model, arg string) tea.Cmd {
			return m.runExtra(e, arg)
		}})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func matchCommands(prefix string) []command {
	var out []command
	for _, c := range allCommands() {
		if strings.HasPrefix(c.name, prefix) {
			out = append(out, c)
			continue
		}
		for _, a := range c.aliases {
			if strings.HasPrefix(a, prefix) && prefix != "" {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

func findCommand(name string) (command, bool) {
	for _, c := range allCommands() {
		if c.name == name {
			return c, true
		}
		for _, a := range c.aliases {
			if a == name {
				return c, true
			}
		}
	}
	return command{}, false
}

func (m *model) runCommand(line string) tea.Cmd {
	name, arg, _ := strings.Cut(strings.TrimPrefix(line, "/"), " ")
	arg = strings.TrimSpace(arg)
	c, ok := findCommand(name)
	if !ok {
		m.emit(renderError(fmt.Sprintf("Unknown command /%s. Type /help for the list.", name)))
		return nil
	}
	m.emit(sAccent.Render("/"+c.name) + " " + sMuted.Render(arg))
	return c.run(m, arg)
}

func (m *model) runExtra(e Extra, arg string) tea.Cmd {
	if e.Job != nil {
		if m.phase != phaseIdle {
			m.emit(renderNotice("Wait for the current turn to finish."))
			return nil
		}
		job, prog := e.Job, m.prog
		// runCommand already echoed the command line.
		return m.launch(func(ctx context.Context, sink agent.Sink) error {
			out, err := job(ctx, arg, sink)
			if strings.TrimSpace(out) != "" {
				prog.Send(extraDoneMsg{out: out})
			}
			return err
		})
	}
	if e.Prompt != nil {
		if m.phase != phaseIdle {
			m.emit(renderNotice("Wait for the current turn to finish."))
			return nil
		}
		p, err := e.Prompt(arg)
		if err != nil {
			m.emit(renderError(err.Error()))
			return nil
		}
		return m.startRun(p, "")
	}
	run := e.Run
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		out, err := run(ctx, arg)
		return extraDoneMsg{out: out, err: err}
	}
}

type extraDoneMsg struct {
	out string
	err error
}

func (m *model) printMarkdown(md string) {
	m.emit(m.md.render(md, m.width-2))
}

// ---------- builtin commands ----------

func cmdHelp(m *model, _ string) tea.Cmd {
	var b strings.Builder
	b.WriteString(sAccentB.Render("Commands") + "\n")
	col := 24
	for _, c := range allCommands() {
		col = max(col, min(32, len("/"+c.name+" "+c.args)+2))
	}
	for _, c := range allCommands() {
		name := "/" + c.name
		if c.args != "" {
			name += " " + c.args
		}
		b.WriteString("  " + sAccent2.Render(padRight(name, col)) + sMuted.Render(c.help) + "\n")
	}
	b.WriteString("\n" + sAccentB.Render("Input") + "\n")
	for _, kv := range [][2]string{
		{"@path", "attach a file's contents to your message"},
		{"!command", "run a shell command; its output goes with your next message"},
		{"enter", "send · alt+enter or ctrl+j for a new line"},
		{"↑ / ↓", "history · tab completes commands"},
		{"shift+tab", "cycle permission mode"},
		{"ctrl+b", "show or hide the live browser panel (gta --browser)"},
		{"esc", "interrupt the agent · ctrl+c twice to exit"},
	} {
		b.WriteString("  " + sKey.Render(padRight(kv[0], col)) + sMuted.Render(kv[1]) + "\n")
	}
	m.emit(strings.TrimRight(b.String(), "\n"))
	return nil
}

func cmdClear(m *model, _ string) tea.Cmd {
	if m.phase != phaseIdle {
		m.emit(renderNotice("Wait for the current turn to finish, or press esc first."))
		return nil
	}
	m.saveSession()
	m.ag.Reset()
	m.sess = session.New(m.opts.Root, m.ag.Settings().Model)
	m.emit(renderInfo("New conversation started."))
	return nil
}

var knownModels = []string{"claude-opus-5-5", "claude-fable-5-2", "claude-sonnet-5-5", "claude-haiku-5-5"}

func cmdModel(m *model, arg string) tea.Cmd {
	if arg == "" {
		cur := m.ag.Settings().Model
		var b strings.Builder
		b.WriteString(renderInfo("Model: " + cur))
		for _, id := range knownModels {
			mark := "  "
			if id == cur {
				mark = sAccent.Render(gPrompt) + " "
			}
			line := mark + sText.Render(padRight(id, 22))
			if r, ok := pricing.Lookup(id); ok {
				line += sFaint.Render(fmt.Sprintf("$%g / $%g per M tokens", r.Input, r.Output))
			}
			b.WriteString("\n" + line)
		}
		b.WriteString("\n" + sFaint.Render("  /model <id> to switch; any model id your provider serves works."))
		m.emit(b.String())
		return nil
	}
	m.ag.Update(func(s *agentSettings) { s.Model = arg })
	m.emit(renderInfo("Model set to " + arg))
	return nil
}

func cmdEffort(m *model, arg string) tea.Cmd {
	if arg == "" {
		m.emit(renderInfo("Effort: " + orDefault(m.ag.Settings().Effort, "model default") + " (" + strings.Join(config.Efforts, ", ") + ")"))
		return nil
	}
	if !config.ValidEffort(arg) {
		m.emit(renderError("Effort must be one of " + strings.Join(config.Efforts, ", ")))
		return nil
	}
	m.ag.Update(func(s *agentSettings) { s.Effort = arg })
	m.emit(renderInfo("Effort set to " + arg))
	return nil
}

func cmdMode(m *model, arg string) tea.Cmd {
	if arg == "" {
		mode := m.ag.Gate.Mode()
		m.emit(renderInfo("Mode: " + string(mode) + " · " + modeHelp(mode)))
		return nil
	}
	mode, err := permission.ParseMode(arg)
	if err != nil {
		m.emit(renderError(err.Error()))
		return nil
	}
	mode, why := m.ag.Gate.Switch(mode)
	if why != "" {
		m.emit(renderError(why))
	}
	m.emit(renderInfo("Mode set to " + string(mode) + " · " + modeHelp(mode)))
	return nil
}

func cmdThinking(m *model, _ string) tea.Cmd {
	var on bool
	m.ag.Update(func(s *agentSettings) { s.ShowThinking = !s.ShowThinking; on = s.ShowThinking })
	if on {
		m.emit(renderInfo("Reasoning summaries are shown."))
	} else {
		m.emit(renderInfo("Reasoning summaries are hidden."))
	}
	return nil
}

func cmdTodos(m *model, _ string) tea.Cmd {
	t := m.ag.Env.Todos()
	if len(t) == 0 {
		m.emit(renderInfo("No tasks yet."))
		return nil
	}
	m.emit(renderTodos(t))
	return nil
}

const compactPrompt = `Summarize our conversation so far so that work can continue in a fresh context. Include: the user's goals and constraints, decisions made, files created or changed (with paths), the current state of the work, open problems, and the exact next steps. Be complete but concise. Do not call any tools.`

func cmdCompact(m *model, arg string) tea.Cmd {
	if m.phase != phaseIdle {
		m.emit(renderNotice("Wait for the current turn to finish."))
		return nil
	}
	if len(m.ag.History()) == 0 {
		m.emit(renderInfo("Nothing to compact yet."))
		return nil
	}
	p := compactPrompt
	if arg != "" {
		p += "\nFocus especially on: " + arg
	}
	m.compacting = true
	return m.startRun(p, "")
}

// finishCompact replaces the history with the summary the model just wrote.
func (m *model) finishCompact() {
	m.compacting = false
	h := m.ag.History()
	if len(h) == 0 || h[len(h)-1].Role != llm.RoleAssistant {
		m.emit(renderError("Compaction did not produce a summary; the conversation is unchanged."))
		return
	}
	summary := h[len(h)-1].Text()
	if strings.TrimSpace(summary) == "" {
		m.emit(renderError("Compaction produced an empty summary; the conversation is unchanged."))
		return
	}
	u, cost, _ := m.ag.Stats()
	m.saveSession()
	m.sess = session.New(m.opts.Root, m.ag.Settings().Model)
	m.ag.SetHistory([]llm.Message{
		llm.UserText("This conversation continues an earlier one. Summary of the earlier conversation:\n\n" + summary),
		{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "Understood. I have the context and will continue from here."}}},
	}, u, cost)
	m.emit(renderInfo(fmt.Sprintf("Conversation compacted (%d messages → 2).", len(h))))
}

func cmdSessions(m *model, _ string) tea.Cmd {
	if m.opts.Store == nil {
		return nil
	}
	list, err := m.opts.Store.List(m.opts.Root)
	if err != nil {
		m.emit(renderError(err.Error()))
		return nil
	}
	if len(list) == 0 {
		m.emit(renderInfo("No saved sessions for this workspace."))
		return nil
	}
	var b strings.Builder
	b.WriteString(renderInfo("Saved sessions (newest first)"))
	for i, s := range list {
		if i == 15 {
			b.WriteString("\n" + sFaint.Render(fmt.Sprintf("  … %d more", len(list)-15)))
			break
		}
		b.WriteString(fmt.Sprintf("\n  %s  %s  %s", sAccent2.Render(s.ID), sFaint.Render(padRight(humanAge(s.Updated), 8)), sText.Render(clip(s.Title, m.width-50))))
	}
	b.WriteString("\n" + sFaint.Render("  /resume <id prefix> to continue one"))
	m.emit(b.String())
	return nil
}

func cmdResume(m *model, arg string) tea.Cmd {
	if m.phase != phaseIdle {
		m.emit(renderNotice("Wait for the current turn to finish."))
		return nil
	}
	if arg == "" {
		return cmdSessions(m, "")
	}
	sess, err := m.opts.Store.Load(arg)
	if err != nil {
		m.emit(renderError(err.Error()))
		return nil
	}
	m.saveSession()
	m.sess = sess
	m.ag.SetHistory(sess.Messages, sess.Usage, sess.CostUSD)
	m.emit(renderInfo(fmt.Sprintf("Resumed %q (%d messages).", sess.Title, len(sess.Messages))))
	m.replayTail(sess.Messages)
	return nil
}

// replayTail prints the last exchange of a resumed conversation.
func (m *model) replayTail(msgs []llm.Message) {
	lastUser, lastAsst := -1, -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == llm.RoleAssistant && lastAsst < 0 && strings.TrimSpace(msgs[i].Text()) != "" {
			lastAsst = i
		}
		if msgs[i].Role == llm.RoleUser && strings.TrimSpace(msgs[i].Text()) != "" {
			lastUser = i
			break
		}
	}
	if lastUser >= 0 {
		m.emit(renderUser(clip(strings.TrimSpace(msgs[lastUser].Text()), 600), m.width))
	}
	if lastAsst > lastUser {
		m.printMarkdown(msgs[lastAsst].Text())
	}
}

func cmdInit(m *model, _ string) tea.Cmd {
	if m.phase != phaseIdle {
		m.emit(renderNotice("Wait for the current turn to finish."))
		return nil
	}
	return m.startRun(prompt.InitPrompt, "")
}

func cmdMemory(m *model, _ string) tea.Cmd {
	files := prompt.LoadedMemoryFiles(m.opts.Root)
	user := filepath.Join(config.UserDir(), "AGENTS.md")
	var b strings.Builder
	b.WriteString(renderInfo("Project memory is read at startup from " + strings.Join(prompt.MemoryFiles, ", ") + " in the workspace root, plus " + prettyPath(user) + "."))
	if len(files) == 0 {
		b.WriteString("\n  " + sMuted.Render("None found. Run /init to create AGENTS.md."))
	}
	for _, f := range files {
		b.WriteString("\n  " + sGreen.Render(gCheck) + " " + sText.Render(f))
	}
	if _, err := os.Stat(user); err == nil {
		b.WriteString("\n  " + sGreen.Render(gCheck) + " " + sText.Render(prettyPath(user)))
	}
	m.emit(b.String())
	return nil
}

func cmdTools(m *model, _ string) tea.Cmd {
	var b strings.Builder
	b.WriteString(renderInfo("Tools"))
	for _, spec := range m.ag.Registry().Specs() {
		desc := spec.Description
		if i := strings.Index(desc, ". "); i > 0 {
			desc = desc[:i+1]
		}
		b.WriteString("\n  " + sAccent2.Render(padRight(spec.Name, 22)) + sMuted.Render(clip(desc, m.width-28)))
	}
	m.emit(b.String())
	return nil
}

func cmdConfig(m *model, _ string) tea.Cmd {
	s := m.ag.Settings()
	c := m.opts.Config
	rows := [][2]string{
		{"model", s.Model}, {"effort", orDefault(s.Effort, "default")}, {"max_tokens", fmt.Sprint(s.MaxTokens)},
		{"permission_mode", string(m.ag.Gate.Mode())}, {"show_thinking", fmt.Sprint(s.ShowThinking)},
		{"web_search", fmt.Sprint(c.WebSearch)}, {"web_fetch", fmt.Sprint(c.WebFetch)},
		{"compaction", fmt.Sprint(c.Compaction)}, {"fallbacks", fmt.Sprint(c.Fallbacks)},
		{"max_turns", fmt.Sprint(s.MaxTurns)}, {"user config", prettyPath(config.UserFile())},
		{"project config", prettyPath(config.ProjectFile(m.opts.Root))},
	}
	var b strings.Builder
	b.WriteString(renderInfo("Configuration"))
	for _, r := range rows {
		b.WriteString("\n  " + sMuted.Render(padRight(r[0], 18)) + sText.Render(r[1]))
	}
	m.emit(b.String())
	return nil
}

// ---------- shell passthrough ----------

func (m *model) runShell(cmd string) tea.Cmd {
	m.emit(sAccent2.Render("!") + " " + sText.Render(cmd))
	env := m.ag.Env
	return func() tea.Msg {
		raw := fmt.Sprintf(`{"command":%q}`, cmd)
		call, err := (tools.Bash{}).Prepare(env, []byte(raw))
		if err != nil {
			return shellDoneMsg{cmd: cmd, err: err}
		}
		out, err := call.Run(context.Background())
		return shellDoneMsg{cmd: cmd, output: out, err: err}
	}
}

func renderShell(cmd, output string, err error, width, tail int) string {
	lines := tailLines(output, tail, width-6, err != nil)
	if output == "" && err != nil {
		lines = []string{sRed.Render(err.Error())}
	}
	return resultBlock(lines)
}

// ---------- @file mentions ----------

var mentionRe = regexp.MustCompile(`(^|\s)@([^\s]+)`)

const maxAttach = 256 << 10

// imageExts are the file types an @-mention attaches as a picture.
var imageExts = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true}

// buildPrompt turns typed text into a user message: @-mentioned text files
// are appended to the prompt and @-mentioned images become image blocks
// (stored by path, loaded when sent). It returns what was attached and any
// mentions that could not be attached.
func buildPrompt(env *tools.Env, text string) (llm.Message, []string, []string) {
	body, attached := expandMentions(env, text)
	msg := llm.UserText(body)
	var problems []string
	seen := map[string]bool{}
	for _, mm := range mentionRe.FindAllStringSubmatch(text, -1) {
		ref := strings.TrimRight(mm[2], ".,;:!?)")
		if !imageExts[strings.ToLower(filepath.Ext(ref))] {
			continue
		}
		abs := env.Resolve(ref)
		if seen[abs] {
			continue
		}
		seen[abs] = true
		if _, err := llm.LoadImage(abs); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", ref, err))
			continue
		}
		msg.Blocks = append(msg.Blocks, llm.Block{Type: llm.BlockImage, Image: &llm.Image{Path: abs}})
		attached = append(attached, env.Rel(abs)+" (image)")
	}
	return msg, attached, problems
}

// expandMentions appends the contents of @-mentioned text files to the
// prompt. Images and binary files are skipped here.
func expandMentions(env *tools.Env, text string) (string, []string) {
	var attached []string
	var parts []string
	seen := map[string]bool{}
	for _, mm := range mentionRe.FindAllStringSubmatch(text, -1) {
		ref := strings.TrimRight(mm[2], ".,;:!?)")
		if imageExts[strings.ToLower(filepath.Ext(ref))] {
			continue
		}
		abs := env.Resolve(ref)
		if seen[abs] {
			continue
		}
		st, err := os.Stat(abs)
		if err != nil || st.IsDir() || st.Size() > maxAttach {
			continue
		}
		data, err := os.ReadFile(abs)
		if err != nil || strings.IndexByte(string(data), 0) >= 0 {
			continue
		}
		seen[abs] = true
		env.MarkRead(abs)
		rel := env.Rel(abs)
		attached = append(attached, rel)
		parts = append(parts, fmt.Sprintf("<file path=%q>\n%s\n</file>", rel, strings.TrimRight(string(data), "\n")))
	}
	if len(parts) == 0 {
		return text, nil
	}
	return text + "\n\nAttached files:\n" + strings.Join(parts, "\n"), attached
}

func humanAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
