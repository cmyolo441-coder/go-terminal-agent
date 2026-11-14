package evals

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
)

// CaseStatus classifies how one case changed between two result files.
type CaseStatus string

const (
	CaseUnchanged CaseStatus = "unchanged"
	CaseImproved  CaseStatus = "improved"
	CaseWorse     CaseStatus = "worse"
	CaseAdded     CaseStatus = "added"
	CaseRemoved   CaseStatus = "removed"
)

// CaseDelta compares one case across two results.
type CaseDelta struct {
	ID                string     `json:"id"`
	Status            CaseStatus `json:"status"`
	BaselinePassRate  float64    `json:"baseline_pass_rate"`
	CandidatePassRate float64    `json:"candidate_pass_rate"`
	BaselineScore     float64    `json:"baseline_score"`
	CandidateScore    float64    `json:"candidate_score"`
	ScoreDelta        float64    `json:"score_delta"`
}

// Comparison is a paired regression analysis of a candidate result against
// a baseline.
type Comparison struct {
	Baseline  string  `json:"baseline"`
	Candidate string  `json:"candidate"`
	Alpha     float64 `json:"alpha"`
	// PairedRuns is the number of (case, repetition) pairs present in
	// both results; McNemar runs on them.
	PairedRuns int           `json:"paired_runs"`
	McNemar    McNemarResult `json:"mcnemar"`
	// PairedCases is the number of cases present in both; Wilcoxon runs
	// on their mean-score differences (candidate minus baseline).
	PairedCases       int            `json:"paired_cases"`
	Wilcoxon          WilcoxonResult `json:"wilcoxon"`
	BaselinePassRate  float64        `json:"baseline_pass_rate"`
	CandidatePassRate float64        `json:"candidate_pass_rate"`
	BaselineScore     float64        `json:"baseline_score"`
	CandidateScore    float64        `json:"candidate_score"`
	Cases             []CaseDelta    `json:"cases"`
	// Regressed is true when either test finds a significant change for
	// the worse; Improved likewise for the better.
	Regressed bool   `json:"regressed"`
	Improved  bool   `json:"improved"`
	Verdict   string `json:"verdict"`
}

