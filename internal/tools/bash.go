package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultBashTimeout = 2 * time.Minute
	maxBashTimeout     = 10 * time.Minute
	maxBashOutput      = 30000
)

type bashInput struct {
	Command         string `json:"command"`
	Description     string `json:"description"`
	Timeout         int64  `json:"timeout"`
	RunInBackground bool   `json:"run_in_background"`
}

func bashTool() *Tool {
	return &Tool{
		Name: "Bash",
		Description: `Runs a shell command with bash in the project working directory and returns its combined stdout and stderr.

- Every call starts in the working directory; "cd" does not carry over to the next call, so use absolute paths or "cd dir && cmd".
- timeout is in milliseconds (default 120000, max 600000).
- Set run_in_background for long-running processes such as dev servers; read their output with BashOutput and stop them with KillShell.
- Prefer the Read, Edit, Write, Glob and Grep tools over cat, sed, find and grep.
- Quote paths that contain spaces. Never run interactive commands that wait for input.`,
		Properties: map[string]any{
			"command":           prop("string", "The command to run"),
			"description":       prop("string", "What the command does, in 5-10 words"),
			"timeout":           prop("integer", "Timeout in milliseconds"),
			"run_in_background": prop("boolean", "Run detached and return a shell id immediately"),
		},
		Required: []string{"command"},
		Key:      func(in json.RawMessage, _ *Env) string { return str(in, "command") },
		Summary:  func(in json.RawMessage, _ *Env) string { return str(in, "command") },
		Run: func(ctx context.Context, raw json.RawMessage, env *Env) Result {
			var in bashInput
			if err := decode(raw, []string{"command"}, &in); err != nil {
				return Errorf("%v", err)
			}
			if strings.TrimSpace(in.Command) == "" {
				return Errorf("command is empty")
			}
			if in.RunInBackground {
				id, err := env.Shells.Start(in.Command, env.Cwd)
				if err != nil {
					return Errorf("failed to start: %v", err)
				}
				return Result{Text: fmt.Sprintf("Started in background with shell id %s. Use BashOutput to read its output.", id)}
			}
			timeout := defaultBashTimeout
			if in.Timeout > 0 {
				timeout = min(time.Duration(in.Timeout)*time.Millisecond, maxBashTimeout)
			}
			return runBash(ctx, in.Command, env.Cwd, timeout)
		},
	}
}

