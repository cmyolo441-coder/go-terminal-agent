# gocode

An AI coding agent for the terminal, written in Go. It works in your project
directory: it reads and edits files, runs commands, searches the code and the
web, and keeps going until the task is done, asking permission before anything
that changes your machine.

```
╭────────────────────────────────────────────────────────────
│ ✻ gocode v1.0.0
│   model: claude-opus-5-5 · effort: high
│   cwd:   ~/src/myapp
╰────────────────────────────────────────────────────────────
❯ add a --json flag to the export command and test it

● I'll look at how the export command parses flags first.

● Read(cmd/export.go)
  ⎿ Read 142 lines

● Edit(cmd/export.go)
  ⎿ Updated cmd/export.go at line 31
    - fs.BoolVar(&o.pretty, "pretty", false, "")
    + fs.BoolVar(&o.pretty, "pretty", false, "")
    + fs.BoolVar(&o.json, "json", false, "")
```

## Install

Requires Go 1.25 or newer.

```bash
go install github.com/cmyolo441-coder/go-terminal-agent/cmd/gocode@latest
```

or from a checkout:

```bash
go build -o gocode ./cmd/gocode
```

Set your API key, then start it inside a project:

```bash
export ANTHROPIC_API_KEY=sk-ant-...
```

```bash
gocode
```

`ANTHROPIC_BASE_URL` and `ANTHROPIC_AUTH_TOKEN` are honored.

## Usage

```bash
gocode                                   # interactive session
gocode "fix the failing test"            # interactive, with a first prompt
gocode -p "summarize this repo"          # print the answer and exit
git diff | gocode -p "review this diff"  # piped input joins the prompt
gocode -c                                # continue the last session here
gocode -r <session-id>                   # resume a specific session
```

| Flag | Meaning |
|---|---|
| `-p`, `--print` | Non-interactive: run one prompt, print the result, exit |
| `--output-format` | With `-p`: `text`, `json` or `stream-json` |
| `-c`, `--continue` / `-r`, `--resume <id>` | Restore an earlier conversation |
| `--model` | `opus`, `sonnet`, `haiku`, `fable` or a full model id |
| `--effort` | `low`, `medium`, `high` (default), `xhigh`, `max` |
| `--permission-mode` | `default`, `acceptEdits`, `plan`, `bypassPermissions` |
| `--allowedTools`, `--disallowedTools` | Permission rules, e.g. `"Bash(git diff:*),Edit"` |
| `--dangerously-skip-permissions` | Never ask. Only for sandboxes |
| `--max-turns` | Cap the number of model requests |
| `--system-prompt`, `--append-system-prompt` | Replace or extend the system prompt |
| `--add-dir`, `--no-web-search`, `--verbose` | Extra workspace dir, disable web search, full tool output |

### In the session

