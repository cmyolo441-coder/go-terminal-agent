package evals

import (
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"
)

// TableOptions tune the terminal report.
type TableOptions struct {
	// Failures lists each failing run with its reason (default on through
	// WriteTable; set MaxFailures < 0 to hide).
	MaxFailures int
	// Grades adds a per-grader line for every failing run.
	Grades bool
}

// WriteTable writes a human-readable summary table.
func WriteTable(w io.Writer, r *SuiteResult, opts TableOptions) error {
	ew := &errWriter{w: w}
	fmt.Fprintf(ew, "Suite: %s", r.Suite)
	if !r.StartedAt.IsZero() {
		fmt.Fprintf(ew, "   started %s", r.StartedAt.UTC().Format(time.RFC3339))
	}
	fmt.Fprintf(ew, "   took %s\n\n", fmtSeconds(r.DurationSeconds))

	maxN := 0
	for _, c := range r.Cases {
		maxN = max(maxN, c.N)
	}
	tw := tabwriter.NewWriter(ew, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "CASE\tRUNS\tPASS\tPASS@1\tPASS@k\tPASS^k\tSCORE\t±SD\t95% CI\tCOST\tAVG TIME\t")
	for _, c := range r.Cases {
		passAtK, passHatK := "-", "-"
		if c.N > 0 {
			passAtK = fmt.Sprintf("%s (k=%d)", fmtPct(c.PassAt(c.N)), c.N)
			passHatK = fmtPct(c.PassHat(c.N))
		}
		fmt.Fprintf(tw, "%s\t%d\t%d/%d\t%s\t%s\t%s\t%.3f\t%.3f\t[%.3f, %.3f]\t%s\t%s\t\n",
			truncateRunes(c.ID, 48), c.N, c.Passes, c.N, fmtPct(c.PassAt(1)), passAtK, passHatK,
			c.MeanScore, c.StdDev, c.CI95.Low, c.CI95.High, fmtUSD(c.CostUSD), fmtSeconds(c.MeanDurationSec))
	}
	s := r.Summary
	fmt.Fprintf(tw, "TOTAL\t%d\t%d/%d\t%s\t\t\t%.3f\t%.3f\t[%.3f, %.3f]\t%s\t\t\n",
		s.Runs, s.PassedRuns, s.Runs, fmtPct(s.PassRate), s.MeanScore, s.StdDev, s.CI95.Low, s.CI95.High, fmtUSD(s.CostUSD))
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(ew, "\n%d case(s): %d fully passing, %d passing at least once; %d agent error(s), %d limit violation(s); tokens in/out %d/%d\n",
		s.Cases, s.CasesAllPassed, s.CasesAnyPassed, s.AgentError, s.Violations, s.Usage.InputTokens, s.Usage.OutputTokens)

	limit := opts.MaxFailures
	if limit == 0 {
		limit = 20
	}
	if limit > 0 {
		var failures []RunResult
		for _, c := range r.Cases {
			for _, run := range c.Runs {
				if !run.Passed {
					failures = append(failures, run)
				}
			}
		}
		if len(failures) > 0 {
			fmt.Fprintf(ew, "\nFailures:\n")
			for i, f := range failures {
				if i == limit {
					fmt.Fprintf(ew, "  … %d more\n", len(failures)-limit)
					break
				}
				fmt.Fprintf(ew, "  %s #%d: %s\n", f.CaseID, f.Repetition, oneLine(f.FailureReason(), 200))
				if opts.Grades {
					for _, g := range f.Grades {
						mark := "ok  "
						if !g.Passed {
							mark = "FAIL"
						}
						fmt.Fprintf(ew, "      %s %-28s %.2f  %s\n", mark, g.Name, g.Score, oneLine(g.Detail, 120))
					}
				}
				if f.Workspace != "" {
					fmt.Fprintf(ew, "      workspace: %s\n", f.Workspace)
				}
			}
		}
	}
	return ew.err
}

// WriteComparison writes a human-readable regression report.
func WriteComparison(w io.Writer, c *Comparison) error {
	ew := &errWriter{w: w}
	fmt.Fprintf(ew, "Baseline:  %s\nCandidate: %s\n\n", c.Baseline, c.Candidate)
	fmt.Fprintf(ew, "Pass rate: %s -> %s (%+.1f pts) over %d paired run(s)\n",
		fmtPct(c.BaselinePassRate), fmtPct(c.CandidatePassRate), 100*(c.CandidatePassRate-c.BaselinePassRate), c.PairedRuns)
	fmt.Fprintf(ew, "  McNemar (%s): %d regressed, %d fixed, p = %.4g\n", c.McNemar.Method, c.McNemar.B, c.McNemar.C, c.McNemar.PValue)
	fmt.Fprintf(ew, "Mean score: %.3f -> %.3f (%+.3f) over %d paired case(s)\n",
		c.BaselineScore, c.CandidateScore, c.CandidateScore-c.BaselineScore, c.PairedCases)
	fmt.Fprintf(ew, "  Wilcoxon signed-rank (%s): W+ = %g, W- = %g, n = %d, p = %.4g\n\n",
		c.Wilcoxon.Method, c.Wilcoxon.WPlus, c.Wilcoxon.WMinus, c.Wilcoxon.N, c.Wilcoxon.PValue)

	tw := tabwriter.NewWriter(ew, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CASE\tSTATUS\tPASS RATE\tSCORE\tΔ SCORE")
	for _, d := range c.Cases {
		if d.Status == CaseUnchanged {
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%s -> %s\t%.3f -> %.3f\t%+.3f\n", truncateRunes(d.ID, 48), d.Status,
			fmtPct(d.BaselinePassRate), fmtPct(d.CandidatePassRate), d.BaselineScore, d.CandidateScore, d.ScoreDelta)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(ew, "\nVerdict: %s\n", c.Verdict)
	return ew.err
}

type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	n, err := e.w.Write(p)
	e.err = err
	return n, err
}

func fmtPct(f float64) string {
	if math.IsNaN(f) {
		return "-"
	}
	return strconv.FormatFloat(100*f, 'f', 1, 64) + "%"
}

func fmtUSD(f float64) string {
	if f == 0 {
		return "-"
	}
	if f < 0.01 {
		return fmt.Sprintf("$%.4f", f)
	}
	return fmt.Sprintf("$%.2f", f)
}

func fmtSeconds(s float64) string {
	d := time.Duration(s * float64(time.Second))
	switch {
	case d < time.Second:
		return d.Round(time.Millisecond).String()
	case d < time.Minute:
		return d.Round(100 * time.Millisecond).String()
	default:
		return d.Round(time.Second).String()
	}
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	return truncateRunes(s, n)
}

// JUnit XML types (the de-facto schema understood by Jenkins, GitLab,
// GitHub Actions reporters and others).
type junitTestSuites struct {
	XMLName  xml.Name         `xml:"testsuites"`
	Name     string           `xml:"name,attr"`
	Tests    int              `xml:"tests,attr"`
	Failures int              `xml:"failures,attr"`
	Errors   int              `xml:"errors,attr"`
	Time     string           `xml:"time,attr"`
	Suites   []junitTestSuite `xml:"testsuite"`
}

type junitTestSuite struct {
	Name       string          `xml:"name,attr"`
	Tests      int             `xml:"tests,attr"`
	Failures   int             `xml:"failures,attr"`
	Errors     int             `xml:"errors,attr"`
	Skipped    int             `xml:"skipped,attr"`
	Time       string          `xml:"time,attr"`
	Timestamp  string          `xml:"timestamp,attr,omitempty"`
	Properties []junitProperty `xml:"properties>property,omitempty"`
	Cases      []junitTestCase `xml:"testcase"`
}

type junitProperty struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

type junitTestCase struct {
	Name      string        `xml:"name,attr"`
	Classname string        `xml:"classname,attr"`
	Time      string        `xml:"time,attr"`
	Failure   *junitProblem `xml:"failure,omitempty"`
	Error     *junitProblem `xml:"error,omitempty"`
	SystemOut *junitText    `xml:"system-out,omitempty"`
}

type junitProblem struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Body    string `xml:",chardata"`
}

type junitText struct {
	Body string `xml:",chardata"`
}

// xmlSafe removes characters XML 1.0 cannot represent at all (most C0
// controls), which encoding/xml would otherwise replace inconsistently, and
// repairs invalid UTF-8.
func xmlSafe(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return r
		case r < 0x20, r == 0xFFFE, r == 0xFFFF, r >= 0xD800 && r <= 0xDFFF:
			return -1
		}
		return r
	}, s)
}

