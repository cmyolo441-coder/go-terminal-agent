// Package ui is the interactive terminal interface: an inline (not
// fullscreen) Bubble Tea program with a compact double-line prompt box,
// streaming Markdown output, live tool activity and approval dialogs.
package ui

import (
	"sort"

	"context"
	"errors"
	"fmt"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/subagent"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
	"golang.org/x/term"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/agent"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/config"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permission"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/pricing"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/prompt"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/session"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

// Options configure the interactive UI.
type Options struct {
	Agent         *agent.Agent
	Session       *session.Session
	Store         *session.Store
	Config        config.Config
	Root          string
	Version       string
	InitialPrompt string
	// AuthHint is shown in the banner when no credentials were found.
	AuthHint string
	// Capabilities reports integrations (provider, sandbox, MCP, hooks,
	// git, policy...) for the banner and the /status dashboard.
	Capabilities func() []Capability
}

type phase int

const (
	phaseIdle phase = iota
	phaseRunning
	phaseApproving
)

const (
	maxInputLines = 8
	tickInterval  = 90 * time.Millisecond
)

type tickMsg time.Time

type model struct {
	opts Options
	ag   *agent.Agent
	sess *session.Session
	prog *tea.Program
	pr   *printer
	md   *mdRenderer

	width  int
	height int
	input  textarea.Model
	phase  phase

	cancel    context.CancelFunc
	runStart  time.Time
	frame     int
	verb      string
	streamed  int // bytes streamed this turn, for the live token estimate
	textBuf   strings.Builder
	thinkBuf  strings.Builder
	blockKind string
	running   []runningTool
	subs      map[int]subRow
	files     *fileIndex
	turnUsage llm.Usage
	turnCost  float64
	costKnown bool

	approval    *approveMsg
	approvalSel int

	history    []string
	histIdx    int
	draft      string
	suggest    []command
	suggestAt  int
	queued     []string
	pending    []string // context from ! shell commands, sent with the next prompt
	ctrlCAt    time.Time
	hint       string
	hintUntil  time.Time
	quitting   bool
	compacting bool
}

type agentSettings = agent.Settings

type subRow struct{ desc, event string }

type runningTool struct {
	id   string
	call *tools.Call
}

// Run starts the interactive UI and blocks until the user exits.
func Run(opts Options) error {
	dark := lipgloss.HasDarkBackground()
	style := "dark"
	switch opts.Config.Theme {
	case "light":
		style = "light"
	case "dark":
	default:
		if !dark {
			style = "light"
		}
	}
	width := 100
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		width = w
	}
	m := newModel(opts, style, width)
	prog := tea.NewProgram(m)
	m.prog = prog
	m.pr.start(prog)
	opts.Agent.Env.OnTodos(func(t []tools.Todo) { prog.Send(todosMsg{t}) })
	_, err := prog.Run()
	m.pr.close()
	return err
}

func newModel(opts Options, style string, width int) *model {
	ta := textarea.New()
	ta.Placeholder = "Ask anything · / for commands · @ to attach a file · ! to run a shell command"
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.MaxHeight = 0
	ta.SetHeight(1)
	ta.Prompt = ""
	ta.SetPromptFunc(2, func(i int) string {
		if i == 0 {
			return gPrompt + " "
		}
		return "  "
	})
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.FocusedStyle.Base = lipgloss.NewStyle()
	ta.FocusedStyle.Prompt = sAccentB
	ta.FocusedStyle.Text = sText
	ta.FocusedStyle.Placeholder = sFaint
	ta.FocusedStyle.EndOfBuffer = lipgloss.NewStyle()
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "ctrl+j"))
	ta.Focus()
	m := &model{
		opts:  opts,
		ag:    opts.Agent,
		sess:  opts.Session,
		pr:    newPrinter(),
		md:    newMD(style),
		width: width,
		input: ta,
	}
	m.layoutInput()
	return m
}

func (m *model) Init() tea.Cmd {
	m.emit(m.banner())
	if p := strings.TrimSpace(m.opts.InitialPrompt); p != "" {
		return m.submit(p)
	}
	return textarea.Blink
}

// emit prints a block above the live view, separated by a blank line.
func (m *model) emit(s string) {
	if s == "" {
		return
	}
	m.pr.print("\n" + s)
}

