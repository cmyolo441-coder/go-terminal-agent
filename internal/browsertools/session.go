// Package browsertools implements the tools that let the model drive a real
// browser the way a person does: read the page as an outline of its
// interactive elements, then click, fill forms, type, select, scroll, hover,
// press keys, wait and extract text.
//
// The browser itself sits behind the Driver interface (a CDP engine
// satisfies it). A Session wraps one Driver, obtained lazily on first use,
// and adds what the tools need on top: element refs from the last snapshot,
// the visible cursor position, human-like pointer motion and typing for the
// live view, and an action log for the UI and audit trail.
//
// Human-like motion here exists so a person watching the live view can
// follow what the agent does. It is not an attempt to evade bot detection:
// nothing in this package spoofs fingerprints or hides automation.
package browsertools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"time"
)

// Driver is the browser engine the tools drive. Coordinates are CSS pixels
// in the top-level viewport. Keys are DOM KeyboardEvent.key values
// ("Enter", "Control", "ArrowDown", "a"). Evaluate runs a JavaScript
// expression in the main frame and returns its JSON-serialized value.
type Driver interface {
	Navigate(ctx context.Context, url string) error
	Reload(ctx context.Context) error
	Back(ctx context.Context) error
	Forward(ctx context.Context) error
	URL() string
	Title(ctx context.Context) (string, error)
	Evaluate(ctx context.Context, expr string) (json.RawMessage, error)
	Screenshot(ctx context.Context, format string, quality int) ([]byte, error)
	Viewport() (w, h int)
	MouseMove(ctx context.Context, x, y float64) error
	MouseDown(ctx context.Context, x, y float64, button string, clickCount int) error
	MouseUp(ctx context.Context, x, y float64, button string, clickCount int) error
	Wheel(ctx context.Context, x, y, dx, dy float64) error
	KeyDown(ctx context.Context, key string) error
	KeyUp(ctx context.Context, key string) error
	InsertText(ctx context.Context, text string) error
	WaitIdle(ctx context.Context, quiet time.Duration) error
}

// Provider returns the Driver a Session uses. It is called on first use
// and again after a failure, never concurrently.
type Provider func(ctx context.Context) (Driver, error)

// Speed sets how human-like interaction is paced.
type Speed string

const (
	// SpeedNatural moves the cursor along curved paths and types with a
	// human cadence, so the live view is easy to follow.
	SpeedNatural Speed = "natural"
	// SpeedInstant skips every human delay: for tests and headless runs.
	SpeedInstant Speed = "instant"
)

// Defaults for Options fields left zero.
const (
	DefaultWPM              = 90
	DefaultMaxSnapshotChars = 12000
	DefaultMaxSnapshotNodes = 600
	DefaultMaxDiffChars     = 3000
	DefaultActionLogSize    = 500
	DefaultIdleQuiet        = 500 * time.Millisecond
	DefaultNavTimeout       = 30 * time.Second
	DefaultActionTimeout    = 60 * time.Second
)

// Options configure a Session.
type Options struct {
	// Speed is SpeedNatural (the default) or SpeedInstant.
	Speed Speed
	// WPM is the typing speed in words (five characters) per minute.
	WPM int
	// Seed seeds the motion and typing randomness; zero picks one from
	// the clock. Tests set it for repeatable paths.
	Seed uint64
	// MaxSnapshotChars caps a rendered snapshot.
	MaxSnapshotChars int
	// MaxSnapshotNodes caps the elements one snapshot collects.
	MaxSnapshotNodes int
	// MaxDiffChars caps the change summary appended to action results.
	MaxDiffChars int
	// ActionLogSize is how many actions the log keeps.
	ActionLogSize int
	// IdleQuiet is how long the network must be quiet to count as idle.
	IdleQuiet time.Duration
	// NavTimeout bounds waiting for a page to settle after navigation.
	NavTimeout time.Duration
	// ActionTimeout bounds one tool action as a whole.
	ActionTimeout time.Duration
	// DisableEval leaves browser_eval out of Tools.
	DisableEval bool
	// Sleep replaces real waiting (human pauses and polling); tests use it
	// to run without wall-clock delays. It must honor ctx.
	Sleep func(ctx context.Context, d time.Duration) error
	// Now replaces time.Now for timestamps and wait deadlines.
	Now func() time.Time
}

// Action is one entry of the action log.
type Action struct {
	Seq  int       `json:"seq"`
	Time time.Time `json:"time"`
	// Kind is the action: navigate, click, hover, type, press, fill,
	// select, check, scroll, wait, snapshot, extract, screenshot, eval,
	// back, forward, reload.
	Kind string `json:"kind"`
	// Target describes the element or URL acted on.
	Target string `json:"target,omitempty"`
	// X and Y are the pointer coordinates for pointer actions.
	X        float64 `json:"x,omitempty"`
	Y        float64 `json:"y,omitempty"`
	HasPoint bool    `json:"has_point,omitempty"`
	Detail   string  `json:"detail,omitempty"`
	Err      string  `json:"error,omitempty"`
}

// RefInfo is what the last snapshot knew about an element ref.
type RefInfo struct {
	Role string
	Name string
}

// Point is a viewport position.
type Point struct{ X, Y float64 }

// Session is one browser driven by the tools. Its methods are safe for
// concurrent use; actions are serialized.
type Session struct {
	provider Provider
	opts     Options

	initMu sync.Mutex
	drv    Driver

	// op serializes actions so two tool calls never interleave events.
	op  sync.Mutex
	rng *rand.Rand // guarded by op

	mu          sync.Mutex
	cursor      Point
	cursorKnown bool
	refs        map[string]RefInfo
	last        *Snapshot
	log         []Action
	seq         int
	onAction    func(Action)
}

