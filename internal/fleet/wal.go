package fleet

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// On-disk layout of a coordinator's state directory:
//
//	wal-<first seq, 20 digits>.log   log segments, one JSON record per line
//	snap-<last seq, 20 digits>.json  snapshots covering records up to seq
//
// Every line, in segments and snapshots alike, is "<crc32c hex> <json>\n",
// where the checksum covers the JSON bytes. A snapshot rotates the log to a
// new segment; segments and snapshots older than the retained snapshots
// are deleted.

const (
	walPrefix  = "wal-"
	walSuffix  = ".log"
	snapPrefix = "snap-"
	snapSuffix = ".json"
	tmpSuffix  = ".tmp"

	opPut    = "put"
	opDelete = "del"

	// maxRecordBytes bounds one log line so a corrupt length cannot make
	// recovery allocate without limit.
	maxRecordBytes = 64 << 20
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// walRecord is one log entry. Token is the fencing token of the leader
// that wrote it; replay ignores records whose token is lower than one
// already seen, which discards writes a deposed leader slipped in.
type walRecord struct {
	Seq   uint64 `json:"seq"`
	Token uint64 `json:"tok"`
	Op    string `json:"op"`
	Task  *Task  `json:"task,omitempty"`
	ID    string `json:"id,omitempty"`
}

type snapshotFile struct {
	Seq     uint64    `json:"seq"`
	Token   uint64    `json:"tok"`
	Created time.Time `json:"created"`
	Tasks   []*Task   `json:"tasks"`
}

// WALOptions configure a WAL.
type WALOptions struct {
	// SyncWrites fsyncs after every record. Without it records reach the
	// OS immediately but the disk only at snapshots, rotation and Close,
	// so a machine crash (not a process crash) can lose recent changes.
	SyncWrites bool
	// KeepSnapshots is how many snapshots to retain (default 2), so that
	// recovery can fall back if the newest one is unreadable.
	KeepSnapshots int
	Logger        *slog.Logger
}

// Recovery describes what OpenWAL rebuilt.
type Recovery struct {
	Tasks          map[string]*Task
	SnapshotSeq    uint64
	LastSeq        uint64
	MaxToken       uint64
	Records        int
	StaleRecords   int
	Segments       int
	TruncatedBytes int64
}

// WAL is an append-only, checksummed, segmented write-ahead log with
// snapshot compaction.
type WAL struct {
	mu       sync.Mutex
	dir      string
	opts     WALOptions
	log      *slog.Logger
	f        *os.File
	segStart uint64
	segSize  int64
	nextSeq  uint64
	broken   error
	closed   bool
}

// encodeLine renders v as a checksummed line.
func encodeLine(v any) ([]byte, error) {
	js, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	line := make([]byte, 0, len(js)+10)
	line = fmt.Appendf(line, "%08x ", crc32.Checksum(js, castagnoli))
	line = append(line, js...)
	return append(line, '\n'), nil
}

// decodeLine verifies and parses a line (without its trailing newline).
func decodeLine(line []byte, v any) error {
	if len(line) < 10 || line[8] != ' ' {
		return fmt.Errorf("%w: malformed record", ErrCorruptLog)
	}
	var sum [4]byte
	if _, err := hex.Decode(sum[:], line[:8]); err != nil {
		return fmt.Errorf("%w: malformed checksum", ErrCorruptLog)
	}
	want := uint32(sum[0])<<24 | uint32(sum[1])<<16 | uint32(sum[2])<<8 | uint32(sum[3])
	js := line[9:]
	if crc32.Checksum(js, castagnoli) != want {
		return fmt.Errorf("%w: checksum mismatch", ErrCorruptLog)
	}
	if err := json.Unmarshal(js, v); err != nil {
		return fmt.Errorf("%w: %v", ErrCorruptLog, err)
	}
	return nil
}

func segName(start uint64) string { return fmt.Sprintf("%s%020d%s", walPrefix, start, walSuffix) }
func snapName(seq uint64) string  { return fmt.Sprintf("%s%020d%s", snapPrefix, seq, snapSuffix) }

// listNumbered returns the numbers embedded in names prefix<n>suffix,
// ascending.
func listNumbered(dir, prefix, suffix string) ([]uint64, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []uint64
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasPrefix(n, prefix) || !strings.HasSuffix(n, suffix) {
			continue
		}
		v, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(n, prefix), suffix), 10, 64)
		if err != nil {
			continue
		}
		out = append(out, v)
	}
	slices.Sort(out)
	return out, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return err
	}
	return nil
}

// writeFileAtomic writes data to path via a synced temporary file and a
// rename, then syncs the directory.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + tmpSuffix
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return syncDir(filepath.Dir(path))
}

