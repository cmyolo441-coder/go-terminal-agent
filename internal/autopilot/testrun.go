package autopilot

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Defaults for running tests.
const (
	// DefaultTestTimeout bounds one run of the test command.
	DefaultTestTimeout = 10 * time.Minute
	// DefaultMaxTestOutput is how much test output is kept in memory: the
	// head and the tail, with the middle dropped.
	DefaultMaxTestOutput = 4 << 20
)

// ErrTestCommandNotRunnable is returned when the shell could not find or
// execute the test command (exit status 126 or 127). Editing code cannot
// fix that, so the fix loop stops.
var ErrTestCommandNotRunnable = errors.New("the test command could not be run")

// RunOptions tune RunTests.
type RunOptions struct {
	// Timeout bounds the run; zero means DefaultTestTimeout.
	Timeout time.Duration
	// Wrap, when set, turns the shell command into the argv that runs it,
	// for example inside a sandbox (normally tools.Env.Wrap).
	Wrap func(shell, command string) ([]string, error)
	// MaxOutput caps buffered output; zero means DefaultMaxTestOutput.
	MaxOutput int
	// Env adds environment variables (KEY=value).
	Env []string
}

// TestRun is the outcome of one run of a test command.
type TestRun struct {
	Command TestCommand
	// ExitCode is the process status, or -1 when it timed out.
	ExitCode int
	TimedOut bool
	Duration time.Duration
	// Output is the cleaned combined output (head and tail when long).
	Output string
	// Failures are the structured problems parsed from Output. A failing
	// run always has at least one, synthesized from the output tail when
	// nothing could be parsed.
	Failures []Failure
}

// Passed reports whether the command exited 0 within its timeout.
func (r *TestRun) Passed() bool { return r != nil && r.ExitCode == 0 && !r.TimedOut }

// Keys returns the set of failure keys.
func (r *TestRun) Keys() map[string]bool {
	m := map[string]bool{}
	if r == nil {
		return m
	}
	for _, f := range r.Failures {
		m[f.Key()] = true
	}
	return m
}

// Headline is a one-line status such as
// "FAIL go test ./... (exit 1, 2 failures, 1.4s)".
func (r *TestRun) Headline() string {
	status := "PASS"
	detail := fmt.Sprintf("exit %d", r.ExitCode)
	switch {
	case r.TimedOut:
		status = "TIMEOUT"
		detail = "timed out"
	case r.ExitCode != 0:
		status = "FAIL"
	}
	if n := len(r.Failures); n > 0 {
		detail += fmt.Sprintf(", %d failure%s", n, plural(n))
	}
	return fmt.Sprintf("%s %s (%s, %s)", status, r.Command.String(), detail, r.Duration.Round(10*time.Millisecond))
}

// Report renders the run for the model within budget bytes: the headline,
// then the most relevant failures.
func (r *TestRun) Report(budget int) string {
	if budget <= 0 {
		budget = DefaultFailureBudget
	}
	var b strings.Builder
	b.WriteString(r.Headline() + "\n")
	if r.Passed() {
		if t := tailLines(r.Output, 3); t != "" {
			b.WriteString(clipBytes(t, 600) + "\n")
		}
		return b.String()
	}
	b.WriteString("\n")
	b.WriteString(FormatFailures(r.Failures, budget-b.Len()))
	return b.String()
}

// RunTests runs a test command in root and parses its output. The error is
// non-nil only when the command could not run (ErrTestCommandNotRunnable,
// a sandbox refusal) or ctx was cancelled; failing tests are not an error.
func RunTests(ctx context.Context, root string, tc TestCommand, opts RunOptions) (*TestRun, error) {
	if strings.TrimSpace(tc.Command) == "" {
		return nil, ErrNoTestCommand
	}
	if tc.Framework == "" {
		tc.Framework = GuessFramework(tc.Command, root)
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTestTimeout
	}
	maxOut := opts.MaxOutput
	if maxOut <= 0 {
		maxOut = DefaultMaxTestOutput
	}
	dir := root
	if tc.Dir != "" {
		dir = filepath.Join(root, tc.Dir)
	}
	argv, err := shellArgv(tc.Command, opts.Wrap)
	if err != nil {
		return nil, err
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(rctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), testEnv...)
	cmd.Env = append(cmd.Env, opts.Env...)
	cmd.Stdin = nil
	setProcessGroup(cmd)
	// Grandchildren that inherited the pipes must not hold Wait forever
	// after the group is killed.
	cmd.WaitDelay = 3 * time.Second
	buf := newCapBuffer(maxOut)
	cmd.Stdout = buf
	cmd.Stderr = buf

	start := time.Now()
	runErr := cmd.Run()
	run := &TestRun{Command: tc, Duration: time.Since(start), Output: cleanOutput(buf.String())}
	if ctx.Err() != nil {
		return run, ctx.Err()
	}
	switch {
	case errors.Is(rctx.Err(), context.DeadlineExceeded):
		run.TimedOut = true
		run.ExitCode = -1
	case runErr != nil:
		var ee *exec.ExitError
		if !errors.As(runErr, &ee) {
			return run, fmt.Errorf("run %q: %w", tc.Command, runErr)
		}
		run.ExitCode = ee.ExitCode()
	}
	if !run.TimedOut && (run.ExitCode == 126 || run.ExitCode == 127) && runtime.GOOS != "windows" {
		return run, fmt.Errorf("%w: %q exited %d: %s", ErrTestCommandNotRunnable, tc.Command, run.ExitCode, clipBytes(tailLines(run.Output, 3), 400))
	}
	if !run.Passed() {
		run.Failures = ParseFailures(tc.Framework, run.Output)
		resolveFailurePaths(root, tc, run.Failures)
		if len(run.Failures) == 0 && !run.TimedOut {
			run.Failures = []Failure{{
				Framework: tc.Framework,
				Kind:      KindError,
				Message:   fmt.Sprintf("the test command exited %d and no individual failures could be parsed; output tail:\n%s", run.ExitCode, tailLines(run.Output, 25)),
			}}
		}
		if run.TimedOut {
			run.Failures = append(run.Failures, Failure{
				Framework: tc.Framework,
				Kind:      KindTimeout,
				Message:   fmt.Sprintf("the test command did not finish within %s; a test may hang, deadlock or wait for input. Output tail:\n%s", timeout, tailLines(run.Output, 15)),
			})
		}
	}
	return run, nil
}

