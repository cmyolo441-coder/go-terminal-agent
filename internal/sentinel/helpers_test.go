package sentinel

import (
	"errors"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// harness drives a Sentinel with a manual clock.
type harness struct {
	t     testing.TB
	s     *Sentinel
	clock *ManualClock
	sess  string
}

func newHarness(t testing.TB, mutate func(*Config)) *harness {
	t.Helper()
	clk := NewManualClock(t0)
	cfg := Config{Clock: clk}
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &harness{t: t, s: s, clock: clk, sess: "s1"}
}

// only returns a mutator that disables every detector except the named ones.
func only(names ...string) func(*Config) {
	return func(c *Config) {
		on := map[string]bool{}
		for _, n := range names {
			on[n] = true
		}
		c.Loop.Disabled = !on[DetectorLoop]
		c.Oscillation.Disabled = !on[DetectorOscillation]
		c.Rate.Disabled = !on[DetectorRate]
		c.Burn.Disabled = !on[DetectorBurn]
		c.Sequence.Disabled = !on[DetectorSequence]
		c.Deletion.Disabled = !on[DetectorDeletion]
		c.Errors.Disabled = !on[DetectorErrors]
		c.Entropy.Disabled = !on[DetectorEntropy]
		c.Breaker.Disabled = !on[DetectorBreaker]
	}
}

func chain(fs ...func(*Config)) func(*Config) {
	return func(c *Config) {
		for _, f := range fs {
			if f != nil {
				f(c)
			}
		}
	}
}

func (h *harness) at(d time.Duration) time.Time { return h.clock.Advance(d) }

func (h *harness) obs(ev Event) Verdict {
	if ev.Session == "" {
		ev.Session = h.sess
	}
	if ev.Time.IsZero() {
		ev.Time = h.clock.Now()
	}
	return h.s.Observe(ev)
}

func (h *harness) exec(cmd string) Verdict {
	return h.obs(Event{Kind: KindExec, Tool: "bash", Target: cmd})
}

func (h *harness) write(path string, payload []byte) Verdict {
	return h.obs(Event{Kind: KindFileWrite, Tool: "write", Target: path, Payload: payload, Bytes: int64(len(payload))})
}

func (h *harness) read(path string) Verdict {
	return h.obs(Event{Kind: KindFileRead, Tool: "read", Target: path})
}

func (h *harness) result(tool string, err error) Verdict {
	return h.obs(Event{Kind: KindToolResult, Tool: tool, Err: err})
}

// incidents returns the incidents of one detector (all when det is "").
func (h *harness) incidents(det string) []Incident {
	var out []Incident
	for _, inc := range h.s.Incidents() {
		if det == "" || inc.Detector == det {
			out = append(out, inc)
		}
	}
	return out
}

func (h *harness) wantIncidents(det string, n int) []Incident {
	h.t.Helper()
	got := h.incidents(det)
	if len(got) != n {
		for _, i := range h.s.Incidents() {
			h.t.Logf("incident: %s/%s %s %q", i.Detector, i.Rule, i.Severity, i.Summary)
		}
		h.t.Fatalf("%s incidents = %d, want %d", det, len(got), n)
	}
	return got
}

var errBoom = errors.New("boom")
