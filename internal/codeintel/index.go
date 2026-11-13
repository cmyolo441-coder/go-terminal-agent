// Package codeintel is a local code index for the workspace: a parallel,
// gitignore-aware indexer, exact Go and structural multi-language symbol
// extraction, BM25F ranked search over identifiers, paths, doc comments and
// code, a centrality-ranked repository map, an incremental on-disk cache,
// and agent tools built on them.
package codeintel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Defaults for Options.
const (
	DefaultMaxFileSize     = 1 << 20
	DefaultMaxFiles        = 200_000
	DefaultRefreshInterval = 15 * time.Second
	// racyWindow: files modified this close to a scan are re-read on the
	// next refresh, because an edit within the same mtime tick could keep
	// size and mtime unchanged.
	racyWindow = 2 * time.Second
)

// ErrClosed is returned by an Index after Close.
var ErrClosed = errors.New("codeintel: index closed")

// Options configure an Index.
type Options struct {
	// Root is the workspace directory to index.
	Root string
	// CacheDir, when set, persists the index there between runs. One
	// directory can hold the caches of many workspaces.
	CacheDir string
	// MaxFileSize skips larger files (default 1 MiB).
	MaxFileSize int64
	// MaxFiles caps the number of files indexed (default 200000).
	MaxFiles int
	// Workers is the parallelism of walking and parsing (default
	// GOMAXPROCS).
	Workers int
	// IncludeGenerated indexes generated files and lock files too.
	IncludeGenerated bool
	// SkipGlobalGitignore ignores the user's global git excludes file.
	SkipGlobalGitignore bool
	// ExtraIgnore are additional gitignore patterns relative to Root.
	ExtraIgnore []string
	// Exclude, when set, leaves out paths it returns true for, for example
	// files a managed policy denies reading. It gets slash-separated paths
	// relative to Root, directories too (an excluded directory is not
	// descended into), and must be safe for concurrent use.
	Exclude func(rel string, dir bool) bool
	// RefreshInterval is how stale the index may get before a query
	// re-scans the tree (default 15s). Zero uses the default; a negative
	// value re-scans before every query.
	RefreshInterval time.Duration
	// Logf receives non-fatal problems such as an unreadable cache.
	Logf func(format string, args ...any)
}

// Stats describe the state of the index.
type Stats struct {
	Root      string         `json:"root"`
	Files     int            `json:"files"`
	Skipped   map[string]int `json:"skipped,omitempty"`
	Symbols   int            `json:"symbols"`
	Chunks    int            `json:"chunks"`
	Terms     int            `json:"terms"`
	Languages map[string]int `json:"languages,omitempty"`
	Truncated bool           `json:"truncated,omitempty"`
	// Parsed, Reused and Removed describe the last refresh.
	Parsed      int           `json:"parsed"`
	Reused      int           `json:"reused"`
	Removed     int           `json:"removed"`
	LastRefresh time.Time     `json:"last_refresh"`
	Duration    time.Duration `json:"duration"`
	FromCache   bool          `json:"from_cache,omitempty"`
}

// Index is a concurrent-safe code index. Readers work on immutable
// snapshots, so queries never block a refresh and never see a half-built
// index.
type Index struct {
	opts   Options
	root   string
	intern *interner

	snap      atomic.Pointer[snapshot]
	refreshMu sync.Mutex // serializes refreshes

	mu       sync.Mutex // guards the fields below
	pending  map[string]bool
	lastFull time.Time
	closed   bool
}

// snapshot is one immutable state of the index.
type snapshot struct {
	all   map[string]*fileEntry // every known file, indexed or skipped
	files []*fileEntry          // indexed files sorted by path
	stats Stats                 // Terms is filled in by Stats
	// walkSkipped are the skip counts of the last scan, before the
	// content-based skips of individual entries are added.
	walkSkipped map[string]int
	// derived holds the search index and reference graph, built on first
	// use. Snapshots with the same files share it, so a refresh that
	// changed nothing does not rebuild them.
	derived *derived
}

// derived is the data computed from a snapshot's indexed files.
type derived struct {
	files      []*fileEntry
	searchOnce sync.Once
	search     *searchIndex
	graphOnce  sync.Once
	graph      *refGraph
}

// searchIndex returns the snapshot's search index, building it on first
// use.
func (s *snapshot) searchIndex() *searchIndex {
	d := s.derived
	d.searchOnce.Do(func() { d.search = buildSearchIndex(d.files) })
	return d.search
}

// prepareSearch builds the search index in the background, so a query
// soon after startup does not wait for it.
func (s *snapshot) prepareSearch() {
	go s.searchIndex()
}

// Open prepares an index rooted at opts.Root. It loads the on-disk cache
// when one exists but does not scan; the first query or Refresh does.
func Open(opts Options) (*Index, error) {
	if strings.TrimSpace(opts.Root) == "" {
		return nil, fmt.Errorf("codeintel: root is required")
	}
	root, err := filepath.Abs(opts.Root)
	if err != nil {
		return nil, err
	}
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	st, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("codeintel: %w", err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("codeintel: %s is not a directory", root)
	}
	if opts.MaxFileSize <= 0 {
		opts.MaxFileSize = DefaultMaxFileSize
	}
	if opts.MaxFiles <= 0 {
		opts.MaxFiles = DefaultMaxFiles
	}
	if opts.Workers <= 0 {
		opts.Workers = runtime.GOMAXPROCS(0)
	}
	if opts.RefreshInterval == 0 {
		opts.RefreshInterval = DefaultRefreshInterval
	}
	opts.Root = root
	ix := &Index{opts: opts, root: root, intern: newInterner(), pending: map[string]bool{}}
	ix.loadCache()
	return ix, nil
}

// Root returns the absolute, symlink-resolved root directory.
func (ix *Index) Root() string { return ix.root }

func (ix *Index) logf(format string, args ...any) {
	if ix.opts.Logf != nil {
		ix.opts.Logf(format, args...)
	}
}

func (ix *Index) isClosed() bool {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return ix.closed
}

// Close waits for a running refresh and makes further calls fail with
// ErrClosed. The cache is written after every refresh that changed
// something, so there is nothing left to flush.
func (ix *Index) Close() error {
	ix.refreshMu.Lock()
	defer ix.refreshMu.Unlock()
	ix.mu.Lock()
	if ix.closed {
		ix.mu.Unlock()
		return nil
	}
	ix.closed = true
	ix.mu.Unlock()
	return nil
}

// Stats returns the current statistics. Before the first refresh the file
// counts are zero unless a cache was loaded.
func (ix *Index) Stats() Stats {
	if s := ix.snap.Load(); s != nil {
		st := s.stats
		st.Terms = len(s.searchIndex().postings)
		st.Skipped = copyCounts(st.Skipped)
		st.Languages = copyCounts(st.Languages)
		return st
	}
	return Stats{Root: ix.root}
}

func copyCounts(m map[string]int) map[string]int {
	if m == nil {
		return nil
	}
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Invalidate marks files as changed so the next query re-reads them
// without a full scan. Paths may be absolute or relative to the root;
// paths outside the root are ignored.
func (ix *Index) Invalidate(paths ...string) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	for _, p := range paths {
		if rel, ok := ix.relPath(p); ok {
			ix.pending[rel] = true
		}
	}
}

// relPath converts a path to the slash-separated form used in the index.
func (ix *Index) relPath(p string) (string, bool) {
	if strings.TrimSpace(p) == "" {
		return "", false
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(ix.root, p)
	}
	p = filepath.Clean(p)
	if rel, ok := relInside(ix.root, p); ok {
		return rel, true
	}
	// The root was resolved through symlinks; resolve the path's directory
	// the same way (the file itself may already be deleted).
	if dir, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		return relInside(ix.root, filepath.Join(dir, filepath.Base(p)))
	}
	return "", false
}

