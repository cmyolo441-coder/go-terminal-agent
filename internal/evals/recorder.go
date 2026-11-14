package evals

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

// Mode selects how a Recorder treats the wrapped provider.
type Mode string

const (
	// ModeRecord calls the real provider for every request and records
	// each call, replacing any previous cassette contents.
	ModeRecord Mode = "record"
	// ModeReplay serves every request from the cassette and never calls
	// the real provider; an unmatched request is an error.
	ModeReplay Mode = "replay"
	// ModeRecordMissing replays matched requests and records the rest,
	// appending them to the cassette.
	ModeRecordMissing Mode = "record-missing"
	// ModePassthrough calls the real provider and records nothing.
	ModePassthrough Mode = "passthrough"
)

// ParseMode parses a mode name. Underscores are accepted for hyphens.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "_", "-")); m {
	case ModeRecord, ModeReplay, ModeRecordMissing, ModePassthrough:
		return m, nil
	case "off", "none":
		return ModePassthrough, nil
	}
	return "", fmt.Errorf("evals: unknown cassette mode %q (want record, replay, record-missing or passthrough)", s)
}

// ErrNoMatch is matched (with errors.Is) by every replay miss.
var ErrNoMatch = errors.New("evals: no recorded interaction matches the request")

// ErrNoProvider is returned when a mode needs the real provider but the
// recorder was built without one.
var ErrNoProvider = errors.New("evals: this cassette mode needs a real provider, but none was configured")

// MismatchError explains a replay miss with a structural diff against the
// closest recorded request.
type MismatchError struct {
	// Hash is the canonical hash of the request that was not found.
	Hash string
	// Recorded is how many interactions the cassette holds.
	Recorded int
	// ClosestSeq is the sequence number of the closest interaction, or -1
	// when the cassette is empty.
	ClosestSeq  int
	ClosestHash string
	// Diffs lists how the closest recorded request differs from the
	// actual one ("recorded" is the cassette side).
	Diffs []Difference
}

func (e *MismatchError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "evals: no recorded interaction matches request %s (cassette holds %d)", shortHash(e.Hash), e.Recorded)
	if e.ClosestSeq < 0 {
		return b.String()
	}
	fmt.Fprintf(&b, "; closest is #%d (%s) with %d difference(s):\n", e.ClosestSeq, shortHash(e.ClosestHash), len(e.Diffs))
	b.WriteString(formatDiffs(e.Diffs, 25))
	return strings.TrimRight(b.String(), "\n")
}

// Is makes errors.Is(err, ErrNoMatch) true.
func (e *MismatchError) Is(target error) bool { return target == ErrNoMatch }

// ReplayedError is a provider error served from a cassette.
type ReplayedError struct{ Message string }

func (e *ReplayedError) Error() string { return e.Message }

// RecorderOptions configure a Recorder.
type RecorderOptions struct {
	// Canonical controls request matching.
	Canonical CanonicalOptions
	// Scrub, when set, is applied to every string before it is hashed or
	// written: request content, streamed text, tool inputs and responses.
	// It must be deterministic. Wire it to a secrets redactor.
	Scrub func(string) string
	// RecordErrors also records provider failures (other than
	// cancellation) so they replay. Off by default because transient
	// errors usually should not become part of a fixture.
	RecordErrors bool
	// StrictReplay makes a request that has already consumed all of its
	// recordings in a session fail instead of re-serving the last one.
	StrictReplay bool
	// Now is the clock used to measure call duration; nil uses time.Now.
	Now func() time.Time
}

// Replacement substitutes a run-specific string (such as a temporary
// workspace path) with a stable placeholder in everything recorded and
// hashed, and reverses the substitution in replayed output.
type Replacement struct {
	Actual      string
	Placeholder string
}

// Recorder wraps a provider with a cassette. It implements llm.Provider
// itself (through a default session with no replacements); use Session to
// get an independent replay cursor per agent run.
type Recorder struct {
	mode  Mode
	inner llm.Provider
	cas   *Cassette
	opts  RecorderOptions

	mu    sync.Mutex
	index map[string][]int
	file  *os.File

	def *Session
}