func (m *model) flash(s string) {
	m.hint = s
	m.hintUntil = time.Now().Add(4 * time.Second)
}

// ---------- update ----------

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layoutInput()
		return m, nil
	case tickMsg:
		if m.phase == phaseIdle {
			return m, nil
		}
		m.frame++
		return m, tick()
	case tea.KeyMsg:
		return m.handleKey(msg)
	case streamMsg:
		m.onStream(msg.ev)
	case toolStartMsg:
		m.flushStreams(true)
		m.running = append(m.running, runningTool{msg.id, msg.call})
		m.verb = "Running"
	case toolDoneMsg:
		m.removeRunning(msg.id)
		m.emit(renderToolDone(msg.call, msg.output, msg.err, m.width))
	case toolFailedMsg:
		m.flushStreams(true)
		m.emit(renderToolFailed(msg.name, msg.reason, m.width))
	case subagentMsg:
		if subagent.Terminal(msg.event) {
			delete(m.subs, msg.index)
			m.emit(sFaint.Render("  "+gResult+"  ") + sMuted.Render(fmt.Sprintf("sub-agent %d · %s · ", msg.index+1, clip(msg.desc, 50))) + subEventStyle(msg.event).Render(msg.event))
		} else {
			if m.subs == nil {
				m.subs = map[int]subRow{}
			}
			m.subs[msg.index] = subRow{msg.desc, msg.event}
		}
	case usageMsg:
		m.turnUsage.Add(msg.usage)
		if c, ok := pricing.Cost(msg.model, msg.usage); ok {
			m.turnCost += c
			m.costKnown = true
		}
	case noticeMsg:
		m.flushStreams(true)
		m.emit(renderNotice(msg.text))
	case todosMsg:
		m.flushStreams(true)
		m.emit(renderTodos(msg.todos))
	case approveMsg:
		m.flushStreams(true)
		m.approval = &msg
		m.approvalSel = 0
		m.phase = phaseApproving
	case runDoneMsg:
		return m, m.finishRun(msg.err)
	case extraDoneMsg:
		if msg.err != nil {
			m.emit(renderError(msg.err.Error()))
		} else if strings.TrimSpace(msg.out) != "" {
			m.printMarkdown(msg.out)
		}
	case shellDoneMsg:
		m.emit(renderShell(msg.cmd, msg.output, msg.err, m.width))
		m.pending = append(m.pending, fmt.Sprintf("I ran `%s` in the shell. Output:\n```\n%s\n```", msg.cmd, strings.TrimSpace(msg.output)))
	}
	return m, nil
}

