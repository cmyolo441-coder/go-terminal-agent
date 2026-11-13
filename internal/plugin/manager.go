package plugin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/version"
)

// Options configure a Manager.
type Options struct {
	// UserDir holds the user's plugins, usually <config dir>/plugins.
	UserDir string
	// ProjectDir holds the project's plugins, usually <root>/.gta/plugins.
	// Its plugins load only when TrustProject is set, because they run
	// code that came with the repository.
	ProjectDir   string
	TrustProject bool
	// Workspace is the project root that plugin processes run in.
	Workspace string
	// ReservedCommands are built-in slash command names. A plugin command
	// with one of these names is registered as "plugin:name" instead.
	ReservedCommands []string
	// ReservedTools are tool names already taken (built-in and MCP). A
	// plugin tool whose name collides is skipped with a warning.
	ReservedTools []string
	// Disabled lists plugin names not to load.
	Disabled []string
	// GTAVersion defaults to version.Version.
	GTAVersion string
	// Environ returns the environment plugins inherit from (after
	// scrubbing); nil means os.Environ.
	Environ func() []string
}

// Problem is a plugin that could not be loaded.
type Problem struct {
	Dir    string
	Source Source
	Err    error
}

func (p Problem) Error() string { return fmt.Sprintf("%s plugin %s: %v", p.Source, p.Dir, p.Err) }

func (p Problem) Unwrap() error { return p.Err }

// Manager discovers, loads and reloads plugins. It is safe for concurrent
// use; Reload swaps the whole set atomically.
type Manager struct {
	opts Options

	mu    sync.RWMutex
	state *state
}

type state struct {
	plugins   []*Plugin
	tools     []*Tool
	commands  []*Command
	byCommand map[string]*Command
	problems  []Problem
	warnings  []string
	untrusted []string
	disabled  []string
}

// NewManager builds a manager. Call Load before using it.
func NewManager(o Options) *Manager {
	if o.GTAVersion == "" {
		o.GTAVersion = version.Version
	}
	if o.Environ == nil {
		o.Environ = os.Environ
	}
	return &Manager{opts: o, state: &state{byCommand: map[string]*Command{}}}
}

// Load discovers and loads every plugin. Plugins that fail validation are
// left out and reported in the returned error (and by Problems); the rest
// are usable either way.
func (m *Manager) Load() error { return m.Reload() }

// Reload re-reads every plugin directory and replaces the loaded set.
func (m *Manager) Reload() error {
	st := m.scan()
	m.mu.Lock()
	m.state = st
	m.mu.Unlock()
	if len(st.problems) == 0 {
		return nil
	}
	errs := make([]error, len(st.problems))
	for i, p := range st.problems {
		errs[i] = p
	}
	return errors.Join(errs...)
}

