package browserview

import (
	"context"
	"fmt"
	"image"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// PaneOptions configures a Pane. The zero value is usable.
type PaneOptions struct {
	// Title is shown in the top border. Default "browser".
	Title string
	// Capabilities overrides terminal detection. When nil, DetectEnv is
	// combined with the cell size reported for stdout.
	Capabilities *Capabilities
	// Protocol forces a protocol (overrides Capabilities.Protocol).
	Protocol Protocol
	// MaxFPS caps how often frames are encoded. Default 30.
	MaxFPS int
	// Dither, SixelColors and JPEGQuality tune the image encoders.
	Dither      Dither
	SixelColors int
	JPEGQuality int
	// Context bounds the pane's subscription; Close also stops it.
	Context context.Context
	// KittyIDBase selects the Kitty image id range (default derived from
	// the pane id so several panes never collide).
	KittyIDBase uint32

	// Colours; nil values use defaults.
	BorderColor      lipgloss.TerminalColor
	FocusBorderColor lipgloss.TerminalColor
	AccentColor      lipgloss.TerminalColor
	MutedColor       lipgloss.TerminalColor
	ErrorColor       lipgloss.TerminalColor
}

// Pane geometry: one border cell on each side, a title bar and a status
// line inside the border. The image body starts at (BodyRowOffset,
// BodyColOffset) relative to the pane's top-left cell.
const (
	BodyRowOffset = 2
	BodyColOffset = 1
	chromeRows    = 4
	chromeCols    = 2
)

var paneIDs atomic.Uint64

// PaneMsg carries rendered frames from the pane's worker goroutine to its
// Update method. Hosts must route every PaneMsg to Pane.Update (messages
// for other panes are ignored, see For).
type PaneMsg struct {
	pane uint64
	ev   paneEvent
}

// For reports whether the message belongs to p.
func (m PaneMsg) For(p *Pane) bool { return p != nil && m.pane == p.id }

type paneEvent struct {
	r       *Rendered
	err     error
	ended   bool
	dropped uint64
}

// PaneStats is a snapshot of rendering statistics.
type PaneStats struct {
	Protocol   Protocol
	FPS        float64
	Seq        uint64
	Dropped    uint64
	EncodeTime time.Duration
	Latency    time.Duration // capture -> render
}

// Pane is a bubbletea component showing the live browser. All heavy work
// (decode, resize, overlay, encode) happens on a private worker goroutine;
// Update only swaps in finished results.
//
// For ProtocolHalfBlock the picture is part of View(). For image protocols
// View() reserves blank cells and the picture travels out of band, see
// WrapOutput and Graphics.
type Pane struct {
	id   uint64
	src  Source
	opts PaneOptions
	caps Capabilities

	ctx    context.Context
	cancel context.CancelFunc
	start  sync.Once

	pmu     sync.Mutex
	params  RenderParams
	kick    *Latest[struct{}]
	events  *Latest[paneEvent]
	gfx     *graphicsState
	styles  paneStyles
	closed  atomic.Bool
	started atomic.Bool

	// UI-goroutine state.
	width, height int
	focused       bool
	visible       bool
	overlay       Overlay
	cur           *Rendered
	meter         fpsMeter
	err           error
	ended         bool
	dropped       uint64
	renders       uint64
}

type paneStyles struct {
	border, focusBorder, title, accent, muted, errStyle, chrome lipgloss.Style
}

// New creates a pane for src. Call Init (via the host's Init) to start it.
func New(src Source, opts PaneOptions) *Pane {
	if opts.MaxFPS <= 0 {
		opts.MaxFPS = 30
	}
	if opts.Title == "" {
		opts.Title = "browser"
	}
	parent := opts.Context
	if parent == nil {
		parent = context.Background()
	}
	var caps Capabilities
	if opts.Capabilities != nil {
		caps = *opts.Capabilities
	} else {
		caps = Detect(os.Stdout.Fd())
	}
	if opts.Protocol != ProtocolAuto {
		caps.Protocol = opts.Protocol
	}
	if caps.Protocol == ProtocolAuto {
		caps.Protocol = ProtocolHalfBlock
	}
	caps.Cell = caps.Cell.orDefault()
	id := paneIDs.Add(1)
	if opts.KittyIDBase == 0 {
		opts.KittyIDBase = 0x67740000 + uint32(id%64)*kittyIDSpan/16
	}
	ctx, cancel := context.WithCancel(parent)
	p := &Pane{
		id: id, src: src, opts: opts, caps: caps, ctx: ctx, cancel: cancel,
		kick: NewLatest[struct{}](), events: NewLatest[paneEvent](),
		gfx:     &graphicsState{proto: caps.Protocol, tmux: caps.Tmux},
		visible: true,
	}
	p.params = RenderParams{
		Protocol: caps.Protocol, TrueColor: caps.TrueColor, Tmux: caps.Tmux, Cell: caps.Cell,
		Dither: opts.Dither, SixelColors: opts.SixelColors, JPEGQuality: opts.JPEGQuality,
	}
	p.styles = makeStyles(opts)
	return p
}

func colorOr(c lipgloss.TerminalColor, def string) lipgloss.TerminalColor {
	if c != nil {
		return c
	}
	return lipgloss.Color(def)
}

func makeStyles(o PaneOptions) paneStyles {
	return paneStyles{
		border:      lipgloss.NewStyle().Foreground(colorOr(o.BorderColor, "240")),
		focusBorder: lipgloss.NewStyle().Foreground(colorOr(o.FocusBorderColor, "39")),
		title:       lipgloss.NewStyle().Bold(true).Foreground(colorOr(o.FocusBorderColor, "39")),
		accent:      lipgloss.NewStyle().Foreground(colorOr(o.AccentColor, "214")).Bold(true),
		muted:       lipgloss.NewStyle().Foreground(colorOr(o.MutedColor, "245")),
		errStyle:    lipgloss.NewStyle().Foreground(colorOr(o.ErrorColor, "203")),
		chrome:      lipgloss.NewStyle().Foreground(colorOr(o.MutedColor, "250")),
	}
}

// Protocol returns the protocol in use.
func (p *Pane) Protocol() Protocol { return p.caps.Protocol }

// Init starts the subscription worker and returns the command that
// delivers rendered frames. Calling it more than once is harmless.
func (p *Pane) Init() tea.Cmd {
	if p.src == nil || p.closed.Load() {
		return nil
	}
	p.start.Do(func() {
		p.started.Store(true)
		go p.run()
	})
	return p.listen()
}

func (p *Pane) listen() tea.Cmd {
	ctx, ev, id := p.ctx, p.events, p.id
	return func() tea.Msg {
		e, ok := ev.Wait(ctx)
		if !ok {
			return nil
		}
		return PaneMsg{pane: id, ev: e}
	}
}

// Update handles PaneMsg for this pane and terminal resizes. Unknown
// messages are ignored.
func (p *Pane) Update(msg tea.Msg) (*Pane, tea.Cmd) {
	switch m := msg.(type) {
	case PaneMsg:
		if m.pane != p.id {
			return p, nil
		}
		ev := m.ev
		if ev.r != nil {
			now := time.Now()
			p.cur = ev.r
			p.meter.tick(now)
			p.renders++
			p.err = nil
			p.gfx.setRendered(ev.r)
		}
		if ev.err != nil {
			p.err = ev.err
		}
		p.ended = ev.ended
		p.dropped = ev.dropped
		if p.closed.Load() {
			return p, nil
		}
		return p, p.listen()
	case tea.WindowSizeMsg:
		// The host repaints after a resize; images must be redrawn.
		p.gfx.invalidate()
	}
	return p, nil
}

// SetSize sets the pane's outer size in cells, including the border.
func (p *Pane) SetSize(cols, rows int) {
	if cols == p.width && rows == p.height {
		return
	}
	p.width, p.height = cols, rows
	bc, br := max(cols-chromeCols, 0), max(rows-chromeRows, 0)
	p.gfx.setBody(bc, br)
	p.updateParams(func(rp *RenderParams) { rp.Cols, rp.Rows = bc, br })
}

// Size returns the outer size set by SetSize.
func (p *Pane) Size() (cols, rows int) { return p.width, p.height }

// SetOrigin tells the pane where its top-left cell is on screen (0-based).
// It is required for image protocols, whose pictures are positioned
// absolutely. With SideBySide in an alt-screen program the origin of the
// right block is (0, leftWidth).
func (p *Pane) SetOrigin(row, col int) { p.gfx.setOrigin(row, col) }

// SetVisible hides or shows the image. Hiding removes on-screen pictures at
// the next flush.
func (p *Pane) SetVisible(v bool) {
	p.visible = v
	p.gfx.setVisible(v)
}

// SetOverlay sets the cursor/highlight/label overlay. Pointer fields are
// copied.
func (p *Pane) SetOverlay(o Overlay) {
	o = o.Clone()
	p.overlay = o
	p.updateParams(func(rp *RenderParams) { rp.Overlay = o })
}

// Overlay returns the current overlay.
func (p *Pane) Overlay() Overlay { return p.overlay.Clone() }

// Focus marks the pane as focused (highlighted border).
func (p *Pane) Focus() { p.focused = true }

// Blur removes focus.
func (p *Pane) Blur() { p.focused = false }

// Focused reports whether the pane has focus.
func (p *Pane) Focused() bool { return p.focused }

// Stats returns rendering statistics.
func (p *Pane) Stats() PaneStats {
	s := PaneStats{Protocol: p.caps.Protocol, FPS: p.meter.value(time.Now()), Dropped: p.dropped}
	if p.cur != nil {
		s.Seq, s.EncodeTime = p.cur.Seq, p.cur.EncodeTime
		if !p.cur.FrameTime.IsZero() {
			s.Latency = p.meter.last.Sub(p.cur.FrameTime)
		}
	}
	return s
}

// Close stops the worker and returns the escape sequence that removes any
// picture still on screen. Write it to the terminal before restoring it
// (when using WrapOutput, the next flush does this automatically).
func (p *Pane) Close() string {
	if p.closed.Swap(true) {
		return ""
	}
	p.cancel()
	return string(p.gfx.clear())
}

func (p *Pane) updateParams(f func(*RenderParams)) {
	p.pmu.Lock()
	f(&p.params)
	p.pmu.Unlock()
	p.kick.Put(struct{}{})
}

func (p *Pane) snapshotParams() RenderParams {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	return p.params.cloneParams()
}

// run is the worker goroutine: it subscribes, coalesces frames, paces and
// renders, and publishes results through p.events (latest wins).
func (p *Pane) run() {
	ctx := p.ctx
	ch, err := p.src.Subscribe(ctx)
	if err != nil {
		p.events.Put(paneEvent{err: fmt.Errorf("subscribe: %w", err), ended: true})
		return
	}
	frames := NewLatest[Frame]()
	srcDone := make(chan struct{})
	go pump(ctx, ch, frames, srcDone)

	r := NewRenderer(p.opts.KittyIDBase)
	pacer := NewPacer(p.opts.MaxFPS)
	interval := time.Second / time.Duration(p.opts.MaxFPS)
	var (
		cur     Frame
		have    bool
		ended   bool
		lastErr error
		anim    cursorAnimator
		ticker  *time.Timer
	)
	defer func() {
		if ticker != nil {
			ticker.Stop()
		}
	}()
	done := (<-chan struct{})(srcDone)
	for {
		var tick <-chan time.Time
		if anim.animating(time.Now()) {
			if ticker == nil {
				ticker = time.NewTimer(interval)
			} else {
				ticker.Reset(interval)
			}
			tick = ticker.C
		}
		select {
		case <-ctx.Done():
			return
		case <-frames.Ready():
		case <-p.kick.Ready():
		case <-tick:
		case <-done:
			done = nil
			if _, ok := frames.Peek(); !ok {
				ended = true
				p.events.Put(paneEvent{ended: true, err: lastErr, dropped: frames.Dropped()})
			}
		}
		if !pacer.Wait(ctx) {
			return
		}
		if f, ok := frames.Take(); ok {
			cur, have = f, true
		}
		if done == nil && !ended {
			ended = true
		}
		params := p.snapshotParams()
		now := time.Now()
		anim.target(params.Overlay.Cursor, now)
		if params.Overlay.Cursor != nil {
			c := anim.at(now)
			params.Cursor = &c
		}
		if !have {
			continue
		}
		out, changed, err := r.Render(cur, params)
		if err != nil {
			lastErr = err
			p.events.Put(paneEvent{err: err, ended: ended, dropped: frames.Dropped()})
			continue
		}
		if !changed {
			continue
		}
		pacer.Mark()
		lastErr = nil
		p.events.Put(paneEvent{r: &out, ended: ended, dropped: frames.Dropped()})
	}
}

// View renders the bordered pane. It returns "" until SetSize was called
// with a usable size.
func (p *Pane) View() string {
	w, h := p.width, p.height
	if w < chromeCols+4 || h < chromeRows+1 {
		return ""
	}
	iw, bodyRows := w-chromeCols, h-chromeRows
	bs := p.styles.border
	if p.focused {
		bs = p.styles.focusBorder
	}
	b := lipgloss.RoundedBorder()
	var sb strings.Builder
	sb.Grow(w * h * 4)

	// Top border with the title.
	title := " " + p.opts.Title + " "
	tw := StringWidth(title)
	if tw > iw-2 {
		title = TruncateANSI(title, max(iw-2, 0))
		tw = StringWidth(title)
	}
	sb.WriteString(bs.Render(b.TopLeft + b.Top))
	sb.WriteString(p.styles.title.Render(title))
	sb.WriteString(bs.Render(strings.Repeat(b.Top, max(iw-1-tw, 0)) + b.TopRight))
	sb.WriteByte('\n')

	left := bs.Render(b.Left)
	right := bs.Render(b.Right)
	row := func(content string) {
		sb.WriteString(left)
		sb.WriteString(PadANSI(content, iw))
		sb.WriteString(right)
		sb.WriteByte('\n')
	}

	row(p.titleBar(iw))
	p.body(iw, bodyRows, row)
	row(p.statusLine(iw))

	sb.WriteString(bs.Render(b.BottomLeft + strings.Repeat(b.Bottom, iw) + b.BottomRight))
	return sb.String()
}

func (p *Pane) titleBar(iw int) string {
	url := p.overlay.URL
	if url == "" {
		url = "about:blank"
	}
	st := p.Stats()
	right := fmt.Sprintf("%2.0f fps · %s", st.FPS, st.Protocol)
	// The encode time is diagnostic: show it only when the URL still fits.
	if st.EncodeTime > 0 {
		withMS := right + fmt.Sprintf(" · %dms", st.EncodeTime.Milliseconds())
		if iw-StringWidth(withMS)-4 > StringWidth(url) {
			right = withMS
		}
	}
	dot := p.styles.accent.Render("●")
	if p.err != nil {
		dot = p.styles.errStyle.Render("●")
	} else if p.ended || p.cur == nil {
		dot = p.styles.muted.Render("○")
	}
	rw := StringWidth(right)
	room := iw - rw - 3
	if room < 8 {
		return " " + dot + " " + TruncateANSI(url, max(iw-3, 0))
	}
	return " " + dot + " " + PadANSI(p.styles.chrome.Render(TruncateANSI(url, room-1)), room) + p.styles.muted.Render(right)
}

func (p *Pane) statusLine(iw int) string {
	if p.err != nil {
		return " " + p.styles.errStyle.Render(TruncateANSI("✗ "+oneLine(p.err.Error()), iw-1))
	}
	var parts []string
	if p.overlay.Label != "" {
		parts = append(parts, p.styles.accent.Render("▶ "+oneLine(p.overlay.Label)))
	}
	switch {
	case p.overlay.Status != "":
		parts = append(parts, p.styles.muted.Render(oneLine(p.overlay.Status)))
	case p.ended:
		parts = append(parts, p.styles.muted.Render("stream ended"))
	case p.cur == nil:
		parts = append(parts, p.styles.muted.Render("waiting for browser…"))
	}
	return " " + TruncateANSI(strings.Join(parts, "  "), iw-1)
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func (p *Pane) body(iw, rows int, row func(string)) {
	cur := p.cur
	placeholder := func(msg string) {
		for i := 0; i < rows; i++ {
			if i == rows/2 {
				pad := max((iw-StringWidth(msg))/2, 0)
				row(strings.Repeat(" ", pad) + p.styles.muted.Render(msg))
			} else {
				row("")
			}
		}
	}
	switch {
	case p.src == nil:
		placeholder("no browser attached")
		return
	case p.caps.Protocol == ProtocolNone:
		placeholder("graphics disabled (GTA_GRAPHICS=off)")
		return
	case cur == nil || cur.Layout.Cols == 0:
		if p.err != nil {
			placeholder("no frame")
		} else {
			placeholder("waiting for first frame…")
		}
		return
	}
	if cur.Protocol.IsImage() {
		// Blank cells reserved for the out-of-band picture. A zero-width
		// SGR that alternates per rendered frame guarantees bubbletea sees
		// a changed line and flushes, which lets WrapOutput append the new
		// picture right after the text it belongs to.
		marker := "\x1b[0m"
		if p.renders&1 == 1 {
			marker = "\x1b[m"
		}
		for i := 0; i < rows; i++ {
			if i == 0 {
				row(marker)
			} else {
				row("")
			}
		}
		return
	}
	l := cur.Layout
	pad := strings.Repeat(" ", l.OffsetCol)
	for i := 0; i < rows; i++ {
		j := i - l.OffsetRow
		if j >= 0 && j < len(cur.Lines) {
			row(pad + cur.Lines[j])
		} else {
			row("")
		}
	}
}

// Graphics returns the escape sequence that draws the current picture for
// image protocols, positioned absolutely: it saves the cursor (DECSC),
// moves to the body origin (CUP, computed from SetOrigin) draws the picture
// and restores the cursor (DECRC). It returns "" when there is nothing new
// to draw, unless force is set (use force after the host cleared or
// repainted the screen). Hosts that write the terminal themselves call it
// after every frame they flush; bubbletea hosts should use WrapOutput
// instead.
func (p *Pane) Graphics(force bool) string { return string(p.gfx.emit(force)) }

// Invalidate makes the next Graphics/WrapOutput flush redraw the picture.
func (p *Pane) Invalidate() { p.gfx.invalidate() }

// WrapOutput returns a writer for tea.WithOutput that forwards everything
// to w and, after each renderer flush, appends the pane's picture when it
// changed or when the flush may have overwritten it (sixel and iTerm2
// pictures live in the cell grid and are erased by repainted text; Kitty
// pictures are redrawn after screen clears). The returned writer also
// implements Fd() when w does, so bubbletea still detects the tty.
//
// Usage:
//
//	pane := browserview.New(src, opts)
//	prog := tea.NewProgram(model, tea.WithAltScreen(), tea.WithOutput(pane.WrapOutput(os.Stdout)))
func (p *Pane) WrapOutput(w io.Writer) io.Writer {
	gw := &graphicsWriter{w: w, g: p.gfx}
	if f, ok := w.(interface{ Fd() uintptr }); ok {
		return &graphicsFileWriter{graphicsWriter: gw, fd: f}
	}
	return gw
}

type graphicsWriter struct {
	mu sync.Mutex
	w  io.Writer
	g  *graphicsState
}

func (gw *graphicsWriter) Write(b []byte) (int, error) {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	n, err := gw.w.Write(b)
	if err != nil {
		return n, err
	}
	if extra := gw.g.afterFlush(b); len(extra) > 0 {
		if _, err := gw.w.Write(extra); err != nil {
			return n, err
		}
	}
	return n, nil
}

type graphicsFileWriter struct {
	*graphicsWriter
	fd interface{ Fd() uintptr }
}

func (g *graphicsFileWriter) Fd() uintptr { return g.fd.Fd() }

// Close is a no-op: the wrapped terminal belongs to the caller.
func (g *graphicsFileWriter) Close() error { return nil }

// Read lets bubbletea treat the writer like the *os.File it wraps when it
// needs a reader for terminal queries; it delegates when possible.
func (g *graphicsFileWriter) Read(b []byte) (int, error) {
	if r, ok := g.fd.(io.Reader); ok {
		return r.Read(b)
	}
	return 0, io.EOF
}

// graphicsState is shared between the UI goroutine (Update) and the
// terminal writer goroutine.
type graphicsState struct {
	mu        sync.Mutex
	proto     Protocol
	tmux      bool
	r         *Rendered
	originRow int
	originCol int
	bodyCols  int
	bodyRows  int
	pending   bool
	hidden    bool
	// What is currently on screen.
	shownKitty uint32
	shownRect  image.Rectangle // in 1-based screen cells; empty if none
}

func (g *graphicsState) setRendered(r *Rendered) {
	if !r.Protocol.IsImage() {
		return
	}
	g.mu.Lock()
	g.r, g.pending = r, true
	g.mu.Unlock()
}

func (g *graphicsState) setOrigin(row, col int) {
	g.mu.Lock()
	if row != g.originRow || col != g.originCol {
		g.originRow, g.originCol, g.pending = row, col, true
	}
	g.mu.Unlock()
}

func (g *graphicsState) setBody(cols, rows int) {
	g.mu.Lock()
	g.bodyCols, g.bodyRows = cols, rows
	g.mu.Unlock()
}

func (g *graphicsState) setVisible(v bool) {
	g.mu.Lock()
	if g.hidden == v {
		g.hidden, g.pending = !v, true
	}
	g.mu.Unlock()
}

func (g *graphicsState) invalidate() {
	g.mu.Lock()
	g.pending = true
	g.mu.Unlock()
}

// afterFlush decides what to append after the host wrote b.
func (g *graphicsState) afterFlush(b []byte) []byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.proto.IsImage() {
		return nil
	}
	cleared := bytesContainsAny(b, "\x1b[2J", "\x1b[H\x1b[J", "\x1bc")
	force := cleared
	if g.proto != ProtocolKitty && !g.shownRect.Empty() && StringWidth(string(b)) > 0 {
		// Cell-based pictures may have been overwritten by repainted text.
		force = true
	}
	if cleared {
		g.shownKitty = 0
		g.shownRect = image.Rectangle{}
	}
	return g.emitLocked(force)
}

func bytesContainsAny(b []byte, subs ...string) bool {
	s := string(b)
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func (g *graphicsState) emit(force bool) []byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.emitLocked(force)
}

func (g *graphicsState) emitLocked(force bool) []byte {
	if !g.pending && !force {
		return nil
	}
	g.pending = false
	r := g.r
	var rect image.Rectangle
	if r != nil && !g.hidden && r.Layout.Cols > 0 && r.Layout.Rows <= g.bodyRows && r.Layout.Cols <= g.bodyCols {
		row := g.originRow + BodyRowOffset + r.Layout.OffsetRow + 1
		col := g.originCol + BodyColOffset + r.Layout.OffsetCol + 1
		rect = image.Rect(col, row, col+r.Layout.Cols, row+r.Layout.Rows)
	}
	var out []byte
	if rect.Empty() {
		return g.clearLocked()
	}
	if g.proto != ProtocolKitty && !g.shownRect.Empty() && g.shownRect != rect {
		out = append(out, blankArea(g.shownRect.Min.Y, g.shownRect.Min.X, g.shownRect.Dy(), g.shownRect.Dx())...)
	}
	var sb strings.Builder
	sb.WriteString(cursorSave)
	writeCUP(&sb, rect.Min.Y, rect.Min.X)
	out = append(out, sb.String()...)
	out = append(out, r.Image...)
	out = append(out, cursorRestore...)
	if g.proto == ProtocolKitty && g.shownKitty != 0 && g.shownKitty != r.KittyID {
		out = append(out, KittyDeleteSeq(g.shownKitty, g.tmux)...)
	}
	g.shownKitty = r.KittyID
	g.shownRect = rect
	return out
}

func (g *graphicsState) clear() []byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.hidden = true
	return g.clearLocked()
}

func (g *graphicsState) clearLocked() []byte {
	var out []byte
	if g.shownKitty != 0 {
		out = append(out, KittyDeleteSeq(g.shownKitty, g.tmux)...)
		g.shownKitty = 0
	}
	if !g.shownRect.Empty() && g.proto != ProtocolKitty {
		out = append(out, blankArea(g.shownRect.Min.Y, g.shownRect.Min.X, g.shownRect.Dy(), g.shownRect.Dx())...)
	}
	g.shownRect = image.Rectangle{}
	return out
}
