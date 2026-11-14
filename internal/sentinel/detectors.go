package sentinel

import (
	"bytes"
	"fmt"
	"math"
	"path"
	"strings"
	"time"
)

// Detector names, as they appear in incidents and verdicts.
const (
	DetectorLoop        = "loop"
	DetectorOscillation = "oscillation"
	DetectorRate        = "rate"
	DetectorBurn        = "burn"
	DetectorSequence    = "sequence"
	DetectorDeletion    = "deletion"
	DetectorErrors      = "errors"
	DetectorEntropy     = "entropy"
	DetectorBreaker     = "breaker"
)

// maxFindings bounds the findings one event can produce.
const maxFindings = 16

// explainFunc renders a finding for humans. It runs only when a finding
// creates an incident, so detectors stay allocation-free on the hot path.
type explainFunc func(f *finding) (summary string, details []string, metrics map[string]float64)

// finding is one detector hit, held in a per-session buffer.
type finding struct {
	detector string
	rule     string
	subject  string // what the finding is about: tool, path or metric
	target   string // the triggering event's target
	sev      Severity
	action   Action // overrides the severity's action when set
	key      uint64 // dedupe key: detector, rule, subject identity, severity
	explain  explainFunc
	v        [6]float64
	seqs     [MaxSteps]uint64
	nseq     int
	pattern  *Pattern
}

func (f *finding) addSeq(seq uint64) {
	if f.nseq < len(f.seqs) {
		f.seqs[f.nseq] = seq
		f.nseq++
	}
}

func findingKey(detector, rule string, ident uint64, sev Severity) uint64 {
	h := fnvAdd(fnvOffset, detector)
	h = fnvAdd(h, rule)
	h = fnvAddU64(h, ident)
	return fnvAddU64(h, uint64(sev))
}

// emit appends a finding to the session buffer and returns it for filling,
// or nil when the buffer is full.
func (ss *session) emit(detector, rule string, sev Severity, ident uint64, explain explainFunc) *finding {
	if len(ss.findings) == cap(ss.findings) {
		return nil
	}
	ss.findings = ss.findings[:len(ss.findings)+1]
	f := &ss.findings[len(ss.findings)-1]
	*f = finding{detector: detector, rule: rule, sev: sev, key: findingKey(detector, rule, ident, sev), explain: explain}
	return f
}

func secs(ns float64) string { return (time.Duration(ns) * time.Second).Round(time.Second).String() }

// ---------------------------------------------------------------- loop

const loopCap = 64

type loopEntry struct {
	fp, sim uint64
	at      int64
	seq     uint64
	epoch   uint32
	toks    uint16
	write   bool
}

type loopState struct {
	ring  [loopCap]loopEntry
	n     int
	head  int
	epoch uint32
}

func (e *engine) detectLoop(ss *session, in *Input, digest uint64, now int64, seq uint64) {
	c := &e.cfg.Loop
	st := &ss.loop
	write := in.Kind == KindFileWrite || in.Kind == KindFileDelete
	fp := fingerprint(in.Tool, in.Lower)
	if write {
		// A write is identical only if it writes the same content.
		fp = fnvAddU64(fp, digest)
	}
	sim, toks := simhash(in.Tool, in.Lower)
	cut := now - int64(c.Window)

	exact, near := 1, 1
	var exactSeqs, nearSeqs [MaxSteps]uint64
	ne, nn := 0, 0
	for i := 0; i < st.n; i++ {
		x := &st.ring[(st.head-1-i+loopCap)%loopCap]
		if x.at < cut {
			break // entries are in time order, newest first
		}
		if !write && !x.write && !c.KeepAcrossWrites && x.epoch != st.epoch {
			continue
		}
		if x.write != write {
			continue
		}
		switch {
		case x.fp == fp:
			exact++
			near++
			if ne < MaxSteps-1 {
				exactSeqs[ne] = x.seq
				ne++
			}
			if nn < MaxSteps-1 {
				nearSeqs[nn] = x.seq
				nn++
			}
		case toks >= c.MinTokens && int(x.toks) >= c.MinTokens && hamming(x.sim, sim) <= c.MaxHamming:
			near++
			if nn < MaxSteps-1 {
				nearSeqs[nn] = x.seq
				nn++
			}
		}
	}

	st.ring[st.head] = loopEntry{fp: fp, sim: sim, at: now, seq: seq, epoch: st.epoch, toks: uint16(min(toks, math.MaxUint16)), write: write}
	st.head = (st.head + 1) % loopCap
	if st.n < c.History {
		st.n++
	}
	if write && !c.KeepAcrossWrites {
		st.epoch++
	}

	var f *finding
	switch {
	case exact >= c.RepeatsHigh:
		f = ss.emit(DetectorLoop, "exact", SeverityHigh, fp, explainLoop)
	case exact >= c.Repeats:
		f = ss.emit(DetectorLoop, "exact", SeverityMedium, fp, explainLoop)
	case near >= c.NearRepeats:
		f = ss.emit(DetectorLoop, "near", SeverityLow, fnv64(in.Tool), explainLoop)
	default:
		return
	}
	if f == nil {
		return
	}
	f.subject, f.target = in.Tool, in.Target
	f.v[0], f.v[1], f.v[2], f.v[3] = float64(exact), float64(near), c.Window.Seconds(), float64(c.MaxHamming)
	seqs, n := exactSeqs, ne
	if f.rule == "near" {
		seqs, n = nearSeqs, nn
	}
	for i := n - 1; i >= 0; i-- {
		f.addSeq(seqs[i])
	}
	f.addSeq(seq)
}

