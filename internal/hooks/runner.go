package hooks

import (
	"bytes"
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
)

// maxCapture bounds how much of a hook's stdout and stderr is kept.
const maxCapture = 64 << 10

// waitDelay is how long Wait keeps reading a hook's output after the hook
// exited or was killed. It stops a backgrounded grandchild that inherited
// stdout from holding the agent up.
const waitDelay = 500 * time.Millisecond

// Payload is the JSON document written to a hook's stdin. Fields that do not
// apply to the event are omitted.
type Payload struct {
	Event      Event           `json:"event"`
	SessionID  string          `json:"session_id"`
	Cwd        string          `json:"cwd"`
	ToolName   string          `json:"tool_name,omitempty"`
	ToolInput  json.RawMessage `json:"tool_input,omitempty"`
	ToolOutput *string         `json:"tool_output,omitempty"`
	IsError    *bool           `json:"is_error,omitempty"`
	Prompt     *string         `json:"prompt,omitempty"`
}

// Result is the combined outcome of the hooks that ran for one event.
type Result struct {
	// Blocked is true when a hook blocked (exit code 2 or a "block"
	// decision). The hooks after it did not run.
	Blocked bool
	// Reason explains a block: the blocking hook's reason or stderr.
	Reason string
	// Approved is true when a hook printed {"decision":"allow"} and no hook
	// blocked. For pre_tool_use the caller may treat it as the user's
	// approval and skip the permission prompt.
	Approved bool
	// Feedback holds non-empty stdout of hooks that succeeded, in order,
	// for showing to the user.
	Feedback []string
	// Context holds stdout of successful user_prompt_submit and
	// session_start hooks, in order, to be added to the model's context.
	Context []string
	// Warnings describes hooks that failed without blocking: a non-zero
	// exit other than 2, a timeout, or a command that could not start.
	Warnings []string
}

// ContextText joins Context into one block, or returns "" when empty.
func (r Result) ContextText() string { return strings.Join(r.Context, "\n\n") }

// Options configures a Runner.
type Options struct {
	// Root is the workspace root: the hooks' working directory and
	// GTA_PROJECT_DIR. Empty means the current directory.
	Root string
	// SessionID identifies the session in payloads and GTA_SESSION_ID. It
	// can be changed later with SetSessionID.
	SessionID string
	// Env is extra KEY=VALUE pairs added to every hook's environment.
	Env []string
}

// Runner executes configured hooks. All methods are safe for concurrent use,
// and a nil *Runner runs nothing.
type Runner struct {
	cfg   Config
	root  string
	env   []string
	shell []string // argv prefix; the command is appended

	mu        sync.RWMutex
	sessionID string
}

// NewRunner validates cfg and returns a Runner that owns a private copy of
// it. An empty cfg yields a Runner whose methods return immediately.
func NewRunner(cfg Config, opts Options) (*Runner, error) {
	c := cfg.clone()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	root := opts.Root
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("hooks: working directory: %w", err)
		}
		root = wd
	}
	return &Runner{
		cfg:       c,
		root:      root,
		env:       append([]string(nil), opts.Env...),
		shell:     shellPrefix(),
		sessionID: opts.SessionID,
	}, nil
}

// shellPrefix returns the argv used to run a hook command.
func shellPrefix() []string {
	if runtime.GOOS == "windows" {
		return []string{"cmd", "/C"}
	}
	if p, err := exec.LookPath("bash"); err == nil {
		return []string{p, "-c"}
	}
	return []string{"sh", "-c"}
}

// SetSessionID changes the session id reported to later hooks, for example
// after the user starts a new conversation.
func (r *Runner) SetSessionID(id string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.sessionID = id
	r.mu.Unlock()
}

// Has reports whether any hook is configured for ev.
func (r *Runner) Has(ev Event) bool { return r != nil && len(r.cfg[ev]) > 0 }

// Config returns a copy of the runner's configuration.
func (r *Runner) Config() Config {
	if r == nil {
		return Config{}
	}
	return r.cfg.clone()
}

