package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
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
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/ui"
)

// setupCheckpoints records every file the agent changes so /undo can put
// it back. A failure here only disables undo; it never blocks the agent.
func setupCheckpoints(ag *agent.Agent, sessionID string) (*checkpoint.Store, error) {
	dir, err := checkpoint.SessionDir(config.UserDir(), sessionID)
	if err != nil {
		return nil, err
	}
	cp, err := checkpoint.Open(dir, checkpoint.Options{})
	if err != nil {
		return nil, err
	}
	ag.BeforeTurn = func(prompt string) { cp.Begin(oneLine(prompt, 80)) }
	ag.BeforeWrite = func(c *tools.Call) {
		if c.Path == "" {
			return
		}
		warn, err := cp.Record(c.Path)
		if s := ag.ActiveSink(); s != nil {
			if err != nil {
				s.Notice("Checkpoint failed for " + c.Path + ": " + err.Error() + ". /undo cannot restore this file.")
			} else if warn != "" {
				s.Notice(warn)
			}
		}
	}
	return cp, nil
}

// registerCommands adds the git, checkpoint, sandbox, hooks and provider
// slash commands to the interactive UI.
func registerCommands(root string, cfg config.Config, cp *checkpoint.Store, hk *hooks.Runner, policy *sandbox.Policy, mm *mcp.Manager) {
	ui.Register(
		ui.Extra{Name: "diff", Args: "[--staged] [paths]", Help: "Show uncommitted changes (git)", Run: func(ctx context.Context, arg string) (string, error) {
			return cmdDiff(ctx, root, arg)
		}},
		ui.Extra{Name: "commit", Args: "<message>", Help: "Commit all changes with a message (git add -A)", Run: func(ctx context.Context, arg string) (string, error) {
			msg := strings.TrimSpace(arg)
			if msg == "" {
				return "", errors.New("usage: /commit <message>, or ask GTA to write the message for you")
			}
			res, err := gitx.Commit(ctx, root, gitx.CommitOptions{Message: msg, All: true})
			if errors.Is(err, gitx.ErrNothingToCommit) {
				return "Nothing to commit.", nil
			}
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("Committed `%s` on %s: %s (%d files, +%d −%d)", res.Short, orNone(res.Branch, "detached HEAD"), res.Subject, res.FilesChanged, res.Insertions, res.Deletions), nil
		}},
		ui.Extra{Name: "log", Args: "[n]", Help: "Show recent commits", Run: func(ctx context.Context, arg string) (string, error) {
			n := 10
			if v, err := strconv.Atoi(strings.TrimSpace(arg)); err == nil && v > 0 {
				n = v
			}
			entries, err := gitx.Log(ctx, root, n)
			if err != nil {
				return "", err
			}
			var b strings.Builder
			b.WriteString("```\n")
			for _, e := range entries {
				b.WriteString(e.String() + "\n")
			}
			b.WriteString("```")
			return b.String(), nil
		}},
		ui.Extra{Name: "undo", Args: "[yes]", Help: "Undo the file changes of the last turn", Run: func(_ context.Context, arg string) (string, error) {
			return cmdUndo(cp, arg)
		}},
		ui.Extra{Name: "checkpoints", Args: "[restore <id> [yes]]", Help: "List file checkpoints or rewind to one", Run: func(_ context.Context, arg string) (string, error) {
			return cmdCheckpoints(cp, arg)
		}},
		ui.Extra{Name: "worktree", Args: "new <name> | list | rm <name> [--force]", Help: "Manage git worktrees for parallel work", Run: func(ctx context.Context, arg string) (string, error) {
			return cmdWorktree(ctx, root, arg)
		}},
		ui.Extra{Name: "sandbox", Help: "Show how shell commands are sandboxed", Run: func(context.Context, string) (string, error) {
			if policy == nil || policy.Mode == sandbox.ModeOff {
				return "Sandbox is **off**: shell commands run with your user's full access. Set `\"sandbox\": \"workspace-write\"` in config or pass `--sandbox workspace-write`.", nil
			}
			return policy.Describe(), nil
		}},
		ui.Extra{Name: "hooks", Help: "List configured hooks", Run: func(context.Context, string) (string, error) {
			if hk == nil {
				msg := "No hooks configured. Add a `hooks` block to your user config."
				if cfg.IgnoredProjectHooks {
					msg += " This project's `.gta/config.json` defines hooks, which are ignored until your user config sets `\"trust_project_hooks\": true`."
				}
				return msg, nil
			}
			var b strings.Builder
			for _, ev := range hooks.Events {
				for _, h := range hk.Config()[ev] {
					fmt.Fprintf(&b, "- **%s**", ev)
					if h.Matcher != "" {
						fmt.Fprintf(&b, " `%s`", h.Matcher)
					}
					fmt.Fprintf(&b, ": `%s`\n", h.Command)
				}
			}
			return b.String(), nil
		}},
		ui.Extra{Name: "mcp", Args: "[reconnect <name>]", Help: "Show MCP servers or reconnect one", Run: func(ctx context.Context, arg string) (string, error) {
			if mm == nil {
				return "No MCP servers configured. Add an `mcp_servers` block to your user config, e.g. `{\"mcp_servers\": {\"fs\": {\"command\": \"npx\", \"args\": [\"-y\", \"@modelcontextprotocol/server-filesystem\", \".\"]}}}`.", nil
			}
			if f := strings.Fields(arg); len(f) == 2 && f[0] == "reconnect" {
				if err := mm.Reconnect(ctx, f[1]); err != nil {
					return "", err
				}
			}
			return "```\n" + strings.TrimRight(mcp.FormatStatus(mm.Status()), "\n") + "\n```", nil
		}},
		ui.Extra{Name: "provider", Help: "Show the model provider and its known models", Run: func(context.Context, string) (string, error) {
			if isAnthropic(cfg) {
				return "Provider: **anthropic** (Claude). Switch with `--provider <name>` or `\"provider\"` in config: " + strings.Join(config.Providers()[1:], ", ") + ".", nil
			}
			info, _ := openai.LookupPreset(cfg.Provider)
			s := fmt.Sprintf("Provider: **%s** (%s) at `%s`.", info.Name, info.Description, info.BaseURL)
			if len(info.KeyEnvs) > 0 {
				s += " Key from `$" + info.KeyEnvs[0] + "`."
			}
			if len(info.KnownModels) > 0 {
				s += "\n\nKnown models (any id works with /model):\n"
				for _, m := range info.KnownModels {
					s += "- `" + m + "`\n"
				}
			}
			return s, nil
		}},
	)
}