func explainLoop(f *finding) (string, []string, map[string]float64) {
	m := map[string]float64{"identical": f.v[0], "near_identical": f.v[1], "window_seconds": f.v[2]}
	if f.rule == "exact" {
		return fmt.Sprintf("%s: identical call repeated %d× within %s", orTool(f.subject), int(f.v[0]), secs(f.v[2])),
			[]string{
				"The same action (after normalising whitespace and quoting) ran " + fmt.Sprint(int(f.v[0])) + " times with no file change in between.",
				"Repeated: " + truncateBytes(f.target, 160),
				"An agent that repeats an action without changing anything is usually stuck; consider a different approach.",
			}, m
	}
	m["max_hamming"] = f.v[3]
	return fmt.Sprintf("%s: %d near-identical calls within %s", orTool(f.subject), int(f.v[1]), secs(f.v[2])),
		[]string{
			fmt.Sprintf("%d actions had 64-bit SimHash fingerprints within Hamming distance %d of each other.", int(f.v[1]), int(f.v[3])),
			"Latest: " + truncateBytes(f.target, 160),
		}, m
}

func orTool(s string) string {
	if s == "" {
		return "(no tool)"
	}
	return s
}

// ---------------------------------------------------------- oscillation

type fileHist struct {
	path       uint64
	digests    [4]uint64
	n          int
	last       int64
	reversions int
	seqs       [MaxSteps]uint64
	nseq       int
}

func (e *engine) detectOscillation(ss *session, in *Input, digest uint64, now int64, seq uint64) {
	c := &e.cfg.Oscillation
	ph := fnv64b(in.Lower)
	var h *fileHist
	lru := 0
	for i := range ss.osc {
		if ss.osc[i].path == ph && ss.osc[i].n > 0 {
			h = &ss.osc[i]
			break
		}
		if ss.osc[i].last < ss.osc[lru].last {
			lru = i
		}
	}
	if h == nil || now-h.last > int64(c.Window) {
		if h == nil {
			h = &ss.osc[lru]
		}
		*h = fileHist{path: ph, n: 1, last: now}
		h.digests[0] = digest
		h.seqs[0], h.nseq = seq, 1
		return
	}
	h.last = now
	if h.digests[(h.n-1)%len(h.digests)] == digest {
		return // rewrite with identical content
	}
	reverted := false
	for i := 0; i < min(h.n, len(h.digests)); i++ {
		if h.digests[i] == digest {
			reverted = true
			break
		}
	}
	h.digests[h.n%len(h.digests)] = digest
	h.n++
	if h.nseq < MaxSteps {
		h.seqs[h.nseq] = seq
		h.nseq++
	} else {
		copy(h.seqs[:], h.seqs[1:])
		h.seqs[MaxSteps-1] = seq
	}
	if !reverted {
		return
	}
	h.reversions++
	sev := SeverityNone
	switch {
	case h.reversions >= c.ReversionsHigh:
		sev = SeverityHigh
	case h.reversions >= c.Reversions:
		sev = SeverityMedium
	default:
		return
	}
	f := ss.emit(DetectorOscillation, "revert", sev, ph, explainOscillation)
	if f == nil {
		return
	}
	f.subject, f.target = in.Target, in.Target
	f.v[0], f.v[1] = float64(h.reversions), float64(h.n)
	for i := 0; i < h.nseq; i++ {
		f.addSeq(h.seqs[i])
	}
}

