package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/agent"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/config"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permission"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/prompt"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/server"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/version"
)

const serveUsage = `gta serve: run GTA as a local HTTP daemon for editors and scripts

Usage:
  gta serve [flags]

Every request except GET /v1/health needs "Authorization: Bearer <token>".
The token comes from $GTA_SERVE_TOKEN, or is generated and written to the
token file (readable only by you). See the README for the API.

Flags:
`

// runServe implements "gta serve".
func runServe(args []string) error {
	var (
		addr, tokenFile, cwd, model, mode, provider, sandboxMode, cors string
		remote, verbose                                                bool
	)
	fs := flag.NewFlagSet("gta serve", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), serveUsage)
		fs.PrintDefaults()
	}
	fs.StringVar(&addr, "addr", server.DefaultAddr, "listen address")
	fs.StringVar(&tokenFile, "token-file", filepath.Join(config.UserDir(), "serve.token"), "where to write the bearer token")
	fs.StringVar(&cwd, "cwd", "", "workspace root (default: current directory)")
	fs.StringVar(&model, "model", "", "default model id")
	fs.StringVar(&mode, "mode", "", "default permission mode: ask, auto-edit, plan, yolo")
	fs.StringVar(&provider, "provider", "", "model provider (see gta -h)")
	fs.StringVar(&sandboxMode, "sandbox", "", "shell sandbox: off, workspace-write, read-only")
	fs.StringVar(&cors, "cors", "", "comma-separated browser origins allowed to call the API")
	fs.BoolVar(&remote, "allow-remote", false, "allow listening on a non-loopback address")
	fs.BoolVar(&verbose, "v", false, "log requests to stderr")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	root, err := workspaceRoot(cwd)
	if err != nil {
		return err
	}
	cfg, err := config.Load(root)
	if err != nil {
		return err
	}
	if n := cfg.IgnoredProjectNotice(); n != "" {
		fmt.Fprintln(os.Stderr, "gta:", n)
	}
	if cfg.IgnoredProjectHooks {
		fmt.Fprintln(os.Stderr, "gta: ignoring hooks and MCP servers in .gta/config.json; set \"trust_project_hooks\": true in your user config to run them")
	}
	applyFlags(&cfg, flags{model: model, mode: mode, provider: provider, sandbox: sandboxMode})
	if err := cfg.Validate(); err != nil {
		return err
	}
	gv, err := loadPolicy(&cfg)
	if err != nil {
		return err
	}
	if _, err := permission.ParseMode(cfg.PermissionMode); err != nil {
		return err
	}
	prov, hint, err := buildProvider(&cfg, model != "", false)
	if err != nil {
		return err
	}
	if err := gv.checkModel(cfg); err != nil {
		return err
	}
	tel, err := startTelemetry(cfg)
	if err != nil {
		return err
	}
	defer shutdownTelemetry(tel)
	if prov, _, err = buildRouting(cfg, prov, tel, gv.checkProviderModel); err != nil {
		return err
	}
	if err := gv.build(cfg, root, "serve"); err != nil {
		return err
	}
	defer gv.close()
	if hint != "" {
		fmt.Fprintln(os.Stderr, "gta:", hint)
	}
	// Check the sandbox once up front so a broken setup fails at start.
	if probe, err := tools.NewEnv(root); err != nil {
		return err
	} else if _, err := buildSandbox(cfg, probe); err != nil {
		return err
	}

	ci := startCodeIntel(cfg, root)
	defer ci.close()
	shared := newToolset(cfg, root, ci.tools()...)
	for _, n := range shared.notes {
		fmt.Fprintln(os.Stderr, "gta:", n)
	}
	var started atomic.Bool
	started.Store(true) // MCP messages go to stderr; there is no UI sink.
	system := prompt.Build(root)
	if m := ci.systemPrompt(); m != "" {
		system += "\n\n" + m
	}
	for _, n := range ci.notes {
		fmt.Fprintln(os.Stderr, "gta:", n)
	}

	var mcpProbe *agent.Agent
	factory := func(ctx context.Context, o server.SessionOptions) (*agent.Agent, error) {
		c := cfg
		if o.Model != "" {
			c.Model = o.Model
		}
		if o.Mode != "" {
			c.PermissionMode = o.Mode
		}
		if o.Model != "" {
			if err := gv.checkModel(c); err != nil {
				return nil, err
			}
		}
		m, err := permission.ParseMode(c.PermissionMode)
		if err != nil {
			return nil, err
		}
		env, err := tools.NewEnv(root)
		if err != nil {
			return nil, err
		}
		if c.ShellTimeout > 0 {
			env.ShellTimeout = secondsDuration(c.ShellTimeout)
		}
		if _, err := buildSandbox(c, env); err != nil {
			return nil, err
		}
		ts := shared.forAgent()
		ag := agent.New(prov, ts.base(), env, permission.NewGate(m, c.AllowCommands), system, agent.Settings{
			Model:        c.Model,
			Effort:       c.Effort,
			MaxTokens:    c.MaxTokens,
			ShowThinking: c.ShowThinking,
			MaxTurns:     c.MaxTurns,
		})
		hk, err := buildHooks(c, root, "serve")
		if err != nil {
			return nil, err
		}
		ag.Hooks = hk
		gv.attach(ag)
		ci.attach(ag)
		ag.Tel = tel
		ts.ag = ag
		addTaskTool(ag, c, root, ts)
		return ag, nil
	}

	// MCP servers are shared by all sessions; a session gets the tools
	// that are up when it is created.
	if len(cfg.MCPServers) > 0 {
		env, _ := tools.NewEnv(root)
		mcpProbe = agent.New(prov, shared.base(), env, permission.NewGate(permission.ModePlan, nil), system, agent.Settings{Model: cfg.Model})
		shared.ag = mcpProbe
		if m := startMCP(mcpProbe, cfg, root, &started, shared); m != nil {
			defer m.Close()
		}
	}

	var logger *slog.Logger
	if verbose {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	var origins []string
	for _, o := range strings.Split(cors, ",") {
		if o = strings.TrimSpace(o); o != "" {
			origins = append(origins, o)
		}
	}
	srv, err := server.New(server.Options{
		Addr:             addr,
		AllowNonLoopback: remote,
		Token:            os.Getenv("GTA_SERVE_TOKEN"),
		TokenFile:        tokenFile,
		Factory:          factory,
		CORSOrigins:      origins,
		Logger:           logger,
	})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(os.Stderr, "gta %s serving %s on http://%s (token in %s)\n", version.Version, root, addr, tokenFile)
	err = srv.ListenAndServe(ctx)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// workspaceRoot resolves the --cwd flag to an absolute directory.
func workspaceRoot(cwd string) (string, error) {
	root := cwd
	var err error
	if root == "" {
		if root, err = os.Getwd(); err != nil {
			return "", err
		}
	}
	if root, err = filepath.Abs(root); err != nil {
		return "", err
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return "", fmt.Errorf("workspace %s is not a directory", root)
	}
	return root, nil
}
