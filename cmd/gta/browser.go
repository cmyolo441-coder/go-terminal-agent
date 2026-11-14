package main

import (
	"context"
	"errors"
	"fmt"
	"image"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/browser"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/browsertools"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/browserview"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/config"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/ui"
)

// Viewport of the agent's browser. Screencast frames use the same size so
// overlay coordinates (CSS pixels) map 1:1 onto frame pixels.
const (
	browserWidth  = 1280
	browserHeight = 800
	// browserLaunchTimeout bounds starting Chromium and opening its page.
	browserLaunchTimeout = 3 * time.Minute
)

// browserHost owns the agent's browser: one Chromium launched on first
// use, one page, the browser_* tools that drive it, the live terminal
// dock and an optional local web viewer.
type browserHost struct {
	headless bool

	mu      sync.Mutex
	br      *browser.Browser
	page    *browser.Page
	ready   chan struct{} // closed once page is set
	web     *browserview.Server
	webStop context.CancelFunc
	closed  bool

	sess *browsertools.Session

	// overlay carries the latest action overlay to the dock (UI
	// goroutine) and the web viewer.
	overlay *browserview.Latest[browserview.Overlay]
	visible atomic.Bool
}

func newBrowserHost(cfg config.Config) *browserHost {
	h := &browserHost{
		headless: !cfg.BrowserHeadful,
		ready:    make(chan struct{}),
		overlay:  browserview.NewLatest[browserview.Overlay](),
	}
	h.sess = browsertools.NewSession(h.driver, browsertools.Options{})
	h.sess.OnAction(h.onAction)
	return h
}

// tools returns the browser_* tools for the agent.
func (h *browserHost) tools() []tools.Tool { return browsertools.Tools(h.sess) }

// driver launches Chromium on first use. It is the browsertools provider.
func (h *browserHost) driver(ctx context.Context) (browsertools.Driver, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, errors.New("browser: shut down")
	}
	if h.page != nil {
		return h.page, nil
	}
	// Launching is not bounded by one action's timeout: Chromium's first
	// start on a cold machine (font cache, new profile) can take longer.
	// If the action gives up first, the launch still finishes and the next
	// call uses the browser.
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), browserLaunchTimeout)
	defer cancel()
	ctx = lctx
	br, err := browser.Launch(ctx, browser.Options{
		Headless: h.headless,
		Width:    browserWidth,
		Height:   browserHeight,
	})
	if err != nil {
		return nil, fmt.Errorf("browser: %w (install Chrome or Chromium, or set %s)", err, browser.EnvBrowser)
	}
	page, err := br.NewPage(ctx)
	if err != nil {
		br.Close()
		return nil, fmt.Errorf("browser: %w", err)
	}
	h.br, h.page = br, page
	close(h.ready)
	return page, nil
}

