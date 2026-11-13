// Package checkpoint gives the agent undo for its file edits, with or
// without git.
//
// Before a tool modifies a file, the caller records the file's current
// state with Store.Record. Records are grouped into numbered checkpoints,
// normally one per user turn (Store.Begin). Undo rewinds the most recent
// checkpoint; Restore rewinds every checkpoint from a given one onwards.
// Previous contents are restored atomically (temporary file and rename) with
// their file modes; files the agent created are deleted again, along with
// any directories created for them that are left empty.
//
// Snapshots live on disk in a directory given to Open, so undo survives a
// restart. File contents are stored once per distinct content (keyed by
// SHA-256) and verified on restore. Files over the size limit (5 MB by
// default) and binary files are not copied; they are recorded as skipped
// and reported, never silently ignored.
//
// A Store is safe for concurrent use by multiple goroutines. One directory
// must be used by a single Store (one process) at a time.
package checkpoint

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

const (
	// DefaultMaxFileSize is the largest file whose content is snapshotted.
	DefaultMaxFileSize = 5 << 20
	// DefaultMaxCheckpoints is how many checkpoints are kept; older ones
	// are discarded together with content no other checkpoint uses.
	DefaultMaxCheckpoints = 100

	indexName     = "index.json"
	blobDirName   = "blobs"
	formatVersion = 1
)

var (
	// ErrNothingToUndo is returned by Undo when there are no checkpoints.
	ErrNothingToUndo = errors.New("nothing to undo")
	// ErrNotFound is returned for an unknown checkpoint id.
	ErrNotFound = errors.New("checkpoint not found")
)

// Options tunes a Store. Zero values select the defaults.
type Options struct {
	// MaxFileSize is the largest file, in bytes, whose content is kept.
	MaxFileSize int64
	// MaxCheckpoints is how many checkpoints are retained.
	MaxCheckpoints int
	// Now returns the current time; tests may replace it.
	Now func() time.Time
}

// FileSnapshot is the state of one file before the first change in a
// checkpoint.
type FileSnapshot struct {
	// Path is the absolute, cleaned file path.
	Path string `json:"path"`
	// Existed is false when the file did not exist, so undo deletes it.
	Existed bool `json:"existed"`
	// Mode holds the permission bits of an existing regular file.
	Mode fs.FileMode `json:"mode,omitempty"`
	// Size is the file size in bytes.
	Size int64 `json:"size,omitempty"`
	// Blob is the SHA-256 of the stored content.
	Blob string `json:"blob,omitempty"`
	// Symlink is true when the path was a symbolic link to LinkTarget.
	Symlink    bool   `json:"symlink,omitempty"`
	LinkTarget string `json:"link_target,omitempty"`
	// CreatedDirs lists parent directories that did not exist yet, deepest
	// first; undo removes them again if they are empty.
	CreatedDirs []string `json:"created_dirs,omitempty"`
	// Skipped explains why the content was not stored (too large, binary,
	// not a regular file); such a file cannot be restored.
	Skipped string `json:"skipped,omitempty"`
}

// Checkpoint is one group of file snapshots, normally one user turn.
type Checkpoint struct {
	ID    int            `json:"id"`
	Label string         `json:"label"`
	Time  time.Time      `json:"time"`
	Files []FileSnapshot `json:"files"`
}

// Summary describes a checkpoint for listing.
type Summary struct {
	ID    int
	Label string
	Time  time.Time
	// Files are the paths changed in the checkpoint, sorted.
	Files []string
	// Created counts files that did not exist before the checkpoint.
	Created int
	// Skipped lists files whose content could not be snapshotted.
	Skipped []string
}

type index struct {
	Version     int           `json:"version"`
	NextID      int           `json:"next_id"`
	Checkpoints []*Checkpoint `json:"checkpoints"`
}

// Store records file snapshots and restores them.
type Store struct {
	dir  string
	opts Options

	mu  sync.Mutex
	idx index
	// cur is the open checkpoint that Record adds to; nil until the first
	// Record after Begin (empty turns leave no checkpoint behind).
	cur      *Checkpoint
	recorded map[string]bool
	label    string
	began    time.Time
}

var sessionID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// SessionDir returns the conventional snapshot directory for a session:
// <base>/checkpoints/<sessionID>. The id must be a plain file name.
func SessionDir(base, id string) (string, error) {
	if !sessionID.MatchString(id) {
		return "", fmt.Errorf("checkpoint: invalid session id %q", id)
	}
	return filepath.Join(base, "checkpoints", id), nil
}

