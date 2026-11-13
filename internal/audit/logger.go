package audit

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/audit/filelock"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/secrets"
)

// Logger records audit events. Implementations are safe for concurrent use.
type Logger interface {
	Log(rec Record) error
	Close() error
}

// Nop is a Logger that discards everything.
type Nop struct{}

// Log implements Logger.
func (Nop) Log(Record) error { return nil }

// Close implements Logger.
func (Nop) Close() error { return nil }

// SyncMode controls when records are flushed to stable storage.
type SyncMode string

const (
	// SyncAlways fsyncs after every record: nothing acknowledged is lost on
	// power failure. This is the default.
	SyncAlways SyncMode = "always"
	// SyncInterval fsyncs in the background every Options.SyncEvery.
	SyncInterval SyncMode = "interval"
	// SyncNever leaves flushing to the operating system.
	SyncNever SyncMode = "never"
)

// ParseSyncMode validates a sync mode name; empty means SyncAlways.
func ParseSyncMode(s string) (SyncMode, error) {
	switch m := SyncMode(strings.ToLower(strings.TrimSpace(s))); m {
	case "":
		return SyncAlways, nil
	case SyncAlways, SyncInterval, SyncNever:
		return m, nil
	}
	return "", fmt.Errorf("audit: unknown sync mode %q (want always, interval or never)", s)
}

// Hash algorithms recorded in Record.Alg.
const (
	AlgSHA256     = "sha256"
	AlgHMACSHA256 = "hmac-sha256"
)

// Genesis is the PrevHash of the first record in a log.
var Genesis = strings.Repeat("0", 64)

// Options configures a FileLogger.
type Options struct {
	// Dir holds the log segments. It is created with mode 0700.
	Dir string
	// Sync picks the flush policy (default SyncAlways).
	Sync SyncMode
	// SyncEvery is the SyncInterval period (default 1s).
	SyncEvery time.Duration
	// MaxFileBytes rotates a day's segment when it would grow past this
	// size (default 64 MiB).
	MaxFileBytes int64
	// HMACKey, when set, keys the hash chain with HMAC-SHA256 so that
	// someone without the key cannot forge a consistent chain.
	HMACKey []byte
	// Redactor removes secrets from free-text fields (default
	// secrets.Default()).
	Redactor *secrets.Redactor
	// MaxFieldBytes truncates Input, Reason, Error and detail values
	// (default 4096).
	MaxFieldBytes int
	// Identity defaults applied to records that leave them empty. User,
	// Host and PID are detected when empty.
	Session, User, Host, Workspace string
	PID                            int
	// LockTimeout bounds the wait for the cross-process lock (default 10s).
	LockTimeout time.Duration
	// Now is the clock (default time.Now).
	Now func() time.Time
}

// ErrClosed is returned by Log after Close.
var ErrClosed = errors.New("audit: logger is closed")

// FileLogger writes the hash-chained JSON Lines log.
type FileLogger struct {
	opts     Options
	alg      string
	lockPath string

	mu     sync.Mutex
	f      *os.File
	day    string
	idx    int
	size   int64
	seq    uint64
	last   string
	known  bool
	dirty  bool
	closed bool

	stop chan struct{}
	done chan struct{}
}

// Open creates the log directory if needed and returns a logger. It takes
// the lock once and reads the chain head, so a corrupt log is reported here
// rather than on the first event.
func Open(opts Options) (*FileLogger, error) {
	if opts.Dir == "" {
		return nil, errors.New("audit: no directory")
	}
	var err error
	if opts.Sync, err = ParseSyncMode(string(opts.Sync)); err != nil {
		return nil, err
	}
	if opts.SyncEvery <= 0 {
		opts.SyncEvery = time.Second
	}
	if opts.MaxFileBytes <= 0 {
		opts.MaxFileBytes = 64 << 20
	}
	if opts.MaxFieldBytes <= 0 {
		opts.MaxFieldBytes = 4096
	}
	if opts.LockTimeout <= 0 {
		opts.LockTimeout = 10 * time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Redactor == nil {
		opts.Redactor = secrets.Default()
	}
	if opts.User == "" {
		opts.User = currentUser()
	}
	if opts.Host == "" {
		opts.Host, _ = os.Hostname()
	}
	if opts.PID == 0 {
		opts.PID = os.Getpid()
	}
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(opts.Dir); err == nil && fi.Mode().Perm()&0o077 != 0 {
			if err := os.Chmod(opts.Dir, 0o700); err != nil {
				return nil, fmt.Errorf("audit: restrict %s: %w", opts.Dir, err)
			}
		}
	}
	l := &FileLogger{opts: opts, alg: AlgSHA256, lockPath: filepath.Join(opts.Dir, ".audit.lock")}
	if len(opts.HMACKey) > 0 {
		l.alg = AlgHMACSHA256
	}
	lock, err := filelock.AcquireTimeout(l.lockPath, opts.LockTimeout)
	if err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	torn, err := l.syncHead(opts.Now())
	if err == nil && torn != nil {
		err = l.writeRecovered(torn)
	}
	lock.Unlock()
	if err != nil {
		l.closeFile()
		return nil, err
	}
	if opts.Sync == SyncInterval {
		l.stop, l.done = make(chan struct{}), make(chan struct{})
		go l.syncLoop()
	}
	return l, nil
}

func currentUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	for _, k := range []string{"USER", "USERNAME", "LOGNAME"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// Dir is the log directory.
func (l *FileLogger) Dir() string { return l.opts.Dir }

// Head returns the sequence number and hash of the last record this logger
// knows of. Publishing it elsewhere (a SIEM, a ticket) anchors the chain
// against truncation.
func (l *FileLogger) Head() (uint64, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seq, l.last
}

// Log appends a record. Free-text fields are redacted and truncated first.
func (l *FileLogger) Log(rec Record) error {
	if rec.Event == "" {
		return errors.New("audit: record has no event type")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrClosed
	}
	l.prepare(&rec)
	lock, err := filelock.AcquireTimeout(l.lockPath, l.opts.LockTimeout)
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	defer lock.Unlock()
	torn, err := l.syncHead(rec.Time)
	if err != nil {
		return err
	}
	if torn != nil {
		if err := l.writeRecovered(torn); err != nil {
			return err
		}
	}
	return l.appendLocked(rec)
}

// Flush forces buffered records to stable storage.
func (l *FileLogger) Flush() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.flushLocked()
}

func (l *FileLogger) flushLocked() error {
	if l.f == nil || !l.dirty {
		return nil
	}
	l.dirty = false
	return l.f.Sync()
}

// Close flushes and closes the log. It is safe to call more than once.
func (l *FileLogger) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	stop := l.stop
	l.mu.Unlock()
	if stop != nil {
		close(stop)
		<-l.done
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	err := l.flushLocked()
	return errors.Join(err, l.closeFile())
}

func (l *FileLogger) closeFile() error {
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

func (l *FileLogger) syncLoop() {
	defer close(l.done)
	t := time.NewTicker(l.opts.SyncEvery)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			l.mu.Lock()
			_ = l.flushLocked()
			l.mu.Unlock()
		}
	}
}

