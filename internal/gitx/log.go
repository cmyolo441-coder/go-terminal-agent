package gitx

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// DefaultLogCount is used by Log when n is zero or negative.
const DefaultLogCount = 20

// MaxLogCount caps how many commits Log returns.
const MaxLogCount = 1000

// LogEntry is one commit in the history.
type LogEntry struct {
	SHA, Short string
	Author     string
	Email      string
	// Time is the author date.
	Time time.Time
	// RelDate is git's relative author date, e.g. "3 hours ago".
	RelDate string
	Subject string
}

// String renders "1a2b3c4 subject (3 hours ago, Ada)".
func (e LogEntry) String() string {
	return fmt.Sprintf("%s %s (%s, %s)", e.Short, e.Subject, e.RelDate, e.Author)
}

// Log returns up to n of the most recent commits reachable from HEAD, newest
// first. A repository without commits yields an empty list.
func Log(ctx context.Context, root string, n int) ([]LogEntry, error) {
	if err := checkRoot(root); err != nil {
		return nil, err
	}
	if n <= 0 {
		n = DefaultLogCount
	}
	n = min(n, MaxLogCount)
	head, err := headCommit(ctx, root)
	if err != nil {
		return nil, err
	}
	if head == "" {
		return []LogEntry{}, nil
	}
	const sep, end = "\x1f", "\x1e"
	res, err := run(ctx, root, runOpts{}, "log", "-n", strconv.Itoa(n), "--no-decorate",
		"--format=%H"+"%x1f%h%x1f%an%x1f%ae%x1f%at%x1f%ar%x1f%s%x1e", "HEAD", "--")
	if err != nil {
		return nil, err
	}
	var out []LogEntry
	for _, rec := range strings.Split(string(res.stdout), end) {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		f := strings.Split(rec, sep)
		if len(f) != 7 {
			return nil, fmt.Errorf("gitx: unexpected log record %q", rec)
		}
		e := LogEntry{SHA: f[0], Short: f[1], Author: f[2], Email: f[3], RelDate: f[5], Subject: f[6]}
		if sec, err := strconv.ParseInt(f[4], 10, 64); err == nil {
			e.Time = time.Unix(sec, 0)
		}
		out = append(out, e)
	}
	if out == nil {
		out = []LogEntry{}
	}
	return out, nil
}