func runBash(ctx context.Context, command, cwd string, timeout time.Duration) Result {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	name, args := shellCommand(command)
	cmd := exec.Command(name, args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "GIT_PAGER=cat", "PAGER=cat", "GIT_TERMINAL_PROMPT=0")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	setProcGroup(cmd)
	if err := cmd.Start(); err != nil {
		return Errorf("failed to start: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		killProcGroup(cmd)
		<-done
		text := truncateMiddle(out.String(), maxBashOutput)
		if ctx.Err() == context.DeadlineExceeded {
			return Result{Text: text + fmt.Sprintf("\n[command timed out after %s and was killed]", timeout), IsError: true}
		}
		return Result{Text: text + "\n[command interrupted by the user]", IsError: true}
	}
	text := truncateMiddle(strings.TrimRight(out.String(), "\n"), maxBashOutput)
	if err != nil {
		code := -1
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		if text != "" {
			text += "\n"
		}
		return Result{Text: fmt.Sprintf("%s[exit code %d]", text, code), IsError: true}
	}
	if text == "" {
		text = "(no output)"
	}
	return Result{Text: text}
}

// ShellManager tracks background shells.
type ShellManager struct {
	mu     sync.Mutex
	shells map[string]*bgShell
	next   int
}

type bgShell struct {
	id      string
	command string
	cmd     *exec.Cmd
	mu      sync.Mutex
	out     bytes.Buffer
	readPos int
	done    bool
	exit    int
}

type lockedWriter struct{ s *bgShell }

func (w lockedWriter) Write(p []byte) (int, error) {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	return w.s.out.Write(p)
}

func NewShellManager() *ShellManager { return &ShellManager{shells: map[string]*bgShell{}} }

// Start launches a detached command and returns its id.
func (m *ShellManager) Start(command, cwd string) (string, error) {
	name, args := shellCommand(command)
	cmd := exec.Command(name, args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "GIT_PAGER=cat", "PAGER=cat")
	setProcGroup(cmd)
	m.mu.Lock()
	m.next++
	s := &bgShell{id: fmt.Sprintf("bash_%d", m.next), command: command, cmd: cmd}
	m.mu.Unlock()
	cmd.Stdout, cmd.Stderr = lockedWriter{s}, lockedWriter{s}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	m.mu.Lock()
	m.shells[s.id] = s
	m.mu.Unlock()
	go func() {
		err := cmd.Wait()
		s.mu.Lock()
		s.done = true
		if ee, ok := err.(*exec.ExitError); ok {
			s.exit = ee.ExitCode()
		} else if err != nil {
			s.exit = -1
		}
		s.mu.Unlock()
	}()
	return s.id, nil
}

// Output returns output produced since the last call, plus a status line.
func (m *ShellManager) Output(id string) (string, error) {
	m.mu.Lock()
	s, ok := m.shells[id]
	m.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("no background shell with id %q", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	all := s.out.String()
	fresh := all[s.readPos:]
	s.readPos = len(all)
	status := "running"
	if s.done {
		status = fmt.Sprintf("exited with code %d", s.exit)
	}
	if fresh == "" {
		fresh = "(no new output)"
	}
	return fmt.Sprintf("%s\n[status: %s]", truncateMiddle(strings.TrimRight(fresh, "\n"), maxBashOutput), status), nil
}

// Kill stops a background shell.
func (m *ShellManager) Kill(id string) error {
	m.mu.Lock()
	s, ok := m.shells[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("no background shell with id %q", id)
	}
	killProcGroup(s.cmd)
	return nil
}

// KillAll stops every running background shell.
func (m *ShellManager) KillAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.shells {
		s.mu.Lock()
		done := s.done
		s.mu.Unlock()
		if !done {
			killProcGroup(s.cmd)
		}
	}
}

// List describes every background shell.
func (m *ShellManager) List() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, s := range m.shells {
		s.mu.Lock()
		st := "running"
		if s.done {
			st = fmt.Sprintf("exited %d", s.exit)
		}
		s.mu.Unlock()
		out = append(out, fmt.Sprintf("%s  [%s]  %s", s.id, st, s.command))
	}
	sort.Strings(out)
	return out
}

func bashOutputTool() *Tool {
	return &Tool{
		Name:        "BashOutput",
		Description: "Returns new output from a background shell started with Bash run_in_background, and whether it is still running.",
		Properties:  map[string]any{"shell_id": prop("string", "The id returned when the shell was started")},
		Required:    []string{"shell_id"},
		ReadOnly:    true,
		Summary:     func(in json.RawMessage, _ *Env) string { return str(in, "shell_id") },
		Run: func(_ context.Context, raw json.RawMessage, env *Env) Result {
			out, err := env.Shells.Output(str(raw, "shell_id"))
			if err != nil {
				return Errorf("%v", err)
			}
			return Result{Text: out}
		},
	}
}

func killShellTool() *Tool {
	return &Tool{
		Name:        "KillShell",
		Description: "Stops a background shell by id.",
		Properties:  map[string]any{"shell_id": prop("string", "The id of the shell to stop")},
		Required:    []string{"shell_id"},
		ReadOnly:    true, // only affects processes this session started
		Summary:     func(in json.RawMessage, _ *Env) string { return str(in, "shell_id") },
		Run: func(_ context.Context, raw json.RawMessage, env *Env) Result {
			id := str(raw, "shell_id")
			if err := env.Shells.Kill(id); err != nil {
				return Errorf("%v", err)
			}
			return Result{Text: "Stopped " + id}
		},
	}
}