func relInside(root, p string) (string, bool) {
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// EnsureFresh brings the index up to date cheaply: a full incremental scan
// when the last one is older than the refresh interval (or never ran),
// otherwise only the files passed to Invalidate.
func (ix *Index) EnsureFresh(ctx context.Context) error {
	if ix.isClosed() {
		return ErrClosed
	}
	ix.mu.Lock()
	stale := ix.lastFull.IsZero() || ix.opts.RefreshInterval < 0 || time.Since(ix.lastFull) >= ix.opts.RefreshInterval
	pending := len(ix.pending) > 0
	ix.mu.Unlock()
	switch {
	case stale:
		ix.refreshMu.Lock()
		defer ix.refreshMu.Unlock()
		return ix.refreshLocked(ctx)
	case pending:
		return ix.refreshPending(ctx)
	}
	return nil
}

// Refresh rescans the tree, re-reading only files whose size or mtime
// changed, and swaps in a new snapshot. A cancelled refresh leaves the
// previous snapshot in place.
func (ix *Index) Refresh(ctx context.Context) (Stats, error) {
	ix.refreshMu.Lock()
	defer ix.refreshMu.Unlock()
	if err := ix.refreshLocked(ctx); err != nil {
		if errors.Is(err, ErrClosed) {
			return Stats{}, err
		}
		return ix.Stats(), err
	}
	return ix.Stats(), nil
}

// refreshLocked is Refresh with refreshMu held. It does not compute the
// statistics, which would force the search index to be built.
func (ix *Index) refreshLocked(ctx context.Context) error {
	if ix.isClosed() {
		return ErrClosed
	}
	start := time.Now()
	ix.mu.Lock()
	taken := ix.pending
	ix.pending = map[string]bool{}
	ix.mu.Unlock()
	restore := func() {
		ix.mu.Lock()
		for k := range taken {
			ix.pending[k] = true
		}
		ix.mu.Unlock()
	}

	wr, err := ix.walk(ctx)
	if err != nil {
		restore()
		return err
	}
	old := ix.snap.Load()
	var oldAll map[string]*fileEntry
	if old != nil {
		oldAll = old.all
	}
	all := make(map[string]*fileEntry, len(wr.files))
	var toParse []walkFile
	reused := 0
	for _, f := range wr.files {
		if e := oldAll[f.rel]; e != nil && !taken[f.rel] && !e.Racy && e.Size == f.size && e.ModTime == f.mtime {
			all[f.rel] = e
			reused++
			continue
		}
		toParse = append(toParse, f)
	}
	parsed, err := ix.parseFiles(ctx, toParse, start)
	if err != nil {
		restore()
		return err
	}
	for _, e := range parsed {
		all[e.Path] = e
	}
	removed := 0
	for p := range oldAll {
		if all[p] == nil {
			removed++
		}
	}
	var same *snapshot
	if len(parsed) == 0 && removed == 0 && old != nil {
		same = old // every entry was reused: the same files
	}
	snap := buildSnapshot(ix.root, all, wr.skipped, wr.truncated, same)
	if same == nil {
		snap.prepareSearch()
	}
	snap.stats.Parsed = len(parsed)
	snap.stats.Reused = reused
	snap.stats.Removed = removed
	snap.stats.LastRefresh = start
	snap.stats.Duration = time.Since(start)
	ix.snap.Store(snap)
	ix.mu.Lock()
	ix.lastFull = start
	ix.mu.Unlock()
	if len(parsed) > 0 || removed > 0 || old == nil {
		if err := ix.saveCache(snap); err != nil {
			ix.logf("codeintel: saving cache: %v", err)
		}
	}
	return nil
}

// refreshPending re-reads only the invalidated files.
func (ix *Index) refreshPending(ctx context.Context) error {
	ix.refreshMu.Lock()
	defer ix.refreshMu.Unlock()
	ix.mu.Lock()
	taken := ix.pending
	ix.pending = map[string]bool{}
	ix.mu.Unlock()
	if len(taken) == 0 {
		return nil
	}
	old := ix.snap.Load()
	if old == nil {
		ix.mu.Lock()
		for k := range taken {
			ix.pending[k] = true
		}
		ix.mu.Unlock()
		return ix.refreshLocked(ctx)
	}
	start := time.Now()
	all := make(map[string]*fileEntry, len(old.all)+len(taken))
	for k, v := range old.all {
		all[k] = v
	}
	var toParse []walkFile
	removed := 0
	for rel := range taken {
		st, err := os.Lstat(filepath.Join(ix.root, filepath.FromSlash(rel)))
		if err != nil || !st.Mode().IsRegular() || st.Size() > ix.opts.MaxFileSize || ix.pathExcluded(rel) {
			if all[rel] != nil {
				delete(all, rel)
				removed++
			}
			continue
		}
		if e := all[rel]; e == nil && len(all) >= ix.opts.MaxFiles {
			continue
		}
		toParse = append(toParse, walkFile{rel: rel, size: st.Size(), mtime: st.ModTime().UnixNano()})
	}
	parsed, err := ix.parseFiles(ctx, toParse, start)
	if err != nil {
		ix.mu.Lock()
		for k := range taken {
			ix.pending[k] = true
		}
		ix.mu.Unlock()
		return err
	}
	for _, e := range parsed {
		all[e.Path] = e
	}
	snap := buildSnapshot(ix.root, all, old.walkSkipped, old.stats.Truncated, nil)
	snap.stats.Parsed = len(parsed)
	snap.stats.Reused = len(all) - len(parsed)
	snap.stats.Removed = removed
	snap.stats.LastRefresh = old.stats.LastRefresh
	snap.stats.Duration = time.Since(start)
	ix.snap.Store(snap)
	return nil
}

// parseFiles reads and analyses files in parallel.
func (ix *Index) parseFiles(ctx context.Context, files []walkFile, scanStart time.Time) ([]*fileEntry, error) {
	if len(files) == 0 {
		return nil, ctx.Err()
	}
	out := make([]*fileEntry, len(files))
	next := make(chan int)
	var wg sync.WaitGroup
	workers := ix.opts.Workers
	if workers > len(files) {
		workers = len(files)
	}
	racyCutoff := scanStart.Add(-racyWindow).UnixNano()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				f := files[i]
				e := ix.parseOne(f)
				e.Racy = f.mtime >= racyCutoff
				ix.intern.internEntry(e)
				out[i] = e
			}
		}()
	}
	var err error
