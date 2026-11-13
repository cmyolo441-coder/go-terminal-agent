// Package gitx integrates the agent with git by running the git binary.
//
// Every command runs non-interactively: it is pointed at the repository with
// -C, bounded by a timeout, never pages, never prompts for credentials and
// never opens an editor. User configuration that would change machine-read
// output (colour, diff prefixes, quoted paths, external diff drivers) is
// overridden per command so parsing stays reliable on any setup.
//
// No library dependency is needed beyond the standard library; git itself
// must be installed and on PATH. Callers can check Available first.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// DefaultTimeout bounds every git command whose context has no earlier
// deadline.
const DefaultTimeout = 30 * time.Second

// maxOutput caps how much of a command's standard output is buffered, so a
// pathological repository cannot exhaust memory. Callers that need less
// (Diff) apply their own, smaller budget on top.
const maxOutput = 64 << 20

// ErrGitNotFound is returned when the git binary is not on PATH.
var ErrGitNotFound = errors.New("git is not installed or not on PATH")

// ErrNotRepo is returned when a directory is not inside a git work tree.
var ErrNotRepo = errors.New("not a git repository")

// ErrNoCommits is returned by operations that need HEAD in a repository
// whose current branch has no commits yet.
var ErrNoCommits = errors.New("the repository has no commits yet")

// ErrNothingToCommit is returned by Commit when nothing is staged.
var ErrNothingToCommit = errors.New("nothing to commit: no changes are staged for the selected files")

// Error describes a git command that ran and exited unsuccessfully.
type Error struct {
	// Args are the git arguments, without the leading global options.
	Args []string
	// ExitCode is the process exit status, or -1 if it did not exit normally.
	ExitCode int
	// Stderr is git's trimmed diagnostic output.
	Stderr string
	// Err is the underlying error from os/exec or the context.
	Err error
}

func (e *Error) Error() string {
	cmd := "git " + strings.Join(e.Args, " ")
	if len(cmd) > 120 {
		cmd = cmd[:117] + "..."
	}
	msg := e.Stderr
	if msg == "" && e.Err != nil {
		msg = e.Err.Error()
	}
	if msg == "" {
		msg = fmt.Sprintf("exit status %d", e.ExitCode)
	}
	return fmt.Sprintf("%s: %s", cmd, msg)
}

func (e *Error) Unwrap() error { return e.Err }

var (
	lookOnce sync.Once
	gitPath  string
	lookErr  error
)

func gitBinary() (string, error) {
	lookOnce.Do(func() {
		gitPath, lookErr = exec.LookPath("git")
		if lookErr != nil {
			lookErr = ErrGitNotFound
		}
	})
	return gitPath, lookErr
}

// Available reports whether the git binary can be found.
func Available() bool {
	_, err := gitBinary()
	return err == nil
}

// globalArgs are prepended to every invocation. They neutralise user
// configuration that would change the shape of machine-read output.
var globalArgs = []string{
	"--no-pager",
	"-c", "core.quotepath=false",
	"-c", "color.ui=never",
	"-c", "core.pager=cat",
	"-c", "diff.noprefix=false",
	"-c", "diff.mnemonicPrefix=false",
	"-c", "diff.relative=false",
	"-c", "advice.detachedHead=false",
	"-c", "log.showSignature=false",
}

// runOpts tweaks a single invocation.
type runOpts struct {
	stdin []byte
	// env holds extra KEY=VALUE pairs.
	env []string
	// literal disables pathspec magic so paths with *, ? or : are literal.
	literal bool
	// allowExit lists non-zero exit codes that are not errors.
	allowExit []int
}

// result is a finished invocation.
type result struct {
	stdout   []byte
	stderr   string
	exitCode int
}

// limitedBuffer keeps at most max bytes and records that more were dropped.
type limitedBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room < len(p) {
		if room > 0 {
			b.buf.Write(p[:room])
		}
		b.truncated = true
		return len(p), nil
	}
	return b.buf.Write(p)
}

