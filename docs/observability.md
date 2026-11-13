# Observability

GTA can export OpenTelemetry traces and metrics over OTLP/HTTP (JSON), or
write them to a local file. It is off by default. The exporter is built in
(`internal/telemetry`); no OpenTelemetry SDK or collector is bundled.

Telemetry never carries prompt text, model output, tool input or tool
output. Spans hold model names, token counts, costs, tool names, call ids,
outcomes and error types.

## Turning it on

Telemetry starts when either is true:

- `telemetry.enabled` is `true` in a config file, or
- `OTEL_EXPORTER_OTLP_ENDPOINT` is set in the environment.

`OTEL_SDK_DISABLED=true` always turns it off. So does `"exporter": "none"`.

```json
{
  "telemetry": {
    "enabled": true,
    "endpoint": "http://localhost:4318"
  }
}
```

Or, with no config change:

```sh
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 gta
```

`/status` shows where telemetry goes. Problems with environment variables
(a malformed header list, an unsupported protocol) are printed once at
startup as `gta: telemetry: …` and do not stop GTA.

## Configuration

All fields live under `telemetry` in `config.json`.

| Field | Type | Default | Description |
| --- | --- | --- | --- |
| `enabled` | bool | `false` | Turn telemetry on. |
| `exporter` | string | `otlp` | `otlp`, `file` or `none`. If unset and only `file` is given (no endpoint in config or env), the file exporter is used. |
| `endpoint` | string | `http://localhost:4318` | OTLP/HTTP base URL. Data goes to `{endpoint}/v1/traces` and `{endpoint}/v1/metrics`. Overrides the `OTEL_EXPORTER_OTLP_*ENDPOINT` variables. |
| `headers` | object | `{}` | Headers for every OTLP request, e.g. an API key. Merged over `OTEL_EXPORTER_OTLP_HEADERS`. |
| `compression` | string | none | `gzip` or `none`. |
| `timeout_sec` | int | 10 | Timeout of one OTLP request. |
| `file` | string | — | Output path for the file exporter. |
| `file_max_mb` | int | 0 | Rotate the file at this size, keeping 3 old files. 0 never rotates. |
| `service_name` | string | `gta` | `service.name` resource attribute. |
| `resource_attributes` | object | `{}` | Extra resource attributes, e.g. `{"deployment.environment": "dev"}`. |
| `sample_ratio` | number 0–1 | 1 | Fraction of traces kept (parent-based). Overrides `OTEL_TRACES_SAMPLER`. |
| `metric_interval_sec` | int | 60 | Metric export period. |
| `temporality` | string | `cumulative` | `cumulative` or `delta`. |
| `disable_traces` | bool | `false` | Export metrics only. |
| `disable_metrics` | bool | `false` | Export traces only. |

### Environment variables

Config fields win over these when both are set.

| Variable | Effect |
| --- | --- |
| `OTEL_SDK_DISABLED` | `true` turns telemetry off. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Base URL; also turns telemetry on. |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`, `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | Full per-signal URLs. They do **not** turn telemetry on by themselves (see below). |
| `OTEL_EXPORTER_OTLP_HEADERS`, `…_TRACES_HEADERS`, `…_METRICS_HEADERS` | `key=value,key2=value2`. |
| `OTEL_EXPORTER_OTLP_TIMEOUT` | Milliseconds. |
| `OTEL_EXPORTER_OTLP_COMPRESSION` | `gzip` or `none`. |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | `http/json`. `http/protobuf` is accepted but data is still sent as JSON; `grpc` is not supported. |
| `OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE` | `cumulative` or `delta`. |
| `OTEL_TRACES_EXPORTER`, `OTEL_METRICS_EXPORTER` | `none` turns one signal off. |
| `OTEL_TRACES_SAMPLER`, `OTEL_TRACES_SAMPLER_ARG` | `always_on`, `always_off`, `traceidratio`, `parentbased_*`. |
| `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES` | Resource. |
| `OTEL_BSP_MAX_QUEUE_SIZE`, `OTEL_BSP_MAX_EXPORT_BATCH_SIZE`, `OTEL_BSP_SCHEDULE_DELAY`, `OTEL_BSP_EXPORT_TIMEOUT` | Span batching. |
| `OTEL_METRIC_EXPORT_INTERVAL`, `OTEL_METRIC_EXPORT_TIMEOUT` | Milliseconds. |
| `OTEL_METRICS_EXEMPLAR_FILTER` | Exemplar filter. |
| `OTEL_ATTRIBUTE_COUNT_LIMIT`, `OTEL_ATTRIBUTE_VALUE_LENGTH_LIMIT`, `OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT`, `OTEL_SPAN_ATTRIBUTE_VALUE_LENGTH_LIMIT`, `OTEL_SPAN_EVENT_COUNT_LIMIT`, `OTEL_SPAN_LINK_COUNT_LIMIT`, `OTEL_EVENT_ATTRIBUTE_COUNT_LIMIT`, `OTEL_LINK_ATTRIBUTE_COUNT_LIMIT` | Span limits. |

