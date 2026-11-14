package workflow

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"
)

// Built-in step type names.
const (
	TypeNoop  = "noop"
	TypeSleep = "sleep"
	TypeFail  = "fail"
	TypeShell = "shell"
	TypeAgent = "agent"
)

// ShellCommand is a request to the shell executor's runner.
type ShellCommand struct {
	Command string
	Dir     string
	// Env holds extra variables, added to the runner's environment.
	Env   map[string]string
	Stdin string
	// IdempotencyKey is the step's key, for runners that deduplicate.
	IdempotencyKey string
}

// ShellResult is the outcome of a shell command.
type ShellResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// ShellRunner runs a shell command. It returns an error only when the
// command could not be run or was cancelled; a non-zero exit is reported in
// ShellResult.ExitCode.
type ShellRunner func(ctx context.Context, cmd ShellCommand) (ShellResult, error)

// AgentRequest asks the host to run a sub-agent.
type AgentRequest struct {
	RunID          string
	StepID         string
	Attempt        int
	IdempotencyKey string
	// Prompt is the task for the agent.
	Prompt string
	// Description is a short label for progress displays.
	Description string
	// Mode is a host-defined agent mode, such as "explore" or "general".
	Mode string
	// Context carries any extra structured inputs.
	Context map[string]any
	Resumed bool
}

// AgentResult is a sub-agent's outcome.
type AgentResult struct {
	// Report is the agent's final message; it becomes the "report" output.
	Report string
	// Outputs are merged into the step outputs.
	Outputs map[string]any
}

// AgentFunc runs a sub-agent for the "agent" step type.
type AgentFunc func(ctx context.Context, req AgentRequest) (AgentResult, error)

// BuiltinOptions configures NewBuiltinRegistry.
type BuiltinOptions struct {
	// Shell runs "shell" steps; nil leaves the type unregistered. Use
	// ExecShellRunner for real commands.
	Shell ShellRunner
	// Agent runs "agent" steps; nil leaves the type unregistered.
	Agent AgentFunc
}

// NewBuiltinRegistry returns a registry with noop, sleep and fail, plus
// shell and agent when their runners are given.
func NewBuiltinRegistry(opts BuiltinOptions) *Registry {
	r := NewRegistry()
	r.MustRegister(TypeNoop, NoopExecutor{})
	r.MustRegister(TypeSleep, SleepExecutor{})
	r.MustRegister(TypeFail, FailExecutor{})
	if opts.Shell != nil {
		r.MustRegister(TypeShell, &ShellExecutor{Run: opts.Shell})
	}
	if opts.Agent != nil {
		r.MustRegister(TypeAgent, &AgentExecutor{Run: opts.Agent})
	}
	return r
}

// NoopExecutor succeeds immediately, echoing its inputs as outputs.
type NoopExecutor struct{}

// Execute implements Executor.
func (NoopExecutor) Execute(ctx context.Context, sc StepContext) (Outputs, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return sc.Inputs, nil
}

// SleepExecutor waits for its "duration" input (a duration string or a
// number of seconds) on the engine clock.
type SleepExecutor struct{}

// DeclaredInputs implements InputDeclarer.
func (SleepExecutor) DeclaredInputs() []InputDecl {
	return []InputDecl{{Name: "duration", Required: true}}
}

// Execute implements Executor.
func (SleepExecutor) Execute(ctx context.Context, sc StepContext) (Outputs, error) {
	d, err := durationInput(sc.Inputs["duration"])
	if err != nil {
		return nil, Permanent(fmt.Errorf("duration: %w", err))
	}
	clock := sc.Clock
	if clock == nil {
		clock = SystemClock()
	}
	if err := sleepCtx(ctx, clock, d); err != nil {
		return nil, err
	}
	return Outputs{"slept": d.String()}, nil
}

