package autopilot

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Severity ranks a review issue or verification gap.
type Severity string

const (
	// SeverityBlocker must be fixed: the plan or result is wrong.
	SeverityBlocker Severity = "blocker"
	// SeverityMajor should be fixed: something important is missing.
	SeverityMajor Severity = "major"
	// SeverityMinor is worth knowing but does not block.
	SeverityMinor Severity = "minor"
)

func normalizeSeverity(s string) Severity {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "blocker", "critical", "blocking", "error", "fatal":
		return SeverityBlocker
	case "minor", "low", "nit", "nitpick", "info", "suggestion", "trivial":
		return SeverityMinor
	default:
		return SeverityMajor
	}
}

// Review verdicts.
const (
	VerdictApprove = "approve"
	VerdictRevise  = "revise"
)

// Issue is one concrete problem a reviewer found in a plan.
type Issue struct {
	Severity Severity `json:"severity"`
	// Step is the plan step the issue concerns, 0 for the plan as a whole.
	Step        int    `json:"step,omitempty"`
	Description string `json:"description"`
	Suggestion  string `json:"suggestion,omitempty"`
}

func (i Issue) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s]", i.Severity)
	if i.Step > 0 {
		fmt.Fprintf(&b, " step %d:", i.Step)
	}
	b.WriteString(" " + i.Description)
	if i.Suggestion != "" {
		b.WriteString(" — suggestion: " + i.Suggestion)
	}
	return b.String()
}

// Review is a reviewer's verdict on a plan.
type Review struct {
	Verdict string  `json:"verdict"`
	Summary string  `json:"summary"`
	Issues  []Issue `json:"issues"`
}

// Approved reports whether the plan may go ahead: the reviewer approved
// and raised no blocker.
func (r *Review) Approved() bool {
	if r == nil || r.Verdict != VerdictApprove {
		return false
	}
	for _, i := range r.Issues {
		if i.Severity == SeverityBlocker {
			return false
		}
	}
	return true
}

// Blocking returns the blocker and major issues.
func (r *Review) Blocking() []Issue {
	if r == nil {
		return nil
	}
	var out []Issue
	for _, i := range r.Issues {
		if i.Severity != SeverityMinor {
			out = append(out, i)
		}
	}
	return out
}

// ParseReview extracts and validates a review. steps is the plan's step
// count, used to drop out-of-range step references.
func ParseReview(text string, steps int) (*Review, error) {
	raw, err := extractObject(text, "verdict")
	if err != nil {
		return nil, err
	}
	var r Review
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("the review JSON does not match the schema: %v", err)
	}
	switch strings.ToLower(strings.TrimSpace(r.Verdict)) {
	case "approve", "approved", "accept", "accepted", "lgtm", "pass":
		r.Verdict = VerdictApprove
	case "revise", "reject", "rejected", "request_changes", "changes_requested", "needs_changes", "needs_revision", "fail":
		r.Verdict = VerdictRevise
	default:
		return nil, fmt.Errorf(`"verdict" must be "approve" or "revise", got %q`, r.Verdict)
	}
	r.Summary = strings.TrimSpace(r.Summary)
	issues, err := cleanIssues(r.Issues, steps)
	if err != nil {
		return nil, err
	}
	r.Issues = issues
	if r.Verdict == VerdictRevise && len(r.Issues) == 0 {
		return nil, errors.New(`a "revise" verdict must list at least one concrete issue`)
	}
	return &r, nil
}

func cleanIssues(in []Issue, steps int) ([]Issue, error) {
	var out []Issue
	for n, i := range in {
		i.Description = strings.TrimSpace(i.Description)
		i.Suggestion = strings.TrimSpace(i.Suggestion)
		if i.Description == "" {
			return nil, fmt.Errorf("issue %d has no description", n+1)
		}
		i.Severity = normalizeSeverity(string(i.Severity))
		if i.Step < 0 || i.Step > steps {
			i.Step = 0
		}
		out = append(out, i)
		if len(out) == maxListEntries {
			break
		}
	}
	return out, nil
}

// Verification verdicts.
const (
	VerdictPass = "pass"
	VerdictGaps = "gaps"
)

// Gap is something the result is missing relative to the plan.
type Gap struct {
	Severity Severity `json:"severity"`
	// Step is the plan step the gap concerns, 0 for the whole result.
	Step        int    `json:"step,omitempty"`
	Description string `json:"description"`
}

func (g Gap) String() string {
	if g.Step > 0 {
		return fmt.Sprintf("[%s] step %d: %s", g.Severity, g.Step, g.Description)
	}
	return fmt.Sprintf("[%s] %s", g.Severity, g.Description)
}

// VerifyReport is the verifier's judgement of the finished work.
type VerifyReport struct {
	Verdict string `json:"verdict"`
	Summary string `json:"summary"`
	Gaps    []Gap  `json:"gaps"`
}

// Passed reports whether the work matches the plan: a pass verdict and no
// blocker or major gap.
func (v *VerifyReport) Passed() bool {
	if v == nil || v.Verdict != VerdictPass {
		return false
	}
	for _, g := range v.Gaps {
		if g.Severity != SeverityMinor {
			return false
		}
	}
	return true
}

// ParseVerifyReport extracts and validates a verification report.
func ParseVerifyReport(text string, steps int) (*VerifyReport, error) {
	raw, err := extractObject(text, "verdict")
	if err != nil {
		return nil, err
	}
	var v VerifyReport
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("the verification JSON does not match the schema: %v", err)
	}
	switch strings.ToLower(strings.TrimSpace(v.Verdict)) {
	case "pass", "passed", "ok", "complete", "completed", "approve", "approved":
		v.Verdict = VerdictPass
	case "gaps", "gap", "fail", "failed", "incomplete", "revise":
		v.Verdict = VerdictGaps
	default:
		return nil, fmt.Errorf(`"verdict" must be "pass" or "gaps", got %q`, v.Verdict)
	}
	v.Summary = strings.TrimSpace(v.Summary)
	var gaps []Gap
	for n, g := range v.Gaps {
		g.Description = strings.TrimSpace(g.Description)
		if g.Description == "" {
			return nil, fmt.Errorf("gap %d has no description", n+1)
		}
		g.Severity = normalizeSeverity(string(g.Severity))
		if g.Step < 0 || g.Step > steps {
			g.Step = 0
		}
		gaps = append(gaps, g)
		if len(gaps) == maxListEntries {
			break
		}
	}
	v.Gaps = gaps
	if v.Verdict == VerdictGaps && len(v.Gaps) == 0 {
		return nil, errors.New(`a "gaps" verdict must list at least one concrete gap`)
	}
	return &v, nil
}
