package gitx

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
)

// EntryKind classifies a line of git status.
type EntryKind int

const (
	// EntryChanged is an ordinary tracked change (porcelain v2 type "1").
	EntryChanged EntryKind = iota
	// EntryRenamed is a rename or copy (type "2"); OrigPath is set.
	EntryRenamed
	// EntryUnmerged is a path with an unresolved merge conflict (type "u").
	EntryUnmerged
	// EntryUntracked is a file git does not track (type "?").
	EntryUntracked
)

func (k EntryKind) String() string {
	switch k {
	case EntryRenamed:
		return "renamed"
	case EntryUnmerged:
		return "unmerged"
	case EntryUntracked:
		return "untracked"
	default:
		return "changed"
	}
}

// StatusEntry is one path reported by git status.
type StatusEntry struct {
	Kind EntryKind
	// Path is relative to the repository top level, with forward slashes.
	Path string
	// OrigPath is the source path of a rename or copy.
	OrigPath string
	// Index and Worktree are the porcelain XY codes ('.' when unchanged,
	// otherwise one of M, T, A, D, R, C, U). Both are '?' for untracked.
	Index, Worktree byte
}

// Staged reports whether the entry has changes in the index.
func (e StatusEntry) Staged() bool {
	return e.Kind != EntryUntracked && e.Kind != EntryUnmerged && e.Index != '.'
}

// Unstaged reports whether the entry has work tree changes not yet staged.
func (e StatusEntry) Unstaged() bool {
	return e.Kind != EntryUntracked && e.Kind != EntryUnmerged && e.Worktree != '.'
}

// Status is a snapshot of a repository's state.
type Status struct {
	// Branch is the checked-out branch name, empty when Detached.
	Branch string
	// Detached is true when HEAD does not point at a branch.
	Detached bool
	// Head is HEAD's abbreviated sha, empty when there are no commits yet.
	Head string
	// HeadFull is HEAD's full sha, empty when there are no commits yet.
	HeadFull string
	// Initial is true on a branch that has no commits yet.
	Initial bool
	// Upstream is the tracking branch, e.g. "origin/main", or empty.
	Upstream string
	// UpstreamGone is true when an upstream is configured but missing.
	UpstreamGone bool
	// Ahead and Behind count commits relative to Upstream.
	Ahead, Behind int
	// Staged, Unstaged, Untracked and Conflicted count files. A file can
	// be both staged and unstaged.
	Staged, Unstaged, Untracked, Conflicted int
	// Entries lists every changed path.
	Entries []StatusEntry
}

// Clean reports whether there are no staged, unstaged, untracked or
// conflicted files.
func (s *Status) Clean() bool {
	return s.Staged == 0 && s.Unstaged == 0 && s.Untracked == 0 && s.Conflicted == 0
}

// String renders a compact one-line summary such as
// "main@1a2b3c4 ↑1 ↓2 · 3 staged, 1 modified, 2 untracked".
func (s *Status) String() string {
	var b strings.Builder
	switch {
	case s.Detached:
		b.WriteString("(detached)")
	case s.Branch != "":
		b.WriteString(s.Branch)
	default:
		b.WriteString("(unknown)")
	}
	if s.Head != "" {
		b.WriteString("@" + s.Head)
	} else if s.Initial {
		b.WriteString(" (no commits)")
	}
	if s.Ahead > 0 {
		fmt.Fprintf(&b, " ↑%d", s.Ahead)
	}
	if s.Behind > 0 {
		fmt.Fprintf(&b, " ↓%d", s.Behind)
	}
	if s.UpstreamGone {
		b.WriteString(" (upstream gone)")
	}
	if s.Clean() {
		b.WriteString(" · clean")
		return b.String()
	}
	var parts []string
	add := func(n int, label string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, label))
		}
	}
	add(s.Conflicted, "conflicted")
	add(s.Staged, "staged")
	add(s.Unstaged, "modified")
	add(s.Untracked, "untracked")
	b.WriteString(" · " + strings.Join(parts, ", "))
	return b.String()
}

