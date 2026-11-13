package checkpoint

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Action is what restoring a checkpoint does to one file.
type Action string

const (
	// ActionRestore overwrites the file with its previous content.
	ActionRestore Action = "restore"
	// ActionRecreate writes back a file that has been deleted since.
	ActionRecreate Action = "recreate"
	// ActionDelete removes a file that did not exist before.
	ActionDelete Action = "delete"
	// ActionUnchanged means the file already matches its previous state.
	ActionUnchanged Action = "unchanged"
	// ActionSkip means the previous content was not snapshotted.
	ActionSkip Action = "skip"
)

// Change is the planned effect of a restore on one file.
type Change struct {
	Path   string
	Action Action
	// Reason explains a skip.
	Reason string
	// Diff is a unified diff from the current content to the restored
	// content; empty when not applicable (binary, unchanged, skipped).
	Diff string
}

// Plan previews a restore without changing anything.
type Plan struct {
	// Checkpoints are the ids that would be rewound, newest first.
	Checkpoints []int
	// Changes lists every affected file, sorted by path.
	Changes []Change
}

// String renders the plan as a short summary followed by the diffs.
func (p *Plan) String() string {
	var b strings.Builder
	ids := make([]string, len(p.Checkpoints))
	for i, id := range p.Checkpoints {
		ids[i] = fmt.Sprintf("#%d", id)
	}
	fmt.Fprintf(&b, "Rewinding checkpoint %s:\n", strings.Join(ids, ", "))
	for _, c := range p.Changes {
		line := fmt.Sprintf("  %-9s %s", c.Action, c.Path)
		if c.Reason != "" {
			line += " (" + c.Reason + ")"
		}
		b.WriteString(line + "\n")
	}
	for _, c := range p.Changes {
		if c.Diff != "" {
			b.WriteString("\n")
			b.WriteString(c.Diff)
		}
	}
	return b.String()
}

// SkippedFile is a file a restore could not bring back.
type SkippedFile struct {
	Path   string
	Reason string
}

// Result reports what a restore did.
type Result struct {
	// Checkpoints are the ids rewound, newest first.
	Checkpoints []int
	// Restored lists files written back with their previous content
	// (including files that had been deleted since).
	Restored []string
	// Deleted lists files removed because they did not exist before.
	Deleted []string
	// Unchanged lists files that already matched their previous state.
	Unchanged []string
	// Skipped lists files whose previous content was never snapshotted.
	Skipped []SkippedFile
}

// String renders a one-line summary such as
// "undid checkpoint #3: restored 2 files, deleted 1, 1 skipped".
func (r *Result) String() string {
	ids := make([]string, len(r.Checkpoints))
	for i, id := range r.Checkpoints {
		ids[i] = fmt.Sprintf("#%d", id)
	}
	word := "checkpoint"
	if len(ids) > 1 {
		word = "checkpoints"
	}
	parts := []string{fmt.Sprintf("restored %d %s", len(r.Restored), plural(len(r.Restored), "file"))}
	if len(r.Deleted) > 0 {
		parts = append(parts, fmt.Sprintf("deleted %d", len(r.Deleted)))
	}
	if len(r.Unchanged) > 0 {
		parts = append(parts, fmt.Sprintf("%d already unchanged", len(r.Unchanged)))
	}
	if len(r.Skipped) > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", len(r.Skipped)))
	}
	return fmt.Sprintf("undid %s %s: %s", word, strings.Join(ids, ", "), strings.Join(parts, ", "))
}

func plural(n int, w string) string {
	if n == 1 {
		return w
	}
	return w + "s"
}

// PreviewUndo previews Undo.
func (s *Store) PreviewUndo() (*Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.idx.Checkpoints) == 0 {
		return nil, ErrNothingToUndo
	}
	return s.plan(len(s.idx.Checkpoints) - 1)
}

// Preview previews Restore(id).
func (s *Store) Preview(id int) (*Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, err := s.position(id)
	if err != nil {
		return nil, err
	}
	return s.plan(i)
}

// Undo restores the most recent checkpoint and removes it. It returns
// ErrNothingToUndo when there are no checkpoints.
func (s *Store) Undo() (*Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.idx.Checkpoints) == 0 {
		return nil, ErrNothingToUndo
	}
	return s.restoreFrom(len(s.idx.Checkpoints) - 1)
}

