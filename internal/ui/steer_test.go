package ui

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/agent"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permission"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

func TestSessionTitle(t *testing.T) {
	for in, want := range map[string]string{
		"fix the failing login test in auth.go please": "Fix the failing login test",
		"@main.go explain this":                        "Explain this",
		"hello":                                        "Hello",
		"   ":                                          "",
		"/help":                                        "",
		"refactor internationalization-configuration-loader thoroughly": "Refactor…",
		"summarise https://example.com for me":                          "Summarise for me",
	} {
		got := sessionTitle(in)
		if want == "Refactor…" {
			if !strings.HasPrefix(got, "Refactor") || len([]rune(got)) > 28 {
				t.Errorf("sessionTitle(%q) = %q", in, got)
			}
			continue
		}
		if got != want {
			t.Errorf("sessionTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

// gateProvider blocks its first call until release is closed, so a test
// can act while a turn is running.
type gateProvider struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	reqs    []llm.Request
}

func (g *gateProvider) Name() string { return "gate" }
func (g *gateProvider) Stream(ctx context.Context, req llm.Request, on func(llm.Event)) (*llm.Response, error) {
	g.mu.Lock()
	g.reqs = append(g.reqs, req)
	n := len(g.reqs)
	g.mu.Unlock()
	if n == 1 {
		g.once.Do(func() { close(g.started) })
		select {
		case <-g.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		raw := []byte(`{}`)
		return &llm.Response{StopReason: llm.StopToolUse, Model: "m", Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Type: llm.BlockToolUse, ToolUseID: "t1", ToolName: "list_dir", Input: raw}}}}, nil
	}
	return &llm.Response{StopReason: llm.StopEndTurn, Model: "m", Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "done"}}}}, nil
}

type nopSink struct{}

func (nopSink) Stream(llm.Event)                            {}
func (nopSink) ToolStart(string, *tools.Call)               {}
func (nopSink) ToolDone(string, *tools.Call, string, error) {}
func (nopSink) ToolFailed(string, string, string)           {}
func (nopSink) Usage(string, llm.Usage)                     {}
func (nopSink) Notice(string)                               {}
func (nopSink) SubagentProgress(int, string, string)        {}
func (nopSink) Approve(context.Context, *tools.Call, string) permission.Decision {
	return permission.DecisionAllowOnce
}

func TestEnterWhileRunningSteers(t *testing.T) {
	g := &gateProvider{started: make(chan struct{}), release: make(chan struct{})}
	env, err := tools.NewEnv(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.New(g, tools.Default(), env, permission.NewGate(permission.ModeYolo, nil), "", agent.Settings{Model: "m"})
	m := newModel(Options{Agent: ag, Root: env.Root}, 100)
	m.phase = phaseRunning // as launch would

	done := make(chan error, 1)
	go func() { done <- ag.Run(context.Background(), llm.UserText("list files"), nopSink{}) }()
	<-g.started

	if m.input.Placeholder != placeholderIdle {
		t.Fatal("placeholder changed before launch")
	}
	m.submit("only look at the docs folder")
	if len(m.queued) != 0 {
		t.Fatalf("steering was queued instead: %v", m.queued)
	}
	close(g.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	last := g.reqs[len(g.reqs)-1]
	found := false
	for _, b := range last.Messages[len(last.Messages)-1].Blocks {
		if strings.Contains(b.Text, "only look at the docs folder") {
			found = true
		}
	}
	if !found {
		t.Fatal("steering did not reach the next model call")
	}
}

func TestEnterWhileBusyWithoutTurnQueues(t *testing.T) {
	m := dockModel(t, nil)
	m.phase = phaseRunning // e.g. a ! shell command, no agent turn
	m.submit("next thing")
	if len(m.queued) != 1 {
		t.Fatalf("queued = %v", m.queued)
	}
}

func TestEscTwiceInterrupts(t *testing.T) {
	m := dockModel(t, nil)
	m.phase = phaseRunning
	cancelled := false
	m.cancel = func() { cancelled = true }
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cancelled || !strings.Contains(m.hint, "esc again") {
		t.Fatalf("first esc: cancelled=%v hint=%q", cancelled, m.hint)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if !cancelled {
		t.Fatal("second esc did not interrupt")
	}
	// A late second esc only arms again.
	cancelled = false
	m.escAt = time.Now().Add(-2 * escTwiceWindow)
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cancelled {
		t.Fatal("esc after the window interrupted")
	}
}

func TestCtrlGQueuesAndCtrlOCycles(t *testing.T) {
	m := dockModel(t, nil)
	m.phase = phaseRunning
	m.input.SetValue("after this, run the tests")
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	if len(m.queued) != 1 || m.input.Value() != "" {
		t.Fatalf("ctrl+g: queued=%v input=%q", m.queued, m.input.Value())
	}
	start := m.density.name
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	if m.density.name == start {
		t.Fatal("ctrl+o did not change density")
	}
}

func TestPromptAreaShowsTitleChipAndCwd(t *testing.T) {
	m := dockModel(t, nil)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.title = "Fix login test"
	view := stripANSI(m.View())
	if !strings.Contains(view, " Fix login test ") || !strings.Contains(view, "●") {
		t.Fatalf("view lacks title chip or cwd:\n%s", view)
	}
	if strings.Contains(view, "╔") {
		t.Fatal("old double-line box still drawn")
	}
	if !strings.Contains(view, "ctrl+o details") {
		t.Fatalf("footer lacks hints:\n%s", view)
	}
}

func TestPromptLineHasNoBackground(t *testing.T) {
	m := dockModel(t, nil)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	for _, l := range strings.Split(m.input.View(), "\n") {
		for _, bg := range []string{"\x1b[40m", "\x1b[48;5;0m", "\x1b[48;5;255m", "\x1b[107m"} {
			if strings.Contains(l, bg) {
				t.Fatalf("prompt line paints a background %q: %q", bg, l)
			}
		}
	}
}