func tick() tea.Cmd {
	return tea.Tick(tickInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *model) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.phase == phaseApproving {
		return m, m.handleApprovalKey(k)
	}
	switch k.String() {
	case "ctrl+c":
		if m.phase == phaseRunning {
			m.interrupt()
			return m, nil
		}
		if m.input.Value() != "" {
			m.input.Reset()
			m.suggest = nil
			m.layoutInput()
			return m, nil
		}
		if time.Since(m.ctrlCAt) < 2*time.Second {
			return m, m.quit()
		}
		m.ctrlCAt = time.Now()
		m.flash("Press Ctrl+C again to exit")
		return m, nil
	case "ctrl+d":
		if m.input.Value() == "" {
			return m, m.quit()
		}
	case "esc":
		if len(m.suggest) > 0 {
			m.suggest = nil
			return m, nil
		}
		if m.phase == phaseRunning {
			m.interrupt()
			return m, nil
		}
		return m, nil
	case "shift+tab":
		mode := m.ag.Gate.Cycle()
		m.flash("Permission mode: " + string(mode) + " · " + modeHelp(mode))
		return m, nil
	case "tab":
		if len(m.suggest) > 0 {
			m.completeSuggestion()
			return m, nil
		}
		return m, nil
	case "up":
		if len(m.suggest) > 0 {
			m.suggestAt = (m.suggestAt - 1 + len(m.suggest)) % len(m.suggest)
			return m, nil
		}
		if m.input.Line() == 0 && m.historyPrev() {
			return m, nil
		}
	case "down":
		if len(m.suggest) > 0 {
			m.suggestAt = (m.suggestAt + 1) % len(m.suggest)
			return m, nil
		}
		if m.input.Line() == m.input.LineCount()-1 && m.historyNext() {
			return m, nil
		}
	case "enter":
		if len(m.suggest) > 0 {
			chosen := m.suggest[m.suggestAt]
			if chosen.file {
				m.completeSuggestion()
				return m, nil
			}
			cur := strings.TrimSpace(m.input.Value())
			if cur != "/"+chosen.name && !strings.HasPrefix(cur, "/"+chosen.name+" ") {
				m.completeSuggestion()
				if chosen.args != "" {
					return m, nil
				}
			}
		}
		text := m.input.Value()
		if strings.TrimSpace(text) == "" {
			return m, nil
		}
		m.input.Reset()
		m.suggest = nil
		m.layoutInput()
		return m, m.submit(text)
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	m.layoutInput()
	m.updateSuggestions()
	return m, cmd
}

func (m *model) handleApprovalKey(k tea.KeyMsg) tea.Cmd {
	choose := func(d permission.Decision) tea.Cmd {
		a := m.approval
		m.approval = nil
		m.phase = phaseRunning
		if d == permission.DecisionDeny {
			m.emit(sRed.Render(gCross) + " " + sMuted.Render("Declined: "+a.call.Summary))
		}
		a.reply <- d
		return nil
	}
	switch k.String() {
	case "up", "k", "shift+tab":
		m.approvalSel = (m.approvalSel + 2) % 3
	case "down", "j", "tab":
		m.approvalSel = (m.approvalSel + 1) % 3
	case "1", "y":
		return choose(permission.DecisionAllowOnce)
	case "2", "a":
		return choose(permission.DecisionAllowSession)
	case "3", "n", "esc":
		return choose(permission.DecisionDeny)
	case "ctrl+c":
		choose(permission.DecisionDeny)
		m.interrupt()
	case "enter":
		return choose([]permission.Decision{permission.DecisionAllowOnce, permission.DecisionAllowSession, permission.DecisionDeny}[m.approvalSel])
	}
	return nil
}

func (m *model) interrupt() {
	if m.cancel != nil {
		m.cancel()
		m.verb = "Interrupting"
	}
}

func (m *model) quit() tea.Cmd {
	m.quitting = true
	if m.cancel != nil {
		m.cancel()
	}
	m.saveSession()
	pr := m.pr
	return func() tea.Msg {
		pr.drain()
		return tea.QuitMsg{}
	}
}

// ---------- input helpers ----------

func (m *model) layoutInput() {
	w := m.width - 6
	if w < 10 {
		w = 10
	}
	m.input.SetWidth(w + 2)
	lines := 0
	for _, l := range strings.Split(m.input.Value(), "\n") {
		n := (runewidth.StringWidth(l) + w) / w
		if n < 1 {
			n = 1
		}
		lines += n
	}
	if lines > maxInputLines {
		lines = maxInputLines
	}
	if lines < 1 {
		lines = 1
	}
	m.input.SetHeight(lines)
}

func (m *model) historyPrev() bool {
	if len(m.history) == 0 || m.histIdx == 0 {
		return false
	}
	if m.histIdx == len(m.history) {
		m.draft = m.input.Value()
	}
	m.histIdx--
	m.input.SetValue(m.history[m.histIdx])
	m.layoutInput()
	return true
}

func (m *model) historyNext() bool {
	if m.histIdx >= len(m.history) {
		return false
	}
	m.histIdx++
	if m.histIdx == len(m.history) {
		m.input.SetValue(m.draft)
	} else {
		m.input.SetValue(m.history[m.histIdx])
	}
	m.layoutInput()
	return true
}

func (m *model) updateSuggestions() {
	v := m.input.Value()
	if q, ok := atToken(v); ok {
		if m.files == nil {
			m.files = &fileIndex{root: m.opts.Root}
		}
		m.suggest = m.suggest[:0]
		for _, p := range m.files.match(q, 8) {
			kind := "file"
			if strings.HasSuffix(p, "/") {
				kind = "directory"
			}
			m.suggest = append(m.suggest, command{name: p, help: kind, file: true})
		}
		if len(m.suggest) == 0 {
			m.suggest = nil
		}
		if m.suggestAt >= len(m.suggest) {
			m.suggestAt = 0
		}
		return
	}
	if !strings.HasPrefix(v, "/") || strings.ContainsAny(v, " \n") {
		m.suggest = nil
		return
	}
	m.suggest = matchCommands(strings.TrimPrefix(v, "/"))
	if m.suggestAt >= len(m.suggest) {
		m.suggestAt = 0
	}
}

func (m *model) completeSuggestion() {
	c := m.suggest[m.suggestAt]
	if c.file {
		v := m.input.Value()
		i := strings.LastIndexAny(v, " \n\t")
		v = v[:i+1] + "@" + c.name
		if !strings.HasSuffix(c.name, "/") {
			v += " "
		}
		m.input.SetValue(v)
		m.input.CursorEnd()
		m.suggest = nil
		m.layoutInput()
		if strings.HasSuffix(c.name, "/") {
			m.updateSuggestions()
		}
		return
	}
	v := "/" + c.name
	if c.args != "" {
		v += " "
	}
	m.input.SetValue(v)
	m.input.CursorEnd()
	m.suggest = nil
	m.layoutInput()
}

// ---------- submitting and running ----------

func (m *model) submit(text string) tea.Cmd {
	text = strings.TrimRight(text, "\n ")
	if len(m.history) == 0 || m.history[len(m.history)-1] != text {
		m.history = append(m.history, text)
	}
	m.histIdx = len(m.history)
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "/") {
		return m.runCommand(trimmed)
	}
	if strings.HasPrefix(trimmed, "!") {
		cmd := strings.TrimSpace(trimmed[1:])
		if cmd == "" {
			return nil
		}
		return m.runShell(cmd)
	}
	if m.phase != phaseIdle {
		m.queued = append(m.queued, text)
		m.flash(fmt.Sprintf("Queued: will send after the current turn (%d waiting)", len(m.queued)))
		return nil
	}
	return m.startRun(text, text)
}

