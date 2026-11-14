package collab

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"math"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Recorder receives every event a room produces, in order, for later
// replay. Implementations must be safe for concurrent use.
type Recorder interface {
	Record(Event) error
	Close() error
}

// Record is one line of a recording: an event plus a hash chain. Hash is
// the hex SHA-256 of the previous record's hash, a newline, and this
// record's JSON encoding with Hash empty, so any edit, insertion, removal
// or reordering of records is detected on replay.
type Record struct {
	Event
	Room string `json:"room,omitempty"`
	Hash string `json:"hash,omitempty"`
}

func chainHash(prev string, r Record) (string, error) {
	r.Hash = ""
	body, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write([]byte(prev))
	h.Write([]byte{'\n'})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// LogRecorder writes a recording as newline-delimited JSON records to an
// append-only writer, one write per record.
type LogRecorder struct {
	mu     sync.Mutex
	w      io.Writer
	closer io.Closer
	syncer interface{ Sync() error }
	room   string
	prev   string
	count  uint64
	err    error
	closed bool
}

// NewLogRecorder records to w. When w is an io.Closer, Close closes it.
func NewLogRecorder(w io.Writer, room string) *LogRecorder {
	lr := &LogRecorder{w: w, room: room}
	if c, ok := w.(io.Closer); ok {
		lr.closer = c
	}
	if s, ok := w.(interface{ Sync() error }); ok {
		lr.syncer = s
	}
	return lr
}

// CreateFileRecorder opens path for appending (mode 0600), continuing the
// hash chain of any records already in it after verifying them.
func CreateFileRecorder(path, room string) (*LogRecorder, error) {
	prev := ""
	if f, err := os.Open(path); err == nil {
		rp := NewReplayer(f, ReplayOptions{Verify: true})
		for {
			_, err := rp.Next(context.Background())
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				f.Close()
				return nil, fmt.Errorf("collab: existing recording %s: %w", path, err)
			}
		}
		prev = rp.prev
		f.Close()
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	lr := NewLogRecorder(f, room)
	lr.prev = prev
	return lr, nil
}

// Record appends ev. After a write error every later call fails with it.
func (l *LogRecorder) Record(ev Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return errors.New("collab: recorder is closed")
	}
	if l.err != nil {
		return l.err
	}
	rec := Record{Event: ev, Room: l.room}
	h, err := chainHash(l.prev, rec)
	if err != nil {
		return err
	}
	rec.Hash = h
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if _, err := l.w.Write(line); err != nil {
		l.err = fmt.Errorf("collab: recording write: %w", err)
		return l.err
	}
	l.prev = h
	l.count++
	return nil
}

// Count returns the number of records written by this recorder.
func (l *LogRecorder) Count() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.count
}

// Close syncs and closes the underlying writer when it supports that.
func (l *LogRecorder) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	var errs []error
	if l.syncer != nil {
		errs = append(errs, l.syncer.Sync())
	}
	if l.closer != nil {
		errs = append(errs, l.closer.Close())
	}
	return errors.Join(errs...)
}

// ErrTampered reports a recording whose hash chain does not verify.
var ErrTampered = errors.New("collab: recording hash chain mismatch")

// ReplayOptions configure a Replayer.
type ReplayOptions struct {
	// Speed scales the original pacing: 1 is real time, 2 twice as fast.
	// Zero or negative replays without waiting.
	Speed float64
	// MaxGap caps any single wait (after scaling); zero means no cap.
	MaxGap time.Duration
	// Verify checks the hash chain and fails with ErrTampered on mismatch.
	Verify bool
	// Sleep replaces the real wait, for tests and simulations.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Replayer iterates a recording, pacing records by their timestamps.
type Replayer struct {
	sc     *bufio.Scanner
	opts   ReplayOptions
	speed  atomic.Uint64 // math.Float64bits
	last   time.Time
	prev   string
	line   int
	paused atomic.Bool
	resume chan struct{}
}

// NewReplayer reads records from r.
func NewReplayer(r io.Reader, opts ReplayOptions) *Replayer {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 2*MaxMessageBytes+(64<<10))
	p := &Replayer{sc: sc, opts: opts, resume: make(chan struct{}, 1)}
	if p.opts.Sleep == nil {
		p.opts.Sleep = sleepCtx
	}
	p.SetSpeed(opts.Speed)
	return p
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SetSpeed changes the pacing; it is safe to call while replaying.
func (p *Replayer) SetSpeed(s float64) {
	if math.IsNaN(s) || math.IsInf(s, 0) {
		s = 0
	}
	p.speed.Store(math.Float64bits(s))
}

// Speed returns the current pacing factor.
func (p *Replayer) Speed() float64 { return math.Float64frombits(p.speed.Load()) }

// Pause stops Next from returning until Resume; safe to call concurrently.
func (p *Replayer) Pause() { p.paused.Store(true) }

// Resume undoes Pause.
func (p *Replayer) Resume() {
	p.paused.Store(false)
	select {
	case p.resume <- struct{}{}:
	default:
	}
}

// Next returns the next record after waiting out its scaled delay from the
// previous one. It returns io.EOF at the end of the recording.
func (p *Replayer) Next(ctx context.Context) (Record, error) {
	for p.paused.Load() {
		select {
		case <-p.resume:
		case <-ctx.Done():
			return Record{}, ctx.Err()
		}
	}
	for {
		if !p.sc.Scan() {
			if err := p.sc.Err(); err != nil {
				return Record{}, fmt.Errorf("collab: read recording line %d: %w", p.line+1, err)
			}
			return Record{}, io.EOF
		}
		p.line++
		if len(p.sc.Bytes()) != 0 {
			break
		}
	}
	var rec Record
	if err := json.Unmarshal(p.sc.Bytes(), &rec); err != nil {
		return Record{}, fmt.Errorf("collab: recording line %d: %w", p.line, err)
	}
	if p.opts.Verify {
		want, err := chainHash(p.prev, rec)
		if err != nil {
			return Record{}, err
		}
		if want != rec.Hash {
			return Record{}, fmt.Errorf("%w at line %d", ErrTampered, p.line)
		}
	}
	p.prev = rec.Hash
	if speed := p.Speed(); speed > 0 && !p.last.IsZero() && rec.Time.After(p.last) {
		d := time.Duration(float64(rec.Time.Sub(p.last)) / speed)
		if p.opts.MaxGap > 0 {
			d = min(d, p.opts.MaxGap)
		}
		if d > 0 {
			if err := p.opts.Sleep(ctx, d); err != nil {
				return Record{}, err
			}
		}
	}
	if rec.Time.After(p.last) {
		p.last = rec.Time
	}
	return rec, nil
}

// All iterates the remaining records; iteration stops after the first
// error, which is yielded.
func (p *Replayer) All(ctx context.Context) iter.Seq2[Record, error] {
	return func(yield func(Record, error) bool) {
		for {
			rec, err := p.Next(ctx)
			if errors.Is(err, io.EOF) {
				return
			}
			if !yield(rec, err) || err != nil {
				return
			}
		}
	}
}