// waitPage blocks until the browser has a page or ctx ends.
func (h *browserHost) waitPage(ctx context.Context) (*browser.Page, error) {
	select {
	case <-h.ready:
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.page == nil {
			return nil, errors.New("browser: shut down")
		}
		return h.page, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Subscribe implements browserview.Source: it waits for the agent to open
// the browser, then streams screencast frames until ctx ends or the page
// closes.
func (h *browserHost) Subscribe(ctx context.Context) (<-chan browserview.Frame, error) {
	out := make(chan browserview.Frame, 2)
	go func() {
		defer close(out)
		page, err := h.waitPage(ctx)
		if err != nil {
			return
		}
		frames, stop, err := page.StartScreencast(ctx, browser.ScreencastOptions{
			Format: "jpeg", Quality: 80, MaxWidth: browserWidth, MaxHeight: browserHeight,
		})
		if err != nil {
			return
		}
		defer stop()
		for f := range frames {
			bf := browserview.Frame{Data: f.Data, Format: f.Format, Width: f.Width, Height: f.Height, Seq: f.Seq, Timestamp: f.Timestamp}
			select {
			case out <- bf:
			case <-ctx.Done():
				return
			default:
				// The viewer coalesces to the newest frame anyway; drop
				// rather than stall the screencast.
			}
		}
	}()
	return out, nil
}

// onAction turns an agent browser action into the live overlay: the cursor
// at the pointer and a label saying what the agent is doing.
func (h *browserHost) onAction(a browsertools.Action) {
	o := browserview.Overlay{Label: actionLabel(a)}
	if p, ok := h.sess.Cursor(); ok {
		o.Cursor = &image.Point{X: int(p.X), Y: int(p.Y)}
	}
	if a.HasPoint {
		o.Cursor = &image.Point{X: int(a.X), Y: int(a.Y)}
	}
	if a.Err != "" {
		o.Status = "error: " + a.Err
	}
	h.mu.Lock()
	if h.page != nil {
		o.URL = h.page.URL()
	}
	web := h.web
	h.mu.Unlock()
	h.overlay.Put(o)
	if web != nil {
		web.SetOverlay(o)
	}
}

func actionLabel(a browsertools.Action) string {
	s := a.Kind
	if a.Target != "" {
		s += " " + a.Target
	}
	if len(s) > 80 {
		s = s[:77] + "..."
	}
	return s
}

// pageInput forwards web-viewer "take control" events to the page once it
// exists.
type pageInput struct{ h *browserHost }

func (p pageInput) pg(ctx context.Context) (*browser.Page, error) {
	select {
	case <-p.h.ready:
		return p.h.waitPage(ctx)
	default:
		return nil, errors.New("browser not started yet")
	}
}

func (p pageInput) MouseMove(ctx context.Context, x, y float64) error {
	pg, err := p.pg(ctx)
	if err != nil {
		return err
	}
	return pg.MouseMove(ctx, x, y)
}

func (p pageInput) MouseDown(ctx context.Context, x, y float64, b string, n int) error {
	pg, err := p.pg(ctx)
	if err != nil {
		return err
	}
	return pg.MouseDown(ctx, x, y, b, n)
}

func (p pageInput) MouseUp(ctx context.Context, x, y float64, b string, n int) error {
	pg, err := p.pg(ctx)
	if err != nil {
		return err
	}
	return pg.MouseUp(ctx, x, y, b, n)
}

func (p pageInput) Wheel(ctx context.Context, x, y, dx, dy float64) error {
	pg, err := p.pg(ctx)
	if err != nil {
		return err
	}
	return pg.Wheel(ctx, x, y, dx, dy)
}

func (p pageInput) KeyDown(ctx context.Context, key string) error {
	pg, err := p.pg(ctx)
	if err != nil {
		return err
	}
	return pg.KeyDown(ctx, key)
}

func (p pageInput) KeyUp(ctx context.Context, key string) error {
	pg, err := p.pg(ctx)
	if err != nil {
		return err
	}
	return pg.KeyUp(ctx, key)
}

func (p pageInput) InsertText(ctx context.Context, text string) error {
	pg, err := p.pg(ctx)
	if err != nil {
		return err
	}
	return pg.InsertText(ctx, text)
}

// startWeb starts the local live viewer (once) and returns its URL.
func (h *browserHost) startWeb(allowInput bool) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return "", errors.New("browser: shut down")
	}
	if h.web != nil {
		return h.web.URL(), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv, err := browserview.Serve(ctx, "127.0.0.1:0", h, pageInput{h}, browserview.ServerOptions{
		AllowInput: allowInput,
		Title:      "gta · agent browser",
	})
	if err != nil {
		cancel()
		return "", err
	}
	h.web, h.webStop = srv, cancel
	return srv.URL(), nil
}

// close shuts the viewer and the browser down.
func (h *browserHost) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	if h.web != nil {
		h.web.Close()
		h.webStop()
	}
	if h.br != nil {
		h.br.Close()
		h.page = nil
	} else {
		close(h.ready)
	}
}

