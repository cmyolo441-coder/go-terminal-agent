# Plugins

A plugin is a directory with a `plugin.json` manifest. It can add:

- **tools** the model can call: executables that read one JSON object on
  stdin and print one JSON object on stdout;
- **slash commands**: prompt templates sent to the model, or executables
  whose output is shown to you.

## Where plugins live

| Source | Directory | Loaded when |
| --- | --- | --- |
| user | `<config dir>/plugins/<name>/` (Linux: `~/.config/gta/plugins/<name>/`) | always, unless `disable_plugins` |
| project | `<workspace>/.gta/plugins/<name>/` | only when the **user** config sets `"trust_project_hooks": true` |

Every non-hidden subdirectory (symlinks included) is a plugin. A plugin
named in `disabled_plugins` is skipped. If two directories declare the same
`name`, the first (user before project, then alphabetical) wins and the
other is reported as a problem. A managed policy can disable plugins
entirely (`disabled_features: ["plugins"]`) or deny single tools
(`tools.deny: ["plugin__jira__*"]`).

`/plugins` lists loaded plugins, their tools and commands, problems,
warnings, untrusted and disabled plugins. `/plugins reload` re-reads every
directory: tool changes apply at once; brand-new slash commands appear after
a restart (changed commands of existing names reload).

## Manifest reference

Unknown fields are errors. A plugin with any problem is not loaded at all.

### Top level

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `name` | string | yes | 1-32 chars, `[a-z0-9][a-z0-9_-]*`. Not `gta`, `mcp`, `plugin` or `builtin`. |
| `version` | string | yes | Semantic version: `1.2.0`, `v1.2.0`, `1.2.0-rc.1`, `1.2.0+build`. |
| `description` | string | yes | Up to 4096 bytes. |
| `min_gta_version` | string | no | Oldest GTA release that works. Pre-release suffixes of GTA are ignored (`0.4.0-dev` satisfies `0.4.0`). |
| `env` | object | no | Variables set for every tool and command. Values may use `${PLUGIN_DIR}` and `${WORKSPACE}`. Names must not start with `GTA_`. |
| `pass_env` | string[] | no | Variables copied from GTA's environment (for example an API token). |
| `tools` | object[] | * | At most 128. |
| `commands` | object[] | * | At most 128. |

\* At least one tool or command is required. The manifest must be at most
1 MiB.

### `tools[]`

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `name` | string | yes | | 1-48 chars, `[a-zA-Z0-9][a-zA-Z0-9_-]*`, unique in the plugin (case-insensitive). |
| `description` | string | yes | | What it does, for the model. Up to 4096 bytes. GTA appends ` (from plugin "<name>")`. |
| `kind` | string | no | `exec` | `read`, `write` or `exec`. Drives permissions (see below). |
| `input_schema` | object | no | `{"type":"object"}` with no properties | JSON Schema of the input. Top level must be `"type": "object"`. |
| `command` | string | yes | | Executable: `./bin/tool` (relative to the plugin directory), an absolute path, or a program name on `PATH`. |
| `args` | string[] | no | | Arguments before anything else. `${PLUGIN_DIR}` and `${WORKSPACE}` are expanded. |
| `timeout_sec` | int | no | 60 | 1 to 600. |
| `max_output_bytes` | int | no | 1048576 | stdout cap, 1 to 16777216. |

The model sees the tool as `plugin__<plugin>__<tool>` (shortened with a
hash suffix past 64 characters).

### `commands[]`

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `name` | string | yes | Same rules as tool names. Becomes `/<name>`. |
| `description` | string | yes | Shown in `/help` with ` (plugin)`. |
| `argument_hint` | string | no | Shown after the name in `/help`, e.g. `<file> [focus]`. |
| `prompt` | string | one of | A template sent to the model as your message. |
| `command` | string | one of | An executable whose stdout is shown to you (not sent to the model). |
| `args` | string[] | no | Executable commands only. Expanded like tool args. |
| `timeout_sec` | int | no | Executable commands only. Default 30, at most 600. |

Set exactly one of `prompt` and `command`.

If a command name equals a built-in command, or is used by several
plugins, it is registered as `/<plugin>:<name>` instead, with a warning.
The qualified form `/<plugin>:<name>` always works too.

## Command templates

