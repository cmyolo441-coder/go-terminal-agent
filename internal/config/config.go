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
	for _, p := range []string{UserFile(), ProjectFile(root)} {
		if err := mergeFile(&cfg, p); err != nil {
			return cfg, err
		}
	}
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
	if v := os.Getenv("ANTHROPIC_BASE_URL"); v != "" && cfg.BaseURL == "" {
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
	switch c.Theme {
	case "auto", "dark", "light":
	default:
		return fmt.Errorf("config: theme %q is not one of auto, dark, light", c.Theme)
	}
	return nil
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