// command is the /browser slash command.
func (h *browserHost) command() ui.Extra {
	return ui.Extra{
		Name: "browser",
		Args: "[on|off|web|control|open <url>|log]",
		Help: "Live view of the agent's browser: dock it beside the prompt (Ctrl+B), open it in a web page, or navigate",
		Run: func(ctx context.Context, arg string) (string, error) {
			verb, rest, _ := strings.Cut(strings.TrimSpace(arg), " ")
			switch verb {
			case "", "on", "show":
				h.setVisible(true)
				return "Browser panel **on** (Ctrl+B toggles). It goes live as soon as the agent opens a page, or use `/browser open <url>`.", nil
			case "off", "hide":
				h.setVisible(false)
				return "Browser panel off.", nil
			case "web", "control":
				u, err := h.startWeb(verb == "control")
				if err != nil {
					return "", err
				}
				note := "view only"
				if verb == "control" {
					note = "\"Take control\" forwards your mouse and keyboard to the page"
				}
				return fmt.Sprintf("Live viewer (%s, this machine only):\n\n%s", note, u), nil
			case "open", "go":
				u := strings.TrimSpace(rest)
				if u == "" {
					return "", errors.New("usage: /browser open <url>")
				}
				h.setVisible(true)
				ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
				defer cancel()
				res, err := h.sess.Navigate(ctx, u)
				if err != nil {
					return "", err
				}
				return res, nil
			case "log":
				acts := h.sess.Actions()
				if len(acts) == 0 {
					return "No browser actions yet.", nil
				}
				var b strings.Builder
				b.WriteString("| # | time | action | target | result |\n|---|---|---|---|---|\n")
				for _, a := range acts {
					res := "ok"
					if a.Err != "" {
						res = a.Err
					}
					fmt.Fprintf(&b, "| %d | %s | %s | %s | %s |\n", a.Seq, a.Time.Format("15:04:05"), a.Kind, mdCell(a.Target), mdCell(res))
				}
				return b.String(), nil
			}
			return "", fmt.Errorf("unknown /browser argument %q", verb)
		},
	}
}

func mdCell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 60 {
		s = s[:57] + "..."
	}
	return s
}

// setVisible shows or hides the dock from outside the UI goroutine; the
// UI re-lays out when the next dock message arrives.
func (h *browserHost) setVisible(v bool) {
	h.visible.Store(v)
	h.overlay.Put(browserview.Overlay{Label: map[bool]string{true: "panel on", false: "panel off"}[v]})
}

// browserDock is the ui.Dock showing the live browser beside the prompt.
type browserDock struct {
	h    *browserHost
	pane *browserview.Pane
	ctx  context.Context
}

func newBrowserDock(h *browserHost) *browserDock {
	return &browserDock{
		h:   h,
		ctx: context.Background(),
		pane: browserview.New(h, browserview.PaneOptions{
			Title: "agent browser",
			// Half-block cells render inline in any truecolor terminal
			// without absolute positioning, so the dock works in the
			// scrollback-preserving inline UI.
			Protocol: browserview.ProtocolHalfBlock,
			MaxFPS:   24,
		}),
	}
}

type dockOverlayMsg struct{ o browserview.Overlay }

func (d *browserDock) waitOverlay() tea.Cmd {
	return func() tea.Msg {
		o, ok := d.h.overlay.Wait(d.ctx)
		if !ok {
			return nil
		}
		return dockOverlayMsg{o}
	}
}

func (d *browserDock) Init() tea.Cmd { return tea.Batch(d.pane.Init(), d.waitOverlay()) }

func (d *browserDock) Update(msg tea.Msg) tea.Cmd {
	if m, ok := msg.(dockOverlayMsg); ok {
		if m.o.Label != "panel on" && m.o.Label != "panel off" {
			d.pane.SetOverlay(m.o)
		}
		return tea.Batch(func() tea.Msg { return ui.DockChanged() }, d.waitOverlay())
	}
	_, cmd := d.pane.Update(msg)
	return cmd
}

func (d *browserDock) Visible() bool     { return d.h.visible.Load() }
func (d *browserDock) SetVisible(v bool) { d.h.visible.Store(v) }
func (d *browserDock) View(w, h int) string {
	d.pane.SetSize(w, h)
	return d.pane.View()
}