func cmdDiff(ctx context.Context, root, arg string) (string, error) {
	if !gitx.IsRepo(root) {
		return "", errors.New("not a git repository")
	}
	opts := gitx.DiffOptions{}
	for _, f := range strings.Fields(arg) {
		if f == "--staged" || f == "--cached" {
			opts.Staged = true
			continue
		}
		opts.Paths = append(opts.Paths, f)
	}
	res, err := gitx.Diff(ctx, root, opts)
	if err != nil {
		return "", err
	}
	if res.Empty() {
		return "No changes.", nil
	}
	out := res.Stat() + "\n\n```diff\n" + strings.TrimRight(res.Text, "\n") + "\n```"
	if res.Truncated {
		out += "\n\n_Diff truncated._"
	}
	return out, nil
}

func cmdUndo(cp *checkpoint.Store, arg string) (string, error) {
	if cp == nil {
		return "", errors.New("checkpoints are not available in this session")
	}
	if cp.Len() == 0 {
		return "Nothing to undo: GTA has not changed any files yet.", nil
	}
	if strings.TrimSpace(arg) != "yes" {
		plan, err := cp.PreviewUndo()
		if err != nil {
			return "", err
		}
		return "```\n" + strings.TrimRight(plan.String(), "\n") + "\n```\n\nRun `/undo yes` to apply.", nil
	}
	res, err := cp.Undo()
	if err != nil {
		return "", err
	}
	return res.String() + "\n\nThe conversation is unchanged; tell GTA what you undid if it should know.", nil
}

func cmdCheckpoints(cp *checkpoint.Store, arg string) (string, error) {
	if cp == nil {
		return "", errors.New("checkpoints are not available in this session")
	}
	f := strings.Fields(arg)
	if len(f) >= 2 && f[0] == "restore" {
		id, err := strconv.Atoi(strings.TrimPrefix(f[1], "#"))
		if err != nil {
			return "", fmt.Errorf("bad checkpoint id %q", f[1])
		}
		if len(f) < 3 || f[2] != "yes" {
			plan, err := cp.Preview(id)
			if err != nil {
				return "", err
			}
			return "```\n" + strings.TrimRight(plan.String(), "\n") + fmt.Sprintf("\n```\n\nRun `/checkpoints restore %d yes` to apply.", id), nil
		}
		res, err := cp.Restore(id)
		if err != nil {
			return "", err
		}
		return res.String(), nil
	}
	list := cp.List()
	if len(list) == 0 {
		return "No checkpoints yet. One is saved for every turn that changes files.", nil
	}
	var b strings.Builder
	for i := len(list) - 1; i >= 0; i-- {
		s := list[i]
		fmt.Fprintf(&b, "- **#%d** %s · %s · %d file(s)", s.ID, s.Time.Format(time.Kitchen), oneLine(s.Label, 50), len(s.Files))
		if s.Created > 0 {
			fmt.Fprintf(&b, ", %d new", s.Created)
		}
		if len(s.Skipped) > 0 {
			fmt.Fprintf(&b, ", %d not restorable", len(s.Skipped))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n`/checkpoints restore <id>` rewinds files to before that checkpoint.")
	return b.String(), nil
}

func cmdWorktree(ctx context.Context, root, arg string) (string, error) {
	if !gitx.IsRepo(root) {
		return "", errors.New("not a git repository")
	}
	f := strings.Fields(arg)
	if len(f) == 0 || f[0] == "list" {
		wts, err := gitx.ListWorktrees(ctx, root)
		if err != nil {
			return "", err
		}
		var b strings.Builder
		for _, w := range wts {
			name := w.Name
			if w.Main {
				name = "main"
			}
			fmt.Fprintf(&b, "- **%s** `%s` %s\n", orNone(name, "(external)"), w.Path, orNone(w.Branch, "(detached)"))
		}
		return b.String(), nil
	}
	switch f[0] {
	case "new", "add":
		if len(f) < 2 {
			return "", errors.New("usage: /worktree new <name>")
		}
		w, err := gitx.CreateWorktree(ctx, root, f[1])
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Created worktree **%s** on branch `%s` at `%s`. Run `gta --cwd %s` in another terminal to work there in parallel.", w.Name, w.Branch, w.Path, w.Path), nil
	case "rm", "remove":
		if len(f) < 2 {
			return "", errors.New("usage: /worktree rm <name> [--force]")
		}
		force := len(f) > 2 && f[2] == "--force"
		r, err := gitx.RemoveWorktree(ctx, root, f[1], force)
		if err != nil {
			return "", err
		}
		s := "Removed worktree `" + r.Path + "`."
		if r.BranchDeleted {
			s += " Deleted branch `" + r.Branch + "`."
		} else if r.BranchNote != "" {
			s += " Kept branch `" + r.Branch + "`: " + r.BranchNote
		}
		return s, nil
	}
	return "", errors.New("usage: /worktree new <name> | list | rm <name> [--force]")
}

func orNone(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}
