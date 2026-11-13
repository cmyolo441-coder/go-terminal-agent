package ui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/prompt"
)

// CapState says whether a capability is active.
type CapState int

const (
	// CapOn is an active, healthy capability.
	CapOn CapState = iota
	// CapOff is available but switched off.
	CapOff
	// CapWarn needs the user's attention (failed server, missing key...).
	CapWarn
)

// Capability is one line of the banner and the /status dashboard, supplied
// by the application so the UI can show integrations it does not know.
type Capability struct {
	// Group is the dashboard section, e.g. "Model", "Safety", "Integrations".
	Group string
	Name  string
	Value string
	State CapState
	// Banner marks capabilities shown in the startup banner.
	Banner bool
}

var capGroups = []string{"Model", "Safety", "Integrations", "Workspace"}

func capDot(s CapState) string {
	switch s {
	case CapOn:
		return sGreen.Render("●")
	case CapWarn:
		return sAmber.Render("▲")
	default:
		return sFaint.Render("○")
	}
}

func (m *model) capabilities() []Capability {
	if m.opts.Capabilities == nil {
		return nil
	}
	return m.opts.Capabilities()
}

// bannerCaps renders the banner capabilities, pairing short cells two to a
// row and giving long ones a row of their own.
func (m *model) bannerCaps(width int) []string {
	var cells []string
	for _, c := range m.capabilities() {
		if c.Banner {
			cells = append(cells, capDot(c.State)+" "+sMuted.Render(padRight(c.Name, 9))+sText.Render(clip(c.Value, width-12)))
		}
	}
	half := width / 2
	var rows []string
	for i := 0; i < len(cells); i++ {
		c := cells[i]
		if i+1 < len(cells) && lipgloss.Width(c) < half-1 && lipgloss.Width(cells[i+1]) <= half {
			rows = append(rows, c+strings.Repeat(" ", half-lipgloss.Width(c))+cells[i+1])
			i++
			continue
		}
		rows = append(rows, c)
	}
	return rows
}

func cmdStatus(m *model, _ string) tea.Cmd {
	s := m.ag.Settings()
	u, cost, ctxTok := m.ag.Stats()
	w := min(m.width-4, 100)
	var out []string
	section := func(title string) {
		out = append(out, "", sAccentB.Render(title)+" "+sFaint.Render(strings.Repeat("─", max(0, w-runewidth.StringWidth(title)-1))))
	}
	row := func(state CapState, name, value string) {
		out = append(out, "  "+capDot(state)+" "+sMuted.Render(padRight(name, 14))+sText.Render(clip(value, w-20)))
	}
	out = append(out, sAccentB.Render("◆ GTA status")+sFaint.Render("  ·  "+m.opts.Version))

	section("Session")
	row(CapOn, "id", m.sess.ID)
	row(CapOn, "messages", fmt.Sprintf("%d in history · %d model calls this run", len(m.ag.History()), m.ag.Calls()))
	row(CapOn, "uptime", fmtDuration(time.Since(m.ag.Started())))
	row(CapOn, "usage", fmt.Sprintf("↑ %s  ↓ %s  cached %s  · %s", fmtTokens(u.InputTokens+u.CacheWriteTokens), fmtTokens(u.OutputTokens), fmtTokens(u.CacheReadTokens), fmtCost(cost)))
	row(CapOn, "context", fmtTokens(ctxTok)+" tokens in the current window")

	caps := m.capabilities()
	byGroup := map[string][]Capability{}
	var extraGroups []string
	for _, c := range caps {
		if _, ok := byGroup[c.Group]; !ok && !contains(capGroups, c.Group) {
			extraGroups = append(extraGroups, c.Group)
		}
		byGroup[c.Group] = append(byGroup[c.Group], c)
	}
	section("Model")
	row(CapOn, "model", s.Model)
	row(CapOn, "effort", orDefault(s.Effort, "default"))
	thinking := CapOff
	if s.ShowThinking {
		thinking = CapOn
	}
	row(thinking, "thinking", onOff(s.ShowThinking))
	for _, g := range append(append([]string{}, capGroups...), extraGroups...) {
		list := byGroup[g]
		if g != "Model" {
			if len(list) == 0 && g != "Safety" && g != "Workspace" {
				continue
			}
			section(g)
		}
		if g == "Safety" {
			row(CapOn, "mode", string(m.ag.Gate.Mode())+" · "+modeHelp(m.ag.Gate.Mode()))
		}
		if g == "Workspace" {
			row(CapOn, "root", prettyPath(m.opts.Root))
			if mem := prompt.LoadedMemoryFiles(m.opts.Root); len(mem) > 0 {
				row(CapOn, "memory", strings.Join(mem, ", "))
			} else {
				row(CapOff, "memory", "none (run /init to create AGENTS.md)")
			}
		}
		for _, c := range list {
			row(c.State, c.Name, c.Value)
		}
	}
	tools := m.ag.Registry().Names()
	section("Tools")
	out = append(out, "  "+sText.Render(wrapList(tools, w-2)))
	m.emit(strings.Join(out, "\n"))
	return nil
}

