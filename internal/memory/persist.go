package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/audit/filelock"
)

// On-disk names and formats. See the package documentation for the layout.
const (
	manifestName   = "MANIFEST.json"
	lockName       = "LOCK"
	manifestFormat = "gta-memory"
	formatVersion  = 1

	segMagic       = "GTAMSEG1"
	segHeaderSize  = 8 + 4 + 4 + 8 + 4
	walMagic       = "GTAMWAL1"
	walHeaderSize  = 8 + 4 + 8
	maxSegmentSize = 4 << 30
)

type manifest struct {
	Format        string    `json:"format"`
	Version       int       `json:"version"`
	Generation    uint64    `json:"generation"`
	Segment       string    `json:"segment"`
	SegmentSize   int64     `json:"segment_size"`
	SegmentSHA256 string    `json:"segment_sha256"`
	WAL           string    `json:"wal"`
	Records       int       `json:"records"`
	Dim           int       `json:"dim"`
	Embedder      string    `json:"embedder"`
	NextKey       uint64    `json:"next_key"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// diskState is the persistent side of a Store; guarded by Store.mu and,
// for anything that touches files, the inter-process lock.
type diskState struct {
	dir     string
	gen     uint64
	segSize int64
	wal     *os.File
	walSize int64 // bytes of the log applied to memory (== file size when idle)
	manMod  time.Time
	manSize int64
}

func segName(gen uint64) string { return fmt.Sprintf("seg-%08d.bin", gen) }
func walName(gen uint64) string { return fmt.Sprintf("wal-%08d.log", gen) }

func (d *diskState) path(name string) string { return filepath.Join(d.dir, name) }

func (d *diskState) close() error {
	if d.wal == nil {
		return nil
	}
	err := d.wal.Close()
	d.wal = nil
	return err
}

// lockFiles takes the inter-process lock; a no-op for in-memory stores.
func (s *Store) lockFiles(ctx context.Context) (func(), error) {
	if s.opts.Dir == "" {
		return func() {}, nil
	}
	l, err := filelock.Acquire(ctx, filepath.Join(s.opts.Dir, lockName))
	if err != nil {
		return nil, fmt.Errorf("memory: locking store: %w", err)
	}
	return func() {
		if err := l.Unlock(); err != nil {
			s.log.Warn("memory: unlocking store", "err", err)
		}
	}, nil
}

// openDisk loads or initialises the store directory.
func (s *Store) openDisk(ctx context.Context) error {
	dir, err := filepath.Abs(s.opts.Dir)
	if err != nil {
		return err
	}
	s.opts.Dir = dir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("memory: %w", err)
	}
	lctx, cancel := s.lockCtx(ctx)
	defer cancel()
	unlock, err := s.lockFiles(lctx)
	if err != nil {
		return err
	}
	defer unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.disk = &diskState{dir: dir}
	man, err := s.readManifest()
	switch {
	case err == nil:
		err = s.loadGeneration(ctx, man)
	case errors.Is(err, fs.ErrNotExist) && !s.hasSegments():
		err = s.snapshotLocked() // a brand-new store
	default:
		err = s.recoverWithoutManifest(ctx, err)
	}
	if err != nil {
		s.disk.close()
		s.disk = nil
		return err
	}
	s.removeStaleFiles()
	return nil
}

func (s *Store) hasSegments() bool {
	m, _ := filepath.Glob(filepath.Join(s.disk.dir, "seg-*.bin"))
	return len(m) > 0
}

func (s *Store) readManifest() (*manifest, error) {
	p := s.disk.path(manifestName)
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%w: manifest: %v", ErrCorrupt, err)
	}
	if m.Format != manifestFormat || m.Version != formatVersion || m.Generation == 0 ||
		m.Segment != segName(m.Generation) || m.WAL != walName(m.Generation) || m.Dim <= 0 {
		return nil, fmt.Errorf("%w: manifest fields invalid", ErrCorrupt)
	}
	if fi, err := os.Stat(p); err == nil {
		s.disk.manMod, s.disk.manSize = fi.ModTime(), fi.Size()
	}
	return &m, nil
}

// catchUpLocked brings memory up to date with the files. The caller holds
// the file lock and s.mu.
func (s *Store) catchUpLocked(ctx context.Context) error {
	if s.disk == nil {
		return nil
	}
	man, err := s.readManifest()
	if err != nil {
		return err
	}
	if man.Generation != s.disk.gen {
		if err := s.loadGeneration(ctx, man); err != nil {
			return err
		}
		s.stats.reloads.Add(1)
		return nil
	}
	return s.replayLog(ctx)
}

// maybeCatchUp reloads changes made by other processes before a read, when
// the files changed since we last looked. Failures leave the current view.
func (s *Store) maybeCatchUp(ctx context.Context) {
	s.mu.RLock()
	d := s.disk
	if d == nil || s.closed {
		s.mu.RUnlock()
		return
	}
	walPath, walSize, manMod, manSize := d.path(walName(d.gen)), d.walSize, d.manMod, d.manSize
	manPath := d.path(manifestName)
	s.mu.RUnlock()
	changed := false
	if fi, err := os.Stat(manPath); err != nil || !fi.ModTime().Equal(manMod) || fi.Size() != manSize {
		changed = true
	} else if fi, err := os.Stat(walPath); err != nil || fi.Size() != walSize {
		changed = true
	}
	if !changed {
		return
	}
	err := s.write(ctx, func(*walBatch) error { return nil })
	if err != nil && !errors.Is(err, ErrClosed) {
		s.log.Warn("memory: catching up with other writers failed", "err", err)
	}
}

// loadGeneration replaces the in-memory state with generation man.
func (s *Store) loadGeneration(ctx context.Context, man *manifest) error {
	d := s.disk
	segPath := d.path(man.Segment)
	data, err := readLimited(segPath, maxSegmentSize)
	if err == nil {
		switch sum := sha256.Sum256(data); {
		case int64(len(data)) != man.SegmentSize:
			err = fmt.Errorf("%w: %s is %d bytes, manifest says %d", ErrCorrupt, man.Segment, len(data), man.SegmentSize)
		case hex.EncodeToString(sum[:]) != man.SegmentSHA256:
			err = fmt.Errorf("%w: %s checksum mismatch", ErrCorrupt, man.Segment)
		}
	}
	var seg *segment
	if err == nil {
		seg, err = parseSegment(data)
	}
	if err != nil {
		if !s.opts.RecoverCorrupt || errors.Is(err, fs.ErrNotExist) && !s.opts.RecoverCorrupt {
			return fmt.Errorf("memory: loading %s: %w", segPath, err)
		}
		s.log.Warn("memory: snapshot is damaged; salvaging", "file", segPath, "err", err)
		seg = salvageSegment(data)
		quarantine(segPath)
		seg.salvaged = true
	}
	compatible := man.Embedder == s.emb.ID() && seg.dim == s.dim && man.Dim == s.dim
	if err := s.installSegment(ctx, seg, compatible); err != nil {
		return err
	}
	if d.wal != nil {
		d.close()
	}
	d.gen, d.segSize = man.Generation, int64(len(data))
	if err := s.openLog(man.Generation); err != nil {
		if !s.opts.RecoverCorrupt {
			return err
		}
		s.log.Warn("memory: log is unreadable; starting a new generation", "err", err)
		quarantine(d.path(walName(man.Generation)))
		return s.snapshotLocked()
	}
	if err := s.replayLog(ctx); err != nil {
		return err
	}
	if !compatible || seg.salvaged {
		// Persist the re-embedded or salvaged state right away.
		return s.snapshotLocked()
	}
	return nil
}

// recoverWithoutManifest handles a missing or unreadable manifest next to
// existing segments: strict mode refuses, recovery salvages the newest.
func (s *Store) recoverWithoutManifest(ctx context.Context, cause error) error {
	if !s.opts.RecoverCorrupt {
		return fmt.Errorf("memory: %s: %w", s.disk.dir, errors.Join(ErrCorrupt, cause))
	}
	segs, _ := filepath.Glob(filepath.Join(s.disk.dir, "seg-*.bin"))
	var best uint64
	for _, p := range segs {
		if g, ok := parseGen(filepath.Base(p), "seg-", ".bin"); ok && g > best {
			best = g
		}
	}
	s.log.Warn("memory: manifest missing or damaged; salvaging newest snapshot", "err", cause, "generation", best)
	if best == 0 {
		quarantine(s.disk.path(manifestName))
		return s.snapshotLocked()
	}
	segPath := s.disk.path(segName(best))
	data, _ := readLimited(segPath, maxSegmentSize)
	seg, err := parseSegment(data)
	if err != nil {
		seg = salvageSegment(data)
	}
	if err := s.installSegment(ctx, seg, seg.dim == s.dim && seg.embTag == s.embTag); err != nil {
		return err
	}
	s.disk.gen = best
	if err := s.openLog(best); err == nil {
		if err := s.replayLog(ctx); err != nil {
			s.log.Warn("memory: replaying log during recovery", "err", err)
		}
	}
	quarantine(s.disk.path(manifestName))
	return s.snapshotLocked()
}

// installSegment resets the in-memory state to seg. When the vectors are
// not compatible with the current embedder, every text is re-embedded.
func (s *Store) installSegment(ctx context.Context, seg *segment, compatible bool) error {
	if err := s.resetIndexes(); err != nil {
		return err
	}
	if !compatible {
		s.log.Warn("memory: embedder changed; re-embedding all records", "records", len(seg.records), "embedder", s.emb.ID())
		if err := s.reembed(ctx, seg.records); err != nil {
			return err
		}
		seg.graph = nil
	}
	graphOK := false
	if seg.graph != nil {
		vecs := make(map[uint64][]float32, len(seg.records))
		for _, r := range seg.records {
			vecs[r.Key] = r.Vec
		}
		err := s.hnsw.loadBinary(seg.graph, func(k uint64) []float32 { return vecs[k] })
		if err == nil && s.hnsw.Len() == len(seg.records) {
			graphOK = true
		} else {
			s.log.Warn("memory: stored graph unusable; rebuilding", "err", err)
			if err := s.resetIndexes(); err != nil {
				return err
			}
		}
	}
	for _, r := range seg.records {
		if graphOK {
			s.indexWithoutVector(r)
		} else if err := s.applyPut(r); err != nil {
			return err
		}
	}
	s.nextKey = max(s.nextKey, seg.nextKey)
	return nil
}

// indexWithoutVector adds a record whose vector is already in the graph.
func (s *Store) indexWithoutVector(sr *storedRecord) {
	e := &entry{rec: sr.Rec, vec: sr.Vec, key: sr.Key}
	e.rec.LastAccess, e.rec.AccessCount = time.Time{}, 0
	e.lastAccess.Store(sr.Rec.LastAccess.UnixNano())
	e.accesses.Store(sr.Rec.AccessCount)
	s.bm25.Add(sr.Key, lexicalDocTerms(&e.rec))
	e.exactKey = groupKey(&e.rec) + "\x00" + normalizedKey(e.rec.Text)
	if _, taken := s.exact[e.exactKey]; !taken {
		s.exact[e.exactKey] = e.rec.ID
	}
	s.byID[e.rec.ID] = e
	s.byKey[e.key] = e
	s.textBytes += int64(len(e.rec.Text))
	if sr.Key >= s.nextKey {
		s.nextKey = sr.Key + 1
	}
}

// reembed replaces the vectors of recs using the current embedder.
func (s *Store) reembed(ctx context.Context, recs []*storedRecord) error {
	const batch = 256
	for start := 0; start < len(recs); start += batch {
		part := recs[start:min(start+batch, len(recs))]
		texts := make([]string, len(part))
		for i, r := range part {
			texts[i] = r.Rec.Text
		}
		vecs, err := s.embedMany(ctx, texts)
		if err != nil {
			return fmt.Errorf("memory: re-embedding: %w", err)
		}
		for i, r := range part {
			r.Vec = vecs[i]
		}
	}
	return nil
}

// openLog opens the log of generation gen and checks its header.
func (s *Store) openLog(gen uint64) error {
	d := s.disk
	f, err := os.OpenFile(d.path(walName(gen)), os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("memory: opening log: %w", err)
	}
	var hdr [walHeaderSize]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil || string(hdr[:8]) != walMagic ||
		binary.LittleEndian.Uint32(hdr[8:]) != formatVersion || binary.LittleEndian.Uint64(hdr[12:]) != gen {
		f.Close()
		return fmt.Errorf("%w: log header of generation %d", ErrCorrupt, gen)
	}
	d.wal, d.walSize = f, walHeaderSize
	return nil
}

// replayLog applies log frames past walSize. A frame that is incomplete or
// fails its checksum ends the log: it can only be the torn tail of a writer
// that crashed (appends happen under the lock we hold), so the file is
// truncated there.
func (s *Store) replayLog(ctx context.Context) error {
	d := s.disk
	fi, err := d.wal.Stat()
	if err != nil {
		return err
	}
	if fi.Size() == d.walSize {
		return nil
	}
	if fi.Size() < d.walSize {
		return fmt.Errorf("%w: log shrank from %d to %d bytes", ErrCorrupt, d.walSize, fi.Size())
	}
	buf := make([]byte, fi.Size()-d.walSize)
	if _, err := d.wal.ReadAt(buf, d.walSize); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	off := 0
	for off < len(buf) {
		typ, payload, size, ok := readFrame(buf[off:])
		if !ok {
			break
		}
		if err := s.applyLogFrame(ctx, typ, payload); err != nil {
			if errors.Is(err, ErrCorrupt) {
				break
			}
			return err
		}
		off += size
	}
	good := d.walSize + int64(off)
	if off < len(buf) {
		s.log.Warn("memory: truncating torn log tail", "file", d.wal.Name(), "dropped_bytes", len(buf)-off)
		if err := d.wal.Truncate(good); err != nil {
			return fmt.Errorf("memory: truncating log: %w", err)
		}
		if err := d.wal.Sync(); err != nil {
			return err
		}
	}
	d.walSize = good
	return nil
}

func (s *Store) applyLogFrame(ctx context.Context, typ byte, payload []byte) error {
	switch typ {
	case walPut:
		if len(payload) < 4 {
			return fmt.Errorf("%w: short put", ErrCorrupt)
		}
		tag := binary.LittleEndian.Uint32(payload)
		sr, err := decodeRecord(payload[4:])
		if err != nil {
			return err
		}
		if tag != s.embTag || len(sr.Vec) != s.dim {
			// Written by a process with another embedder.
			if err := s.reembed(ctx, []*storedRecord{sr}); err != nil {
				return err
			}
		}
		if old := s.byID[sr.Rec.ID]; old != nil && old.key != sr.Key {
			if other := s.byKey[sr.Key]; other != nil {
				sr.Key = s.allocKey()
			}
		}
		return s.applyPut(sr)
	case walDelete:
		ids, err := decodeIDs(payload)
		if err != nil {
			return err
		}
		for _, id := range ids {
			s.applyDelete(id)
		}
	case walTouch:
		ts, err := decodeTouches(payload)
		if err != nil {
			return err
		}
		for _, t := range ts {
			s.applyTouch(t)
		}
	default:
		return fmt.Errorf("%w: unknown log frame type %d", ErrCorrupt, typ)
	}
	return nil
}

func encodeWALPut(tag uint32, sr *storedRecord) []byte {
	buf := binary.LittleEndian.AppendUint32(make([]byte, 0, 256+4*len(sr.Vec)), tag)
	return encodeRecord(buf, sr)
}

// appendLog appends frames at the end of the log and syncs them. On a
// failure the partial write is cut off again so the log stays parseable.
func (s *Store) appendLog(frames []byte) error {
	d := s.disk
	if d.wal == nil {
		return ErrClosed
	}
	if _, err := d.wal.WriteAt(frames, d.walSize); err != nil {
		_ = d.wal.Truncate(d.walSize)
		return fmt.Errorf("memory: writing log: %w", err)
	}
	if !s.opts.NoSync {
		if err := d.wal.Sync(); err != nil {
			_ = d.wal.Truncate(d.walSize)
			return fmt.Errorf("memory: syncing log: %w", err)
		}
	}
	d.walSize += int64(len(frames))
	return nil
}

func (s *Store) maybeCompactLocked() error {
	if s.disk == nil || s.disk.walSize < s.opts.CompactBytes {
		return nil
	}
	return s.snapshotLocked()
}

// Compact writes a fresh snapshot and starts an empty log.
func (s *Store) Compact(ctx context.Context) error {
	if s.opts.Dir == "" {
		return nil
	}
	lctx, cancel := s.lockCtx(ctx)
	defer cancel()
	unlock, err := s.lockFiles(lctx)
	if err != nil {
		return err
	}
	defer unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if err := s.catchUpLocked(ctx); err != nil {
		return err
	}
	return s.snapshotLocked()
}

// snapshotLocked writes generation gen+1 (segment, empty log, manifest) and
// removes generation gen. The caller holds the file lock and s.mu.
//
// Order matters for crash safety: the new segment and log are complete and
// synced before the manifest rename publishes them, and the old files are
// removed only afterwards. A crash before the rename leaves the previous
// generation in force (the orphans are removed at the next open); a crash
// after it leaves the new one.
func (s *Store) snapshotLocked() error {
	d := s.disk
	gen := d.gen + 1
	// Access-time updates still buffered are captured by the snapshot.
	s.takeTouches()

	entries := make([]*entry, 0, len(s.byKey))
	for _, e := range s.byKey {
		entries = append(entries, e)
	}
	slices.SortFunc(entries, func(a, b *entry) int {
		switch {
		case a.key < b.key:
			return -1
		case a.key > b.key:
			return 1
		}
		return 0
	})
	buf := make([]byte, segHeaderSize, segHeaderSize+len(entries)*(256+4*s.dim))
	copy(buf, segMagic)
	binary.LittleEndian.PutUint32(buf[8:], formatVersion)
	binary.LittleEndian.PutUint32(buf[12:], uint32(s.dim))
	binary.LittleEndian.PutUint64(buf[16:], s.nextKey)
	binary.LittleEndian.PutUint32(buf[24:], s.embTag)
	var payload []byte
	for _, e := range entries {
		payload = encodeRecord(payload[:0], e.stored())
		buf = appendFrame(buf, frameRecord, payload)
	}
	buf = appendFrame(buf, frameGraph, s.hnsw.appendBinary(nil))
	buf = appendFrame(buf, frameEnd, binary.LittleEndian.AppendUint64(nil, uint64(len(entries))))
	if err := writeFileAtomic(d.path(segName(gen)), buf); err != nil {
		return fmt.Errorf("memory: writing snapshot: %w", err)
	}
	var hdr [walHeaderSize]byte
	copy(hdr[:], walMagic)
	binary.LittleEndian.PutUint32(hdr[8:], formatVersion)
	binary.LittleEndian.PutUint64(hdr[12:], gen)
	if err := writeFileAtomic(d.path(walName(gen)), hdr[:]); err != nil {
		return fmt.Errorf("memory: creating log: %w", err)
	}
	sum := sha256.Sum256(buf)
	man := manifest{
		Format: manifestFormat, Version: formatVersion, Generation: gen,
		Segment: segName(gen), SegmentSize: int64(len(buf)), SegmentSHA256: hex.EncodeToString(sum[:]),
		WAL: walName(gen), Records: len(entries), Dim: s.dim, Embedder: s.emb.ID(),
		NextKey: s.nextKey, UpdatedAt: s.now().UTC(),
	}
	mdata, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(d.path(manifestName), append(mdata, '\n')); err != nil {
		return fmt.Errorf("memory: writing manifest: %w", err)
	}
	if fi, err := os.Stat(d.path(manifestName)); err == nil {
		d.manMod, d.manSize = fi.ModTime(), fi.Size()
	}
	old := d.gen
	d.close()
	d.gen, d.segSize = gen, int64(len(buf))
	if err := s.openLog(gen); err != nil {
		return err
	}
	if old != 0 {
		for _, name := range []string{segName(old), walName(old)} {
			if err := os.Remove(d.path(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				s.log.Warn("memory: removing old generation", "file", name, "err", err)
			}
		}
	}
	s.stats.compactions.Add(1)
	return nil
}

// removeStaleFiles deletes segments, logs and temporary files that do not
// belong to the current generation (left by crashes mid-snapshot).
func (s *Store) removeStaleFiles() {
	d := s.disk
	ents, err := os.ReadDir(d.dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		name := e.Name()
		stale := strings.HasSuffix(name, ".tmp")
		if g, ok := parseGen(name, "seg-", ".bin"); ok && g != d.gen {
			stale = true
		}
		if g, ok := parseGen(name, "wal-", ".log"); ok && g != d.gen {
			stale = true
		}
		if stale {
			_ = os.Remove(d.path(name))
		}
	}
}

func parseGen(name, prefix, suffix string) (uint64, bool) {
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
		return 0, false
	}
	g, err := strconv.ParseUint(name[len(prefix):len(name)-len(suffix)], 10, 64)
	return g, err == nil && g > 0
}

// segment is a parsed snapshot.
type segment struct {
	dim      int
	embTag   uint32
	nextKey  uint64
	records  []*storedRecord
	graph    []byte
	salvaged bool
}

func parseSegment(data []byte) (*segment, error) {
	if len(data) < segHeaderSize || string(data[:8]) != segMagic {
		return nil, fmt.Errorf("%w: bad snapshot header", ErrCorrupt)
	}
	if v := binary.LittleEndian.Uint32(data[8:]); v != formatVersion {
		return nil, fmt.Errorf("%w: snapshot format version %d", ErrCorrupt, v)
	}
	seg := &segment{
		dim:     int(binary.LittleEndian.Uint32(data[12:])),
		nextKey: binary.LittleEndian.Uint64(data[16:]),
		embTag:  binary.LittleEndian.Uint32(data[24:]),
	}
	off := segHeaderSize
	ids := map[string]bool{}
	for {
		typ, payload, size, ok := readFrame(data[off:])
		if !ok {
			return nil, fmt.Errorf("%w: bad frame at offset %d", ErrCorrupt, off)
		}
		off += size
		switch typ {
		case frameRecord:
			sr, err := decodeRecord(payload)
			if err != nil {
				return nil, err
			}
			if len(sr.Vec) != seg.dim || ids[sr.Rec.ID] {
				return nil, fmt.Errorf("%w: record %q is malformed or repeated", ErrCorrupt, sr.Rec.ID)
			}
			ids[sr.Rec.ID] = true
			seg.records = append(seg.records, sr)
		case frameGraph:
			seg.graph = payload
		case frameEnd:
			if len(payload) != 8 || binary.LittleEndian.Uint64(payload) != uint64(len(seg.records)) {
				return nil, fmt.Errorf("%w: snapshot record count mismatch", ErrCorrupt)
			}
			if off != len(data) {
				return nil, fmt.Errorf("%w: bytes after snapshot trailer", ErrCorrupt)
			}
			return seg, nil
		default:
			return nil, fmt.Errorf("%w: unknown snapshot frame type %d", ErrCorrupt, typ)
		}
	}
}

// salvageSegment recovers every intact record frame from damaged data by
// scanning for frames that pass their checksum. The graph is rebuilt.
func salvageSegment(data []byte) *segment {
	seg := &segment{salvaged: true}
	if len(data) >= segHeaderSize && string(data[:8]) == segMagic {
		seg.dim = int(binary.LittleEndian.Uint32(data[12:]))
		seg.nextKey = binary.LittleEndian.Uint64(data[16:])
		seg.embTag = binary.LittleEndian.Uint32(data[24:])
	}
	byID := map[string]int{}
	for off := min(segHeaderSize, len(data)); off < len(data); {
		typ, payload, size, ok := readFrame(data[off:])
		if !ok || typ != frameRecord {
			if ok {
				off += size
			} else {
				off++
			}
			continue
		}
		if sr, err := decodeRecord(payload); err == nil && (seg.dim == 0 || len(sr.Vec) == seg.dim) {
			if seg.dim == 0 {
				seg.dim = len(sr.Vec)
			}
			if i, dup := byID[sr.Rec.ID]; dup {
				seg.records[i] = sr
			} else {
				byID[sr.Rec.ID] = len(seg.records)
				seg.records = append(seg.records, sr)
			}
			seg.nextKey = max(seg.nextKey, sr.Key+1)
		}
		off += size
	}
	// Keys must be unique for the indexes.
	seen := map[uint64]bool{}
	for _, r := range seg.records {
		for seen[r.Key] {
			r.Key = seg.nextKey
			seg.nextKey++
		}
		seen[r.Key] = true
	}
	return seg
}

// quarantine renames a damaged file out of the way, keeping it for
// inspection.
func quarantine(path string) {
	if _, err := os.Stat(path); err == nil {
		_ = os.Rename(path, path+".corrupt-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	}
}

func readLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var b bytes.Buffer
	n, err := io.Copy(&b, io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if n > limit {
		return nil, fmt.Errorf("%w: %s exceeds %d bytes", ErrCorrupt, path, limit)
	}
	return b.Bytes(), nil
}

// writeFileAtomic writes data to path through a synced temporary file and
// a rename, then syncs the directory so the rename itself is durable.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil // directories cannot be fsynced; renames are journaled
	}
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return err
	}
	return nil
}
