package browsertools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Target identifies an element: by ref from the last snapshot, by CSS
// selector (first visible match, piercing open shadow roots and same-origin
// frames), or by visible text / accessible name. Exactly one is set.
type Target struct {
	Ref      string
	Selector string
	Text     string
}

var refPattern = regexp.MustCompile(`^e[1-9][0-9]{0,8}$`)

// IsZero reports whether no field is set.
func (t Target) IsZero() bool { return t.Ref == "" && t.Selector == "" && t.Text == "" }

// String describes the target for logs and messages.
func (t Target) String() string {
	switch {
	case t.Ref != "":
		return t.Ref
	case t.Selector != "":
		return "selector " + quote(t.Selector)
	case t.Text != "":
		return "text " + quote(t.Text)
	}
	return "(no target)"
}

// Validate checks that exactly one well-formed field is set.
func (t Target) Validate() error {
	n := 0
	for _, f := range []string{t.Ref, t.Selector, t.Text} {
		if f != "" {
			n++
		}
	}
	if n == 0 {
		return errors.New("an element is required: give ref (from browser_snapshot), selector or match_text")
	}
	if n > 1 {
		return errors.New("give only one of ref, selector and match_text")
	}
	switch {
	case t.Ref != "":
		if !refPattern.MatchString(t.Ref) {
			return fmt.Errorf("ref %q is not a snapshot ref like e12", t.Ref)
		}
	case t.Selector != "":
		if strings.TrimSpace(t.Selector) == "" {
			return errors.New("selector is blank")
		}
		if len(t.Selector) > 1000 {
			return errors.New("selector is longer than 1000 bytes")
		}
	case t.Text != "":
		if strings.TrimSpace(t.Text) == "" {
			return errors.New("match_text is blank")
		}
		if utf8.RuneCountInString(t.Text) > 200 {
			return errors.New("match_text is longer than 200 characters")
		}
	}
	return nil
}

func (t Target) args() map[string]string {
	switch {
	case t.Ref != "":
		return map[string]string{"ref": t.Ref}
	case t.Selector != "":
		return map[string]string{"selector": t.Selector}
	}
	return map[string]string{"text": t.Text}
}

// element is what the page script reports about a located element.
type element struct {
	Ref       string    `json:"ref"`
	Role      string    `json:"role"`
	Name      string    `json:"name"`
	Tag       string    `json:"tag"`
	Type      string    `json:"type"`
	Box       []float64 `json:"box"`
	Vis       []float64 `json:"vis"`
	VW        float64   `json:"vw"`
	VH        float64   `json:"vh"`
	Visible   bool      `json:"visible"`
	Disabled  bool      `json:"disabled"`
	Readonly  bool      `json:"readonly"`
	Editable  bool      `json:"editable"`
	IsSelect  bool      `json:"isSelect"`
	Multiple  bool      `json:"multiple"`
	Checkable bool      `json:"checkable"`
	Checked   Tri       `json:"checked"`
	Value     *string   `json:"value"`
	Focused   bool      `json:"focused"`
	Mac       bool      `json:"mac"`
	Count     int       `json:"count"`
}

func (e *element) label() string {
	role := e.Role
	if role == "" {
		role = e.Tag
	}
	s := "[" + e.Ref + "] " + role
	if e.Name != "" {
		s += " " + quote(e.Name)
	}
	return s
}

func (e *element) box() Rect { return rectOf(e.Box) }
func (e *element) vis() Rect { return rectOf(e.Vis) }

// inView reports whether enough of the element shows to act on it: most of
// it, or for an element larger than the viewport, a good part of the screen.
func (e *element) inView() bool {
	v := e.vis()
	if v.W < 1 || v.H < 1 {
		return false
	}
	need := math.Min(e.box().Area(), 0.5*e.VW*e.VH)
	return v.Area() >= 0.75*need
}

// fieldState is the page script's view of a control's current state.
type fieldState struct {
	Value    string `json:"value"`
	Masked   bool   `json:"masked"`
	Length   int    `json:"length"`
	Checked  Tri    `json:"checked"`
	Expanded Tri    `json:"expanded"`
	Focused  bool   `json:"focused"`
}

type scrollState struct {
	SX int `json:"sx"`
	SY int `json:"sy"`
	SW int `json:"sw"`
	SH int `json:"sh"`
	VW int `json:"vw"`
	VH int `json:"vh"`
}

// actOut is what one action reports for the result and the log.
type actOut struct {
	summary string
	target  string
	detail  string
	point   *Point
	// full renders the whole page after the action instead of a diff.
	full bool
}

// act runs one action: serialized, time-bounded, logged, and followed by
// a summary of how the page changed when diff is set.
func (s *Session) act(ctx context.Context, kind, target string, diff bool, f func(ctx context.Context, d Driver) (actOut, error)) (string, error) {
	s.op.Lock()
	defer s.op.Unlock()
	ctx, cancel := context.WithTimeout(ctx, s.opts.ActionTimeout)
	defer cancel()
	d, err := s.Driver(ctx)
	if err != nil {
		s.record(Action{Kind: kind, Target: target, Err: err.Error()})
		return "", err
	}
	var before *Snapshot
	urlBefore := d.URL()
	if diff {
		before = s.LastSnapshot()
		if before == nil || before.Scope != "" || stripFragment(before.URL) != stripFragment(urlBefore) {
			before = nil
			if b, err := s.takeSnapshot(ctx, d, SnapshotOptions{}); err == nil {
				before = b
			}
		}
	}
	out, err := f(ctx, d)
	a := Action{Kind: kind, Target: target, Detail: out.detail}
	if out.target != "" {
		a.Target = out.target
	}
	if out.point != nil {
		a.X, a.Y, a.HasPoint = out.point.X, out.point.Y, true
	}
	if err != nil {
		a.Err = err.Error()
		s.record(a)
		if out.summary != "" {
			return "", fmt.Errorf("%w\n(%s)", err, out.summary)
		}
		return "", err
	}
	s.record(a)
	if !diff && !out.full {
		return out.summary, nil
	}
	return out.summary + "\n\n" + s.changes(ctx, d, before, urlBefore, out.full), nil
}