feed:
	for i := range files {
		select {
		case next <- i:
		case <-ctx.Done():
			err = ctx.Err()
			break feed
		}
	}
	close(next)
	wg.Wait()
	if err != nil {
		return nil, err
	}
	return out, nil
}

// parseOne reads one file and builds its entry.
func (ix *Index) parseOne(f walkFile) *fileEntry {
	base := &fileEntry{Path: f.rel, Size: f.size, ModTime: f.mtime, Lang: DetectLanguage(f.rel)}
	abs := filepath.Join(ix.root, filepath.FromSlash(f.rel))
	data, err := readCapped(abs, ix.opts.MaxFileSize)
	if err != nil {
		base.Skip = SkipError
		return base
	}
	if reason := sniffContent(data, ix.opts.IncludeGenerated); reason != "" {
		base.Skip = reason
		return base
	}
	e := analyzeFile(f.rel, data)
	e.Size, e.ModTime = f.size, f.mtime
	return e
}

// readCapped reads a regular file of at most limit bytes.
func readCapped(abs string, limit int64) ([]byte, error) {
	fh, err := os.Open(abs)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", abs)
	}
	data, err := io.ReadAll(io.LimitReader(fh, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", abs, limit)
	}
	return data, nil
}

// buildSnapshot assembles an immutable snapshot from file entries. same,
// when not nil, is a snapshot with exactly the same entries, whose derived
// data is shared instead of rebuilt.
func buildSnapshot(root string, all map[string]*fileEntry, skipped map[string]int, truncated bool, same *snapshot) *snapshot {
	s := &snapshot{all: all, walkSkipped: skipped}
	stats := Stats{Root: root, Skipped: copyCounts(skipped), Languages: map[string]int{}, Truncated: truncated}
	if stats.Skipped == nil {
		stats.Skipped = map[string]int{}
	}
	for _, e := range all {
		if e.Skip != "" {
			stats.Skipped[e.Skip]++
			continue
		}
		if same == nil {
			s.files = append(s.files, e)
		}
		stats.Languages[e.Lang]++
		stats.Symbols += len(e.Symbols)
		stats.Chunks += len(e.Chunks)
	}
	for k, v := range stats.Skipped {
		if v == 0 {
			delete(stats.Skipped, k)
		}
	}
	if same != nil {
		s.files, s.derived = same.files, same.derived
	} else {
		sort.Slice(s.files, func(i, j int) bool { return s.files[i].Path < s.files[j].Path })
		s.derived = &derived{files: s.files}
	}
	stats.Files = len(s.files)
	s.stats = stats
	return s
}

// current returns a fresh snapshot, refreshing as needed.
func (ix *Index) current(ctx context.Context) (*snapshot, error) {
	if err := ix.EnsureFresh(ctx); err != nil {
		if s := ix.snap.Load(); s != nil && !errors.Is(err, ErrClosed) && ctx.Err() == nil {
			ix.logf("codeintel: refresh failed, serving the previous index: %v", err)
			return s, nil
		}
		return nil, err
	}
	s := ix.snap.Load()
	if s == nil {
		return nil, fmt.Errorf("codeintel: index is empty")
	}
	return s, nil
}

// Files lists the indexed files (relative, slash-separated), sorted.
func (ix *Index) Files(ctx context.Context) ([]string, error) {
	s, err := ix.current(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(s.files))
	for i, f := range s.files {
		out[i] = f.Path
	}
	return out, nil
}

// FileSymbols returns the symbols defined in one file.
func (ix *Index) FileSymbols(ctx context.Context, path string) ([]Symbol, error) {
	s, err := ix.current(ctx)
	if err != nil {
		return nil, err
	}
	rel, ok := ix.relPath(path)
	if !ok {
		return nil, fmt.Errorf("%s is outside the workspace", path)
	}
	e := s.all[rel]
	if e == nil || e.Skip != "" {
		return nil, fmt.Errorf("%s is not indexed", rel)
	}
	return append([]Symbol(nil), e.Symbols...), nil
}