func junitSeconds(s float64) string { return strconv.FormatFloat(s, 'f', 3, 64) }

// WriteJUnit writes one <testsuite> per case and one <testcase> per run.
// Failed graders become <failure>; agent errors and grader malfunctions
// become <error>.
func WriteJUnit(w io.Writer, r *SuiteResult) error {
	root := junitTestSuites{Name: xmlSafe(r.Suite), Time: junitSeconds(r.DurationSeconds)}
	for _, c := range r.Cases {
		ts := junitTestSuite{
			Name:  xmlSafe(r.Suite + "/" + c.ID),
			Tests: len(c.Runs),
		}
		if !r.StartedAt.IsZero() {
			ts.Timestamp = r.StartedAt.UTC().Format("2006-01-02T15:04:05")
		}
		ts.Properties = []junitProperty{
			{Name: "pass_rate", Value: strconv.FormatFloat(c.PassRate, 'f', 4, 64)},
			{Name: "mean_score", Value: strconv.FormatFloat(c.MeanScore, 'f', 4, 64)},
			{Name: "ci95", Value: fmt.Sprintf("%.4f..%.4f", c.CI95.Low, c.CI95.High)},
			{Name: "threshold", Value: strconv.FormatFloat(c.Threshold, 'f', 4, 64)},
		}
		if c.N > 0 {
			ts.Properties = append(ts.Properties,
				junitProperty{Name: fmt.Sprintf("pass@%d", c.N), Value: strconv.FormatFloat(c.PassAt(c.N), 'f', 4, 64)},
				junitProperty{Name: fmt.Sprintf("pass^%d", c.N), Value: strconv.FormatFloat(c.PassHat(c.N), 'f', 4, 64)})
		}
		var total float64
		for _, run := range c.Runs {
			total += run.DurationSeconds
			tc := junitTestCase{
				Name:      xmlSafe(fmt.Sprintf("%s#%d", c.ID, run.Repetition)),
				Classname: xmlSafe(r.Suite + "." + c.ID),
				Time:      junitSeconds(run.DurationSeconds),
			}
			var out strings.Builder
			fmt.Fprintf(&out, "score=%.4f turns=%d tool_calls=%d tokens_in=%d tokens_out=%d cost_usd=%.6f model=%s\n",
				run.Score, run.Turns, run.ToolCalls, run.Usage.InputTokens, run.Usage.OutputTokens, run.CostUSD, run.Model)
			for _, g := range run.Grades {
				status := "PASS"
				if !g.Passed {
					status = "FAIL"
				}
				fmt.Fprintf(&out, "%s %s (%s, weight %g, score %.3f): %s\n", status, g.Name, g.Type, g.Weight, g.Score, g.Detail)
			}
			if run.Workspace != "" {
				fmt.Fprintf(&out, "workspace: %s\n", run.Workspace)
			}
			tc.SystemOut = &junitText{Body: xmlSafe(out.String())}

			graderErr := ""
			for _, g := range run.Grades {
				if g.Error != "" {
					graderErr = g.Name + ": " + g.Error
					break
				}
			}
			switch {
			case run.Passed:
			case run.Error != "":
				ts.Errors++
				tc.Error = &junitProblem{Message: xmlSafe(oneLine(run.Error, 300)), Type: "AgentError", Body: xmlSafe(run.Error)}
			case graderErr != "":
				ts.Errors++
				tc.Error = &junitProblem{Message: xmlSafe(oneLine(graderErr, 300)), Type: "GraderError", Body: xmlSafe(run.FailureReason())}
			default:
				ts.Failures++
				typ := "GradeFailure"
				if len(run.Violations) > 0 {
					typ = "LimitExceeded"
				}
				tc.Failure = &junitProblem{Message: xmlSafe(oneLine(run.FailureReason(), 300)), Type: typ, Body: xmlSafe(run.FailureReason())}
			}
			ts.Cases = append(ts.Cases, tc)
		}
		ts.Time = junitSeconds(total)
		root.Tests += ts.Tests
		root.Failures += ts.Failures
		root.Errors += ts.Errors
		root.Suites = append(root.Suites, ts)
	}
	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(root); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}