// Compare pairs runs by case id and repetition index and tests whether the
// candidate differs from the baseline: McNemar's test on paired pass/fail
// outcomes and the Wilcoxon signed-rank test on paired per-case mean
// scores. alpha defaults to 0.05.
func Compare(baseline, candidate *SuiteResult, alpha float64) (*Comparison, error) {
	if baseline == nil || candidate == nil {
		return nil, errors.New("evals: compare needs two results")
	}
	if alpha <= 0 || alpha >= 1 {
		alpha = 0.05
	}
	cmp := &Comparison{
		Baseline:  resultLabel(baseline),
		Candidate: resultLabel(candidate),
		Alpha:     alpha,
	}
	base := indexCases(baseline)
	cand := indexCases(candidate)

	ids := map[string]bool{}
	for id := range base {
		ids[id] = true
	}
	for id := range cand {
		ids[id] = true
	}
	var b, c int
	var diffs, baseScores, candScores []float64
	var basePass, baseRuns, candPass, candRuns int
	for _, id := range sortedKeys(ids) {
		bc, inB := base[id]
		cc, inC := cand[id]
		d := CaseDelta{ID: id}
		switch {
		case inB && !inC:
			d.Status = CaseRemoved
			d.BaselinePassRate, d.BaselineScore = bc.PassRate, bc.MeanScore
			cmp.Cases = append(cmp.Cases, d)
			continue
		case !inB && inC:
			d.Status = CaseAdded
			d.CandidatePassRate, d.CandidateScore = cc.PassRate, cc.MeanScore
			cmp.Cases = append(cmp.Cases, d)
			continue
		}
		d.BaselinePassRate, d.CandidatePassRate = bc.PassRate, cc.PassRate
		d.BaselineScore, d.CandidateScore = bc.MeanScore, cc.MeanScore
		d.ScoreDelta = cc.MeanScore - bc.MeanScore
		switch {
		case d.CandidatePassRate < d.BaselinePassRate-1e-12 || (d.CandidatePassRate == d.BaselinePassRate && d.ScoreDelta < -1e-9):
			d.Status = CaseWorse
		case d.CandidatePassRate > d.BaselinePassRate+1e-12 || (d.CandidatePassRate == d.BaselinePassRate && d.ScoreDelta > 1e-9):
			d.Status = CaseImproved
		default:
			d.Status = CaseUnchanged
		}
		cmp.Cases = append(cmp.Cases, d)
		cmp.PairedCases++
		diffs = append(diffs, d.ScoreDelta)
		baseScores = append(baseScores, bc.MeanScore)
		candScores = append(candScores, cc.MeanScore)

		candByRep := map[int]RunResult{}
		for _, r := range cc.Runs {
			candByRep[r.Repetition] = r
		}
		for _, br := range bc.Runs {
			cr, ok := candByRep[br.Repetition]
			if !ok {
				continue
			}
			cmp.PairedRuns++
			baseRuns++
			candRuns++
			if br.Passed {
				basePass++
			}
			if cr.Passed {
				candPass++
			}
			switch {
			case br.Passed && !cr.Passed:
				b++
			case !br.Passed && cr.Passed:
				c++
			}
		}
	}
	if baseRuns > 0 {
		cmp.BaselinePassRate = float64(basePass) / float64(baseRuns)
		cmp.CandidatePassRate = float64(candPass) / float64(candRuns)
	}
	cmp.BaselineScore = Mean(baseScores)
	cmp.CandidateScore = Mean(candScores)
	cmp.McNemar = McNemar(b, c)
	cmp.Wilcoxon = Wilcoxon(diffs)

	passWorse := cmp.McNemar.PValue < alpha && b > c
	passBetter := cmp.McNemar.PValue < alpha && c > b
	scoreWorse := cmp.Wilcoxon.PValue < alpha && cmp.Wilcoxon.WMinus > cmp.Wilcoxon.WPlus
	scoreBetter := cmp.Wilcoxon.PValue < alpha && cmp.Wilcoxon.WPlus > cmp.Wilcoxon.WMinus
	cmp.Regressed = passWorse || scoreWorse
	cmp.Improved = (passBetter || scoreBetter) && !cmp.Regressed
	switch {
	case cmp.PairedCases == 0:
		cmp.Verdict = "no cases in common"
	case cmp.Regressed:
		cmp.Verdict = fmt.Sprintf("REGRESSION (McNemar p=%.4g, Wilcoxon p=%.4g, alpha=%g)", cmp.McNemar.PValue, cmp.Wilcoxon.PValue, alpha)
	case cmp.Improved:
		cmp.Verdict = fmt.Sprintf("improvement (McNemar p=%.4g, Wilcoxon p=%.4g, alpha=%g)", cmp.McNemar.PValue, cmp.Wilcoxon.PValue, alpha)
	default:
		cmp.Verdict = fmt.Sprintf("no significant change (McNemar p=%.4g, Wilcoxon p=%.4g, alpha=%g)", cmp.McNemar.PValue, cmp.Wilcoxon.PValue, alpha)
	}
	return cmp, nil
}

func resultLabel(r *SuiteResult) string {
	if r.StartedAt.IsZero() {
		return r.Suite
	}
	return fmt.Sprintf("%s @ %s", r.Suite, r.StartedAt.UTC().Format("2006-01-02T15:04:05Z"))
}

func indexCases(r *SuiteResult) map[string]CaseResult {
	m := make(map[string]CaseResult, len(r.Cases))
	for _, c := range r.Cases {
		m[c.ID] = c
	}
	return m
}

// WriteJSON writes the result as indented JSON.
func (r *SuiteResult) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(r)
}

// SaveResult writes a result file atomically.
func SaveResult(path string, r *SuiteResult) error {
	return writeFileAtomic(path, r.WriteJSON)
}

// ReadResult parses a result written by WriteJSON.
func ReadResult(rd io.Reader) (*SuiteResult, error) {
	data, err := io.ReadAll(io.LimitReader(rd, 1<<30))
	if err != nil {
		return nil, err
	}
	var r SuiteResult
	if err := json.Unmarshal(bytes.TrimSpace(data), &r); err != nil {
		return nil, fmt.Errorf("evals: parse result: %w", err)
	}
	if r.Schema != ResultSchema {
		return nil, fmt.Errorf("evals: unsupported result schema %q (want %q)", r.Schema, ResultSchema)
	}
	for i := range r.Cases {
		c := &r.Cases[i]
		if c.N != len(c.Runs) {
			return nil, fmt.Errorf("evals: case %q: n=%d but %d runs", c.ID, c.N, len(c.Runs))
		}
		if math.IsNaN(c.MeanScore) {
			return nil, fmt.Errorf("evals: case %q: invalid mean score", c.ID)
		}
	}
	return &r, nil
}

// LoadResult reads a result file.
func LoadResult(path string) (*SuiteResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r, err := ReadResult(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return r, nil
}
