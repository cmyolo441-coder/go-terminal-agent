package workflow

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"
)

// TimelineEntry is one bar of a Gantt chart.
type TimelineEntry struct {
	Step         string
	Status       StepStatus
	Start        time.Time
	End          time.Time
	Attempts     int
	Compensation CompensationStatus
}

// Timeline returns the run's steps as Gantt entries in topological order.
func (r *Result) Timeline() []TimelineEntry {
	out := make([]TimelineEntry, 0, len(r.Steps))
	for _, s := range r.Steps {
		out = append(out, TimelineEntry{
			Step: s.ID, Status: s.Status, Start: s.Started, End: s.Finished,
			Attempts: s.Attempts, Compensation: s.Compensation,
		})
	}
	return out
}

// TimelineFromRecords rebuilds a timeline from journal records, for
// inspecting a run that is not in memory (see ReadJournal). Steps appear in
// the order they were first recorded.
func TimelineFromRecords(recs []Record) []TimelineEntry {
	idx := map[string]int{}
	var out []TimelineEntry
	get := func(id string) *TimelineEntry {
		i, ok := idx[id]
		if !ok {
			i = len(out)
			idx[id] = i
			out = append(out, TimelineEntry{Step: id, Status: StepPending})
		}
		return &out[i]
	}
	for _, rec := range recs {
		if rec.Step == "" {
			continue
		}
		e := get(rec.Step)
		switch rec.Type {
		case RecStepStarted:
			if e.Start.IsZero() {
				e.Start = rec.Time
			}
			e.Attempts = max(e.Attempts, rec.Attempt)
			e.Status = StepRunning
		case RecStepRetry:
			e.Status = StepRetrying
		case RecStepSucceeded:
			e.Status, e.End = StepSucceeded, rec.Time
		case RecStepFailed:
			e.Status, e.End = StepFailed, rec.Time
		case RecStepSkipped:
			e.Status, e.End = StepSkipped, rec.Time
		case RecStepCancelled:
			e.Status, e.End = StepCancelled, rec.Time
		case RecCompensationStarted:
			e.Compensation = CompensationRunning
		case RecCompensationSucceeded:
			e.Compensation = CompensationSucceeded
		case RecCompensationFailed:
			e.Compensation = CompensationFailed
		}
	}
	return out
}

// TimelineOptions configures RenderTimeline.
type TimelineOptions struct {
	// Width is the number of cells in the bar area; default 50.
	Width int
	// ASCII draws with '#' and '.' instead of block characters.
	ASCII bool
	// Now ends bars of steps still running; default the latest time seen.
	Now time.Time
	// MaxNameWidth truncates step names; default 32.
	MaxNameWidth int
}

// RenderTimeline draws a text Gantt chart: one row per step with a bar
// spanning its start to end on a shared time axis, then its duration,
// status and attempts.
//
//	step        |0s               1.50s              3.00s|
//	fetch       |██████····························|  1.00s  succeeded
//	build[os=a] |      ██████████████··············|  2.00s  succeeded (2 attempts)
func RenderTimeline(w io.Writer, entries []TimelineEntry, opts TimelineOptions) error {
	width := opts.Width
	if width <= 0 {
		width = 50
	}
	width = max(width, 12)
	nameMax := opts.MaxNameWidth
	if nameMax <= 0 {
		nameMax = 32
	}
	full, empty := "█", "·"
	if opts.ASCII {
		full, empty = "#", "."
	}

	var t0, t1 time.Time
	for _, e := range entries {
		if e.Start.IsZero() {
			continue
		}
		if t0.IsZero() || e.Start.Before(t0) {
			t0 = e.Start
		}
		end := e.End
		if end.IsZero() {
			end = opts.Now
		}
		if end.After(t1) {
			t1 = end
		}
	}
	if t1.Before(t0) {
		t1 = t0
	}
	span := t1.Sub(t0)

	nameW := runewidth.StringWidth("step")
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = runewidth.Truncate(e.Step, nameMax, "…")
		nameW = max(nameW, runewidth.StringWidth(names[i]))
	}

	var b strings.Builder
	left, mid, right := "0s", formatSeconds(span/2), formatSeconds(span)
	gap := width - len(left) - len(mid) - len(right)
	axis := left + strings.Repeat(" ", max(gap/2, 1)) + mid + strings.Repeat(" ", max(gap-gap/2, 1)) + right
	fmt.Fprintf(&b, "%s |%s|\n", runewidth.FillRight("step", nameW), axis)

	for i, e := range entries {
		bar := make([]string, width)
		for j := range bar {
			bar[j] = empty
		}
		dur := "      -"
		if !e.Start.IsZero() {
			end := e.End
			if end.IsZero() {
				end = t1
			}
			from, to := 0, width
			if span > 0 {
				from = int(float64(e.Start.Sub(t0)) / float64(span) * float64(width))
				to = int(float64(end.Sub(t0))/float64(span)*float64(width) + 0.5)
			}
			from = min(max(from, 0), width-1)
			to = min(max(to, from+1), width)
			for j := from; j < to; j++ {
				bar[j] = full
			}
			dur = fmt.Sprintf("%7s", formatSeconds(end.Sub(e.Start)))
		}
		status := string(e.Status)
		if e.Attempts > 1 {
			status += fmt.Sprintf(" (%d attempts)", e.Attempts)
		}
		if e.Compensation != CompensationNone {
			status += ", compensation " + string(e.Compensation)
		}
		fmt.Fprintf(&b, "%s |%s| %s  %s\n", runewidth.FillRight(names[i], nameW), strings.Join(bar, ""), dur, status)
	}
	_, err := io.WriteString(w, b.String())
	return err
}
