// Package hooks runs user-configured shell commands at lifecycle events.
package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/config"
)

// Event names.
const (
	PreToolUse       = "PreToolUse"
	PostToolUse      = "PostToolUse"
	UserPromptSubmit = "UserPromptSubmit"
	Stop             = "Stop"
	SessionStart     = "SessionStart"
)

// Runner executes hooks from settings.
type Runner struct {
	Hooks     map[string][]config.HookMatcher
	Cwd       string
	SessionID string
}

// Outcome is the combined result of the hooks for one event.
type Outcome struct {
	Blocked bool   // a hook exited with code 2
	Reason  string // stderr of the blocking hook
	Output  string // stdout of successful hooks, added as context
}

// Run fires every hook registered for event whose matcher matches toolName.
// The payload is sent to each hook as JSON on stdin. Exit code 2 blocks.
func (r *Runner) Run(ctx context.Context, event, toolName string, payload map[string]any) Outcome {
	var out Outcome
	if r == nil || len(r.Hooks[event]) == 0 {
		return out
	}
	if payload == nil {
		payload = map[string]any{}
	}
	payload["hook_event_name"] = event
	payload["session_id"] = r.SessionID
	payload["cwd"] = r.Cwd
	if toolName != "" {
		payload["tool_name"] = toolName
	}
	stdin, _ := json.Marshal(payload)
	var outputs []string
	for _, m := range r.Hooks[event] {
		if !matches(m.Matcher, toolName) {
			continue
		}
		for _, h := range m.Hooks {
			if h.Command == "" || (h.Type != "" && h.Type != "command") {
				continue
			}
			timeout := 60 * time.Second
			if h.Timeout > 0 {
				timeout = time.Duration(h.Timeout) * time.Second
			}
			hctx, cancel := context.WithTimeout(ctx, timeout)
			cmd := exec.CommandContext(hctx, "sh", "-c", h.Command)
			cmd.Dir = r.Cwd
			cmd.Env = append(os.Environ(), "GOCODE_PROJECT_DIR="+r.Cwd)
			cmd.Stdin = bytes.NewReader(stdin)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			cancel()
			if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 2 {
				out.Blocked = true
				out.Reason = strings.TrimSpace(stderr.String())
				if out.Reason == "" {
					out.Reason = "blocked by " + event + " hook"
				}
				return out
			}
			if err == nil {
				if s := strings.TrimSpace(stdout.String()); s != "" {
					outputs = append(outputs, s)
				}
			}
		}
	}
	out.Output = strings.Join(outputs, "\n")
	return out
}

func matches(matcher, tool string) bool {
	if matcher == "" || matcher == "*" || tool == "" {
		return true
	}
	re, err := regexp.Compile("^(?:" + matcher + ")$")
	if err != nil {
		return matcher == tool
	}
	return re.MatchString(tool)
}