Note: the telemetry library treats the per-signal endpoint variables as
"on", but the `gta` binary only checks `OTEL_EXPORTER_OTLP_ENDPOINT` (or
`telemetry.enabled`) before starting it. If you only set a per-signal
endpoint, also set `"enabled": true`.

## File exporter

```json
{ "telemetry": { "enabled": true, "exporter": "file", "file": "~/.config/gta/telemetry.jsonl", "file_max_mb": 50 } }
```

Each line is one OTLP/JSON export request (`resourceSpans` or
`resourceMetrics`), the format the Collector's `otlpjsonfile` receiver
reads. Useful offline or to replay into a collector later.

## Resource

| Attribute | Value |
| --- | --- |
| `service.name` | `gta` (or `service_name` / `OTEL_SERVICE_NAME`) |
| `service.version` | GTA version |
| `service.instance.id` | Random per process |
| `telemetry.sdk.name`, `telemetry.sdk.language`, `telemetry.sdk.version` | The built-in SDK |
| `process.runtime.name`, `process.runtime.version`, `process.pid` | Go runtime and process |
| `os.type`, `host.arch`, `host.name` | Host |

The instrumentation scope uses schema URL
`https://opentelemetry.io/schemas/1.37.0`. Attribute names follow the
OpenTelemetry GenAI semantic conventions where one exists, and use a
`gta.` prefix otherwise.

## Traces

One user prompt produces this tree:

```
invoke_agent gta                     (internal)
├── chat claude-opus-5-5             (client)   one per model call
├── execute_tool read_file           (internal) one per executed tool call
├── execute_tool bash
└── chat claude-opus-5-5
```

Sub-agents started by the `task` tool do not get their own
`invoke_agent` span; their model calls appear as `chat` spans under the
parent's `execute_tool task` span. `gta serve` creates the same tree per
API turn.

### `invoke_agent gta`

| Attribute | Value |
| --- | --- |
| `gen_ai.operation.name` | `invoke_agent` |
| `gen_ai.agent.name` | `gta` |
| `gen_ai.conversation.id` | Session id (interactive and `-p`; absent in `gta serve`) |

### `chat {model}`

Set at the start of the call:

| Attribute | Value |
| --- | --- |
| `gen_ai.operation.name` | `chat` |
| `gen_ai.system`, `gen_ai.provider.name` | Provider name (`anthropic`, `openai`, `openrouter`, …; `gemini` becomes `gcp.gemini`) |
| `gen_ai.request.model` | Requested model |
| `gen_ai.request.max_tokens` | When set |
| `gta.request.effort` | When set |
| `gta.request.messages` | Messages sent |
| `gta.request.tools` | Tool definitions sent |

Set on success:

| Attribute | Value |
| --- | --- |
| `gen_ai.response.model` | Model that answered |
| `gen_ai.response.finish_reasons` | `["stop"]`, `["tool_calls"]`, `["length"]`, `["content_filter"]`, `["unknown"]`, or the provider's own value |
| `gen_ai.usage.input_tokens` | Input tokens **including** cache reads and writes |
| `gen_ai.usage.output_tokens` | Output tokens |
| `gen_ai.usage.cache_read.input_tokens` | Cache reads |
| `gen_ai.usage.cache_creation.input_tokens` | Cache writes |
| `gta.cost.usd` | Estimated cost, when the model's price is known |