// Open opens or creates the store in dir, loading any checkpoints saved by
// an earlier process.
func Open(dir string, opts Options) (*Store, error) {
	if dir == "" {
		return nil, errors.New("checkpoint: directory is empty")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: %w", err)
	}
	if opts.MaxFileSize <= 0 {
		opts.MaxFileSize = DefaultMaxFileSize
	}
	if opts.MaxCheckpoints <= 0 {
		opts.MaxCheckpoints = DefaultMaxCheckpoints
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if err := os.MkdirAll(filepath.Join(abs, blobDirName), 0o700); err != nil {
		return nil, fmt.Errorf("checkpoint: %w", err)
	}
	s := &Store{dir: abs, opts: opts, idx: index{Version: formatVersion, NextID: 1}}
	data, err := os.ReadFile(filepath.Join(abs, indexName))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("checkpoint: %w", err)
	default:
		var idx index
		if err := json.Unmarshal(data, &idx); err != nil {
			return nil, fmt.Errorf("checkpoint: %s is corrupt: %w", filepath.Join(abs, indexName), err)
		}
		if idx.Version > formatVersion {
			return nil, fmt.Errorf("checkpoint: %s was written by a newer version (format %d)", abs, idx.Version)
		}
		idx.Version = formatVersion
		for _, c := range idx.Checkpoints {
			if c == nil {
				return nil, fmt.Errorf("checkpoint: %s is corrupt: null checkpoint", filepath.Join(abs, indexName))
			}
			idx.NextID = max(idx.NextID, c.ID+1)
		}
		idx.NextID = max(idx.NextID, 1)
		s.idx = idx
	}
	return s, nil
}

// Dir returns the directory the store keeps its data in.
func (s *Store) Dir() string { return s.dir }

// Begin starts a new checkpoint with label (typically the user's prompt).
// The checkpoint is created by the first Record that follows, so a turn
// that changes no files leaves no empty checkpoint.
func (s *Store) Begin(label string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cur = nil
	s.recorded = nil
	s.label = label
	s.began = s.opts.Now()
}

// Record snapshots path before it is modified. Only the first call per path
// in a checkpoint stores anything; later calls are no-ops, so the snapshot
// is the state before the checkpoint's first change. Without a preceding
// Begin a checkpoint is started automatically.
//
// A non-empty warning means the file is listed but its content was not
// stored (too large, binary or not a regular file), so undo cannot restore
// it; callers should show the warning to the user. An error means nothing
// was recorded and the caller should not proceed silently.
func (s *Store) Record(path string) (warning string, err error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("checkpoint: path %q is not absolute", path)
	}
	path = filepath.Clean(path)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur != nil && s.recorded[path] {
		return "", nil
	}
	snap, err := s.snapshot(path)
	if err != nil {
		return "", err
	}

	created := false
	if s.cur == nil {
		label, when := s.label, s.began
		if when.IsZero() {
			when = s.opts.Now()
		}
		if label == "" {
			label = fmt.Sprintf("checkpoint %d", s.idx.NextID)
		}
		s.cur = &Checkpoint{ID: s.idx.NextID, Label: label, Time: when}
		s.recorded = map[string]bool{}
		s.idx.NextID++
		s.idx.Checkpoints = append(s.idx.Checkpoints, s.cur)
		created = true
	}
	s.cur.Files = append(s.cur.Files, snap)
	if err := s.saveIndex(); err != nil {
		// Roll back so memory matches disk.
		s.cur.Files = s.cur.Files[:len(s.cur.Files)-1]
		if created {
			s.idx.Checkpoints = s.idx.Checkpoints[:len(s.idx.Checkpoints)-1]
			s.idx.NextID--
			s.cur, s.recorded = nil, nil
		}
		return "", err
	}
	s.recorded[path] = true

	if len(s.idx.Checkpoints) > s.opts.MaxCheckpoints {
		drop := len(s.idx.Checkpoints) - s.opts.MaxCheckpoints
		s.idx.Checkpoints = append([]*Checkpoint(nil), s.idx.Checkpoints[drop:]...)
		if err := s.saveIndex(); err == nil {
			s.gcBlobs()
		}
	}
	if snap.Skipped != "" {
		return fmt.Sprintf("%s was not snapshotted (%s); undo will not be able to restore it", path, snap.Skipped), nil
	}
	return "", nil
}

// snapshot captures the current state of path.
func (s *Store) snapshot(path string) (FileSnapshot, error) {
	snap := FileSnapshot{Path: path}
	st, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		snap.CreatedDirs = missingDirs(filepath.Dir(path))
		return snap, nil
	}
	if err != nil {
		return snap, fmt.Errorf("checkpoint: %w", err)
	}
	snap.Existed = true
	snap.Size = st.Size()
	switch {
	case st.IsDir():
		return snap, fmt.Errorf("checkpoint: %s is a directory", path)
	case st.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return snap, fmt.Errorf("checkpoint: %w", err)
		}
		snap.Symlink, snap.LinkTarget = true, target
		return snap, nil
	case !st.Mode().IsRegular():
		snap.Skipped = "not a regular file"
		return snap, nil
	}
	snap.Mode = st.Mode().Perm()
	if st.Size() > s.opts.MaxFileSize {
		snap.Skipped = fmt.Sprintf("larger than %s (%s)", formatSize(s.opts.MaxFileSize), formatSize(st.Size()))
		return snap, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return snap, fmt.Errorf("checkpoint: %w", err)
	}
	snap.Size = int64(len(data))
	if isBinary(data) {
		snap.Skipped = "binary file"
		return snap, nil
	}
	sum, err := s.putBlob(data)
	if err != nil {
		return snap, err
	}
	snap.Blob = sum
	return snap, nil
}