// prepare fills defaults and removes secrets from free text.
func (l *FileLogger) prepare(rec *Record) {
	if rec.Time.IsZero() {
		rec.Time = l.opts.Now()
	}
	rec.Time = rec.Time.UTC()
	if rec.Session == "" {
		rec.Session = l.opts.Session
	}
	if rec.User == "" {
		rec.User = l.opts.User
	}
	if rec.Host == "" {
		rec.Host = l.opts.Host
	}
	if rec.PID == 0 {
		rec.PID = l.opts.PID
	}
	if rec.Workspace == "" {
		rec.Workspace = l.opts.Workspace
	}
	red := l.opts.Redactor
	n := 0
	clean := func(s string) string {
		if s == "" {
			return s
		}
		out, fs := red.Redact(s)
		n += len(fs)
		return truncate(out, l.opts.MaxFieldBytes)
	}
	if rec.Input != "" {
		in := []byte(rec.Input)
		if json.Valid(in) {
			if out, fs, err := red.RedactJSON(in); err == nil {
				n += len(fs)
				var b bytes.Buffer
				if json.Compact(&b, out) == nil {
					out = b.Bytes()
				}
				rec.Input = truncate(string(out), l.opts.MaxFieldBytes)
			}
		} else {
			rec.Input = clean(rec.Input)
		}
	}
	rec.Reason = clean(rec.Reason)
	rec.Error = clean(rec.Error)
	if len(rec.Details) > 0 {
		keys := make([]string, 0, len(rec.Details))
		for k := range rec.Details {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		d := make(map[string]string, min(len(keys), maxDetails))
		for i, k := range keys {
			if i == maxDetails {
				d["_truncated"] = strconv.Itoa(len(keys)-maxDetails) + " more details"
				break
			}
			v := rec.Details[k]
			if secrets.IsSecretKey(k) && v != "" {
				v = red.Placeholder(secrets.RuleGenericSecret, v)
				n++
			} else {
				v = clean(v)
			}
			d[truncate(k, 128)] = v
		}
		rec.Details = d
	}
	rec.Redactions += n
}

const maxDetails = 64

// tornTail describes a partial final line cut off after a crash.
type tornTail struct {
	file  string
	bytes int64
}

// syncHead makes l.f the active segment for now's day and loads the chain
// head, picking up records other processes appended since our last write.
// The cross-process lock must be held.
func (l *FileLogger) syncHead(now time.Time) (*tornTail, error) {
	day := now.UTC().Format(time.DateOnly)
	if l.f != nil && l.day == day {
		// Another process may have rotated to a newer segment.
		if _, err := os.Stat(filepath.Join(l.opts.Dir, segmentName(day, l.idx+1))); err == nil {
			l.closeFile()
		}
	} else {
		l.closeFile()
	}
	if l.f == nil {
		if err := l.openActive(day); err != nil {
			return nil, err
		}
	}
	fi, err := l.f.Stat()
	if err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	if l.known && fi.Size() == l.size {
		return nil, nil
	}
	return l.loadHead(fi.Size())
}

// openActive opens the newest segment for day, creating the first one.
func (l *FileLogger) openActive(day string) error {
	segs, err := listSegments(l.opts.Dir)
	if err != nil {
		return err
	}
	idx := 0
	for _, s := range segs {
		if s.day == day && s.idx > idx {
			idx = s.idx
		}
	}
	f, err := openSegment(filepath.Join(l.opts.Dir, segmentName(day, idx)))
	if err != nil {
		return err
	}
	l.f, l.day, l.idx, l.known = f, day, idx, false
	return nil
}

func openSegment(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	if runtime.GOOS != "windows" {
		if fi, err := f.Stat(); err == nil && fi.Mode().Perm()&0o077 != 0 {
			if err := f.Chmod(0o600); err != nil {
				f.Close()
				return nil, fmt.Errorf("audit: restrict %s: %w", path, err)
			}
		}
	}
	return f, nil
}

// loadHead reads the chain head from the active segment, or from earlier
// segments when it is empty, and cuts off a torn final line.
func (l *FileLogger) loadHead(size int64) (*tornTail, error) {
	var torn *tornTail
	if size > 0 {
		line, cut, err := lastLine(l.f, size)
		if err != nil {
			return nil, err
		}
		if cut > 0 {
			if err := l.f.Truncate(size - cut); err != nil {
				return nil, fmt.Errorf("audit: repair torn record: %w", err)
			}
			size -= cut
			torn = &tornTail{file: l.f.Name(), bytes: cut}
		}
		if line != nil {
			seq, h, err := parseHead(line)
			if err != nil {
				return nil, fmt.Errorf("audit: %s: last record unreadable: %w", filepath.Base(l.f.Name()), err)
			}
			l.seq, l.last, l.size, l.known = seq, h, size, true
			return torn, nil
		}
	}
	// The active segment is empty: continue from the newest earlier one.
	segs, err := listSegments(l.opts.Dir)
	if err != nil {
		return nil, err
	}
	l.seq, l.last = 0, Genesis
	for i := len(segs) - 1; i >= 0; i-- {
		s := segs[i]
		if !s.before(l.day, l.idx) {
			continue
		}
		seq, h, found, err := segmentHead(filepath.Join(l.opts.Dir, s.name))
		if err != nil {
			return nil, err
		}
		if found {
			l.seq, l.last = seq, h
			break
		}
	}
	l.size, l.known = size, true
	return torn, nil
}

func segmentHead(path string) (uint64, string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", false, fmt.Errorf("audit: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, "", false, err
	}
	line, _, err := lastLine(f, fi.Size())
	if err != nil || line == nil {
		return 0, "", false, err
	}
	seq, h, err := parseHead(line)
	if err != nil {
		return 0, "", false, fmt.Errorf("audit: %s: last record unreadable: %w", filepath.Base(path), err)
	}
	return seq, h, true, nil
}

// lastLine returns the last complete line of f (without its newline) and
// the size of any partial line after it. It reads the tail in growing
// chunks, so it costs little however large the file is.
func lastLine(f *os.File, size int64) ([]byte, int64, error) {
	n := int64(64 << 10)
	for {
		start := max(0, size-n)
		buf := make([]byte, size-start)
		if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
			return nil, 0, fmt.Errorf("audit: %w", err)
		}
		nl := bytes.LastIndexByte(buf, '\n')
		if nl < 0 {
			if start == 0 {
				return nil, size, nil
			}
			n *= 2
			continue
		}
		cut := int64(len(buf) - nl - 1)
		body := buf[:nl]
		if prev := bytes.LastIndexByte(body, '\n'); prev >= 0 {
			return body[prev+1:], cut, nil
		}
		if start == 0 {
			return body, cut, nil
		}
		n *= 2
	}
}

