# Configuration

GTA reads JSON configuration from two files, then environment variables,
then command-line flags. A managed policy (see [security.md](security.md))
is applied last and can only tighten the result.

## Files

| File | Location |
| --- | --- |
| User config | `$GTA_CONFIG_DIR/config.json`, else `<os config dir>/gta/config.json` (Linux: `~/.config/gta/config.json`, macOS: `~/Library/Application Support/gta/config.json`), else `~/.gta/config.json` |
| Project config | `<workspace>/.gta/config.json` |

The workspace is the current directory, or `--cwd`. Both files are optional.
Unknown keys are an error (a typo stops GTA from starting instead of being
ignored). Each file only overrides the keys it contains.

The same user config directory also holds: `sessions/`, `checkpoints/`,
`index/` (code index cache), `audit/` (default audit log), `budget.json`
(spend ledger), `plugins/`, `serve.token` and an optional user-level
`AGENTS.md` that is added to the system prompt.

## Precedence

```
built-in defaults  <  user config  <  project config  <  environment  <  flags  →  managed policy clamps
```

Exceptions, all enforced in `internal/config` and `cmd/gta`:

- Unless the user config sets `"trust_project_hooks": true`, the project
  config is **untrusted**: it may set preferences and tighten security
  settings, and every other key it sets is ignored. See
  [Project trust](#project-trust).
- `hooks` and `mcp_servers` are **merged**, not replaced: user entries always
  apply; project entries apply only when the user config sets
  `"trust_project_hooks": true`.
- `trust_project_hooks` is read from the user config only. A project file
  cannot grant itself trust.
- `ANTHROPIC_BASE_URL` is used only when no config file sets `base_url` and
  the provider is `anthropic`.
- With a non-Anthropic provider, a `model` that still starts with `claude-`
  (and was not given with `--model`) is replaced by the preset's first known
  model, and a `max_tokens` still at the default 64000 becomes 8192.
- The managed policy runs after flags and can lower `permission_mode`,
  force `sandbox`, turn off web tools, plugins, MCP or hooks, remove
  `allow_commands` entries and tighten `budget`. Each change is printed at
  startup (`gta: policy: ...`) and audited.

## Fields

Defaults are from `config.Default()`. "—" means the zero value (empty,
`false` or `0`).

### Model and conversation

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `model` | string | `"claude-opus-5-5"` | Model id. Free-form; any id the provider serves. Must not be empty. |
| `effort` | string | `"high"` | Reasoning effort: `low`, `medium`, `high`, `xhigh`, `max`. |
| `max_tokens` | int | `64000` | Output cap per model call, 1024 to 128000. |
| `show_thinking` | bool | `true` | Show the model's reasoning summaries. |
| `max_turns` | int | `200` | Maximum model calls per prompt (at least 1). |
| `provider` | string | `"anthropic"` | `anthropic`, or an OpenAI-compatible preset: `openai`, `openrouter`, `ollama`, `gemini`, `groq`, `deepseek`, `together`, `lmstudio`, `vllm`, `nvidia`. |
| `base_url` | string | — | Override the provider's API URL. |
| `subagent_model` | string | — | Model for `task` sub-agents and the `/plan` planner and reviewer. Empty means the main model. |
| `theme` | string | `"auto"` | Markdown style: `auto`, `dark`, `light`. |
| `density` | string | `"focus"` | How tool calls show in the transcript: `focus` (one ✓/✗ line per call, with the command or target), `compact` (output collapsed after a few lines) or `expanded`. `/density` switches at runtime. |

### Anthropic server-side features

These only affect the `anthropic` provider.

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `web_search` | bool | `false` | Anthropic's server-side web search tool. `--web` turns on both web tools. |
| `web_fetch` | bool | `false` | Anthropic's server-side web fetch tool. |
| `compaction` | bool | `false` | Server-side context compaction for long sessions. |
| `fallbacks` | bool | `true` | Server-side refusal fallbacks. |
| `client_web_tools` | bool | `false` | Add GTA's own `web_fetch` and `web_search` tools for Claude too. They are always on for other providers. `web_search` needs `BRAVE_API_KEY`, `TAVILY_API_KEY` or `SEARXNG_URL`. |

### Permissions and shell

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `permission_mode` | string | `"ask"` | `ask`, `auto-edit`, `plan` or `yolo`. See [security.md](security.md#permission-modes). |
| `allow_commands` | string[] | `ls`, `pwd`, `git status`, `git diff`, `git log`, `git show`, `git branch`, `go build`, `go test`, `go vet`, `go version`, `cat`, `head`, `tail`, `wc` | Command prefixes that run without asking in `ask` and `auto-edit` mode. A command matches when it equals an entry or starts with the entry plus a space. Commands containing `;`, `|`, `&`, `` ` ``, `$(`, `>` or a newline never match. Setting the key replaces the default list. |
| `shell_timeout_sec` | int | `120` | Timeout of the `bash` tool. |
| `sandbox` | string | `"off"` | Shell sandbox: `off`, `workspace-write`, `read-only`. If set but unavailable, GTA refuses to start. |
| `sandbox_network` | bool | `false` | Keep network access inside the sandbox. |

### Hooks, MCP and plugins

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `hooks` | object | — | Map of event to a list of hooks. See [Hooks](#hooks). |
| `trust_project_hooks` | bool | `false` | User config only. Trusts the project: every key of its `.gta/config.json` applies (including hooks and MCP servers) and `.gta/plugins/` load. See [Project trust](#project-trust). |
| `mcp_servers` | object | — | Map of server name to server config. See [MCP servers](#mcp-servers). |
| `disable_plugins` | bool | `false` | Turn the plugin system off. |
| `disabled_plugins` | string[] | — | Plugin names not to load. |

### Governance

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `redact_secrets` | bool | `true` | Replace credentials with placeholders in everything sent to the model. |
| `secrets_allow` | string[] | — | Exact strings that are never redacted. |
| `audit_log` | bool | `false` | Write the hash-chained audit log. |
| `audit_dir` | string | `<config dir>/audit` | Audit log directory. `~/` is expanded. A policy `audit.dir` wins. |
| `audit_sync` | string | `"always"` | `always` (fsync every record), `interval` (fsync every second), `never`. A policy `audit.sync` wins. |
| `budget` | object | no caps | `session_usd`, `daily_usd`, `monthly_usd` (USD, 0 = no cap, not negative) and `warn_at` (fraction in 0..1, default 0.8). |

### Reliability

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `fallback_models` | object[] | — | Routes tried in order after the main model. Each: `provider` (default: the main provider), `model` (required), `max_tokens`, `rpm`, `tpm`. Non-Anthropic fallbacks get no effort and `max_tokens` 8192 unless set. |
| `routing` | object | see below | Retry and breaker tuning. **Used only when `fallback_models` is non-empty.** |

`routing` keys (all optional ints, 0 means the router default):

| Key | Default | Description |
| --- | --- | --- |
| `max_retries` | 2 | Retries per route before falling back. Negative: none. |
| `first_token_timeout_sec` | off | Treat a call that streamed nothing in this time as a retryable timeout. |
| `max_rate_wait_sec` | 30 | Longest wait for a route's client-side rate limit before trying the next route. |
| `breaker_failures` | 5 | Consecutive health failures that open a route's circuit breaker. |
| `breaker_cooldown_sec` | 30 | How long an open breaker skips the route (doubles on re-open, up to 5 minutes). |
| `retry_after_stream` | false (bool) | Allow retry/fallback after part of a reply already streamed. |
| `rpm`, `tpm` | unlimited | Client-side requests and estimated tokens per minute for the main model. |

### Observability

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `telemetry` | object | off | OpenTelemetry export. See [observability.md](observability.md). |

### Code intelligence

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `code_index` | bool | `true` | Build the local index used by `code_search`, `symbols` and `repo_map`. |
| `code_index_max_file_size` | int | `1048576` | Skip files larger than this (bytes). |
| `code_index_ignore` | string[] | — | Extra gitignore-style patterns, relative to the workspace. |
| `repo_map_budget` | int | `1500` | Approximate tokens of repository map added to the system prompt. 0 leaves it out. |
| `lsp` | bool | `true` | Run installed language servers for diagnostics after edits and for `diagnostics` / `code_nav`. |
| `lsp_servers` | object[] | — | Add or override servers. See [Language servers](#language-servers). |
| `lsp_diagnostics_wait_ms` | int | `4000` | How long to wait for diagnostics after an edit. |

`repo_map_budget`, `code_index_max_file_size` and `lsp_diagnostics_wait_ms`
cannot be negative.

## Hooks

```json
{
  "hooks": {
    "pre_tool_use":       [{ "matcher": "bash", "command": "./scripts/check.sh", "timeout_sec": 10 }],
    "post_tool_use":      [{ "matcher": "write_file|edit_file", "command": "gofmt -l ." }],
    "user_prompt_submit": [{ "command": "git status --short" }],
    "stop":               [{ "command": "notify-send 'GTA finished'" }],
    "session_start":      [{ "command": "cat .gta/context.md" }]
  }
}
```

| Field | Description |
| --- | --- |
| `command` | Required. Run with `bash -c` (`sh -c` without bash, `cmd /C` on Windows) in the workspace root. |
| `matcher` | Regular expression matched against the whole tool name (`^(?:matcher)$`). Empty or `*` matches all. Only for `pre_tool_use` and `post_tool_use`. |
| `timeout_sec` | 0 to 600; 0 means 60 seconds. |

Each hook gets a JSON payload on stdin with `event`, `session_id`, `cwd` and,
when they apply, `tool_name`, `tool_input`, `tool_output`, `is_error`,
`prompt`. The environment adds `GTA_EVENT`, `GTA_TOOL_NAME`,
`GTA_SESSION_ID` and `GTA_PROJECT_DIR`.

| Result | Effect |
| --- | --- |
| exit 0 | Continue. stdout of `user_prompt_submit` and `session_start` is added to the model's context; other stdout is shown to the user. |
| exit 2 | Block; stderr is the reason. `pre_tool_use`: the call is refused. `user_prompt_submit`: the prompt is rejected. `stop`: the model is sent back to work with the reason (at most 3 times per turn). `post_tool_use`: the reason is appended to the tool result. `session_start` cannot block (warning only). |
| other exit | Warning, continue. |
| stdout `{"decision":"block","reason":"..."}` | Same as exit 2, overrides the exit code. |
| stdout `{"decision":"allow"}` (or `"approve"`) | From `pre_tool_use`: approve the call without a prompt. Destructive commands still ask. Policy denials still apply. |

`gta serve` does not run `session_start` hooks.

## MCP servers

```json
{
  "mcp_servers": {
    "fs":   { "command": "npx", "args": ["-y", "@modelcontextprotocol/server-filesystem", "."] },
    "docs": { "url": "https://example.com/mcp", "headers": { "Authorization": "Bearer ${DOCS_TOKEN}" } }
  }
}
```

| Field | Description |
| --- | --- |
| `command`, `args`, `env` | A stdio server. |
| `url`, `headers` | A Streamable HTTP server. |
| `disabled` | Keep the entry but do not start it. |
| `timeout_sec` | Per-request timeout, default 60, at most 3600. |

Set exactly one of `command` and `url`. Server names are 1-64 characters of
`[a-zA-Z0-9_-]`. `args`, `env`, `url` and `headers` may use `${NAME}` or
`${NAME:-default}`. Tools appear as `mcp__<server>__<tool>`; they are treated
as commands for permissions (they ask in `ask` and `auto-edit` mode).

## Language servers

Built-in: `gopls`, `typescript-language-server --stdio`,
`pyright-langserver --stdio`, `rust-analyzer`, `clangd`. Servers not on
`PATH` are skipped.

```json
{ "lsp_servers": [
  { "name": "zls", "command": "zls", "extensions": [".zig"], "root_markers": ["build.zig"] },
  { "name": "clangd", "disabled": true }
] }
```

| Field | Description |
| --- | --- |
| `name` | Required. An entry with a built-in name replaces that server. |
| `command`, `args`, `env` | How to start it. |
| `extensions` | File extensions it handles (required for new servers). |
| `language_ids` | Map of extension to LSP `languageId`. |
| `root_markers` | Files whose directory is the server root (nearest wins). |
| `initialization_options`, `settings` | JSON passed to the server. |
| `disabled` | Turn a built-in server off. |

## Environment variables

| Variable | Effect |
| --- | --- |
| `GTA_CONFIG_DIR` | User config directory. |
| `GTA_MODEL`, `GTA_EFFORT`, `GTA_PERMISSION_MODE`, `GTA_MAX_TOKENS`, `GTA_PROVIDER`, `GTA_SANDBOX` | Override the matching config key. |
| `ANTHROPIC_BASE_URL` | Anthropic API URL, when no `base_url` is configured. |
| `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN` | Anthropic credentials (or an `ant auth login` profile). |
| `OPENAI_API_KEY`, `OPENROUTER_API_KEY`, `GEMINI_API_KEY` / `GOOGLE_API_KEY`, `GROQ_API_KEY`, `DEEPSEEK_API_KEY`, `TOGETHER_API_KEY`, `NVIDIA_API_KEY`, `OLLAMA_API_KEY`, `LMSTUDIO_API_KEY`, `VLLM_API_KEY` | Preset keys. |
| `OPENAI_BASE_URL`, `OLLAMA_HOST` | Override the `openai` and `ollama` preset URLs. |
| `BRAVE_API_KEY`, `TAVILY_API_KEY`, `SEARXNG_URL`, `GTA_WEB_SEARCH_BACKEND` | Client-side `web_search` backend (`brave`, `tavily` or `searxng`; default: the first one configured). |
| `HTTPS_PROXY` / `https_proxy` | Proxy for client-side `web_fetch`. |
| `GTA_POLICY_FILE` | An extra managed policy file that can only add restrictions. |
| `GTA_SERVE_TOKEN` | Bearer token for `gta serve` (at least 16 characters). |
| `OTEL_*` | OpenTelemetry settings; see [observability.md](observability.md). |

Keys are never read from config files and GTA never writes one to disk.

## Project trust

A cloned repository's `.gta/config.json` and `.gta/plugins/` are written by
whoever controls the repository, so they are **untrusted** unless the
**user** config sets `"trust_project_hooks": true`. The rule for an untrusted
project file:

1. **Preferences apply as written.** They cannot run a command, send data
   anywhere new or weaken a protection.
2. **Security settings apply only if they are at least as strict** as the
   setting from defaults and the user config. A looser value is ignored and
   the user's value stays.
3. **Every other key is ignored**, including keys added in later versions
   until they are classified.

GTA prints one startup notice naming every key that was ignored or only
partly applied (`gta: this project is not trusted, so these settings in
.gta/config.json were ignored: base_url, permission_mode ...`), plus a
separate one for ignored hooks and MCP servers, and `/status` shows them
under *project config*. The file is still parsed strictly: unknown keys,
trailing data and invalid values stop GTA
from starting.

| Class | Keys | Without `trust_project_hooks` |
| --- | --- | --- |
| Preference | `model`, `effort`, `max_tokens`, `show_thinking`, `compaction`, `fallbacks`, `max_turns`, `shell_timeout_sec`, `theme`, `density`, `subagent_model`, `routing`, `code_index`, `code_index_max_file_size`, `code_index_ignore`, `repo_map_budget`, `lsp_diagnostics_wait_ms` | Applied |
| Tighten only | `permission_mode` | Applied if not more permissive (`plan` < `ask` < `auto-edit` < `yolo`) |
| | `sandbox` | Applied if not weaker (`off` < `workspace-write` < `read-only`) |
| | `allow_commands` | Only entries already in the user's list are kept |
| | `sandbox_network`, `web_search`, `web_fetch`, `client_web_tools`, `lsp` | Only `false` applies |
| | `redact_secrets`, `audit_log`, `disable_plugins` | Only `true` applies |
| | `disabled_plugins` | Added to the user's list |
| | `audit_sync` | Applied if not less durable (`never` < `interval` < `always`) |
| | `budget` | Each cap is tightened, never raised or removed (as with a policy) |
| | `trust_project_hooks` | Never applies; a project cannot grant itself trust |
| Ignored | `provider`, `base_url`, `fallback_models`, `telemetry`, `lsp_servers`, `hooks`, `mcp_servers`, `secrets_allow`, `audit_dir` | Ignored |
| Plugins | `.gta/plugins/*` | Not loaded; listed by `/plugins` as untrusted |

With `trust_project_hooks` in the user config, every key of the project file
applies as before: it overrides the user config, project `hooks` are
appended after user hooks, project `mcp_servers` are added (same name
replaces the user entry), and `.gta/plugins/*` are loaded after user
plugins. Only trust repositories whose configuration you would run yourself.

Environment variables and flags are the user's own and are not restricted
(`GTA_PERMISSION_MODE=yolo` still works). A managed policy applies on top
either way.

## Full example

```json
{
  "model": "claude-opus-5-5",
  "effort": "high",
  "permission_mode": "ask",
  "allow_commands": ["ls", "git status", "go test"],
  "sandbox": "workspace-write",
  "hooks": { "post_tool_use": [{ "matcher": "write_file|edit_file", "command": "gofmt -l ." }] },
  "audit_log": true,
  "budget": { "session_usd": 5, "daily_usd": 20, "monthly_usd": 300, "warn_at": 0.8 },
  "fallback_models": [{ "provider": "anthropic", "model": "claude-sonnet-5-5" }],
  "routing": { "max_retries": 2, "first_token_timeout_sec": 60 },
  "telemetry": { "enabled": true, "endpoint": "http://localhost:4318" }
}
```