func cmdCost(m *model, _ string) tea.Cmd {
	u, cost, ctxTok := m.ag.Stats()
	w := min(m.width-4, 100)
	var out []string
	out = append(out, sAccentB.Render("◆ Usage and cost")+sFaint.Render("  ·  this session"))
	out = append(out, "")
	in := u.InputTokens + u.CacheWriteTokens + u.CacheReadTokens
	hit := 0.0
	if in > 0 {
		hit = float64(u.CacheReadTokens) / float64(in)
	}
	stat := func(label, value string) string {
		return sMuted.Render(padRight(label, 13)) + sText.Render(padRight(value, 8))
	}
	out = append(out,
		"  "+stat("input", fmtTokens(u.InputTokens))+"    "+stat("output", fmtTokens(u.OutputTokens)),
		"  "+stat("cache read", fmtTokens(u.CacheReadTokens))+"    "+stat("cache write", fmtTokens(u.CacheWriteTokens)),
		"  "+stat("context now", fmtTokens(ctxTok))+"    "+sMuted.Render(padRight("total cost", 13))+sAccentB.Render(fmtCost(cost)),
		"",
		"  "+sMuted.Render(padRight("cache hits", 13))+meter(hit, 30)+" "+sText.Render(fmt.Sprintf("%.0f%%", hit*100)),
	)
	models := m.ag.UsageByModel()
	if len(models) > 0 {
		out = append(out, "", "  "+sFaint.Render(padRight("model", 28)+padRight("calls", 10)+padRight("in", 9)+padRight("out", 9)+"cost"))
		for _, mu := range models {
			calls := fmt.Sprintf("%d", mu.Calls)
			if mu.SubCalls > 0 {
				calls += fmt.Sprintf("+%d sub", mu.SubCalls)
			}
			c := "n/a"
			if mu.CostKnown {
				c = fmtCost(mu.Cost)
			}
			share := 0.0
			if cost > 0 && mu.CostKnown {
				share = mu.Cost / cost
			}
			out = append(out, "  "+sText.Render(padRight(clip(mu.Model, 26), 28))+sMuted.Render(padRight(calls, 10)+padRight(fmtTokens(mu.Usage.InputTokens+mu.Usage.CacheReadTokens+mu.Usage.CacheWriteTokens), 9)+padRight(fmtTokens(mu.Usage.OutputTokens), 9))+sText.Render(padRight(c, 9))+meter(share, max(6, w-66)))
		}
	}
	if m.opts.Capabilities != nil {
		for _, c := range m.capabilities() {
			if c.Group == "Budget" {
				out = append(out, "", "  "+capDot(c.State)+" "+sMuted.Render(c.Name+" ")+sText.Render(c.Value))
			}
		}
	}
	m.emit(strings.Join(out, "\n"))
	return nil
}

// meter draws a horizontal bar for a 0..1 fraction.
func meter(frac float64, width int) string {
	frac = max(0, min(1, frac))
	full := int(frac*float64(width) + 0.5)
	return sAccent.Render(strings.Repeat("█", full)) + sFaint.Render(strings.Repeat("░", width-full))
}

func wrapList(items []string, width int) string {
	var lines []string
	line := ""
	for _, it := range items {
		add := it
		if line != "" {
			add = " · " + it
		}
		if runewidth.StringWidth(line+add) > width && line != "" {
			lines = append(lines, line)
			line = it
			continue
		}
		line += add
	}
	if line != "" {
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n  ")
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func lipglossWidth(s string) int { return lipgloss.Width(s) }