// OpenWAL opens (creating if needed) the log in dir and rebuilds the task
// set from the newest readable snapshot plus the records after it. A torn
// or corrupt tail of the final segment, as left by a crash mid-append, is
// truncated; corruption anywhere else is reported as ErrCorruptLog rather
// than silently dropping acknowledged writes.
func OpenWAL(dir string, opts WALOptions) (*WAL, *Recovery, error) {
	if opts.KeepSnapshots <= 0 {
		opts.KeepSnapshots = 2
	}
	lg := opts.Logger
	if lg == nil {
		lg = slog.New(slog.DiscardHandler)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("fleet: wal dir: %w", err)
	}
	if ents, err := os.ReadDir(dir); err == nil {
		for _, e := range ents {
			if strings.HasSuffix(e.Name(), tmpSuffix) {
				os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	rec := &Recovery{Tasks: map[string]*Task{}}

	snaps, err := listNumbered(dir, snapPrefix, snapSuffix)
	if err != nil {
		return nil, nil, fmt.Errorf("fleet: wal list: %w", err)
	}
	loaded := len(snaps) == 0
	for i := len(snaps) - 1; i >= 0; i-- {
		sf, err := readSnapshot(filepath.Join(dir, snapName(snaps[i])))
		if err != nil {
			lg.Warn("fleet: unreadable snapshot, trying an older one", "seq", snaps[i], "err", err)
			continue
		}
		for _, t := range sf.Tasks {
			rec.Tasks[t.ID] = t
		}
		rec.SnapshotSeq, rec.LastSeq, rec.MaxToken = sf.Seq, sf.Seq, sf.Token
		loaded = true
		break
	}
	if !loaded {
		return nil, nil, fmt.Errorf("%w: no readable snapshot in %s", ErrCorruptLog, dir)
	}

	segs, err := listNumbered(dir, walPrefix, walSuffix)
	if err != nil {
		return nil, nil, fmt.Errorf("fleet: wal list: %w", err)
	}
	for i, start := range segs {
		// Skip segments wholly covered by the snapshot.
		if i+1 < len(segs) && segs[i+1] <= rec.SnapshotSeq+1 {
			continue
		}
		last := i == len(segs)-1
		if err := replaySegment(filepath.Join(dir, segName(start)), last, rec); err != nil {
			return nil, nil, err
		}
		rec.Segments++
	}

	w := &WAL{dir: dir, opts: opts, log: lg, nextSeq: rec.LastSeq + 1}
	start := rec.LastSeq + 1
	if len(segs) > 0 && segs[len(segs)-1] <= start {
		start = segs[len(segs)-1]
	}
	if err := w.openSegment(start); err != nil {
		return nil, nil, err
	}
	return w, rec, nil
}

func readSnapshot(path string) (*snapshotFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	b = bytes.TrimSuffix(b, []byte("\n"))
	var sf snapshotFile
	if err := decodeLine(b, &sf); err != nil {
		return nil, err
	}
	return &sf, nil
}

// replaySegment applies one segment's records to rec. In the final
// segment, an invalid record with no valid record after it is a torn
// tail and is truncated away.
func replaySegment(path string, last bool, rec *Recovery) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("fleet: wal open: %w", err)
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	var (
		off     int64
		goodEnd int64
		badAt   int64 = -1
		badErr  error
	)
	for {
		line, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			// Long record: accumulate it.
			buf := append([]byte(nil), line...)
			for errors.Is(err, bufio.ErrBufferFull) && len(buf) <= maxRecordBytes {
				line, err = r.ReadSlice('\n')
				buf = append(buf, line...)
			}
			line = buf
		}
		if len(line) == 0 && errors.Is(err, io.EOF) {
			break
		}
		if err != nil && !errors.Is(err, io.EOF) {
			if errors.Is(err, bufio.ErrBufferFull) {
				err = fmt.Errorf("%w: record exceeds %d bytes", ErrCorruptLog, maxRecordBytes)
			}
			if badAt < 0 {
				badAt, badErr = off, err
			}
			break
		}
		n := int64(len(line))
		complete := len(line) > 0 && line[len(line)-1] == '\n'
		var wr walRecord
		var derr error
		if !complete {
			derr = fmt.Errorf("%w: torn record", ErrCorruptLog)
		} else {
			derr = decodeLine(line[:len(line)-1], &wr)
		}
		if derr != nil {
			if badAt < 0 {
				badAt, badErr = off, derr
			}
		} else {
			if badAt >= 0 {
				return fmt.Errorf("%w: %s at offset %d: %v (valid records follow)", ErrCorruptLog, filepath.Base(path), badAt, badErr)
			}
			applyRecord(rec, &wr)
			goodEnd = off + n
		}
		off += n
		if errors.Is(err, io.EOF) {
			break
		}
	}
	if badAt < 0 {
		return nil
	}
	if !last {
		return fmt.Errorf("%w: %s at offset %d: %v", ErrCorruptLog, filepath.Base(path), badAt, badErr)
	}
	if err := os.Truncate(path, goodEnd); err != nil {
		return fmt.Errorf("fleet: wal truncate torn tail: %w", err)
	}
	rec.TruncatedBytes += off - goodEnd
	return nil
}

func applyRecord(rec *Recovery, wr *walRecord) {
	if wr.Seq <= rec.SnapshotSeq {
		return
	}
	if wr.Token < rec.MaxToken {
		rec.StaleRecords++
		return
	}
	rec.MaxToken = wr.Token
	switch wr.Op {
	case opPut:
		if wr.Task != nil {
			rec.Tasks[wr.Task.ID] = wr.Task
		}
	case opDelete:
		delete(rec.Tasks, wr.ID)
	}
	rec.Records++
	rec.LastSeq = max(rec.LastSeq, wr.Seq)
}

func (w *WAL) openSegment(start uint64) error {
	path := filepath.Join(w.dir, segName(start))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("fleet: wal segment: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("fleet: wal segment: %w", err)
	}
	if err := syncDir(w.dir); err != nil {
		f.Close()
		return fmt.Errorf("fleet: wal segment: %w", err)
	}
	w.f, w.segStart, w.segSize = f, start, st.Size()
	return nil
}

// Append writes one record stamped with token and returns its sequence
// number. After a failed write the log refuses further appends, since its
// tail is in an unknown state; reopen it to recover.
func (w *WAL) Append(token uint64, op string, t *Task, id string) (uint64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, ErrClosed
	}
	if w.broken != nil {
		return 0, w.broken
	}
	wr := walRecord{Seq: w.nextSeq, Token: token, Op: op, Task: t, ID: id}
	line, err := encodeLine(&wr)
	if err != nil {
		return 0, fmt.Errorf("fleet: wal encode: %w", err)
	}
	if len(line) > maxRecordBytes {
		return 0, fmt.Errorf("%w: record of %d bytes exceeds limit", ErrInvalid, len(line))
	}
	if _, err := w.f.Write(line); err != nil {
		// Best effort: cut off the partial line so the next open sees a
		// clean tail, then refuse further writes.
		_ = w.f.Truncate(w.segSize)
		w.broken = fmt.Errorf("fleet: wal write: %w", err)
		return 0, w.broken
	}
	if w.opts.SyncWrites {
		if err := w.f.Sync(); err != nil {
			w.broken = fmt.Errorf("fleet: wal sync: %w", err)
			return 0, w.broken
		}
	}
	w.segSize += int64(len(line))
	w.nextSeq++
	return wr.Seq, nil
}