On failure: `error.type` (a router error kind such as `rate_limit`,
`overloaded`, `timeout`, `auth`) and an error status.

Events: `gen_ai.first_token` when the first streamed token arrives.

When `fallback_models` is configured, a fallback adds the event
`gta.fallback` with attributes `from` and `to` (route names) to the active
span.

### `execute_tool {tool}`

| Attribute | Value |
| --- | --- |
| `gen_ai.operation.name` | `execute_tool` |
| `gen_ai.tool.name` | Tool name, e.g. `bash`, `mcp__github__search`, `plugin__linecount__count_lines` |
| `gen_ai.tool.type` | `function` |
| `gen_ai.tool.call.id` | The model's tool-use id |
| `gta.tool.outcome` | `success`, `error` or `cancelled` |
| `error.type` | When not successful |

Only calls that run get a span. Calls refused by the permission gate,
policy or a hook do not.

## Metrics

| Name | Type | Unit | Attributes |
| --- | --- | --- | --- |
| `gen_ai.client.token.usage` | histogram | `{token}` | `gen_ai.operation.name`, `gen_ai.system`, `gen_ai.request.model`, `gen_ai.response.model`, `gen_ai.token.type` (`input`, `output`) |
| `gen_ai.client.operation.duration` | histogram | `s` | same as above without token type; `error.type` on failure |
| `gen_ai.server.time_to_first_token` | histogram | `s` | as above |
| `gen_ai.server.time_per_output_token` | histogram | `s` | as above |
| `gta.tokens` | counter | `{token}` | `gen_ai.response.model`, `gen_ai.token.type` (`input`, `output`, `cache_read`, `cache_write`) |
| `gta.cost` | counter | `USD` | `gen_ai.response.model` |
| `gta.tool.calls` | counter | `{call}` | `gen_ai.tool.name`, `gta.tool.outcome` |
| `gta.tool.duration` | histogram | `s` | `gen_ai.tool.name`, `gta.tool.outcome` |
| `gta.errors` | counter | `{error}` | `gta.component` (`model`, `tool`), `error.type` |
| `gta.model.retries` | counter | `{retry}` | `gta.route`, `gta.reason` |
| `gta.model.fallbacks` | counter | `{fallback}` | `gta.route.from`, `gta.route.to`, `gta.reason` |
| `gta.telemetry.spans` | observable counter | `{span}` | `state` (`exported`, `dropped`, `failed`) |
| `gta.telemetry.queue.size` | observable gauge | `{span}` | — |
| `gta.telemetry.export.retries` | observable counter | `{retry}` | — |

`gta.model.retries` and `gta.model.fallbacks` are only recorded when
`fallback_models` is set (that is when the router runs). In
`gen_ai.client.token.usage`, `input` includes cached tokens; `gta.tokens`
splits them.

Histogram buckets: durations 0.01 s to 81.92 s (doubling), tokens 1 to
67,108,864 (×4).

## Sample collector config

An OpenTelemetry Collector that receives GTA's OTLP/HTTP data and prints it:

```yaml
# otelcol.yaml
receivers:
  otlp:
    protocols:
      http:
        endpoint: 127.0.0.1:4318

processors:
  batch: {}

exporters:
  debug:
    verbosity: detailed
  # Example: forward to a backend instead.
  # otlphttp:
  #   endpoint: https://otlp.example.com
  #   headers:
  #     authorization: Bearer ${env:OTLP_TOKEN}

service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [batch]
      exporters: [debug]
    metrics:
      receivers: [otlp]
      processors: [batch]
      exporters: [debug]
```

```sh
otelcol-contrib --config otelcol.yaml &
OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318 gta -p "list the Go packages"
```

To replay a file written by the file exporter:

```yaml
receivers:
  otlpjsonfile:
    include: [/home/me/.config/gta/telemetry.jsonl]
```

## Overhead and failure behaviour

- Spans are batched and exported in the background. A full queue drops
  spans (counted in `gta.telemetry.spans{state="dropped"}`) instead of
  slowing the agent.
- Failed exports are retried (`gta.telemetry.export.retries`).
- On exit GTA flushes for at most 5 seconds.
- `TRACEPARENT` propagation helpers exist in `internal/telemetry` but are
  not wired into `gta`; GTA always starts a new trace.