// run executes git in root with args and returns its output. A non-zero exit
// that is not listed in o.allowExit becomes an *Error.
func run(ctx context.Context, root string, o runOpts, args ...string) (result, error) {
	bin, err := gitBinary()
	if err != nil {
		return result{}, err
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultTimeout)
		defer cancel()
	}
	full := make([]string, 0, len(globalArgs)+len(args)+3)
	full = append(full, "-C", root)
	full = append(full, globalArgs...)
	if o.literal {
		full = append(full, "--literal-pathspecs")
	}
	full = append(full, args...)

	cmd := exec.CommandContext(ctx, bin, full...)
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_PAGER=cat",
		"PAGER=cat",
		"GIT_EDITOR=true",
		"GIT_SEQUENCE_EDITOR=true",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		"GCM_INTERACTIVE=never",
		"LC_ALL=C",
		"LANG=C",
	)
	cmd.Env = append(cmd.Env, o.env...)
	if o.stdin != nil {
		cmd.Stdin = bytes.NewReader(o.stdin)
	}
	stdout := &limitedBuffer{max: maxOutput}
	stderr := &limitedBuffer{max: 64 << 10}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// Hooks or helpers that inherit the pipes must not keep us waiting
	// once git itself has exited or been killed.
	cmd.WaitDelay = 2 * time.Second

	runErr := cmd.Run()
	res := result{stdout: stdout.buf.Bytes(), stderr: strings.TrimSpace(stderr.buf.String())}
	if runErr == nil {
		return res, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return res, &Error{Args: args, ExitCode: -1, Stderr: "timed out", Err: ctxErr}
		}
		return res, &Error{Args: args, ExitCode: -1, Stderr: res.stderr, Err: ctxErr}
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		res.exitCode = exitErr.ExitCode()
		for _, c := range o.allowExit {
			if c == res.exitCode {
				return res, nil
			}
		}
		e := &Error{Args: args, ExitCode: res.exitCode, Stderr: res.stderr, Err: runErr}
		if isNotRepoMessage(res.stderr) {
			return res, fmt.Errorf("%s: %w", root, ErrNotRepo)
		}
		return res, e
	}
	return res, &Error{Args: args, ExitCode: -1, Stderr: res.stderr, Err: runErr}
}

// output is run that returns trimmed stdout as a string.
func output(ctx context.Context, root string, args ...string) (string, error) {
	res, err := run(ctx, root, runOpts{}, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(res.stdout)), nil
}

func isNotRepoMessage(stderr string) bool {
	return strings.Contains(stderr, "not a git repository")
}

// checkRoot verifies root is an existing directory, giving a clear error
// instead of git's "cannot change to" message.
func checkRoot(root string) error {
	if strings.TrimSpace(root) == "" {
		return errors.New("gitx: repository root is empty")
	}
	st, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("gitx: %w", err)
	}
	if !st.IsDir() {
		return fmt.Errorf("gitx: %s is not a directory", root)
	}
	return nil
}

// IsRepo reports whether root is inside a git work tree. It returns false
// when git is not installed, the directory does not exist or the check
// times out.
func IsRepo(root string) bool {
	if checkRoot(root) != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := output(ctx, root, "rev-parse", "--is-inside-work-tree")
	return err == nil && out == "true"
}

// TopLevel returns the absolute path of the work tree containing root.
func TopLevel(ctx context.Context, root string) (string, error) {
	if err := checkRoot(root); err != nil {
		return "", err
	}
	out, err := output(ctx, root, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", fmt.Errorf("%s: %w (inside the .git directory or a bare repository)", root, ErrNotRepo)
	}
	return out, nil
}

// headCommit returns the full sha of HEAD, or "" with no error when the
// current branch has no commits yet.
func headCommit(ctx context.Context, root string) (string, error) {
	res, err := run(ctx, root, runOpts{allowExit: []int{1}}, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err != nil {
		return "", err
	}
	if res.exitCode != 0 {
		return "", nil
	}
	return strings.TrimSpace(string(res.stdout)), nil
}

// emptyTree returns the id of the empty tree in this repository's object
// format (it differs between SHA-1 and SHA-256 repositories).
func emptyTree(ctx context.Context, root string) (string, error) {
	res, err := run(ctx, root, runOpts{stdin: []byte{}}, "hash-object", "-t", "tree", "--stdin")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(res.stdout)), nil
}

// baseRev is HEAD, or the empty tree on an unborn branch, so diffs work in a
// freshly initialised repository.
func baseRev(ctx context.Context, root string) (string, error) {
	head, err := headCommit(ctx, root)
	if err != nil {
		return "", err
	}
	if head != "" {
		return "HEAD", nil
	}
	return emptyTree(ctx, root)
}