// testEnv makes runners non-interactive and colourless.
var testEnv = []string{
	"CI=true", "GTA=1", "TERM=dumb", "NO_COLOR=1", "FORCE_COLOR=0",
	"CARGO_TERM_COLOR=never", "PY_COLORS=0", "GIT_PAGER=cat", "PAGER=cat",
}

func shellArgv(command string, wrap func(shell, command string) ([]string, error)) ([]string, error) {
	if wrap != nil {
		sh := ""
		if runtime.GOOS != "windows" {
			sh = posixShell()
		}
		argv, err := wrap(sh, command)
		if err != nil {
			return nil, fmt.Errorf("sandbox: %w", err)
		}
		if len(argv) == 0 {
			return nil, errors.New("sandbox: empty command")
		}
		return argv, nil
	}
	if runtime.GOOS == "windows" {
		return []string{"cmd", "/C", command}, nil
	}
	return []string{posixShell(), "-c", command}, nil
}

func posixShell() string {
	if _, err := exec.LookPath("bash"); err == nil {
		return "bash"
	}
	return "sh"
}

// resolveFailurePaths makes Go test file paths, which go test prints
// relative to the package directory, relative to the workspace root, and
// absolute paths inside the root relative to it.
func resolveFailurePaths(root string, tc TestCommand, fs []Failure) {
	module := ""
	if tc.Framework == FrameworkGo || tc.Framework == FrameworkAuto {
		module = goModulePath(root)
	}
	absRoot, _ := filepath.Abs(root)
	for i := range fs {
		f := &fs[i]
		if f.File == "" {
			continue
		}
		native := filepath.FromSlash(f.File)
		if filepath.IsAbs(native) {
			if rel, err := filepath.Rel(absRoot, native); err == nil && !strings.HasPrefix(rel, "..") {
				f.File = filepath.ToSlash(rel)
			}
			continue
		}
		if f.Framework != FrameworkGo || module == "" || strings.Contains(f.File, "/") || f.Kind == KindBuild {
			continue
		}
		// "calc_test.go" in package module/sub → "sub/calc_test.go".
		if f.Suite == module {
			continue
		}
		if sub, ok := strings.CutPrefix(f.Suite, module+"/"); ok {
			candidate := filepath.Join(absRoot, filepath.FromSlash(sub), native)
			if fileExists(candidate) {
				f.File = sub + "/" + f.File
			}
		}
	}
}

// goModulePath reads the module path from root/go.mod.
func goModulePath(root string) string {
	fh, err := os.Open(filepath.Join(root, "go.mod"))
	if err != nil {
		return ""
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			return strings.Trim(strings.TrimSpace(rest), `"`)
		}
	}
	return ""
}

// capBuffer keeps the head and the tail of a stream, dropping the middle
// once the cap is reached. It is safe for concurrent writes.
type capBuffer struct {
	mu      sync.Mutex
	headCap int
	tailCap int
	head    []byte
	tail    []byte
	dropped int64
}

func newCapBuffer(max int) *capBuffer {
	head := max / 8
	return &capBuffer{headCap: head, tailCap: max - head}
}

func (b *capBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if room := b.headCap - len(b.head); room > 0 {
		k := min(room, len(p))
		b.head = append(b.head, p[:k]...)
		p = p[k:]
	}
	if len(p) == 0 {
		return n, nil
	}
	b.tail = append(b.tail, p...)
	if over := len(b.tail) - b.tailCap; over > 0 && len(b.tail) >= 2*b.tailCap {
		b.dropped += int64(over)
		b.tail = append(b.tail[:0:0], b.tail[over:]...)
	}
	return n, nil
}

func (b *capBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	tail := b.tail
	dropped := b.dropped
	if over := len(tail) - b.tailCap; over > 0 {
		dropped += int64(over)
		tail = tail[over:]
	}
	if dropped == 0 {
		return string(b.head) + string(tail)
	}
	// Resume the tail at a line boundary so parsers see whole lines.
	if i := strings.IndexByte(string(tail), '\n'); i >= 0 && i < 4096 {
		dropped += int64(i + 1)
		tail = tail[i+1:]
	}
	return string(b.head) + fmt.Sprintf("\n… [%d bytes of output omitted] …\n", dropped) + string(tail)
}