// changes lets the page settle, takes a fresh snapshot and summarizes it
// against before.
func (s *Session) changes(ctx context.Context, d Driver, before *Snapshot, urlBefore string, full bool) string {
	navigated := d.URL() != urlBefore
	if navigated || full {
		wctx, cancel := context.WithTimeout(ctx, min(s.opts.NavTimeout, 15*time.Second))
		_ = d.WaitIdle(wctx, s.opts.IdleQuiet)
		cancel()
	} else {
		wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_ = d.WaitIdle(wctx, 150*time.Millisecond)
		cancel()
	}
	cur, err := s.takeSnapshot(ctx, d, SnapshotOptions{})
	if err != nil {
		s.invalidateBaseline()
		return "(Could not read the page after the action: " + err.Error() + ". Call browser_snapshot.)"
	}
	s.setSnapshot(cur)
	if full {
		return cur.Render(s.opts.MaxSnapshotChars)
	}
	if before == nil || stripFragment(before.URL) != stripFragment(cur.URL) {
		return Diff(nil, cur, s.opts.MaxSnapshotChars)
	}
	return Diff(before, cur, s.opts.MaxDiffChars)
}

// locate finds a target on the page.
func (s *Session) locate(ctx context.Context, d Driver, t Target) (*element, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	var e element
	if err := callAgent(ctx, d, &e, "locate", t.args()); err != nil {
		if t.Ref != "" && isAgentError(err) {
			if _, known := s.Refs()[t.Ref]; !known && s.LastSnapshot() != nil {
				return nil, fmt.Errorf("unknown ref %s: it is not in the last snapshot; call browser_snapshot for current refs", t.Ref)
			}
		}
		return nil, err
	}
	if e.Ref == "" {
		return nil, fmt.Errorf("could not locate %s", t)
	}
	return &e, nil
}

func (s *Session) relocate(ctx context.Context, d Driver, ref string) (*element, error) {
	var e element
	if err := callAgent(ctx, d, &e, "locate", map[string]string{"ref": ref}); err != nil {
		return nil, err
	}
	return &e, nil
}

// ensureInView scrolls until the element shows. Natural speed turns the
// wheel in notches like a person would; if that does not bring it into
// view (a nested scroller, or instant speed) the page scrolls it into view
// directly.
func (s *Session) ensureInView(ctx context.Context, d Driver, e *element) (*element, error) {
	if !e.Visible && e.box().Area() == 0 {
		return nil, fmt.Errorf("%s is not visible (it has no size or is hidden)", e.label())
	}
	if e.inView() {
		return e, nil
	}
	if s.natural() {
		for round := 0; round < 3 && !e.inView(); round++ {
			vw, vh := e.VW, e.VH
			c := e.box().Center()
			dy := c.Y - vh/2
			dx := 0.0
			if c.X < 0 || c.X > vw {
				dx = c.X - vw/2
			}
			at := s.wheelPoint(vw, vh)
			prev := e.box()
			if err := s.wheel(ctx, d, at, dx, dy); err != nil {
				return nil, err
			}
			if err := s.pause(ctx, s.jitterDur(80*time.Millisecond, 160*time.Millisecond)); err != nil {
				return nil, err
			}
			ne, err := s.relocate(ctx, d, e.Ref)
			if err != nil {
				return nil, err
			}
			e = ne
			if e.box() == prev {
				break // the wheel did not move this element's scroller
			}
		}
	}
	if !e.inView() {
		var ne element
		if err := callAgent(ctx, d, &ne, "scrollIntoView", e.Ref); err != nil {
			return nil, err
		}
		e = &ne
		if err := s.pause(ctx, s.jitterDur(60*time.Millisecond, 120*time.Millisecond)); err != nil {
			return nil, err
		}
	}
	if e.vis().Area() < 1 {
		b := e.box()
		return nil, fmt.Errorf("%s cannot be scrolled into view (box %.0f,%.0f %.0fx%.0f, viewport %.0fx%.0f)", e.label(), b.X, b.Y, b.W, b.H, e.VW, e.VH)
	}
	return e, nil
}

// wheelPoint is where wheel events go: under the cursor when it is on
// screen, else the middle of the viewport.
func (s *Session) wheelPoint(vw, vh float64) Point {
	if p, ok := s.Cursor(); ok && p.X >= 0 && p.Y >= 0 && p.X <= vw && p.Y <= vh {
		return p
	}
	return Point{vw / 2, vh / 2}
}

// wheel scrolls by (dx, dy) in natural notches or one step.
func (s *Session) wheel(ctx context.Context, d Driver, at Point, dx, dy float64) error {
	ys := WheelSteps(s.rng, dy, s.natural())
	xs := WheelSteps(s.rng, dx, s.natural())
	n := max(len(xs), len(ys))
	for i := 0; i < n; i++ {
		var sx, sy float64
		if i < len(xs) {
			sx = xs[i]
		}
		if i < len(ys) {
			sy = ys[i]
		}
		if err := d.Wheel(ctx, at.X, at.Y, sx, sy); err != nil {
			return fmt.Errorf("scroll: %w", err)
		}
		if i < n-1 {
			if err := s.pause(ctx, s.jitterDur(14*time.Millisecond, 34*time.Millisecond)); err != nil {
				return err
			}
		}
	}
	return nil
}

// aim picks a point on the element and verifies the element is what a
// click there would hit, so overlays and covered elements are reported
// rather than clicked through.
func (s *Session) aim(ctx context.Context, d Driver, e *element) (Point, error) {
	vis := e.vis()
	candidates := []Point{PickPoint(s.rng, vis, s.natural()), vis.Center()}
	var blocker string
	for _, p := range candidates {
		var h struct {
			OK      bool   `json:"ok"`
			Blocker string `json:"blocker"`
		}
		if err := callAgent(ctx, d, &h, "hit", e.Ref, p.X, p.Y); err != nil {
			return Point{}, err
		}
		if h.OK {
			return p, nil
		}
		blocker = h.Blocker
	}
	return Point{}, &OverlayError{Element: e.label(), At: candidates[1], Blocker: blocker}
}

// OverlayError reports that another element covers the target.
type OverlayError struct {
	Element string
	At      Point
	Blocker string
}

