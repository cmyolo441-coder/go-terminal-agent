package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/agent"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/autopilot"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/config"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/mcp"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/plugin"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/version"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/webtools"
)

// toolset assembles an agent's registry from its sources: built-in tools,
// client-side web tools, plugin tools, the sub-agent task tool and MCP
// tools. Plugins and MCP servers can change at runtime, so the registry is
// rebuilt from these parts whenever one does.
type toolset struct {
	mu      sync.Mutex
	ag      *agent.Agent
	core    []tools.Tool
	plugins *plugin.Manager
	mcp     *mcp.Manager
	// extra builds tools that wrap the base set, such as the sub-agent
	// task tool; it runs on every rebuild so they see plugin changes.
	extra func(base *tools.Registry) []tools.Tool
	notes []string // startup notes for the user
}

// newToolset builds the core and plugin tools for cfg.
func newToolset(cfg config.Config, root string) *toolset {
	ts := &toolset{}
	ts.core = append(tools.Default().All(), autopilot.NewVerifyTool(autopilot.VerifyToolConfig{}))
	if webClientSide(cfg) {
		opts := webtools.Options{}
		if os.Getenv("HTTPS_PROXY") != "" || os.Getenv("https_proxy") != "" {
			opts.Fetch.Proxy = http.ProxyFromEnvironment
		}
		wt, err := webtools.Tools(opts)
		ts.core = append(ts.core, wt...)
		if err != nil && !errors.Is(err, webtools.ErrNoSearchBackend) {
			ts.notes = append(ts.notes, "web_search: "+err.Error())
		}
	}
	if !cfg.DisablePlugins {
		reserved := make([]string, 0, len(ts.core))
		for _, t := range ts.core {
			reserved = append(reserved, t.Spec().Name)
		}
		reserved = append(reserved, "task")
		ts.plugins = plugin.NewManager(plugin.Options{
			UserDir:          filepath.Join(config.UserDir(), "plugins"),
			ProjectDir:       filepath.Join(root, ".gta", "plugins"),
			TrustProject:     cfg.TrustProjectHooks,
			Workspace:        root,
			ReservedCommands: reservedCommands(),
			ReservedTools:    reserved,
			Disabled:         cfg.DisabledPlugins,
			GTAVersion:       version.Version,
		})
		if err := ts.plugins.Load(); err != nil {
			ts.notes = append(ts.notes, "plugins: "+err.Error())
		}
		ts.notes = append(ts.notes, ts.plugins.Warnings()...)
		if u := ts.plugins.Untrusted(); len(u) > 0 {
			ts.notes = append(ts.notes, fmt.Sprintf("plugins: %d project plugin(s) ignored; set \"trust_project_hooks\": true in your user config to load them", len(u)))
		}
	}
	return ts
}

// webClientSide reports whether GTA should run its own web tools: always
// for non-Anthropic providers (which have no server-side web tools), and
// for Anthropic only when explicitly asked.
func webClientSide(cfg config.Config) bool {
	return !isAnthropic(cfg) || cfg.ClientWebTools
}

// base returns the tools sub-agents get: core and plugin tools.
func (ts *toolset) base() *tools.Registry {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.baseLocked()
}

func (ts *toolset) baseLocked() *tools.Registry {
	all := append([]tools.Tool{}, ts.core...)
	if ts.plugins != nil {
		all = append(all, ts.plugins.Tools()...)
	}
	return tools.NewRegistry(all...)
}

// full returns every tool: base, extra and MCP.
func (ts *toolset) full() *tools.Registry {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	base := ts.baseLocked()
	all := base.All()
	if ts.extra != nil {
		all = append(all, ts.extra(base)...)
	}
	if ts.mcp != nil {
		all = append(all, ts.mcp.Tools()...)
	}
	return tools.NewRegistry(all...)
}

// forAgent returns a toolset for another agent that shares this one's
// core tools, plugins and MCP manager, as it stands now.
func (ts *toolset) forAgent() *toolset {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return &toolset{core: ts.core, plugins: ts.plugins, mcp: ts.mcp}
}

// setMCP attaches the MCP manager whose tools join the registry.
func (ts *toolset) setMCP(m *mcp.Manager) {
	ts.mu.Lock()
	ts.mcp = m
	ts.mu.Unlock()
}

// rebuild installs the full registry on the agent.
func (ts *toolset) rebuild() {
	if ts.ag != nil {
		ts.ag.SetRegistry(ts.full())
	}
}

// reservedCommands are names plugin commands may not take.
func reservedCommands() []string {
	return []string{
		"help", "status", "clear", "new", "reset", "model", "effort", "mode", "thinking", "cost", "usage",
		"tasks", "todos", "compact", "sessions", "resume", "init", "memory", "tools", "config", "exit", "quit", "q",
		"diff", "commit", "log", "undo", "checkpoints", "worktree", "sandbox", "hooks", "mcp", "provider", "plugins",
		"plan", "fixtests", "index", "audit", "budget", "policy",
	}
}
