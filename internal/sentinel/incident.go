package sentinel

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"
)

// Incident is a recorded finding with its evidence.
type Incident struct {
	// ID is stable: the same session, detector, rule, subject, severity and
	// triggering event sequence number always produce the same ID, so
	// replaying a recorded stream reproduces it.
	ID       string   `json:"id"`
	Session  string   `json:"session,omitempty"`
	Detector string   `json:"detector"`
	Rule     string   `json:"rule"`
	Severity Severity `json:"severity"`
	Action   Action   `json:"action"`
	// Summary is one line for humans; Details explain how it was detected.
	Summary string             `json:"summary"`
	Details []string           `json:"details,omitempty"`
	Metrics map[string]float64 `json:"metrics,omitempty"`
	Subject string             `json:"subject,omitempty"`
	// FirstSeen and LastSeen bound the repeats folded into this incident.
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	Count     int       `json:"count"`
	// Evidence is the event window at detection time, oldest first, with
	// the events that matched the rule marked.
	Evidence []EventRecord `json:"evidence,omitempty"`
}

// incidentID derives a stable ID.
func incidentID(session string, f *finding, seq uint64) string {
	h := fnvAdd(fnvOffset, session)
	h = fnvAddU64(h, f.key)
	h = fnvAdd(h, f.subject)
	h = fnvAddU64(h, seq)
	return fmt.Sprintf("snt_%012x", mix64(h)>>16)
}

// incidentStore is a bounded, ordered set of incidents.
type incidentStore struct {
	mu   sync.Mutex
	list []*Incident
	max  int
	byID map[string]*Incident
}

func (s *incidentStore) add(inc *Incident) Incident {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byID == nil {
		s.byID = make(map[string]*Incident)
	}
	if prev, ok := s.byID[inc.ID]; ok {
		// A replayed stream reproduces the ID; fold it in.
		prev.Count += inc.Count
		prev.LastSeen = maxTime(prev.LastSeen, inc.LastSeen)
		return *prev
	}
	if len(s.list) >= s.max {
		drop := len(s.list) - s.max + 1
		for _, d := range s.list[:drop] {
			delete(s.byID, d.ID)
		}
		s.list = slices.Delete(s.list, 0, drop)
	}
	s.list = append(s.list, inc)
	s.byID[inc.ID] = inc
	return *inc
}

func (s *incidentStore) bump(inc *Incident, at time.Time) {
	s.mu.Lock()
	inc.Count++
	inc.LastSeen = maxTime(inc.LastSeen, at)
	s.mu.Unlock()
}

func (s *incidentStore) snapshot(filter func(*Incident) bool) []Incident {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Incident, 0, len(s.list))
	for _, inc := range s.list {
		if filter == nil || filter(inc) {
			out = append(out, *inc)
		}
	}
	return out
}

func (s *incidentStore) get(id string) (Incident, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if inc, ok := s.byID[id]; ok {
		return *inc, true
	}
	return Incident{}, false
}

func (s *incidentStore) clear() {
	s.mu.Lock()
	s.list, s.byID = nil, nil
	s.mu.Unlock()
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// Report is the JSON export document.
type Report struct {
	GeneratedAt time.Time       `json:"generated_at"`
	Killed      bool            `json:"killed"`
	KillReason  string          `json:"kill_reason,omitempty"`
	Stats       Stats           `json:"stats"`
	Breakers    []BreakerStatus `json:"breakers,omitempty"`
	Incidents   []Incident      `json:"incidents"`
}

// WriteJSON writes an indented Report to w.
func (s *Sentinel) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(s.Report())
}

// Report builds the export document.
func (s *Sentinel) Report() Report {
	killed, reason, _ := s.KillState()
	incs := s.Incidents()
	if incs == nil {
		incs = []Incident{}
	}
	return Report{
		GeneratedAt: s.clock.Now().UTC(),
		Killed:      killed,
		KillReason:  reason,
		Stats:       s.Stats(),
		Breakers:    s.Breakers(),
		Incidents:   incs,
	}
}

// ReadReport decodes a document written by WriteJSON.
func ReadReport(r io.Reader) (Report, error) {
	var rep Report
	dec := json.NewDecoder(r)
	if err := dec.Decode(&rep); err != nil {
		return Report{}, fmt.Errorf("sentinel: decode report: %w", err)
	}
	return rep, nil
}
