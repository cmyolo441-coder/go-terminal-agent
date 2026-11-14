// Package workflow is a durable DAG workflow engine for orchestrating
// multi-step, multi-agent pipelines.
//
// Where package subagent fans a handful of free-form tasks out to child
// agents and package autopilot drives one plan-execute-verify loop, workflow
// runs an explicit, declarative graph of typed steps — shell commands,
// sub-agents, or any host-registered Executor — with the guarantees a long
// pipeline needs: it survives crashes, retries what is retryable, undoes
// what it already did when it fails, and never runs more in parallel than it
// was told to.
//
// # Spec
//
// A workflow is a JSON document (see Spec). Each step names a registered
// executor type and has templated inputs, dependencies, an optional `when`
// condition, an optional matrix fan-out, a retry policy (attempt cap,
// exponential backoff with full jitter, retryable error classes), a
// per-attempt timeout, an optional compensating step (saga), concurrency
// groups and declared outputs. Decoding is strict: unknown fields, cycles
// (reported with the cycle path), missing dependencies and references to
// undeclared inputs, outputs or non-ancestor steps are rejected before
// anything runs.
//
// Strings in inputs may embed expressions as ${...}:
//
//	${inputs.target}                 a workflow input
//	${steps.build.outputs.binary}    an output of an ancestor step
//	${env.HOME}                      a host environment variable
//	${matrix.os}                     the current matrix value
//	${format('{0}-{1}', matrix.os, run.id)}
//
// The expression language (see ParseExpr) has literals, member and index
// access, arithmetic, comparisons, &&, ||, !, `in`, and a fixed set of pure
// functions. It is parsed into an AST and interpreted; nothing is ever
// evaluated by reflection or code generation, and parse depth and input
// length are bounded.
//
// # Scheduling
//
// The scheduler is a single coordinator goroutine. Steps become ready when
// all their dependencies are terminal and their condition holds; ready steps
// are dispatched highest critical-path rank first (ties broken by spec
// order, so runs are deterministic), subject to the global concurrency limit
// and every concurrency group the step belongs to. A step waiting for a
// retry backoff does not hold a slot. In the default fail-fast mode the
// first unrecoverable failure cancels running steps and stops dispatch;
// with continue_on_failure independent branches keep going.
//
// # Durability
//
// Every state transition is appended to a write-ahead journal (JSONL, one
// CRC32-C checksummed record per line, fsynced before the transition takes
// effect). Resume replays the journal, truncates a torn tail left by a crash,
// skips completed steps and re-runs the ones that were in flight with the
// same idempotency key, so executors that talk to external systems can make
// re-execution safe. When a run fails, completed steps that declare a
// compensation are compensated in reverse topological order, and that phase
// is journaled and resumable too.
//
// # Observability
//
// Options.Observer and Options.Events receive run and step lifecycle events;
// Result.Timeline and RenderTimeline draw a Gantt chart for the terminal.
package workflow
