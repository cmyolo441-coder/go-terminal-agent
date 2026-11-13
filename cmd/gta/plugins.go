package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/ui"
)

// registerPluginCommands adds each plugin command as a slash command and
// the /plugins command. Commands are looked up by name when run, so a
// /plugins reload updates them; brand-new commands appear after a restart.
func registerPluginCommands(ts *toolset) {
	m := ts.plugins
	ui.Register(ui.Extra{Name: "plugins", Args: "[reload]", Help: "List plugins, or reload them from disk", Run: func(ctx context.Context, arg string) (string, error) {
		if m == nil {
			return "Plugins are turned off (`\"disable_plugins\": true`).", nil
		}
		if strings.TrimSpace(arg) == "reload" {
			if err := m.Reload(); err != nil {
				return "", err
			}
			ts.rebuild()
			return "Reloaded.\n\n" + pluginReport(ts), nil
		}
		if arg != "" {
			return "", fmt.Errorf("usage: /plugins [reload]")
		}
		return pluginReport(ts), nil
	}})
	if m == nil {
		return
	}
	for _, c := range m.Commands() {
		name := c.Name
		e := ui.Extra{Name: name, Args: c.ArgumentHint, Help: c.Description + " (plugin)"}
		if c.IsPrompt() {
			e.Prompt = func(arg string) (string, error) {
				cur, ok := m.Command(name)
				if !ok {
					return "", fmt.Errorf("plugin command /%s is no longer loaded", name)
				}
				return cur.Expand(arg)
			}
		} else {
			e.Run = func(ctx context.Context, arg string) (string, error) {
				cur, ok := m.Command(name)
				if !ok {
					return "", fmt.Errorf("plugin command /%s is no longer loaded", name)
				}
				return cur.Run(ctx, arg)
			}
		}
		ui.Register(e)
	}
}

// pluginReport is the Markdown body of /plugins.
func pluginReport(ts *toolset) string {
	m := ts.plugins
	var b strings.Builder
	list := m.List()
	if len(list) == 0 {
		b.WriteString("No plugins loaded. Put a plugin directory with a `plugin.json` in `~/.config/gta/plugins/` (or `.gta/plugins/` in a trusted project).\n")
	}
	for _, p := range list {
		fmt.Fprintf(&b, "- **%s** %s (%s): %s\n", p.Name(), orNone(p.Manifest.Version, "dev"), p.Source, p.Manifest.Description)
		for _, t := range p.Tools {
			fmt.Fprintf(&b, "  - tool `%s`\n", t.Spec().Name)
		}
		for _, c := range p.Commands {
			fmt.Fprintf(&b, "  - command `/%s`\n", c.Name)
		}
	}
	for _, pr := range m.Problems() {
		fmt.Fprintf(&b, "\n⚠ %s", pr.Error())
	}
	for _, w := range m.Warnings() {
		fmt.Fprintf(&b, "\n⚠ %s", w)
	}
	if u := m.Untrusted(); len(u) > 0 {
		fmt.Fprintf(&b, "\n\nIgnored project plugins (untrusted): %s", strings.Join(u, ", "))
	}
	if d := m.Disabled(); len(d) > 0 {
		fmt.Fprintf(&b, "\n\nDisabled: %s", strings.Join(d, ", "))
	}
	return strings.TrimRight(b.String(), "\n")
}

// pluginCaps reports plugins and web tools for the banner and /status.
func pluginCaps(ts *toolset) []ui.Capability {
	var out []ui.Capability
	if ts.plugins == nil {
		out = append(out, ui.Capability{Group: "Integrations", Name: "plugins", Value: "off", State: ui.CapOff, Banner: true})
	} else {
		n, tl, cm := 0, 0, 0
		for _, p := range ts.plugins.List() {
			n++
			tl += len(p.Tools)
			cm += len(p.Commands)
		}
		state := ui.CapOn
		if n == 0 {
			state = ui.CapOff
		}
		if len(ts.plugins.Problems()) > 0 {
			state = ui.CapWarn
		}
		out = append(out, ui.Capability{Group: "Integrations", Name: "plugins", Value: fmt.Sprintf("%d loaded · %d tools · %d commands", n, tl, cm), State: state, Banner: true})
	}
	var web []string
	for _, t := range ts.core {
		switch t.Spec().Name {
		case "web_fetch", "web_search":
			web = append(web, t.Spec().Name)
		}
	}
	if len(web) > 0 {
		out = append(out, ui.Capability{Group: "Model", Name: "web tools", Value: strings.Join(web, " + ") + " (client-side)", State: ui.CapOn})
	}
	return out
}
