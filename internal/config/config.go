// Package config loads settings, project memory and custom slash commands.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	AppName    = "gocode"
	Version    = "1.0.0"
	DirName    = ".gocode"
	MemoryFile = "AGENTS.md"
)

// MemoryNames are the project instruction files read into the system prompt.
var MemoryNames = []string{"AGENTS.md", "CLAUDE.md"}

// Hook is one shell command run for a lifecycle event.
type Hook struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"` // seconds
}

// HookMatcher selects which tools a group of hooks applies to.
type HookMatcher struct {
	Matcher string `json:"matcher,omitempty"`
	Hooks   []Hook `json:"hooks"`
}

// MCPServer describes a stdio MCP server to launch.
type MCPServer struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// Permissions holds the rule lists from settings.
type Permissions struct {
	Allow       []string `json:"allow,omitempty"`
	Deny        []string `json:"deny,omitempty"`
	Ask         []string `json:"ask,omitempty"`
	DefaultMode string   `json:"defaultMode,omitempty"`
}

// Settings is the merged view of user, project and local settings files.
type Settings struct {
	Model           string                   `json:"model,omitempty"`
	Effort          string                   `json:"effort,omitempty"`
	Permissions     Permissions              `json:"permissions,omitempty"`
	Hooks           map[string][]HookMatcher `json:"hooks,omitempty"`
	MCPServers      map[string]MCPServer     `json:"mcpServers,omitempty"`
	Env             map[string]string        `json:"env,omitempty"`
	WebSearch       *bool                    `json:"webSearch,omitempty"`
	RefusalFallback *bool                    `json:"refusalFallback,omitempty"`
	AutoCompact     *bool                    `json:"autoCompact,omitempty"`
	MaxTokens       int64                    `json:"maxTokens,omitempty"`
}

// Home returns the per-user state directory.
func Home() string {
	if h := os.Getenv("GOCODE_HOME"); h != "" {
		return h
	}
	h, err := os.UserHomeDir()
	if err != nil {
		h = "."
	}
	return filepath.Join(h, DirName)
}

// SettingsPaths lists settings files from lowest to highest precedence.
func SettingsPaths(cwd string) []string {
	return []string{
		filepath.Join(Home(), "settings.json"),
		filepath.Join(cwd, DirName, "settings.json"),
		filepath.Join(cwd, DirName, "settings.local.json"),
	}
}

// Load merges every settings file that exists. Malformed files are reported
// but do not stop the load.
func Load(cwd string) (*Settings, []error) {
	out := &Settings{Hooks: map[string][]HookMatcher{}, MCPServers: map[string]MCPServer{}, Env: map[string]string{}}
	var errs []error
	for _, p := range SettingsPaths(cwd) {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var s Settings
		if err := json.Unmarshal(b, &s); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
			continue
		}
		out.merge(&s)
	}
	// .mcp.json at the project root, same shape as other agent CLIs use.
	if b, err := os.ReadFile(filepath.Join(cwd, ".mcp.json")); err == nil {
		var f struct {
			MCPServers map[string]MCPServer `json:"mcpServers"`
		}
		if err := json.Unmarshal(b, &f); err != nil {
			errs = append(errs, fmt.Errorf(".mcp.json: %w", err))
		}
		for k, v := range f.MCPServers {
			out.MCPServers[k] = v
		}
	}
	return out, errs
}

func (s *Settings) merge(o *Settings) {
	if o.Model != "" {
		s.Model = o.Model
	}
	if o.Effort != "" {
		s.Effort = o.Effort
	}
	if o.MaxTokens != 0 {
		s.MaxTokens = o.MaxTokens
	}
	if o.Permissions.DefaultMode != "" {
		s.Permissions.DefaultMode = o.Permissions.DefaultMode
	}
	s.Permissions.Allow = append(s.Permissions.Allow, o.Permissions.Allow...)
	s.Permissions.Deny = append(s.Permissions.Deny, o.Permissions.Deny...)
	s.Permissions.Ask = append(s.Permissions.Ask, o.Permissions.Ask...)
	for k, v := range o.Hooks {
		s.Hooks[k] = append(s.Hooks[k], v...)
	}
	for k, v := range o.MCPServers {
		s.MCPServers[k] = v
	}
	for k, v := range o.Env {
		s.Env[k] = v
	}
	if o.WebSearch != nil {
		s.WebSearch = o.WebSearch
	}
	if o.RefusalFallback != nil {
		s.RefusalFallback = o.RefusalFallback
	}
	if o.AutoCompact != nil {
		s.AutoCompact = o.AutoCompact
	}
}

