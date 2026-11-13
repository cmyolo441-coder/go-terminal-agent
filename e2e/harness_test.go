// Package e2e_test drives the real gta binary against an in-process mock of
// the Anthropic Messages API. TestMain builds the binary once; every test
// gets its own workspace, config dir and mock server.
package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// gtaBin is the binary under test, built by TestMain.
var gtaBin string

func TestMain(m *testing.M) { os.Exit(runTests(m)) }

func runTests(m *testing.M) int {
	dir, err := os.MkdirTemp("", "gta-e2e-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		return 1
	}
	defer os.RemoveAll(dir)
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	gtaBin = filepath.Join(dir, "gta"+exe)
	goTool, err := exec.LookPath("go")
	if err != nil {
		goTool = filepath.Join(os.Getenv("GOROOT"), "bin", "go"+exe)
	}
	build := exec.Command(goTool, "build", "-o", gtaBin, "./cmd/gta")
	build.Dir = ".." // the module root
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "e2e: building gta:", err)
		return 1
	}
	return m.Run()
}

// runTimeout bounds every gta invocation, so a hang fails instead of
// stalling the suite.
const runTimeout = 45 * time.Second

// baseConfig keeps runs fast and hermetic: no language servers, plugins or
// server-side extras.
var baseConfig = map[string]any{
	"lsp":             false,
	"disable_plugins": true,
}

// gtaEnv is one test's isolated world: a workspace, a config dir and the
// mock API that gta talks to.
type gtaEnv struct {
	t      *testing.T
	root   string
	cfgDir string
	mock   *mockAPI
	// extra environment variables (KEY=value) for every run.
	extra []string
}

// newEnv creates a workspace and config dir and writes the base user
// config merged with cfg (nil for none).
func newEnv(t *testing.T, mock *mockAPI, cfg map[string]any) *gtaEnv {
	t.Helper()
	base := t.TempDir()
	// Resolve symlinks (macOS /var) so paths match what gta reports.
	if r, err := filepath.EvalSymlinks(base); err == nil {
		base = r
	}
	e := &gtaEnv{t: t, root: filepath.Join(base, "work"), cfgDir: filepath.Join(base, "config"), mock: mock}
	for _, d := range []string{e.root, e.cfgDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	merged := map[string]any{}
	for k, v := range baseConfig {
		merged[k] = v
	}
	for k, v := range cfg {
		merged[k] = v
	}
	data, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	e.writeFile(filepath.Join(e.cfgDir, "config.json"), string(data))
	return e
}

// writeFile writes a file, creating its directory.
func (e *gtaEnv) writeFile(path, content string) {
	e.t.Helper()
	if !filepath.IsAbs(path) {
		path = filepath.Join(e.root, path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// environ is the process environment for gta: the test's own minus
// anything that could point gta at a real API, a real config or a proxy.
func (e *gtaEnv) environ() []string {
	drop := []string{"ANTHROPIC_", "GTA_", "OTEL_", "OPENAI_", "NVIDIA_", "OPENROUTER_", "OLLAMA_",
		"HTTP_PROXY=", "HTTPS_PROXY=", "ALL_PROXY=", "NO_PROXY=", "BRAVE_", "TAVILY_", "SEARXNG_"}
	var env []string
	for _, kv := range os.Environ() {
		keep := true
		for _, p := range drop {
			if strings.HasPrefix(strings.ToUpper(kv), p) {
				keep = false
				break
			}
		}
		if keep {
			env = append(env, kv)
		}
	}
	env = append(env,
		"GTA_CONFIG_DIR="+e.cfgDir,
		"ANTHROPIC_API_KEY="+fakeAPIKey,
		"NO_COLOR=1",
	)
	if e.mock != nil {
		env = append(env, "ANTHROPIC_BASE_URL="+e.mock.URL())
	}
	return append(env, e.extra...)
}

// command builds a gta command in the workspace.
func (e *gtaEnv) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, gtaBin, args...)
	cmd.Dir = e.root
	cmd.Env = e.environ()
	cmd.Stdin = strings.NewReader("") // never a terminal
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// runResult is a finished gta process.
type runResult struct {
	stdout, stderr string
	code           int
}

// run runs gta to completion.
func (e *gtaEnv) run(args ...string) runResult {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	cmd := e.command(ctx, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	res := runResult{stdout: out.String(), stderr: errb.String()}
	var ee *exec.ExitError
	switch {
	case ctx.Err() != nil:
		e.t.Fatalf("gta %s timed out after %s\nstderr:\n%s", strings.Join(args, " "), runTimeout, res.stderr)
	case errors.As(err, &ee):
		res.code = ee.ExitCode()
	case err != nil:
		e.t.Fatalf("running gta: %v", err)
	}
	return res
}

// headlessResult mirrors the JSON document of `gta -p ... --json`.
type headlessResult struct {
	Result     string   `json:"result"`
	IsError    bool     `json:"is_error"`
	Error      string   `json:"error"`
	SessionID  string   `json:"session_id"`
	Model      string   `json:"model"`
	DurationMS int64    `json:"duration_ms"`
	Turns      int      `json:"turns"`
	ToolCalls  []string `json:"tool_calls"`
	CostUSD    float64  `json:"cost_usd"`
	Usage      struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

// headless runs `gta [flags] -p prompt --json`, expects exit code 0 and
// decodes the result.
func (e *gtaEnv) headless(prompt string, flags ...string) (headlessResult, runResult) {
	e.t.Helper()
	args := append(append([]string{}, flags...), "-p", prompt, "--json")
	res := e.run(args...)
	if res.code != 0 {
		e.t.Fatalf("gta %s: exit code %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), res.code, res.stdout, res.stderr)
	}
	var hr headlessResult
	if err := json.Unmarshal([]byte(res.stdout), &hr); err != nil {
		e.t.Fatalf("stdout is not one JSON result: %v\nstdout:\n%s\nstderr:\n%s", err, res.stdout, res.stderr)
	}
	return hr, res
}

// requireUnixShell skips tests whose scripted commands need a POSIX shell.
func requireUnixShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a Unix shell")
	}
}

// exists reports whether path (relative to the workspace) exists.
func (e *gtaEnv) exists(path string) bool {
	_, err := os.Stat(filepath.Join(e.root, path))
	return err == nil
}
