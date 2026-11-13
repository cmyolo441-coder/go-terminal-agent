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

### Enterprise features

| Area | What you get |
| --- | --- |
| **Code intelligence** | A local code index with ranked search (`code_search`), symbol lookup (`symbols`) and a repository map in the system prompt. Language servers (gopls, pyright, typescript-language-server, rust-analyzer, clangd) report compiler errors after every edit so the model fixes them in the same turn. |
| **Autopilot** | `/plan <task>`: a planner writes a plan, a reviewer critiques it, you approve it, the agent executes it and a verifier checks the result against the plan and the tests. `/fixtests`: run the tests and fix failures until they pass. |
| **Governance** | A managed policy file IT can deploy that users cannot loosen, a tamper-evident hash-chained audit log, secret redaction before anything reaches the model, and session, daily and monthly spending caps. |
| **Reliability** | Fallback models across providers, retries with backoff, circuit breakers, rate limits (`rpm`, `tpm`) and a first-token timeout. |
| **Observability** | OpenTelemetry traces and metrics (GenAI conventions) over OTLP/HTTP or to a file. |
| **Extensibility** | Plugins that add tools and slash commands, client-side `web_fetch` and `web_search` for any provider, image attachments, and `gta serve`: a local HTTP API with streaming turns and remote approvals. |

`/status` shows every one of these with its live state.

## Install

Prebuilt binaries for Linux, macOS and Windows, once a release is published
(the installer checks the release's `checksums.txt` before installing; until
the first release, use `go install` below):

```sh
curl -fsSL https://raw.githubusercontent.com/cmyolo441-coder/go-terminal-agent/main/scripts/install.sh | sh
curl -fsSL https://raw.githubusercontent.com/cmyolo441-coder/go-terminal-agent/main/scripts/install.sh | sh -s -- vX.Y.Z   # pin a release (replace vX.Y.Z)
```

On Windows (PowerShell):

```powershell
irm https://raw.githubusercontent.com/cmyolo441-coder/go-terminal-agent/main/scripts/install.ps1 | iex
```

Or build from source (Go 1.26 or newer):

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

Type `@path/to/file` to attach a file (`@screenshot.png` attaches an image),
and `!command` to run a shell command yourself; its output goes with your
next prompt.

