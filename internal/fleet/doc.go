// Package fleet runs agent tasks across a fleet of worker machines: one
// elected coordinator owns a durable task queue and N gta workers lease
// tasks from it, e.g. "apply this migration across 200 repositories".
//
// # Coordinator
//
// The Coordinator keeps every task in memory and records each state change
// in a write-ahead log (JSONL, one CRC32-C checksummed record per line)
// before applying it. The log is compacted into periodic snapshots and
// replayed on restart; a torn final record left by a crash is truncated.
// Tasks carry a tenant, a priority, optional dependencies, an optional
// earliest start time, capability requirements and an idempotency key.
//
// Ready tasks are dispatched in strict priority bands; within a band,
// tenants share workers by deficit round robin, in proportion to their
// configured weights. A dispatched task is leased to one worker for a
// visibility timeout that the worker extends with heartbeats. A lease that
// expires makes the task deliverable again (at-least-once delivery); a
// task that exhausts its attempts goes to the dead-letter queue, from which
// it can be redriven. Completion is accepted only from the current lease
// holder, so a task's effect is committed exactly once even when a slow or
// partitioned worker finishes it late.
//
// # Workers
//
// Workers register their capabilities (OS, architecture, tools, labels and
// a concurrency limit), long-poll for leases, heartbeat, and drain
// gracefully on shutdown. The coordinator scores each worker's health from
// its outcomes and quarantines workers that fail repeatedly or flap.
//
// # High availability
//
// Coordinators elect a leader through a LeaseStore (a shared directory of
// lock files, or memory for tests). Every election hands out a strictly
// larger fencing token; the leader checks its token before each write and
// stamps it into each log record, so a deposed leader cannot change state,
// and records it slipped in are ignored on replay.
//
// # API
//
// NewHandler serves the coordinator over HTTP/JSON. Requests are signed
// with HMAC-SHA256 over method, path, query, body digest, timestamp and a
// single-use nonce; responses are signed back over the request nonce, so
// both sides authenticate each other and neither requests nor responses can
// be replayed. Client is the matching Go client and also implements
// WorkerAPI, so a Worker runs the same against a remote coordinator or an
// in-process one. ServerTLSConfig and ClientTLSConfig build mutual-TLS
// configurations for deployments that want transport authentication too.
//
// Endpoints (all under /v1/fleet):
//
//	GET    /healthz                       leadership probe (unsigned)
//	POST   /tasks                         submit one task or a batch
//	GET    /tasks                         list tasks (?tenant, ?state, ?after, ?limit)
//	GET    /tasks/{id}                    one task
//	POST   /tasks/{id}/cancel             cancel a task and its dependents
//	POST   /tasks/{id}/complete           worker: finish a leased task
//	POST   /tasks/{id}/fail               worker: fail a leased task
//	POST   /tasks/{id}/release            worker: give a lease back unused
//	GET    /dlq                           dead-lettered tasks
//	POST   /dlq/{id}/redrive              move a dead task back to ready
//	GET    /workers                       registered workers
//	POST   /workers/register              worker: register capabilities
//	POST   /workers/{id}/heartbeat        worker: extend leases, learn revocations
//	POST   /workers/{id}/lease            worker: long-poll for tasks
//	POST   /workers/{id}/drain            stop handing a worker new tasks
//	POST   /workers/{id}/deregister       remove a worker, releasing its leases
//	GET    /metrics                       metrics Snapshot
//
// Snapshot and RenderStatus give queue depths, lease expiries, throughput
// and latency quantiles from a t-digest sketch.
package fleet