func (m *Manager) scan() *state {
	st := &state{byCommand: map[string]*Command{}}
	disabled := map[string]bool{}
	for _, d := range m.opts.Disabled {
		disabled[strings.TrimSpace(d)] = true
	}
	load := LoadOptions{Workspace: m.opts.Workspace, GTAVersion: m.opts.GTAVersion, Environ: m.opts.Environ}
	byName := map[string]*Plugin{}

	sources := []struct {
		dir    string
		source Source
	}{{m.opts.UserDir, SourceUser}, {m.opts.ProjectDir, SourceProject}}
	for _, src := range sources {
		if src.dir == "" {
			continue
		}
		dirs, err := pluginDirs(src.dir)
		if err != nil {
			st.problems = append(st.problems, Problem{Dir: src.dir, Source: src.source, Err: err})
			continue
		}
		if src.source == SourceProject && !m.opts.TrustProject {
			st.untrusted = append(st.untrusted, dirs...)
			continue
		}
		for _, dir := range dirs {
			lo := load
			lo.Source = src.source
			p, err := LoadDir(dir, lo)
			if err != nil {
				st.problems = append(st.problems, Problem{Dir: dir, Source: src.source, Err: err})
				continue
			}
			if disabled[p.Name()] {
				st.disabled = append(st.disabled, p.Name())
				continue
			}
			if prev, ok := byName[p.Name()]; ok {
				st.problems = append(st.problems, Problem{Dir: dir, Source: src.source,
					Err: fmt.Errorf("plugin %q is already loaded from %s (%s); this copy is ignored", p.Name(), prev.Dir, prev.Source)})
				continue
			}
			byName[p.Name()] = p
			st.plugins = append(st.plugins, p)
		}
	}
	sort.Slice(st.plugins, func(i, j int) bool { return st.plugins[i].Name() < st.plugins[j].Name() })

	reservedTools := map[string]bool{}
	for _, n := range m.opts.ReservedTools {
		reservedTools[n] = true
	}
	for _, p := range st.plugins {
		kept := p.Tools[:0:0]
		for _, t := range p.Tools {
			name := t.Spec().Name
			if reservedTools[name] {
				st.warnings = append(st.warnings, fmt.Sprintf("plugin %q: tool %s is skipped because the name is already in use", p.Name(), name))
				continue
			}
			reservedTools[name] = true
			kept = append(kept, t)
			st.tools = append(st.tools, t)
		}
		p.Tools = kept
	}

	// Command names: a name used by more than one plugin, or by a
	// built-in, is registered in its qualified plugin:name form.
	reserved := map[string]bool{}
	for _, n := range m.opts.ReservedCommands {
		reserved[strings.ToLower(strings.TrimPrefix(n, "/"))] = true
	}
	count := map[string]int{}
	for _, p := range st.plugins {
		for _, c := range p.Commands {
			count[strings.ToLower(c.LocalName())]++
		}
	}
	for _, p := range st.plugins {
		for _, c := range p.Commands {
			key := strings.ToLower(c.LocalName())
			c.Name = c.LocalName()
			switch {
			case reserved[key]:
				c.Name = c.QualifiedName()
				st.warnings = append(st.warnings, fmt.Sprintf("plugin %q: /%s is a built-in command; use /%s", p.Name(), c.LocalName(), c.Name))
			case count[key] > 1:
				c.Name = c.QualifiedName()
				st.warnings = append(st.warnings, fmt.Sprintf("plugin %q: /%s is defined by several plugins; use /%s", p.Name(), c.LocalName(), c.Name))
			}
			st.commands = append(st.commands, c)
			st.byCommand[strings.ToLower(c.Name)] = c
			st.byCommand[strings.ToLower(c.QualifiedName())] = c
		}
	}
	sort.Slice(st.commands, func(i, j int) bool { return st.commands[i].Name < st.commands[j].Name })
	return st
}

// pluginDirs lists the plugin directories under root: every subdirectory
// (symlinks to directories included) whose name does not start with a dot.
// A missing root is not an error.
func pluginDirs(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		p := filepath.Join(root, e.Name())
		st, err := os.Stat(p)
		if err != nil || !st.IsDir() {
			continue
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

func (m *Manager) current() *state {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state
}

// List returns the loaded plugins sorted by name.
func (m *Manager) List() []*Plugin { return append([]*Plugin(nil), m.current().plugins...) }

// Get returns a loaded plugin by name.
func (m *Manager) Get(name string) (*Plugin, bool) {
	for _, p := range m.current().plugins {
		if p.Name() == name {
			return p, true
		}
	}
	return nil, false
}

// Tools returns every plugin tool, ready to add to a tools.Registry.
func (m *Manager) Tools() []tools.Tool {
	st := m.current()
	out := make([]tools.Tool, len(st.tools))
	for i, t := range st.tools {
		out[i] = t
	}
	return out
}

// Commands returns every plugin slash command, sorted by name.
func (m *Manager) Commands() []*Command { return append([]*Command(nil), m.current().commands...) }

// Command finds a command by its registered or qualified name, with or
// without the leading slash.
func (m *Manager) Command(name string) (*Command, bool) {
	c, ok := m.current().byCommand[strings.ToLower(strings.TrimPrefix(name, "/"))]
	return c, ok
}

// Problems lists plugins that failed to load in the last Load or Reload.
func (m *Manager) Problems() []Problem { return append([]Problem(nil), m.current().problems...) }

// Warnings lists non-fatal issues such as renamed commands.
func (m *Manager) Warnings() []string { return append([]string(nil), m.current().warnings...) }

// Untrusted lists project plugin directories that were not loaded because
// the project is not trusted.
func (m *Manager) Untrusted() []string { return append([]string(nil), m.current().untrusted...) }

// Disabled lists plugins skipped because Options.Disabled names them.
func (m *Manager) Disabled() []string { return append([]string(nil), m.current().disabled...) }
