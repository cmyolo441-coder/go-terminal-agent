// Package sentinel is a runtime behavioural guardrail for the agent.
//
// Where internal/policy decides whether a single tool call is permitted and
// internal/budget caps spend, sentinel watches the *stream* of what the agent
// does and stops runaway or dangerous behaviour as it develops: a model stuck
// re-running the same command, an edit that flips a file back and forth, a
// burst of tool calls far above the session's baseline, token burn that will
// exhaust the budget within minutes, a download that is made executable and
// then run, a secret file read shortly before a network request, a storm of
// failing calls, or a high-entropy blob written somewhere it does not belong.
//
// # Event model
//
// Every observable action is an Event: a tool call about to run, its result,
// a model request or response, a file write, a shell command, a network
// request. Callers feed events through Sentinel.Observe, or through a Guard,
// which wraps an existing decision callback and adds the sentinel's verdict
// to it (Before) and records outcomes (After).
//
// # Detectors
//
// Each detector is explainable (every finding carries the numbers that
// triggered it), configurable (thresholds live in Config) and independent:
//
//   - loop: identical (normalised fingerprint) or near-identical (64-bit
//     SimHash, Hamming distance) actions repeated within a window with no
//     intervening file change.
//   - oscillation: a file whose content digest returns to an earlier value
//     (A→B→A) repeatedly.
//   - rate: per-metric EWMA/EWMVar (optionally Holt linear trend) over fixed
//     buckets, flagged by z-score with enter/exit hysteresis.
//   - burn: least-squares regression of cumulative tokens and cost over a
//     sliding window, forecasting time to budget exhaustion.
//   - sequence: a small NFA over event patterns (download → chmod +x → run,
//     secret read → network, writes to ~/.ssh, shell rc files, CI configs and
//     git hooks, catastrophic deletes, piping downloads into a shell).
//   - deletion: an exponentially decayed count of deletions.
//   - errors: an error-storm detector on decayed error ratio and consecutive
//     failures.
//   - entropy: Shannon entropy and alphabet analysis of written content.
//
// # Responses
//
// Findings map, by severity or by an explicit override, to actions: log,
// warn, require approval, throttle (a token bucket), pause (until Resume) and
// kill (a global switch, until ClearKill). Throttle and pause are sticky
// session postures that decay after a quiet period; the decay period doubles
// when a session re-escalates soon after decaying, which stops flapping. Each
// tool also has a circuit breaker (closed / open / half-open with probes and
// exponential back-off).
//
// # Incidents
//
// The first finding for a (detector, rule, subject, severity) key creates an
// Incident with a stable ID, the evidence window (the session's most recent
// events, with the events that matched marked) and a human explanation;
// repeats within the cooldown only bump its count. Incidents export as JSON
// and render as a compact terminal summary.
//
// # Performance
//
// The hot path takes one read-locked shard lookup and one per-session mutex;
// distinct sessions never contend. Observe does not allocate unless a finding
// creates a new incident; see BenchmarkObserve and TestObserveZeroAlloc.
package sentinel
