// Command gta is a terminal coding agent powered by Claude.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"

	"golang.org/x/term"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/agent"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/config"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permission"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/prompt"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/session"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/ui"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/version"
)

const usage = `gta: a terminal coding agent powered by Claude

Usage:
  gta [flags] [prompt]           start the interactive agent (optionally with a first prompt)
  gta -p "prompt" [flags]        run one prompt non-interactively and print the answer
  echo "prompt" | gta -p         read the prompt from stdin
  gta serve [flags]              run a local HTTP API (gta serve -h)
  gta version [--short|--json]   print version, commit, build date, Go version and OS/arch

Flags:
`

type flags struct {
	print      bool
	jsonOut    bool
	model      string
	effort     string
	mode       string
	yolo       bool
	resume     string
	cont       bool
	cwd        string
	web        bool
	maxTurns   int
	showVer    bool
	noThinking bool
	provider   string
	sandbox    string
}

func parseFlags(args []string) (flags, []string, error) {
	var f flags
	fs := flag.NewFlagSet("gta", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), usage)
		fs.PrintDefaults()
	}
	fs.BoolVar(&f.print, "p", false, "print mode: run the prompt non-interactively and exit")
	fs.BoolVar(&f.print, "print", false, "same as -p")
	fs.BoolVar(&f.jsonOut, "json", false, "with -p, print a JSON result (text, usage, cost, tool calls)")
	fs.StringVar(&f.model, "model", "", "model id (default from config, claude-opus-5-5)")
	fs.StringVar(&f.effort, "effort", "", "reasoning effort: low, medium, high, xhigh, max")
	fs.StringVar(&f.mode, "mode", "", "permission mode: ask, auto-edit, plan, yolo")
	fs.BoolVar(&f.yolo, "yolo", false, "same as --mode yolo")
	fs.StringVar(&f.resume, "resume", "", "resume a saved session by id or id prefix")
	fs.BoolVar(&f.cont, "c", false, "continue the most recent session in this workspace")
	fs.BoolVar(&f.cont, "continue", false, "same as -c")
	fs.StringVar(&f.cwd, "cwd", "", "workspace root (default: current directory)")
	fs.BoolVar(&f.web, "web", false, "enable server-side web search and web fetch")
	fs.IntVar(&f.maxTurns, "max-turns", 0, "maximum model calls per prompt")
	fs.BoolVar(&f.noThinking, "no-thinking", false, "hide reasoning summaries")
	fs.StringVar(&f.provider, "provider", "", "model provider: anthropic, openai, openrouter, ollama, gemini, groq, deepseek, together, lmstudio, vllm, nvidia")
	fs.StringVar(&f.sandbox, "sandbox", "", "shell sandbox: off, workspace-write, read-only")
	fs.BoolVar(&f.showVer, "version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return f, nil, err
	}
	// Flags may also follow the prompt (gta -p "do x" --json): an argument
	// that names a gta flag is parsed as one; anything else, and everything
	// after "--", is prompt text.
	var rest []string
	for args = fs.Args(); len(args) > 0; {
		a := args[0]
		if a == "--" {
			rest = append(rest, args[1:]...)
			break
		}
		name, hasValue := strings.TrimLeft(a, "-"), false
		if i := strings.IndexByte(name, '='); i >= 0 {
			name, hasValue = name[:i], true
		}
		fl := fs.Lookup(name)
		if !strings.HasPrefix(a, "-") || name == "" || fl == nil {
			rest, args = append(rest, a), args[1:]
			continue
		}
		n := 2
		if b, ok := fl.Value.(interface{ IsBoolFlag() bool }); hasValue || (ok && b.IsBoolFlag()) {
			n = 1
		}
		if err := fs.Parse(args[:min(n, len(args))]); err != nil {
			return f, nil, err
		}
		args = args[min(n, len(args)):]
	}
	return f, rest, nil
}

