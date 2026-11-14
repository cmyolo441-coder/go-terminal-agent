// Package evals measures agent quality deterministically.
//
// It has three layers that can be used independently:
//
//   - Cassettes (Cassette, Recorder, Mode) wrap any llm.Provider and record
//     each request together with its streamed events and final response to a
//     JSONL file, then replay them byte-for-byte. Requests are matched by a
//     SHA-256 hash of a canonical form that ignores volatile data (tool-use
//     ids, provider-native replay blobs, workspace paths, configurable
//     fields), and secrets are scrubbed by an injected function before
//     anything touches disk. A replay miss reports a structural diff against
//     the closest recorded request.
//
//   - Suites (Suite, Case, GraderSpec) are JSON specifications of evaluation
//     cases: a fixture workspace, a prompt, resource limits and weighted
//     graders (file checks, commands, tool-call predicates, output regexes,
//     JSON Schema validation and LLM-as-judge rubrics).
//
//   - The Runner executes every case several times in parallel, each in an
//     isolated copy of its fixture, through an injected AgentFunc so the
//     harness stays decoupled from internal/agent. Results carry pass@k,
//     pass^k, mean/stddev and bootstrap confidence intervals; Compare runs
//     paired significance tests (McNemar, Wilcoxon signed-rank) between two
//     result files to flag regressions. Reports render as a terminal table,
//     JSON and JUnit XML.
package evals
