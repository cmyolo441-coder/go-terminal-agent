package workflow

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// RecordType is the kind of a journal record.
type RecordType string

// Journal record types.
const (
	RecRunStarted            RecordType = "run_started"
	RecRunResumed            RecordType = "run_resumed"
	RecStepStarted           RecordType = "step_started"
	RecStepRetry             RecordType = "step_retry"
	RecStepSucceeded         RecordType = "step_succeeded"
	RecStepFailed            RecordType = "step_failed"
	RecStepSkipped           RecordType = "step_skipped"
	RecStepCancelled         RecordType = "step_cancelled"
	RecRunCompensating       RecordType = "run_compensating"
	RecCompensationStarted   RecordType = "compensation_started"
	RecCompensationSucceeded RecordType = "compensation_succeeded"
	RecCompensationFailed    RecordType = "compensation_failed"
	RecRunFinished           RecordType = "run_finished"
)

// Record is one journal entry. Fields not relevant to a type are omitted.
type Record struct {
	Seq     uint64         `json:"seq"`
	Time    time.Time      `json:"time"`
	Type    RecordType     `json:"type"`
	RunID   string         `json:"run_id,omitempty"`
	Step    string         `json:"step,omitempty"`
	Attempt int            `json:"attempt,omitempty"`
	Key     string         `json:"key,omitempty"`
	Outputs map[string]any `json:"outputs,omitempty"`
	Error   string         `json:"error,omitempty"`
	Class   ErrorClass     `json:"class,omitempty"`
	Delay   Duration       `json:"delay,omitempty"`
	// Tolerated marks a step failure absorbed by continue_on_error.
	Tolerated bool      `json:"tolerated,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	Status    RunStatus `json:"status,omitempty"`
	// Spec, Inputs and Nodes are written once, in run_started, so a run
	// can be resumed from its journal alone and the expansion verified.
	Spec   json.RawMessage `json:"spec,omitempty"`
	Inputs map[string]any  `json:"inputs,omitempty"`
	Nodes  []string        `json:"nodes,omitempty"`
}

// Journal is the durable sink for run records. Append must not return
// until the record is durable.
type Journal interface {
	Append(rec *Record) error
}

// ErrJournalLocked is returned when another process holds the journal.
var ErrJournalLocked = errors.New("workflow journal is locked by another process")

// CorruptJournalError reports a damaged record that is not the torn tail
// of a crash, so it cannot be repaired by truncation.
type CorruptJournalError struct {
	Path   string
	Offset int64
	Err    error
}

func (e *CorruptJournalError) Error() string {
	return fmt.Sprintf("workflow journal %s is corrupt at byte %d: %v", e.Path, e.Offset, e.Err)
}

func (e *CorruptJournalError) Unwrap() error { return e.Err }

const maxRecordBytes = 32 << 20

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// FileJournal is an append-only, fsynced JSONL journal. Each line is
//
//	<crc32c of the JSON as 8 hex digits> <space> <JSON record> <newline>
//
// and records carry consecutive sequence numbers starting at 1.
type FileJournal struct {
	mu     sync.Mutex
	f      *os.File
	path   string
	seq    uint64
	broken error
	// TornBytes is the number of bytes of torn tail removed on open.
	TornBytes int64
}

// CreateJournal creates a new journal; the file must not exist.
func CreateJournal(path string) (*FileJournal, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create workflow journal: %w", err)
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, err
	}
	syncDir(filepath.Dir(path))
	return &FileJournal{f: f, path: path}, nil
}

// OpenJournal opens an existing journal for appending after recovering it:
// a torn final record left by a crash is truncated away (and reported in
// TornBytes); any other damage is a *CorruptJournalError.
func OpenJournal(path string) (*FileJournal, []Record, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open workflow journal: %w", err)
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, nil, err
	}
	recs, good, err := readRecords(f, path)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	end, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	j := &FileJournal{f: f, path: path}
	if end > good {
		if err := f.Truncate(good); err != nil {
			f.Close()
			return nil, nil, fmt.Errorf("truncate torn journal tail: %w", err)
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return nil, nil, err
		}
		j.TornBytes = end - good
	}
	if _, err := f.Seek(good, io.SeekStart); err != nil {
		f.Close()
		return nil, nil, err
	}
	if len(recs) > 0 {
		j.seq = recs[len(recs)-1].Seq
	}
	return j, recs, nil
}

// ReadJournal reads a journal without modifying or locking it, ignoring a
// torn tail.
func ReadJournal(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	recs, _, err := readRecords(f, path)
	return recs, err
}

// Path returns the journal's file path.
func (j *FileJournal) Path() string { return j.path }

// Append implements Journal: it assigns the next sequence number, writes
// the record and fsyncs. After a failed write the journal refuses further
// appends, since its tail is in an unknown state.
func (j *FileJournal) Append(rec *Record) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.broken != nil {
		return j.broken
	}
	if j.f == nil {
		return errors.New("workflow journal is closed")
	}
	rec.Seq = j.seq + 1
	line, err := encodeRecord(rec)
	if err != nil {
		return err
	}
	if _, err := j.f.Write(line); err != nil {
		j.broken = fmt.Errorf("write workflow journal: %w", err)
		return j.broken
	}
	if err := j.f.Sync(); err != nil {
		j.broken = fmt.Errorf("sync workflow journal: %w", err)
		return j.broken
	}
	j.seq = rec.Seq
	return nil
}

// Close releases the journal.
func (j *FileJournal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return nil
	}
	err := j.f.Close()
	j.f = nil
	return err
}

func encodeRecord(rec *Record) ([]byte, error) {
	body, err := json.Marshal(rec)
	if err != nil {
		return nil, fmt.Errorf("encode journal record: %w", err)
	}
	if len(body) > maxRecordBytes {
		return nil, fmt.Errorf("journal record is %d bytes; the limit is %d (are step outputs too large?)", len(body), maxRecordBytes)
	}
	line := make([]byte, 0, len(body)+10)
	line = fmt.Appendf(line, "%08x ", crc32.Checksum(body, crcTable))
	line = append(line, body...)
	return append(line, '\n'), nil
}

func decodeRecord(line []byte, wantSeq uint64) (Record, error) {
	var rec Record
	line = bytes.TrimSuffix(line, []byte{'\n'})
	if len(line) < 10 || line[8] != ' ' {
		return rec, errors.New("malformed record")
	}
	sum, err := strconv.ParseUint(string(line[:8]), 16, 32)
	if err != nil {
		return rec, errors.New("malformed checksum")
	}
	body := line[9:]
	if got := crc32.Checksum(body, crcTable); got != uint32(sum) {
		return rec, fmt.Errorf("checksum mismatch (stored %08x, computed %08x)", sum, got)
	}
	if err := json.Unmarshal(body, &rec); err != nil {
		return rec, fmt.Errorf("decode record: %w", err)
	}
	if rec.Seq != wantSeq {
		return rec, fmt.Errorf("sequence %d where %d was expected", rec.Seq, wantSeq)
	}
	return rec, nil
}

// readRecords decodes records from r. It returns the offset just past the
// last good record. A damaged final line — incomplete, or complete but
// failing its checksum, with nothing after it — is a torn tail and is
// ignored; damage followed by more data is corruption.
func readRecords(r io.Reader, path string) ([]Record, int64, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	var recs []Record
	var off int64
	for {
		line, err := readLine(br)
		if len(line) == 0 && errors.Is(err, io.EOF) {
			return recs, off, nil
		}
		if err != nil && !errors.Is(err, io.EOF) {
			if errors.Is(err, errLineTooLong) {
				return nil, 0, &CorruptJournalError{Path: path, Offset: off, Err: err}
			}
			return nil, 0, err
		}
		if errors.Is(err, io.EOF) {
			// No trailing newline: the write was torn.
			return recs, off, nil
		}
		rec, derr := decodeRecord(line, uint64(len(recs))+1)
		if derr != nil {
			if _, perr := br.Peek(1); errors.Is(perr, io.EOF) {
				return recs, off, nil
			}
			return nil, 0, &CorruptJournalError{Path: path, Offset: off, Err: derr}
		}
		recs = append(recs, rec)
		off += int64(len(line))
	}
}

var errLineTooLong = errors.New("record exceeds the size limit")

func readLine(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > maxRecordBytes+16 {
			return buf, errLineTooLong
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return buf, err
	}
}

// memJournal keeps records in memory, for runs without durability.
type memJournal struct {
	mu   sync.Mutex
	recs []Record
}

func (m *memJournal) Append(rec *Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec.Seq = uint64(len(m.recs)) + 1
	m.recs = append(m.recs, *rec)
	return nil
}
