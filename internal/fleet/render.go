package fleet

import (
	"fmt"
	"io"
	"math"
	"strings"
	"text/tabwriter"
	"time"
)

// RenderOptions tune RenderStatus.
type RenderOptions struct {
	// Color enables ANSI colors.
	Color bool
	// MaxWorkers caps the worker table (default 20; the rest are summarised).
	MaxWorkers int
	// BarWidth is the width of queue bars (default 20).
	BarWidth int
}

const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
)

type painter bool

func (p painter) c(code, s string) string {
	if !p {
		return s
	}
	return code + s + ansiReset
}

// RenderStatus writes a terminal dashboard for s: leadership, task states,
// per-tenant queues, throughput, latency quantiles and worker health.
func RenderStatus(w io.Writer, s Snapshot, o RenderOptions) error {
	if o.MaxWorkers <= 0 {
		o.MaxWorkers = 20
	}
	if o.BarWidth <= 0 {
		o.BarWidth = 20
	}
	p := painter(o.Color)
	var b strings.Builder

	role := p.c(ansiYellow, "follower")
	if s.Leader {
		role = p.c(ansiGreen, "leader")
	}
	fmt.Fprintf(&b, "%s  %s  %s  token %d", p.c(ansiBold, "gta fleet"), s.NodeID, role, s.FencingToken)
	if !s.LeaderSince.IsZero() {
		fmt.Fprintf(&b, "  up %s", fmtDur(s.Time.Sub(s.LeaderSince)))
	}
	fmt.Fprintf(&b, "  %s\n\n", p.c(ansiDim, s.Time.UTC().Format(time.RFC3339)))
	if !s.Leader {
		b.WriteString("Not the leader: no queue state here.\n")
		_, err := io.WriteString(w, b.String())
		return err
	}

	b.WriteString(p.c(ansiBold, "Tasks") + "  ")
	for i, st := range AllStates {
		if i > 0 {
			b.WriteString("  ")
		}
		n := s.States[st]
		label := fmt.Sprintf("%s %d", st, n)
		switch {
		case st == StateDead && n > 0:
			label = p.c(ansiRed, label)
		case st == StateSucceeded:
			label = p.c(ansiGreen, label)
		}
		b.WriteString(label)
	}
	fmt.Fprintf(&b, "\nThroughput  %.2f/s (10s)  %.2f/s (1m)    Expiries %d  Redeliveries %d  Retries %d  Dead-lettered %d  Duplicates %d\n\n",
		s.Throughput.Last10s, s.Throughput.Last1m, s.Counters.LeaseExpiries, s.Counters.Redeliveries,
		s.Counters.Retries, s.Counters.DeadLettered, s.Counters.Duplicates)

	b.WriteString(p.c(ansiBold, "Queues") + "\n")
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  TENANT\tPRIORITY\tPENDING\tSCHED\tREADY\tLEASED\t")
	maxTotal := 1
	for _, q := range s.Queues {
		maxTotal = max(maxTotal, q.Total())
	}
	if len(s.Queues) == 0 {
		fmt.Fprintln(tw, "  (empty)\t\t\t\t\t\t")
	}
	for _, q := range s.Queues {
		fmt.Fprintf(tw, "  %s\t%s\t%d\t%d\t%d\t%d\t%s\n", q.Tenant, q.Priority, q.Pending, q.Scheduled, q.Ready, q.Leased,
			p.c(ansiCyan, bar(q.Total(), maxTotal, o.BarWidth)))
	}
	tw.Flush()
	fmt.Fprintf(&b, "  dead-letter queue: %d\n\n", s.DLQ)

	b.WriteString(p.c(ansiBold, "Latency") + "\n")
	tw = tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  \tCOUNT\tP50\tP95\tP99\tMAX")
	for _, r := range []struct {
		name string
		l    LatencySummary
	}{{"queue wait", s.QueueWait}, {"run time", s.RunTime}, {"end to end", s.EndToEnd}} {
		fmt.Fprintf(tw, "  %s\t%d\t%s\t%s\t%s\t%s\n", r.name, r.l.Count, fmtDur(r.l.P50.D()), fmtDur(r.l.P95.D()), fmtDur(r.l.P99.D()), fmtDur(r.l.Max.D()))
	}
	tw.Flush()
	b.WriteString("\n")

	fmt.Fprintf(&b, "%s (%d)\n", p.c(ansiBold, "Workers"), len(s.Workers))
	tw = tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  ID\tSTATE\tPLATFORM\tLEASES\tHEALTH\tOK\tFAIL\tEXPIRED\tSEEN")
	for i, wk := range s.Workers {
		if i == o.MaxWorkers {
			fmt.Fprintf(tw, "  … %d more\t\t\t\t\t\t\t\t\n", len(s.Workers)-i)
			break
		}
		state := string(wk.State)
		switch wk.State {
		case WorkerActive:
			state = p.c(ansiGreen, state)
		case WorkerQuarantined, WorkerLost:
			state = p.c(ansiRed, state)
		case WorkerDraining:
			state = p.c(ansiYellow, state)
		}
		platform := strings.Trim(wk.Capabilities.OS+"/"+wk.Capabilities.Arch, "/")
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%d/%d\t%s\t%d\t%d\t%d\t%s ago\n", wk.ID, state, platform, wk.Leases,
			wk.Capabilities.MaxConcurrency, healthBar(p, wk.Health), wk.Succeeded, wk.Failed, wk.Expired,
			fmtDur(s.Time.Sub(wk.LastSeen)))
	}
	tw.Flush()
	_, err := io.WriteString(w, b.String())
	return err
}

func bar(n, total, width int) string {
	if total <= 0 || n <= 0 {
		return ""
	}
	k := int(math.Ceil(float64(n) / float64(total) * float64(width)))
	return strings.Repeat("█", min(k, width))
}

func healthBar(p painter, h float64) string {
	h = min(max(h, 0), 1)
	filled := int(math.Round(h * 5))
	s := strings.Repeat("●", filled) + strings.Repeat("○", 5-filled)
	switch {
	case h < 0.3:
		s = p.c(ansiRed, s)
	case h < 0.7:
		s = p.c(ansiYellow, s)
	default:
		s = p.c(ansiGreen, s)
	}
	return fmt.Sprintf("%s %.2f", s, h)
}

// fmtDur renders a duration compactly with about three significant digits.
func fmtDur(d time.Duration) string {
	switch {
	case d <= 0:
		return "0"
	case d < time.Microsecond:
		return fmt.Sprintf("%dns", d.Nanoseconds())
	case d < time.Millisecond:
		return fmt.Sprintf("%.1fµs", float64(d)/float64(time.Microsecond))
	case d < time.Second:
		return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
	case d < time.Minute:
		return fmt.Sprintf("%.2fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
