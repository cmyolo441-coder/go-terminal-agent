package sentinel

import (
	"errors"
	"fmt"
	"time"
)

// Config configures a Sentinel. The zero value is valid: every detector is
// on with the defaults documented on each field, applied by New.
type Config struct {
	// Clock defaults to SystemClock.
	Clock Clock
	// EventWindow is the number of recent events kept per session for
	// evidence (rounded up to a power of two; default 64).
	EventWindow int
	// EvidenceSize is the number of trailing events copied into an
	// incident (default 16).
	EvidenceSize int
	// MaxIncidents bounds the incident store; the oldest are dropped
	// (default 1000).
	MaxIncidents int
	// Cooldown suppresses a repeat of the same finding (same detector,
	// rule, subject and severity) within this period: the existing
	// incident's count is bumped instead (default 2m).
	Cooldown time.Duration
	// MaxTargetBytes truncates targets in exported evidence (default 512).
	MaxTargetBytes int
	// Redact, when set, is applied to targets and error text when they are
	// copied into incident evidence, for example to strip tokens from
	// command lines.
	Redact func(string) string
	// OnIncident is called, outside any lock, for every new incident.
	OnIncident func(Incident)

	Loop        LoopConfig
	Oscillation OscillationConfig
	Rate        RateConfig
	Burn        BurnConfig
	Sequence    SequenceConfig
	Deletion    DeletionConfig
	Errors      ErrorStormConfig
	Entropy     EntropyConfig
	Response    ResponseConfig
	Breaker     BreakerConfig
}

// LoopConfig configures loop detection.
type LoopConfig struct {
	Disabled bool
	// Window is how far back repeats count (default 3m).
	Window time.Duration
	// History is the number of recent actions compared (default 32, max 64).
	History int
	// Repeats is the number of identical actions, the current one
	// included, that is a loop (default 4, severity medium).
	Repeats int
	// RepeatsHigh escalates to high severity (default 8).
	RepeatsHigh int
	// NearRepeats is the number of near-identical actions that is a loop
	// (default 6, severity low).
	NearRepeats int
	// MaxHamming is the SimHash distance under which two actions are
	// near-identical (default 3).
	MaxHamming int
	// MinTokens is the number of tokens an action needs for near-duplicate
	// detection; SimHash is unreliable on very short inputs (default 5).
	MinTokens int
	// KeepAcrossWrites counts repeats across file writes. By default a
	// write or delete resets the comparison, since re-running a test after
	// an edit is progress, not a loop.
	KeepAcrossWrites bool
}

// OscillationConfig configures edit-oscillation detection.
type OscillationConfig struct {
	Disabled bool
	// Window forgets a file's history after this long without writes
	// (default 15m).
	Window time.Duration
	// Reversions is the number of returns to an earlier content that is an
	// oscillation (default 2, severity medium).
	Reversions int
	// ReversionsHigh escalates to high severity (default 4).
	ReversionsHigh int
	// Files is the number of files tracked per session (default 64).
	Files int
}

// RateConfig configures rate-anomaly detection.
type RateConfig struct {
	Disabled bool
	// Bucket is the aggregation interval (default 10s).
	Bucket time.Duration
	// Alpha is the EWMA smoothing factor (default 0.2).
	Alpha float64
	// Trend uses Holt's linear trend model instead of a flat EWMA.
	Trend bool
	// Beta is the trend smoothing factor when Trend is set (default 0.1).
	Beta float64
	// ZEnter starts an anomaly (default 4); ZExit ends it (default 2).
	ZEnter, ZExit float64
	// Warmup is the number of buckets before scoring (default 12).
	Warmup int
	// IdleBuckets is the longest run of empty buckets treated as zeros;
	// longer gaps are idle time and skipped (default 2).
	IdleBuckets int
	// MinValue is the per-bucket floor below which nothing is anomalous,
	// indexed by Metric. Zero entries take defaults.
	MinValue [NumMetrics]float64
	// MinStd floors the standard deviation, indexed by Metric.
	MinStd [NumMetrics]float64
}

// BurnConfig configures token and cost burn-rate forecasting.
type BurnConfig struct {
	Disabled bool
	// TokenBudget and CostBudget are per-session budgets; zero disables
	// that forecast. Sentinel.SetBudget overrides them per session.
	TokenBudget int64
	CostBudget  float64
	// Window is the number of samples in the regression (default 24, max 64).
	Window int
	// MinSamples is the number of samples needed (default 4).
	MinSamples int
	// MinSpan is the time the samples must cover (default 30s).
	MinSpan time.Duration
	// WarnHorizon flags exhaustion forecast within it (low severity,
	// default 15m); CriticalHorizon escalates to high (default 3m).
	WarnHorizon, CriticalHorizon time.Duration
}

// SequenceConfig configures dangerous-sequence detection.
type SequenceConfig struct {
	Disabled bool
	// NoBuiltins drops BuiltinPatterns.
	NoBuiltins bool
	// Disable lists built-in pattern names to drop.
	Disable []string
	// Patterns are added to the built-ins.
	Patterns []Pattern
	// Threads bounds partial matches per pattern per session (default 16).
	Threads int
}

