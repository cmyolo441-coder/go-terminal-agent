package plugin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// program is a resolved executable.
type program struct {
	// path is absolute for files shipped with the plugin or given as
	// absolute paths, and a bare name for programs found in PATH.
	path string
	args []string
}

// resolveProgram turns a manifest command into a program, checking that a
// plugin-relative executable exists, stays inside the plugin directory
// (after following symlinks) and is executable.
func resolveProgram(pluginDir, command string, args []string) (program, error) {
	command = strings.TrimSpace(command)
	switch {
	case filepath.IsAbs(command):
		if err := checkProgramFile(command); err != nil {
			return program{}, err
		}
		return program{path: command, args: args}, nil
	case strings.ContainsAny(command, `/\`) || strings.HasPrefix(command, "."):
		p := filepath.Join(pluginDir, filepath.FromSlash(command))
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			return program{}, fmt.Errorf("command %q: %w", command, notExistHint(err))
		}
		root, err := filepath.EvalSymlinks(pluginDir)
		if err != nil {
			return program{}, err
		}
		if rel, err := filepath.Rel(root, real); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return program{}, fmt.Errorf("command %q resolves outside the plugin directory", command)
		}
		if err := checkProgramFile(real); err != nil {
			return program{}, err
		}
		return program{path: real, args: args}, nil
	default:
		if _, err := exec.LookPath(command); err != nil {
			return program{}, fmt.Errorf("command %q was not found in PATH", command)
		}
		return program{path: command, args: args}, nil
	}
}

func notExistHint(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("file does not exist")
	}
	return err
}

func checkProgramFile(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("command %s: %w", path, notExistHint(err))
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("command %s is not a regular file", path)
	}
	if err := checkExecutable(path, st); err != nil {
		return err
	}
	return checkNotWorldWritable(path, st)
}

// runSpec describes one process run.
type runSpec struct {
	prog      program
	extraArgs []string
	dir       string
	env       []string
	stdin     []byte
	timeout   time.Duration
	maxOut    int
	maxErr    int
}

// runResult is what a finished process produced.
type runResult struct {
	stdout   []byte
	overflow bool
	stderr   string
	exitCode int
	timedOut bool
	elapsed  time.Duration
}

// cappedBuffer keeps at most limit bytes and calls onOverflow once when
// more arrive. Writes always succeed so the child never blocks on a pipe.
type cappedBuffer struct {
	mu         sync.Mutex
	buf        bytes.Buffer
	limit      int
	over       bool
	onOverflow func()
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	room := c.limit - c.buf.Len()
	if len(p) > room {
		if room > 0 {
			c.buf.Write(p[:room])
		}
		if !c.over {
			c.over = true
			if c.onOverflow != nil {
				c.onOverflow()
			}
		}
		return len(p), nil
	}
	c.buf.Write(p)
	return len(p), nil
}

func (c *cappedBuffer) result() ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.buf.Bytes()...), c.over
}

// runProcess starts the program, feeds stdin, and waits for it, killing
// its process group on timeout, on cancellation and when stdout overflows.
// A non-zero exit is reported in the result, not as an error.
func runProcess(ctx context.Context, s runSpec) (runResult, error) {
	if s.timeout <= 0 {
		s.timeout = DefaultToolTimeout * time.Second
	}
	if s.maxOut <= 0 {
		s.maxOut = DefaultMaxOutput
	}
	if s.maxErr <= 0 {
		s.maxErr = 64 << 10
	}
	runCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	killCtx, kill := context.WithCancel(runCtx)
	defer kill()

	args := append(append([]string(nil), s.prog.args...), s.extraArgs...)
	cmd := exec.CommandContext(killCtx, s.prog.path, args...)
	cmd.Dir = s.dir
	cmd.Env = s.env
	cmd.Stdin = bytes.NewReader(s.stdin)
	stdout := &cappedBuffer{limit: s.maxOut, onOverflow: kill}
	stderr := &cappedBuffer{limit: s.maxErr}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	setProcessGroup(cmd)
	// If a grandchild keeps the pipes open after the process exits, stop
	// waiting for it after a short grace period.
	cmd.WaitDelay = 2 * time.Second

	start := time.Now()
	err := cmd.Start()
	if err != nil {
		return runResult{}, fmt.Errorf("start %s: %w", filepath.Base(s.prog.path), err)
	}
	err = cmd.Wait()
	res := runResult{elapsed: time.Since(start), exitCode: -1}
	res.stdout, res.overflow = stdout.result()
	errOut, errOver := stderr.result()
	res.stderr = strings.TrimSpace(string(errOut))
	if errOver {
		res.stderr += "\n… (stderr truncated)"
	}
	if cmd.ProcessState != nil {
		res.exitCode = cmd.ProcessState.ExitCode()
	}
	switch {
	case ctx.Err() != nil:
		return res, ctx.Err()
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		res.timedOut = true
		return res, nil
	case res.overflow:
		return res, nil
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) && !errors.Is(err, exec.ErrWaitDelay) {
		return res, err
	}
	return res, nil
}

// basicEnv lists the variables every plugin process inherits.
var basicEnv = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "LANGUAGE", "TERM", "TMPDIR", "TZ",
	"XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
	"SSL_CERT_FILE", "SSL_CERT_DIR",
	// Windows needs these to start most programs.
	"SYSTEMROOT", "SYSTEMDRIVE", "WINDIR", "COMSPEC", "PATHEXT", "TEMP", "TMP", "USERPROFILE",
	"APPDATA", "LOCALAPPDATA", "PROGRAMDATA", "PROGRAMFILES",
}

// buildEnv assembles a plugin process environment from the parent
// environment: the basics and LC_* locale variables, the plugin's
// pass_env names, its static env values, then GTA_* call variables.
func buildEnv(parent []string, passEnv []string, static map[string]string, gta map[string]string) []string {
	norm := func(k string) string {
		if runtime.GOOS == "windows" {
			return strings.ToUpper(k)
		}
		return k
	}
	allowed := map[string]bool{}
	for _, k := range basicEnv {
		allowed[norm(k)] = true
	}
	for _, k := range passEnv {
		allowed[norm(k)] = true
	}
	vals := map[string]string{}
	var order []string
	set := func(k, v string) {
		nk := norm(k)
		if _, ok := vals[nk]; !ok {
			order = append(order, k)
		}
		vals[nk] = v
	}
	for _, kv := range parent {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		if allowed[norm(k)] || strings.HasPrefix(k, "LC_") {
			set(k, v)
		}
	}
	for _, k := range sortedKeys(static) {
		set(k, static[k])
	}
	for _, k := range sortedKeys(gta) {
		set(k, gta[k])
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+vals[norm(k)])
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// expander replaces ${PLUGIN_DIR} and ${WORKSPACE}.
func expander(pluginDir, workspace string) *strings.Replacer {
	return strings.NewReplacer("${PLUGIN_DIR}", pluginDir, "${WORKSPACE}", workspace)
}

func expandAll(r *strings.Replacer, in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = r.Replace(s)
	}
	return out
}

// splitWords splits a string into shell-style words: whitespace separates
// words, single quotes keep text literally, double quotes allow \" and \\,
// and a backslash outside quotes escapes the next character.
func splitWords(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	const (
		none = iota
		single
		double
	)
	quote := none
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch quote {
		case single:
			if r == '\'' {
				quote = none
			} else {
				cur.WriteRune(r)
			}
		case double:
			switch {
			case r == '"':
				quote = none
			case r == '\\' && i+1 < len(rs) && (rs[i+1] == '"' || rs[i+1] == '\\'):
				i++
				cur.WriteRune(rs[i])
			default:
				cur.WriteRune(r)
			}
		default:
			switch {
			case r == ' ' || r == '\t' || r == '\n' || r == '\r':
				if inWord {
					words = append(words, cur.String())
					cur.Reset()
					inWord = false
				}
			case r == '\'':
				quote, inWord = single, true
			case r == '"':
				quote, inWord = double, true
			case r == '\\' && i+1 < len(rs):
				i++
				cur.WriteRune(rs[i])
				inWord = true
			default:
				cur.WriteRune(r)
				inWord = true
			}
		}
	}
	if quote != none {
		return nil, errors.New("unterminated quote in arguments")
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}
