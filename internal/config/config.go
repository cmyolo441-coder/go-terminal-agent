// Package config loads settings from the user config file, the project
// config file and environment variables, in that order of precedence
// (later wins).
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/hooks"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm/openai"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/mcp"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/sandbox"
)

// DefaultModel is the model used when none is configured.
const DefaultModel = "claude-opus-5-5"

// Config is the full set of settings.
type Config struct {
	Model string `json:"model"`
	// Effort is low, medium, high, xhigh or max.
	Effort         string   `json:"effort"`
	MaxTokens      int64    `json:"max_tokens"`
	PermissionMode string   `json:"permission_mode"`
	AllowCommands  []string `json:"allow_commands"`
	ShowThinking   bool     `json:"show_thinking"`
	WebSearch      bool     `json:"web_search"`
	WebFetch       bool     `json:"web_fetch"`
	Compaction     bool     `json:"compaction"`
	Fallbacks      bool     `json:"fallbacks"`
	MaxTurns       int      `json:"max_turns"`
	ShellTimeout   int      `json:"shell_timeout_sec"`
	BaseURL        string   `json:"base_url"`
	// Theme is "auto", "dark" or "light"; it picks the markdown style.
	Theme string `json:"theme"`

	// Provider is "anthropic" (default) or an OpenAI-compatible preset such
	// as openai, openrouter, ollama, gemini, groq, deepseek, together,
	// lmstudio, vllm or nvidia. Keys always come from environment variables.
	Provider string `json:"provider"`
	// SubagentModel is the model the task tool's sub-agents use; empty means
	// the main model.
	SubagentModel string `json:"subagent_model"`
	// Sandbox is off, workspace-write or read-only and applies to the bash
	// tool. SandboxNetwork keeps network access inside the sandbox.
	Sandbox        string `json:"sandbox"`
	SandboxNetwork bool   `json:"sandbox_network"`
	// Hooks maps hook events to commands. Hooks from a project config file
	// run only when the user config sets trust_project_hooks, because a
	// cloned repository should not be able to run commands on its own.
	Hooks             hooks.Config `json:"hooks,omitempty"`
	TrustProjectHooks bool         `json:"trust_project_hooks"`

	// MCPServers are Model Context Protocol servers whose tools GTA can
	// use. Like hooks, servers from a project file need trust_project_hooks.
	MCPServers mcp.Config `json:"mcp_servers,omitempty"`

	// DisablePlugins turns the plugin system off; DisabledPlugins skips
	// individual plugins by name. Project plugins need trust_project_hooks.
	DisablePlugins  bool     `json:"disable_plugins"`
	DisabledPlugins []string `json:"disabled_plugins,omitempty"`
	// ClientWebTools adds GTA's own web_fetch/web_search tools for Claude
	// too (they are always on for other providers). web_search needs
	// BRAVE_API_KEY, TAVILY_API_KEY or SEARXNG_URL.
	ClientWebTools bool `json:"client_web_tools"`

	// IgnoredProjectHooks is set by Load when the project file defines hooks
	// that were skipped because the project is not trusted.
	IgnoredProjectHooks bool `json:"-"`
}

// Default returns the built-in settings.
func Default() Config {
	return Config{
		Model:          DefaultModel,
		Effort:         "high",
		MaxTokens:      64000,
		PermissionMode: "ask",
		AllowCommands: []string{
			"ls", "pwd", "git status", "git diff", "git log", "git show", "git branch",
			"go build", "go test", "go vet", "go version", "cat", "head", "tail", "wc",
		},
		ShowThinking: true,
		Fallbacks:    true,
		MaxTurns:     200,
		ShellTimeout: 120,
		Theme:        "auto",
		Provider:     "anthropic",
		Sandbox:      "off",
	}
}

// Efforts lists valid effort levels.
var Efforts = []string{"low", "medium", "high", "xhigh", "max"}

// ValidEffort reports whether s is a valid effort level.
func ValidEffort(s string) bool {
	for _, e := range Efforts {
		if e == s {
			return true
		}
	}
	return false
}

// UserDir is the per-user config directory (~/.config/gta).
func UserDir() string {
	if d := os.Getenv("GTA_CONFIG_DIR"); d != "" {
		return d
	}
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "gta")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".gta")
}

// UserFile is the path of the user config file.
func UserFile() string { return filepath.Join(UserDir(), "config.json") }

// ProjectFile is the path of the project config file under root.
func ProjectFile(root string) string { return filepath.Join(root, ".gta", "config.json") }

