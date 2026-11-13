# Security

This page describes what GTA enforces, in the order a tool call meets it,
and what it does not protect.

## Order of checks for a tool call

1. The tool validates its input (`Prepare`). Invalid input never runs.
2. `pre_tool_use` hooks run. A block refuses the call; `{"decision":"allow"}`
   pre-approves it.
3. If the audit log is **required** and failing, every tool call is refused.
4. The **managed policy** checks the call. A denial is final: the user is
   never asked, the model gets the reason and the policy message, and the
   denial is audited.
5. The **permission gate** decides allow, ask or deny for the current mode.
6. If it asks, the user (or the API client) answers: allow once, allow for
   the session, or deny. A denial ends the turn.
7. The call runs. `bash` (and `verify`, `/fixtests`, `!command`) run inside
   the **sandbox** when one is configured.
8. The result is audited, `post_tool_use` hooks run, and before the next
   model call everything is passed through **secret redaction** and the
   **budget** check.

## Permission modes

Tools have a kind: **read**, **write** or **exec**.

| Kind | `plan` | `ask` (default) | `auto-edit` | `yolo` |
| --- | --- | --- | --- | --- |
| read | allow | allow | allow | allow |
| write inside the workspace | deny | ask | allow | allow |
| write outside the workspace | deny | ask | ask | allow |
| exec matching `allow_commands` | deny | allow | allow | allow |
| other exec | deny | ask | ask | allow |
| exec that looks destructive | deny | ask | ask | **ask** |

- Read tools never ask, in any mode, including `read_file` on paths
  outside the workspace. Use policy `paths.deny` to protect files.