func (e *OverlayError) Error() string {
	return fmt.Sprintf("%s is covered at (%.0f, %.0f) by %s; a click would hit that instead. Close or deal with the overlay first (or target it), then retry",
		e.Element, e.At.X, e.At.Y, e.Blocker)
}

// moveTo moves the pointer to p: along a planned path at natural speed,
// in one jump at instant speed.
func (s *Session) moveTo(ctx context.Context, d Driver, p Point, width float64) error {
	from, ok := s.Cursor()
	if !ok {
		w, h := d.Viewport()
		from = Point{float64(w) / 2, float64(h) / 2}
		if w <= 0 || h <= 0 {
			from = Point{0, 0}
		}
	}
	if !s.natural() {
		if err := d.MouseMove(ctx, p.X, p.Y); err != nil {
			return fmt.Errorf("move mouse: %w", err)
		}
		s.setCursor(p)
		return nil
	}
	var last time.Duration
	for _, pp := range PlanPath(s.rng, from, p, width) {
		if err := s.pause(ctx, pp.At-last); err != nil {
			return err
		}
		last = pp.At
		if err := d.MouseMove(ctx, pp.X, pp.Y); err != nil {
			return fmt.Errorf("move mouse: %w", err)
		}
		s.setCursor(pp.Point)
	}
	return nil
}

// approach locates, scrolls to, aims at and moves onto an element.
func (s *Session) approach(ctx context.Context, d Driver, t Target, allowDisabled bool) (*element, Point, error) {
	e, err := s.locate(ctx, d, t)
	if err != nil {
		return nil, Point{}, err
	}
	if e.Disabled && !allowDisabled {
		return e, Point{}, fmt.Errorf("%s is disabled", e.label())
	}
	if e, err = s.ensureInView(ctx, d, e); err != nil {
		return nil, Point{}, err
	}
	p, err := s.aim(ctx, d, e)
	if err != nil {
		return e, Point{}, err
	}
	if err := s.moveTo(ctx, d, p, math.Min(e.vis().W, e.vis().H)); err != nil {
		return e, p, err
	}
	return e, p, nil
}

// ClickOptions configure a click.
type ClickOptions struct {
	// Button is left (default), right or middle.
	Button string
	// Count is 1 for a click, 2 for a double click, 3 for a triple click.
	Count int
	// Modifiers are held during the click (DOM key values).
	Modifiers []string
}

// clickAt presses and releases at p, count times, holding modifiers.
func (s *Session) clickAt(ctx context.Context, d Driver, p Point, o ClickOptions) (err error) {
	if o.Button == "" {
		o.Button = "left"
	}
	if o.Count <= 0 {
		o.Count = 1
	}
	if err := s.pause(ctx, s.jitterDur(40*time.Millisecond, 110*time.Millisecond)); err != nil {
		return err
	}
	release, err := s.holdKeys(ctx, d, o.Modifiers)
	defer func() {
		if rerr := release(); err == nil {
			err = rerr
		}
	}()
	if err != nil {
		return err
	}
	for i := 1; i <= o.Count; i++ {
		if err := d.MouseDown(ctx, p.X, p.Y, o.Button, i); err != nil {
			return fmt.Errorf("mouse down: %w", err)
		}
		if err := s.pause(ctx, s.jitterDur(45*time.Millisecond, 110*time.Millisecond)); err != nil {
			_ = d.MouseUp(context.WithoutCancel(ctx), p.X, p.Y, o.Button, i)
			return err
		}
		if err := d.MouseUp(ctx, p.X, p.Y, o.Button, i); err != nil {
			return fmt.Errorf("mouse up: %w", err)
		}
		if i < o.Count {
			if err := s.pause(ctx, s.jitterDur(60*time.Millisecond, 120*time.Millisecond)); err != nil {
				return err
			}
		}
	}
	return nil
}

// holdKeys presses keys in order and returns a function that releases
// them in reverse, even after ctx is done.
func (s *Session) holdKeys(ctx context.Context, d Driver, keys []string) (func() error, error) {
	var down []string
	release := func() error {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		var first error
		for i := len(down) - 1; i >= 0; i-- {
			if err := d.KeyUp(rctx, down[i]); err != nil && first == nil {
				first = fmt.Errorf("release %s: %w", down[i], err)
			}
		}
		down = nil
		return first
	}
	for _, k := range keys {
		if err := d.KeyDown(ctx, k); err != nil {
			return release, fmt.Errorf("press %s: %w", k, err)
		}
		down = append(down, k)
		if err := s.pause(ctx, s.jitterDur(20*time.Millisecond, 55*time.Millisecond)); err != nil {
			return release, err
		}
	}
	return release, nil
}

// pressChord presses one chord: modifiers down, key down and up, modifiers
// up in reverse.
func (s *Session) pressChord(ctx context.Context, d Driver, c Chord) (err error) {
	release, err := s.holdKeys(ctx, d, c.Modifiers)
	defer func() {
		if rerr := release(); err == nil {
			err = rerr
		}
	}()
	if err != nil {
		return err
	}
	if err := d.KeyDown(ctx, c.Key); err != nil {
		return fmt.Errorf("press %s: %w", c, err)
	}
	if err := s.pause(ctx, s.jitterDur(35*time.Millisecond, 90*time.Millisecond)); err != nil {
		_ = d.KeyUp(context.WithoutCancel(ctx), c.Key)
		return err
	}
	if err := d.KeyUp(ctx, c.Key); err != nil {
		return fmt.Errorf("release %s: %w", c, err)
	}
	return nil
}

