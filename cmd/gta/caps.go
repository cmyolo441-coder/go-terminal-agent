package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/agent"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/checkpoint"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/config"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/gitx"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/hooks"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm/openai"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/mcp"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/sandbox"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/ui"
)

// capSources is everything the banner and /status dashboard report on.
type capSources struct {
	cfg      config.Config
	root     string
	ag       *agent.Agent
	authHint string
	policy   *sandbox.Policy
	hooks    *hooks.Runner
	mcp      *mcp.Manager
	cp       *checkpoint.Store
	// extra lets later integrations add their own lines.
	extra []func() []ui.Capability
}

func (s *capSources) capabilities() []ui.Capability {
	var out []ui.Capability
	add := func(group, name, value string, state ui.CapState, banner bool) {
		out = append(out, ui.Capability{Group: group, Name: name, Value: value, State: state, Banner: banner})
	}

	// Model
	prov := "anthropic"
	if !isAnthropic(s.cfg) {
		prov = s.cfg.Provider
	}
	provState := ui.CapOn
	provValue := prov
	if s.authHint != "" {
		provState = ui.CapWarn
		provValue += " · no API key"
	} else if info, ok := openai.LookupPreset(s.cfg.Provider); ok && len(info.KeyEnvs) > 0 {
		provValue += " · key from $" + info.KeyEnvs[0]
	}
	add("Model", "provider", provValue, provState, true)
	if isAnthropic(s.cfg) {
		web := []string{}
		if s.cfg.WebSearch {
			web = append(web, "search")
		}
		if s.cfg.WebFetch {
			web = append(web, "fetch")
		}
		if len(web) > 0 {
			add("Model", "web", strings.Join(web, " + ")+" (server-side)", ui.CapOn, false)
		} else {
			add("Model", "web", "off (--web enables search + fetch)", ui.CapOff, false)
		}
		add("Model", "fallbacks", onOffText(s.cfg.Fallbacks, "server-side model fallback"), boolState(s.cfg.Fallbacks), false)
	}
	sub := s.cfg.SubagentModel
	if sub == "" {
		sub = "same as main model"
	}
	add("Model", "sub-agents", "up to 4 in parallel · "+sub, ui.CapOn, false)

	// Safety
	if s.policy != nil && s.policy.Mode != sandbox.ModeOff {
		net := "no network"
		if s.policy.AllowNetwork {
			net = "network on"
		}
		add("Safety", "sandbox", string(s.policy.Mode)+" · "+net, ui.CapOn, true)
	} else {
		add("Safety", "sandbox", "off", ui.CapOff, true)
	}
	add("Safety", "allow-list", fmt.Sprintf("%d command prefixes run without asking", len(s.cfg.AllowCommands)), ui.CapOn, false)
	if s.cp != nil {
		add("Safety", "checkpoints", fmt.Sprintf("%d saved · /undo restores files", s.cp.Len()), ui.CapOn, false)
	} else {
		add("Safety", "checkpoints", "disabled", ui.CapWarn, false)
	}

	// Integrations
	if s.mcp != nil {
		total, up, failed := 0, 0, 0
		var rows []ui.Capability
		for _, st := range s.mcp.Status() {
			state := ui.CapOn
			val := fmt.Sprintf("%s · %d tools", st.State, len(st.Tools))
			switch st.State {
			case mcp.StateConnected:
				up++
				total += len(st.Tools)
			case mcp.StateFailed:
				failed++
				state = ui.CapWarn
				if st.Err != nil {
					val = "failed · " + st.Err.Error()
				}
			default:
				state = ui.CapOff
			}
			rows = append(rows, ui.Capability{Group: "Integrations", Name: "  " + st.Name, Value: val, State: state})
		}
		state := ui.CapOn
		if failed > 0 {
			state = ui.CapWarn
		}
		add("Integrations", "mcp", fmt.Sprintf("%d servers · %d tools", up, total), state, true)
		out = append(out, rows...)
	} else {
		add("Integrations", "mcp", "none", ui.CapOff, true)
	}
	if s.hooks != nil && s.hooks.Config().Count() > 0 {
		var evs []string
		for _, ev := range hooks.Events {
			if n := len(s.hooks.Config()[ev]); n > 0 {
				evs = append(evs, fmt.Sprintf("%s×%d", ev, n))
			}
		}
		add("Integrations", "hooks", strings.Join(evs, " "), ui.CapOn, true)
	} else {
		state := ui.CapOff
		val := "none"
		if s.cfg.IgnoredProjectHooks {
			state, val = ui.CapWarn, "project hooks/MCP ignored (untrusted)"
		}
		add("Integrations", "hooks", val, state, true)
	}
	for _, f := range s.extra {
		out = append(out, f()...)
	}

	// Workspace
	if gitx.IsRepo(s.root) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		st, err := gitx.Info(ctx, s.root)
		cancel()
		if err == nil {
			state := ui.CapOn
			if st.Conflicted > 0 {
				state = ui.CapWarn
			}
			add("Workspace", "git", st.String(), state, true)
		} else {
			add("Workspace", "git", "error: "+err.Error(), ui.CapWarn, true)
		}
	} else {
		add("Workspace", "git", "not a repository", ui.CapOff, true)
	}
	add("Workspace", "tools", fmt.Sprintf("%d available", len(s.ag.Registry().Names())), ui.CapOn, true)
	return out
}

func boolState(b bool) ui.CapState {
	if b {
		return ui.CapOn
	}
	return ui.CapOff
}

func onOffText(b bool, what string) string {
	if b {
		return "on · " + what
	}
	return "off"
}