// PreToolUse runs the pre_tool_use hooks whose matcher fits toolName. When
// the result is Blocked the tool must not run, and Reason should be returned
// to the model as the tool's error output.
func (r *Runner) PreToolUse(ctx context.Context, toolName string, input json.RawMessage) (Result, error) {
	if !r.Has(PreToolUse) {
		return Result{}, nil
	}
	return r.run(ctx, Payload{Event: PreToolUse, ToolName: toolName, ToolInput: input})
}

// PostToolUse runs the post_tool_use hooks whose matcher fits toolName, after
// the tool produced output (isError reports whether it failed).
func (r *Runner) PostToolUse(ctx context.Context, toolName string, input json.RawMessage, output string, isError bool) (Result, error) {
	if !r.Has(PostToolUse) {
		return Result{}, nil
	}
	return r.run(ctx, Payload{Event: PostToolUse, ToolName: toolName, ToolInput: input, ToolOutput: &output, IsError: &isError})
}

// UserPromptSubmit runs the user_prompt_submit hooks. When the result is
// Blocked the prompt must be rejected and Reason shown to the user;
// otherwise ContextText should be added to the model's context.
func (r *Runner) UserPromptSubmit(ctx context.Context, prompt string) (Result, error) {
	if !r.Has(UserPromptSubmit) {
		return Result{}, nil
	}
	return r.run(ctx, Payload{Event: UserPromptSubmit, Prompt: &prompt})
}

// Stop runs the stop hooks after the agent finished a turn. A Blocked result
// asks the agent to continue with Reason as the next instruction; callers
// should cap how many times in a row they honour that.
func (r *Runner) Stop(ctx context.Context) (Result, error) {
	if !r.Has(Stop) {
		return Result{}, nil
	}
	return r.run(ctx, Payload{Event: Stop})
}

// SessionStart runs the session_start hooks. ContextText should be added to
// the model's context. A session cannot be blocked, so exit code 2 and a
// "block" decision are reported as warnings.
func (r *Runner) SessionStart(ctx context.Context) (Result, error) {
	if !r.Has(SessionStart) {
		return Result{}, nil
	}
	return r.run(ctx, Payload{Event: SessionStart})
}

// run executes the hooks for p.Event in order. It returns an error only when
// ctx is cancelled; hook failures are reported in the Result.
func (r *Runner) run(ctx context.Context, p Payload) (Result, error) {
	r.mu.RLock()
	p.SessionID = r.sessionID
	r.mu.RUnlock()
	p.Cwd = r.root
	if len(p.ToolInput) > 0 && !json.Valid(p.ToolInput) {
		// Keep the payload valid JSON: pass malformed input as a string.
		quoted, _ := json.Marshal(string(p.ToolInput))
		p.ToolInput = quoted
	}
	stdin, err := json.Marshal(p)
	if err != nil {
		return Result{}, fmt.Errorf("hooks: encode payload: %w", err)
	}
	env := r.environ(p)

	var res Result
	for i := range r.cfg[p.Event] {
		h := &r.cfg[p.Event][i]
		if p.Event.usesMatcher() && !h.matches(p.ToolName) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return res, err
		}
		out := r.exec(ctx, h, stdin, env)
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if stop := apply(&res, p.Event, h, out); stop {
			break
		}
	}
	if res.Blocked {
		res.Approved = false
	}
	return res, nil
}

// environ builds the environment shared by every hook of one event.
func (r *Runner) environ(p Payload) []string {
	env := append(os.Environ(), r.env...)
	return append(env,
		"GTA_EVENT="+string(p.Event),
		"GTA_TOOL_NAME="+p.ToolName,
		"GTA_SESSION_ID="+p.SessionID,
		"GTA_PROJECT_DIR="+r.root,
	)
}

// outcome is what one hook process did.
type outcome struct {
	code     int    // exit code; -1 when the process did not exit normally
	status   string // the process state, e.g. "signal: segmentation fault"
	stdout   string
	stderr   string
	timedOut bool
	startErr error
}