| Command | What it does |
| --- | --- |
| `/help` | list commands |
| `/model [id]`, `/effort [level]`, `/mode [mode]`, `/thinking` | settings |
| `/clear`, `/compact`, `/sessions`, `/resume <id>` | conversation |
| `/cost`, `/tasks`, `/tools`, `/config`, `/memory`, `/init` | info and setup |
| `/status` | dashboard of every feature and its live state |
| `/diff [--staged] [paths]`, `/commit <msg>`, `/log [n]` | git |
| `/theme [name]`, `/density [compact\|expanded]` | appearance |
| `/undo [yes]`, `/checkpoints [restore <id> [yes]]` | undo file changes |
| `/worktree new <name> \| list \| rm <name> [--force]` | git worktrees |
| `/mcp [reconnect <name>]`, `/hooks`, `/sandbox`, `/provider`, `/plugins [reload]` | integrations |
| `/plan <task>`, `/fixtests [n]` | autopilot |
| `/index [refresh]`, `/lsp` | code index and language servers |
| `/policy`, `/audit [verify \| tail n]`, `/budget` | governance |
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
file within the limits of [project trust](docs/configuration.md#project-trust). Environment variables `GTA_MODEL`, `GTA_EFFORT`, `GTA_PERMISSION_MODE`,
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
  "trust_project_hooks": false,

  "code_index": true,
  "repo_map_budget": 1500,
  "lsp": true,
  "lsp_diagnostics_wait_ms": 4000,

  "redact_secrets": true,
  "audit_log": false,
  "budget": { "session_usd": 5, "daily_usd": 20, "monthly_usd": 300, "warn_at": 0.8 },

  "fallback_models": [
    { "provider": "anthropic", "model": "claude-sonnet-5-5" },
    { "provider": "nvidia", "model": "kimi-k3.1-pro" }
  ],
  "routing": { "max_retries": 2, "first_token_timeout_sec": 60, "rpm": 0, "tpm": 0 },
  "telemetry": { "enabled": false, "exporter": "otlp", "endpoint": "http://localhost:4318" },

  "disable_plugins": false,
  "client_web_tools": false
}
```

A project's `.gta/config.json` comes from whoever controls the repository,
so unless your **user** config sets `"trust_project_hooks": true` it may only
set preferences (model, effort, theme, `max_turns`, code index settings, …)
and make security settings stricter; hooks, MCP servers, `base_url`,
`provider`, `lsp_servers`, `telemetry` and any loosening are ignored with a
startup notice. See [docs/configuration.md](docs/configuration.md#project-trust).

### Hooks

A hook gets a JSON payload on stdin (`event`, `session_id`, `cwd`,
`tool_name`, `tool_input`, `tool_output`, `is_error`, `prompt`) and the
variables `GTA_PROJECT_DIR`, `GTA_SESSION_ID`, `GTA_EVENT` and `GTA_TOOL_NAME`.

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

## Feature tour

### Code intelligence

GTA indexes the workspace on start (it honours `.gitignore`, caches the index
in your config dir and refreshes changed files only). The model gets:

- `code_search`: ranked search over code by meaning-bearing terms, not just
  regex, with snippets.
- `symbols`: find functions, types and methods by name, including camel-case
  abbreviations (`NewReg` finds `NewRegistry`).
- `repo_map`: the most important files and their key symbols, ranked by how
  much the rest of the code refers to them. A map of about
  `repo_map_budget` tokens is also in the system prompt from the first turn.
- `diagnostics` and `code_nav` (definition, references, hover) from the
  language servers you have installed.

After every file edit, GTA asks the language server for that file's errors
and adds them to the tool result, for example:

```
<diagnostics file="app.py" server="pyright">
The language server reports 1 error(s) in this file after the change:
L4:10 error: Type "int" is not assignable to declared type "str"
</diagnostics>
```

Servers that are not installed are skipped silently; `/lsp` shows which ones
run. Add or override servers with `lsp_servers`:

```json
{ "lsp_servers": [{ "name": "zls", "command": "zls", "extensions": [".zig"], "root_markers": ["build.zig"] }] }
```

### Autopilot

```
❯ /plan add rate limiting to the HTTP client
```

A planner agent explores the code and writes a step-by-step plan, a reviewer
agent critiques it, and you see the result in an approval dialog: run it
asking before each edit, run it with edits auto-approved, or reject it. After execution a verifier compares the diff and the test
results with the plan. `/fixtests 5` runs the test suite and fixes failures,
at most 5 rounds. The `verify` tool lets the model run the project's tests
at any time and get a structured failure report.

### Governance

**Managed policy.** IT can deploy a policy file that every user's GTA obeys.
It lives at `/etc/gta/policy.json` (Linux),
`/Library/Application Support/GTA/policy.json` (macOS) or
`%ProgramData%\GTA\policy.json` (Windows); `GTA_POLICY_FILE` adds one more
file, which can only tighten the rules. An invalid policy stops GTA from
starting rather than running unmanaged.

```json
{
  "version": 1,
  "message": "Blocked by company policy. Ask #dev-tools for an exception.",
  "max_permission_mode": "auto-edit",
  "commands": { "deny": ["curl", "wget", "git push --force"] },
  "paths": { "deny": ["~/.ssh", "~/.aws"], "deny_write": [".github/workflows"] },
  "providers": { "allow": ["anthropic"] },
  "models": { "allow": ["claude-*"] },
  "sandbox": { "mode": "workspace-write", "network": false },
  "audit": { "required": true },
  "budget": { "daily_usd": 50 },
  "secrets": { "redact": true }
}
```

A denied call is refused before any approval prompt, the model sees the
message, and the decision is audited. `/policy` shows the active rules.

**Audit log.** With `"audit_log": true` (or a policy that requires it), every
tool call, approval, model call and policy decision is appended to a
hash-chained log under your config dir. `/audit verify` checks that no record
was changed or removed; `/audit tail 20` shows the latest records.

**Secret redaction.** API keys, tokens, private keys and passwords are
replaced with placeholders in everything sent to the model, while your local
files and history keep the real values. `secrets_allow` lists strings that
are never redacted.

**Budgets.** `budget.session_usd`, `daily_usd` and `monthly_usd` cap spending;
GTA warns at `warn_at` (80% by default) and stops new model calls at the
cap. `/budget` and `/cost` show where you stand.

### Reliability

When the main model is overloaded, rate limited or down, GTA retries with
backoff and then moves to the next entry of `fallback_models`, across
providers, and tells you:

```
! Routing: primary claude-opus-5-5 failed (overloaded); falling back to anthropic claude-sonnet-5-5
```

A route that keeps failing is skipped for a while (circuit breaker), and
`rpm` / `tpm` keep you under a provider's limits. The router (and the `routing`
settings) is active when `fallback_models` is set.

### Observability

Set `"telemetry": {"enabled": true}` or `OTEL_EXPORTER_OTLP_ENDPOINT` to
export OpenTelemetry traces (`invoke_agent`, `chat`, `execute_tool` spans)
and metrics (token usage, operation duration, tool calls, retries,
fallbacks) to any OTLP/HTTP collector. `"exporter": "file"` with
`"file": "gta-otel.jsonl"` writes them locally instead.

### Plugins

A plugin is a directory with a `plugin.json` in `~/.config/gta/plugins/<name>/`
(or `.gta/plugins/` in a trusted project):

```json
{
  "name": "jira",
  "version": "1.0.0",
  "description": "Jira helpers",
  "pass_env": ["JIRA_TOKEN"],
  "tools": [{
    "name": "jira_issue",
    "description": "Fetch a Jira issue by key",
    "kind": "read",
    "input_schema": { "type": "object", "properties": { "key": { "type": "string" } }, "required": ["key"] },
    "command": "./jira-issue"
  }],
  "commands": [{ "name": "standup", "description": "Draft my standup", "prompt": "Summarize my commits since yesterday. {{args}}" }]
}
```

A tool gets its input as JSON on stdin and prints
`{"output": "...", "is_error": false}`. It runs with a timeout, a capped
output and a scrubbed environment. `/plugins` lists plugins and
`/plugins reload` reloads them.

### Web tools

Non-Anthropic providers (and Anthropic with `"client_web_tools": true`) get
`web_fetch` and `web_search`. Search uses `BRAVE_API_KEY`,
`TAVILY_API_KEY` or `SEARXNG_URL`, whichever is set.

### Server mode

```sh
gta serve                      # listens on 127.0.0.1:7878
TOKEN=$(cat ~/.config/gta/serve.token)
curl -s -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"mode":"ask"}' localhost:7878/v1/sessions
curl -N -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"text":"run the tests"}' localhost:7878/v1/sessions/<id>/messages
```

A turn streams Server-Sent Events (`text`, `tool_call`, `tool_done`,
`approval_required`, `usage`, `done`, …). Answer an approval with
`POST /v1/sessions/<id>/approvals/<request_id>` and
`{"decision": "allow_once"}`. The server listens on loopback only unless you
pass `--allow-remote`, and every request needs the bearer token (from
`GTA_SERVE_TOKEN`, or generated and saved to `--token-file`). The same
policy, audit, budgets and sandbox apply to every session.

## Headless mode

```sh
gta -p "add a unit test for parseConfig" --mode auto-edit --json
```

Text streams to stdout (or one JSON object with `--json`), tool activity to
stderr. Actions that would need approval are declined, so choose a
permission mode that fits the job. The session is saved, so `gta -c` can
pick it up interactively.

## Documentation

Full reference docs are in [docs/](docs/README.md): commands, configuration,
architecture, security, plugins, the server API and observability.

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
| `internal/codeintel` | code index, search, symbols and repository map |
| `internal/lsp` | language server client and diagnostics |
| `internal/autopilot` | `/plan` and `/fixtests` loops and the `verify` tool |
| `internal/policy`, `internal/audit`, `internal/secrets`, `internal/budget` | governance |
| `internal/router`, `internal/telemetry` | fallback routing and OpenTelemetry |
| `internal/plugin`, `internal/webtools`, `internal/server` | plugins, web tools, HTTP API |
| `internal/config`, `internal/session`, `internal/prompt`, `internal/pricing` | support |
