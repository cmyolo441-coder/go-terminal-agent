# GTA documentation

Reference documentation for GTA, the terminal AI coding agent. The
[project README](../README.md) covers installation and a quick start; these
pages cover every flag, setting and interface in detail.

| Page | What it covers |
| --- | --- |
| [Commands](commands.md) | `gta` and `gta serve` flags, headless mode and `--json` output, input syntax (`!`, `@`), keys, every slash command, the model's tools. |
| [Configuration](configuration.md) | Every `config.json` field with type, default and example; precedence; environment variables; project trust; hooks, MCP servers and language servers. |
| [Architecture](architecture.md) | How the packages fit together, startup, the lifecycle of one turn, providers and routing, sub-agents, autopilot, code intelligence, MCP, plugins, UI, server, extension points. |
| [Security](security.md) | Permission modes, the sandbox, managed policy reference, audit log format and verification, secret redaction, budgets, threat model and what is not protected. |
| [Plugins](plugins.md) | Manifest reference, tool protocol, command templates, limits, trust, and a complete example. |
| [Server API](server-api.md) | `gta serve`: authentication, endpoints, SSE events, the approval flow, errors, curl examples. |
| [Observability](observability.md) | OpenTelemetry configuration, span names and attributes, metrics, a sample collector config. |

Example plugin: [`examples/plugins/linecount/`](examples/plugins/linecount/),
a working plugin with a read-only tool and two slash commands, written as
POSIX `sh` scripts.

## Where to start

- New user: [Commands](commands.md), then [Configuration](configuration.md).
- Setting GTA up for a team: [Security](security.md) (managed policy, audit,
  budgets).
- Extending GTA: [Plugins](plugins.md), MCP and hooks in
  [Configuration](configuration.md), and the extension table in
  [Architecture](architecture.md#extension-points).
- Integrating with an editor or service: [Server API](server-api.md), or
  `gta -p --json` in [Commands](commands.md#headless-mode--p).
- Monitoring usage and cost: [Observability](observability.md).