func durationInput(v any) (time.Duration, error) {
	switch x := v.(type) {
	case string:
		d, err := time.ParseDuration(strings.TrimSpace(x))
		if err != nil {
			return 0, err
		}
		if d < 0 {
			return 0, errors.New("negative duration")
		}
		return d, nil
	case float64:
		if x < 0 || x > 1e9 {
			return 0, fmt.Errorf("%v seconds is out of range", x)
		}
		return time.Duration(x * float64(time.Second)), nil
	}
	return 0, fmt.Errorf("want a duration string or seconds, got a %s", typeName(v))
}

// FailExecutor fails, for testing pipelines. Inputs:
//
//	message        the error message (default "step failed")
//	class          transient, timeout, error (default) or permanent
//	fail_attempts  fail only the first N attempts, then succeed; 0 or
//	               absent fails every attempt
type FailExecutor struct{}

// DeclaredInputs implements InputDeclarer.
func (FailExecutor) DeclaredInputs() []InputDecl {
	return []InputDecl{{Name: "message"}, {Name: "class"}, {Name: "fail_attempts"}}
}

// Execute implements Executor.
func (FailExecutor) Execute(ctx context.Context, sc StepContext) (Outputs, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	msg := "step failed"
	if s, ok := sc.Inputs["message"].(string); ok && s != "" {
		msg = s
	}
	class := ClassError
	if s, ok := sc.Inputs["class"].(string); ok && s != "" {
		c, ok := parseErrorClass(s)
		if !ok {
			return nil, Permanent(fmt.Errorf("unknown error class %q", s))
		}
		class = c
	}
	if raw, ok := sc.Inputs["fail_attempts"]; ok && raw != nil {
		n, ok := toNumber(raw)
		if !ok || n < 0 {
			return nil, Permanent(fmt.Errorf("fail_attempts must be a non-negative number"))
		}
		if n > 0 && float64(sc.Attempt) > n {
			return Outputs{"attempts": float64(sc.Attempt)}, nil
		}
	}
	return nil, &StepError{Class: class, Err: errors.New(msg)}
}

// ShellExecutor runs a command through a ShellRunner. Inputs:
//
//	command        the command line (required)
//	dir            working directory
//	env            map of extra environment variables
//	stdin          standard input
//	allow_failure  succeed even on a non-zero exit status
//
// Outputs are stdout, stderr and exit_code. A non-zero exit fails the step
// with class error, so a retry policy with retry_on ["error"] retries it.
type ShellExecutor struct {
	Run ShellRunner
}

// DeclaredInputs implements InputDeclarer.
func (*ShellExecutor) DeclaredInputs() []InputDecl {
	return []InputDecl{{Name: "command", Required: true}, {Name: "dir"}, {Name: "env"}, {Name: "stdin"}, {Name: "allow_failure"}}
}

// Execute implements Executor.
func (e *ShellExecutor) Execute(ctx context.Context, sc StepContext) (Outputs, error) {
	cmd := ShellCommand{IdempotencyKey: sc.IdempotencyKey}
	var ok bool
	if cmd.Command, ok = sc.Inputs["command"].(string); !ok || strings.TrimSpace(cmd.Command) == "" {
		return nil, Permanent(errors.New("command must be a non-empty string"))
	}
	if v, present := sc.Inputs["dir"]; present && v != nil {
		if cmd.Dir, ok = v.(string); !ok {
			return nil, Permanent(errors.New("dir must be a string"))
		}
	}
	if v, present := sc.Inputs["stdin"]; present && v != nil {
		if cmd.Stdin, ok = v.(string); !ok {
			return nil, Permanent(errors.New("stdin must be a string"))
		}
	}
	if v, present := sc.Inputs["env"]; present && v != nil {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, Permanent(errors.New("env must be a map of strings"))
		}
		cmd.Env = make(map[string]string, len(m))
		for k, val := range m {
			if k == "" || strings.ContainsAny(k, "=\x00") {
				return nil, Permanent(fmt.Errorf("invalid environment variable name %q", k))
			}
			cmd.Env[k] = stringify(val)
		}
	}
	allowFailure := truthy(sc.Inputs["allow_failure"])
	res, err := e.Run(ctx, cmd)
	if err != nil {
		return nil, err
	}
	out := Outputs{"stdout": res.Stdout, "stderr": res.Stderr, "exit_code": float64(res.ExitCode)}
	if res.ExitCode != 0 && !allowFailure {
		detail := strings.TrimSpace(res.Stderr)
		if detail == "" {
			detail = strings.TrimSpace(res.Stdout)
		}
		msg := fmt.Sprintf("command exited with status %d", res.ExitCode)
		if detail != "" {
			msg += ": " + tail(detail, 2000)
		}
		return nil, &StepError{Class: ClassError, Err: errors.New(msg)}
	}
	return out, nil
}