// typeText types text into the focused element: rune by rune with a human
// cadence at natural speed, in runs at instant speed. Newlines press Enter
// and tabs press Tab.
func (s *Session) typeText(ctx context.Context, d Driver, text string) error {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if !s.natural() {
		var run strings.Builder
		flush := func() error {
			if run.Len() == 0 {
				return nil
			}
			err := d.InsertText(ctx, run.String())
			run.Reset()
			if err != nil {
				return fmt.Errorf("type: %w", err)
			}
			return nil
		}
		for _, r := range text {
			switch r {
			case '\n', '\t':
				if err := flush(); err != nil {
					return err
				}
				key := "Enter"
				if r == '\t' {
					key = "Tab"
				}
				if err := s.pressChord(ctx, d, Chord{Key: key}); err != nil {
					return err
				}
			case '\r':
			default:
				run.WriteRune(r)
			}
		}
		return flush()
	}
	delays := TypingDelays(s.rng, text, s.opts.WPM)
	for i, r := range []rune(text) {
		if err := s.pause(ctx, delays[i]); err != nil {
			return err
		}
		switch r {
		case '\n':
			if err := s.pressChord(ctx, d, Chord{Key: "Enter"}); err != nil {
				return err
			}
		case '\t':
			if err := s.pressChord(ctx, d, Chord{Key: "Tab"}); err != nil {
				return err
			}
		case '\r':
		default:
			if err := d.InsertText(ctx, string(r)); err != nil {
				return fmt.Errorf("type: %w", err)
			}
		}
	}
	return nil
}

func (s *Session) fieldState(ctx context.Context, d Driver, ref string) (*fieldState, error) {
	var st fieldState
	if err := callAgent(ctx, d, &st, "state", ref); err != nil {
		return nil, err
	}
	return &st, nil
}

// focusField clicks an editable element so it has focus.
func (s *Session) focusField(ctx context.Context, d Driver, t Target) (*element, Point, error) {
	e, p, err := s.approach(ctx, d, t, false)
	if err != nil {
		return e, p, err
	}
	if !e.Editable {
		if e.Readonly {
			return e, p, fmt.Errorf("%s is read-only", e.label())
		}
		return e, p, fmt.Errorf("%s is not a text field; use browser_click, browser_select or browser_check", e.label())
	}
	if err := s.clickAt(ctx, d, p, ClickOptions{}); err != nil {
		return e, p, err
	}
	st, err := s.fieldState(ctx, d, e.Ref)
	if err != nil {
		return e, p, err
	}
	if !st.Focused {
		// The click landed but focus went elsewhere (a wrapper took it);
		// focus the control directly.
		var f struct {
			Focused bool `json:"focused"`
		}
		if err := callAgent(ctx, d, &f, "focus", e.Ref); err != nil {
			return e, p, err
		}
		if !f.Focused {
			return e, p, fmt.Errorf("%s did not take keyboard focus", e.label())
		}
	}
	return e, p, nil
}

// clearField empties the focused field: select all and delete, as a person
// would, falling back to clearing it through the page.
func (s *Session) clearField(ctx context.Context, d Driver, e *element) error {
	st, err := s.fieldState(ctx, d, e.Ref)
	if err != nil {
		return err
	}
	if st.Length == 0 {
		return nil
	}
	mod := "Control"
	if e.Mac {
		mod = "Meta"
	}
	if err := s.pressChord(ctx, d, Chord{Modifiers: []string{mod}, Key: "a"}); err != nil {
		return err
	}
	if err := s.pressChord(ctx, d, Chord{Key: "Backspace"}); err != nil {
		return err
	}
	if st, err = s.fieldState(ctx, d, e.Ref); err != nil {
		return err
	}
	if st.Length == 0 {
		return nil
	}
	var sel struct {
		OK bool `json:"ok"`
	}
	if err := callAgent(ctx, d, &sel, "selectAll", e.Ref); err == nil && sel.OK {
		if err := s.pressChord(ctx, d, Chord{Key: "Backspace"}); err != nil {
			return err
		}
		if st, err = s.fieldState(ctx, d, e.Ref); err != nil {
			return err
		}
		if st.Length == 0 {
			return nil
		}
	}
	var cl struct {
		Value string `json:"value"`
	}
	if err := callAgent(ctx, d, &cl, "clear", e.Ref); err != nil {
		return fmt.Errorf("clear %s: %w", e.label(), err)
	}
	if cl.Value != "" {
		return fmt.Errorf("could not clear %s", e.label())
	}
	return nil
}

// describeValue renders a field's value for a result line.
func describeValue(st *fieldState) string {
	if st.Masked {
		return fmt.Sprintf("(%d characters, masked)", st.Length)
	}
	v := st.Value
	if utf8.RuneCountInString(v) > 120 {
		v = string([]rune(v)[:119]) + "…"
	}
	return quote(v)
}

// fillText focuses, optionally clears, and types into a field.
func (s *Session) fillText(ctx context.Context, d Driver, t Target, text string, clear bool) (*element, Point, string, error) {
	e, p, err := s.focusField(ctx, d, t)
	if err != nil {
		return e, p, "", err
	}
	if clear {
		if err := s.clearField(ctx, d, e); err != nil {
			return e, p, "", err
		}
	}
	if err := s.typeText(ctx, d, text); err != nil {
		return e, p, "", err
	}
	st, err := s.fieldState(ctx, d, e.Ref)
	if err != nil {
		return e, p, "", err
	}
	note := ""
	if !st.Masked && !strings.Contains(st.Value, strings.TrimRight(text, "\n")) {
		note = " (the field reformatted or limited the input)"
	}
	return e, p, fmt.Sprintf("%s value=%s%s", e.label(), describeValue(st), note), nil
}

// selectIn selects options of a native <select>, or opens a custom
// listbox and clicks the matching options.
func (s *Session) selectIn(ctx context.Context, d Driver, t Target, values []string, by string) (*element, Point, string, error) {
	e, err := s.locate(ctx, d, t)
	if err != nil {
		return nil, Point{}, "", err
	}
	if e.Disabled {
		return e, Point{}, "", fmt.Errorf("%s is disabled", e.label())
	}
	if e, err = s.ensureInView(ctx, d, e); err != nil {
		return nil, Point{}, "", err
	}
	if e.IsSelect {
		// A native select's popup is drawn by the browser outside the
		// page, so point at it and choose through the page.
		p, err := s.aim(ctx, d, e)
		if err != nil {
			return e, p, "", err
		}
		if err := s.moveTo(ctx, d, p, math.Min(e.vis().W, e.vis().H)); err != nil {
			return e, p, "", err
		}
		var res struct {
			Selected []string `json:"selected"`
		}
		if err := callAgent(ctx, d, &res, "select", e.Ref, values, by); err != nil {
			return e, p, "", fmt.Errorf("%s: %w", e.label(), err)
		}
		return e, p, fmt.Sprintf("%s now has %s selected", e.label(), quoteList(res.Selected)), nil
	}
	if by == "value" {
		return e, Point{}, "", fmt.Errorf("%s is not a native <select>; custom dropdowns are matched by option text, so use by=label or auto", e.label())
	}
	// Custom dropdown: open it, then click each option by its text.
	p, err := s.aim(ctx, d, e)
	if err != nil {
		return e, p, "", err
	}
	if err := s.moveTo(ctx, d, p, math.Min(e.vis().W, e.vis().H)); err != nil {
		return e, p, "", err
	}
	if err := s.clickAt(ctx, d, p, ClickOptions{}); err != nil {
		return e, p, "", err
	}
	var picked []string
	for i, v := range values {
		if err := s.pause(ctx, s.jitterDur(150*time.Millisecond, 300*time.Millisecond)); err != nil {
			return e, p, "", err
		}
		var opt element
		err := callAgent(ctx, d, &opt, "findOption", v)
		if err != nil && i > 0 && isAgentError(err) {
			// The list closed after the previous choice; open it again.
			if p, err = s.aim(ctx, d, e); err != nil {
				return e, p, "", err
			}
			if err := s.moveTo(ctx, d, p, math.Min(e.vis().W, e.vis().H)); err != nil {
				return e, p, "", err
			}
			if err := s.clickAt(ctx, d, p, ClickOptions{}); err != nil {
				return e, p, "", err
			}
			if err := s.pause(ctx, s.jitterDur(150*time.Millisecond, 300*time.Millisecond)); err != nil {
				return e, p, "", err
			}
			err = callAgent(ctx, d, &opt, "findOption", v)
		}
		if err != nil {
			return e, p, "", fmt.Errorf("%s: %w", e.label(), err)
		}
		oe, op, err := s.approach(ctx, d, Target{Ref: opt.Ref}, false)
		if err != nil {
			return e, op, "", err
		}
		if err := s.clickAt(ctx, d, op, ClickOptions{}); err != nil {
			return e, op, "", err
		}
		p = op
		picked = append(picked, oe.Name)
	}
	return e, p, fmt.Sprintf("chose %s in %s", quoteList(picked), e.label()), nil
}

// setChecked clicks a checkbox, radio or switch if it is not already in
// the wanted state, then verifies the state changed.
func (s *Session) setChecked(ctx context.Context, d Driver, t Target, want bool) (*element, Point, string, error) {
	e, err := s.locate(ctx, d, t)
	if err != nil {
		return nil, Point{}, "", err
	}
	if !e.Checkable {
		return e, Point{}, "", fmt.Errorf("%s is not a checkbox, radio button or switch", e.label())
	}
	wantTri := Tri("false")
	if want {
		wantTri = "true"
	}
	if e.Checked == wantTri {
		return e, Point{}, fmt.Sprintf("%s was already %s", e.label(), checkWord(want)), nil
	}
	if !want && (e.Role == "radio" || e.Type == "radio") {
		return e, Point{}, "", fmt.Errorf("%s is a radio button; select another option in its group instead of unchecking it", e.label())
	}
	e, p, err := s.approach(ctx, d, t, false)
	if err != nil {
		return e, p, "", err
	}
	if err := s.clickAt(ctx, d, p, ClickOptions{}); err != nil {
		return e, p, "", err
	}
	st, err := s.fieldState(ctx, d, e.Ref)
	if err != nil {
		return e, p, "", err
	}
	if st.Checked != wantTri {
		return e, p, "", fmt.Errorf("clicked %s but it is still %s", e.label(), triWord(st.Checked))
	}
	return e, p, fmt.Sprintf("%s is now %s", e.label(), checkWord(want)), nil
}

func checkWord(b bool) string {
	if b {
		return "checked"
	}
	return "unchecked"
}

func triWord(t Tri) string {
	switch t {
	case "true":
		return "checked"
	case "mixed":
		return "partly checked"
	}
	return "unchecked"
}

func quoteList(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = quote(s)
	}
	return strings.Join(q, ", ")
}

// ---- public actions ------------------------------------------------------

// Navigate opens url, waits for the page to settle and returns its outline.
// The caller validates url (see CheckURL).
func (s *Session) Navigate(ctx context.Context, url string) (string, error) {
	return s.navLike(ctx, "navigate", url, func(ctx context.Context, d Driver) error { return d.Navigate(ctx, url) })
}

// Back goes back in history.
func (s *Session) Back(ctx context.Context) (string, error) {
	return s.navLike(ctx, "back", "", func(ctx context.Context, d Driver) error { return d.Back(ctx) })
}

// Forward goes forward in history.
func (s *Session) Forward(ctx context.Context) (string, error) {
	return s.navLike(ctx, "forward", "", func(ctx context.Context, d Driver) error { return d.Forward(ctx) })
}

// Reload reloads the page.
func (s *Session) Reload(ctx context.Context) (string, error) {
	return s.navLike(ctx, "reload", "", func(ctx context.Context, d Driver) error { return d.Reload(ctx) })
}

func (s *Session) navLike(ctx context.Context, kind, target string, f func(context.Context, Driver) error) (string, error) {
	return s.act(ctx, kind, target, false, func(ctx context.Context, d Driver) (actOut, error) {
		nctx, cancel := context.WithTimeout(ctx, s.opts.NavTimeout)
		defer cancel()
		if err := f(nctx, d); err != nil {
			return actOut{}, fmt.Errorf("%s: %w", kind, err)
		}
		verb := map[string]string{"navigate": "Navigated to", "back": "Went back to", "forward": "Went forward to", "reload": "Reloaded"}[kind]
		return actOut{summary: verb + " " + d.URL(), target: d.URL(), full: true}, nil
	})
}

