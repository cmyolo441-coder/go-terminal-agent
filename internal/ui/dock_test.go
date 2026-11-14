package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/agent"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permission"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

func dockModel(t *testing.T, d Dock) *model {
	t.Helper()
	env, err := tools.NewEnv(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.New(nil, tools.NewRegistry(), env, permission.NewGate(permission.ModeAsk, nil), "", agent.Settings{Model: "test-model"})
	return newModel(Options{Dock: d, Agent: ag, Root: env.Root}, 80)
}

type fakeDock struct {
	visible bool
	w, h    int
	msgs    int
}

func (d *fakeDock) Init() tea.Cmd          { return nil }
func (d *fakeDock) Update(tea.Msg) tea.Cmd { d.msgs++; return nil }
func (d *fakeDock) Visible() bool          { return d.visible }
func (d *fakeDock) SetVisible(v bool)      { d.visible = v }
func (d *fakeDock) View(w, h int) string {
	d.w, d.h = w, h
	rows := make([]string, h)
	for i := range rows {
		rows[i] = "D" + strings.Repeat("d", w-1)
	}
	return strings.Join(rows, "\n")
}

func TestDockSideBySide(t *testing.T) {
	d := &fakeDock{visible: true}
	m := dockModel(t, d)
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	if d.msgs == 0 {
		t.Fatal("dock did not receive messages")
	}
	if m.width >= 140 || m.width+1+m.dockWidth() != 140 {
		t.Fatalf("live column %d + dock %d != 140", m.width, m.dockWidth())
	}
	view := m.View()
	lines := strings.Split(view, "\n")
	if len(lines) != d.h {
		t.Fatalf("view has %d lines, dock asked for %d", len(lines), d.h)
	}
	for i, l := range lines {
		if w := lipgloss.Width(l); w != 140 {
			t.Fatalf("line %d is %d wide, want 140: %q", i, w, l)
		}
		if !strings.Contains(l, "D") {
			t.Fatalf("line %d lacks the dock: %q", i, l)
		}
	}
	// The prompt sits on the last lines, left of the dock.
	if !strings.Contains(lines[len(lines)-1], "D") {
		t.Fatal("dock not bottom-aligned with the prompt")
	}

	// Ctrl+B hides it and the live column takes the full width again.
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlB})
	if d.visible || m.width != 140 || strings.Contains(m.View(), "Ddd") {
		t.Fatalf("ctrl+b did not hide the dock (visible=%v width=%d)", d.visible, m.width)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlB})
	if !d.visible || m.width == 140 {
		t.Fatal("ctrl+b did not show the dock again")
	}
}

func TestDockHiddenWhenNarrow(t *testing.T) {
	d := &fakeDock{visible: true}
	m := dockModel(t, d)
	m.Update(tea.WindowSizeMsg{Width: dockMinWidth - 1, Height: 30})
	if m.dockShown() || m.width != dockMinWidth-1 || strings.Contains(m.View(), "Ddd") {
		t.Fatal("dock shown on a narrow terminal")
	}
}

func TestNoDockCtrlBHints(t *testing.T) {
	m := newModel(Options{}, 100)
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlB})
	if !strings.Contains(m.hint, "--browser") {
		t.Fatalf("hint = %q", m.hint)
	}
}