| Placeholder | Replaced by |
| --- | --- |
| `{{args}}` | Everything typed after the command, trimmed. |
| `{{1}}` … `{{9}}` | The 1st to 9th word, split shell-style (whitespace separates; `'single'` and `"double"` quotes group; `\` escapes). Missing words become empty. |

Whitespace inside the braces is allowed (`{{ 1 }}`). Any other placeholder
is a manifest error. A template with no placeholders gets the arguments
appended after a blank line. An unterminated quote in the arguments is an
error.

```
"prompt": "Explain what {{1}} does. Focus on: {{args}}"

/explain main.go "error handling"
→ Explain what main.go does. Focus on: main.go "error handling"
```

Executable commands get the words as extra arguments after `args`, and the
raw text in `$GTA_ARGS`.

## Tool protocol

1. The model calls `plugin__<plugin>__<tool>` with a JSON object.
2. GTA validates it against `input_schema`. Invalid input is refused
   before any process starts; the model sees the reason.
3. Permission checks run (policy, then the permission mode for the tool's
   `kind`).
4. GTA starts the executable with the workspace root as working directory,
   in its own process group, and writes the input as one line of compact
   JSON to stdin.
5. The executable prints **one** JSON object to stdout and exits:

   ```json
   {"output": "text for the model", "is_error": false}
   ```

| Situation | What the model gets |
| --- | --- |
| Valid JSON, `is_error` false, exit 0 | `output` (or `(tool returned no output)` if empty) |
| `is_error` true, or non-zero exit with valid JSON | `output` as an error result |
| No JSON / invalid JSON / extra data after it, exit 0 | Error naming the expected format and the start of stdout |
| No valid JSON and non-zero exit | `exited with status N` plus the end of stderr |
| Timeout | `timed out after …` plus the end of stderr; the process group is killed |
| stdout over `max_output_bytes` | `printed more than N bytes and was stopped` |

stderr is kept (64 KiB) for error messages only. `output` is cut to the
session's tool output limit, keeping head and tail.

### Environment

Plugin processes do **not** inherit GTA's environment. They get:

- basics: `PATH`, `HOME`, `USER`, `LOGNAME`, `SHELL`, `LANG`, `LANGUAGE`,
  `TERM`, `TMPDIR`, `TZ`, `LC_*`, `XDG_RUNTIME_DIR`, `XDG_CONFIG_HOME`,
  `XDG_CACHE_HOME`, `XDG_DATA_HOME`, proxy variables (`HTTP_PROXY`,
  `HTTPS_PROXY`, `NO_PROXY` and lower-case forms), `SSL_CERT_FILE`,
  `SSL_CERT_DIR` (plus the usual Windows system variables);
- the names in `pass_env`;
- the manifest's `env`;
- `GTA_PLUGIN_NAME`, `GTA_PLUGIN_DIR`, `GTA_PLUGIN_VERSION`,
  `GTA_WORKSPACE`, `GTA_VERSION`;
- tools: `GTA_TOOL_NAME` (local name) and `GTA_TOOL_KIND`;
- commands: `GTA_COMMAND_NAME` and `GTA_ARGS`.

### Input schema subset

Supported keywords: `type` (string or array), `enum`, `const`,
`properties`, `required`, `additionalProperties`, `items`, `minLength`,
`maxLength`, `minItems`, `maxItems`, `minimum`, `maximum`,
`exclusiveMinimum`, `exclusiveMaximum`, `pattern`, `allOf`, `anyOf`,
`oneOf`, `not`. Refused (rather than ignored): `$ref`, `$dynamicRef`, `if`,
`then`, `else`, `patternProperties`, `dependentSchemas`,
`dependentRequired`, `unevaluatedProperties`, `prefixItems`, `contains`.
Every `required` name must be in `properties`.

## Permissions

| `kind` | Behaves like | `ask` mode | `auto-edit` | `yolo` |
| --- | --- | --- | --- | --- |
| `read` | `read_file` | runs | runs | runs |
| `write` | `write_file` | asks | runs | runs |
| `exec` | `bash` | asks | asks | runs |

For an `exec` tool, "allow for the session" allows that tool for the rest
of the session, and putting its full name in `allow_commands` allows it
without asking. If the input has a `path` or `file_path` field, policy path
rules check it. Plugin processes are not sandboxed and run with the user's
rights: declare `read` only for tools that really change nothing.

## Limits and trust checks

| Limit | Value |
| --- | --- |
| Manifest size | 1 MiB |
| Tools / commands per plugin | 128 / 128 |
| Plugin name | 32 chars |
| Tool / command name | 48 chars |
| Description | 4096 bytes |
| Tool timeout | default 60 s, max 600 s |
| Command timeout | default 30 s, max 600 s (text commands are also cancelled by the UI after 2 minutes) |
| Tool stdout | default 1 MiB, max 16 MiB |
| Command stdout | 256 KiB, then `… (output truncated)` |
| stderr kept | 64 KiB |

At load time GTA refuses:

- a `plugin.json` that is not a regular file, or is world-writable;
- a plugin-relative executable that does not exist, is not executable
  (`chmod +x`), is world-writable, or resolves (after symlinks) outside the
  plugin directory;
- an absolute executable that is missing, not executable or world-writable;
- a bare program name not found on `PATH`;
- a tool whose model-facing name is already taken (built-in, `task`, or
  another plugin): it is skipped with a warning.

## Complete example

The example lives in [`examples/plugins/linecount/`](examples/plugins/linecount/).
It has one read-only tool and two commands, and uses only POSIX `sh`.

```
linecount/
├── plugin.json
└── bin/
    ├── count-lines.sh
    └── today.sh
```

`plugin.json`:

```json
{
  "name": "linecount",
  "version": "1.0.0",
  "description": "Count lines in workspace files, plus two example slash commands",
  "env": { "LINECOUNT_DIR": "${PLUGIN_DIR}" },
  "tools": [
    {
      "name": "count_lines",
      "description": "Count the lines of one file in the workspace. Give a path relative to the workspace root.",
      "kind": "read",
      "input_schema": {
        "type": "object",
        "properties": {
          "path": { "type": "string", "description": "File path relative to the workspace root", "minLength": 1 }
        },
        "required": ["path"],
        "additionalProperties": false
      },
      "command": "./bin/count-lines.sh",
      "timeout_sec": 10,
      "max_output_bytes": 4096
    }
  ],
  "commands": [
    {
      "name": "explain",
      "description": "Ask the model to explain a file",
      "argument_hint": "<file> [focus]",
      "prompt": "Explain what {{1}} does. Read it first with read_file. Focus on: {{args}}"
    },
    {
      "name": "today",
      "description": "Show the date and the current git branch",
      "command": "./bin/today.sh",
      "timeout_sec": 10
    }
  ]
}
```

`bin/count-lines.sh`:

```sh
#!/bin/sh
input=$(cat)
path=$(printf '%s' "$input" | sed -n 's/.*"path":"\([^"\\]*\)".*/\1/p')

reply() { printf '{"output": "%s", "is_error": %s}\n' "$1" "$2"; }

if [ -z "$path" ]; then
  reply "path is missing or contains a quote or backslash" true; exit 0
fi
case "$path" in
  /* | .. | ../* | */../* | */..)
    reply "path must be relative to the workspace and must not contain .." true; exit 0 ;;