func explainOscillation(f *finding) (string, []string, map[string]float64) {
	return fmt.Sprintf("%s: content reverted to an earlier version %d×", path.Base(f.subject), int(f.v[0])),
		[]string{
			fmt.Sprintf("Across %d writes, the file returned to a previous content digest %d times (A→B→A).", int(f.v[1]), int(f.v[0])),
			"File: " + truncateBytes(f.subject, 200),
			"Flip-flopping edits usually mean two goals are in conflict; stop and reconcile them.",
		}, map[string]float64{"reversions": f.v[0], "writes": f.v[1]}
}

// ---------------------------------------------------------------- rate

// Metric is a rate-anomaly series.
type Metric uint8

// Metrics tracked per session.
const (
	MetricCalls Metric = iota
	MetricTokens
	MetricCost
	MetricBytesWritten
	MetricNet
	MetricErrors
	NumMetrics
)

var metricDefaults = [NumMetrics]struct {
	name             string
	minValue, minStd float64
}{
	{"calls", 8, 1},
	{"tokens", 200_000, 5_000},
	{"cost_usd", 1, 0.05},
	{"bytes_written", 4 << 20, 64 << 10},
	{"net_requests", 6, 1},
	{"errors", 6, 1},
}

func (m Metric) String() string {
	if m < NumMetrics {
		return metricDefaults[m].name
	}
	return fmt.Sprintf("metric(%d)", uint8(m))
}

type rateSeries struct {
	bucket  int64 // index of the open bucket
	acc     float64
	ewm     EWM
	holt    Holt
	closed  int
	active  bool
	started bool
}

func (e *engine) expected(s *rateSeries) (mean, std float64) {
	if e.cfg.Rate.Trend {
		return s.holt.Forecast(), s.holt.Res.Std()
	}
	return s.ewm.Mean, s.ewm.Std()
}

func (e *engine) zscore(s *rateSeries, m Metric, x float64) float64 {
	mean, std := e.expected(s)
	return (x - mean) / math.Max(std, e.cfg.Rate.MinStd[m])
}

func (e *engine) feed(s *rateSeries, m Metric, x float64) {
	c := &e.cfg.Rate
	if s.closed >= c.Warmup {
		// Winsorise so an anomaly does not drag the baseline up with it.
		mean, std := e.expected(s)
		x = math.Min(x, mean+c.ZEnter*math.Max(std, c.MinStd[m]))
	}
	if c.Trend {
		s.holt.Update(x)
	} else {
		s.ewm.Update(x)
	}
	s.closed++
}

// roll closes buckets up to the one containing now.
func (e *engine) roll(s *rateSeries, m Metric, now int64) {
	c := &e.cfg.Rate
	idx := now / int64(c.Bucket)
	if !s.started {
		s.bucket, s.started = idx, true
		return
	}
	if idx <= s.bucket {
		return
	}
	closedValue := s.acc
	if s.active && e.zscore(s, m, closedValue) < c.ZExit {
		s.active = false
	}
	e.feed(s, m, closedValue)
	if gap := idx - s.bucket - 1; gap > 0 && gap <= int64(c.IdleBuckets) {
		for i := int64(0); i < gap; i++ {
			if s.active && e.zscore(s, m, 0) < c.ZExit {
				s.active = false
			}
			e.feed(s, m, 0)
		}
	} else if gap > int64(c.IdleBuckets) {
		s.active = false // idle: whatever was happening has stopped
	}
	s.bucket, s.acc = idx, 0
}