// startRun sends a prompt to the agent. shown is what the transcript shows;
// empty shows nothing (the caller already echoed it).
func (m *model) startRun(text, shown string) tea.Cmd {
	if shown != "" {
		m.emit(renderUser(shown, m.width))
	}
	msg, attached, problems := buildPrompt(m.ag.Env, text)
	for _, a := range attached {
		m.emit(sFaint.Render("  "+gResult+"  ") + sMuted.Render("Attached "+a))
	}
	for _, p := range problems {
		m.emit(sFaint.Render("  "+gResult+"  ") + sAmber.Render("Not attached: "+p))
	}
	if len(m.pending) > 0 {
		msg.Blocks[0].Text = strings.Join(m.pending, "\n\n") + "\n\n" + msg.Blocks[0].Text
		m.pending = nil
	}
	ag := m.ag
	return m.launch(func(ctx context.Context, sink agent.Sink) error {
		return ag.Run(ctx, msg, sink)
	})
}

// launch runs fn in the background with the running-turn UI.
func (m *model) launch(fn func(ctx context.Context, sink agent.Sink) error) tea.Cmd {
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.phase = phaseRunning
	m.runStart = time.Now()
	m.verb = "Thinking"
	m.streamed = 0
	m.turnUsage = llm.Usage{}
	m.turnCost = 0
	m.costKnown = false
	prog := m.prog
	go func() {
		err := fn(ctx, teaSink{prog})
		prog.Send(runDoneMsg{err})
	}()
	return tick()
}

func (m *model) finishRun(err error) tea.Cmd {
	m.flushStreams(true)
	m.running = nil
	m.subs = nil
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.phase = phaseIdle
	switch {
	case errors.Is(err, agent.ErrInterrupted):
		m.emit(renderNotice("Interrupted. Tell GTA what to do instead."))
	case err != nil:
		m.emit(renderError(err.Error()))
	default:
		m.emit(renderTurnFooter(time.Since(m.runStart), m.turnUsage, m.turnCost, m.costKnown))
	}
	if m.compacting {
		if err == nil {
			m.finishCompact()
		} else {
			m.compacting = false
		}
	}
	m.saveSession()
	if m.quitting {
		return nil
	}
	if len(m.queued) > 0 {
		next := m.queued[0]
		m.queued = m.queued[1:]
		return m.startRun(next, next)
	}
	return textarea.Blink
}

func (m *model) saveSession() {
	if m.opts.Store == nil || m.sess == nil {
		return
	}
	m.sess.Messages = m.ag.History()
	u, cost, _ := m.ag.Stats()
	m.sess.Usage, m.sess.CostUSD = u, cost
	m.sess.Model = m.ag.Settings().Model
	if err := m.opts.Store.Save(m.sess); err != nil {
		m.emit(renderError("could not save session: " + err.Error()))
	}
}

