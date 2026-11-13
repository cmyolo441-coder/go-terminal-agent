# GTA — Go Terminal Agent

An AI coding agent for your terminal, written in Go. It reads and edits your
code, runs commands, and works through multi-step tasks with you, inline in
your shell (no fullscreen takeover), behind a compact double-line prompt box.

```
❯ create hello.go and run it
✻ The user wants a hello file. I'll look at the directory, then create it.
● Tasks 1/3
  ⎿  ☒ Inspect workspace
     ◼ Create hello.go
     ☐ Run it
● Write hello.go
  ⎿  created hello.go (7 lines)  +7 −0
● Bash go run hello.go
  ⎿  hello from gta
     [exit code 0, 130ms]
  ✓ 7.4s · ↑ 16.8k ↓ 960 · 12.0k cached · $0.04
╔══════════════════════════════════════════════════════════════════╗
║ ❯ Ask anything · / for commands · @ to attach a file · ! shell   ║
╚══════════════════════════════════════════════════════════════════╝
 ◆ opus-5-5 · high · ask                               4.4k ctx · $0.04
```

## Features

- **Streaming agent loop** on Claude (default `claude-opus-5-5`) with adaptive
  thinking, effort levels, prompt caching, server-side fallbacks and lossless
  replay of thinking blocks across turns.
- **Any model provider**: Anthropic, or any OpenAI-compatible API through
  presets: `openai`, `openrouter`, `ollama`, `gemini`, `groq`, `deepseek`,
  `together`, `lmstudio`, `vllm`, `nvidia`.
- **Tools**: read, write, edit (with diff previews), list, glob, grep, bash,
  todo list, parallel **sub-agents** (`task`), and every tool from your
  **MCP servers** (stdio and streamable HTTP).
- **Safety**: four permission modes (`ask`, `auto-edit`, `plan`, `yolo`),
  an approval dialog with diffs, a read-before-write guard, destructive
  commands always asked, and an optional OS **sandbox** for the shell
  (bubblewrap on Linux, sandbox-exec on macOS).
- **Undo**: every turn that changes files is checkpointed; `/undo` and
  `/checkpoints restore <id>` put files back.
- **Git**: `/diff`, `/status`, `/commit`, `/log`, `/worktree` for parallel
  work in separate worktrees.
- **Hooks**: run your own commands before/after tools, on prompt submit, on
  stop and at session start; hooks can block, approve or add context.
- **Sessions**: saved automatically; `gta -c` continues, `gta --resume <id>`
  resumes, `/compact` summarizes a long conversation.
- **Headless mode** for scripts and CI: `gta -p "..." --json`.
- **Memory files**: `AGENTS.md`, `GTA.md` or `CLAUDE.md` in the project (and
  `AGENTS.md` in your config dir) are added to the system prompt.

## Install

Needs Go 1.26 or newer.

```sh
go install github.com/cmyolo441-coder/go-terminal-agent/cmd/gta@latest
```

or from a checkout:

```sh
go build -o gta ./cmd/gta
```

## Quick start

```sh
export ANTHROPIC_API_KEY=sk-ant-...    # or run `ant auth login`
cd your-project
gta                                    # interactive
gta "fix the failing test"             # interactive, with a first prompt
gta -p "summarize this repo" --json    # one-shot, JSON result
```

API keys are read only from environment variables (or the `ant` CLI
profile). GTA never writes a key to disk.

## Using other providers

Pick a provider with `--provider` or `"provider"` in the config. Each preset
reads its key from its own environment variable.

| Provider | Key variable | Notes |
| --- | --- | --- |
| `anthropic` | `ANTHROPIC_API_KEY` | default |
| `nvidia` | `NVIDIA_API_KEY` | `https://integrate.api.nvidia.com/v1` |
| `openai` | `OPENAI_API_KEY` | `OPENAI_BASE_URL` overrides the URL |
| `openrouter` | `OPENROUTER_API_KEY` | |
| `ollama` | none | `OLLAMA_HOST` overrides the URL |
| `gemini`, `groq`, `deepseek`, `together` | their own `*_API_KEY` | |
| `lmstudio`, `vllm` | optional | local servers |

`/provider` inside GTA shows the active preset, its key variable and known
models. Model ids are free-form, so `/model <any-id>` works.

### NVIDIA

```sh
export NVIDIA_API_KEY=nvapi-...        # keep it out of files and chats
gta --provider nvidia --model mistralai/mistral-large-4-1t-a52b
gta --provider nvidia --model kimi-k3.1-pro
```

To check that a model answers before a long session:

```sh
gta --provider nvidia --model kimi-k3.1-pro -p "Reply with OK" --json
```

A working model prints a JSON result with `"is_error": false`. A 404 means
the model id is not available to your account; a 401 means the key is wrong
or not exported. Some NVIDIA models do not support tool calls; pick a
tool-capable model for coding work.

## Keys and commands

| Key | Action |
| --- | --- |
| `enter` | send (while a turn runs, the prompt is queued) |
| `alt+enter`, `ctrl+j` | new line |
| `shift+tab` | cycle permission mode |
| `tab` | complete a command or `@file` |
| `↑` / `↓` | prompt history |
| `esc` | interrupt the running turn |
| `ctrl+c` twice | exit |