// exec runs one hook with its timeout. Its whole process group is killed
// when the timeout expires or ctx is cancelled.
func (r *Runner) exec(ctx context.Context, h *Hook, stdin []byte, env []string) outcome {
	timeout := h.timeout()
	hctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	argv := append(append([]string(nil), r.shell...), h.Command)
	cmd := exec.CommandContext(hctx, argv[0], argv[1:]...)
	cmd.Dir = r.root
	cmd.Env = env
	cmd.Stdin = bytes.NewReader(stdin)
	stdout := &cappedBuffer{limit: maxCapture}
	stderr := &cappedBuffer{limit: maxCapture}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = waitDelay
	setProcessGroup(cmd)

	err := cmd.Run()
	out := outcome{
		stdout: strings.TrimSpace(stdout.String()),
		stderr: strings.TrimSpace(stderr.String()),
	}
	if errors.Is(hctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		out.timedOut = true
		out.code = -1
		return out
	}
	if cmd.ProcessState != nil {
		out.code = cmd.ProcessState.ExitCode()
		out.status = cmd.ProcessState.String()
		return out
	}
	if err != nil {
		out.startErr = err
		out.code = -1
	}
	return out
}

// decision is the optional JSON object a hook may print on stdout.
type decision struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

// parseDecision recognises a {"decision": ...} object on stdout.
func parseDecision(stdout string) (decision, bool) {
	if !strings.HasPrefix(stdout, "{") {
		return decision{}, false
	}
	var d decision
	if err := json.Unmarshal([]byte(stdout), &d); err != nil {
		return decision{}, false
	}
	d.Decision = strings.ToLower(strings.TrimSpace(d.Decision))
	switch d.Decision {
	case "block", "allow":
		d.Reason = strings.TrimSpace(d.Reason)
		return d, true
	case "approve":
		d.Decision = "allow"
		d.Reason = strings.TrimSpace(d.Reason)
		return d, true
	}
	return decision{}, false
}

// apply folds one hook's outcome into res and reports whether the chain
// must stop.
func apply(res *Result, ev Event, h *Hook, out outcome) (stop bool) {
	name := h.label()
	switch {
	case out.startErr != nil:
		res.Warnings = append(res.Warnings, fmt.Sprintf("%s hook %q could not run: %v", ev, name, out.startErr))
		return false
	case out.timedOut:
		res.Warnings = append(res.Warnings, fmt.Sprintf("%s hook %q timed out after %s and was killed", ev, name, h.timeout()))
		return false
	}

	blocked := out.code == 2
	reason := out.stderr
	text := out.stdout
	explicit := false
	if d, ok := parseDecision(out.stdout); ok {
		explicit = true
		blocked = d.Decision == "block"
		text = ""
		if blocked {
			if d.Reason != "" {
				reason = d.Reason
			}
		} else {
			res.Approved = true
			text = d.Reason
		}
	}

	if blocked {
		if reason == "" {
			reason = fmt.Sprintf("blocked by %s hook %q", ev, name)
		}
		if ev == SessionStart {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s hook %q asked to block, which session_start cannot do: %s", ev, name, reason))
			return false
		}
		res.Blocked = true
		res.Reason = reason
		return true
	}

	if out.code != 0 && !explicit {
		msg := fmt.Sprintf("%s hook %q exited with code %d", ev, name, out.code)
		if out.code < 0 {
			msg = fmt.Sprintf("%s hook %q was terminated (%s)", ev, name, out.status)
		}
		if out.stderr != "" {
			msg += ": " + out.stderr
		}
		res.Warnings = append(res.Warnings, msg)
		return false
	}
	if text != "" {
		if ev == UserPromptSubmit || ev == SessionStart {
			res.Context = append(res.Context, text)
		} else {
			res.Feedback = append(res.Feedback, text)
		}
	}
	return false
}

// cappedBuffer keeps at most limit bytes and silently discards the rest, so
// a chatty hook cannot exhaust memory. It never returns a write error, which
// would make the hook see a broken pipe.
type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
			b.truncated = true
		} else {
			b.buf.Write(p)
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string {
	if b.truncated {
		return b.buf.String() + "\n[output truncated]"
	}
	return b.buf.String()
}