func (e *engine) detectRate(ss *session, ev *Event, now int64, seq uint64) {
	var add [NumMetrics]float64
	if ev.Kind.IsAction() {
		add[MetricCalls] = 1
	}
	if ev.Kind == KindNet {
		add[MetricNet] = 1
	}
	if ev.Kind == KindFileWrite && ev.Bytes > 0 {
		add[MetricBytesWritten] = float64(ev.Bytes)
	}
	if ev.Kind == KindLLMResponse {
		add[MetricTokens] = float64(ev.Tokens)
		add[MetricCost] = ev.Cost
	}
	if ev.Kind.isResult() && ev.Err != nil {
		add[MetricErrors] = 1
	}
	c := &e.cfg.Rate
	for m := Metric(0); m < NumMetrics; m++ {
		if add[m] <= 0 {
			continue
		}
		s := &ss.rate[m]
		e.roll(s, m, now)
		s.acc += add[m]
		if s.active || s.closed < c.Warmup || s.acc < c.MinValue[m] {
			continue
		}
		z := e.zscore(s, m, s.acc)
		if z < c.ZEnter {
			continue
		}
		s.active = true
		sev := SeverityMedium
		if z >= 2*c.ZEnter {
			sev = SeverityHigh
		}
		f := ss.emit(DetectorRate, metricDefaults[m].name, sev, uint64(m), explainRate)
		if f == nil {
			continue
		}
		mean, std := e.expected(s)
		f.subject, f.target = metricDefaults[m].name, ev.Target
		f.v[0], f.v[1], f.v[2], f.v[3], f.v[4] = s.acc, mean, math.Max(std, c.MinStd[m]), z, c.Bucket.Seconds()
		if c.Trend {
			f.v[5] = s.holt.Trend
		}
		f.addSeq(seq)
	}
}

func explainRate(f *finding) (string, []string, map[string]float64) {
	return fmt.Sprintf("%s rate %.4g per %s is %.1fσ above baseline %.4g", f.subject, f.v[0], secs(f.v[4]), f.v[3], f.v[1]),
		[]string{
			fmt.Sprintf("Current %s bucket: %.4g; exponentially weighted baseline %.4g ± %.4g (z = %.2f).", secs(f.v[4]), f.v[0], f.v[1], f.v[2], f.v[3]),
			"The anomaly clears once a bucket falls back within the exit threshold.",
		}, map[string]float64{"value": f.v[0], "baseline": f.v[1], "stddev": f.v[2], "z": f.v[3], "bucket_seconds": f.v[4], "trend": f.v[5]}
}

// ---------------------------------------------------------------- burn

type burnState struct {
	used   [2]float64 // tokens, cost
	budget [2]float64
	ring   [2]sampleRing
}

var burnNames = [2]string{"tokens", "cost_usd"}

// BurnForecast is a time-to-budget-exhaustion estimate.
type BurnForecast struct {
	Metric string  `json:"metric"`
	Used   float64 `json:"used"`
	Budget float64 `json:"budget"`
	// RatePerMinute is the regression slope.
	RatePerMinute float64 `json:"rate_per_minute"`
	// R2 is the fit's coefficient of determination.
	R2 float64 `json:"r2"`
	// TimeToExhaustion is the forecast; valid when OK.
	TimeToExhaustion time.Duration `json:"time_to_exhaustion"`
	// OK is false when there are too few samples or spend is not rising.
	OK bool `json:"ok"`
}

func (e *engine) forecast(b *burnState, i int) BurnForecast {
	c := &e.cfg.Burn
	fc := BurnForecast{Metric: burnNames[i], Used: b.used[i], Budget: b.budget[i]}
	r := &b.ring[i]
	if b.budget[i] <= 0 || r.n < c.MinSamples {
		return fc
	}
	slope, r2, span, ok := r.fit()
	if !ok || span < int64(c.MinSpan) {
		return fc
	}
	fc.RatePerMinute, fc.R2 = slope*60, r2
	remaining := b.budget[i] - b.used[i]
	switch {
	case remaining <= 0:
		fc.OK = true
	case slope > 0:
		fc.TimeToExhaustion = time.Duration(remaining / slope * 1e9)
		fc.OK = true
	}
	return fc
}