func parseHead(line []byte) (uint64, string, error) {
	var h struct {
		Seq  uint64 `json:"seq"`
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal(line, &h); err != nil {
		return 0, "", err
	}
	if !hashPattern.MatchString(h.Hash) || h.Seq == 0 {
		return 0, "", errors.New("missing seq or hash")
	}
	return h.Seq, h.Hash, nil
}

var hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (l *FileLogger) writeRecovered(t *tornTail) error {
	rec := Record{Event: EventRecovered, Reason: "cut off a partial record left by an interrupted write",
		Details: map[string]string{"file": filepath.Base(t.file), "discarded_bytes": strconv.FormatInt(t.bytes, 10)}}
	l.prepare(&rec)
	return l.appendLocked(rec)
}

// appendLocked chains, serializes and writes rec. The lock must be held and
// the head loaded.
func (l *FileLogger) appendLocked(rec Record) error {
	rec.V = RecordVersion
	rec.Seq = l.seq + 1
	rec.PrevHash = l.last
	rec.Alg = l.alg
	rec.Hash = ""
	line, sum, err := seal(rec, l.opts.HMACKey)
	if err != nil {
		return err
	}
	if l.size > 0 && l.size+int64(len(line)) > l.opts.MaxFileBytes {
		if err := l.rotate(); err != nil {
			return err
		}
	}
	n, err := l.f.Write(line)
	if err != nil {
		// Do not leave half a record behind.
		if n > 0 {
			_ = l.f.Truncate(l.size)
		}
		return fmt.Errorf("audit: write: %w", err)
	}
	l.size += int64(n)
	l.seq, l.last = rec.Seq, sum
	switch l.opts.Sync {
	case SyncAlways:
		if err := l.f.Sync(); err != nil {
			return fmt.Errorf("audit: sync: %w", err)
		}
	case SyncInterval:
		l.dirty = true
	}
	return nil
}

func (l *FileLogger) rotate() error {
	if err := l.f.Sync(); err != nil {
		return fmt.Errorf("audit: sync: %w", err)
	}
	l.dirty = false
	l.closeFile()
	f, err := openSegment(filepath.Join(l.opts.Dir, segmentName(l.day, l.idx+1)))
	if err != nil {
		return err
	}
	l.f, l.idx, l.size = f, l.idx+1, 0
	if l.opts.Sync == SyncAlways {
		syncDir(l.opts.Dir)
	}
	return nil
}

// syncDir makes a new directory entry durable where the platform allows.
func syncDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

// seal serializes rec and appends its hash as the final field.
func seal(rec Record, key []byte) ([]byte, string, error) {
	body, err := json.Marshal(rec)
	if err != nil {
		return nil, "", fmt.Errorf("audit: encode: %w", err)
	}
	sum := digest(rec.Alg, key, body)
	line := make([]byte, 0, len(body)+80)
	line = append(line, body[:len(body)-1]...)
	line = append(line, `,"hash":"`...)
	line = append(line, sum...)
	line = append(line, "\"}\n"...)
	return line, sum, nil
}

func digest(alg string, key, body []byte) string {
	var h hash.Hash
	if alg == AlgHMACSHA256 {
		h = hmac.New(sha256.New, key)
	} else {
		h = sha256.New()
	}
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

type segment struct {
	day  string
	idx  int
	name string
}

func (s segment) before(day string, idx int) bool {
	return s.day < day || (s.day == day && s.idx < idx)
}

var segmentPattern = regexp.MustCompile(`^audit-(\d{4}-\d{2}-\d{2})(?:\.(\d{4,9}))?\.jsonl$`)

func segmentName(day string, idx int) string {
	if idx == 0 {
		return "audit-" + day + ".jsonl"
	}
	return fmt.Sprintf("audit-%s.%04d.jsonl", day, idx)
}

// listSegments returns the log segments in dir in chain order.
func listSegments(dir string) ([]segment, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("audit: %w", err)
	}
	var out []segment
	for _, e := range ents {
		m := segmentPattern.FindStringSubmatch(e.Name())
		if m == nil || !e.Type().IsRegular() {
			continue
		}
		idx := 0
		if m[2] != "" {
			idx, _ = strconv.Atoi(m[2])
		}
		out = append(out, segment{day: m[1], idx: idx, name: e.Name()})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].day != out[j].day {
			return out[i].day < out[j].day
		}
		return out[i].idx < out[j].idx
	})
	return out, nil
}
