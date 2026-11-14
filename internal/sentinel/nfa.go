package sentinel

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// MaxSteps bounds the length of a Pattern.
const MaxSteps = 8

// defaultThreads bounds the partial matches kept per pattern per session.
const defaultThreads = 16

// Step is one element of a Pattern.
type Step struct {
	// Name labels the step in explanations.
	Name string
	// Kinds restricts the event kinds the step matches; zero means
	// ActionKinds.
	Kinds KindSet
	// Tool, when set, must equal the event's tool (case-insensitive).
	Tool string
	// Match, when set, must match the event.
	Match Matcher
	// Capture, when set, extracts a value that later steps can Bind to. A
	// step whose Capture fails does not match.
	Capture Capturer
	// Bind requires the event's target to contain the value captured by the
	// most recent earlier capturing step (case-insensitive).
	Bind bool
}

// Pattern is an ordered sequence of steps that must be observed, in order
// but not necessarily adjacently, within Within of the first step.
type Pattern struct {
	Name        string
	Description string
	Severity    Severity
	// Action overrides the severity's default action when set.
	Action Action
	// Within bounds the time from the first matched event to the last.
	Within time.Duration
	Steps  []Step
}

func (p *Pattern) validate() error {
	if strings.TrimSpace(p.Name) == "" {
		return errors.New("pattern has no name")
	}
	if len(p.Steps) == 0 || len(p.Steps) > MaxSteps {
		return fmt.Errorf("pattern %q: need 1..%d steps, have %d", p.Name, MaxSteps, len(p.Steps))
	}
	if p.Within <= 0 {
		return fmt.Errorf("pattern %q: Within must be positive", p.Name)
	}
	if p.Severity == SeverityNone || p.Severity >= numSeverities {
		return fmt.Errorf("pattern %q: invalid severity", p.Name)
	}
	if p.Action >= numActions {
		return fmt.Errorf("pattern %q: invalid action", p.Name)
	}
	captured := false
	for i, s := range p.Steps {
		if s.Bind && !captured {
			return fmt.Errorf("pattern %q step %d: Bind without an earlier Capture", p.Name, i)
		}
		if s.Capture != nil {
			captured = true
		}
	}
	return nil
}

// thread is one partial match: the next step to match, when the first step
// matched, the bound capture and the sequence numbers of matched events.
type thread struct {
	pos     uint8
	start   int64
	capture string
	seqs    [MaxSteps]uint64
}

// nfa runs a set of patterns over an event stream. It is immutable and
// shared; per-session progress lives in nfaState.
//
// The automaton keeps, per pattern, a bounded set of threads. Because a
// thread that started later can do everything an earlier one at the same
// position and with the same capture can (it has more time left), only the
// latest-starting thread per (position, capture) is kept. Without captures
// this makes the set exact with at most len(Steps) threads; with captures
// the set is bounded by evicting the earliest start.
type nfa struct {
	pats    []Pattern
	threads int
}

func compileNFA(pats []Pattern, threads int) (*nfa, error) {
	if threads <= 0 {
		threads = defaultThreads
	}
	seen := make(map[string]bool, len(pats))
	for i := range pats {
		if err := pats[i].validate(); err != nil {
			return nil, err
		}
		if seen[pats[i].Name] {
			return nil, fmt.Errorf("duplicate pattern name %q", pats[i].Name)
		}
		seen[pats[i].Name] = true
	}
	return &nfa{pats: pats, threads: threads}, nil
}

type patState struct {
	th []thread
	n  int
}

type nfaMatch struct {
	pat int
	th  thread
}

type nfaState struct {
	pats    []patState
	scratch []thread
	matches []nfaMatch
}

func (m *nfa) newState() *nfaState {
	st := &nfaState{
		pats:    make([]patState, len(m.pats)),
		scratch: make([]thread, m.threads+1),
		matches: make([]nfaMatch, 0, len(m.pats)),
	}
	backing := make([]thread, len(m.pats)*m.threads)
	for i := range st.pats {
		st.pats[i].th = backing[i*m.threads : (i+1)*m.threads : (i+1)*m.threads]
	}
	return st
}

func stepMatches(s *Step, in *Input, capture string) bool {
	kinds := s.Kinds
	if kinds == 0 {
		kinds = ActionKinds
	}
	if !kinds.Has(in.Kind) {
		return false
	}
	if s.Tool != "" && !strings.EqualFold(s.Tool, in.Tool) {
		return false
	}
	if s.Bind && !containsFold(in.Lower, capture) {
		return false
	}
	return s.Match == nil || s.Match.Match(in)
}

// advance tries to move t past its current step on in. It returns the new
// thread and whether the step matched.
func advance(p *Pattern, t thread, in *Input, seq uint64) (thread, bool) {
	s := &p.Steps[t.pos]
	if !stepMatches(s, in, t.capture) {
		return t, false
	}
	if s.Capture != nil {
		c, ok := s.Capture(in)
		if !ok {
			return t, false
		}
		t.capture = c
	}
	t.seqs[t.pos] = seq
	t.pos++
	return t, true
}

// step feeds one event to every pattern and returns the patterns that
// completed. The returned slice is reused by the next call.
func (m *nfa) step(st *nfaState, in *Input, now int64, seq uint64) []nfaMatch {
	st.matches = st.matches[:0]
	for pi := range m.pats {
		p := &m.pats[pi]
		ps := &st.pats[pi]
		cut := now - int64(p.Within)

		// Expire threads whose first step is too old.
		live := 0
		for i := 0; i < ps.n; i++ {
			if ps.th[i].start >= cut {
				ps.th[live] = ps.th[i]
				live++
			}
		}
		ps.n = live

		// Advance existing threads; this event can extend each by one step.
		k := 0
		var done thread
		matched := false
		for i := 0; i < ps.n; i++ {
			nt, ok := advance(p, ps.th[i], in, seq)
			if !ok {
				continue
			}
			if int(nt.pos) == len(p.Steps) {
				if !matched || nt.start > done.start {
					done = nt
				}
				matched = true
				continue
			}
			if k < len(st.scratch) {
				st.scratch[k] = nt
				k++
			}
		}
		// Start a new thread when the first step matches.
		if nt, ok := advance(p, thread{start: now}, in, seq); ok {
			if int(nt.pos) == len(p.Steps) {
				if !matched || nt.start >= done.start {
					done = nt
				}
				matched = true
			} else if k < len(st.scratch) {
				st.scratch[k] = nt
				k++
			}
		}
		if matched {
			// Report once and start over, so one episode is one finding.
			ps.n = 0
			st.matches = append(st.matches, nfaMatch{pat: pi, th: done})
			continue
		}
		for i := 0; i < k; i++ {
			ps.merge(st.scratch[i])
		}
	}
	return st.matches
}

// merge inserts t, keeping only the latest start per (pos, capture) and
// evicting the earliest start when full.
func (ps *patState) merge(t thread) {
	for i := 0; i < ps.n; i++ {
		e := &ps.th[i]
		if e.pos == t.pos && e.capture == t.capture {
			if t.start >= e.start {
				*e = t
			}
			return
		}
	}
	if ps.n < len(ps.th) {
		ps.th[ps.n] = t
		ps.n++
		return
	}
	oldest := 0
	for i := 1; i < ps.n; i++ {
		if ps.th[i].start < ps.th[oldest].start {
			oldest = i
		}
	}
	if t.start > ps.th[oldest].start {
		ps.th[oldest] = t
	}
}

// pending returns the number of live partial matches, for tests.
func (st *nfaState) pending() int {
	n := 0
	for i := range st.pats {
		n += st.pats[i].n
	}
	return n
}