- "Allow for the session" grants all file writes, or, for a command, its
  program plus subcommand (`go test`, `npm run`). Commands that chain
  (`;`, `|`, `&`, `` ` ``, `$(`, `>`, newline) have no prefix and always ask.
- Destructive patterns: `rm -rf` on `/`, `~`, `$HOME` or `*`; `mkfs`;
  `dd ... of=/dev/`; fork bombs; `shutdown`; `reboot`;
  `git push --force` / `-f`; `git reset --hard`; `git clean -f`;
  `> /dev/sd*`; `chmod -R 777 /`.
- A `pre_tool_use` hook approval cannot approve a destructive command.
- MCP tools are exec; plugin tools have the kind their manifest declares.
- `max_permission_mode` in a managed policy caps every mode change,
  including `shift+tab` and `/mode`.

## Untrusted project configuration

A repository's `.gta/config.json` is attacker-controlled when you open a
repository you just cloned. Unless your **user** config sets
`"trust_project_hooks": true`, GTA applies it under one rule:

- **Preferences apply**: `model`, `effort`, `max_tokens`, `show_thinking`,
  `compaction`, `fallbacks`, `max_turns`, `shell_timeout_sec`, `theme`,
  `density`, `subagent_model`, `routing`, `code_index`,
  `code_index_max_file_size`, `code_index_ignore`, `repo_map_budget`,
  `lsp_diagnostics_wait_ms`.
- **Security settings may only tighten**: a stricter `permission_mode` or
  `sandbox`; `allow_commands` entries the user already allows;
  `sandbox_network`, `web_search`, `web_fetch`, `client_web_tools` and
  `lsp` only turned off; `redact_secrets`, `audit_log` and
  `disable_plugins` only turned on; more `disabled_plugins`; a more durable
  `audit_sync`; lower `budget` caps.
- **Everything else is ignored**: `provider`, `base_url`,
  `fallback_models` and `telemetry` (where your code, API key and metadata
  are sent), `lsp_servers`, `hooks` and `mcp_servers` (commands GTA
  starts), `secrets_allow`, `audit_dir`, `trust_project_hooks`, and any
  key not classified yet.

One startup notice names every ignored (or only partly applied) key, and
`/status` repeats it. The full table is in
[configuration.md](configuration.md#project-trust). Environment variables
and flags are yours and are not restricted.

## Sandbox

`"sandbox"` (or `--sandbox`, `GTA_SANDBOX`) confines shell commands:

| Mode | Writes allowed | Reads |
| --- | --- | --- |
| `off` | anywhere the user can | everything |
| `workspace-write` | workspace root and a temporary directory | everything |
| `read-only` | a temporary directory only | everything |

Network is cut unless `"sandbox_network": true`. Linux uses bubblewrap
(`bwrap`, package `bubblewrap`; the whole file system is bound read-only,
with a new PID namespace, a fresh `/tmp`, and a new network namespace when network is off). macOS uses `sandbox-exec`. If a
sandbox is configured but cannot run, GTA refuses to start. `/sandbox` shows
the active setup.

The sandbox covers `bash`, the `verify` tool, `/fixtests` and `!command`.
It does **not** cover hooks, MCP servers, plugin tools and commands, or
language servers.

## Managed policy

An administrator installs a policy file that every GTA process on the
machine obeys and users cannot loosen.

| Platform | Path |
| --- | --- |
| Linux and others | `/etc/gta/policy.json` |
| macOS | `/Library/Application Support/GTA/policy.json` |
| Windows | `%ProgramData%\GTA\policy.json` |

`GTA_POLICY_FILE` names one more file. It is layered on the system file and
can only add restrictions. A missing file means no policy. A file that
exists but is invalid, too large (over 1 MiB), not a regular file or
world-writable stops GTA from starting. Unknown fields, unknown feature
names, bad patterns and trailing data are errors.

With several files: any file's deny refuses; every file's allow list must
admit. Deny wins over allow.

### Fields

| Field | Type | Description |
| --- | --- | --- |
| `version` | int | Required, must be `1`. |
| `message` | string | Shown with every denial (for example where to ask for an exception). |
| `tools.allow` / `tools.deny` | string[] | Tool names with `*` and `?` wildcards, including `mcp__server__tool` and `plugin__name__tool`. With an allow list, every other tool is refused (built-ins included). |
| `commands.allow` / `commands.deny` | string[] | Patterns for each simple command the `bash` tool would run. See below. |
| `paths.deny` | string[] | Paths no tool may read or write. |
| `paths.deny_read` | string[] | Paths no tool may read. |
| `paths.deny_write` | string[] | Paths no tool may write. |
| `providers.allow` / `providers.deny` | string[] | Provider names (case-insensitive, wildcards). |
| `models.allow` / `models.deny` | string[] | Model ids (case-insensitive, wildcards). Checked for the main model, `subagent_model` and every fallback. |
| `max_permission_mode` | string | Most permissive mode allowed. Order: `plan` < `ask` < `auto-edit` < `yolo`. |
| `sandbox.mode` | string | Weakest sandbox allowed: `workspace-write` or `read-only` (`read-only` is stronger). |
| `sandbox.network` | bool | `false` forces network off inside the sandbox. |
| `disabled_features` | string[] | `web`, `web_search`, `web_fetch`, `mcp`, `hooks`, `plugins`, `subagents`. |
| `audit.required` | bool | GTA refuses to start if the audit log cannot be opened, and stops running tools if it cannot be written. |
| `audit.dir` | string | Audit log directory (overrides the user's `audit_dir`). |
| `audit.sync` | string | `always`, `interval` or `never`. The most durable value across files wins over the user's `audit_sync`. |
| `audit.hmac_key_file` | string | File holding a key; records are then chained with HMAC-SHA256. |
| `budget` | object | `session_usd`, `daily_usd`, `monthly_usd`, `warn_at`. The stricter of user and policy caps applies. A model with no known price is blocked while any policy cap is set. |
| `secrets.redact` | bool | Force secret redaction on. |
| `secrets.rules` | object[] | Extra detectors: `id` (`[a-z0-9][a-z0-9-]{0,47}`), `pattern` (Go regexp), optional `keywords` (each at least 2 bytes; the rule runs only near them). |

### What the policy changes at startup

The user's configuration is clamped and each change printed and audited:

| Condition | Change |
| --- | --- |
| Main provider or model not allowed | GTA refuses to start. |
| `subagent_model` not allowed | Cleared (the main model is used). |
| A fallback model not allowed | GTA refuses to start. |
| Mode above `max_permission_mode` | Lowered to the maximum. |
| Sandbox weaker than `sandbox.mode` | Raised. |
| `sandbox_network` true and `sandbox.network` false | Set to false. |
| `web_search` / `web_fetch` / `client_web_tools` with the feature disabled | Turned off. |
| `plugins` disabled | `disable_plugins` set. |
| `mcp` disabled | All `mcp_servers` removed. |
| `hooks` disabled | All hooks removed, project trust off. |
| An `allow_commands` entry the command rules deny | Removed. |
| User budget caps looser than policy caps | Tightened. |

At runtime, a disabled feature also refuses its tools: `web` / `web_search`
/ `web_fetch` (the web tools), `mcp` (`mcp__*`), `plugins` (`plugin__*`),
`subagents` (`task`).

### Command patterns

GTA parses the command line the way a shell would and checks **every**
simple command in it: commands after `;`, `&&`, `||`, `|`; inside `$(...)`
and backticks; the `-c` script of `sh`, `bash`, `zsh` and other shells; and
behind wrappers such as `sudo`, `env`, `nohup`, `timeout`, `xargs`, `nice`,
`time`, `exec`, `strace`. Environment assignments are stripped and the
program is reduced to its base name, so `FOO=1 /usr/bin/curl x`, `sudo curl
x`, `echo $(curl x)` and `bash -c 'curl x'` all count as `curl`.

| Pattern | Matches |
| --- | --- |
| `curl` | `curl` alone or followed by arguments. |
| `git push --force` | that command with any further arguments. |
| `rm -rf *` | `*` matches anything; a trailing ` *` also matches the bare `rm -rf`. |
| `re:<regexp>` | Go regular expression. In `commands.deny` it is matched against the whole normalized command line as well as each simple command. |

Arguments, `--flag=value` values and redirection targets of each simple
command are also checked against the path rules.

### Path patterns

- `~` and `~/...` expand to the user's home directory.
- Absolute patterns are matched as written, using `**` and `*` globs
  (doublestar syntax).
- Relative patterns match anywhere: `.env` becomes `**/.env`.
- A pattern also covers everything under a matching directory: `~/.ssh`
  protects `~/.ssh/id_ed25519`.
- Symlinks are resolved on both the pattern's prefix and the checked path.
- Matching is case-insensitive on macOS and Windows.

The checked path is the tool call's target, or the `path` / `file_path`
field of its input (this also covers MCP and plugin tools that use those
names).

### Examples

Lock-down for a regulated team:

```json
{
  "version": 1,
  "message": "Blocked by company policy. Ask #dev-tools for an exception.",
  "max_permission_mode": "auto-edit",
  "commands": { "deny": ["curl", "wget", "git push --force", "re:\\bnc\\b"] },
  "paths": { "deny": ["~/.ssh", "~/.aws", ".env"], "deny_write": [".github/workflows"] },
  "providers": { "allow": ["anthropic"] },
  "models": { "allow": ["claude-*"] },
  "sandbox": { "mode": "workspace-write", "network": false },
  "disabled_features": ["plugins", "web"],
  "audit": { "required": true, "dir": "/var/log/gta", "sync": "always", "hmac_key_file": "/etc/gta/audit.key" },
  "budget": { "daily_usd": 50, "monthly_usd": 500 },
  "secrets": {
    "redact": true,
    "rules": [{ "id": "acme-token", "pattern": "acme_[A-Za-z0-9]{32}", "keywords": ["acme_"] }]
  }
}
```

Allow-list only read tools and a few commands:

```json
{
  "version": 1,
  "tools": { "allow": ["read_file", "list_dir", "glob", "grep", "todo_write", "bash"] },
  "commands": { "allow": ["ls", "git status", "git diff", "go test"] }
}
```

`/policy` shows the rules in force and what was changed at startup.

## Audit log

Enabled by `"audit_log": true` or a policy with `audit.required`.

- Directory: policy `audit.dir`, else `audit_dir`, else
  `<config dir>/audit`. Created `0700`; files are `0600`.
- Files: JSON Lines, one segment per UTC day: `audit-2026-11-13.jsonl`,
  rotated at 64 MiB into `audit-2026-11-13.0001.jsonl`, and so on.
- Several GTA processes can share one directory; appends take a
  cross-process lock (`.audit.lock`).
- A torn final line left by a crash is cut off and a `log_recovered`
  record is written.

### Events

| `event` | Written when |
| --- | --- |
| `session_start` / `session_end` | The process starts / exits (with provider and model at start). |
| `tool_call` | A tool ran, or the gate denied it (`decision`: `allow`, `allow_once`, `allow_session`, `hook`, `deny`). |
| `approval` | The user answered a prompt (`allow_once`, `allow_session`, `declined`). |
| `model_call` | A model call finished (tokens, cost, duration, error). |
| `policy_decision` | The policy denied a tool call (`details.rule`, `details.source`). |
| `config_change` | The policy changed a setting at startup. |
| `budget` | A model call was blocked by a cap (`decision`: `block`). |
| `secrets_redacted` | Secrets were redacted before a model call. |
| `log_recovered` | A torn line was removed after a crash. |

### Record format

```json
{"v":1,"seq":2,"time":"2026-11-13T16:52:34.688823432Z","event":"tool_call","session":"20261113-120000-abc123","user":"me","host":"laptop","pid":14738,"workspace":"/home/me/project","tool":"bash","tool_use_id":"toolu_01","kind":"exec","input":"{\"command\":\"go test ./...\",\"token\":\"[REDACTED:github-token:58d4]\"}","decision":"allow_once","duration_ms":1200,"redactions":1,"alg":"sha256","prev_hash":"b0a1a258…","hash":"3a436ca4…"}
```

| Field | Description |
| --- | --- |
| `v` | Record format version, `1`. |
| `seq` | Sequence number, continuous across segments. |
| `time` | UTC timestamp. |
| `event` | See above. |
| `session`, `user`, `host`, `pid`, `workspace` | Identity. `session` is `serve` for `gta serve`. |
| `tool`, `tool_use_id`, `kind`, `input` | Tool call details. `input` is redacted and truncated to 4096 bytes. |
| `decision`, `reason`, `error`, `duration_ms` | Outcome. |
| `provider`, `model`, `input_tokens`, `output_tokens`, `cache_read_tokens`, `cache_write_tokens`, `cost_usd` | Model call details. |
| `redactions` | Secrets removed from this record. |
| `details` | Extra string map (at most 64 keys). |
| `alg` | `sha256` or `hmac-sha256`. |
| `prev_hash` | Hash of the previous record; 64 zeros for the first. |
| `hash` | Always the last field. |

`hash` is SHA-256 (or HMAC-SHA256 with the policy key) over the record's
JSON with the trailing `,"hash":"…"` removed and `}` put back. Editing,
deleting, inserting or reordering a record breaks the chain.

### Verifying

In GTA: `/audit verify`. It checks every segment in order and reports the
first broken link (file, line, sequence, reason).

Outside GTA, this script does the same check (Python 3, no dependencies):

```python
#!/usr/bin/env python3
"""Verify a GTA audit log directory: verify_audit.py DIR [HMAC_KEY_FILE]"""
import hashlib, hmac, json, os, re, sys

d = sys.argv[1]
key = open(sys.argv[2], "rb").read().strip() if len(sys.argv) > 2 else None
pat = re.compile(r"^audit-(\d{4}-\d{2}-\d{2})(?:\.(\d{4,9}))?\.jsonl$")
segs = sorted((m.group(1), int(m.group(2) or 0), f)
              for f in os.listdir(d) if (m := pat.match(f)))
prev, seq, n = "0" * 64, 0, 0
for _, _, name in segs:
    for lineno, line in enumerate(open(os.path.join(d, name), "rb"), 1):
        line = line.rstrip(b"\n")
        body, stated = line[:-75] + b"}", line[-66:-2].decode()  # drop ,"hash":"<64 hex>"}
        rec = json.loads(line)
        if rec["alg"] == "hmac-sha256":
            if key is None:
                sys.exit(f"{name}:{lineno}: HMAC-signed; pass the key file")
            want = hmac.new(key, body, hashlib.sha256).hexdigest()
        else:
            want = hashlib.sha256(body).hexdigest()
        if want != stated or rec["hash"] != stated:
            sys.exit(f"{name}:{lineno} (seq {rec['seq']}): hash mismatch")
        if rec["prev_hash"] != prev or rec["seq"] != seq + 1:
            sys.exit(f"{name}:{lineno} (seq {rec['seq']}): chain broken")
        prev, seq, n = want, rec["seq"], n + 1
print(f"ok: {n} records, head {prev}")
```

The key file is read with surrounding whitespace trimmed, as GTA does.
Publish the head hash somewhere the user cannot write (a ticket, a log
server) to detect truncation of the newest records. Without an HMAC key, an
attacker who can write the files can recompute every hash; use
`audit.hmac_key_file` with a key users cannot read for real tamper
resistance.

> Note: `/audit verify` does not pass the HMAC key to the verifier, so on an
> HMAC-keyed log it reports the first record as broken ("verification needs
> the key"). Use the script above with the key file instead.

## Secret redaction

On by default (`redact_secrets`), and forced by policy `secrets.redact` or
`secrets.rules`. Before each model call, every message block's text and
tool input (and provider-native replay data) is scanned; matches are
replaced with placeholders like `[REDACTED:github-token:3f2a]`. The four hex
digits are a keyed fingerprint, stable within a process, so the model can
tell two secrets apart without learning them. Your files, history and saved
sessions keep the real values. The audit log is always redacted (with the
default rules when redaction is off).

Built-in rules: `private-key`, `aws-access-key-id`, `aws-secret-access-key`,
`github-token`, `gitlab-token`, `slack-token`, `slack-webhook`, `stripe-key`,
`google-api-key`, `anthropic-api-key`, `openai-api-key`, `nvidia-api-key`,
`huggingface-token`, `npm-token`, `sendgrid-api-key`, `azure-storage-key`,
`jwt`, `authorization-header`, `connection-string`, `generic-secret`
(`password=`, `api_key=` style assignments) and `high-entropy-string`
(random-looking tokens of 20+ characters).

`secrets_allow` lists exact strings never redacted (a public test key, for
example). The first redaction of a session shows a notice, and every
redaction is audited as `secrets_redacted`.

## Budgets

```json
{ "budget": { "session_usd": 5, "daily_usd": 20, "monthly_usd": 300, "warn_at": 0.8 } }
```

- Cost is estimated from the built-in price table (`internal/pricing`) and
  token usage, including sub-agent calls.
- Daily and monthly spend is kept in `<config dir>/budget.json`, shared by
  all GTA processes of the user (file lock, atomic replace). Days and
  months use local time.
- Before each model call: at or over a cap the call is **blocked**; at
  `warn_at` of a cap, or when the call could cost more than what is left,
  a warning is shown.
- A cap stops the *next* call; one call can finish past the cap.
- In `gta serve`, `session_usd` covers the whole server process, not one
  API session.
- `/budget` and `/cost` show the numbers.

## API server

`gta serve` listens on `127.0.0.1:7878`, requires a bearer token on every
request except `GET /v1/health`, refuses non-loopback `Host` headers
(DNS rebinding) unless `--allow-remote`, and sends no CORS headers unless
`--cors` lists exact origins. It speaks plain HTTP: put TLS in front of it
if you allow remote access. See [server-api.md](server-api.md).

## Threat model

GTA assumes the person running it is trusted, and protects them from:

- **The model making mistakes**: permission modes, approvals with diffs,
  read-before-write, destructive command checks, checkpoints and `/undo`.
- **The model leaking credentials to the provider**: secret redaction.
- **Runaway spend**: budgets, `max_turns`, per-sub-agent turn limits.
- **A cloned repository running code on its own or loosening your
  settings**: unless the user config sets `trust_project_hooks`, a project's
  `.gta/config.json` may only set preferences and tighten security settings
  (see [Untrusted project configuration](#untrusted-project-configuration)),
  and project plugins do not load.
- **Users loosening company rules**: the managed policy is read from a
  system path, cannot be switched off by environment variables, and fails
  closed.
- **Silent tampering with the record**: the hash-chained audit log.
- **Other local web pages and users talking to `gta serve`**: loopback
  bind, bearer token in a `0600` file, Host check, no CORS by default.

## What is not protected

- **Reads are never gated.** In every mode the model can read any file the
  user can, inside or outside the workspace, and `cat`/`head`/`tail` are in
  the default `allow_commands`. Only policy `paths.deny` /
  `paths.deny_read` stop this, and only for the checked path of a call.
- **Directory-walking tools are not checked per file.** `grep`, `glob`,
  `list_dir`, `code_search`, `symbols` and `repo_map` check only the path
  argument, so a denied file under a searched directory can still appear in
  results. The code index also indexes denied workspace files.
- **Policy command rules apply to `bash` only.** They do not cover the
  `verify` tool's test command, `/fixtests`, hooks, MCP servers, plugin
  tools or language servers.
- **Command parsing is best effort.** Rules match program names after
  unwrapping, not behaviour: a program can be copied or renamed, or a
  script written and run. Use `commands.allow` plus the sandbox for strong
  limits.
- **The sandbox does not hide files.** It limits writes and network, not
  reads, and only for shell commands.
- **The system prompt is not redacted.** Memory files (`AGENTS.md`, …),
  the repository map and `session_start` hook output are sent as written.
  Redaction is also pattern-based and can miss secrets or redact
  non-secrets.
- **A trusted project is fully trusted.** With `trust_project_hooks` in the
  user config, every project uses its `.gta/config.json` as written:
  `permission_mode`, `base_url` (where your API key is sent), `lsp_servers`
  and hooks (commands GTA starts) included. Trust is all or nothing and
  applies to every workspace.
- **Preferences from an untrusted project still apply.** A repository can
  pick the model (and so the cost), `max_turns`, `shell_timeout_sec` and
  the code index settings. Use budget caps to bound spend.
- **Local files under the user's control.** The budget ledger, sessions and
  an unkeyed audit log can be edited or deleted by the user.
- **MCP servers, plugins and hooks run with the user's full rights** and
  a scrubbed (plugins) or full (hooks, MCP) environment.
- **Approvals in `gta serve`** come from whoever holds the token; anyone
  who can read the token file can drive the agent. Image `path` inputs in
  API messages are read from the server's disk without policy path checks.