// AgentExecutor runs a sub-agent through an AgentFunc. Inputs: prompt
// (required), description, mode and context (a map). Outputs are "report"
// plus the agent's own outputs.
type AgentExecutor struct {
	Run AgentFunc
}

// DeclaredInputs implements InputDeclarer.
func (*AgentExecutor) DeclaredInputs() []InputDecl {
	return []InputDecl{{Name: "prompt", Required: true}, {Name: "description"}, {Name: "mode"}, {Name: "context"}}
}

// Execute implements Executor.
func (e *AgentExecutor) Execute(ctx context.Context, sc StepContext) (Outputs, error) {
	prompt, ok := sc.Inputs["prompt"].(string)
	if !ok || strings.TrimSpace(prompt) == "" {
		return nil, Permanent(errors.New("prompt must be a non-empty string"))
	}
	req := AgentRequest{
		RunID: sc.RunID, StepID: sc.StepID, Attempt: sc.Attempt, IdempotencyKey: sc.IdempotencyKey,
		Prompt: prompt, Resumed: sc.Resumed,
	}
	req.Description, _ = sc.Inputs["description"].(string)
	if req.Description == "" {
		req.Description = sc.StepID
	}
	req.Mode, _ = sc.Inputs["mode"].(string)
	if v, present := sc.Inputs["context"]; present && v != nil {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, Permanent(errors.New("context must be a map"))
		}
		req.Context = m
	}
	res, err := e.Run(ctx, req)
	if err != nil {
		return nil, err
	}
	out := Outputs{}
	for k, v := range res.Outputs {
		out[k] = v
	}
	out["report"] = res.Report
	return out, nil
}

// maxShellOutput caps each captured stream of ExecShellRunner.
const maxShellOutput = 1 << 20

// ExecShellRunner runs commands with the platform shell (sh -c, or
// cmd /C on Windows), capturing at most 1 MiB of each output stream. On
// cancellation the process is killed.
func ExecShellRunner(ctx context.Context, sc ShellCommand) (ShellResult, error) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/C", sc.Command)
	} else {
		cmd = exec.CommandContext(ctx, "/bin/sh", "-c", sc.Command)
	}
	cmd.Dir = sc.Dir
	cmd.WaitDelay = 5 * time.Second
	if len(sc.Env) > 0 {
		env := os.Environ()
		for _, k := range sortedKeys(sc.Env) {
			env = append(env, k+"="+sc.Env[k])
		}
		cmd.Env = env
	}
	if sc.Stdin != "" {
		cmd.Stdin = strings.NewReader(sc.Stdin)
	}
	stdout := &cappedBuffer{limit: maxShellOutput}
	stderr := &cappedBuffer{limit: maxShellOutput}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	res := ShellResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.ExitCode = exitErr.ExitCode()
		if res.ExitCode < 0 {
			res.ExitCode = 1
		}
		return res, nil
	}
	if err != nil {
		return res, err
	}
	return res, nil
}

// cappedBuffer keeps the first limit bytes written and counts the rest.
type cappedBuffer struct {
	buf     bytes.Buffer
	limit   int
	dropped int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		n := min(room, len(p))
		b.buf.Write(p[:n])
		b.dropped += len(p) - n
	} else {
		b.dropped += len(p)
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string {
	if b.dropped == 0 {
		return b.buf.String()
	}
	return b.buf.String() + fmt.Sprintf("\n[... %d bytes truncated]", b.dropped)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return "..." + s[i:]
}
