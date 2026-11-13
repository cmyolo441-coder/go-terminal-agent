package config

import (
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
)

// A repository's .gta/config.json is written by whoever controls the
// repository. Unless the user config sets trust_project_hooks, Load applies
// it under one rule (restrictUntrusted):
//
//   - preferences that cannot run code, send data anywhere or weaken a
//     protection (projectPreferences) apply as written;
//   - security settings (projectTighteners) apply only when they are at
//     least as strict as the user's own setting;
//   - every other key keeps the user's value, and is named in
//     IgnoredProjectSettings when the project tried to change it.
//
// The lists are allow lists: a key added to Config later is ignored from
// an untrusted project until it is classified here (a test enforces that
// every key is). hooks, mcp_servers and trust_project_hooks are handled by
// Load itself.

// projectPreferences are keys an untrusted project file may set freely.
var projectPreferences = map[string]bool{
	"model": true, "effort": true, "max_tokens": true, "show_thinking": true,
	"compaction": true, "fallbacks": true, "max_turns": true, "shell_timeout_sec": true,
	"theme": true, "density": true, "subagent_model": true, "routing": true,
	"code_index": true, "code_index_max_file_size": true, "code_index_ignore": true,
	"repo_map_budget": true, "lsp_diagnostics_wait_ms": true,
}

// projectTighteners are security keys an untrusted project file may only
// make stricter. Each sets dst's value to the project's p when it is at
// least as strict, and reports false when it was (wholly or partly) not
// applied. An unchanged value always applies.
var projectTighteners = map[string]func(dst *Config, p Config) bool{
	"permission_mode": func(d *Config, p Config) bool {
		r, ok := modeRank[p.PermissionMode]
		if !ok || r > modeRank[d.PermissionMode] {
			return false
		}
		d.PermissionMode = p.PermissionMode
		return true
	},
	"allow_commands": func(d *Config, p Config) bool {
		kept := []string{}
		for _, c := range p.AllowCommands {
			if slices.Contains(d.AllowCommands, c) {
				kept = append(kept, c)
			}
		}
		d.AllowCommands = kept
		return len(kept) == len(p.AllowCommands)
	},
	"sandbox": func(d *Config, p Config) bool {
		if sandboxRank(p.Sandbox) < 0 || sandboxRank(p.Sandbox) < sandboxRank(d.Sandbox) {
			return false
		}
		d.Sandbox = p.Sandbox
		return true
	},
	"sandbox_network":  offOnly(func(c *Config) *bool { return &c.SandboxNetwork }),
	"web_search":       offOnly(func(c *Config) *bool { return &c.WebSearch }),
	"web_fetch":        offOnly(func(c *Config) *bool { return &c.WebFetch }),
	"client_web_tools": offOnly(func(c *Config) *bool { return &c.ClientWebTools }),
	"lsp":              offOnly(func(c *Config) *bool { return &c.LSP }),
	"redact_secrets":   onOnly(func(c *Config) *bool { return &c.RedactSecrets }),
	"audit_log":        onOnly(func(c *Config) *bool { return &c.AuditLog }),
	"disable_plugins":  onOnly(func(c *Config) *bool { return &c.DisablePlugins }),
	"disabled_plugins": func(d *Config, p Config) bool {
		all := slices.Clone(d.DisabledPlugins)
		for _, n := range p.DisabledPlugins {
			if !slices.Contains(all, n) {
				all = append(all, n)
			}
		}
		// The user's entries always stay, so this only ever adds.
		d.DisabledPlugins = all
		return true
	},
	"audit_sync": func(d *Config, p Config) bool {
		r, ok := syncRank[p.AuditSync]
		if !ok || r < syncRank[d.AuditSync] {
			return false
		}
		d.AuditSync = p.AuditSync
		return true
	},
	"budget": func(d *Config, p Config) bool {
		t := d.Budget.Tighten(p.Budget)
		d.Budget = t
		return t == p.Budget
	},
}

// projectIgnored are keys an untrusted project file can never set: where
// the conversation, API key or telemetry go, programs GTA starts, what is
// redacted and where the audit log is written. Listed so that every key is
// classified; unlisted keys are ignored too.
var projectIgnored = []string{
	"provider", "base_url", "fallback_models", "telemetry", "lsp_servers",
	"secrets_allow", "audit_dir",
}

// projectByLoad are handled by Load: project hooks and MCP servers are
// dropped and reported through IgnoredProjectHooks, and trust comes from
// the user file only.
var projectByLoad = []string{"hooks", "mcp_servers", "trust_project_hooks"}

var (
	modeRank = map[string]int{"plan": 0, "ask": 1, "auto-edit": 2, "yolo": 3}
	syncRank = map[string]int{"never": 0, "interval": 1, "always": 2, "": 2}
)

// offOnly lets a project turn a boolean off but not on.
func offOnly(f func(*Config) *bool) func(*Config, Config) bool {
	return func(d *Config, p Config) bool {
		v := *f(&p)
		if v && !*f(d) {
			return false
		}
		*f(d) = v
		return true
	}
}

// onOnly lets a project turn a boolean on but not off.
func onOnly(f func(*Config) *bool) func(*Config, Config) bool {
	return func(d *Config, p Config) bool {
		v := *f(&p)
		if !v && *f(d) {
			return false
		}
		*f(d) = v
		return true
	}
}

// jsonKey is a struct field's JSON key, or "" when it has none.
func jsonKey(f reflect.StructField) string {
	k, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	if k == "-" {
		return ""
	}
	return k
}

// restrictUntrusted applies the untrusted-project rule. cfg is the user
// configuration with the project file decoded on top, user a deep copy of
// the configuration before the project file. cfg is reset to user, then
// preferences and tightenings are taken from the project. It returns the
// keys the project set that were ignored or only partly applied, sorted.
func restrictUntrusted(cfg, user *Config) []string {
	proj := *cfg
	out := *user
	out.Hooks, out.MCPServers, out.TrustProjectHooks = proj.Hooks, proj.MCPServers, proj.TrustProjectHooks
	var ignored []string
	ov, pv, uv := reflect.ValueOf(&out).Elem(), reflect.ValueOf(proj), reflect.ValueOf(*user)
	ty := ov.Type()
	for i := 0; i < ty.NumField(); i++ {
		key := jsonKey(ty.Field(i))
		switch {
		case key == "" || slices.Contains(projectByLoad, key):
		case projectPreferences[key]:
			ov.Field(i).Set(pv.Field(i))
		case projectTighteners[key] != nil:
			if !projectTighteners[key](&out, proj) {
				ignored = append(ignored, key)
			}
		default:
			if !sameJSON(pv.Field(i).Interface(), uv.Field(i).Interface()) {
				ignored = append(ignored, key)
			}
		}
	}
	*cfg = out
	sort.Strings(ignored)
	return ignored
}

// IgnoredProjectNotice is the startup notice for untrusted project
// settings that were not applied, or "" when there are none.
func (c Config) IgnoredProjectNotice() string {
	if len(c.IgnoredProjectSettings) == 0 {
		return ""
	}
	return fmt.Sprintf("this project is not trusted, so these settings in .gta/config.json were ignored: %s (an untrusted project may only set preferences and tighten security settings; set \"trust_project_hooks\": true in your user config to allow them)",
		strings.Join(c.IgnoredProjectSettings, ", "))
}