// ---------- streaming ----------

func (m *model) onStream(ev llm.Event) {
	switch ev.Type {
	case llm.EventTextDelta:
		if m.blockKind == "thinking" {
			m.flushThinking()
		}
		m.blockKind = "text"
		m.verb = "Responding"
		m.streamed += len(ev.Text)
		m.textBuf.WriteString(ev.Text)
		m.flushText(false)
	case llm.EventThinkingDelta:
		m.blockKind = "thinking"
		m.verb = "Thinking"
		m.streamed += len(ev.Text)
		m.thinkBuf.WriteString(ev.Text)
	case llm.EventToolUseStart:
		m.flushStreams(true)
		m.verb = "Preparing " + displayName(ev.ToolName)
	case llm.EventToolInputDelta:
		m.streamed += ev.Bytes
	case llm.EventServerToolStart:
		m.flushStreams(true)
		m.emit(sAccent2.Render(gBullet) + " " + sBold.Render(displayName(ev.ToolName)) + sMuted.Render(" (server)"))
		m.verb = "Searching"
	case llm.EventBlockStop:
		m.flushStreams(true)
	}
}

func (m *model) flushStreams(all bool) {
	m.flushThinking()
	m.flushText(all)
	m.blockKind = ""
}

func (m *model) flushThinking() {
	if m.thinkBuf.Len() == 0 {
		return
	}
	if m.ag.Settings().ShowThinking {
		m.emit(renderThinking(m.thinkBuf.String(), m.width))
	}
	m.thinkBuf.Reset()
}

func (m *model) flushText(all bool) {
	buf := m.textBuf.String()
	if buf == "" {
		return
	}
	n := len(buf)
	if !all {
		n = flushPoint(buf)
	}
	if n == 0 {
		return
	}
	chunk := buf[:n]
	rest := buf[n:]
	m.textBuf.Reset()
	m.textBuf.WriteString(rest)
	if strings.TrimSpace(chunk) == "" {
		return
	}
	m.emit(m.md.render(chunk, m.width-2))
}

func (m *model) removeRunning(id string) {
	for i, r := range m.running {
		if r.id == id {
			m.running = append(m.running[:i], m.running[i+1:]...)
			break
		}
	}
	if len(m.running) == 0 {
		m.verb = "Thinking"
	}
}

// ---------- view ----------

func (m *model) View() string {
	if m.quitting {
		return ""
	}
	var sections []string
	if live := m.liveView(); live != "" {
		sections = append(sections, live)
	}
	if m.phase == phaseApproving && m.approval != nil {
		sections = append(sections, m.approvalView())
	} else {
		sections = append(sections, m.promptBox())
		sections = append(sections, m.statusLine())
		if len(m.suggest) > 0 {
			sections = append(sections, m.suggestView())
		}
	}
	return strings.Join(sections, "\n")
}

func (m *model) liveView() string {
	if m.phase == phaseIdle {
		return ""
	}
	var parts []string
	if tail := strings.TrimRight(m.textBuf.String(), "\n"); tail != "" {
		wrapped := lipgloss.NewStyle().Width(m.width - 4).Render(tail)
		lines := strings.Split(wrapped, "\n")
		if len(lines) > 12 {
			lines = lines[len(lines)-12:]
		}
		for i := range lines {
			lines[i] = "  " + sText.Render(strings.TrimRight(lines[i], " "))
		}
		parts = append(parts, strings.Join(lines, "\n"))
	}
	spin := sAccent.Render(spinnerFrames[m.frame%len(spinnerFrames)])
	for _, r := range m.running {
		line := spin + " " + sBold.Render(displayName(r.call.Tool))
		if arg := toolArg(r.call); arg != "" {
			line += " " + sMuted.Render(clip(arg, m.width-20))
		}
		parts = append(parts, line)
	}
	if len(m.subs) > 0 {
		idx := make([]int, 0, len(m.subs))
		for i := range m.subs {
			idx = append(idx, i)
		}
		sort.Ints(idx)
		for _, i := range idx {
			r := m.subs[i]
			parts = append(parts, "  "+sFaint.Render(gResult+" ")+sAccent2.Render(fmt.Sprintf("#%d ", i+1))+sText.Render(clip(r.desc, m.width/2))+sFaint.Render(" · "+clip(r.event, m.width/3)))
		}
	}
	if m.phase == phaseRunning {
		verb := m.verb
		if verb == "" {
			verb = "Working"
		}
		meta := []string{fmtDuration(time.Since(m.runStart))}
		if m.streamed > 0 {
			meta = append(meta, "↓ "+fmtTokens(int64(m.streamed/4))+" tokens")
		}
		meta = append(meta, "esc to interrupt")
		parts = append(parts, spin+" "+sAccentB.Render(verb+"…")+" "+sFaint.Render("("+strings.Join(meta, " · ")+")"))
	}
	return strings.Join(parts, "\n")
}