// Click clicks an element like a person: scroll it into view, move the
// pointer onto it, check nothing covers it, press and release.
func (s *Session) Click(ctx context.Context, t Target, o ClickOptions) (string, error) {
	return s.act(ctx, "click", t.String(), true, func(ctx context.Context, d Driver) (actOut, error) {
		e, p, err := s.approach(ctx, d, t, false)
		out := actOut{point: pointIf(p, err == nil)}
		if e != nil {
			out.target = e.label()
		}
		if err != nil {
			return out, err
		}
		if err := s.clickAt(ctx, d, p, o); err != nil {
			return out, err
		}
		verb := "Clicked"
		switch o.Count {
		case 2:
			verb = "Double-clicked"
		case 3:
			verb = "Triple-clicked"
		}
		if o.Button != "" && o.Button != "left" {
			verb = strings.Replace(verb, "licked", "licked ("+o.Button+" button)", 1)
		}
		if len(o.Modifiers) > 0 {
			verb += " with " + strings.Join(o.Modifiers, "+")
		}
		out.summary = fmt.Sprintf("%s %s at (%.0f, %.0f)%s", verb, e.label(), p.X, p.Y, ambiguity(e))
		return out, nil
	})
}

func pointIf(p Point, ok bool) *Point {
	if !ok {
		return nil
	}
	return &p
}

func ambiguity(e *element) string {
	if e.Count > 1 {
		return fmt.Sprintf(" (first visible of %d matches)", e.Count)
	}
	return ""
}

// Hover moves the pointer onto an element and leaves it there.
func (s *Session) Hover(ctx context.Context, t Target) (string, error) {
	return s.act(ctx, "hover", t.String(), true, func(ctx context.Context, d Driver) (actOut, error) {
		e, p, err := s.approach(ctx, d, t, true)
		out := actOut{point: pointIf(p, err == nil)}
		if e != nil {
			out.target = e.label()
		}
		if err != nil {
			return out, err
		}
		if err := s.pause(ctx, s.jitterDur(200*time.Millisecond, 400*time.Millisecond)); err != nil {
			return out, err
		}
		out.summary = fmt.Sprintf("Hovering over %s at (%.0f, %.0f)", e.label(), p.X, p.Y)
		return out, nil
	})
}

// TypeOptions configure Type.
type TypeOptions struct {
	// Clear empties the field first.
	Clear bool
	// Submit presses Enter afterwards.
	Submit bool
}

// Type types text into an element (clicking it first) or, with a zero
// target, into whatever has focus.
func (s *Session) Type(ctx context.Context, t Target, text string, o TypeOptions) (string, error) {
	target := t.String()
	if t.IsZero() {
		target = "focused element"
	}
	return s.act(ctx, "type", target, true, func(ctx context.Context, d Driver) (actOut, error) {
		out := actOut{detail: fmt.Sprintf("%d characters", utf8.RuneCountInString(text))}
		if t.IsZero() {
			if err := s.typeText(ctx, d, text); err != nil {
				return out, err
			}
			out.summary = fmt.Sprintf("Typed %d characters into the focused element", utf8.RuneCountInString(text))
		} else {
			e, p, line, err := s.fillText(ctx, d, t, text, o.Clear)
			out.point = pointIf(p, e != nil && p != (Point{}))
			if e != nil {
				out.target = e.label()
			}
			if err != nil {
				return out, err
			}
			out.summary = "Typed into " + line
		}
		if o.Submit {
			if err := s.pressChord(ctx, d, Chord{Key: "Enter"}); err != nil {
				return out, err
			}
			out.summary += "; pressed Enter"
		}
		return out, nil
	})
}

// Press presses a key or chord repeat times.
func (s *Session) Press(ctx context.Context, keys string, repeat int) (string, error) {
	c, err := ParseChord(keys)
	if err != nil {
		return "", err
	}
	repeat = max(repeat, 1)
	return s.act(ctx, "press", c.String(), true, func(ctx context.Context, d Driver) (actOut, error) {
		for i := 0; i < repeat; i++ {
			if i > 0 {
				if err := s.pause(ctx, s.jitterDur(60*time.Millisecond, 140*time.Millisecond)); err != nil {
					return actOut{}, err
				}
			}
			if err := s.pressChord(ctx, d, c); err != nil {
				return actOut{}, err
			}
		}
		times := ""
		if repeat > 1 {
			times = fmt.Sprintf(" %d times", repeat)
		}
		return actOut{summary: "Pressed " + c.String() + times}, nil
	})
}

// FillField is one field of a Fill: a target and the value to enter. For
// a checkbox or switch the value is true/false; for a select it is the
// option label or value.
type FillField struct {
	Target Target
	Value  string
	// NoClear keeps existing text and appends to it.
	NoClear bool
}

// Fill fills several fields in order, like a person working down a form.
func (s *Session) Fill(ctx context.Context, fields []FillField, submit bool) (string, error) {
	return s.act(ctx, "fill", fmt.Sprintf("%d fields", len(fields)), true, func(ctx context.Context, d Driver) (actOut, error) {
		var lines []string
		out := actOut{}
		for i, f := range fields {
			e, err := s.locate(ctx, d, f.Target)
			if err != nil {
				out.summary = filledSoFar(lines)
				return out, fmt.Errorf("field %d (%s): %w", i+1, f.Target, err)
			}
			var line string
			var p Point
			switch {
			case e.IsSelect:
				_, p, line, err = s.selectIn(ctx, d, Target{Ref: e.Ref}, []string{f.Value}, "auto")
			case e.Checkable && !e.Editable:
				var want bool
				want, err = parseBoolValue(f.Value)
				if err == nil {
					_, p, line, err = s.setChecked(ctx, d, Target{Ref: e.Ref}, want)
				}
			default:
				_, p, line, err = s.fillText(ctx, d, Target{Ref: e.Ref}, f.Value, !f.NoClear)
			}
			if p != (Point{}) {
				out.point = &p
			}
			if err != nil {
				out.summary = filledSoFar(lines)
				return out, fmt.Errorf("field %d (%s): %w", i+1, f.Target, err)
			}
			lines = append(lines, fmt.Sprintf("%d. %s", i+1, line))
			if i < len(fields)-1 {
				if err := s.pause(ctx, s.jitterDur(150*time.Millisecond, 350*time.Millisecond)); err != nil {
					return out, err
				}
			}
		}
		summary := fmt.Sprintf("Filled %d field(s):\n%s", len(fields), strings.Join(lines, "\n"))
		if submit {
			if err := s.pressChord(ctx, d, Chord{Key: "Enter"}); err != nil {
				return out, err
			}
			summary += "\nPressed Enter to submit."
		}
		out.summary = summary
		return out, nil
	})
}