// Restore rewinds checkpoint id and every later one, bringing each file
// back to its state before the earliest of them changed it, and removes
// those checkpoints. If any file fails to restore, the checkpoints are kept
// so the restore can be retried; files already restored stay restored.
func (s *Store) Restore(id int) (*Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, err := s.position(id)
	if err != nil {
		return nil, err
	}
	return s.restoreFrom(i)
}

func (s *Store) position(id int) (int, error) {
	for i, c := range s.idx.Checkpoints {
		if c.ID == id {
			return i, nil
		}
	}
	return 0, fmt.Errorf("%w: %d", ErrNotFound, id)
}

// targets maps each file touched by checkpoints[from:] to its snapshot in
// the earliest of them, which is the state to go back to.
func (s *Store) targets(from int) ([]int, []FileSnapshot) {
	seen := map[string]bool{}
	var snaps []FileSnapshot
	var ids []int
	for _, c := range s.idx.Checkpoints[from:] {
		ids = append(ids, c.ID)
		for _, f := range c.Files {
			if !seen[f.Path] {
				seen[f.Path] = true
				snaps = append(snaps, f)
			}
		}
	}
	sort.Slice(snaps, func(a, b int) bool { return snaps[a].Path < snaps[b].Path })
	// Newest first reads naturally in reports.
	for l, r := 0, len(ids)-1; l < r; l, r = l+1, r-1 {
		ids[l], ids[r] = ids[r], ids[l]
	}
	return ids, snaps
}

// current describes what is on disk now.
type current struct {
	exists  bool
	dir     bool
	symlink bool
	target  string
	regular bool
	mode    fs.FileMode
	data    []byte // nil when not read (too large, not regular)
}

func (s *Store) readCurrent(path string) (current, error) {
	st, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return current{}, nil
	}
	if err != nil {
		return current{}, err
	}
	c := current{exists: true, dir: st.IsDir(), mode: st.Mode().Perm()}
	switch {
	case st.Mode()&fs.ModeSymlink != 0:
		c.symlink = true
		c.target, err = os.Readlink(path)
		return c, err
	case st.Mode().IsRegular():
		c.regular = true
		if st.Size() <= s.opts.MaxFileSize {
			c.data, err = os.ReadFile(path)
			if c.data == nil && err == nil {
				c.data = []byte{}
			}
		}
	}
	return c, err
}

// change works out the action for one snapshot, loading the previous
// content when it will be written back.
func (s *Store) change(snap FileSnapshot, withDiff bool) (Change, []byte, error) {
	ch := Change{Path: snap.Path}
	if snap.Skipped != "" {
		ch.Action, ch.Reason = ActionSkip, snap.Skipped
		return ch, nil, nil
	}
	cur, err := s.readCurrent(snap.Path)
	if err != nil {
		return ch, nil, fmt.Errorf("checkpoint: %w", err)
	}
	if cur.dir {
		return ch, nil, fmt.Errorf("checkpoint: %s is now a directory; not touched", snap.Path)
	}
	name := snap.Path
	switch {
	case !snap.Existed:
		if !cur.exists {
			ch.Action = ActionUnchanged
			return ch, nil, nil
		}
		ch.Action = ActionDelete
		if withDiff && cur.data != nil && !isBinary(cur.data) {
			ch.Diff = unifiedDiff(name, "/dev/null", string(cur.data), "")
		}
		return ch, nil, nil

	case snap.Symlink:
		if cur.symlink && cur.target == snap.LinkTarget {
			ch.Action = ActionUnchanged
			return ch, nil, nil
		}
		ch.Action = ActionRestore
		if !cur.exists {
			ch.Action = ActionRecreate
		}
		ch.Reason = "symbolic link to " + snap.LinkTarget
		return ch, nil, nil
	}

	prev, err := s.getBlob(snap.Blob)
	if err != nil {
		return ch, nil, fmt.Errorf("%w (%s)", err, snap.Path)
	}
	if cur.regular && cur.data != nil && bytes.Equal(cur.data, prev) && cur.mode == snap.Mode {
		ch.Action = ActionUnchanged
		return ch, nil, nil
	}
	ch.Action = ActionRestore
	if !cur.exists {
		ch.Action = ActionRecreate
	}
	if withDiff {
		switch {
		case !cur.exists || cur.symlink:
			ch.Diff = unifiedDiff("/dev/null", name, "", string(prev))
		case cur.data == nil:
			ch.Reason = "current file is too large to diff"
		case isBinary(cur.data):
			ch.Reason = "current file is binary"
		case bytes.Equal(cur.data, prev):
			ch.Reason = fmt.Sprintf("mode %v → %v", cur.mode, snap.Mode)
		default:
			ch.Diff = unifiedDiff(name, name, string(cur.data), string(prev))
		}
	}
	return ch, prev, nil
}