func (e *engine) detectBurn(ss *session, ev *Event, now int64, seq uint64) {
	add := [2]float64{float64(ev.Tokens), ev.Cost}
	c := &e.cfg.Burn
	b := &ss.burn
	for i := range add {
		if add[i] <= 0 {
			continue
		}
		b.used[i] += add[i]
		b.ring[i].push(now, b.used[i])
		fc := e.forecast(b, i)
		if !fc.OK {
			continue
		}
		var sev Severity
		switch {
		case fc.Used >= fc.Budget:
			sev = SeverityHigh
		case fc.TimeToExhaustion <= c.CriticalHorizon:
			sev = SeverityHigh
		case fc.TimeToExhaustion <= c.WarnHorizon:
			sev = SeverityLow
		default:
			continue
		}
		f := ss.emit(DetectorBurn, burnNames[i], sev, uint64(i), explainBurn)
		if f == nil {
			continue
		}
		f.subject = burnNames[i]
		f.v[0], f.v[1], f.v[2], f.v[3], f.v[4] = fc.Used, fc.Budget, fc.RatePerMinute, fc.TimeToExhaustion.Seconds(), fc.R2
		f.addSeq(seq)
	}
}

func explainBurn(f *finding) (string, []string, map[string]float64) {
	m := map[string]float64{"used": f.v[0], "budget": f.v[1], "rate_per_minute": f.v[2], "seconds_to_exhaustion": f.v[3], "r2": f.v[4]}
	if f.v[0] >= f.v[1] {
		return fmt.Sprintf("%s budget exhausted: %.4g of %.4g", f.subject, f.v[0], f.v[1]),
			[]string{"Cumulative spend has reached the session budget."}, m
	}
	return fmt.Sprintf("%s budget exhausted in ~%s at %.4g/min", f.subject, secs(f.v[3]), f.v[2]),
		[]string{
			fmt.Sprintf("Used %.4g of %.4g; least-squares slope over the recent window is %.4g/min (R² = %.2f).", f.v[0], f.v[1], f.v[2], f.v[4]),
			"At this burn rate the budget runs out soon; consider a cheaper model or a narrower task.",
		}, m
}

// ------------------------------------------------------------ sequence

func (e *engine) detectSequence(ss *session, in *Input, now int64, seq uint64) {
	for _, m := range e.nfa.step(ss.nfa, in, now, seq) {
		p := &e.nfa.pats[m.pat]
		f := ss.emit(DetectorSequence, p.Name, p.Severity, fnv64(m.th.capture), explainSequence)
		if f == nil {
			return
		}
		f.subject, f.target, f.action, f.pattern = m.th.capture, in.Target, p.Action, p
		for i := 0; i < len(p.Steps); i++ {
			f.addSeq(m.th.seqs[i])
		}
	}
}

func explainSequence(f *finding) (string, []string, map[string]float64) {
	p := f.pattern
	names := make([]string, len(p.Steps))
	for i, s := range p.Steps {
		names[i] = s.Name
		if names[i] == "" {
			names[i] = fmt.Sprintf("step %d", i+1)
		}
	}
	summary := p.Description
	if summary == "" {
		summary = p.Name
	}
	details := []string{"Matched pattern " + p.Name + ": " + strings.Join(names, " → ")}
	if f.subject != "" {
		details = append(details, "Bound value: "+truncateBytes(f.subject, 120))
	}
	details = append(details, "Trigger: "+truncateBytes(f.target, 200))
	return summary, details, map[string]float64{"steps": float64(len(p.Steps)), "within_seconds": p.Within.Seconds()}
}

// ------------------------------------------------------------ deletion

type deletionState struct {
	score decay
	level Severity
}

func (e *engine) detectDeletion(ss *session, in *Input, now int64, seq uint64) {
	c := &e.cfg.Deletion
	w := 0.0
	switch in.Kind {
	case KindFileDelete:
		w = operandWeight(in.Lower, c.Wildcard)
	case KindExec, KindToolCall:
		w = in.shell().deletionWeight(c.Wildcard)
	}
	tau := float64(c.Tau)
	st := &ss.del
	if w <= 0 {
		if st.level != SeverityNone && st.score.value(now, tau) < c.Threshold/2 {
			st.level = SeverityNone
		}
		return
	}
	v := st.score.add(now, w, tau)
	if st.level != SeverityNone && v < c.Threshold/2 {
		st.level = SeverityNone
	}
	sev := SeverityNone
	switch {
	case v >= c.Critical:
		sev = SeverityCritical
	case v >= c.Threshold:
		sev = SeverityHigh
	}
	if sev <= st.level {
		return
	}
	st.level = sev
	f := ss.emit(DetectorDeletion, "mass-delete", sev, 0, explainDeletion)
	if f == nil {
		return
	}
	f.subject, f.target = in.Tool, in.Target
	f.v[0], f.v[1], f.v[2] = v, c.Tau.Seconds(), w
	f.addSeq(seq)
}

