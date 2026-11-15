// gocode is a terminal AI coding agent: an interactive REPL and a scriptable
// print mode around a model/tool loop with permissions, sessions and MCP.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/cli"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/config"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/ui"
)

// listFlag collects a repeatable, comma-separated flag.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error {
	for _, p := range splitRules(v) {
		*l = append(*l, p)
	}
	return nil
}

// splitRules splits on commas that are not inside parentheses, so rules such
// as Bash(git diff:*,x) survive.
func splitRules(v string) []string {
	var out []string
	depth, start := 0, 0
	for i, ch := range v {
		switch ch {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, v[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, v[start:])
	var clean []string
	for _, s := range out {
		if s = strings.TrimSpace(s); s != "" {
			clean = append(clean, s)
		}
	}
	return clean
}

const usage = `%[1]s - an AI coding agent for your terminal

Usage:
  %[1]s [flags] [prompt]          start an interactive session
  %[1]s -p [flags] "prompt"       answer once and exit (reads piped stdin too)

Flags:
  -p, --print                         non-interactive: print the result and exit
      --output-format <fmt>           with -p: text (default), json or stream-json
  -c, --continue                      continue the most recent session in this directory
  -r, --resume <id>                   resume a session by id
      --model <name>                  opus, sonnet, haiku, fable or a full model id
      --effort <level>                low, medium, high (default), xhigh or max
      --permission-mode <mode>        default, acceptEdits, plan or bypassPermissions
      --dangerously-skip-permissions  never ask for permission (use only in a sandbox)
      --allowedTools <rules>          allow rules, e.g. "Bash(git diff:*),Edit"
      --disallowedTools <rules>       tools to remove or deny rules
      --add-dir <dir>                 extra directory where edits may be auto-accepted
      --max-turns <n>                 stop after n model requests
      --system-prompt <text>          replace the built-in system prompt
      --append-system-prompt <text>   add to the system prompt
      --no-web-search                 disable the web search tool
      --verbose                       show full tool output
  -v, --version                       print the version
  -h, --help                          show this help

Authentication: set ANTHROPIC_API_KEY (ANTHROPIC_BASE_URL is honored).
Settings: ~/%[2]s/settings.json, %[2]s/settings.json, %[2]s/settings.local.json
`

func main() {
	var o cli.Options
	var allowed, disallowed, addDirs listFlag
	var version bool
	fs := flag.NewFlagSet(config.AppName, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&o.Print, "p", false, "")
	fs.BoolVar(&o.Print, "print", false, "")
	fs.StringVar(&o.OutputFormat, "output-format", "", "")
	fs.BoolVar(&o.Continue, "c", false, "")
	fs.BoolVar(&o.Continue, "continue", false, "")
	fs.StringVar(&o.Resume, "r", "", "")
	fs.StringVar(&o.Resume, "resume", "", "")
	fs.StringVar(&o.Model, "model", "", "")
	fs.StringVar(&o.Effort, "effort", "", "")
	fs.StringVar(&o.PermissionMode, "permission-mode", "", "")
	fs.BoolVar(&o.SkipPermissions, "dangerously-skip-permissions", false, "")
	fs.Var(&allowed, "allowedTools", "")
	fs.Var(&allowed, "allowed-tools", "")
	fs.Var(&disallowed, "disallowedTools", "")
	fs.Var(&disallowed, "disallowed-tools", "")
	fs.Var(&addDirs, "add-dir", "")
	fs.IntVar(&o.MaxTurns, "max-turns", 0, "")
	fs.StringVar(&o.SystemPrompt, "system-prompt", "", "")
	fs.StringVar(&o.AppendSystem, "append-system-prompt", "", "")
	fs.BoolVar(&o.NoWebSearch, "no-web-search", false, "")
	fs.BoolVar(&o.Verbose, "verbose", false, "")
	fs.BoolVar(&version, "v", false, "")
	fs.BoolVar(&version, "version", false, "")
	if err := fs.Parse(os.Args[1:]); err != nil {
		if err == flag.ErrHelp {
			fmt.Printf(usage, config.AppName, config.DirName)
			return
		}
		fmt.Fprintf(os.Stderr, "%v\nRun `%s --help` for usage.\n", err, config.AppName)
		os.Exit(2)
	}
	if version {
		fmt.Printf("%s %s\n", config.AppName, config.Version)
		return
	}
	o.AllowedTools, o.DisallowedTools, o.AddDirs = allowed, disallowed, addDirs
	o.Prompt = strings.Join(fs.Args(), " ")

	// Piped stdin becomes part of the prompt.
	stdinPiped := !ui.IsTerminal(os.Stdin)
	if stdinPiped {
		if b, err := io.ReadAll(os.Stdin); err == nil && len(strings.TrimSpace(string(b))) > 0 {
			if o.Prompt != "" {
				o.Prompt += "\n\n"
			}
			o.Prompt += strings.TrimRight(string(b), "\n")
		}
	}
	if o.Print || stdinPiped {
		if strings.TrimSpace(o.Prompt) == "" {
			fmt.Fprintf(os.Stderr, "error: no prompt given. Usage: %s -p \"your prompt\"\n", config.AppName)
			os.Exit(2)
		}
		os.Exit(cli.RunPrint(o))
	}
	os.Exit(cli.RunREPL(o))
}