// LastSeq returns the sequence number of the newest record.
func (w *WAL) LastSeq() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.nextSeq - 1
}

// Snapshot writes the full task set as of the newest record, starts a new
// segment and deletes what the retained snapshots make redundant.
func (w *WAL) Snapshot(token uint64, tasks []*Task, now time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	if w.broken != nil {
		return w.broken
	}
	seq := w.nextSeq - 1
	line, err := encodeLine(&snapshotFile{Seq: seq, Token: token, Created: now.UTC(), Tasks: tasks})
	if err != nil {
		return fmt.Errorf("fleet: snapshot encode: %w", err)
	}
	if err := w.f.Sync(); err != nil {
		w.broken = fmt.Errorf("fleet: wal sync: %w", err)
		return w.broken
	}
	if err := writeFileAtomic(filepath.Join(w.dir, snapName(seq)), line, 0o600); err != nil {
		return fmt.Errorf("fleet: snapshot write: %w", err)
	}
	if w.segSize > 0 || w.segStart != seq+1 {
		if err := w.f.Close(); err != nil {
			w.broken = fmt.Errorf("fleet: wal close: %w", err)
			return w.broken
		}
		if err := w.openSegment(seq + 1); err != nil {
			w.broken = err
			return err
		}
	}
	return w.compact()
}

// compact deletes snapshots beyond KeepSnapshots and segments containing
// only records the oldest kept snapshot already covers.
func (w *WAL) compact() error {
	snaps, err := listNumbered(w.dir, snapPrefix, snapSuffix)
	if err != nil {
		return err
	}
	if len(snaps) <= w.opts.KeepSnapshots {
		if len(snaps) == 0 {
			return nil
		}
	} else {
		for _, s := range snaps[:len(snaps)-w.opts.KeepSnapshots] {
			if err := os.Remove(filepath.Join(w.dir, snapName(s))); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		snaps = snaps[len(snaps)-w.opts.KeepSnapshots:]
	}
	oldest := snaps[0]
	segs, err := listNumbered(w.dir, walPrefix, walSuffix)
	if err != nil {
		return err
	}
	for i := 0; i+1 < len(segs); i++ {
		if segs[i+1] <= oldest+1 && segs[i] != w.segStart {
			if err := os.Remove(filepath.Join(w.dir, segName(segs[i]))); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return syncDir(w.dir)
}

// Sync flushes the current segment to disk.
func (w *WAL) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	return w.f.Sync()
}

// Close syncs and closes the log.
func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	serr := w.f.Sync()
	cerr := w.f.Close()
	return errors.Join(serr, cerr)
}