func filledSoFar(lines []string) string {
	if len(lines) == 0 {
		return "no field was filled"
	}
	return "filled before the error:\n" + strings.Join(lines, "\n")
}

func parseBoolValue(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "yes", "on", "checked", "1", "x":
		return true, nil
	case "false", "no", "off", "unchecked", "0", "":
		return false, nil
	}
	return false, fmt.Errorf("value %q for a checkbox must be true or false", v)
}

// Select chooses options in a <select> (by label, value or auto) or in a
// custom dropdown (by visible option text).
func (s *Session) Select(ctx context.Context, t Target, values []string, by string) (string, error) {
	return s.act(ctx, "select", t.String(), true, func(ctx context.Context, d Driver) (actOut, error) {
		e, p, line, err := s.selectIn(ctx, d, t, values, by)
		out := actOut{point: pointIf(p, p != (Point{})), detail: strings.Join(values, ", ")}
		if e != nil {
			out.target = e.label()
		}
		if err != nil {
			return out, err
		}
		out.summary = "Selected: " + line
		return out, nil
	})
}

// Check sets a checkbox, radio button or switch.
func (s *Session) Check(ctx context.Context, t Target, checked bool) (string, error) {
	return s.act(ctx, "check", t.String(), true, func(ctx context.Context, d Driver) (actOut, error) {
		e, p, line, err := s.setChecked(ctx, d, t, checked)
		out := actOut{point: pointIf(p, p != (Point{})), detail: checkWord(checked)}
		if e != nil {
			out.target = e.label()
		}
		if err != nil {
			return out, err
		}
		out.summary = line
		return out, nil
	})
}

// ScrollOptions configure Scroll. With only Target, the element is
// scrolled into view; with Direction, the page (or, with Target, the
// element's scroll area) moves by Amount pixels; with To, the page goes to
// its top or bottom.
type ScrollOptions struct {
	Target    Target
	Direction string // up, down, left, right
	Amount    float64
	To        string // top, bottom
}

// Scroll scrolls with the mouse wheel.
func (s *Session) Scroll(ctx context.Context, o ScrollOptions) (string, error) {
	target := o.Direction + o.To
	if !o.Target.IsZero() {
		target = o.Target.String()
	}
	return s.act(ctx, "scroll", target, true, func(ctx context.Context, d Driver) (actOut, error) {
		var before scrollState
		if err := callAgent(ctx, d, &before, "scrollInfo"); err != nil {
			return actOut{}, err
		}
		out := actOut{}
		if !o.Target.IsZero() && o.Direction == "" {
			e, err := s.locate(ctx, d, o.Target)
			if err != nil {
				return out, err
			}
			out.target = e.label()
			if e, err = s.ensureInView(ctx, d, e); err != nil {
				return out, err
			}
			out.summary = fmt.Sprintf("Scrolled %s into view", e.label())
			return out, nil
		}
		at := s.wheelPoint(float64(before.VW), float64(before.VH))
		if !o.Target.IsZero() {
			e, err := s.locate(ctx, d, o.Target)
			if err != nil {
				return out, err
			}
			out.target = e.label()
			if e, err = s.ensureInView(ctx, d, e); err != nil {
				return out, err
			}
			at = PickPoint(s.rng, e.vis(), s.natural())
			if err := s.moveTo(ctx, d, at, math.Min(e.vis().W, e.vis().H)); err != nil {
				return out, err
			}
		}
		var dx, dy float64
		switch o.To {
		case "top":
			dy = -float64(before.SY)
		case "bottom":
			dy = float64(max(before.SH-before.VH-before.SY, 0))
		}
		amount := o.Amount
		switch o.Direction {
		case "down", "up":
			if amount <= 0 {
				amount = math.Round(0.8 * float64(before.VH))
			}
			dy = amount
			if o.Direction == "up" {
				dy = -amount
			}
		case "left", "right":
			if amount <= 0 {
				amount = math.Round(0.8 * float64(before.VW))
			}
			dx = amount
			if o.Direction == "left" {
				dx = -amount
			}
		}
		out.point = &at
		if dx == 0 && dy == 0 {
			out.summary = fmt.Sprintf("Already at the %s (y=%d)", o.To, before.SY)
			return out, nil
		}
		if err := s.wheel(ctx, d, at, dx, dy); err != nil {
			return out, err
		}
		if err := s.pause(ctx, s.jitterDur(120*time.Millisecond, 220*time.Millisecond)); err != nil {
			return out, err
		}
		var after scrollState
		if err := callAgent(ctx, d, &after, "scrollInfo"); err != nil {
			return out, err
		}
		out.detail = fmt.Sprintf("dx=%.0f dy=%.0f", dx, dy)
		switch {
		case after.SY != before.SY || after.SX != before.SX:
			out.summary = fmt.Sprintf("Scrolled the page to x=%d y=%d (max y %d, viewport %dx%d)",
				after.SX, after.SY, max(after.SH-after.VH, 0), after.VW, after.VH)
		case !o.Target.IsZero():
			out.summary = "Scrolled inside " + out.target + " (the page itself did not move)"
		default:
			out.summary = fmt.Sprintf("The page did not move (at y=%d of %d): it is already at that end, or the area under the pointer does not scroll", after.SY, max(after.SH-after.VH, 0))
		}
		return out, nil
	})
}

// WaitOptions say what to wait for; every condition given must hold.
type WaitOptions struct {
	Text        string
	TextGone    string
	Ref         string
	Selector    string
	URLContains string
	NetworkIdle bool
	// Duration waits a fixed time first.
	Duration time.Duration
	// Timeout bounds the wait; zero means 10 seconds.
	Timeout time.Duration
}

func (o WaitOptions) describe() string {
	var parts []string
	if o.Text != "" {
		parts = append(parts, "text "+quote(o.Text))
	}
	if o.TextGone != "" {
		parts = append(parts, "text "+quote(o.TextGone)+" to disappear")
	}
	if o.Ref != "" {
		parts = append(parts, o.Ref+" to be visible")
	}
	if o.Selector != "" {
		parts = append(parts, "selector "+quote(o.Selector)+" to be visible")
	}
	if o.URLContains != "" {
		parts = append(parts, "URL containing "+quote(o.URLContains))
	}
	if o.NetworkIdle {
		parts = append(parts, "network idle")
	}
	if len(parts) == 0 && o.Duration > 0 {
		return o.Duration.String()
	}
	return strings.Join(parts, ", ")
}

