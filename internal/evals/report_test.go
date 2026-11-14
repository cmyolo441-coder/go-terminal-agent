package evals

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// syntheticResult builds a result with given per-run pass flags and scores.
func syntheticResult(name string, cases map[string][]bool, scores map[string][]float64) *SuiteResult {
	var crs []CaseResult
	for _, id := range sortedKeys(cases) {
		c := &Case{ID: id, threshold: 1}
		var runs []RunResult
		for i, p := range cases[id] {
			s := 0.0
			if p {
				s = 1
			}
			if sc, ok := scores[id]; ok {
				s = sc[i]
			}
			run := RunResult{CaseID: id, Repetition: i, Passed: p, Score: s, DurationSeconds: 1.5,
				Grades: []GradeResult{{Name: "g", Type: GradeOutputMatches, Score: s, Passed: p, Weight: 1, Detail: "detail <&> \x00 \"q\""}}}
			runs = append(runs, run)
		}
		crs = append(crs, aggregateCase(c, runs, 1, 500))
	}
	return &SuiteResult{Schema: ResultSchema, Suite: name, StartedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		DurationSeconds: 12.3, Cases: crs, Summary: summarizeSuite(crs, 1, 500)}
}

func TestWriteTable(t *testing.T) {
	r := syntheticResult("demo", map[string][]bool{"alpha": {true, true, false}, "beta": {false, false}}, nil)
	r.Cases[1].Runs[0].Error = "model unavailable"
	r.Cases[1].Runs[1].Workspace = "/tmp/ws"
	var buf bytes.Buffer
	if err := WriteTable(&buf, r, TableOptions{Grades: true}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"Suite: demo", "CASE", "PASS@1", "PASS^k", "alpha", "2/3", "66.7%", "100.0% (k=3)", "0.0%", "TOTAL", "2/5", "Failures:", "beta #0: agent error: model unavailable", "workspace: /tmp/ws", "FAIL g"} {
		if !strings.Contains(out, want) {
			t.Errorf("table misses %q:\n%s", want, out)
		}
	}
	if err := WriteTable(failWriter{}, r, TableOptions{}); err == nil {
		t.Error("write errors must propagate")
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestResultJSONRoundTrip(t *testing.T) {
	r := syntheticResult("demo", map[string][]bool{"a": {true, false}}, nil)
	path := filepath.Join(t.TempDir(), "out", "result.json")
	if err := SaveResult(path, r); err != nil {
		t.Fatal(err)
	}
	got, err := LoadResult(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Suite != "demo" || len(got.Cases) != 1 || got.Cases[0].Passes != 1 || !got.StartedAt.Equal(r.StartedAt) || got.Cases[0].K[1].PassAt != 1 {
		t.Errorf("round trip: %+v", got)
	}
	if _, err := ReadResult(strings.NewReader(`{"schema":"other"}`)); err == nil {
		t.Error("wrong schema accepted")
	}
	if _, err := ReadResult(strings.NewReader(`{"schema":"gta-eval-result/v1","cases":[{"id":"x","n":3,"runs":[]}]}`)); err == nil {
		t.Error("inconsistent n accepted")
	}
}

func TestJUnitWellFormed(t *testing.T) {
	r := syntheticResult("demo <suite> & co", map[string][]bool{"alpha": {true, false}, "beta\x01": {false}}, nil)
	r.Cases[1].Runs[0].Error = "panic: bad \x07 bytes \xff"
	r.Cases[0].Runs[1].Violations = []string{"max_turns"}
	var buf bytes.Buffer
	if err := WriteJUnit(&buf, r); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "<?xml") {
		t.Fatal("missing XML header")
	}
	// Strict well-formedness: decode every token.
	dec := xml.NewDecoder(strings.NewReader(out))
	dec.Strict = true
	for {
		_, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("not well-formed: %v\n%s", err, out)
		}
	}
	var parsed junitTestSuites
	if err := xml.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Tests != 3 || parsed.Failures != 1 || parsed.Errors != 1 || len(parsed.Suites) != 2 {
		t.Errorf("counts: tests=%d failures=%d errors=%d suites=%d", parsed.Tests, parsed.Failures, parsed.Errors, len(parsed.Suites))
	}
	alpha := parsed.Suites[0]
	if alpha.Cases[0].Failure != nil || alpha.Cases[1].Failure == nil || alpha.Cases[1].Failure.Type != "LimitExceeded" {
		t.Errorf("alpha cases: %+v", alpha.Cases)
	}
	if alpha.Cases[0].Name != "alpha#0" || alpha.Cases[0].Time != "1.500" {
		t.Errorf("testcase attrs: %+v", alpha.Cases[0])
	}
	var props []string
	for _, p := range alpha.Properties {
		props = append(props, p.Name)
	}
	if !strings.Contains(strings.Join(props, ","), "pass@2") {
		t.Errorf("properties: %v", props)
	}
	if e := parsed.Suites[1].Cases[0].Error; e == nil || e.Type != "AgentError" || strings.ContainsRune(e.Body, 0x07) {
		t.Errorf("error element: %+v", e)
	}
}

func TestCompareRegression(t *testing.T) {
	ids := []string{"c01", "c02", "c03", "c04", "c05", "c06", "c07", "c08", "c09", "c10", "c11", "c12"}
	basePass := map[string][]bool{}
	candPass := map[string][]bool{}
	baseScore := map[string][]float64{}
	candScore := map[string][]float64{}
	for i, id := range ids {
		basePass[id] = []bool{true, true}
		baseScore[id] = []float64{1, 1}
		// The candidate breaks every case on both repetitions except
		// two, and scores drop across the board.
		ok := i < 2
		candPass[id] = []bool{ok, ok}
		s := 0.4 + 0.01*float64(i)
		if ok {
			s = 1
		}
		candScore[id] = []float64{s, s}
	}
	candPass["new"] = []bool{true}
	basePass["gone"] = []bool{false}
	base := syntheticResult("s", basePass, baseScore)
	cand := syntheticResult("s", candPass, candScore)
	cmp, err := Compare(base, cand, 0.05)
	if err != nil {
		t.Fatal(err)
	}
	if cmp.PairedRuns != 24 || cmp.PairedCases != 12 {
		t.Fatalf("pairs: runs=%d cases=%d", cmp.PairedRuns, cmp.PairedCases)
	}
	if cmp.McNemar.B != 20 || cmp.McNemar.C != 0 || cmp.McNemar.Method != "exact" {
		t.Errorf("mcnemar: %+v", cmp.McNemar)
	}
	if want := 2 * math.Pow(0.5, 20); math.Abs(cmp.McNemar.PValue-want) > 1e-15 {
		t.Errorf("mcnemar p = %g, want %g", cmp.McNemar.PValue, want)
	}
	// 10 negative differences, all distinct: exact p = 2/2^10.
	if cmp.Wilcoxon.N != 10 || cmp.Wilcoxon.WPlus != 0 || math.Abs(cmp.Wilcoxon.PValue-2.0/1024) > 1e-15 {
		t.Errorf("wilcoxon: %+v", cmp.Wilcoxon)
	}
	if !cmp.Regressed || cmp.Improved || !strings.HasPrefix(cmp.Verdict, "REGRESSION") {
		t.Errorf("verdict: %s", cmp.Verdict)
	}
	status := map[string]CaseStatus{}
	for _, d := range cmp.Cases {
		status[d.ID] = d.Status
	}
	if status["c01"] != CaseUnchanged || status["c05"] != CaseWorse || status["new"] != CaseAdded || status["gone"] != CaseRemoved {
		t.Errorf("statuses: %v", status)
	}
	var buf bytes.Buffer
	if err := WriteComparison(&buf, cmp); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"McNemar (exact): 20 regressed, 0 fixed", "Wilcoxon signed-rank (exact)", "c05", "worse", "added", "Verdict: REGRESSION"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("comparison report misses %q:\n%s", want, buf.String())
		}
	}

	// Reversed roles: an improvement, not a regression.
	rev, _ := Compare(cand, base, 0.05)
	if rev.Regressed || !rev.Improved {
		t.Errorf("reverse verdict: %s", rev.Verdict)
	}
	// Identical results: no change.
	same, _ := Compare(base, base, 0.05)
	if same.Regressed || same.Improved || same.McNemar.PValue != 1 || same.Wilcoxon.PValue != 1 {
		t.Errorf("identical: %+v", same)
	}
	if _, err := Compare(nil, base, 0); err == nil {
		t.Error("nil result accepted")
	}
}