// DeletionConfig configures mass-deletion detection.
type DeletionConfig struct {
	Disabled bool
	// Tau is the decay time constant (default 60s).
	Tau time.Duration
	// Threshold is the decayed deletion count that is high severity
	// (default 25); Critical is critical (default 100).
	Threshold, Critical float64
	// Wildcard is the weight of a glob, find -delete or git clean (default 10).
	Wildcard float64
}

// ErrorStormConfig configures error-storm detection.
type ErrorStormConfig struct {
	Disabled bool
	// Tau is the decay time constant (default 60s).
	Tau time.Duration
	// MinErrors is the decayed error count needed (default 5).
	MinErrors float64
	// Ratio is the decayed error ratio that starts a storm (default 0.6);
	// ExitRatio ends it (default 0.3).
	Ratio, ExitRatio float64
	// Consecutive failures that start a storm regardless of ratio (default 8).
	Consecutive int
}

// EntropyConfig configures high-entropy write detection.
type EntropyConfig struct {
	Disabled bool
	// MinBytes is the smallest payload inspected (default 512).
	MinBytes int
	// SampleBytes is the most inspected (default 8192).
	SampleBytes int
	// BinaryBits flags arbitrary content at or above this entropy in bits
	// per byte (default 7.2).
	BinaryBits float64
	// EncodedBits flags base64-alphabet content at or above it (default 5.4).
	EncodedBits float64
	// HexBits flags long hex-alphabet content at or above it (default 3.85).
	HexBits float64
	// AllowExt lists extensions where high entropy is expected; it
	// replaces the default list when set.
	AllowExt []string
	// AllowBase lists basenames where high entropy is expected (lockfiles,
	// checksums); it replaces the default list when set.
	AllowBase []string
}

// ResponseConfig maps findings to actions.
type ResponseConfig struct {
	// Actions maps severity to action. Zero entries take the defaults:
	// info→log, low→warn, medium→throttle, high→require approval,
	// critical→pause.
	Actions [numSeverities]Action
	// ThrottleRate is the sustained events per second allowed while
	// throttled (default 0.5); ThrottleBurst is the bucket size (default 3).
	ThrottleRate, ThrottleBurst float64
	// Decay is how long a throttle posture lasts without a new finding
	// (default 2m). It doubles, up to MaxDecay (default 16m), when a
	// session re-escalates within Decay of the last decay.
	Decay, MaxDecay time.Duration
	// StickyApproval makes require-approval a session posture like
	// throttle: every gated event asks until it decays.
	StickyApproval bool
	// KillAfter engages the kill switch after this many new critical
	// incidents within KillWindow across all sessions (default 3; negative
	// disables).
	KillAfter int
	// KillWindow defaults to 10m.
	KillWindow time.Duration
}

// BreakerConfig configures per-tool circuit breakers.
type BreakerConfig struct {
	Disabled bool
	// Failures is the consecutive failures that open the circuit (default 5).
	Failures int
	// OpenFor is the first open period (default 30s); it doubles on each
	// failed probe, up to MaxOpenFor (default 10m).
	OpenFor, MaxOpenFor time.Duration
	// Probes is the concurrent calls allowed while half-open (default 1).
	Probes int
	// Successes is the probe successes that close the circuit (default 2).
	Successes int
	// ProbeTimeout frees a probe slot whose result never arrived (default 2m).
	ProbeTimeout time.Duration
}

func defInt(p *int, v int) {
	if *p <= 0 {
		*p = v
	}
}

func defFloat(p *float64, v float64) {
	if *p <= 0 {
		*p = v
	}
}

func defDur(p *time.Duration, v time.Duration) {
	if *p <= 0 {
		*p = v
	}
}