// NewRecorder wraps inner with an in-memory cassette. inner may be nil in
// ModeReplay.
func NewRecorder(mode Mode, cas *Cassette, inner llm.Provider, opts RecorderOptions) (*Recorder, error) {
	if _, err := ParseMode(string(mode)); err != nil {
		return nil, err
	}
	if cas == nil {
		name := ""
		if inner != nil {
			name = inner.Name()
		}
		cas = NewCassette(name)
	}
	if mode != ModeReplay && inner == nil {
		return nil, ErrNoProvider
	}
	r := &Recorder{mode: mode, inner: inner, cas: cas, opts: opts}
	if err := r.reindex(); err != nil {
		return nil, err
	}
	r.def = r.Session()
	return r, nil
}

// OpenRecorder opens (or creates) a cassette file for the mode:
// record truncates it, record-missing appends to it (creating it when
// missing), replay requires it, passthrough does not touch it.
func OpenRecorder(path string, mode Mode, inner llm.Provider, opts RecorderOptions) (*Recorder, error) {
	if _, err := ParseMode(string(mode)); err != nil {
		return nil, err
	}
	providerName := ""
	if inner != nil {
		providerName = inner.Name()
	}
	var (
		cas *Cassette
		f   *os.File
		err error
	)
	switch mode {
	case ModePassthrough:
		cas = NewCassette(providerName)
	case ModeReplay:
		if cas, err = LoadCassette(path); err != nil {
			return nil, err
		}
	case ModeRecord:
		if err = os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		if f, err = os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|os.O_APPEND, 0o644); err != nil {
			return nil, err
		}
		cas = NewCassette(providerName)
		if err = writeHeader(f, cas); err != nil {
			f.Close()
			return nil, err
		}
	case ModeRecordMissing:
		cas, err = LoadCassette(path)
		switch {
		case err == nil:
			if f, err = os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644); err != nil {
				return nil, err
			}
		case errors.Is(err, os.ErrNotExist):
			if err = os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return nil, err
			}
			if f, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o644); err != nil {
				return nil, err
			}
			cas = NewCassette(providerName)
			if err = writeHeader(f, cas); err != nil {
				f.Close()
				return nil, err
			}
		default:
			return nil, err
		}
	}
	if f != nil {
		cas.mu.Lock()
		cas.sink = f
		cas.mu.Unlock()
	}
	r, err := NewRecorder(mode, cas, inner, opts)
	if err != nil {
		if f != nil {
			f.Close()
		}
		return nil, err
	}
	r.file = f
	return r, nil
}

func writeHeader(w io.Writer, c *Cassette) error {
	line, err := encodeLine(c.Header())
	if err != nil {
		return err
	}
	_, err = w.Write(line)
	return err
}

// Close flushes and closes the cassette file, if any.
func (r *Recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return nil
	}
	r.cas.mu.Lock()
	r.cas.sink = nil
	r.cas.mu.Unlock()
	err := r.file.Sync()
	if cerr := r.file.Close(); err == nil {
		err = cerr
	}
	r.file = nil
	return err
}

// Mode reports the recorder's mode.
func (r *Recorder) Mode() Mode { return r.mode }

// Cassette returns the underlying cassette.
func (r *Recorder) Cassette() *Cassette { return r.cas }

// Name is the wrapped provider's name, or the recorded one when replaying
// without a provider.
func (r *Recorder) Name() string {
	if r.inner != nil {
		return r.inner.Name()
	}
	if n := r.cas.Header().Provider; n != "" {
		return n
	}
	return "cassette"
}

// Stream implements llm.Provider through the default session.
func (r *Recorder) Stream(ctx context.Context, req llm.Request, onEvent func(llm.Event)) (*llm.Response, error) {
	return r.def.Stream(ctx, req, onEvent)
}

// reindex hashes every recorded request under the current options.
func (r *Recorder) reindex() error {
	idx := map[string][]int{}
	for _, it := range r.cas.Interactions() {
		h, err := hashTree(it.Request, r.opts.Canonical.IgnoreFields)
		if err != nil {
			return fmt.Errorf("evals: hash interaction %d: %w", it.Seq, err)
		}
		idx[h] = append(idx[h], it.Seq)
	}
	r.mu.Lock()
	r.index = idx
	r.mu.Unlock()
	return nil
}

func (r *Recorder) lookup(hash string) []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.index[hash]
}

