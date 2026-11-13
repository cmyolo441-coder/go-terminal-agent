package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

// Bash runs a shell command in the workspace.
type Bash struct{}

type bashInput struct {
	Command     string `json:"command"`
	TimeoutSec  int    `json:"timeout_sec"`
	Description string `json:"description"`
}

const maxShellTimeout = 10 * time.Minute

func (Bash) Kind() Kind { return KindExec }

func (Bash) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "bash",
		Description: "Run a shell command in the workspace root and return its combined stdout and stderr with the exit code. " +
			"Each call runs in a fresh shell (cd does not persist; chain with &&). Default timeout 120s, max 600s. " +
			"Do not start interactive programs or long-running servers. Use grep, glob and read_file for searching and reading files.",
		Properties: map[string]any{
			"command":     map[string]any{"type": "string", "description": "The command to run."},
			"timeout_sec": map[string]any{"type": "integer", "description": "Timeout in seconds (max 600)."},
			"description": map[string]any{"type": "string", "description": "Five to ten words on what the command does."},
		},
		Required: []string{"command"},
	}
}

func (Bash) Prepare(env *Env, raw json.RawMessage) (*Call, error) {
	var in bashInput
	if err := decode(raw, &in); err != nil {
		return nil, err
	}
	if err := required("command", in.Command); err != nil {
		return nil, err
	}
	timeout := env.ShellTimeout
	if in.TimeoutSec > 0 {
		timeout = time.Duration(in.TimeoutSec) * time.Second
	}
	if timeout > maxShellTimeout {
		timeout = maxShellTimeout
	}
	summary := "Run " + firstLine(in.Command)
	if d := strings.TrimSpace(in.Description); d != "" {
		summary = d
	}
	cmdText := in.Command
	return &Call{
		Tool:    "bash",
		Kind:    KindExec,
		Summary: summary,
		Preview: cmdText,
		Command: cmdText,
		run: func(ctx context.Context) (string, error) {
			return runShell(ctx, env, cmdText, timeout)
		},
	}, nil
}

func runShell(ctx context.Context, env *Env, command string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var cmd *exec.Cmd
	if env.Wrap != nil {
		sh := ""
		if runtime.GOOS != "windows" {
			sh = "bash"
			if _, err := exec.LookPath("bash"); err != nil {
				sh = "sh"
			}
		}
		argv, err := env.Wrap(sh, command)
		if err != nil {
			return "", fmt.Errorf("sandbox: %w", err)
		}
		cmd = exec.CommandContext(ctx, argv[0], argv[1:]...)
	} else if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/C", command)
	} else {
		sh := "bash"
		if _, err := exec.LookPath("bash"); err != nil {
			sh = "sh"
		}
		cmd = exec.CommandContext(ctx, sh, "-c", command)
	}
	cmd.Dir = env.Root
	cmd.Env = append(os.Environ(), "GTA=1", "TERM=dumb", "NO_COLOR=1", "GIT_PAGER=cat", "PAGER=cat")
	cmd.Stdin = nil
	setProcessGroup(cmd)
	// Keep only what truncate can show: a command such as `yes` must not
	// fill memory before its timeout.
	buf := newHeadTail(env.OutputLimit)
	cmd.Stdout = buf
	cmd.Stderr = buf
	// A background process (`server &`) inherits the output pipe; once the
	// shell itself has exited, stop waiting for it after a grace period
	// instead of hanging until the timeout.
	cmd.WaitDelay = shellWaitDelay
	start := time.Now()
	err := cmd.Run()
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil
	}
	elapsed := time.Since(start).Round(10 * time.Millisecond)
	out := truncate(strings.TrimRight(buf.String(), "\n"), env.OutputLimit)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Sprintf("%s\n[timed out after %s]", out, timeout), errors.New("command timed out")
	}
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			return out, err
		}
	}
	if out == "" {
		out = "(no output)"
	}
	result := fmt.Sprintf("%s\n[exit code %d, %s]", out, code, elapsed)
	if code != 0 {
		return result, fmt.Errorf("exit code %d", code)
	}
	return result, nil
}

// shellWaitDelay is how long a finished shell's output pipes may stay open.
const shellWaitDelay = 2 * time.Second

// headTail keeps the first and last limit bytes written to it and counts
// the rest, which is all truncate shows of a command's output. It is safe
// for concurrent writes (stdout and stderr share it).
type headTail struct {
	mu      sync.Mutex
	limit   int
	head    []byte
	tail    []byte // ring of the last limit bytes once head is full
	pos     int
	dropped int64
}

func newHeadTail(limit int) *headTail {
	if limit <= 0 {
		limit = 1 << 20
	}
	return &headTail{limit: limit}
}

func (h *headTail) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := len(p)
	if room := h.limit - len(h.head); room > 0 {
		k := min(room, len(p))
		h.head = append(h.head, p[:k]...)
		p = p[k:]
	}
	for len(p) > 0 {
		if len(h.tail) < h.limit {
			k := min(h.limit-len(h.tail), len(p))
			h.tail = append(h.tail, p[:k]...)
			p = p[k:]
			continue
		}
		k := min(h.limit-h.pos, len(p))
		copy(h.tail[h.pos:], p[:k])
		h.dropped += int64(k)
		h.pos = (h.pos + k) % h.limit
		p = p[k:]
	}
	return n, nil
}

// String returns the kept output, with a marker where the middle was
// dropped.
func (h *headTail) String() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var b strings.Builder
	b.Write(h.head)
	if h.dropped > 0 {
		fmt.Fprintf(&b, "\n… [%d bytes of output dropped] …\n", h.dropped)
	}
	b.Write(h.tail[h.pos:])
	b.Write(h.tail[:h.pos])
	return b.String()
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if len(s) > 80 {
		cut := 79
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}
