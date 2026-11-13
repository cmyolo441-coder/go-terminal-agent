package subagent

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Format renders the report for the parent model: a one-line summary, one
// "## <description>" section per task in task order, and the total usage.
// Each report is cut to limit characters (bytes), keeping its head and tail;
// limit <= 0 means DefaultReportLimit.
func (r *Report) Format(limit int) string {
	if limit <= 0 {
		limit = DefaultReportLimit
	}
	var b strings.Builder
	failed := r.Failed()
	fmt.Fprintf(&b, "%s finished in %s (%s mode): %d succeeded, %d failed.\n",
		plural(len(r.Results), "sub-agent"), fmtDuration(r.Duration), r.Mode, len(r.Results)-failed, failed)

	for _, res := range r.Results {
		fmt.Fprintf(&b, "\n## %s\n\n", res.Description)
		report := truncateReport(res.Report, limit)
		switch {
		case res.Err == nil:
			b.WriteString(report)
			b.WriteString("\n")
		case errors.Is(res.Err, context.Canceled) || errors.Is(res.Err, context.DeadlineExceeded):
			b.WriteString("**Cancelled** before finishing.\n")
			if report != "" {
				b.WriteString("\nLatest output before it stopped:\n\n" + report + "\n")
			}
		default:
			b.WriteString("**Failed:** " + oneLine(res.Err.Error(), 1000) + "\n")
			if report != "" {
				b.WriteString("\nLatest output before it stopped:\n\n" + report + "\n")
			}
		}
		for _, n := range res.Notes {
			b.WriteString("\n_Note: " + oneLine(n, 500) + "_\n")
		}
	}

	u := r.Usage
	fmt.Fprintf(&b, "\n---\nSub-agent usage (%s): %s input tokens", r.Model, fmtInt(u.ContextTokens()))
	if cached := u.CacheReadTokens + u.CacheWriteTokens; cached > 0 {
		fmt.Fprintf(&b, " (%s cache read, %s cache write)", fmtInt(u.CacheReadTokens), fmtInt(u.CacheWriteTokens))
	}
	fmt.Fprintf(&b, ", %s output tokens, %s, %s.", fmtInt(u.OutputTokens), plural(r.ModelCalls, "model call"), plural(r.ToolCalls, "tool call"))
	return b.String()
}

// truncateReport keeps the head and tail of a long report, cut at line
// boundaries where possible and never inside a UTF-8 sequence.
func truncateReport(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	headLen := limit * 3 / 4
	tailLen := limit - headLen
	head := clipBytes(s, headLen)
	if i := strings.LastIndexByte(head, '\n'); i >= len(head)*4/5 {
		head = head[:i]
	}
	start := len(s) - tailLen
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	tail := s[start:]
	if i := strings.IndexByte(tail, '\n'); i >= 0 && i <= len(tail)/5 {
		tail = tail[i+1:]
	}
	omitted := len(s) - len(head) - len(tail)
	return strings.TrimRight(head, "\n") + fmt.Sprintf("\n\n[… %s characters of this report omitted …]\n\n", fmtInt(int64(omitted))) + strings.TrimLeft(tail, "\n")
}

// oneLine collapses whitespace and clips s to n bytes.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return clipBytes(s, n) + "…"
	}
	return s
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

// fmtInt renders n with thousands separators.
func fmtInt(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func fmtDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return d.Round(time.Millisecond).String()
	case d < time.Minute:
		return d.Round(100 * time.Millisecond).String()
	default:
		return d.Round(time.Second).String()
	}
}
