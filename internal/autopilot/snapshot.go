package autopilot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

// SnapshotOptions bound the cost of a workspace snapshot.
type SnapshotOptions struct {
	// MaxFiles stops the walk after this many files (default 20000).
	MaxFiles int
	// MaxFileBytes is the largest file whose content is kept for diffing
	// (default 1 MiB); larger files are tracked by hash only.
	MaxFileBytes int64
	// MaxTotalBytes caps the content kept across all files (default 64 MiB).
	MaxTotalBytes int64
}

func (o SnapshotOptions) withDefaults() SnapshotOptions {
	if o.MaxFiles <= 0 {
		o.MaxFiles = 20000
	}
	if o.MaxFileBytes <= 0 {
		o.MaxFileBytes = 1 << 20
	}
	if o.MaxTotalBytes <= 0 {
		o.MaxTotalBytes = 64 << 20
	}
	return o
}

// snapshotSkipDirs are never walked: VCS metadata, dependencies, caches
// and build output.
var snapshotSkipDirs = map[string]bool{
	".git": true, ".hg": true, ".svn": true, "node_modules": true, "vendor": true, ".venv": true, "venv": true,
	"__pycache__": true, ".pytest_cache": true, ".mypy_cache": true, ".ruff_cache": true, ".tox": true,
	".idea": true, ".vscode": true, ".next": true, ".nuxt": true, "dist": true, "build": true, "target": true,
	".cache": true, ".gradle": true, ".terraform": true, "coverage": true, ".gta": true,
}

type fileState struct {
	size    int64
	hash    [32]byte
	content []byte // nil when not kept
	kept    bool
}

// Snapshot records the state of a workspace's files so the changes made
// afterwards can be diffed, independent of git and including files changed
// by shell commands.
type Snapshot struct {
	root  string
	opts  SnapshotOptions
	files map[string]fileState
	// Incomplete is true when the walk stopped at MaxFiles.
	Incomplete bool
}

// TakeSnapshot walks root and records every regular file.
func TakeSnapshot(ctx context.Context, root string, opts SnapshotOptions) (*Snapshot, error) {
	opts = opts.withDefaults()
	s := &Snapshot{root: root, opts: opts, files: map[string]fileState{}}
	var kept int64
	err := walkWorkspace(ctx, root, opts.MaxFiles, func(rel string, data []byte) {
		st := fileState{size: int64(len(data)), hash: sha256.Sum256(data)}
		if int64(len(data)) <= opts.MaxFileBytes && kept+int64(len(data)) <= opts.MaxTotalBytes {
			st.content, st.kept = data, true
			kept += int64(len(data))
		}
		s.files[rel] = st
	})
	if errors.Is(err, errWalkLimit) {
		s.Incomplete = true
		err = nil
	}
	if err != nil {
		return nil, err
	}
	return s, nil
}

var errWalkLimit = errors.New("file limit reached")

// walkWorkspace calls fn for every regular file under root, with its
// slash-separated relative path and content (at most maxHashedFile bytes).
// Symlinks, devices and skipped directories are ignored. It returns
// errWalkLimit when more than maxFiles files exist.
func walkWorkspace(ctx context.Context, root string, maxFiles int, fn func(rel string, data []byte)) error {
	n := 0
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			return nil // unreadable entries are skipped, not fatal
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if d.IsDir() {
			if path != root && snapshotSkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if n >= maxFiles {
			return errWalkLimit
		}
		n++
		data, rerr := readCapped(path, maxHashedFile)
		if rerr != nil {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		fn(filepath.ToSlash(rel), data)
		return nil
	})
}

// maxHashedFile bounds how much of one file is read; larger files are
// compared by their first 32 MiB plus size.
const maxHashedFile = 32 << 20

func readCapped(path string, n int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var buf bytes.Buffer
	_, err = buf.ReadFrom(io.LimitReader(f, n))
	return buf.Bytes(), err
}

// ChangeKind says how a file changed.
type ChangeKind string

const (
	ChangeAdded    ChangeKind = "added"
	ChangeModified ChangeKind = "modified"
	ChangeDeleted  ChangeKind = "deleted"
)

// Change is one changed file.
type Change struct {
	Path    string     `json:"path"`
	Kind    ChangeKind `json:"kind"`
	Added   int        `json:"added"`
	Deleted int        `json:"deleted"`
	Binary  bool       `json:"binary,omitempty"`
	// Note explains a change whose content could not be diffed.
	Note string `json:"note,omitempty"`
	// Patch is the unified diff for the file, when available.
	Patch string `json:"-"`
}

// ChangeSet is the set of changes made since a snapshot.
type ChangeSet struct {
	Files []Change `json:"files"`
	// Text is the combined unified diff within the budget.
	Text string `json:"-"`
	// Truncated is true when some patches did not fit the budget.
	Truncated bool `json:"truncated,omitempty"`
	// Incomplete is true when the snapshot hit its file limit, so changes
	// to files beyond it may be missing.
	Incomplete bool `json:"incomplete,omitempty"`
}

