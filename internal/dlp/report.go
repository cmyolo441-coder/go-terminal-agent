package dlp

import (
	"math"
	"sort"
	"sync"
)

// Report summarises findings for audit. It holds counts and labels only,
// never values, so it can be logged or shipped to an audit sink as is.
type Report struct {
	Policy    string `json:"policy,omitempty"`
	Bytes     int64  `json:"bytes"`
	Documents int    `json:"documents"`
	Findings  int    `json:"findings"`
	// Blocked counts blocked documents.
	Blocked  int              `json:"blocked"`
	ByType   map[InfoType]int `json:"by_type,omitempty"`
	ByLabel  map[Label]int    `json:"by_label,omitempty"`
	ByAction map[Action]int   `json:"by_action,omitempty"`
	// Frameworks is the sorted union of the findings' frameworks.
	Frameworks []Framework `json:"frameworks,omitempty"`
	// Label is the most sensitive finding label (Public when none).
	Label Label `json:"label"`
	// Score is the document sensitivity score in [0, 100]; for merged
	// reports it is the highest document score.
	Score float64 `json:"score"`
}

// documentScore turns findings into a saturating 0-100 score: each
// finding adds its label weight times its confidence, and the sum s maps to
// 100·(1 − e^(−s/10)). One certain Restricted value scores 63, three score
// 95; one Confidential value scores 26.
func documentScore(fs []Finding) float64 {
	s := 0.0
	for _, f := range fs {
		s += labelWeight(f.Label) * f.Confidence
	}
	return math.Round(1000*(1-math.Exp(-s/10))) / 10
}

func buildReport(policy string, n int64, fs []Finding) Report {
	r := Report{Policy: policy, Bytes: n, Documents: 1, Findings: len(fs), Label: Public}
	if len(fs) == 0 {
		return r
	}
	r.ByType = map[InfoType]int{}
	r.ByLabel = map[Label]int{}
	r.ByAction = map[Action]int{}
	fw := map[Framework]bool{}
	for _, f := range fs {
		r.ByType[f.Type]++
		r.ByLabel[f.Label]++
		r.ByAction[f.Action]++
		if f.Label > r.Label {
			r.Label = f.Label
		}
		for _, x := range f.Frameworks {
			fw[x] = true
		}
	}
	r.Frameworks = sortedFrameworks(fw)
	r.Score = documentScore(fs)
	return r
}

func sortedFrameworks(m map[Framework]bool) []Framework {
	if len(m) == 0 {
		return nil
	}
	out := make([]Framework, 0, len(m))
	for f := range m {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Merge adds o into r: counts are summed, frameworks united, and the label
// and score are the maxima.
func (r *Report) Merge(o Report) {
	if r.Policy == "" {
		r.Policy = o.Policy
	}
	r.Bytes += o.Bytes
	r.Documents += o.Documents
	r.Findings += o.Findings
	r.Blocked += o.Blocked
	if o.Label > r.Label {
		r.Label = o.Label
	}
	if o.Score > r.Score {
		r.Score = o.Score
	}
	if len(o.ByType) > 0 && r.ByType == nil {
		r.ByType = map[InfoType]int{}
	}
	for k, v := range o.ByType {
		r.ByType[k] += v
	}
	if len(o.ByLabel) > 0 && r.ByLabel == nil {
		r.ByLabel = map[Label]int{}
	}
	for k, v := range o.ByLabel {
		r.ByLabel[k] += v
	}
	if len(o.ByAction) > 0 && r.ByAction == nil {
		r.ByAction = map[Action]int{}
	}
	for k, v := range o.ByAction {
		r.ByAction[k] += v
	}
	if len(o.Frameworks) > 0 {
		fw := map[Framework]bool{}
		for _, f := range r.Frameworks {
			fw[f] = true
		}
		for _, f := range o.Frameworks {
			fw[f] = true
		}
		r.Frameworks = sortedFrameworks(fw)
	}
}

// clone returns a deep copy.
func (r Report) clone() Report {
	c := r
	c.ByType, c.ByLabel, c.ByAction = nil, nil, nil
	c.Frameworks = append([]Framework(nil), r.Frameworks...)
	c.Documents, c.Bytes, c.Findings, c.Blocked = 0, 0, 0, 0
	c.Label, c.Score = Public, 0
	c.Merge(r)
	c.Policy = r.Policy
	return c
}

// Auditor accumulates reports per channel (for example "prompt",
// "tool_output", "web") for periodic audit export. It is safe for
// concurrent use.
type Auditor struct {
	mu       sync.Mutex
	channels map[string]*Report
}

// NewAuditor returns an empty Auditor.
func NewAuditor() *Auditor { return &Auditor{channels: map[string]*Report{}} }

// Record adds a report under channel.
func (a *Auditor) Record(channel string, r Report) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cur, ok := a.channels[channel]
	if !ok {
		cur = &Report{Label: Public}
		a.channels[channel] = cur
	}
	cur.Merge(r)
}

// Snapshot returns a copy of every channel's totals and their sum under
// the key "*".
func (a *Auditor) Snapshot() map[string]Report {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]Report, len(a.channels)+1)
	total := Report{Label: Public}
	for k, r := range a.channels {
		out[k] = r.clone()
		total.Merge(*r)
	}
	out["*"] = total
	return out
}

// Reset clears all totals.
func (a *Auditor) Reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.channels = map[string]*Report{}
}