func main() {
	run := run
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "serve" {
		run, args = runServe, args[1:]
	}
	if len(args) > 0 && args[0] == "version" {
		run, args = runVersion, args[1:]
	}
	if err := run(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "gta:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	f, rest, err := parseFlags(args)
	if err != nil {
		return err
	}
	if f.showVer {
		fmt.Println(versionString())
		return nil
	}
	root := f.cwd
	if root == "" {
		if root, err = os.Getwd(); err != nil {
			return err
		}
	}
	if root, err = filepath.Abs(root); err != nil {
		return err
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return fmt.Errorf("workspace %s is not a directory", root)
	}

	cfg, err := config.Load(root)
	if err != nil {
		return err
	}
	applyFlags(&cfg, f)
	if err := cfg.Validate(); err != nil {
		return err
	}
	gv, err := loadPolicy(&cfg)
	if err != nil {
		return err
	}
	mode, err := permission.ParseMode(cfg.PermissionMode)
	if err != nil {
		return err
	}

	env, err := tools.NewEnv(root)
	if err != nil {
		return err
	}
	gv.guardEnv(env)
	if cfg.ShellTimeout > 0 {
		env.ShellTimeout = secondsDuration(cfg.ShellTimeout)
	}
	provider, hint, err := buildProvider(&cfg, f.model != "", false)
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
	provider, rt, err := buildRouting(cfg, provider, tel, gv.checkProviderModel)
	if err != nil {
		return err
	}
	policy, err := buildSandbox(cfg, env)
	if err != nil {
		return err
	}

	store := session.DefaultStore()
	sess := session.New(root, cfg.Model)
	switch {
	case f.resume != "":
		if sess, err = store.Load(f.resume); err != nil {
			return err
		}
	case f.cont:
		if sess, err = store.Latest(root); err != nil {
			return err
		}
	}

	if err := gv.build(cfg, root, sess.ID); err != nil {
		return err
	}
	defer gv.close()
	hookRunner, err := buildHooks(cfg, root, sess.ID)
	if err != nil {
		return err
	}
	// prompt.Build runs git; overlap it with opening the code index.
	systemc := make(chan string, 1)
	go func() { systemc <- prompt.Build(root) }()
	ci := startCodeIntel(cfg, root, gv.codeExclude(root))
	defer ci.close()
	ciPrompt := ci.systemPrompt()
	system := <-systemc
	if m := ciPrompt; m != "" {
		system += "\n\n" + m
	}
	for _, n := range ci.notes {
		fmt.Fprintln(os.Stderr, "gta:", n)
	}
	ci.warm(root)
	if extra, warns := sessionStartContext(hookRunner); extra != "" || len(warns) > 0 {
		for _, w := range warns {
			fmt.Fprintln(os.Stderr, "gta: hook warning:", w)
		}
		if extra != "" {
			system += "\n\n<session-start-hook>\n" + extra + "\n</session-start-hook>"
		}
	}
	if n := cfg.IgnoredProjectNotice(); n != "" {
		fmt.Fprintln(os.Stderr, "gta:", n)
	}
	if cfg.IgnoredProjectHooks {
		fmt.Fprintln(os.Stderr, "gta: ignoring hooks in .gta/config.json; set \"trust_project_hooks\": true in your user config to run them")
	}

	ts := newToolset(cfg, root, ci.tools()...)
	for _, n := range ts.notes {
		fmt.Fprintln(os.Stderr, "gta:", n)
	}
	ag := agent.New(provider, ts.base(), env, permission.NewGate(mode, cfg.AllowCommands), system, agent.Settings{
		Model:        cfg.Model,
		Effort:       cfg.Effort,
		MaxTokens:    cfg.MaxTokens,
		ShowThinking: cfg.ShowThinking,
		MaxTurns:     cfg.MaxTurns,
	})
	ag.Hooks = hookRunner
	gv.attach(ag)
	ci.attach(ag)
	ag.Tel, ag.SessionID = tel, sess.ID
	rt.setNotify(func(msg string) {
		if s := ag.ActiveSink(); s != nil {
			s.Notice(msg)
		}
	})
	ts.ag = ag
	addTaskTool(ag, cfg, root, ts)
	cp, cpErr := setupCheckpoints(ag, sess.ID)
	if cpErr != nil {
		fmt.Fprintln(os.Stderr, "gta: checkpoints disabled:", cpErr)
	}
	var uiStarted atomic.Bool
	mcpManager := startMCP(ag, cfg, root, &uiStarted, ts)
	if mcpManager != nil {
		defer mcpManager.Close()
	}
	registerCommands(root, cfg, cp, hookRunner, policy, mcpManager)
	registerPluginCommands(ts)
	registerGovernanceCommands(gv)
	registerAutopilotCommands(ag, cfg, root)
	registerCodeCommands(ci)
	caps := &capSources{cfg: cfg, root: root, ag: ag, authHint: hint, policy: policy, hooks: hookRunner, mcp: mcpManager, cp: cp, ts: ts, gv: gv}
	caps.extra = append(caps.extra, rt.capabilities, ci.capabilities)
	if len(sess.Messages) > 0 {
		ag.SetHistory(sess.Messages, sess.Usage, sess.CostUSD)
	}

	promptText := strings.TrimSpace(strings.Join(rest, " "))
	if f.print {
		if promptText == "" && !term.IsTerminal(int(os.Stdin.Fd())) {
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				return err
			}
			promptText = strings.TrimSpace(string(data))
		}
		if promptText == "" {
			return errors.New("-p needs a prompt (as an argument or on stdin)")
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		runErr := ui.RunHeadless(ctx, ag, promptText, sess.ID, f.jsonOut, os.Stdout, os.Stderr)
		saveSession(store, sess, ag)
		if errors.Is(runErr, agent.ErrInterrupted) {
			return errors.New("interrupted")
		}
		return runErr
	}

	if !term.IsTerminal(int(os.Stdout.Fd())) || !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("interactive mode needs a terminal; use -p for scripts and pipes")
	}
	uiStarted.Store(true)
	err = ui.Run(ui.Options{
		Agent:         ag,
		Session:       sess,
		Store:         store,
		Config:        cfg,
		Root:          root,
		Version:       version.Version,
		InitialPrompt: promptText,
		AuthHint:      hint,
		Capabilities:  caps.capabilities,
		Density:       cfg.Density,
	})
	if len(ag.History()) > 0 {
		fmt.Printf("\nSession saved. Resume with: gta --resume %s\n", sess.ID)
	}
	return err
}

func applyFlags(cfg *config.Config, f flags) {
	if f.model != "" {
		cfg.Model = f.model
	}
	if f.effort != "" {
		cfg.Effort = f.effort
	}
	if f.mode != "" {
		cfg.PermissionMode = f.mode
	}
	if f.yolo {
		cfg.PermissionMode = string(permission.ModeYolo)
	}
	if f.web {
		cfg.WebSearch, cfg.WebFetch = true, true
	}
	if f.maxTurns > 0 {
		cfg.MaxTurns = f.maxTurns
	}
	if f.noThinking {
		cfg.ShowThinking = false
	}
	if f.provider != "" {
		cfg.Provider = f.provider
	}
	if f.sandbox != "" {
		cfg.Sandbox = f.sandbox
	}
}

func saveSession(store *session.Store, sess *session.Session, ag *agent.Agent) {
	sess.Messages = ag.History()
	sess.Usage, sess.CostUSD, _ = ag.Stats()
	sess.Model = ag.Settings().Model
	if err := store.Save(sess); err != nil {
		fmt.Fprintln(os.Stderr, "gta: could not save session:", err)
	}
}