func (m *model) promptBox() string {
	border := cAccent
	if m.phase != phaseIdle {
		border = cFaint
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.DoubleBorder()).
		BorderForeground(border).
		Padding(0, 1).
		Width(m.width - 2)
	return box.Render(m.input.View())
}

func (m *model) statusLine() string {
	s := m.ag.Settings()
	mode := m.ag.Gate.Mode()
	left := sAccent.Render(gDiamond) + " " + sMuted.Render(shortModel(s.Model))
	if s.Effort != "" {
		left += sFaint.Render(" · ") + sMuted.Render(s.Effort)
	}
	left += sFaint.Render(" · ") + modeBadge(mode)
	if m.hint != "" && time.Now().Before(m.hintUntil) {
		left += "  " + sAmber.Render(m.hint)
	} else if len(m.queued) > 0 {
		left += "  " + sAmber.Render(fmt.Sprintf("%d queued", len(m.queued)))
	}
	u, cost, ctxTok := m.ag.Stats()
	var right []string
	if ctxTok > 0 {
		right = append(right, fmtTokens(ctxTok)+" ctx")
	}
	if u.OutputTokens > 0 {
		right = append(right, fmtCost(cost))
	}
	if len(right) == 0 {
		right = append(right, "? /help")
	}
	r := sFaint.Render(strings.Join(right, " · "))
	gap := m.width - 2 - lipgloss.Width(left) - lipgloss.Width(r)
	if gap < 1 {
		return " " + left
	}
	return " " + left + strings.Repeat(" ", gap) + r
}