// AddLocalAllowRule appends a permission rule to .gocode/settings.local.json.
func AddLocalAllowRule(cwd, rule string) error {
	p := filepath.Join(cwd, DirName, "settings.local.json")
	raw := map[string]any{}
	if b, err := os.ReadFile(p); err == nil {
		if err := json.Unmarshal(b, &raw); err != nil {
			return fmt.Errorf("%s is not valid JSON: %w", p, err)
		}
	}
	perms, _ := raw["permissions"].(map[string]any)
	if perms == nil {
		perms = map[string]any{}
	}
	allow, _ := perms["allow"].([]any)
	for _, a := range allow {
		if a == rule {
			return nil
		}
	}
	perms["allow"] = append(allow, rule)
	raw["permissions"] = perms
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // keep && and > readable in saved commands
	enc.SetIndent("", "  ")
	if err := enc.Encode(raw); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, buf.Bytes(), 0o644)
}

// MemorySource is one loaded instruction file.
type MemorySource struct {
	Path    string
	Content string
}

// LoadMemory reads user-level and project instruction files. Project files
// are collected from the filesystem root down to cwd so nearer files come last.
func LoadMemory(cwd string) []MemorySource {
	var out []MemorySource
	add := func(p string) {
		b, err := os.ReadFile(p)
		if err != nil || strings.TrimSpace(string(b)) == "" {
			return
		}
		out = append(out, MemorySource{Path: p, Content: strings.TrimSpace(string(b))})
	}
	add(filepath.Join(Home(), MemoryFile))
	var dirs []string
	for d := cwd; ; d = filepath.Dir(d) {
		dirs = append(dirs, d)
		if filepath.Dir(d) == d {
			break
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		for _, n := range MemoryNames {
			add(filepath.Join(dirs[i], n))
		}
	}
	return out
}

// AppendMemory adds a note to the project memory file.
func AppendMemory(cwd, note string) (string, error) {
	p := filepath.Join(cwd, MemoryFile)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return p, err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "\n- %s\n", strings.TrimSpace(note))
	return p, err
}

// Command is a custom slash command backed by a markdown prompt file.
type Command struct {
	Name        string
	Description string
	Body        string
	Path        string
}

// LoadCommands reads *.md files from the user and project commands directories.
// Project commands override user commands of the same name.
func LoadCommands(cwd string) []Command {
	byName := map[string]Command{}
	for _, dir := range []string{filepath.Join(Home(), "commands"), filepath.Join(cwd, DirName, "commands")} {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			p := filepath.Join(dir, e.Name())
			b, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			c := Command{Name: strings.TrimSuffix(e.Name(), ".md"), Path: p}
			c.Description, c.Body = splitFrontmatter(string(b))
			if c.Description == "" {
				c.Description = "custom command"
			}
			byName[c.Name] = c
		}
	}
	out := make([]Command, 0, len(byName))
	for _, c := range byName {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// splitFrontmatter pulls `description:` out of an optional --- block.
func splitFrontmatter(s string) (desc, body string) {
	if !strings.HasPrefix(s, "---\n") {
		return "", strings.TrimSpace(s)
	}
	end := strings.Index(s[4:], "\n---")
	if end < 0 {
		return "", strings.TrimSpace(s)
	}
	for _, l := range strings.Split(s[4:4+end], "\n") {
		if k, v, ok := strings.Cut(l, ":"); ok && strings.TrimSpace(k) == "description" {
			desc = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	body = s[4+end+4:]
	return desc, strings.TrimSpace(body)
}