| Input | Effect |
|---|---|
| `@path` | Attach a file, image or directory listing (tab completes) |
| `!command` | Run a shell command yourself; the output is shared with the agent |
| `# note` | Append a note to `AGENTS.md` |
| `line \` | Continue the prompt on the next line |
| `ctrl+c` | Interrupt the current turn; twice at the prompt to exit |

Slash commands: `/help`, `/clear`, `/compact`, `/context`, `/cost`, `/status`,
`/model`, `/effort`, `/mode`, `/plan`, `/permissions`, `/tools`, `/mcp`,
`/todos`, `/bashes`, `/memory`, `/init`, `/review`, `/diff`, `/resume`,
`/export`, `/config`, `/exit`.

## What it can do

**Tools.** `Bash` (with timeouts and background shells via `BashOutput` /
`KillShell`), `Read` (text and images), `Write`, `Edit`, `Glob`, `Grep`
(ripgrep when installed, a built-in engine otherwise), `WebFetch`, web search,
`TodoWrite`, and `Task`, which hands a self-contained job to a sub-agent with
its own context. Read-only calls in one response run in parallel.

**Permissions.** Reading is free; edits and commands ask first. Each prompt
offers "yes", "yes, always" (saves a rule to `.gocode/settings.local.json`) or
"no, do this instead". Rules look like `Bash(npm test:*)`, `Read(.env)`,
`Edit(src/**)`, `WebFetch(domain:example.com)` or `mcp__server`. Deny rules
win in every mode. Chained shell commands are checked part by part, and
anything using command substitution or output redirection always asks.

| Mode | Behavior |
|---|---|
| `default` | Ask before edits and non-read-only commands |
| `acceptEdits` | Edit files inside the project freely; still ask for commands |
| `plan` | Read-only research; the agent proposes a plan and waits for approval |
| `bypassPermissions` | Never ask (deny rules still apply) |

**Memory.** `AGENTS.md` (and `CLAUDE.md`, if present) from the project and its
parent directories, plus `~/.gocode/AGENTS.md`, are loaded into the system
prompt. `/init` writes one for the current project.

**Sessions.** Every conversation is saved as JSONL under
`~/.gocode/projects/`. `/compact` replaces the history with a summary, and this
happens automatically when the context window is 85% full.

**Custom commands.** A file `.gocode/commands/fix-issue.md` becomes
`/fix-issue`; `$ARGUMENTS` is replaced with what follows the command. An
optional front matter `description:` shows up in `/help`.

**MCP.** Stdio MCP servers listed under `mcpServers` in `.mcp.json` or the
settings file are started at launch; their tools appear as
`mcp__<server>__<tool>`.

**Hooks.** Shell commands for `PreToolUse`, `PostToolUse`, `UserPromptSubmit`,
`Stop` and `SessionStart`. A hook receives the event as JSON on stdin; exit
code 2 blocks the action and its stderr is given to the agent.

## Settings

Merged in this order, later files winning:
`~/.gocode/settings.json`, `.gocode/settings.json`,
`.gocode/settings.local.json`.

```json
{
  "model": "opus",
  "effort": "high",
  "permissions": {
    "defaultMode": "acceptEdits",
    "allow": ["Bash(go test:*)", "Bash(git diff:*)"],
    "deny": ["Read(.env)", "Bash(rm -rf:*)"],
    "ask": ["Bash(git push:*)"]
  },
  "hooks": {
    "PostToolUse": [
      { "matcher": "Edit|Write", "hooks": [{ "type": "command", "command": "gofmt -l . >&2" }] }
    ]
  },
  "mcpServers": {
    "files": { "command": "npx", "args": ["-y", "@modelcontextprotocol/server-filesystem", "."] }
  },
  "env": { "GOFLAGS": "-mod=mod" },
  "webSearch": true,
  "autoCompact": true,
  "refusalFallback": true
}
```

Environment: `GOCODE_MODEL`, `GOCODE_HOME` (state directory),
`GOCODE_CONTEXT_WINDOW`, `NO_COLOR`.

## How it works

`internal/agent` runs the loop: it streams a response from the Messages API
through the official Go SDK, shows text and thinking summaries as they arrive,
runs the requested tools behind `internal/permissions`, sends the results back
and repeats until the model stops asking for tools. The system prompt and tool
list are kept byte-stable and cached, so long sessions mostly pay cache-read
prices. Thinking blocks are replayed unchanged and the history is append-only.

```
cmd/gocode              flags, print mode vs REPL
internal/agent          model/tool loop, system prompt, compaction, sub-agents, cost
internal/tools          Bash, Read, Write, Edit, Glob, Grep, WebFetch, TodoWrite
internal/permissions    modes and allow/deny/ask rules
internal/cli            REPL, slash commands, terminal rendering, print mode
internal/session        JSONL transcripts
internal/config         settings, memory files, custom commands
internal/mcp            stdio MCP client
internal/hooks          lifecycle hooks
internal/ui             colors, spinner, streaming markdown
internal/mockapi        scripted API server used by the tests
```

## Development

```bash
go test ./...
```

The tests drive the real agent loop and the built binary against a scripted
mock of the API, so they need no network or API key.