// NewSession returns a session that obtains its Driver from provider on
// first use.
func NewSession(provider Provider, opts Options) *Session {
	if opts.Speed == "" {
		opts.Speed = SpeedNatural
	}
	if opts.WPM <= 0 {
		opts.WPM = DefaultWPM
	}
	if opts.MaxSnapshotChars <= 0 {
		opts.MaxSnapshotChars = DefaultMaxSnapshotChars
	}
	if opts.MaxSnapshotNodes <= 0 {
		opts.MaxSnapshotNodes = DefaultMaxSnapshotNodes
	}
	if opts.MaxDiffChars <= 0 {
		opts.MaxDiffChars = DefaultMaxDiffChars
	}
	if opts.ActionLogSize <= 0 {
		opts.ActionLogSize = DefaultActionLogSize
	}
	if opts.IdleQuiet <= 0 {
		opts.IdleQuiet = DefaultIdleQuiet
	}
	if opts.NavTimeout <= 0 {
		opts.NavTimeout = DefaultNavTimeout
	}
	if opts.ActionTimeout <= 0 {
		opts.ActionTimeout = DefaultActionTimeout
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Sleep == nil {
		opts.Sleep = sleepCtx
	}
	seed := opts.Seed
	if seed == 0 {
		seed = uint64(time.Now().UnixNano())
	}
	return &Session{
		provider: provider,
		opts:     opts,
		rng:      rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)),
		refs:     map[string]RefInfo{},
	}
}

// Options returns the session's effective options.
func (s *Session) Options() Options { return s.opts }

// Driver returns the session's driver, obtaining it on first use. A
// failed attempt is not cached, so a later call tries again.
func (s *Session) Driver(ctx context.Context) (Driver, error) {
	s.initMu.Lock()
	defer s.initMu.Unlock()
	if s.drv != nil {
		return s.drv, nil
	}
	if s.provider == nil {
		return nil, errors.New("no browser is configured")
	}
	d, err := s.provider(ctx)
	if err != nil {
		return nil, fmt.Errorf("start browser: %w", err)
	}
	if d == nil {
		return nil, errors.New("start browser: the provider returned no driver")
	}
	s.drv = d
	return d, nil
}

// Started reports whether a driver has been obtained.
func (s *Session) Started() bool {
	s.initMu.Lock()
	defer s.initMu.Unlock()
	return s.drv != nil
}

// Cursor returns the last pointer position and whether it is known.
func (s *Session) Cursor() (Point, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursor, s.cursorKnown
}

func (s *Session) setCursor(p Point) {
	s.mu.Lock()
	s.cursor, s.cursorKnown = p, true
	s.mu.Unlock()
}

// Refs returns the element refs of the last snapshot.
func (s *Session) Refs() map[string]RefInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]RefInfo, len(s.refs))
	for k, v := range s.refs {
		out[k] = v
	}
	return out
}

// LastSnapshot returns the most recent snapshot, or nil.
func (s *Session) LastSnapshot() *Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

func (s *Session) setSnapshot(snap *Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last = snap
	if snap == nil {
		s.refs = map[string]RefInfo{}
		return
	}
	if snap.Scope != "" {
		// A scoped snapshot adds to what is known; it does not prove the
		// rest of the page's refs gone.
		for _, n := range snap.Nodes {
			s.refs[n.Ref] = RefInfo{Role: n.Role, Name: n.Name}
		}
		return
	}
	s.refs = make(map[string]RefInfo, len(snap.Nodes))
	for _, n := range snap.Nodes {
		s.refs[n.Ref] = RefInfo{Role: n.Role, Name: n.Name}
	}
}

// invalidateBaseline forgets the last snapshot as a diff baseline while
// keeping its refs, after something changed the page out of band.
func (s *Session) invalidateBaseline() {
	s.mu.Lock()
	s.last = nil
	s.mu.Unlock()
}

// OnAction registers a callback run after each logged action, for a live
// UI. It runs synchronously and must not block.
func (s *Session) OnAction(f func(Action)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onAction = f
}

// Actions returns a copy of the action log, oldest first.
func (s *Session) Actions() []Action {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Action(nil), s.log...)
}

func (s *Session) record(a Action) {
	s.mu.Lock()
	s.seq++
	a.Seq = s.seq
	if a.Time.IsZero() {
		a.Time = s.opts.Now()
	}
	s.log = append(s.log, a)
	if over := len(s.log) - s.opts.ActionLogSize; over > 0 {
		s.log = append(s.log[:0:0], s.log[over:]...)
	}
	f := s.onAction
	s.mu.Unlock()
	if f != nil {
		f(a)
	}
}

// natural reports whether human pacing is on.
func (s *Session) natural() bool { return s.opts.Speed != SpeedInstant }

// pause waits for d in natural speed and not at all in instant speed.
func (s *Session) pause(ctx context.Context, d time.Duration) error {
	if !s.natural() || d <= 0 {
		return ctx.Err()
	}
	return s.opts.Sleep(ctx, d)
}

// jitterDur returns a duration uniformly in [lo, hi].
func (s *Session) jitterDur(lo, hi time.Duration) time.Duration {
	if hi <= lo {
		return lo
	}
	return lo + time.Duration(s.rng.Int64N(int64(hi-lo)+1))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ParseSpeed parses a speed name: natural (or human) and instant (or fast).
func ParseSpeed(s string) (Speed, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "natural", "human":
		return SpeedNatural, nil
	case "instant", "fast", "none":
		return SpeedInstant, nil
	}
	return "", fmt.Errorf("unknown browser speed %q (want natural or instant)", s)
}
