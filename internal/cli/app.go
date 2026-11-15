// Package cli wires the agent to the terminal: setup, the interactive REPL,
// slash commands and the non-interactive print mode.
package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/agent"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/config"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/hooks"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/mcp"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permissions"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/session"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

// Options are the command-line settings.
type Options struct {
	Prompt          string
	Print           bool
	OutputFormat    string
	Continue        bool
	Resume          string
	Model           string
	Effort          string
	PermissionMode  string
	SkipPermissions bool
	AllowedTools    []string
	DisallowedTools []string
	AddDirs         []string
	MaxTurns        int
	SystemPrompt    string
	AppendSystem    string
	Verbose         bool
	NoWebSearch     bool
}

// App holds everything a session needs.
type App struct {
	Opts     Options
	Cwd      string
	Settings *config.Settings
	Agent    *agent.Agent
	MCP      []*mcp.Client
	MCPErrs  map[string]error
	Commands []config.Command
	Warnings []string
	Resumed  int // number of restored messages
}

// NewApp loads configuration and builds the agent.
func NewApp(opts Options, ui agent.UI) (*App, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	settings, errs := config.Load(cwd)
	app := &App{Opts: opts, Cwd: cwd, Settings: settings, MCPErrs: map[string]error{}}
	for _, e := range errs {
		app.Warnings = append(app.Warnings, "settings: "+e.Error())
	}
	for k, v := range settings.Env {
		os.Setenv(k, v)
	}

	model := firstNonEmpty(opts.Model, os.Getenv("GOCODE_MODEL"), os.Getenv("ANTHROPIC_MODEL"), settings.Model, agent.DefaultModel)
	model = agent.ResolveModel(model)
	effort := firstNonEmpty(opts.Effort, settings.Effort, "high")
	if !validEffort(effort) {
		return nil, fmt.Errorf("invalid effort %q (use %s)", effort, strings.Join(agent.Efforts, ", "))
	}

	modeName := firstNonEmpty(opts.PermissionMode, settings.Permissions.DefaultMode)
	if opts.SkipPermissions {
		modeName = string(permissions.ModeBypass)
	}
	mode, ok := permissions.ParseMode(modeName)
	if !ok {
		return nil, fmt.Errorf("invalid permission mode %q (use default, acceptEdits, plan or bypassPermissions)", modeName)
	}
	perm := permissions.New(mode, cwd, append(settings.Permissions.Allow, opts.AllowedTools...),
		settings.Permissions.Deny, settings.Permissions.Ask)
	for _, d := range opts.AddDirs {
		if abs, err := filepath.Abs(d); err == nil {
			perm.AddDir(abs)
		}
	}

	reg := tools.Builtin()
	env := tools.NewEnv(cwd)
	for name, cfg := range settings.MCPServers {
		c, err := mcp.Connect(context.Background(), name, cfg, cwd)
		if err != nil {
			app.MCPErrs[name] = err
			app.Warnings = append(app.Warnings, fmt.Sprintf("MCP server %q failed to start: %v", name, err))
			continue
		}
		c.Register(reg)
		app.MCP = append(app.MCP, c)
	}

	sess := session.New(cwd)
	var history []json.RawMessage
	if opts.Continue || opts.Resume != "" {
		var info session.Info
		if opts.Resume != "" {
			if info, err = session.Find(cwd, opts.Resume); err != nil {
				return nil, err
			}
		} else if list := session.List(cwd); len(list) > 0 {
			info = list[0]
		} else {
			app.Warnings = append(app.Warnings, "no earlier session in this directory; starting a new one")
		}
		if info.Path != "" {
			s, msgs, err := session.Load(info.Path)
			if err != nil {
				return nil, fmt.Errorf("cannot load session: %w", err)
			}
			sess, history = s, msgs
		}
	}

	maxTokens := settings.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 64000
	}
	a := &agent.Agent{
		Client:      anthropic.NewClient(option.WithMaxRetries(3)),
		Model:       model,
		Effort:      effort,
		MaxTokens:   maxTokens,
		System:      agent.BuildSystemPrompt(cwd, model, opts.SystemPrompt, opts.AppendSystem),
		Tools:       reg,
		Env:         env,
		Perm:        perm,
		Hooks:       &hooks.Runner{Hooks: settings.Hooks, Cwd: cwd, SessionID: sess.ID},
		UI:          ui,
		Session:     sess,
		Usage:       &agent.Usage{},
		MaxTurns:    opts.MaxTurns,
		WebSearch:   !opts.NoWebSearch && (settings.WebSearch == nil || *settings.WebSearch),
		Fallback:    settings.RefusalFallback == nil || *settings.RefusalFallback,
		AutoCompact: settings.AutoCompact == nil || *settings.AutoCompact,
		Cwd:         cwd,
	}
	a.RegisterAgentTools()
	// A bare tool name in --disallowedTools removes the tool entirely;
	// anything with a pattern becomes a deny rule.
	for _, d := range opts.DisallowedTools {
		if _, ok := reg.Get(d); ok {
			reg.Remove(d)
		} else {
			perm.AddDeny(d)
		}
	}
	if len(history) > 0 {
		if err := a.LoadHistory(history); err != nil {
			return nil, fmt.Errorf("cannot restore session: %w", err)
		}
		app.Resumed = len(history)
	}
	app.Agent = a
	app.Commands = config.LoadCommands(cwd)
	if !hasCredentials() {
		app.Warnings = append(app.Warnings, "no API credentials found: set ANTHROPIC_API_KEY (or run `ant auth login`)")
	}
	return app, nil
}