// Wait waits until every given condition holds or the timeout passes.
func (s *Session) Wait(ctx context.Context, o WaitOptions) (string, error) {
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	return s.act(ctx, "wait", o.describe(), true, func(ctx context.Context, d Driver) (actOut, error) {
		start := s.opts.Now()
		if o.Duration > 0 {
			if err := s.opts.Sleep(ctx, o.Duration); err != nil {
				return actOut{}, err
			}
		}
		deadline := start.Add(o.Timeout + o.Duration)
		if o.NetworkIdle {
			wctx, cancel := context.WithTimeout(ctx, o.Timeout)
			err := d.WaitIdle(wctx, s.opts.IdleQuiet)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return actOut{}, ctx.Err()
				}
				return actOut{}, fmt.Errorf("timed out after %s waiting for network idle: %w", o.Timeout, err)
			}
		}
		for {
			pending, err := s.pendingConditions(ctx, d, o)
			if err != nil {
				return actOut{}, err
			}
			if len(pending) == 0 {
				break
			}
			if !s.opts.Now().Before(deadline) {
				return actOut{}, fmt.Errorf("timed out after %s waiting for %s (page: %s)", o.Timeout, strings.Join(pending, ", "), d.URL())
			}
			if err := s.opts.Sleep(ctx, 200*time.Millisecond); err != nil {
				return actOut{}, err
			}
		}
		took := s.opts.Now().Sub(start).Round(10 * time.Millisecond)
		return actOut{summary: fmt.Sprintf("Waited %s for %s", took, o.describe())}, nil
	})
}

// pendingConditions lists the wait conditions that do not hold yet. A
// page that is mid-navigation counts as not ready rather than failing.
func (s *Session) pendingConditions(ctx context.Context, d Driver, o WaitOptions) ([]string, error) {
	var pending []string
	if o.URLContains != "" && !strings.Contains(d.URL(), o.URLContains) {
		pending = append(pending, "URL containing "+quote(o.URLContains))
	}
	probes := []struct{ kind, value, label string }{
		{"text", o.Text, "text " + quote(o.Text)},
		{"text_gone", o.TextGone, "text " + quote(o.TextGone) + " to disappear"},
		{"ref", o.Ref, o.Ref + " to be visible"},
		{"selector", o.Selector, "selector " + quote(o.Selector) + " to be visible"},
	}
	for _, p := range probes {
		if p.value == "" {
			continue
		}
		var r struct {
			OK bool `json:"ok"`
		}
		err := callAgent(ctx, d, &r, "probe", p.kind, p.value)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			var ae *agentError
			if errors.As(err, &ae) && strings.Contains(ae.Msg, "invalid CSS selector") {
				return nil, err
			}
			r.OK = false
		}
		if !r.OK {
			pending = append(pending, p.label)
		}
	}
	return pending, nil
}

// Extract returns the readable text of the page or of one element, capped
// at maxChars.
func (s *Session) Extract(ctx context.Context, t Target, maxChars int) (string, error) {
	target := "page"
	if !t.IsZero() {
		target = t.String()
	}
	return s.act(ctx, "extract", target, false, func(ctx context.Context, d Driver) (actOut, error) {
		var ref any
		label := "page"
		if !t.IsZero() {
			e, err := s.locate(ctx, d, t)
			if err != nil {
				return actOut{}, err
			}
			ref = e.Ref
			label = e.label()
		}
		var r struct {
			Text      string `json:"text"`
			Total     int    `json:"total"`
			Truncated bool   `json:"truncated"`
			URL       string `json:"url"`
			Title     string `json:"title"`
		}
		if err := callAgent(ctx, d, &r, "extract", ref, maxChars); err != nil {
			return actOut{}, err
		}
		var b strings.Builder
		if t.IsZero() {
			fmt.Fprintf(&b, "Text of %s (%s):\n\n", quote(r.Title), r.URL)
		} else {
			fmt.Fprintf(&b, "Text of %s:\n\n", label)
		}
		if strings.TrimSpace(r.Text) == "" {
			b.WriteString("(no text)")
		} else {
			b.WriteString(r.Text)
		}
		if r.Truncated {
			fmt.Fprintf(&b, "\n\n… [showing %d of %d characters; target a ref to read a part]", utf8.RuneCountInString(r.Text), r.Total)
		}
		return actOut{summary: b.String(), target: label, detail: fmt.Sprintf("%d characters", r.Total)}, nil
	})
}

// Screenshot captures the viewport in format png or jpeg.
func (s *Session) Screenshot(ctx context.Context, format string, quality int) ([]byte, error) {
	var data []byte
	_, err := s.act(ctx, "screenshot", format, false, func(ctx context.Context, d Driver) (actOut, error) {
		var err error
		data, err = d.Screenshot(ctx, format, quality)
		if err != nil {
			return actOut{}, fmt.Errorf("screenshot: %w", err)
		}
		if len(data) == 0 {
			return actOut{}, errors.New("screenshot: the browser returned no image")
		}
		return actOut{detail: fmt.Sprintf("%d bytes", len(data))}, nil
	})
	return data, err
}

// Eval evaluates a JavaScript expression in the page and returns its
// JSON value.
func (s *Session) Eval(ctx context.Context, expr string) (json.RawMessage, error) {
	var res json.RawMessage
	_, err := s.act(ctx, "eval", firstLineOf(expr, 80), false, func(ctx context.Context, d Driver) (actOut, error) {
		var err error
		res, err = d.Evaluate(ctx, expr)
		if err != nil {
			return actOut{}, fmt.Errorf("evaluate: %w", err)
		}
		// The page may have changed; refs stay valid, but the cached outline
		// should not be trusted as a diff baseline.
		s.invalidateBaseline()
		return actOut{detail: fmt.Sprintf("%d bytes", len(res))}, nil
	})
	return res, err
}

func firstLineOf(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if utf8.RuneCountInString(s) > n {
		s = string([]rune(s)[:n-1]) + "…"
	}
	return s
}