// Empty reports whether nothing changed.
func (c *ChangeSet) Empty() bool { return c == nil || len(c.Files) == 0 }

// Paths lists the changed paths.
func (c *ChangeSet) Paths() []string {
	if c == nil {
		return nil
	}
	out := make([]string, len(c.Files))
	for i, f := range c.Files {
		out[i] = f.Path
	}
	return out
}

// Stat renders a per-file summary.
func (c *ChangeSet) Stat() string {
	if c.Empty() {
		return "no changes"
	}
	var b strings.Builder
	added, deleted := 0, 0
	for _, f := range c.Files {
		letter := map[ChangeKind]string{ChangeAdded: "A", ChangeModified: "M", ChangeDeleted: "D"}[f.Kind]
		if f.Binary {
			fmt.Fprintf(&b, "%s  %s  (binary)\n", letter, f.Path)
		} else {
			fmt.Fprintf(&b, "%s  %s  +%d -%d\n", letter, f.Path, f.Added, f.Deleted)
		}
		added += f.Added
		deleted += f.Deleted
	}
	fmt.Fprintf(&b, "%d file%s changed, %d insertion%s(+), %d deletion%s(-)", len(c.Files), plural(len(c.Files)), added, plural(added), deleted, plural(deleted))
	return b.String()
}

// DefaultDiffBudget is the default byte budget of ChangeSet.Text.
const DefaultDiffBudget = 60000

// Changes walks the workspace again and diffs it against the snapshot.
// budget caps ChangeSet.Text (zero means DefaultDiffBudget).
func (s *Snapshot) Changes(ctx context.Context, budget int) (*ChangeSet, error) {
	if budget <= 0 {
		budget = DefaultDiffBudget
	}
	cs := &ChangeSet{Incomplete: s.Incomplete}
	present := map[string]bool{}
	changed := map[string][]byte{}
	err := walkWorkspace(ctx, s.root, s.opts.MaxFiles, func(rel string, data []byte) {
		present[rel] = true
		old, ok := s.files[rel]
		if ok && old.size == int64(len(data)) && old.hash == sha256.Sum256(data) {
			return
		}
		changed[rel] = data
	})
	if errors.Is(err, errWalkLimit) {
		cs.Incomplete = true
		err = nil
	}
	if err != nil {
		return nil, err
	}
	var paths []string
	for rel := range changed {
		paths = append(paths, rel)
	}
	for rel := range s.files {
		// A file missing from an incomplete walk may simply not have been
		// reached, so deletions are only reported for complete walks.
		if !present[rel] && !cs.Incomplete {
			paths = append(paths, rel)
		}
	}
	slices.Sort(paths)
	var text strings.Builder
	for _, rel := range paths {
		old, existed := s.files[rel]
		data, exists := changed[rel]
		ch := Change{Path: rel}
		var a, b string
		switch {
		case !existed:
			ch.Kind = ChangeAdded
			b = string(data)
		case !exists:
			ch.Kind = ChangeDeleted
			if !old.kept {
				ch.Note = "content was not captured (file too large)"
			}
			a = string(old.content)
		default:
			ch.Kind = ChangeModified
			if !old.kept {
				ch.Note = "previous content was not captured (file too large); diff unavailable"
			}
			a, b = string(old.content), string(data)
		}
		if isBinaryContent([]byte(a)) || isBinaryContent([]byte(b)) {
			ch.Binary = true
		} else if ch.Note == "" {
			name := rel
			if ch.Kind == ChangeAdded {
				a = ""
			}
			ch.Patch = tools.UnifiedDiff(name, a, b)
			ch.Added, ch.Deleted = countPatchLines(ch.Patch)
		}
		cs.Files = append(cs.Files, ch)
		entry := ch.Patch
		if entry == "" {
			entry = fmt.Sprintf("--- %s\n+++ %s\n(%s%s)\n", rel, rel, ch.Kind, noteSuffix(ch))
		}
		if text.Len()+len(entry) > budget {
			cs.Truncated = true
			continue
		}
		text.WriteString(entry)
	}
	if cs.Truncated {
		fmt.Fprintf(&text, "# diff truncated to %d bytes; files without a patch above: see the stat\n", budget)
	}
	cs.Text = text.String()
	return cs, nil
}

func noteSuffix(c Change) string {
	switch {
	case c.Binary:
		return ", binary"
	case c.Note != "":
		return "; " + c.Note
	}
	return ""
}

// countPatchLines counts the +/- lines of a patch, skipping its two-line
// "--- / +++" header.
func countPatchLines(patch string) (added, deleted int) {
	lines := strings.Split(patch, "\n")
	if len(lines) >= 2 && strings.HasPrefix(lines[0], "--- ") && strings.HasPrefix(lines[1], "+++ ") {
		lines = lines[2:]
	}
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "+"):
			added++
		case strings.HasPrefix(l, "-"):
			deleted++
		}
	}
	return added, deleted
}

func isBinaryContent(b []byte) bool {
	n := min(len(b), 8000)
	return bytes.IndexByte(b[:n], 0) >= 0
}