// Close releases background processes.
func (app *App) Close() {
	app.Agent.Env.Shells.KillAll()
	for _, c := range app.MCP {
		c.Close()
	}
}

func hasCredentials() bool {
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_PROFILE", "ANTHROPIC_FEDERATION_RULE_ID"} {
		if os.Getenv(k) != "" {
			return true
		}
	}
	if h, err := os.UserHomeDir(); err == nil {
		if _, err := os.Stat(filepath.Join(h, ".config", "anthropic")); err == nil {
			return true
		}
	}
	return false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func validEffort(e string) bool {
	for _, x := range agent.Efforts {
		if x == e {
			return true
		}
	}
	return false
}

var reMention = regexp.MustCompile(`(^|\s)@([^\s]+)`)

var mentionImages = map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp"}

// expandMentions turns @path references in a prompt into attached content
// blocks and reports what was attached.
func expandMentions(cwd, prompt string) (blocks []anthropic.BetaContentBlockParamUnion, attached []string) {
	seen := map[string]bool{}
	for _, m := range reMention.FindAllStringSubmatch(prompt, -1) {
		ref := strings.TrimRight(m[2], ".,;:!?)")
		path := ref
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		st, err := os.Stat(path)
		if err != nil || seen[path] {
			continue
		}
		seen[path] = true
		if st.IsDir() {
			entries, _ := os.ReadDir(path)
			var names []string
			for _, e := range entries {
				n := e.Name()
				if e.IsDir() {
					n += "/"
				}
				names = append(names, n)
			}
			blocks = append(blocks, anthropic.NewBetaTextBlock(fmt.Sprintf("<directory path=%q>\n%s\n</directory>", ref, strings.Join(names, "\n"))))
			attached = append(attached, ref+"/")
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if mt, ok := mentionImages[strings.ToLower(filepath.Ext(path))]; ok && len(b) <= 5<<20 {
			blocks = append(blocks, anthropic.BetaContentBlockParamUnion{OfImage: &anthropic.BetaImageBlockParam{
				Source: anthropic.BetaImageBlockParamSourceUnion{OfBase64: &anthropic.BetaBase64ImageSourceParam{
					Data: base64.StdEncoding.EncodeToString(b), MediaType: anthropic.BetaBase64ImageSourceMediaType(mt),
				}},
			}})
			attached = append(attached, ref)
			continue
		}
		const maxAttach = 200 << 10
		if len(b) > maxAttach {
			blocks = append(blocks, anthropic.NewBetaTextBlock(fmt.Sprintf(
				"<file path=%q>\n[file is %d bytes, too large to attach in full; use the Read tool with offset and limit]\n</file>", ref, len(b))))
			attached = append(attached, ref+" (too large, not inlined)")
			continue
		}
		blocks = append(blocks, anthropic.NewBetaTextBlock(fmt.Sprintf("<file path=%q>\n%s\n</file>", ref, string(b))))
		attached = append(attached, ref)
	}
	return blocks, attached
}