// Load merges defaults, the user file, the project file and the environment.
func Load(root string) (Config, error) {
	cfg := Default()
	if err := mergeFile(&cfg, UserFile()); err != nil {
		return cfg, err
	}
	userHooks, trust := cfg.Hooks, cfg.TrustProjectHooks
	userMCP := cfg.MCPServers
	cfg.Hooks = nil
	cfg.MCPServers = nil
	if err := mergeFile(&cfg, ProjectFile(root)); err != nil {
		return cfg, err
	}
	// The project file may not grant itself trust or replace user hooks.
	projectHooks := cfg.Hooks
	cfg.TrustProjectHooks = trust
	cfg.Hooks = hooks.Config{}
	for ev, hs := range userHooks {
		cfg.Hooks[ev] = append(cfg.Hooks[ev], hs...)
	}
	if trust {
		for ev, hs := range projectHooks {
			cfg.Hooks[ev] = append(cfg.Hooks[ev], hs...)
		}
	}
	projectMCP := cfg.MCPServers
	cfg.MCPServers = mcp.Config{}
	for name, sc := range userMCP {
		cfg.MCPServers[name] = sc
	}
	if trust {
		for name, sc := range projectMCP {
			cfg.MCPServers[name] = sc
		}
	}
	cfg.IgnoredProjectHooks = !trust && (projectHooks.Count() > 0 || len(projectMCP) > 0)
	applyEnv(&cfg)
	return cfg, cfg.Validate()
}

func mergeFile(cfg *Config, path string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	// Decoding onto the existing struct overrides only the keys present.
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("GTA_MODEL"); v != "" {
		cfg.Model = v
	}
	if v := os.Getenv("GTA_EFFORT"); v != "" {
		cfg.Effort = v
	}
	if v := os.Getenv("GTA_PERMISSION_MODE"); v != "" {
		cfg.PermissionMode = v
	}
	if v := os.Getenv("GTA_MAX_TOKENS"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.MaxTokens = n
		}
	}
	if v := os.Getenv("GTA_PROVIDER"); v != "" {
		cfg.Provider = v
	}
	if v := os.Getenv("GTA_SANDBOX"); v != "" {
		cfg.Sandbox = v
	}
	if v := os.Getenv("ANTHROPIC_BASE_URL"); v != "" && cfg.BaseURL == "" && (cfg.Provider == "" || cfg.Provider == "anthropic") {
		cfg.BaseURL = v
	}
}

// Validate checks the settings.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Model) == "" {
		return fmt.Errorf("config: model is empty")
	}
	if c.Effort != "" && !ValidEffort(c.Effort) {
		return fmt.Errorf("config: effort %q is not one of %s", c.Effort, strings.Join(Efforts, ", "))
	}
	if c.MaxTokens < 1024 || c.MaxTokens > 128000 {
		return fmt.Errorf("config: max_tokens must be between 1024 and 128000")
	}
	switch c.PermissionMode {
	case "ask", "auto-edit", "plan", "yolo":
	default:
		return fmt.Errorf("config: permission_mode %q is not one of ask, auto-edit, plan, yolo", c.PermissionMode)
	}
	if c.MaxTurns < 1 {
		return fmt.Errorf("config: max_turns must be at least 1")
	}
	if _, err := sandbox.ParseMode(c.Sandbox); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if err := c.Hooks.Validate(); err != nil {
		return fmt.Errorf("config: hooks: %w", err)
	}
	if err := c.MCPServers.Validate(); err != nil {
		return fmt.Errorf("config: mcp_servers: %w", err)
	}
	if !ValidProvider(c.Provider) {
		return fmt.Errorf("config: provider %q is not one of %s", c.Provider, strings.Join(Providers(), ", "))
	}
	switch c.Theme {
	case "auto", "dark", "light":
	default:
		return fmt.Errorf("config: theme %q is not one of auto, dark, light", c.Theme)
	}
	return nil
}

// Providers lists the accepted provider names.
func Providers() []string { return append([]string{"anthropic"}, openai.PresetNames()...) }

// ValidProvider reports whether name is a known provider.
func ValidProvider(name string) bool {
	if name == "" || name == "anthropic" {
		return true
	}
	_, ok := openai.LookupPreset(name)
	return ok
}

// SaveUser writes the given config to the user file.
func SaveUser(c Config) error {
	if err := os.MkdirAll(UserDir(), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(UserFile(), append(data, '\n'), 0o600)
}