// Session returns an independent provider view with its own replay cursor.
// Replacements are applied longest-first.
func (r *Recorder) Session(replacements ...Replacement) *Session {
	reps := make([]Replacement, 0, len(replacements))
	for _, rp := range replacements {
		if rp.Actual != "" && rp.Placeholder != "" && rp.Actual != rp.Placeholder {
			reps = append(reps, rp)
		}
	}
	sort.SliceStable(reps, func(i, j int) bool { return len(reps[i].Actual) > len(reps[j].Actual) })
	return &Session{r: r, reps: reps, cursor: map[string]int{}}
}

// Session is one agent run's view of a Recorder. It implements llm.Provider
// and is safe for concurrent use.
type Session struct {
	r    *Recorder
	reps []Replacement

	mu     sync.Mutex
	cursor map[string]int
}

// Name implements llm.Provider.
func (s *Session) Name() string { return s.r.Name() }

// outbound scrubs secrets and swaps run-specific strings for placeholders.
func (s *Session) outbound(v string) string {
	if v == "" {
		return v
	}
	if s.r.opts.Scrub != nil {
		v = s.r.opts.Scrub(v)
	}
	for _, rp := range s.reps {
		v = strings.ReplaceAll(v, rp.Actual, rp.Placeholder)
	}
	return v
}

// inbound restores run-specific strings in replayed content.
func (s *Session) inbound(v string) string {
	if v == "" {
		return v
	}
	for i := len(s.reps) - 1; i >= 0; i-- {
		v = strings.ReplaceAll(v, s.reps[i].Placeholder, s.reps[i].Actual)
	}
	return v
}

// Hash returns the canonical hash this session computes for req.
func (s *Session) Hash(req llm.Request) (string, error) {
	_, h, err := s.canonical(req)
	return h, err
}

func (s *Session) canonical(req llm.Request) (map[string]any, string, error) {
	tree := canonicalRequest(req, s.r.opts.Canonical, s.outbound)
	h, err := hashTree(tree, s.r.opts.Canonical.IgnoreFields)
	return tree, h, err
}