func explainDeletion(f *finding) (string, []string, map[string]float64) {
	return fmt.Sprintf("mass deletion: ~%.0f files deleted in the last %s", f.v[0], secs(f.v[1])),
		[]string{
			fmt.Sprintf("Exponentially decayed deletion count %.1f (τ = %s); this event added %.0f (globs, find -delete and git clean count as many).", f.v[0], secs(f.v[1]), f.v[2]),
			"Trigger: " + truncateBytes(f.target, 200),
		}, map[string]float64{"decayed_count": f.v[0], "tau_seconds": f.v[1], "weight": f.v[2]}
}

// -------------------------------------------------------------- errors

type errorState struct {
	errs, total decay
	consec      int
	active      bool
}

func (e *engine) detectErrors(ss *session, ev *Event, now int64, seq uint64) {
	c := &e.cfg.Errors
	tau := float64(c.Tau)
	st := &ss.errs
	total := st.total.add(now, 1, tau)
	var errs float64
	if ev.Err != nil {
		errs = st.errs.add(now, 1, tau)
		st.consec++
	} else {
		errs = st.errs.value(now, tau)
		st.consec = 0
	}
	ratio := errs / math.Max(total, 1e-9)
	if st.active {
		if ratio < c.ExitRatio && st.consec == 0 {
			st.active = false
		}
		return
	}
	storm := errs >= c.MinErrors && ratio >= c.Ratio
	if !storm && st.consec < c.Consecutive {
		return
	}
	st.active = true
	sev := SeverityMedium
	if st.consec >= 2*c.Consecutive || ratio >= 0.9 && errs >= 2*c.MinErrors {
		sev = SeverityHigh
	}
	f := ss.emit(DetectorErrors, "storm", sev, 0, explainErrors)
	if f == nil {
		return
	}
	f.subject, f.target = ev.Tool, ev.Target
	f.v[0], f.v[1], f.v[2], f.v[3] = errs, ratio, float64(st.consec), c.Tau.Seconds()
	f.addSeq(seq)
}

func explainErrors(f *finding) (string, []string, map[string]float64) {
	return fmt.Sprintf("error storm: %.0f%% of recent calls failing (%d in a row)", f.v[1]*100, int(f.v[2])),
		[]string{
			fmt.Sprintf("Decayed error count %.1f, error ratio %.2f over τ = %s, %d consecutive failures.", f.v[0], f.v[1], secs(f.v[3]), int(f.v[2])),
			"Retrying a failing approach rarely helps; read the errors and change course.",
		}, map[string]float64{"errors": f.v[0], "ratio": f.v[1], "consecutive": f.v[2], "tau_seconds": f.v[3]}
}

// ------------------------------------------------------------- entropy

// blobClass is the kind of high-entropy content found.
type blobClass uint8

const (
	blobNone blobClass = iota
	blobBinary
	blobBase64
	blobHex
)

var blobNames = [...]string{"", "binary", "base64", "hex"}

// classifyBlob measures entropy and alphabet of a payload sample.
func classifyBlob(b []byte, c *EntropyConfig) (blobClass, float64) {
	h := shannon(b)
	if h >= c.BinaryBits {
		return blobBinary, h
	}
	var b64, hex, space, run, longest int
	for _, ch := range b {
		switch {
		case isSpace(ch):
			space++
			run = 0
			continue
		case '0' <= ch && ch <= '9' || 'a' <= ch && ch <= 'f' || 'A' <= ch && ch <= 'F':
			hex++
			b64++
		case 'a' <= ch && ch <= 'z' || 'A' <= ch && ch <= 'Z' || ch == '+' || ch == '/' || ch == '=' || ch == '-' || ch == '_':
			b64++
		}
		run++
		longest = max(longest, run)
	}
	body := len(b) - space
	if body <= 0 || longest < 64 {
		return blobNone, h
	}
	// Measure the non-whitespace body: line breaks dilute entropy.
	switch {
	case float64(hex) >= 0.98*float64(body) && h >= c.HexBits && body >= 256:
		return blobHex, h
	case float64(b64) >= 0.98*float64(body) && h >= c.EncodedBits:
		return blobBase64, h
	}
	return blobNone, h
}

