# Server API (`gta serve`)

`gta serve` runs GTA as a local HTTP daemon for editors, scripts and other
front ends. Each API session is an agent conversation in the server's
workspace. Sessions live in memory only (they are not saved to
`sessions/` and are lost when the server stops).

```sh
gta serve                         # 127.0.0.1:7878, token in ~/.config/gta/serve.token
gta serve --addr 127.0.0.1:9000 --mode auto-edit --cwd ~/src/app -v
```

Flags are listed in [commands.md](commands.md#gta-serve). The same
configuration, managed policy, audit log, budgets, secret redaction,
sandbox, hooks (except `session_start`), plugins and MCP servers apply to
every session. MCP servers are shared; a session gets the MCP tools that are
connected when it is created.

## Authentication and transport

- Every request except `GET /v1/health` needs
  `Authorization: Bearer <token>` (the scheme is case-insensitive).
- The token is `GTA_SERVE_TOKEN` (at least 16 characters) or, if unset, a
  random `gta_…` value. Either way it is written to `--token-file`
  (default `<config dir>/serve.token`, mode `0600`, removed on shutdown).
- The server refuses to listen on a non-loopback address unless
  `--allow-remote`. While on loopback it also refuses requests whose `Host`
  is not `localhost` or a loopback IP (HTTP 421), which stops DNS-rebinding
  attacks from web pages.
- No CORS headers are sent unless `--cors https://app.example` lists exact
  origins (`*` is refused). Preflights from other origins get 403.
- Plain HTTP only. Put a TLS proxy in front if you allow remote access.
- Request bodies are JSON (`Content-Type: application/json` or none),
  strict (unknown fields are 400), at most 12 MiB.

## Errors

Every error is JSON:

```json
{ "error": { "code": "session_busy", "message": "a turn is already running in this session; wait for it or interrupt it" } }
```

| Status | `code` | When |
| --- | --- | --- |
| 400 | `bad_request` | Invalid JSON, unknown field, bad mode, bad decision, missing text, bad image, title over 200 chars. |
| 400 | `session_failed` | The session could not be created (for example a model the policy forbids). |
| 401 | `unauthorized` | Missing or wrong bearer token (`WWW-Authenticate: Bearer realm="gta"`). |
| 403 | `cors` | Cross-origin preflight from an origin not in `--cors`. |
| 404 | `no_session` | Unknown session id. |
| 404 | `no_approval` | No turn running, or the approval id is not pending (answered, timed out). |
| 404 | `not_found` | Unknown endpoint. |
| 405 | `method_not_allowed` | Wrong method for the path. |
| 409 | `session_busy` | A turn is already running in the session. |
| 413 | `too_large` | Body over 12 MiB. |
| 415 | `bad_content_type` | Body sent with a non-JSON content type. |
| 421 | `bad_host` | Non-loopback `Host` while bound to loopback. |
| 500 | `internal`, `session_failed` | Server bug. |
| 503 | `shutting_down` | The server is stopping. |
| 503 | `too_many_sessions` | 64 sessions exist. |

## Endpoints

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/v1/health` | Liveness (no token). |
| POST | `/v1/sessions` | Create a session. |
| GET | `/v1/sessions` | List sessions. |
| GET | `/v1/sessions/{id}` | One session; `?history=1` adds its messages. |
| DELETE | `/v1/sessions/{id}` | Interrupt and delete a session. |
| POST | `/v1/sessions/{id}/messages` | Run a turn; the response is an SSE stream. |
| POST | `/v1/sessions/{id}/approvals/{request_id}` | Answer an approval. |
| POST | `/v1/sessions/{id}/interrupt` | Cancel the running turn. |

### `GET /v1/health`

```json
{ "status": "ok", "version": "0.1.0-dev", "uptime_sec": 42 }
```

### `POST /v1/sessions`

Body (optional, every field optional):

```json
{ "model": "claude-sonnet-5-5", "mode": "ask", "title": "editor: main.go" }
```

| Field | Description |
| --- | --- |
| `model` | Overrides the server's model; checked against the managed policy. |
| `mode` | `ask`, `auto-edit`, `plan` or `yolo`; clamped by the policy's `max_permission_mode`. |
| `title` | Free-form label, at most 200 characters. |

Response `201`, a session object:

```json
{
  "id": "ses_5f0c2a9e4b7d41c8a3e2f1d0c9b8a7e6",
  "title": "editor: main.go",
  "model": "claude-sonnet-5-5",
  "mode": "ask",
  "created_at": "2026-11-13T16:00:00Z",
  "last_active_at": "2026-11-13T16:00:00Z",
  "busy": false,
  "turns": 0,
  "messages": 0,
  "usage": { "input_tokens": 0, "output_tokens": 0, "cache_read_tokens": 0, "cache_write_tokens": 0 },
  "cost_usd": 0,
  "context_tokens": 0
}
```

Additional fields: `last_error` (the last turn's error, if any) and
`history` (only with `?history=1`).

### `GET /v1/sessions`

```json
{ "sessions": [ { "id": "ses_…", "model": "…", "busy": false, "turns": 2, "…": "…" } ] }
```

Sorted by creation time.

### `GET /v1/sessions/{id}[?history=1]`

A session object. With `history=1` (or `true`), `history` holds the
conversation:

```json
"history": [
  { "role": "user", "blocks": [ { "type": "text", "text": "run the tests" } ] },
  { "role": "assistant", "blocks": [
      { "type": "text", "text": "Running them now." },
      { "type": "tool_use", "tool_use_id": "toolu_01", "tool_name": "bash", "input": { "command": "go test ./..." } } ] },
  { "role": "user", "blocks": [ { "type": "tool_result", "tool_use_id": "toolu_01", "text": "ok …" } ] }
]
```

Block `type` is `text`, `thinking`, `tool_use`, `tool_result`,
`server_tool` or `image`; `is_error` marks failed tool results. Messages may
also carry `provider` and `native` (opaque provider data for lossless
replay). The history is not redacted.

### `DELETE /v1/sessions/{id}`

```json
{ "deleted": true, "interrupted": false }
```

`interrupted` is true when a turn was running and was cancelled.

### `POST /v1/sessions/{id}/interrupt`

Body empty or `{}`.

```json
{ "interrupted": true }
```

`false` when no turn was running.

### `POST /v1/sessions/{id}/messages`

```json
{
  "text": "what does this screenshot show?",
  "images": [
    { "media_type": "image/png", "data": "<base64>" },
    { "path": "docs/screenshot.png" }
  ]
}
```

| Field | Description |
| --- | --- |
| `text` | The prompt. `text` or `images` is required. |
| `images` | Up to 20. Either `data` (base64) with `media_type`, or `path` on the server's machine (relative to the workspace; a path the managed policy denies reading is refused with `bad_request`). PNG, JPEG, GIF or WebP, at most 3.75 MiB and 8000 px per side. |

One turn per session at a time (409 otherwise). The response is
`200 text/event-stream`. Disconnecting interrupts the turn.

## Event stream

Each event:

```
id: 3
event: tool_start
data: {"id":"toolu_01","tool":"bash","kind":"exec","summary":"Run go test ./...","command":"go test ./..."}

```

`id` counts from 1 per stream. `data` is one line of JSON. A comment line
`: ping` is sent every 15 seconds. The stream always starts with
`turn_start` and ends with `done`.

| Event | Payload | Meaning |
| --- | --- | --- |
| `turn_start` | `session_id`, `turn` | The turn began (`turn` counts from 1). |
| `text` | `text` | Assistant text delta. |
| `thinking` | `text` | Reasoning summary delta. |
| `tool_call` | `id`, `name` | The model started a tool call (input still streaming). |
| `server_tool` | `id`, `name` | A provider-side tool started (Anthropic web search/fetch). |
| `tool_start` | `id`, `tool`, `kind`, `summary`, optional `preview`, `command`, `path`, `outside_root` | A validated, permitted call is about to run. |
| `tool_done` | `id`, `tool`, `output`, `truncated`, `is_error`, optional `error` | It finished. `output` is cut at 16 KiB (`truncated: true`). |
| `tool_failed` | `id`, `tool`, `reason` | It never ran: unknown tool, invalid input, hook block, policy denial, mode denial, declined. |
| `approval_required` | `request_id`, `tool`, `kind`, `summary`, `reason`, `expires_at`, `destructive`, optional `preview`, `command`, `path`, `outside_root` | The call waits for `POST …/approvals/{request_id}`. |
| `approval_resolved` | `request_id`, `decision`, `by` | `decision`: `allow_once`, `allow_session`, `deny`. `by`: `api`, `timeout`, `cancelled`. |
| `usage` | `model`, `usage` | Token usage of one model call (also for sub-agent calls). |
| `notice` | `message` | Something the user should know (routing fallback, budget warning, redaction, hook output, …). |
| `subagent` | `index`, `description`, `event` | Progress of a `task` sub-agent. |
| `error` | `message` | The turn failed. Sent just before `done`. |
| `done` | see below | The turn is over. Always last. |

`kind` is `read`, `write` or `exec`. `preview` (at most 16 KiB) is the diff
for edits and the input for other tools. `destructive` is true for exec
calls matching the destructive-command patterns.

`done` payload:

```json
{
  "turn": 1,
  "duration_ms": 7400,
  "stop_reason": "end_turn",
  "text": "All tests pass.",
  "usage": { "input_tokens": 16800, "output_tokens": 960, "cache_read_tokens": 12000, "cache_write_tokens": 0 },
  "cost_usd": 0.04,
  "context_tokens": 17760
}
```

`stop_reason` is `end_turn`, `interrupted` or `error` (then `error` holds
the message). `text` is the last assistant message of this turn. `usage`
and `cost_usd` are session totals.

## Approval flow

1. A call needs approval under the session's mode. The stream emits
   `approval_required` with a `request_id` and `expires_at`.
2. The client answers on a separate connection:

   ```
   POST /v1/sessions/{id}/approvals/{request_id}
   {"decision": "allow_once"}
   ```

   | `decision` | Aliases | Effect |
   | --- | --- | --- |
   | `allow_once` | `allow`, `yes` | Run this call. |
   | `allow_session` | `always` | Run it and grant the same kind of call for the rest of the session (all edits, or that command prefix). |
   | `deny` | `no` | Refuse it. The model is told not to retry and the turn ends. |

   Response: `{"request_id": "apr_…", "decision": "allow_once"}`.
3. The stream emits `approval_resolved` with `by: "api"`.

Unanswered approvals are denied after 5 minutes (`by: "timeout"`), or
when the turn is interrupted (`by: "cancelled"`). Policy denials and
`plan` mode refusals never produce an approval; they appear as
`tool_failed`.

## curl walkthrough

```sh
TOKEN=$(cat ~/.config/gta/serve.token)
API=http://127.0.0.1:7878
auth=(-H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json')

curl -s $API/v1/health

SID=$(curl -s "${auth[@]}" -d '{"mode":"ask","title":"demo"}' $API/v1/sessions | jq -r .id)

# Terminal 1: run a turn and watch the stream.
curl -N "${auth[@]}" -d '{"text":"run the tests"}' $API/v1/sessions/$SID/messages

# Terminal 2: answer the approval_required event.
curl -s "${auth[@]}" -d '{"decision":"allow_once"}' \
  $API/v1/sessions/$SID/approvals/apr_0123456789abcdef0123456789abcdef

curl -s "${auth[@]}" -X POST $API/v1/sessions/$SID/interrupt
curl -s "${auth[@]}" "$API/v1/sessions/$SID?history=1" | jq '.history | length'
curl -s "${auth[@]}" -X DELETE $API/v1/sessions/$SID
```

## Limits

| Limit | Value |
| --- | --- |
| Sessions | 64 per server |
| Request body | 12 MiB, read within 30 s |
| Images per message | 20 |
| Approval timeout | 5 minutes |
| Tool output per `tool_done` | 16 KiB |
| Keep-alive ping | 15 s |
| Graceful shutdown | 15 s; running turns are interrupted |