// missingDirs lists dir and its ancestors that do not exist, deepest first.
func missingDirs(dir string) []string {
	var out []string
	for {
		if _, err := os.Lstat(dir); err == nil || !errors.Is(err, fs.ErrNotExist) {
			return out
		}
		out = append(out, dir)
		parent := filepath.Dir(dir)
		if parent == dir {
			return out
		}
		dir = parent
	}
}

// List returns all checkpoints, oldest first.
func (s *Store) List() []Summary {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Summary, 0, len(s.idx.Checkpoints))
	for _, c := range s.idx.Checkpoints {
		sum := Summary{ID: c.ID, Label: c.Label, Time: c.Time}
		for _, f := range c.Files {
			sum.Files = append(sum.Files, f.Path)
			if !f.Existed {
				sum.Created++
			}
			if f.Skipped != "" {
				sum.Skipped = append(sum.Skipped, f.Path)
			}
		}
		sort.Strings(sum.Files)
		out = append(out, sum)
	}
	return out
}

// Get returns a copy of the checkpoint with id.
func (s *Store) Get(id int) (Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.idx.Checkpoints {
		if c.ID == id {
			cp := *c
			cp.Files = append([]FileSnapshot(nil), c.Files...)
			return cp, nil
		}
	}
	return Checkpoint{}, fmt.Errorf("%w: %d", ErrNotFound, id)
}

// Len returns the number of checkpoints.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.idx.Checkpoints)
}

// Clear discards every checkpoint and all stored content.
func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.idx.Checkpoints = nil
	s.cur, s.recorded = nil, nil
	if err := s.saveIndex(); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(s.dir, blobDirName)); err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(s.dir, blobDirName), 0o700); err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	return nil
}

// saveIndex writes the index atomically. Callers hold s.mu.
func (s *Store) saveIndex() error {
	data, err := json.MarshalIndent(&s.idx, "", " ")
	if err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(s.dir, indexName), data, 0o600); err != nil {
		return fmt.Errorf("checkpoint: saving index: %w", err)
	}
	return nil
}

func (s *Store) blobPath(sum string) string {
	return filepath.Join(s.dir, blobDirName, sum[:2], sum)
}

// putBlob stores data under its SHA-256 unless already present.
func (s *Store) putBlob(data []byte) (string, error) {
	h := sha256.Sum256(data)
	sum := hex.EncodeToString(h[:])
	p := s.blobPath(sum)
	if _, err := os.Stat(p); err == nil {
		return sum, nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", fmt.Errorf("checkpoint: %w", err)
	}
	if err := writeFileAtomic(p, data, 0o600); err != nil {
		return "", fmt.Errorf("checkpoint: storing snapshot: %w", err)
	}
	return sum, nil
}

// getBlob reads stored content and verifies its checksum.
func (s *Store) getBlob(sum string) ([]byte, error) {
	if len(sum) != sha256.Size*2 {
		return nil, fmt.Errorf("checkpoint: invalid snapshot id %q", sum)
	}
	data, err := os.ReadFile(s.blobPath(sum))
	if err != nil {
		return nil, fmt.Errorf("checkpoint: snapshot content missing: %w", err)
	}
	h := sha256.Sum256(data)
	if hex.EncodeToString(h[:]) != sum {
		return nil, fmt.Errorf("checkpoint: snapshot content %s is corrupt", sum[:12])
	}
	return data, nil
}

// gcBlobs deletes stored content no checkpoint refers to. It is best
// effort: a leftover file only costs disk space. Callers hold s.mu.
func (s *Store) gcBlobs() {
	used := map[string]bool{}
	for _, c := range s.idx.Checkpoints {
		for _, f := range c.Files {
			if f.Blob != "" {
				used[f.Blob] = true
			}
		}
	}
	root := filepath.Join(s.dir, blobDirName)
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !used[d.Name()] {
			_ = os.Remove(p)
			_ = os.Remove(filepath.Dir(p)) // only succeeds once empty
		}
		return nil
	})
}

// writeFileAtomic writes data to a temporary file in the same directory and
// renames it over path, so readers never see a partial file.
func writeFileAtomic(path string, data []byte, mode fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".ckpt-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	ok = true
	return nil
}

func isBinary(b []byte) bool {
	return bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0
}

func formatSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}