func (e *engine) allowedBlobPath(p []byte) bool {
	c := &e.cfg.Entropy
	base := baseOf(p)
	ext := ""
	if i := bytes.LastIndexByte(base, '.'); i > 0 {
		ext = string(base[i:])
	}
	for _, a := range c.AllowExt {
		if strings.EqualFold(ext, a) {
			return true
		}
	}
	for _, a := range c.AllowBase {
		if strings.EqualFold(string(base), a) {
			return true
		}
	}
	return false
}

// unusualPath reports paths where an opaque blob is suspicious: outside the
// workspace, in temp directories, in hidden directories, or without an
// extension.
func unusualPath(p []byte, flags EventFlags) bool {
	if flags&FlagOutsideRoot != 0 {
		return true
	}
	for _, t := range [...]string{"/tmp/", "/var/tmp/", "/dev/shm/", "/private/tmp/", "appdata/local/temp/"} {
		if bytes.Contains(p, []byte(t)) || bytes.HasPrefix(p, []byte(t[1:])) {
			return true
		}
	}
	base := baseOf(p)
	if bytes.IndexByte(base, '.') <= 0 {
		return true // no extension, or a dotfile
	}
	rest := p[:len(p)-len(base)]
	for len(rest) > 0 {
		i := bytes.IndexByte(rest, '/')
		if i < 0 {
			break
		}
		seg := rest[:i]
		if len(seg) > 1 && seg[0] == '.' && !(len(seg) == 2 && seg[1] == '.') && !bytes.Equal(seg, []byte(".github")) {
			return true
		}
		rest = rest[i+1:]
	}
	return false
}

func (e *engine) detectEntropy(ss *session, ev *Event, in *Input, seq uint64) {
	c := &e.cfg.Entropy
	if len(ev.Payload) < c.MinBytes || e.allowedBlobPath(in.Lower) {
		return
	}
	sample := ev.Payload[:min(len(ev.Payload), c.SampleBytes)]
	class, h := classifyBlob(sample, c)
	if class == blobNone {
		return
	}
	sev := SeverityLow
	if unusualPath(in.Lower, in.Flags) {
		sev = SeverityHigh
	}
	f := ss.emit(DetectorEntropy, blobNames[class], sev, fnv64b(in.Lower), explainEntropy)
	if f == nil {
		return
	}
	f.subject, f.target = in.Target, in.Target
	f.v[0], f.v[1], f.v[2] = h, float64(len(ev.Payload)), float64(len(sample))
	if sev == SeverityHigh {
		f.v[3] = 1
	}
	f.addSeq(seq)
}

func explainEntropy(f *finding) (string, []string, map[string]float64) {
	where := "a source path"
	if f.v[3] == 1 {
		where = "an unusual path"
	}
	return fmt.Sprintf("%s blob (%.2f bits/byte) written to %s", f.rule, f.v[0], path.Base(f.subject)),
		[]string{
			fmt.Sprintf("Shannon entropy %.2f bits/byte over a %.0f-byte sample of %.0f bytes; content looks like %s data written to %s.", f.v[0], f.v[2], f.v[1], f.rule, where),
			"File: " + truncateBytes(f.subject, 200),
			"Opaque encoded blobs can hide payloads or exfiltrated data; check what was written.",
		}, map[string]float64{"entropy_bits": f.v[0], "bytes": f.v[1], "sample_bytes": f.v[2], "unusual_path": f.v[3]}
}

// ------------------------------------------------------------- breaker

func explainBreaker(f *finding) (string, []string, map[string]float64) {
	return fmt.Sprintf("%s: circuit opened after repeated failures", f.subject),
		[]string{
			fmt.Sprintf("Calls to %s are rejected for %s, then probed; it closes after consecutive successful probes.", f.subject, secs(f.v[0])),
		}, map[string]float64{"open_seconds": f.v[0]}
}