// withDefaults fills zero fields and validates the result.
func (c Config) withDefaults() (Config, error) {
	if c.Clock == nil {
		c.Clock = SystemClock{}
	}
	defInt(&c.EventWindow, 64)
	w := 1
	for w < c.EventWindow {
		w <<= 1
	}
	c.EventWindow = w
	defInt(&c.EvidenceSize, 16)
	if c.EvidenceSize > c.EventWindow {
		c.EvidenceSize = c.EventWindow
	}
	defInt(&c.MaxIncidents, 1000)
	defDur(&c.Cooldown, 2*time.Minute)
	defInt(&c.MaxTargetBytes, 512)

	l := &c.Loop
	defDur(&l.Window, 3*time.Minute)
	defInt(&l.History, 32)
	if l.History > 64 {
		l.History = 64
	}
	defInt(&l.Repeats, 4)
	defInt(&l.RepeatsHigh, 8)
	defInt(&l.NearRepeats, 6)
	defInt(&l.MaxHamming, 3)
	defInt(&l.MinTokens, 5)
	if l.RepeatsHigh < l.Repeats {
		return c, errors.New("sentinel: Loop.RepeatsHigh < Loop.Repeats")
	}

	o := &c.Oscillation
	defDur(&o.Window, 15*time.Minute)
	defInt(&o.Reversions, 2)
	defInt(&o.ReversionsHigh, 4)
	defInt(&o.Files, 64)

	r := &c.Rate
	defDur(&r.Bucket, 10*time.Second)
	defFloat(&r.Alpha, 0.2)
	defFloat(&r.Beta, 0.1)
	defFloat(&r.ZEnter, 4)
	defFloat(&r.ZExit, 2)
	defInt(&r.Warmup, 12)
	defInt(&r.IdleBuckets, 2)
	if r.Alpha > 1 || r.Beta > 1 {
		return c, errors.New("sentinel: Rate.Alpha and Rate.Beta must be in (0, 1]")
	}
	if r.ZExit >= r.ZEnter {
		return c, errors.New("sentinel: Rate.ZExit must be below Rate.ZEnter")
	}
	for m := Metric(0); m < NumMetrics; m++ {
		defFloat(&r.MinValue[m], metricDefaults[m].minValue)
		defFloat(&r.MinStd[m], metricDefaults[m].minStd)
	}

	b := &c.Burn
	defInt(&b.Window, 24)
	if b.Window > sampleCap {
		b.Window = sampleCap
	}
	defInt(&b.MinSamples, 4)
	if b.MinSamples < 2 {
		b.MinSamples = 2
	}
	defDur(&b.MinSpan, 30*time.Second)
	defDur(&b.WarnHorizon, 15*time.Minute)
	defDur(&b.CriticalHorizon, 3*time.Minute)

	defInt(&c.Sequence.Threads, defaultThreads)

	d := &c.Deletion
	defDur(&d.Tau, time.Minute)
	defFloat(&d.Threshold, 25)
	defFloat(&d.Critical, 100)
	defFloat(&d.Wildcard, 10)

	e := &c.Errors
	defDur(&e.Tau, time.Minute)
	defFloat(&e.MinErrors, 5)
	defFloat(&e.Ratio, 0.6)
	defFloat(&e.ExitRatio, 0.3)
	defInt(&e.Consecutive, 8)
	if e.ExitRatio >= e.Ratio {
		return c, errors.New("sentinel: Errors.ExitRatio must be below Errors.Ratio")
	}

	en := &c.Entropy
	defInt(&en.MinBytes, 512)
	defInt(&en.SampleBytes, 8192)
	defFloat(&en.BinaryBits, 7.2)
	defFloat(&en.EncodedBits, 5.4)
	defFloat(&en.HexBits, 3.85)
	if en.AllowExt == nil {
		en.AllowExt = defaultAllowExt
	}
	if en.AllowBase == nil {
		en.AllowBase = defaultAllowBase
	}

	rc := &c.Response
	def := [numSeverities]Action{ActionNone, ActionLog, ActionWarn, ActionThrottle, ActionRequireApproval, ActionPause}
	for s := range rc.Actions {
		if rc.Actions[s] == ActionNone {
			rc.Actions[s] = def[s]
		}
		if rc.Actions[s] >= numActions {
			return c, fmt.Errorf("sentinel: invalid action for severity %s", Severity(s))
		}
	}
	defFloat(&rc.ThrottleRate, 0.5)
	defFloat(&rc.ThrottleBurst, 3)
	defDur(&rc.Decay, 2*time.Minute)
	defDur(&rc.MaxDecay, 16*time.Minute)
	if rc.MaxDecay < rc.Decay {
		rc.MaxDecay = rc.Decay
	}
	if rc.KillAfter == 0 {
		rc.KillAfter = 3
	}
	defDur(&rc.KillWindow, 10*time.Minute)

	br := &c.Breaker
	defInt(&br.Failures, 5)
	defDur(&br.OpenFor, 30*time.Second)
	defDur(&br.MaxOpenFor, 10*time.Minute)
	if br.MaxOpenFor < br.OpenFor {
		br.MaxOpenFor = br.OpenFor
	}
	defInt(&br.Probes, 1)
	defInt(&br.Successes, 2)
	defDur(&br.ProbeTimeout, 2*time.Minute)
	return c, nil
}

var defaultAllowExt = []string{
	".png", ".jpg", ".jpeg", ".gif", ".webp", ".ico", ".bmp", ".tiff", ".avif", ".heic",
	".gz", ".tgz", ".zip", ".bz2", ".xz", ".zst", ".7z", ".rar", ".tar", ".jar", ".war", ".whl",
	".woff", ".woff2", ".ttf", ".otf", ".eot", ".pdf", ".mp3", ".mp4", ".wav", ".ogg", ".webm", ".mov",
	".wasm", ".so", ".dylib", ".dll", ".exe", ".a", ".o", ".pyc", ".class", ".db", ".sqlite",
	".pb", ".onnx", ".bin", ".map", ".lock", ".sum",
}

var defaultAllowBase = []string{
	"go.sum", "package-lock.json", "yarn.lock", "pnpm-lock.yaml", "cargo.lock", "poetry.lock",
	"composer.lock", "gemfile.lock", "flake.lock", "uv.lock", "bun.lockb", "checksums.txt", "sha256sums",
}
