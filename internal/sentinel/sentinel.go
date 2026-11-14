package sentinel

import (
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// engine holds the immutable configuration the detectors read.
type engine struct {
	cfg Config
	nfa *nfa
}

const numShards = 64

type shard struct {
	mu sync.RWMutex
	m  map[string]*session
}

// Sentinel watches event streams and decides what to do about them. It is
// safe for concurrent use; events of different sessions never contend.
type Sentinel struct {
	engine
	clock  Clock
	shards [numShards]shard

	brMu     sync.RWMutex
	breakers map[string]*Breaker

	store incidentStore

	killed     atomic.Bool
	killMu     sync.Mutex
	killReason string
	killWhy    string
	killInc    string
	killAt     time.Time

	critMu sync.Mutex
	crit   []int64

	nEvents, nFindings, nIncidents, nSuppressed, nThrottled, nDenied atomic.Uint64
}

// New builds a Sentinel. Zero config fields take defaults; invalid
// thresholds and patterns are errors.
func New(cfg Config) (*Sentinel, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	var pats []Pattern
	if !cfg.Sequence.NoBuiltins {
		for _, p := range BuiltinPatterns() {
			if !slices.Contains(cfg.Sequence.Disable, p.Name) {
				pats = append(pats, p)
			}
		}
	}
	pats = append(pats, cfg.Sequence.Patterns...)
	m, err := compileNFA(pats, cfg.Sequence.Threads)
	if err != nil {
		return nil, fmt.Errorf("sentinel: %w", err)
	}
	s := &Sentinel{engine: engine{cfg: cfg, nfa: m}, clock: cfg.Clock, breakers: make(map[string]*Breaker)}
	s.store.max = cfg.MaxIncidents
	for i := range s.shards {
		s.shards[i].m = make(map[string]*session)
	}
	return s, nil
}

// Config returns the effective configuration (with defaults applied).
func (s *Sentinel) Config() Config { return s.cfg }

// Patterns returns the names of the active sequence patterns.
func (s *Sentinel) Patterns() []string {
	out := make([]string, len(s.nfa.pats))
	for i := range s.nfa.pats {
		out[i] = s.nfa.pats[i].Name
	}
	return out
}

// coolEntry remembers a recent incident for dedupe.
type coolEntry struct {
	key uint64
	at  int64
	inc *Incident
}

const coolSlots = 32

// session is the per-session detector state. Everything in it is guarded
// by mu.
type session struct {
	mu       sync.Mutex
	id       string
	created  int64
	last     int64
	seq      uint64
	ring     []record
	mask     uint64
	lower    []byte
	in       Input
	loop     loopState
	osc      []fileHist
	rate     [NumMetrics]rateSeries
	burn     burnState
	nfa      *nfaState
	del      deletionState
	errs     errorState
	findings []finding
	cool     [coolSlots]coolEntry

	posture     Action
	postureAt   int64 // last escalation or refresh
	postureSev  Severity
	postureDet  string
	postureInc  string
	postureWhy  string // full reason for verdicts, built when escalating
	decayPeriod time.Duration
	lastDecayAt int64
	bucket      tokenBucket
}

func (s *Sentinel) newSession(id string, now int64) *session {
	c := &s.cfg
	ss := &session{
		id:          id,
		created:     now,
		ring:        make([]record, c.EventWindow),
		mask:        uint64(c.EventWindow - 1),
		lower:       make([]byte, 0, 256),
		osc:         make([]fileHist, c.Oscillation.Files),
		nfa:         s.nfa.newState(),
		findings:    make([]finding, 0, maxFindings),
		decayPeriod: c.Response.Decay,
	}
	ss.burn.budget = [2]float64{float64(c.Burn.TokenBudget), c.Burn.CostBudget}
	for i := range ss.burn.ring {
		ss.burn.ring[i].size = c.Burn.Window
	}
	for m := range ss.rate {
		ss.rate[m].ewm.Alpha = c.Rate.Alpha
		ss.rate[m].holt.Alpha, ss.rate[m].holt.Beta = c.Rate.Alpha, c.Rate.Beta
		ss.rate[m].holt.Res.Alpha = c.Rate.Alpha
	}
	return ss
}

func (s *Sentinel) shardFor(id string) *shard { return &s.shards[fnv64(id)%numShards] }

// session returns the state for id, creating it on first use.
func (s *Sentinel) session(id string, now int64) *session {
	sh := s.shardFor(id)
	sh.mu.RLock()
	ss := sh.m[id]
	sh.mu.RUnlock()
	if ss != nil {
		return ss
	}
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if ss = sh.m[id]; ss == nil {
		ss = s.newSession(id, now)
		sh.m[id] = ss
	}
	return ss
}

func (s *Sentinel) lookup(id string) *session {
	sh := s.shardFor(id)
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	return sh.m[id]
}

// Observe feeds one event to the detectors and returns the verdict. For
// gated kinds (actions and LLM requests) the verdict decides whether the
// event may proceed; for results it reports the session's state. Observe
// does not allocate unless a finding opens a new incident.
func (s *Sentinel) Observe(ev Event) Verdict {
	s.nEvents.Add(1)
	var now int64
	if ev.Time.IsZero() {
		now = s.clock.Now().UnixNano()
	} else {
		now = ev.Time.UnixNano()
	}
	ss := s.session(ev.Session, now)

	var fresh [maxFindings]*Incident
	nfresh := 0
	var crit [maxFindings]string
	ncrit := 0

	ss.mu.Lock()
	if now < ss.last {
		now = ss.last // keep each session's clock monotonic
	}
	ss.last = now
	seq := ss.push(&ev, now)
	ss.findings = ss.findings[:0]
	ss.lower = lowerInto(ss.lower, ev.Target)
	ss.in.reset(ev.Kind, ev.Tool, ev.Target, ev.Flags, ss.lower)
	in := &ss.in

	s.detect(ss, &ev, in, now, seq)

	gated := ev.Kind.gated()
	if gated {
		s.decayPosture(ss, now)
	}
	var v Verdict
	if len(ss.findings) > 0 {
		s.nFindings.Add(uint64(len(ss.findings)))
	}
	for i := range ss.findings {
		f := &ss.findings[i]
		action := f.action
		if action == ActionNone {
			action = s.cfg.Response.Actions[f.sev]
		}
		inc, isNew := s.incidentFor(ss, f, action, now, seq)
		if isNew {
			fresh[nfresh] = inc
			nfresh++
			if f.sev == SeverityCritical {
				crit[ncrit] = inc.ID
				ncrit++
			}
		}
		if gated {
			v = v.Stricter(Verdict{Decision: decisionFor(action), Severity: f.sev, Reason: inc.Summary, Detector: f.detector, Incident: inc.ID})
		}
		s.escalate(ss, action, f, inc, now)
	}
	if gated {
		v = s.postureVerdict(ss, v, now)
		if ev.Kind.IsAction() && ev.Tool != "" && v.MayRun() && !s.cfg.Breaker.Disabled {
			if ok, wait := s.breaker(ev.Tool).Allow(); !ok {
				v = v.Stricter(Verdict{Decision: DecisionDeny, Severity: SeverityMedium, Reason: "circuit open: " + ev.Tool + " keeps failing", Detector: DetectorBreaker, RetryAfter: wait})
			}
		}
	}
	ss.mu.Unlock()

	switch v.Decision {
	case DecisionThrottle:
		s.nThrottled.Add(1)
	case DecisionDeny, DecisionKill:
		s.nDenied.Add(1)
	}
	if gated && s.killed.Load() {
		v = v.Stricter(s.killVerdict())
	}
	for i := 0; i < nfresh; i++ {
		if s.cfg.OnIncident != nil {
			snap, _ := s.store.get(fresh[i].ID)
			s.cfg.OnIncident(snap)
		}
	}
	for i := 0; i < ncrit; i++ {
		if s.noteCritical(now) {
			s.Kill(fmt.Sprintf("%d critical incidents within %s", s.cfg.Response.KillAfter, s.cfg.Response.KillWindow), crit[i])
			if gated {
				v = v.Stricter(s.killVerdict())
			}
		}
	}
	return v
}

// Recheck re-evaluates the gate for an event that Observe already saw
// (the kill switch, the session posture and the tool's circuit breaker)
// without recording it or running detectors. Use it to retry a throttled
// event.
func (s *Sentinel) Recheck(ev Event) Verdict {
	if s.killed.Load() {
		return s.killVerdict()
	}
	var v Verdict
	if ss := s.lookup(ev.Session); ss != nil {
		now := s.clock.Now().UnixNano()
		if !ev.Time.IsZero() {
			now = ev.Time.UnixNano()
		}
		ss.mu.Lock()
		now = max(now, ss.last)
		s.decayPosture(ss, now)
		v = s.postureVerdict(ss, v, now)
		ss.mu.Unlock()
	}
	if ev.Kind.IsAction() && ev.Tool != "" && v.MayRun() && !s.cfg.Breaker.Disabled {
		if ok, wait := s.breaker(ev.Tool).Allow(); !ok {
			v = v.Stricter(Verdict{Decision: DecisionDeny, Severity: SeverityMedium, Reason: "circuit open: " + ev.Tool + " keeps failing", Detector: DetectorBreaker, RetryAfter: wait})
		}
	}
	return v
}

// push records ev in the session ring and returns its sequence number.
func (ss *session) push(ev *Event, now int64) uint64 {
	ss.seq++
	r := &ss.ring[ss.seq&ss.mask]
	*r = record{seq: ss.seq, at: now, kind: ev.Kind, flags: ev.Flags, tool: ev.Tool, target: ev.Target,
		bytes: ev.Bytes, tokens: ev.Tokens, cost: ev.Cost, err: ev.Err}
	return ss.seq
}

// detect runs the detectors that apply to the event's kind.
func (s *Sentinel) detect(ss *session, ev *Event, in *Input, now int64, seq uint64) {
	c := &s.cfg
	action := ev.Kind.IsAction()
	if action && !c.Loop.Disabled {
		s.detectLoop(ss, in, now, seq)
	}
	if !c.Rate.Disabled {
		s.detectRate(ss, ev, now, seq)
	}
	if action && !c.Sequence.Disabled && len(s.nfa.pats) > 0 {
		s.detectSequence(ss, in, now, seq)
	}
	if !c.Deletion.Disabled && (ev.Kind == KindFileDelete || ev.Kind == KindExec || ev.Kind == KindToolCall) {
		s.detectDeletion(ss, in, now, seq)
	}
	if ev.Kind == KindFileWrite || ev.Kind == KindToolResult && ev.Digest != 0 {
		digest := ev.Digest
		if digest == 0 && len(ev.Payload) > 0 {
			digest = fnv64b(ev.Payload)
		}
		if digest != 0 && !c.Oscillation.Disabled && ev.Target != "" {
			s.detectOscillation(ss, in, digest, now, seq)
		}
		if ev.Kind == KindFileWrite && !c.Entropy.Disabled {
			s.detectEntropy(ss, ev, in, seq)
		}
	}
	if ev.Kind == KindLLMResponse && !c.Burn.Disabled {
		s.detectBurn(ss, ev, now, seq)
	}
	if ev.Kind.isResult() && !c.Errors.Disabled {
		s.detectErrors(ss, ev, now, seq)
	}
	if ev.Kind == KindToolResult && ev.Tool != "" && !c.Breaker.Disabled {
		if s.breaker(ev.Tool).Record(ev.Err == nil) {
			if f := ss.emit(DetectorBreaker, "open", SeverityMedium, fnv64(ev.Tool), explainBreaker); f != nil {
				f.subject, f.target, f.action = ev.Tool, ev.Target, ActionLog
				f.v[0] = s.breaker(ev.Tool).Status().OpenFor.Seconds()
				f.addSeq(seq)
			}
		}
	}
}

// incidentFor returns the incident for a finding, creating one unless the
// same finding fired within the cooldown.
func (s *Sentinel) incidentFor(ss *session, f *finding, action Action, now int64, seq uint64) (*Incident, bool) {
	oldest := 0
	for i := range ss.cool {
		e := &ss.cool[i]
		if e.inc != nil && e.key == f.key && now-e.at < int64(s.cfg.Cooldown) {
			s.store.bump(e.inc, time.Unix(0, now).UTC())
			s.nSuppressed.Add(1)
			return e.inc, false
		}
		if e.at < ss.cool[oldest].at {
			oldest = i
		}
	}
	summary, details, metrics := f.explain(f)
	at := time.Unix(0, now).UTC()
	inc := &Incident{
		ID: incidentID(ss.id, f, seq), Session: ss.id, Detector: f.detector, Rule: f.rule,
		Severity: f.sev, Action: action, Summary: summary, Details: details, Metrics: metrics,
		Subject: truncateBytes(f.subject, s.cfg.MaxTargetBytes), FirstSeen: at, LastSeen: at, Count: 1,
		Evidence: s.evidence(ss, f),
	}
	if s.cfg.Redact != nil {
		inc.Subject = s.cfg.Redact(inc.Subject)
	}
	s.store.add(inc)
	s.nIncidents.Add(1)
	ss.cool[oldest] = coolEntry{key: f.key, at: now, inc: inc}
	return inc, true
}

// evidence copies the trailing event window plus any older matched events
// still in the ring, oldest first.
func (s *Sentinel) evidence(ss *session, f *finding) []EventRecord {
	n := min(uint64(s.cfg.EvidenceSize), ss.seq)
	from := ss.seq - n + 1
	var seqs []uint64
	for i := 0; i < f.nseq; i++ {
		if q := f.seqs[i]; q < from && q > 0 && ss.seq-q <= ss.mask {
			seqs = append(seqs, q)
		}
	}
	for q := from; q <= ss.seq; q++ {
		seqs = append(seqs, q)
	}
	slices.Sort(seqs)
	seqs = slices.Compact(seqs)
	out := make([]EventRecord, 0, len(seqs))
	for _, q := range seqs {
		r := &ss.ring[q&ss.mask]
		if r.seq != q {
			continue
		}
		er := r.export(s.cfg.Redact, s.cfg.MaxTargetBytes)
		for i := 0; i < f.nseq; i++ {
			if f.seqs[i] == q {
				er.Matched = true
				break
			}
		}
		out = append(out, er)
	}
	return out
}

// isSticky reports whether an action becomes a session posture.
func (s *Sentinel) isSticky(a Action) bool {
	return a == ActionThrottle || a == ActionPause || a == ActionRequireApproval && s.cfg.Response.StickyApproval
}

// escalate raises the session posture for a finding's action.
func (s *Sentinel) escalate(ss *session, a Action, f *finding, inc *Incident, now int64) {
	if !s.isSticky(a) || a < ss.posture {
		return
	}
	if a > ss.posture {
		// Hysteresis: re-escalating soon after a decay means the decay was
		// premature, so hold the next posture for longer.
		if ss.lastDecayAt != 0 && now-ss.lastDecayAt < int64(ss.decayPeriod) {
			ss.decayPeriod = min(2*ss.decayPeriod, s.cfg.Response.MaxDecay)
		}
		if a == ActionThrottle {
			ss.bucket.reset(now, s.cfg.Response.ThrottleBurst)
		}
		ss.posture, ss.postureSev, ss.postureDet, ss.postureInc = a, f.sev, f.detector, inc.ID
		switch a {
		case ActionPause:
			ss.postureWhy = "session paused: " + inc.Summary
		case ActionRequireApproval:
			ss.postureWhy = "heightened scrutiny: " + inc.Summary
		default:
			ss.postureWhy = "throttled: " + inc.Summary
		}
	}
	ss.postureAt = now
}

// decayPosture lowers a throttle or sticky-approval posture one level after
// a quiet period. Pause never decays; it needs Resume.
func (s *Sentinel) decayPosture(ss *session, now int64) {
	switch {
	case ss.posture == ActionThrottle || ss.posture == ActionRequireApproval:
		if now-ss.postureAt < int64(ss.decayPeriod) {
			return
		}
		if ss.posture == ActionRequireApproval {
			ss.posture = ActionThrottle
			ss.postureWhy = "throttled (decaying): " + ss.postureWhy
			ss.bucket.reset(now, s.cfg.Response.ThrottleBurst)
			ss.postureAt = now
		} else {
			ss.posture = ActionNone
			ss.postureWhy = ""
		}
		ss.lastDecayAt = now
	case ss.posture == ActionNone && ss.lastDecayAt != 0 && now-ss.lastDecayAt > 4*int64(s.cfg.Response.MaxDecay):
		ss.decayPeriod = s.cfg.Response.Decay // long quiet: forgive past flapping
		ss.lastDecayAt = 0
	}
}

// postureVerdict applies the session posture to a gated event.
func (s *Sentinel) postureVerdict(ss *session, v Verdict, now int64) Verdict {
	switch ss.posture {
	case ActionPause, ActionKill:
		return v.Stricter(Verdict{Decision: DecisionDeny, Severity: ss.postureSev, Reason: ss.postureWhy, Detector: ss.postureDet, Incident: ss.postureInc})
	case ActionRequireApproval:
		return v.Stricter(Verdict{Decision: DecisionAsk, Severity: ss.postureSev, Reason: ss.postureWhy, Detector: ss.postureDet, Incident: ss.postureInc})
	case ActionThrottle:
		if v.Decision >= DecisionAsk {
			return v // a human sets the pace
		}
		if ok, wait := ss.bucket.take(now, s.cfg.Response.ThrottleRate, s.cfg.Response.ThrottleBurst); !ok {
			return v.Stricter(Verdict{Decision: DecisionThrottle, Severity: ss.postureSev, Reason: ss.postureWhy, Detector: ss.postureDet, Incident: ss.postureInc, RetryAfter: wait})
		}
		if v.Decision == DecisionAllow {
			v = Verdict{Decision: DecisionWarn, Severity: ss.postureSev, Reason: ss.postureWhy, Detector: ss.postureDet, Incident: ss.postureInc}
		}
	}
	return v
}

// noteCritical records a critical incident and reports whether the kill
// threshold has been reached.
func (s *Sentinel) noteCritical(now int64) bool {
	k := s.cfg.Response.KillAfter
	if k < 0 {
		return false
	}
	s.critMu.Lock()
	defer s.critMu.Unlock()
	cut := now - int64(s.cfg.Response.KillWindow)
	live := s.crit[:0]
	for _, t := range s.crit {
		if t >= cut {
			live = append(live, t)
		}
	}
	s.crit = append(live, now)
	return len(s.crit) >= k
}

func (s *Sentinel) killVerdict() Verdict {
	s.killMu.Lock()
	defer s.killMu.Unlock()
	return Verdict{Decision: DecisionKill, Severity: SeverityCritical, Reason: s.killWhy, Detector: "kill", Incident: s.killInc}
}

// Kill engages the global kill switch: every gated event in every session
// is refused until ClearKill. incident may be empty.
func (s *Sentinel) Kill(reason, incident string) {
	s.killMu.Lock()
	if !s.killed.Load() {
		s.killReason, s.killInc, s.killAt = reason, incident, s.clock.Now().UTC()
		s.killWhy = "kill switch engaged: " + reason
	}
	s.killed.Store(true)
	s.killMu.Unlock()
}

// ClearKill releases the kill switch.
func (s *Sentinel) ClearKill() {
	s.killMu.Lock()
	s.killed.Store(false)
	s.killReason, s.killWhy, s.killInc, s.killAt = "", "", "", time.Time{}
	s.killMu.Unlock()
	s.critMu.Lock()
	s.crit = s.crit[:0]
	s.critMu.Unlock()
}

// Killed reports whether the kill switch is engaged.
func (s *Sentinel) Killed() bool { return s.killed.Load() }

// KillState returns the kill switch state, reason and engagement time.
func (s *Sentinel) KillState() (bool, string, time.Time) {
	s.killMu.Lock()
	defer s.killMu.Unlock()
	return s.killed.Load(), s.killReason, s.killAt
}

// Posture describes a session's standing response.
type Posture struct {
	Action   Action    `json:"action"`
	Severity Severity  `json:"severity,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Detector string    `json:"detector,omitempty"`
	Incident string    `json:"incident,omitempty"`
	Since    time.Time `json:"since,omitzero"`
	// DecayIn is how long until the posture lowers without new findings;
	// zero for none and for pause, which needs Resume.
	DecayIn time.Duration `json:"decay_in,omitempty"`
}

// Posture returns a session's posture.
func (s *Sentinel) Posture(session string) Posture {
	ss := s.lookup(session)
	if ss == nil {
		return Posture{}
	}
	now := s.clock.Now().UnixNano()
	ss.mu.Lock()
	defer ss.mu.Unlock()
	p := Posture{Action: ss.posture, Severity: ss.postureSev, Reason: ss.postureWhy, Detector: ss.postureDet, Incident: ss.postureInc}
	if ss.posture == ActionNone {
		return Posture{}
	}
	p.Since = time.Unix(0, ss.postureAt).UTC()
	if ss.posture == ActionThrottle || ss.posture == ActionRequireApproval {
		p.DecayIn = max(time.Duration(ss.postureAt+int64(ss.decayPeriod)-now), 0)
	}
	return p
}

// Resume clears a session's posture (including pause).
func (s *Sentinel) Resume(session string) {
	ss := s.lookup(session)
	if ss == nil {
		return
	}
	now := s.clock.Now().UnixNano()
	ss.mu.Lock()
	if ss.posture != ActionNone {
		ss.lastDecayAt = max(now, ss.last)
	}
	ss.posture, ss.postureSev, ss.postureDet, ss.postureInc, ss.postureWhy = ActionNone, SeverityNone, "", "", ""
	ss.mu.Unlock()
}

// SetBudget sets a session's token and USD budgets for burn forecasting;
// zero disables that forecast.
func (s *Sentinel) SetBudget(session string, tokens int64, costUSD float64) {
	ss := s.session(session, s.clock.Now().UnixNano())
	ss.mu.Lock()
	ss.burn.budget = [2]float64{float64(tokens), costUSD}
	ss.mu.Unlock()
}

// Forecast returns the session's token and cost burn forecasts.
func (s *Sentinel) Forecast(session string) []BurnForecast {
	ss := s.lookup(session)
	if ss == nil {
		return nil
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return []BurnForecast{s.forecast(&ss.burn, 0), s.forecast(&ss.burn, 1)}
}

// EndSession drops a session's state. Its incidents are kept.
func (s *Sentinel) EndSession(session string) {
	sh := s.shardFor(session)
	sh.mu.Lock()
	delete(sh.m, session)
	sh.mu.Unlock()
}

// Sweep drops sessions with no events for idle and returns how many.
func (s *Sentinel) Sweep(idle time.Duration) int {
	cut := s.clock.Now().UnixNano() - int64(idle)
	n := 0
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.Lock()
		for id, ss := range sh.m {
			ss.mu.Lock()
			stale := ss.last < cut
			ss.mu.Unlock()
			if stale {
				delete(sh.m, id)
				n++
			}
		}
		sh.mu.Unlock()
	}
	return n
}

// breaker returns the breaker for a tool, creating it on first use.
func (s *Sentinel) breaker(tool string) *Breaker {
	s.brMu.RLock()
	b := s.breakers[tool]
	s.brMu.RUnlock()
	if b != nil {
		return b
	}
	s.brMu.Lock()
	defer s.brMu.Unlock()
	if b = s.breakers[tool]; b == nil {
		b = NewBreaker(s.cfg.Breaker, s.clock)
		s.breakers[tool] = b
	}
	return b
}

// Breaker returns the circuit breaker for tool.
func (s *Sentinel) Breaker(tool string) *Breaker { return s.breaker(tool) }

// Breakers returns a snapshot of every breaker that has left the closed
// state at least once, sorted by tool.
func (s *Sentinel) Breakers() []BreakerStatus {
	s.brMu.RLock()
	defer s.brMu.RUnlock()
	var out []BreakerStatus
	for tool, b := range s.breakers {
		st := b.Status()
		if st.Trips == 0 && st.State == BreakerClosed {
			continue
		}
		st.Tool = tool
		out = append(out, st)
	}
	slices.SortFunc(out, func(a, b BreakerStatus) int {
		switch {
		case a.Tool < b.Tool:
			return -1
		case a.Tool > b.Tool:
			return 1
		}
		return 0
	})
	return out
}

// release frees a breaker probe reserved for an event that will not run.
func (s *Sentinel) release(ev *Event) {
	if ev.Kind.IsAction() && ev.Tool != "" && !s.cfg.Breaker.Disabled {
		s.breaker(ev.Tool).Release()
	}
}

// Incidents returns all stored incidents, oldest first.
func (s *Sentinel) Incidents() []Incident { return s.store.snapshot(nil) }

// SessionIncidents returns a session's incidents, oldest first.
func (s *Sentinel) SessionIncidents(session string) []Incident {
	return s.store.snapshot(func(i *Incident) bool { return i.Session == session })
}

// Incident returns an incident by ID.
func (s *Sentinel) Incident(id string) (Incident, bool) { return s.store.get(id) }

// ClearIncidents empties the incident store.
func (s *Sentinel) ClearIncidents() {
	s.store.clear()
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.RLock()
		for _, ss := range sh.m {
			ss.mu.Lock()
			ss.cool = [coolSlots]coolEntry{}
			ss.mu.Unlock()
		}
		sh.mu.RUnlock()
	}
}

// Stats are counters since New.
type Stats struct {
	Events     uint64 `json:"events"`
	Findings   uint64 `json:"findings"`
	Incidents  uint64 `json:"incidents"`
	Suppressed uint64 `json:"suppressed"`
	Throttled  uint64 `json:"throttled"`
	Denied     uint64 `json:"denied"`
	Sessions   int    `json:"sessions"`
}

// Stats returns the counters.
func (s *Sentinel) Stats() Stats {
	st := Stats{
		Events: s.nEvents.Load(), Findings: s.nFindings.Load(), Incidents: s.nIncidents.Load(),
		Suppressed: s.nSuppressed.Load(), Throttled: s.nThrottled.Load(), Denied: s.nDenied.Load(),
	}
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.RLock()
		st.Sessions += len(sh.m)
		sh.mu.RUnlock()
	}
	return st
}

// tokenBucket is a classic token bucket, guarded by the session mutex.
type tokenBucket struct {
	tokens float64
	last   int64
}

func (b *tokenBucket) reset(now int64, burst float64) {
	b.tokens, b.last = burst, now
}

func (b *tokenBucket) take(now int64, rate, burst float64) (bool, time.Duration) {
	if now > b.last {
		b.tokens = min(burst, b.tokens+float64(now-b.last)/1e9*rate)
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / rate * 1e9)
}