// Info reads branch, HEAD, upstream and file-state information for the
// repository containing root. It does not take the index lock, so it is safe
// to call while other git processes run.
func Info(ctx context.Context, root string) (*Status, error) {
	if err := checkRoot(root); err != nil {
		return nil, err
	}
	res, err := run(ctx, root, runOpts{env: []string{"GIT_OPTIONAL_LOCKS=0"}},
		"status", "--porcelain=v2", "--branch", "-z", "--untracked-files=all", "--ignore-submodules=dirty")
	if err != nil {
		return nil, err
	}
	st, err := parseStatus(res.stdout)
	if err != nil {
		return nil, err
	}
	if st.HeadFull != "" {
		short, err := output(ctx, root, "rev-parse", "--short", st.HeadFull)
		if err != nil || short == "" {
			short = st.HeadFull[:min(7, len(st.HeadFull))]
		}
		st.Head = short
	}
	return st, nil
}

// parseStatus parses `git status --porcelain=v2 --branch -z` output.
func parseStatus(data []byte) (*Status, error) {
	st := &Status{}
	recs := bytes.Split(data, []byte{0})
	for i := 0; i < len(recs); i++ {
		rec := string(recs[i])
		if rec == "" {
			continue
		}
		switch rec[0] {
		case '#':
			parseBranchHeader(st, rec)
		case '1':
			// 1 XY sub mH mI mW hH hI path
			f := strings.SplitN(rec, " ", 9)
			if len(f) != 9 || len(f[1]) != 2 {
				return nil, fmt.Errorf("gitx: malformed status entry %q", rec)
			}
			st.addEntry(StatusEntry{Kind: EntryChanged, Path: f[8], Index: f[1][0], Worktree: f[1][1]})
		case '2':
			// 2 XY sub mH mI mW hH hI Xscore path NUL origPath
			f := strings.SplitN(rec, " ", 10)
			if len(f) != 10 || len(f[1]) != 2 || i+1 >= len(recs) || len(recs[i+1]) == 0 {
				return nil, fmt.Errorf("gitx: malformed rename entry %q", rec)
			}
			i++
			st.addEntry(StatusEntry{Kind: EntryRenamed, Path: f[9], OrigPath: string(recs[i]), Index: f[1][0], Worktree: f[1][1]})
		case 'u':
			// u XY sub m1 m2 m3 mW h1 h2 h3 path
			f := strings.SplitN(rec, " ", 11)
			if len(f) != 11 || len(f[1]) != 2 {
				return nil, fmt.Errorf("gitx: malformed unmerged entry %q", rec)
			}
			st.addEntry(StatusEntry{Kind: EntryUnmerged, Path: f[10], Index: f[1][0], Worktree: f[1][1]})
		case '?':
			if len(rec) < 3 {
				return nil, fmt.Errorf("gitx: malformed untracked entry %q", rec)
			}
			st.addEntry(StatusEntry{Kind: EntryUntracked, Path: rec[2:], Index: '?', Worktree: '?'})
		case '!':
			// Ignored files are not requested; tolerate them anyway.
		default:
			return nil, fmt.Errorf("gitx: unexpected status record %q", rec)
		}
	}
	return st, nil
}

func parseBranchHeader(st *Status, rec string) {
	f := strings.Fields(rec)
	if len(f) < 3 {
		return
	}
	switch f[1] {
	case "branch.oid":
		if f[2] == "(initial)" {
			st.Initial = true
		} else {
			st.HeadFull = f[2]
		}
	case "branch.head":
		if f[2] == "(detached)" {
			st.Detached = true
		} else {
			st.Branch = strings.Join(f[2:], " ")
		}
	case "branch.upstream":
		st.Upstream = f[2]
		// Until branch.ab arrives the upstream is presumed missing; git
		// omits branch.ab when the configured upstream no longer exists.
		st.UpstreamGone = true
	case "branch.ab":
		if len(f) >= 4 {
			st.Ahead, _ = strconv.Atoi(strings.TrimPrefix(f[2], "+"))
			st.Behind, _ = strconv.Atoi(strings.TrimPrefix(f[3], "-"))
			st.UpstreamGone = false
		}
	}
}

func (st *Status) addEntry(e StatusEntry) {
	st.Entries = append(st.Entries, e)
	switch e.Kind {
	case EntryUntracked:
		st.Untracked++
	case EntryUnmerged:
		st.Conflicted++
	default:
		if e.Staged() {
			st.Staged++
		}
		if e.Unstaged() {
			st.Unstaged++
		}
	}
}
