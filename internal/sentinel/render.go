package sentinel

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"
)

// RenderOptions control RenderSummary.
type RenderOptions struct {
	// Width is the terminal width (default 100).
	Width int
	// Max is the number of incidents listed (default 8); the rest are
	// counted.
	Max int
	// Color enables ANSI colours.
	Color bool
	// Now is the reference for relative times (default time.Now()).
	Now time.Time
	// Killed and KillReason add a kill-switch banner.
	Killed     bool
	KillReason string
	// Details adds each incident's first explanation line.
	Details bool
}

const (
	ansiReset = "\x1b[0m"
	ansiDim   = "\x1b[2m"
	ansiBold  = "\x1b[1m"
)

var sevStyle = [numSeverities]struct{ label, color string }{
	{"NONE", ansiDim},
	{"INFO", ansiDim},
	{"LOW ", "\x1b[36m"},
	{"MED ", "\x1b[33m"},
	{"HIGH", "\x1b[31m"},
	{"CRIT", "\x1b[1;97;41m"},
}

// RenderSummary writes a compact, terminal-friendly summary of incidents:
// a header with counts by severity, then one line per incident, most
// severe and most recent first.
//
//	sentinel  3 incidents · 1 crit · 1 high · 1 med
//	CRIT sequence/pipe-to-shell    a download was piped …   ask   ×1  12s  snt_9f…
func RenderSummary(w io.Writer, incs []Incident, opt RenderOptions) error {
	if opt.Width <= 0 {
		opt.Width = 100
	}
	if opt.Max <= 0 {
		opt.Max = 8
	}
	if opt.Now.IsZero() {
		opt.Now = time.Now()
	}
	paint := func(code, s string) string {
		if !opt.Color || code == "" {
			return s
		}
		return code + s + ansiReset
	}

	var counts [numSeverities]int
	for i := range incs {
		if incs[i].Severity < numSeverities {
			counts[incs[i].Severity]++
		}
	}
	var b strings.Builder
	head := fmt.Sprintf("sentinel  %d incident%s", len(incs), plural(len(incs)))
	for s := SeverityCritical; s >= SeverityInfo; s-- {
		if counts[s] > 0 {
			head += fmt.Sprintf(" · %d %s", counts[s], strings.ToLower(strings.TrimSpace(sevStyle[s].label)))
		}
	}
	b.WriteString(paint(ansiBold, head))
	b.WriteByte('\n')
	if opt.Killed {
		b.WriteString(paint(sevStyle[SeverityCritical].color, runewidth.Truncate(" KILL SWITCH ENGAGED: "+opt.KillReason+" ", opt.Width, "…")))
		b.WriteByte('\n')
	}
	if len(incs) == 0 {
		b.WriteString(paint(ansiDim, "  no incidents"))
		b.WriteByte('\n')
		_, err := io.WriteString(w, b.String())
		return err
	}

	sorted := slices.Clone(incs)
	slices.SortStableFunc(sorted, func(a, b Incident) int {
		if a.Severity != b.Severity {
			return int(b.Severity) - int(a.Severity)
		}
		return b.LastSeen.Compare(a.LastSeen)
	})
	ruleW := 0
	for i := range sorted[:min(len(sorted), opt.Max)] {
		ruleW = max(ruleW, runewidth.StringWidth(sorted[i].Detector+"/"+sorted[i].Rule))
	}
	ruleW = min(ruleW, 28)

	for i := range sorted[:min(len(sorted), opt.Max)] {
		inc := &sorted[i]
		st := sevStyle[min(inc.Severity, SeverityCritical)]
		rule := runewidth.FillRight(runewidth.Truncate(inc.Detector+"/"+inc.Rule, ruleW, "…"), ruleW)
		tail := fmt.Sprintf("  %-8s ×%-3d %5s  %s", shortAction(inc.Action), inc.Count, ago(opt.Now.Sub(inc.LastSeen)), inc.ID)
		// label + space + rule + 2 spaces + summary + tail
		room := opt.Width - 4 - 1 - ruleW - 2 - runewidth.StringWidth(tail)
		summary := inc.Summary
		if room < 12 {
			room = max(opt.Width-4-1-ruleW-2, 12)
			tail = ""
		}
		summary = runewidth.FillRight(runewidth.Truncate(summary, room, "…"), room)
		b.WriteString(paint(st.color, st.label))
		b.WriteByte(' ')
		b.WriteString(rule)
		b.WriteString("  ")
		b.WriteString(summary)
		b.WriteString(paint(ansiDim, tail))
		b.WriteByte('\n')
		if opt.Details && len(inc.Details) > 0 {
			b.WriteString(paint(ansiDim, runewidth.Truncate("     "+inc.Details[0], opt.Width, "…")))
			b.WriteByte('\n')
		}
	}
	if extra := len(sorted) - opt.Max; extra > 0 {
		b.WriteString(paint(ansiDim, fmt.Sprintf("     … %d more", extra)))
		b.WriteByte('\n')
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// Summary renders the sentinel's incidents with RenderSummary.
func (s *Sentinel) Summary(opt RenderOptions) string {
	if opt.Now.IsZero() {
		opt.Now = s.clock.Now()
	}
	opt.Killed, opt.KillReason, _ = s.KillState()
	var b strings.Builder
	_ = RenderSummary(&b, s.Incidents(), opt)
	return b.String()
}

func shortAction(a Action) string {
	if a == ActionRequireApproval {
		return "approve"
	}
	return a.String()
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// ago formats a duration compactly: 12s, 4m, 3h, 2d.
func ago(d time.Duration) string {
	switch {
	case d < 0:
		return "now"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}
