# Commands, flags and keys

## `gta`

```
gta [flags] [prompt]           interactive agent, optionally with a first prompt
gta -p "prompt" [flags]        run one prompt non-interactively and exit
echo "prompt" | gta -p         read the prompt from stdin
gta serve [flags]              local HTTP API (see server-api.md)
```

Flags may be written with one or two dashes (`-model` or `--model`).
Flags must come before the prompt words.

| Flag | Default | Description |
| --- | --- | --- |
| `-p`, `--print` | off | Run the prompt non-interactively and exit. |
| `--json` | off | With `-p`: print one JSON result object instead of streaming text. |
| `--model <id>` | config, `claude-opus-5-5` | Model id. |
| `--effort <level>` | config, `high` | `low`, `medium`, `high`, `xhigh`, `max`. |
| `--mode <mode>` | config, `ask` | Permission mode: `ask`, `auto-edit`, `plan`, `yolo`. |
| `--yolo` | off | Same as `--mode yolo`. |
| `--resume <id>` | — | Resume a saved session by id or id prefix. |
| `-c`, `--continue` | off | Continue the most recent session in this workspace. |
| `--cwd <dir>` | current directory | Workspace root. |
| `--web` | off | Turn on Anthropic's server-side `web_search` and `web_fetch`. |
| `--max-turns <n>` | config, 200 | Maximum model calls per prompt. |
| `--no-thinking` | off | Hide reasoning summaries. |
| `--provider <name>` | config, `anthropic` | `anthropic`, `openai`, `openrouter`, `ollama`, `gemini`, `groq`, `deepseek`, `together`, `lmstudio`, `vllm`, `nvidia`. |
| `--sandbox <mode>` | config, `off` | `off`, `workspace-write`, `read-only`. |
| `--version` | — | Print `gta <version> (<commit>, <date>)` and exit. |
| `-h`, `--help` | — | Print usage. |

Interactive mode needs a terminal on stdin and stdout; otherwise use `-p`.
When an interactive session ends, GTA prints `gta --resume <id>`.

### Headless mode (`-p`)

- The model's text streams to stdout; tool activity and notices go to
  stderr.
- Calls that would need approval are **declined** (there is nobody to ask).
  Pick a mode that fits: `--mode auto-edit` for edits, `--mode yolo` for
  commands. Destructive commands are declined in every mode.
- `@file` mentions in the prompt are attached as in the interactive UI.
- The session is saved, so `gta -c` can pick it up.
- A failed run exits with status 1.

`--json` prints:

```json
{
  "result": "final assistant text",
  "is_error": false,
  "error": "only when is_error is true",
  "session_id": "20261113-120000-a1b2c3",
  "model": "claude-opus-5-5",
  "duration_ms": 7400,
  "turns": 3,
  "usage": { "input_tokens": 16800, "output_tokens": 960, "cache_read_tokens": 12000, "cache_write_tokens": 0 },
  "cost_usd": 0.04,
  "tool_calls": ["write_file", "bash"]
}
```

Examples:

```sh
gta -p "summarize this repo" --json
gta -p "add a unit test for parseConfig" --mode auto-edit
git diff | gta -p "review this diff"
gta --provider ollama --model qwen3-coder "explain main.go"
gta -c                       # continue the last session here
gta --cwd ../other-checkout  # work in another directory
```

## `gta serve`

| Flag | Default | Description |
| --- | --- | --- |
| `--addr <host:port>` | `127.0.0.1:7878` | Listen address. Non-loopback addresses need `--allow-remote`. |
| `--token-file <path>` | `<config dir>/serve.token` | Where the bearer token is written (mode 0600). Removed on shutdown. |
| `--cwd <dir>` | current directory | Workspace root for every session. |
| `--model <id>` | config | Default model for new sessions. |
| `--mode <mode>` | config | Default permission mode for new sessions. |
| `--provider <name>` | config | Model provider. |
| `--sandbox <mode>` | config | Shell sandbox. |
| `--cors <origins>` | none | Comma-separated browser origins allowed to call the API. `*` is refused. |
| `--allow-remote` | off | Allow a non-loopback listen address. |
| `-v` | off | Log requests to stderr. |