func (m *model) suggestView() string {
	var b strings.Builder
	for i, c := range m.suggest {
		if i >= 8 {
			break
		}
		name := "/" + c.name
		if c.file {
			name = "@" + c.name
		}
		if c.args != "" {
			name += " " + c.args
		}
		line := padRight(name, 26)
		if i == m.suggestAt {
			b.WriteString(" " + sAccentB.Render(gPrompt+" "+line) + sText.Render(c.help))
		} else {
			b.WriteString("   " + sMuted.Render(line) + sFaint.Render(c.help))
		}
		if i < len(m.suggest)-1 && i < 7 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

func (m *model) approvalView() string {
	a := m.approval
	c := a.call
	title := "Allow this action?"
	switch c.Kind {
	case tools.KindWrite:
		title = "Allow this edit?"
	case tools.KindExec:
		title = "Allow this command?"
	}
	if c.Tool == "plan" {
		title = "Run this plan?"
	}
	inner := m.width - 6
	var body []string
	body = append(body, sAmber.Bold(true).Render(title)+"  "+sBold.Render(displayName(c.Tool))+" "+sMuted.Render(clip(toolArg(c), inner-30)))
	if a.reason != "" {
		body = append(body, sAmber.Render("⚠ "+a.reason))
	}
	body = append(body, "")
	switch {
	case c.Tool == "plan":
		lines := strings.Split(strings.TrimRight(m.md.render(c.Preview, inner), "\n"), "\n")
		limit := max(10, m.height-14)
		if len(lines) > limit {
			lines = append(lines[:limit-1], sFaint.Render(fmt.Sprintf("… %d more lines (the full plan is printed above)", len(lines)-limit+1)))
		}
		body = append(body, lines...)
	case c.Tool == "task":
		lines := strings.Split(strings.TrimRight(c.Preview, "\n"), "\n")
		for i, l := range lines {
			if i == 14 {
				body = append(body, sFaint.Render(fmt.Sprintf("… %d more lines", len(lines)-i)))
				break
			}
			body = append(body, sText.Render(clip(l, inner)))
		}
	case c.Kind == tools.KindWrite:
		diff := renderDiff(c.Preview, inner, 18)
		if len(diff) == 0 {
			diff = []string{sMuted.Render("(no textual change)")}
		}
		body = append(body, diff...)
	case c.Kind == tools.KindExec:
		for i, l := range strings.Split(c.Command, "\n") {
			if i == 10 {
				body = append(body, sFaint.Render("…"))
				break
			}
			prefix := "$ "
			if i > 0 {
				prefix = "  "
			}
			body = append(body, sAccent2.Render(prefix)+sText.Render(clip(l, inner-2)))
		}
	}
	body = append(body, "")
	opts := []string{"Yes", "Yes, and allow " + permission.GrantLabel(c), "No, and tell GTA what to do differently"}
	if c.Tool == "plan" {
		opts = []string{"Yes, run it (ask before each edit)", "Yes, run it and auto-approve file edits", "No, and tell GTA what to change"}
	}
	for i, o := range opts {
		label := fmt.Sprintf("%d. %s", i+1, o)
		if i == m.approvalSel {
			body = append(body, sAccentB.Render(gPrompt+" "+label))
		} else {
			body = append(body, "  "+sMuted.Render(label))
		}
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.DoubleBorder()).
		BorderForeground(cAmber).
		Padding(0, 1).
		Width(m.width - 2)
	help := sFaint.Render(" ↑/↓ choose · enter confirm · y/a/n · esc decline")
	return box.Render(strings.Join(body, "\n")) + "\n" + help
}

// ---------- misc ----------

func (m *model) banner() string {
	s := m.ag.Settings()
	logo := sAccentB.Render("◆ GTA") + sFaint.Render(" · ") + sMuted.Render("Go Terminal Agent") + " " + sFaint.Render(m.opts.Version)
	lines := []string{
		logo,
		"",
		sMuted.Render("model     ") + sText.Render(s.Model) + sFaint.Render("  ·  effort ") + sText.Render(orDefault(s.Effort, "default")) + sFaint.Render("  ·  mode ") + modeBadge(m.ag.Gate.Mode()),
		sMuted.Render("workspace ") + sText.Render(clipMiddle(prettyPath(m.opts.Root), m.width-20)),
	}
	if mem := prompt.LoadedMemoryFiles(m.opts.Root); len(mem) > 0 {
		lines = append(lines, sMuted.Render("memory    ")+sText.Render(strings.Join(mem, ", ")))
	}
	if caps := m.bannerCaps(min(m.width-8, 92)); len(caps) > 0 {
		lines = append(lines, "")
		lines = append(lines, caps...)
	}
	if len(m.ag.History()) > 0 {
		lines = append(lines, sMuted.Render("session   ")+sText.Render(fmt.Sprintf("resumed %s (%d messages)", m.sess.ID, len(m.ag.History()))))
	}
	if m.opts.AuthHint != "" {
		lines = append(lines, "", sAmber.Render("! ")+sText.Render(m.opts.AuthHint))
	}
	lines = append(lines, "", sFaint.Render("/help · /status · shift+tab mode · alt+enter newline · esc interrupt · ctrl+c ×2 exit"))
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(cAccent).
		Padding(0, 2).
		Width(min(m.width-2, 96))
	return box.Render(strings.Join(lines, "\n"))
}

func modeBadge(mode permission.Mode) string {
	switch mode {
	case permission.ModeAutoEdit:
		return sGreen.Render("⏵⏵ auto-edit")
	case permission.ModePlan:
		return sBlue.Render("⏸ plan")
	case permission.ModeYolo:
		return sRed.Bold(true).Render("⚡ yolo")
	default:
		return sMuted.Render("ask")
	}
}

func modeHelp(mode permission.Mode) string {
	switch mode {
	case permission.ModeAutoEdit:
		return "edits apply without asking, commands still ask"
	case permission.ModePlan:
		return "read-only, nothing is changed"
	case permission.ModeYolo:
		return "everything runs without asking except destructive commands"
	default:
		return "asks before every edit and command"
	}
}

func shortModel(m string) string { return strings.TrimPrefix(m, "claude-") }

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func subEventStyle(ev string) lipgloss.Style {
	switch {
	case strings.HasPrefix(ev, subagent.EventFailedPrefix), ev == subagent.EventCancelled:
		return sRed
	default:
		return sGreen
	}
}