Type `@path/to/file` to attach a file, and `!command` to run a shell command
yourself; its output goes with your next prompt.

| Command | What it does |
| --- | --- |
| `/help` | list commands |
| `/model [id]`, `/effort [level]`, `/mode [mode]`, `/thinking` | settings |
| `/clear`, `/compact`, `/sessions`, `/resume <id>` | conversation |
| `/cost`, `/tasks`, `/tools`, `/config`, `/memory`, `/init` | info and setup |
| `/diff [--staged] [paths]`, `/status`, `/commit <msg>`, `/log [n]` | git |
| `/undo [yes]`, `/checkpoints [restore <id> [yes]]` | undo file changes |
| `/worktree new <name> \| list \| rm <name> [--force]` | git worktrees |
| `/mcp [reconnect <name>]`, `/hooks`, `/sandbox`, `/provider` | integrations |
| `/exit` | quit |

## Permission modes

- **ask** (default): reads run freely; edits and commands ask first.
- **auto-edit**: file edits run without asking; commands still ask.
- **plan**: read-only; nothing is changed.
- **yolo**: everything runs, except commands that look destructive
  (`rm -rf`, `git push --force`, …), which always ask.

Commands that start with an entry in `allow_commands` run without asking,
unless they chain several commands.

## Configuration

User config: `~/.config/gta/config.json` (or `$GTA_CONFIG_DIR/config.json`).
Project config: `.gta/config.json` in the workspace; it overrides the user
file. Environment variables `GTA_MODEL`, `GTA_EFFORT`, `GTA_PERMISSION_MODE`,
`GTA_MAX_TOKENS`, `GTA_PROVIDER`, `GTA_SANDBOX` and `ANTHROPIC_BASE_URL`
override both, and flags override everything.

```json
{
  "model": "claude-opus-5-5",
  "effort": "high",
  "max_tokens": 64000,
  "permission_mode": "ask",
  "allow_commands": ["ls", "git status", "go test"],
  "show_thinking": true,
  "web_search": false,
  "web_fetch": false,
  "max_turns": 200,
  "shell_timeout_sec": 120,
  "theme": "auto",
  "provider": "anthropic",
  "subagent_model": "",
  "sandbox": "off",
  "sandbox_network": false,
  "mcp_servers": {
    "fs": { "command": "npx", "args": ["-y", "@modelcontextprotocol/server-filesystem", "."] },
    "docs": { "url": "https://example.com/mcp", "headers": { "Authorization": "Bearer ${DOCS_TOKEN}" } }
  },
  "hooks": {
    "pre_tool_use": [{ "matcher": "bash", "command": "./scripts/check-command.sh", "timeout_sec": 10 }],
    "post_tool_use": [{ "matcher": "write_file|edit_file", "command": "gofmt -l ." }],
    "user_prompt_submit": [],
    "stop": [],
    "session_start": []
  },
  "trust_project_hooks": false
}
```

Hooks and MCP servers in a project's `.gta/config.json` run commands, so they
are ignored unless your **user** config sets `"trust_project_hooks": true`.

### Hooks

A hook gets a JSON payload on stdin (`event`, `session_id`, `cwd`,
`tool_name`, `tool_input`, `tool_output`, `is_error`, `prompt`) and the
variables `GTA_PROJECT_DIR` and `GTA_SESSION_ID`.

- Exit code **2** blocks: the tool call, prompt or stop is refused and
  stderr is the reason. A blocked `stop` sends the model back to work (at
  most 3 times per turn).
- Printing `{"decision":"allow"}` from a `pre_tool_use` hook approves the
  call without a prompt (destructive commands still ask).
- stdout of `user_prompt_submit` and `session_start` hooks is added to the
  model's context.

### Sandbox

`"sandbox": "workspace-write"` lets shell commands write only inside the
workspace and a temp dir; `"read-only"` allows no writes outside a temp dir.
Network is off unless `"sandbox_network": true`. Linux needs `bwrap`
(package `bubblewrap`); macOS uses the built-in `sandbox-exec`. If a sandbox
is configured but cannot run, GTA refuses to start instead of running
commands unprotected.

## Headless mode

```sh
gta -p "add a unit test for parseConfig" --mode auto-edit --json
```

Text streams to stdout (or one JSON object with `--json`), tool activity to
stderr. Actions that would need approval are declined, so choose a
permission mode that fits the job. The session is saved, so `gta -c` can
pick it up interactively.

## Development

```sh
go vet ./...
go test -race ./...
go build ./cmd/gta
```

Package layout:

| Package | Role |
| --- | --- |
| `cmd/gta` | CLI entry point and wiring |
| `internal/agent` | agent loop, tool execution, hooks, usage |
| `internal/llm` | provider interface; `anthropic` and `openai` providers |
| `internal/tools` | built-in tools |
| `internal/subagent` | parallel sub-agent `task` tool |
| `internal/mcp` | MCP client and manager |
| `internal/permission` | permission modes and approval rules |
| `internal/sandbox` | shell sandboxing |
| `internal/hooks` | user hooks |
| `internal/checkpoint` | file snapshots and undo |
| `internal/gitx` | git helpers |
| `internal/ui` | inline terminal UI and headless output |
| `internal/config`, `internal/session`, `internal/prompt`, `internal/pricing` | support |