func (s *Store) plan(from int) (*Plan, error) {
	ids, snaps := s.targets(from)
	p := &Plan{Checkpoints: ids}
	for _, snap := range snaps {
		ch, _, err := s.change(snap, true)
		if err != nil {
			return nil, err
		}
		p.Changes = append(p.Changes, ch)
	}
	return p, nil
}

// restoreFrom rewinds checkpoints[from:]. Callers hold s.mu.
func (s *Store) restoreFrom(from int) (*Result, error) {
	ids, snaps := s.targets(from)
	res := &Result{Checkpoints: ids}
	var errs []error
	for _, snap := range snaps {
		if err := s.apply(snap, res); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return res, fmt.Errorf("checkpoint: restore incomplete, checkpoints kept so it can be retried: %w", errors.Join(errs...))
	}
	removed := s.idx.Checkpoints[from:]
	for _, c := range removed {
		if c == s.cur {
			// Later edits in the same turn start a fresh checkpoint.
			s.cur, s.recorded = nil, nil
		}
	}
	s.idx.Checkpoints = append([]*Checkpoint(nil), s.idx.Checkpoints[:from]...)
	if err := s.saveIndex(); err != nil {
		s.idx.Checkpoints = append(s.idx.Checkpoints, removed...)
		return res, err
	}
	s.gcBlobs()
	return res, nil
}

// apply restores one file and records the outcome in res.
func (s *Store) apply(snap FileSnapshot, res *Result) error {
	ch, prev, err := s.change(snap, false)
	if err != nil {
		return err
	}
	switch ch.Action {
	case ActionSkip:
		res.Skipped = append(res.Skipped, SkippedFile{Path: snap.Path, Reason: snap.Skipped})
		return nil
	case ActionUnchanged:
		if !snap.Existed {
			removeEmptyDirs(snap.CreatedDirs)
		}
		res.Unchanged = append(res.Unchanged, snap.Path)
		return nil
	case ActionDelete:
		if err := os.Remove(snap.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("checkpoint: deleting %s: %w", snap.Path, err)
		}
		removeEmptyDirs(snap.CreatedDirs)
		res.Deleted = append(res.Deleted, snap.Path)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(snap.Path), 0o755); err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	if snap.Symlink {
		if err := symlinkAtomic(snap.LinkTarget, snap.Path); err != nil {
			return fmt.Errorf("checkpoint: restoring link %s: %w", snap.Path, err)
		}
	} else if err := writeFileAtomic(snap.Path, prev, snap.Mode); err != nil {
		return fmt.Errorf("checkpoint: restoring %s: %w", snap.Path, err)
	}
	res.Restored = append(res.Restored, snap.Path)
	return nil
}

// symlinkAtomic creates a link at a temporary name and renames it into
// place, replacing whatever is there.
func symlinkAtomic(target, path string) error {
	for i := 0; ; i++ {
		tmp := filepath.Join(filepath.Dir(path), fmt.Sprintf(".%s.ckpt-link-%d-%d", filepath.Base(path), os.Getpid(), i))
		err := os.Symlink(target, tmp)
		if errors.Is(err, fs.ErrExist) && i < 100 {
			continue
		}
		if err != nil {
			return err
		}
		if err := os.Rename(tmp, path); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		return nil
	}
}

// removeEmptyDirs removes directories (deepest first) that are empty; a
// directory that still has content stops the walk.
func removeEmptyDirs(dirs []string) {
	for _, d := range dirs {
		if err := os.Remove(d); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return
		}
	}
}