The token comes from `GTA_SERVE_TOKEN`, or is generated. See
[server-api.md](server-api.md).

## Input

| Input | What it does |
| --- | --- |
| text + `enter` | Send. While a turn runs, the prompt is queued and sent afterwards. |
| `/command args` | Run a slash command. |
| `!command` | Run a shell command yourself (through the same sandbox as the `bash` tool). Its output is shown and sent along with your next prompt. |
| `@path` | Attach a text file (up to 256 KiB, no binary) to the message. `@shot.png` (`.png`, `.jpg`, `.jpeg`, `.gif`, `.webp`) attaches an image. Paths are relative to the workspace; `~/` works. |

## Keys

| Key | Action |
| --- | --- |
| `enter` | Send, or accept the highlighted completion. |
| `alt+enter`, `ctrl+j` | New line. |
| `tab` | Complete a `/command` or `@file`. |
| `↑` / `↓` | Prompt history (or move through completions). |
| `shift+tab` | Cycle permission mode `ask → auto-edit → plan → yolo`, skipping modes a managed policy forbids. |
| `esc` | Close completions, or interrupt the running turn. |
| `ctrl+c` | Interrupt the running turn; clear the input; twice within 2 s on an empty prompt: exit. |
| `ctrl+d` | Exit when the prompt is empty. |
| `ctrl+b` | Show or hide the live browser panel beside the prompt (needs `--browser`). |

In an approval dialog:

| Key | Action |
| --- | --- |
| `1`, `y` | Allow once. |
| `2`, `a` | Allow for the session (all file edits, or commands with the same program and subcommand, e.g. `npm run`). |
| `3`, `n`, `esc` | Deny. The model is told not to retry and the turn stops. |
| `↑`/`↓`, `k`/`j`, `shift+tab`/`tab` | Move the selection. |
| `enter` | Choose the selection. |
| `ctrl+c` | Deny and interrupt the turn. |

## Slash commands

Built-in commands and those registered by `cmd/gta`. Plugin commands are
added too (see [plugins.md](plugins.md)). `/help` lists everything that is
loaded.

### Session and settings

| Command | Description | Example |
| --- | --- | --- |
| `/help`, `/?` | Commands and keys. | `/help` |
| `/status` | Dashboard: session, model, safety, integrations, workspace. | `/status` |
| `/clear`, `/new`, `/reset` | Save and start a new conversation. | `/clear` |
| `/model [id]` | Show known models with prices, or switch. Any id works. | `/model claude-sonnet-5-5` |
| `/effort [level]` | Show or set effort. | `/effort max` |
| `/mode [mode]` | Show or set the permission mode (clamped by policy). | `/mode auto-edit` |
| `/thinking` | Toggle reasoning summaries. | `/thinking` |
| `/cost`, `/usage` | Token usage and estimated cost. | `/cost` |
| `/tasks`, `/todos` | The agent's task list. | `/tasks` |
| `/compact [focus]` | Ask the model for a summary and continue from it in a new session. | `/compact keep the API decisions` |
| `/sessions` | Saved sessions for this workspace (newest 15). | `/sessions` |
| `/resume <id>` | Resume a saved session (id prefix works). Without an id, lists sessions. | `/resume 20261113-1201` |
| `/init` | Ask the agent to write an `AGENTS.md` for the repository. | `/init` |
| `/memory` | Which memory files are loaded (`AGENTS.md`, `GTA.md`, `CLAUDE.md` in the workspace root, and `AGENTS.md` in the config dir). | `/memory` |
| `/tools` | Tools the agent can use now. | `/tools` |
| `/config` | Effective settings and config file paths. | `/config` |
| `/exit`, `/quit`, `/q` | Save and exit. | `/exit` |

### Git and undo