// Stream implements llm.Provider.
func (s *Session) Stream(ctx context.Context, req llm.Request, onEvent func(llm.Event)) (*llm.Response, error) {
	if onEvent == nil {
		onEvent = func(llm.Event) {}
	}
	if s.r.mode == ModePassthrough {
		return s.r.inner.Stream(ctx, req, onEvent)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tree, hash, err := s.canonical(req)
	if err != nil {
		return nil, fmt.Errorf("evals: canonicalise request: %w", err)
	}
	switch s.r.mode {
	case ModeRecord:
		return s.record(ctx, req, tree, hash, onEvent)
	case ModeRecordMissing:
		if seq, ok := s.next(hash); ok {
			return s.replay(ctx, seq, onEvent)
		}
		return s.record(ctx, req, tree, hash, onEvent)
	default:
		seq, ok := s.next(hash)
		if !ok {
			if n := len(s.r.lookup(hash)); n > 0 {
				return nil, fmt.Errorf("%w: all %d recording(s) of request %s were already used in this session", ErrNoMatch, n, shortHash(hash))
			}
			return nil, s.r.mismatch(tree, hash)
		}
		return s.replay(ctx, seq, onEvent)
	}
}

// next picks the recording to serve for hash and advances the session's
// cursor. Once every recording has been used, the last one is served
// again unless StrictReplay is set.
func (s *Session) next(hash string) (int, bool) {
	seqs := s.r.lookup(hash)
	if len(seqs) == 0 {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.cursor[hash]
	if n >= len(seqs) {
		if s.r.opts.StrictReplay {
			return 0, false
		}
		n = len(seqs) - 1
	}
	s.cursor[hash] = n + 1
	return seqs[n], true
}

func (s *Session) replay(ctx context.Context, seq int, onEvent func(llm.Event)) (*llm.Response, error) {
	it := s.r.cas.interaction(seq)
	for _, rev := range it.Events {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ev, err := rev.event(s.inbound)
		if err != nil {
			return nil, fmt.Errorf("evals: interaction %d: %w", it.Seq, err)
		}
		onEvent(ev)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if it.Error != "" {
		return nil, &ReplayedError{Message: s.inbound(it.Error)}
	}
	return it.Response.response(s.inbound), nil
}

func (s *Session) record(ctx context.Context, req llm.Request, tree map[string]any, hash string, onEvent func(llm.Event)) (*llm.Response, error) {
	if s.r.inner == nil {
		return nil, ErrNoProvider
	}
	var (
		evMu   sync.Mutex
		events []RecordedEvent
	)
	capture := func(ev llm.Event) {
		evMu.Lock()
		events = append(events, recordEvent(ev, s.outbound))
		evMu.Unlock()
		onEvent(ev)
	}
	now := s.r.opts.Now
	if now == nil {
		now = time.Now
	}
	start := now()
	resp, err := s.r.inner.Stream(ctx, req, capture)
	elapsed := now().Sub(start)
	evMu.Lock()
	defer evMu.Unlock()
	it := &Interaction{Hash: hash, Request: tree, Events: events, DurationMS: elapsed.Milliseconds()}
	switch {
	case err != nil:
		if !s.r.opts.RecordErrors || ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		msg := s.outbound(err.Error())
		if msg == "" {
			msg = "provider error"
		}
		it.Error = msg
	case resp == nil:
		return nil, errors.New("evals: provider returned neither a response nor an error")
	default:
		it.Response = recordResponse(resp, s.outbound)
	}
	seq, werr := s.r.cas.add(it)
	if werr != nil {
		if err != nil {
			return nil, errors.Join(err, werr)
		}
		return nil, werr
	}
	s.r.mu.Lock()
	s.r.index[hash] = append(s.r.index[hash], seq)
	s.r.mu.Unlock()
	// The live caller has already seen this call; later identical requests
	// in this session should replay what follows it.
	s.mu.Lock()
	s.cursor[hash] = len(s.r.lookup(hash))
	s.mu.Unlock()
	return resp, err
}

// mismatch builds a diagnostic against the closest recorded request.
func (r *Recorder) mismatch(tree map[string]any, hash string) error {
	all := r.cas.Interactions()
	e := &MismatchError{Hash: hash, Recorded: len(all), ClosestSeq: -1}
	if len(all) == 0 {
		return e
	}
	ignore := r.opts.Canonical.IgnoreFields
	actual := stripIgnored(tree, ignore)
	best := -1
	var bestDiffs []Difference
	for _, it := range all {
		d := structuralDiff(stripIgnored(it.Request, ignore), actual)
		if best < 0 || len(d) < len(bestDiffs) {
			best, bestDiffs = it.Seq, d
		}
	}
	e.ClosestSeq = best
	e.Diffs = bestDiffs
	if h, err := hashTree(all[best].Request, ignore); err == nil {
		e.ClosestHash = h
	}
	return e
}

func stripIgnored(tree any, ignore []string) any {
	t := cloneTree(tree)
	for _, p := range ignore {
		if p = strings.TrimSpace(p); p != "" {
			t = deletePath(t, strings.Split(p, "."))
		}
	}
	return t
}

// ParseCassetteSpec parses a GTA_CASSETTE style value: "mode:path" with a
// mode accepted by ParseMode, or a bare path, which means replay (the safe
// default for CI: it never reaches the network).
func ParseCassetteSpec(spec string) (Mode, string, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "", "", errors.New("evals: empty cassette spec")
	}
	if i := strings.IndexByte(spec, ':'); i > 0 {
		if m, err := ParseMode(spec[:i]); err == nil {
			path := strings.TrimSpace(spec[i+1:])
			if path == "" && m != ModePassthrough {
				return "", "", fmt.Errorf("evals: cassette spec %q has no path", spec)
			}
			return m, path, nil
		}
	}
	if m, err := ParseMode(spec); err == nil && m == ModePassthrough {
		return m, "", nil
	}
	return ModeReplay, spec, nil
}

// WrapProvider wraps inner according to a cassette spec (see
// ParseCassetteSpec). An empty spec returns inner unchanged. The returned
// closer must be called to flush the cassette.
func WrapProvider(spec string, inner llm.Provider, opts RecorderOptions) (llm.Provider, io.Closer, error) {
	if strings.TrimSpace(spec) == "" {
		return inner, nopCloser{}, nil
	}
	mode, path, err := ParseCassetteSpec(spec)
	if err != nil {
		return nil, nil, err
	}
	if mode == ModePassthrough {
		return inner, nopCloser{}, nil
	}
	r, err := OpenRecorder(path, mode, inner, opts)
	if err != nil {
		return nil, nil, err
	}
	return r, r, nil
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }
