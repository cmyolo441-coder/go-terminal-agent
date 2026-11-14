package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Page is one browser tab attached through a flattened CDP session. Its
// methods are safe for concurrent use, although input events are only
// meaningful in the order they are sent.
type Page struct {
	b         *Browser
	conn      *cdpConn
	targetID  string
	sessionID string

	mu          sync.Mutex
	url         string
	mainFrameID string
	width       int
	height      int
	closed      bool
	closeErr    error
	unsubs      []func()
	screencast  *screencast
	done        chan struct{} // closed when the page is closed

	in inputState

	net netTracker
}

func newPage(b *Browser, targetID, sessionID string) *Page {
	p := &Page{
		b:         b,
		conn:      b.conn,
		targetID:  targetID,
		sessionID: sessionID,
		url:       "about:blank",
		width:     b.opts.Width,
		height:    b.opts.Height,
		done:      make(chan struct{}),
	}
	p.in.pressed = make(map[string]bool)
	p.in.mu.ch = make(chan struct{}, 1)
	p.net.init()
	return p
}

// on subscribes to an event of this page's session for the page's
// lifetime.
func (p *Page) on(method string, h func(json.RawMessage)) {
	unsub := p.conn.subscribe(p.sessionID, method, func(_ string, params json.RawMessage) { h(params) })
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		unsub()
		return
	}
	p.unsubs = append(p.unsubs, unsub)
	p.mu.Unlock()
}

type frameInfo struct {
	ID          string `json:"id"`
	ParentID    string `json:"parentId"`
	LoaderID    string `json:"loaderId"`
	URL         string `json:"url"`
	URLFragment string `json:"urlFragment"`
}

// init subscribes to the events the page tracks and enables the domains
// that produce them.
func (p *Page) init(ctx context.Context) error {
	p.on("Page.frameNavigated", func(params json.RawMessage) {
		var ev struct {
			Frame frameInfo `json:"frame"`
		}
		if json.Unmarshal(params, &ev) != nil || ev.Frame.ParentID != "" {
			return
		}
		p.mu.Lock()
		p.mainFrameID = ev.Frame.ID
		p.url = ev.Frame.URL + ev.Frame.URLFragment
		p.mu.Unlock()
	})
	p.on("Page.navigatedWithinDocument", func(params json.RawMessage) {
		var ev struct {
			FrameID string `json:"frameId"`
			URL     string `json:"url"`
		}
		if json.Unmarshal(params, &ev) != nil {
			return
		}
		p.mu.Lock()
		if ev.FrameID == p.mainFrameID {
			p.url = ev.URL
		}
		p.mu.Unlock()
	})
	p.on("Inspector.targetCrashed", func(json.RawMessage) { p.markClosed(ErrPageCrashed) })
	p.on("Inspector.detached", func(json.RawMessage) { p.markClosed(ErrPageClosed) })
	p.on("Network.requestWillBeSent", func(params json.RawMessage) {
		var ev struct {
			RequestID string `json:"requestId"`
		}
		if json.Unmarshal(params, &ev) == nil {
			p.net.start(ev.RequestID)
		}
	})
	finish := func(params json.RawMessage) {
		var ev struct {
			RequestID string `json:"requestId"`
		}
		if json.Unmarshal(params, &ev) == nil {
			p.net.finish(ev.RequestID)
		}
	}
	p.on("Network.loadingFinished", finish)
	p.on("Network.loadingFailed", finish)

	for _, cmd := range []struct {
		method string
		params any
	}{
		{"Page.enable", nil},
		{"Page.setLifecycleEventsEnabled", map[string]any{"enabled": true}},
		{"Network.enable", nil},
		{"Runtime.enable", nil},
		{"Emulation.setDeviceMetricsOverride", p.metrics(p.width, p.height)},
	} {
		if err := p.call(ctx, cmd.method, cmd.params, nil); err != nil {
			return err
		}
	}
	var tree struct {
		FrameTree struct {
			Frame frameInfo `json:"frame"`
		} `json:"frameTree"`
	}
	if err := p.call(ctx, "Page.getFrameTree", nil, &tree); err != nil {
		return err
	}
	p.mu.Lock()
	p.mainFrameID = tree.FrameTree.Frame.ID
	if u := tree.FrameTree.Frame.URL; u != "" {
		p.url = u + tree.FrameTree.Frame.URLFragment
	}
	p.mu.Unlock()
	return nil
}

func (p *Page) metrics(w, h int) map[string]any {
	return map[string]any{
		"width": w, "height": h, "deviceScaleFactor": 1, "mobile": false,
		"screenWidth": w, "screenHeight": h,
	}
}

// call sends a command on the page's session, failing fast once the page
// is closed.
func (p *Page) call(ctx context.Context, method string, params, result any) error {
	if err := p.err(); err != nil {
		return err
	}
	err := p.conn.callAbort(ctx, p.done, ErrPageClosed, p.sessionID, method, params, result)
	if err != nil && errors.Is(err, ErrPageClosed) {
		if cerr := p.err(); cerr != nil {
			return fmt.Errorf("cdp %s: %w", method, cerr)
		}
	}
	return err
}

// err returns the reason the page is unusable, or nil.
func (p *Page) err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return p.closeErr
	}
	return nil
}

// markClosed records that the page is gone and releases its
// subscriptions. The first reason wins.
func (p *Page) markClosed(reason error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.closeErr = reason
	unsubs := p.unsubs
	p.unsubs = nil
	close(p.done)
	p.mu.Unlock()
	for _, u := range unsubs {
		u()
	}
	p.b.removePage(p)
}

// Close closes the tab. It is idempotent; closing a page whose browser is
// gone is not an error.
func (p *Page) Close() error {
	if p.err() != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), gracefulCloseTimeout)
	defer cancel()
	err := p.conn.call(ctx, "", "Target.closeTarget", map[string]any{"targetId": p.targetID}, nil)
	p.markClosed(ErrPageClosed)
	if err != nil && !errors.Is(err, ErrClosed) {
		var perr *ProtocolError
		if errors.As(err, &perr) {
			return nil // target already gone
		}
		return fmt.Errorf("browser: close page: %w", err)
	}
	return nil
}

// URL returns the main frame's current URL.
func (p *Page) URL() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.url
}

// Viewport returns the current viewport size in CSS pixels.
func (p *Page) Viewport() (w, h int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.width, p.height
}

// SetViewport resizes the viewport (device metrics override) to w x h CSS
// pixels at a device scale factor of 1.
func (p *Page) SetViewport(ctx context.Context, w, h int) error {
	if w <= 0 || h <= 0 || w > 10000 || h > 10000 {
		return fmt.Errorf("browser: invalid viewport %dx%d", w, h)
	}
	if err := p.call(ctx, "Emulation.setDeviceMetricsOverride", p.metrics(w, h), nil); err != nil {
		return err
	}
	p.mu.Lock()
	p.width, p.height = w, h
	p.mu.Unlock()
	return nil
}

// navWatcher observes main-frame commits and load events from the moment
// it is created, so a navigation's events cannot be missed between sending
// the command and receiving its response.
type navWatcher struct {
	p       *Page
	mu      sync.Mutex
	commit  []string        // loader ids committed in the main frame, in order
	loaded  map[string]bool // loader ids whose main-frame load fired
	sameDoc bool            // a same-document navigation happened
	notify  chan struct{}
	unsubs  []func()
}

func (p *Page) watchNavigation() *navWatcher {
	w := &navWatcher{p: p, loaded: make(map[string]bool), notify: make(chan struct{}, 1)}
	poke := func() {
		select {
		case w.notify <- struct{}{}:
		default:
		}
	}
	isMain := func(frameID string) bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.mainFrameID == "" || frameID == p.mainFrameID
	}
	sub := func(method string, h func(json.RawMessage)) {
		w.unsubs = append(w.unsubs, p.conn.subscribe(p.sessionID, method, func(_ string, params json.RawMessage) { h(params) }))
	}
	sub("Page.frameNavigated", func(params json.RawMessage) {
		var ev struct {
			Frame frameInfo `json:"frame"`
		}
		if json.Unmarshal(params, &ev) != nil || ev.Frame.ParentID != "" {
			return
		}
		w.mu.Lock()
		w.commit = append(w.commit, ev.Frame.LoaderID)
		w.mu.Unlock()
		poke()
	})
	sub("Page.lifecycleEvent", func(params json.RawMessage) {
		var ev struct {
			FrameID  string `json:"frameId"`
			LoaderID string `json:"loaderId"`
			Name     string `json:"name"`
		}
		if json.Unmarshal(params, &ev) != nil || ev.Name != "load" || !isMain(ev.FrameID) {
			return
		}
		w.mu.Lock()
		w.loaded[ev.LoaderID] = true
		w.mu.Unlock()
		poke()
	})
	sub("Page.navigatedWithinDocument", func(params json.RawMessage) {
		var ev struct {
			FrameID string `json:"frameId"`
		}
		if json.Unmarshal(params, &ev) != nil || !isMain(ev.FrameID) {
			return
		}
		w.mu.Lock()
		w.sameDoc = true
		w.mu.Unlock()
		poke()
	})
	return w
}

func (w *navWatcher) stop() {
	for _, u := range w.unsubs {
		u()
	}
}

// wait blocks until done reports true (evaluated under the watcher's
// lock), the page closes or ctx ends.
func (w *navWatcher) wait(ctx context.Context, what string, done func(w *navWatcher) bool) error {
	for {
		w.mu.Lock()
		ok := done(w)
		w.mu.Unlock()
		if ok {
			return nil
		}
		select {
		case <-w.notify:
		case <-ctx.Done():
			return fmt.Errorf("browser: %s: waiting for load: %w", what, ctx.Err())
		case <-w.p.done:
			return fmt.Errorf("browser: %s: %w", what, w.p.err())
		case <-w.p.conn.Done():
			return fmt.Errorf("browser: %s: %w", what, ErrClosed)
		}
	}
}

// loadedAfter reports whether loader, or a loader committed after it (a
// redirect by script or meta refresh that superseded it), has loaded.
func (w *navWatcher) loadedAfter(loader string) bool {
	if w.loaded[loader] {
		return true
	}
	seen := false
	for _, l := range w.commit {
		if l == loader {
			seen = true
			continue
		}
		if seen && w.loaded[l] {
			return true
		}
	}
	return false
}

// anyCommittedLoaded reports whether some document committed since the
// watcher started has loaded, or a same-document navigation happened.
func (w *navWatcher) anyCommittedLoaded() bool {
	if w.sameDoc {
		return true
	}
	for _, l := range w.commit {
		if w.loaded[l] {
			return true
		}
	}
	return false
}

// Navigate loads url in the main frame and waits for its load event.
// Same-document navigations (fragment changes) return once committed.
// Network-level failures (DNS, refused connections) are returned as errors.
func (p *Page) Navigate(ctx context.Context, url string) error {
	w := p.watchNavigation()
	defer w.stop()
	var res struct {
		FrameID   string `json:"frameId"`
		LoaderID  string `json:"loaderId"`
		ErrorText string `json:"errorText"`
	}
	if err := p.call(ctx, "Page.navigate", map[string]any{"url": url}, &res); err != nil {
		return fmt.Errorf("browser: navigate to %s: %w", url, err)
	}
	if res.ErrorText != "" {
		return fmt.Errorf("browser: navigate to %s: %s", url, res.ErrorText)
	}
	if res.LoaderID == "" {
		// Same-document navigation: the URL update arrives as an event
		// that may trail the response. Wait briefly so URL() reflects it.
		sctx, cancel := context.WithTimeout(ctx, sameDocumentWait)
		defer cancel()
		err := w.wait(sctx, "navigate to "+url, func(w *navWatcher) bool { return w.sameDoc })
		if err != nil && sctx.Err() != nil && ctx.Err() == nil {
			return nil // committed; the event did not arrive in time
		}
		return err
	}
	return w.wait(ctx, "navigate to "+url, func(w *navWatcher) bool { return w.loadedAfter(res.LoaderID) })
}

// Reload reloads the page and waits for the load event.
func (p *Page) Reload(ctx context.Context) error {
	w := p.watchNavigation()
	defer w.stop()
	if err := p.call(ctx, "Page.reload", map[string]any{"ignoreCache": false}, nil); err != nil {
		return fmt.Errorf("browser: reload: %w", err)
	}
	return w.wait(ctx, "reload", (*navWatcher).anyCommittedLoaded)
}

// sameDocumentWait bounds the wait for a same-document navigation's event.
const sameDocumentWait = time.Second

// ErrNoHistory is returned by Back and Forward at either end of the
// session history.
var ErrNoHistory = errors.New("browser: no history entry in that direction")

// Back goes one entry back in session history and waits for it to load.
func (p *Page) Back(ctx context.Context) error { return p.history(ctx, -1) }

// Forward goes one entry forward in session history and waits for it to
// load.
func (p *Page) Forward(ctx context.Context) error { return p.history(ctx, +1) }

func (p *Page) history(ctx context.Context, delta int) error {
	var h struct {
		CurrentIndex int `json:"currentIndex"`
		Entries      []struct {
			ID int `json:"id"`
		} `json:"entries"`
	}
	if err := p.call(ctx, "Page.getNavigationHistory", nil, &h); err != nil {
		return fmt.Errorf("browser: history: %w", err)
	}
	idx := h.CurrentIndex + delta
	if idx < 0 || idx >= len(h.Entries) {
		return ErrNoHistory
	}
	w := p.watchNavigation()
	defer w.stop()
	if err := p.call(ctx, "Page.navigateToHistoryEntry", map[string]any{"entryId": h.Entries[idx].ID}, nil); err != nil {
		return fmt.Errorf("browser: history: %w", err)
	}
	return w.wait(ctx, "history navigation", (*navWatcher).anyCommittedLoaded)
}

// EvalError is a JavaScript exception thrown by Evaluate's expression.
type EvalError struct {
	Message      string
	Line, Column int
}

func (e *EvalError) Error() string { return "javascript: " + e.Message }

type remoteObject struct {
	Type                string          `json:"type"`
	Subtype             string          `json:"subtype"`
	Value               json.RawMessage `json:"value"`
	UnserializableValue string          `json:"unserializableValue"`
	Description         string          `json:"description"`
}

// Evaluate runs expr in the page's main world, awaiting a returned promise,
// and returns the result serialized as JSON. undefined becomes null and
// values JSON cannot represent (NaN, Infinity, -0, bigints) become
// strings. A thrown exception is returned as an *EvalError.
func (p *Page) Evaluate(ctx context.Context, expr string) (json.RawMessage, error) {
	var res struct {
		Result           remoteObject `json:"result"`
		ExceptionDetails *struct {
			Text         string        `json:"text"`
			LineNumber   int           `json:"lineNumber"`
			ColumnNumber int           `json:"columnNumber"`
			Exception    *remoteObject `json:"exception"`
		} `json:"exceptionDetails"`
	}
	params := map[string]any{
		"expression":    expr,
		"returnByValue": true,
		"awaitPromise":  true,
		"userGesture":   true,
	}
	if err := p.call(ctx, "Runtime.evaluate", params, &res); err != nil {
		return nil, fmt.Errorf("browser: evaluate: %w", err)
	}
	if ed := res.ExceptionDetails; ed != nil {
		msg := ed.Text
		if ex := ed.Exception; ex != nil {
			switch {
			case ex.Description != "":
				msg = ex.Description
			case len(ex.Value) > 0:
				var s string
				if json.Unmarshal(ex.Value, &s) == nil {
					msg = strings.TrimSpace(ed.Text + " " + s)
				} else {
					msg = strings.TrimSpace(ed.Text + " " + string(ex.Value))
				}
			}
		}
		return nil, &EvalError{Message: msg, Line: ed.LineNumber, Column: ed.ColumnNumber}
	}
	r := res.Result
	switch {
	case r.UnserializableValue != "":
		b, _ := json.Marshal(r.UnserializableValue)
		return b, nil
	case len(r.Value) > 0:
		return r.Value, nil
	default:
		return json.RawMessage("null"), nil
	}
}

// Title returns document.title.
func (p *Page) Title(ctx context.Context) (string, error) {
	raw, err := p.Evaluate(ctx, "document.title")
	if err != nil {
		return "", err
	}
	var t string
	if err := json.Unmarshal(raw, &t); err != nil {
		return "", fmt.Errorf("browser: title: %w", err)
	}
	return t, nil
}

// normalizeImageFormat maps user spellings to CDP image formats.
func normalizeImageFormat(format string, allowWebP bool) (string, error) {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "png":
		return "png", nil
	case "jpeg", "jpg":
		return "jpeg", nil
	case "webp":
		if allowWebP {
			return "webp", nil
		}
	}
	return "", fmt.Errorf("browser: unsupported image format %q", format)
}

// Screenshot captures the viewport as "png" (default), "jpeg" or "webp".
// quality (1-100) applies to jpeg and webp; 0 means 80.
func (p *Page) Screenshot(ctx context.Context, format string, quality int) ([]byte, error) {
	f, err := normalizeImageFormat(format, true)
	if err != nil {
		return nil, err
	}
	params := map[string]any{"format": f, "fromSurface": true, "captureBeyondViewport": false}
	if f != "png" {
		params["quality"] = clampQuality(quality)
	}
	var res struct {
		Data string `json:"data"`
	}
	if err := p.call(ctx, "Page.captureScreenshot", params, &res); err != nil {
		return nil, fmt.Errorf("browser: screenshot: %w", err)
	}
	img, err := base64.StdEncoding.DecodeString(res.Data)
	if err != nil {
		return nil, fmt.Errorf("browser: screenshot: decode: %w", err)
	}
	return img, nil
}

func clampQuality(q int) int {
	switch {
	case q <= 0:
		return 80
	case q > 100:
		return 100
	}
	return q
}

// InsertText inserts text at the focused element as if typed by an IME,
// without key events.
func (p *Page) InsertText(ctx context.Context, text string) error {
	if err := p.call(ctx, "Input.insertText", map[string]any{"text": text}, nil); err != nil {
		return fmt.Errorf("browser: insert text: %w", err)
	}
	return nil
}

// WaitIdle waits until the page has had no network request in flight for
// quiet (500ms when quiet <= 0). Long-lived requests such as event streams
// keep the page busy; bound the wait with ctx.
func (p *Page) WaitIdle(ctx context.Context, quiet time.Duration) error {
	if quiet <= 0 {
		quiet = 500 * time.Millisecond
	}
	timer := time.NewTimer(quiet)
	defer timer.Stop()
	for {
		if err := p.err(); err != nil {
			return err
		}
		inflight, since, changed := p.net.state()
		if inflight == 0 {
			idleFor := time.Since(since)
			if idleFor >= quiet {
				return nil
			}
			timer.Reset(quiet - idleFor)
		} else {
			timer.Reset(time.Hour)
		}
		select {
		case <-changed:
		case <-timer.C:
		case <-ctx.Done():
			return fmt.Errorf("browser: wait for network idle: %w", ctx.Err())
		case <-p.done:
		}
	}
}

// netTracker counts in-flight network requests by request id.
type netTracker struct {
	mu       sync.Mutex
	inflight map[string]struct{}
	last     time.Time     // last time the count changed
	changed  chan struct{} // closed and replaced on every change
}

func (n *netTracker) init() {
	n.inflight = make(map[string]struct{})
	n.last = time.Now()
	n.changed = make(chan struct{})
}

func (n *netTracker) bump() {
	n.last = time.Now()
	close(n.changed)
	n.changed = make(chan struct{})
}

func (n *netTracker) start(id string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	// Redirects reuse the request id; the request is still one in flight.
	n.inflight[id] = struct{}{}
	n.bump()
}

func (n *netTracker) finish(id string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, ok := n.inflight[id]; !ok {
		return
	}
	delete(n.inflight, id)
	n.bump()
}

func (n *netTracker) state() (inflight int, since time.Time, changed <-chan struct{}) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.inflight), n.last, n.changed
}
