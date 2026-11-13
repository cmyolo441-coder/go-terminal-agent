package autopilot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

// VerifyToolName is the tool name exposed to the model.
const VerifyToolName = "verify"

// maxVerifyTimeout caps the timeout the model may ask for.
const maxVerifyTimeout = 30 * time.Minute

// VerifyToolConfig configures the verify tool. The zero value detects the
// test command per workspace.
type VerifyToolConfig struct {
	// Command overrides detection.
	Command *TestCommand
	// Timeout is the default per-run timeout (default 10 minutes).
	Timeout time.Duration
	// Budget caps the failure report in bytes (default 6000).
	Budget int
}

// VerifyTool lets the model run the project's test command and get a
// structured failure report. Calls are KindExec with the test command as
// their command, so the permission gate's allow-list ("go test", "npm
// test") and "allow for this session" apply as for the shell tool.
type VerifyTool struct {
	cfg VerifyToolConfig

	mu       sync.Mutex
	detected map[string]TestCommand
}

var _ tools.Tool = (*VerifyTool)(nil)

// NewVerifyTool builds the verify tool.
func NewVerifyTool(cfg VerifyToolConfig) *VerifyTool {
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTestTimeout
	}
	if cfg.Budget <= 0 {
		cfg.Budget = DefaultFailureBudget
	}
	return &VerifyTool{cfg: cfg, detected: map[string]TestCommand{}}
}

// Kind reports KindExec: the tool runs a command.
func (t *VerifyTool) Kind() tools.Kind { return tools.KindExec }

// Spec describes the tool to the model.
func (t *VerifyTool) Spec() llm.ToolSpec {
	desc := "Run the project's test suite (detected automatically: go test, npm/pnpm/yarn test, pytest, cargo test, Maven, Gradle or make test) " +
		"and get a structured report: pass/fail, then each failure with file:line, test name and message, most relevant first (build errors before test failures). " +
		"Use it after changing code to verify your work, and to see exactly what fails. Pass \"filter\" to run only matching tests while iterating, then run without it to confirm everything passes."
	if t.cfg.Command != nil {
		desc += fmt.Sprintf(" The test command is `%s`.", t.cfg.Command.Command)
	}
	return llm.ToolSpec{
		Name:        VerifyToolName,
		Description: desc,
		Properties: map[string]any{
			"filter": map[string]any{
				"type":        "string",
				"description": "Optional test name pattern: a -run regexp for Go, a -k expression for pytest, a -t name pattern for Jest/Vitest, a name filter for cargo, a test class/method for Maven and Gradle.",
			},
			"timeout_sec": map[string]any{"type": "integer", "description": "Timeout in seconds (default 600, max 1800)."},
		},
	}
}

type verifyInput struct {
	Filter     string `json:"filter"`
	TimeoutSec int    `json:"timeout_sec"`
}

// Prepare validates input and builds the test call.
func (t *VerifyTool) Prepare(env *tools.Env, raw json.RawMessage) (*tools.Call, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var in verifyInput
	if err := dec.Decode(&in); err != nil {
		return nil, fmt.Errorf("invalid tool input: %w", err)
	}
	if dec.More() {
		return nil, errors.New("invalid tool input: trailing data")
	}
	if env == nil || env.Root == "" {
		return nil, errors.New("verify is not available: no workspace environment")
	}
	if in.TimeoutSec < 0 {
		return nil, errors.New("invalid tool input: timeout_sec must not be negative")
	}
	tc, err := t.command(env.Root)
	if err != nil {
		return nil, err
	}
	filter := strings.TrimSpace(in.Filter)
	if filter != "" {
		if strings.ContainsAny(filter, "\n\r\x00") || utf8.RuneCountInString(filter) > 200 || !utf8.ValidString(filter) {
			return nil, errors.New("invalid tool input: filter must be a single line of at most 200 characters")
		}
		cmd, err := withFilter(tc, filter)
		if err != nil {
			return nil, err
		}
		tc.Command = cmd
	}
	timeout := t.cfg.Timeout
	if in.TimeoutSec > 0 {
		timeout = min(time.Duration(in.TimeoutSec)*time.Second, maxVerifyTimeout)
	}
	summary := "Verify: " + tc.String()
	budget := t.cfg.Budget
	wrap, check := env.Wrap, env.CheckCommand
	root := env.Root
	run := func(ctx context.Context) (string, error) {
		tr, err := RunTests(ctx, root, tc, RunOptions{Timeout: timeout, Wrap: wrap, Check: check})
		if err != nil {
			if tr != nil && ctx.Err() == nil {
				return tr.Report(budget), err
			}
			return "", err
		}
		out := tr.Report(budget)
		if !tr.Passed() {
			return out, fmt.Errorf("%d failure%s", len(tr.Failures), plural(len(tr.Failures)))
		}
		return out, nil
	}
	return tools.NewCall(VerifyToolName, tools.KindExec, summary, tc.Command, tc.Command, run), nil
}

// command returns the configured or detected test command for root.
func (t *VerifyTool) command(root string) (TestCommand, error) {
	if t.cfg.Command != nil && strings.TrimSpace(t.cfg.Command.Command) != "" {
		tc := *t.cfg.Command
		if tc.Framework == "" {
			tc.Framework = GuessFramework(tc.Command, root)
		}
		return tc, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if tc, ok := t.detected[root]; ok {
		return tc, nil
	}
	tc, err := DetectTestCommand(root)
	if err != nil {
		return TestCommand{}, fmt.Errorf("verify: %w", err)
	}
	t.detected[root] = tc
	return tc, nil
}

// withFilter adds a test-name filter to a command in the runner's syntax.
func withFilter(tc TestCommand, filter string) (string, error) {
	q := shellQuote(filter)
	cmd := strings.TrimSpace(tc.Command)
	isScript := false
	for _, pm := range []string{"npm", "pnpm", "yarn", "bun"} {
		if strings.HasPrefix(cmd, pm+" ") {
			isScript = true
		}
	}
	switch tc.Framework {
	case FrameworkGo:
		if rest, ok := strings.CutPrefix(cmd, "go test"); ok {
			return "go test -run " + q + rest, nil
		}
	case FrameworkPytest:
		return cmd + " -k " + q, nil
	case FrameworkJest, FrameworkVitest:
		if isScript {
			return cmd + " -- -t " + q, nil
		}
		return cmd + " -t " + q, nil
	case FrameworkCargo:
		return cmd + " " + q, nil
	case FrameworkMaven:
		return cmd + " -Dtest=" + q, nil
	case FrameworkGradle:
		return cmd + " --tests " + q, nil
	}
	return "", fmt.Errorf("verify: a filter is not supported for `%s`; run it without a filter", cmd)
}

// shellQuote quotes s for the platform shell.
func shellQuote(s string) string {
	if runtime.GOOS == "windows" {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