esac
if [ ! -f "$path" ]; then
  reply "no such file: $path" true; exit 0
fi
lines=$(wc -l < "$path" | tr -d ' ')
reply "$path has $lines lines" false
```

(The file in the repository has comments.) The script relies on GTA's
guarantees: the input already matched the schema and arrives compacted, so
`"path":"..."` has no spaces; the reply never contains `"` or `\`, so it is
valid JSON without escaping. A real tool should use a JSON library or `jq`.

`bin/today.sh` prints the date, the git branch and `$GTA_ARGS`.

Install and try it:

```sh
mkdir -p ~/.config/gta/plugins
cp -R docs/examples/plugins/linecount ~/.config/gta/plugins/
chmod +x ~/.config/gta/plugins/linecount/bin/*.sh
gta
```

```
❯ /plugins
- linecount 1.0.0 (user): Count lines in workspace files, plus two example slash commands
  - tool `plugin__linecount__count_lines`
  - command `/explain`
  - command `/today`
❯ how many lines does README.md have?
● plugin__linecount__count_lines (plugin linecount)
  ⎿  README.md has 461 lines
❯ /today release
Date: 2026-11-13
Branch: main
Arguments: release
```

Test a tool without GTA by piping JSON into it from the workspace root:

```sh
echo '{"path":"README.md"}' | ~/.config/gta/plugins/linecount/bin/count-lines.sh
# {"output": "README.md has 461 lines", "is_error": false}
```
