// Package permission decides whether a tool call may run, needs the user's
// approval, or is refused, based on the session's permission mode, the
// project's allow-list and approvals granted earlier in the session.
package permission

import (
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

// Mode controls how much the agent may do without asking.
type Mode string

const (
	// ModeAsk runs reads freely and asks before every write or command.
	ModeAsk Mode = "ask"
	// ModeAutoEdit also applies file edits inside the workspace without asking.
	ModeAutoEdit Mode = "auto-edit"
	// ModePlan is read-only: writes and commands are refused.
	ModePlan Mode = "plan"
	// ModeYolo runs everything except commands that look destructive.
	ModeYolo Mode = "yolo"
)

// Modes lists the valid modes in cycle order.
var Modes = []Mode{ModeAsk, ModeAutoEdit, ModePlan, ModeYolo}

// ParseMode validates a mode name.
func ParseMode(s string) (Mode, error) {
	for _, m := range Modes {
		if string(m) == s {
			return m, nil
		}
	}
	return "", fmt.Errorf("unknown permission mode %q (want ask, auto-edit, plan or yolo)", s)
}

// Next returns the following mode in the cycle.
func (m Mode) Next() Mode {
	for i, x := range Modes {
		if x == m {
			return Modes[(i+1)%len(Modes)]
		}
	}
	return ModeAsk
}

// Verdict is the outcome of a check.
type Verdict int

const (
	Allow Verdict = iota
	Ask
	Deny
)

// Decision is the user's answer to an approval prompt.
type Decision int

const (
	DecisionDeny Decision = iota
	DecisionAllowOnce
	DecisionAllowSession
)

// Gate holds the permission state for one session.
type Gate struct {
	mu        sync.Mutex
	mode      Mode
	allowCmds []string
	session   map[string]bool
	clamp     func(Mode) (Mode, string)
}

// SetClamp installs a limit on modes, such as a managed policy's maximum.
// clamp returns the mode to use and, when it lowered the mode, why.
func (g *Gate) SetClamp(clamp func(Mode) (Mode, string)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.clamp = clamp
	if clamp != nil {
		g.mode, _ = clamp(g.mode)
	}
}

// Switch changes the mode within the clamp. It returns the mode now in
// effect and, when the request was lowered, the reason.
func (g *Gate) Switch(m Mode) (Mode, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	why := ""
	if g.clamp != nil {
		m, why = g.clamp(m)
	}
	g.mode = m
	return m, why
}

// Cycle moves to the next mode the clamp allows.
func (g *Gate) Cycle() Mode {
	g.mu.Lock()
	defer g.mu.Unlock()
	m := g.mode
	for i := 0; i < 4; i++ {
		m = m.Next()
		if g.clamp == nil {
			break
		}
		if c, why := g.clamp(m); why == "" && c == m {
			break
		}
	}
	if g.clamp != nil {
		m, _ = g.clamp(m)
	}
	g.mode = m
	return m
}

// NewGate builds a gate. allowCommands are command prefixes (for example
// "go test" or "git status") that run without asking in ask mode.
func NewGate(mode Mode, allowCommands []string) *Gate {
	return &Gate{mode: mode, allowCmds: allowCommands, session: map[string]bool{}}
}

// Mode returns the current mode.
func (g *Gate) Mode() Mode {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.mode
}

// SetMode changes the mode.
func (g *Gate) SetMode(m Mode) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mode = m
}

// destructive matches commands that always need an explicit yes.
var destructive = regexp.MustCompile(`(?i)(\brm\s+(-[a-z]*r[a-z]*f|-[a-z]*f[a-z]*r)[a-z]*\s+(/|~|\$HOME|\*)(\s|$))|\bmkfs\b|\bdd\s+.*of=/dev/|:\(\)\s*\{|\bshutdown\b|\breboot\b|\bgit\s+push\s+.*--force\b|\bgit\s+push\s+-f\b|\bgit\s+reset\s+--hard\b|\bgit\s+clean\s+-[a-z]*f|>\s*/dev/sd|\bchmod\s+-R\s+777\s+/`)

// IsDestructive reports whether a command matches the destructive patterns.
func IsDestructive(cmd string) bool { return destructive.MatchString(cmd) }

// Check decides what to do with a prepared call.
func (g *Gate) Check(c *tools.Call) (Verdict, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch c.Kind {
	case tools.KindRead:
		return Allow, ""
	case tools.KindWrite:
		switch g.mode {
		case ModePlan:
			return Deny, "plan mode is read-only; describe the change instead of making it"
		case ModeYolo:
			return Allow, ""
		}
		if c.OutsideRoot {
			return Ask, "writes outside the workspace"
		}
		if g.mode == ModeAutoEdit || g.session["write"] {
			return Allow, ""
		}
		return Ask, ""
	case tools.KindExec:
		if g.mode == ModePlan {
			return Deny, "plan mode is read-only; commands are not run"
		}
		if IsDestructive(c.Command) {
			return Ask, "looks destructive"
		}
		if g.mode == ModeYolo {
			return Allow, ""
		}
		if g.commandAllowed(c.Command) {
			return Allow, ""
		}
		return Ask, ""
	}
	return Ask, ""
}

// Grant records an "allow for this session" answer for a call.
func (g *Gate) Grant(c *tools.Call) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch c.Kind {
	case tools.KindWrite:
		g.session["write"] = true
	case tools.KindExec:
		if p := CommandPrefix(c.Command); p != "" {
			g.session["cmd:"+p] = true
		}
	}
}

// GrantLabel says what Grant would allow, for the approval prompt.
func GrantLabel(c *tools.Call) string {
	switch c.Kind {
	case tools.KindWrite:
		return "all file edits this session"
	case tools.KindExec:
		if p := CommandPrefix(c.Command); p != "" {
			return fmt.Sprintf("`%s` commands this session", p)
		}
	}
	return "this session"
}

func (g *Gate) commandAllowed(cmd string) bool {
	if hasShellChaining(cmd) {
		return false
	}
	trimmed := strings.TrimSpace(cmd)
	for _, p := range g.allowCmds {
		if p != "" && (trimmed == p || strings.HasPrefix(trimmed, p+" ")) {
			return true
		}
	}
	if p := CommandPrefix(trimmed); p != "" && g.session["cmd:"+p] {
		return true
	}
	return false
}

// hasShellChaining reports operators that could smuggle a second command
// past a prefix rule.
func hasShellChaining(cmd string) bool {
	return strings.ContainsAny(cmd, ";|&`\n") || strings.Contains(cmd, "$(") || strings.Contains(cmd, ">")
}

// CommandPrefix is the program plus its subcommand, e.g. "go test" or
// "npm run". Commands with chaining get no prefix and always ask.
func CommandPrefix(cmd string) string {
	if hasShellChaining(cmd) {
		return ""
	}
	f := strings.Fields(cmd)
	if len(f) == 0 {
		return ""
	}
	if len(f) > 1 && !strings.HasPrefix(f[1], "-") && !strings.ContainsAny(f[1], "/.=") {
		return f[0] + " " + f[1]
	}
	return f[0]
}