| Command | Description | Example |
| --- | --- | --- |
| `/diff [--staged\|--cached] [paths]` | Uncommitted changes with a stat. | `/diff --staged internal/` |
| `/commit <message>` | `git add -A` and commit. | `/commit Fix config precedence` |
| `/log [n]` | Last `n` commits (default 10). | `/log 5` |
| `/undo [yes]` | Preview, then (`yes`) restore the files changed in the last turn. The conversation is not changed. | `/undo yes` |
| `/checkpoints [restore <id> [yes]]` | List checkpoints, preview a restore, or apply it with `yes`. | `/checkpoints restore 3 yes` |
| `/worktree new <name>` (or `add`) | Create a git worktree and branch for parallel work. | `/worktree new fix-login` |
| `/worktree list` | List worktrees (also the default). | `/worktree` |
| `/worktree rm <name> [--force]` (or `remove`) | Remove a worktree, and its branch when merged. | `/worktree rm fix-login` |

### Integrations

| Command | Description | Example |
| --- | --- | --- |
| `/mcp [reconnect <name>]` | MCP server status, or reconnect one. | `/mcp reconnect docs` |
| `/hooks` | Configured hooks. | `/hooks` |
| `/sandbox` | How shell commands are sandboxed. | `/sandbox` |
| `/provider` | Active provider, key variable, URL and known models. | `/provider` |
| `/plugins [reload]` | List plugins, problems and ignored project plugins, or reload from disk. New plugin commands need a restart; tools reload at once. | `/plugins reload` |

### Autopilot

| Command | Description | Example |
| --- | --- | --- |
| `/plan <task>` | A planner writes a plan, a reviewer critiques it, you approve it in a dialog (allow once: execute and ask before edits; allow for session: switch to `auto-edit` and execute; deny: reject), the agent executes it, and a verifier checks the diff and tests against the plan. | `/plan add rate limiting to the HTTP client` |
| `/fixtests [n]` | Run the detected test command and fix failures, up to `n` rounds (1-50, default 5). Stops early when two rounds make no progress. | `/fixtests 3` |

`/plan` and `/fixtests` run like a turn: `esc` interrupts them.

### Code intelligence

| Command | Description | Example |
| --- | --- | --- |
| `/index [refresh]` | Code index stats, or re-scan the workspace. | `/index refresh` |
| `/lsp` | Language servers and their state. | `/lsp` |

### Governance

| Command | Description | Example |
| --- | --- | --- |
| `/policy` | The managed policy in force and what it changed at startup. | `/policy` |
| `/audit` | Audit log location, record count, head hash, redactions this session. | `/audit` |
| `/audit verify` | Check the hash chain. | `/audit verify` |
| `/audit tail [n]` | Last `n` records (default 15). | `/audit tail 20` |
| `/budget` | Spend against the session, daily and monthly caps. | `/budget` |

Commands that return text (not `/plan`, `/fixtests` or prompt commands) are
cancelled after 2 minutes.

## Model tools

What the model can call. `/tools` shows the live list.

| Tool | Kind | Notes |
| --- | --- | --- |
| `read_file`, `list_dir`, `glob`, `grep`, `todo_write` | read | Never ask. |
| `write_file`, `edit_file` | write | An existing file must be read first in the session. |
| `bash` | exec | Sandboxed when `sandbox` is on; timeout `shell_timeout_sec`. |
| `verify` | exec | Runs the detected test command; optional `filter`, `timeout_sec` (default 600, max 1800). |
| `task` | read (`explore`) / exec (`general`) | Up to 8 sub-agents per call, 4 at a time, 40 model calls each. |
| `code_search`, `symbols`, `repo_map` | read | When `code_index` is on. |
| `diagnostics`, `code_nav` | read | When `lsp` is on. |
| `web_fetch`, `web_search` | read | Client-side; non-Anthropic providers, or `client_web_tools`. |
| `mcp__<server>__<tool>` | exec | From MCP servers. |
| `plugin__<plugin>__<tool>` | from the manifest | From plugins. |
