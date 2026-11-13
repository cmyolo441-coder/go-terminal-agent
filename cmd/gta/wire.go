package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/agent"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/config"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/hooks"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm/anthropic"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm/openai"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/mcp"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permission"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/prompt"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/sandbox"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/subagent"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

// nonClaudeMaxTokens is the default output cap for OpenAI-compatible
// providers: many of their models reject Claude's larger default.
const nonClaudeMaxTokens = 8192

// isAnthropic reports whether cfg talks to the Anthropic API.
func isAnthropic(cfg config.Config) bool {
	return cfg.Provider == "" || cfg.Provider == "anthropic"
}

// buildProvider returns the configured model provider and, when credentials
// look missing, a hint for the banner. It also fixes up Claude-specific
// defaults (model id, max_tokens) that would break other providers.
func buildProvider(cfg *config.Config, modelSet, maxTokensSet bool) (llm.Provider, string, error) {
	if isAnthropic(*cfg) {
		p := anthropic.New(anthropic.Options{
			BaseURL:    cfg.BaseURL,
			WebSearch:  cfg.WebSearch,
			WebFetch:   cfg.WebFetch,
			Compaction: cfg.Compaction,
			Fallbacks:  cfg.Fallbacks,
		})
		return p, authHint(), nil
	}
	opts, err := openai.Preset(cfg.Provider)
	if err != nil {
		return nil, "", err
	}
	if cfg.BaseURL != "" {
		opts.BaseURL = cfg.BaseURL
	}
	if !modelSet && strings.HasPrefix(cfg.Model, "claude-") {
		known := openai.KnownModels(cfg.Provider)
		if len(known) == 0 {
			return nil, "", fmt.Errorf("provider %s needs a model: set \"model\" in config or pass --model", cfg.Provider)
		}
		cfg.Model = known[0]
	}
	if !maxTokensSet && cfg.MaxTokens == config.Default().MaxTokens {
		cfg.MaxTokens = nonClaudeMaxTokens
	}
	p, err := openai.New(opts)
	if err != nil {
		return nil, "", err
	}
	hint := ""
	if err := openai.MissingKeyError(opts); err != nil {
		hint = err.Error()
	}
	return p, hint, nil
}

// buildSandbox installs the shell sandbox on env. It fails closed: when a
// sandbox is configured but cannot run here, gta refuses to start rather
// than run commands unprotected.
func buildSandbox(cfg config.Config, env *tools.Env) (*sandbox.Policy, error) {
	mode, err := sandbox.ParseMode(cfg.Sandbox)
	if err != nil {
		return nil, err
	}
	policy := sandbox.Policy{Mode: mode, AllowNetwork: cfg.SandboxNetwork, Root: env.Root}
	if mode == sandbox.ModeOff {
		return &policy, nil
	}
	if _, err := sandbox.Detect(policy); err != nil {
		return nil, fmt.Errorf("sandbox %q is configured but unavailable: %w (set \"sandbox\": \"off\" to run without it)", mode, err)
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	env.Wrap = func(shell, command string) ([]string, error) {
		return sandbox.Wrap(policy, shell, command)
	}
	return &policy, nil
}

// buildHooks returns the hook runner for cfg, or nil when no hooks are set.
func buildHooks(cfg config.Config, root, sessionID string) (*hooks.Runner, error) {
	if cfg.Hooks.Count() == 0 {
		return nil, nil
	}
	return hooks.NewRunner(cfg.Hooks, hooks.Options{Root: root, SessionID: sessionID})
}

// addTaskTool registers the sub-agent "task" tool on ag. Sub-agent approvals,
// progress and usage go through whatever sink the parent turn is using.
func addTaskTool(ag *agent.Agent, cfg config.Config, root string) {
	base := ag.Tools
	t := subagent.New(subagent.Config{
		Provider:      ag.Provider,
		Settings:      ag.Settings,
		Model:         cfg.SubagentModel,
		Tools:         base,
		AllowCommands: cfg.AllowCommands,
		ParentMode:    ag.Gate.Mode,
		Instructions:  prompt.Memory(root),
		Approve: func(ctx context.Context, call *tools.Call, reason string) permission.Decision {
			if s := ag.ActiveSink(); s != nil {
				return s.Approve(ctx, call, reason)
			}
			return permission.DecisionDeny
		},
		Progress: func(index int, description, event string) {
			if ps, ok := ag.ActiveSink().(agent.ProgressSink); ok {
				ps.SubagentProgress(index, description, event)
			}
		},
		OnUsage: ag.AddUsage,
	})
	all := []tools.Tool{}
	for _, name := range base.Names() {
		tl, _ := base.Get(name)
		all = append(all, tl)
	}
	ag.Tools = tools.NewRegistry(append(all, t)...)
}

// sessionStartContext runs session_start hooks and returns text to add to
// the system prompt.
func sessionStartContext(r *hooks.Runner) (string, []string) {
	if !r.Has(hooks.SessionStart) {
		return "", nil
	}
	res, err := r.SessionStart(context.Background())
	if err != nil {
		return "", nil
	}
	warn := append([]string{}, res.Warnings...)
	return res.ContextText(), warn
}

// authHint explains how to authenticate when no credentials are visible.
// The SDK also reads `ant auth login` profiles, so this is only a hint.
func authHint() string {
	if os.Getenv("ANTHROPIC_API_KEY") != "" || os.Getenv("ANTHROPIC_AUTH_TOKEN") != "" {
		return ""
	}
	home, _ := os.UserHomeDir()
	if _, err := os.Stat(filepath.Join(home, ".config", "anthropic")); err == nil {
		return ""
	}
	return "No ANTHROPIC_API_KEY found. Export one, or run `ant auth login`, before sending a prompt."
}

// startMCP connects the configured MCP servers and adds their tools to ag,
// rebuilding the registry whenever a server's tool list changes. Startup
// waits at most mcpStartTimeout; slower servers keep connecting and their
// tools appear when ready.
func startMCP(ag *agent.Agent, cfg config.Config, root string, uiStarted *atomic.Bool) *mcp.Manager {
	if len(cfg.MCPServers) == 0 {
		return nil
	}
	base := ag.Registry()
	builtins := make([]tools.Tool, 0, len(base.Names()))
	for _, name := range base.Names() {
		t, _ := base.Get(name)
		builtins = append(builtins, t)
	}
	var m *mcp.Manager
	rebuild := func() {
		ag.SetRegistry(tools.NewRegistry(append(append([]tools.Tool{}, builtins...), m.Tools()...)...))
	}
	m = mcp.NewManager(mcp.Options{
		Dir: root,
		Logf: func(format string, args ...any) {
			msg := fmt.Sprintf(format, args...)
			if !uiStarted.Load() {
				fmt.Fprintln(os.Stderr, "gta: mcp:", msg)
			} else if s := ag.ActiveSink(); s != nil {
				s.Notice("MCP: " + msg)
			}
		},
		OnChange: func() {
			if m != nil {
				rebuild()
			}
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), mcpStartTimeout)
	defer cancel()
	if err := m.Start(ctx, cfg.MCPServers); err != nil {
		fmt.Fprintln(os.Stderr, "gta: mcp:", err)
	}
	rebuild()
	for _, st := range m.Status() {
		if st.State == mcp.StateFailed {
			fmt.Fprintf(os.Stderr, "gta: mcp server %s failed: %v (see /mcp)\n", st.Name, st.Err)
		}
	}
	return m
}

const mcpStartTimeout = 30 * time.Second
