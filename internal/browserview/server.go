package browserview

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"image/jpeg"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

//go:embed viewer.html
var viewerHTML string

// ServerOptions configures the local web viewer.
type ServerOptions struct {
	// AllowInput enables "take control": mouse and keyboard events from
	// the page are forwarded to Input. Disabled by default.
	AllowInput bool
	// MaxFPS caps frames pushed to each client. Default 30.
	MaxFPS int
	// JPEGQuality is used when PNG frames are transcoded for MJPEG.
	// Default 80.
	JPEGQuality int
	// Title is shown in the page header. Default "gta browser".
	Title string
	// MaxClients bounds concurrent streaming clients. Default 8.
	MaxClients int
	// Token fixes the bearer token (mainly for tests). Default: 32 random
	// bytes, hex encoded.
	Token string
	// PingInterval for WebSocket keep-alive. Default 20s.
	PingInterval time.Duration
	// InputTimeout bounds each Input call. Default 5s.
	InputTimeout time.Duration
	// Logf receives diagnostic messages. Default: discard.
	Logf func(format string, args ...any)
}

// Server is the loopback live viewer. Create it with Serve.
type Server struct {
	opts  ServerOptions
	src   Source
	in    Input
	token string
	ln    net.Listener
	http  *http.Server
	hosts map[string]bool

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	clients map[*viewerClient]struct{}
	latest  atomic.Pointer[hubFrame]
	overlay atomic.Pointer[Overlay]
	ended   atomic.Bool

	closeOnce sync.Once
	closeErr  error
}

type hubFrame struct {
	Frame
	jpegOnce sync.Once
	jpeg     []byte
	jpegErr  error
}

// jpegBytes returns the frame as JPEG, transcoding PNG once per frame.
func (h *hubFrame) jpegBytes(quality int) ([]byte, error) {
	if h.format() == "jpeg" {
		return h.Data, nil
	}
	h.jpegOnce.Do(func() {
		img, err := h.Decode()
		if err != nil {
			h.jpegErr = err
			return
		}
		var b bytes.Buffer
		h.jpegErr = jpeg.Encode(&b, img, &jpeg.Options{Quality: quality})
		h.jpeg = b.Bytes()
	})
	return h.jpeg, h.jpegErr
}

type viewerClient struct {
	frames  *Latest[*hubFrame]
	overlay *Latest[Overlay]
	ended   chan struct{}
	endOnce sync.Once
}

func (c *viewerClient) end() { c.endOnce.Do(func() { close(c.ended) }) }

// Serve starts the viewer on addr, which must be a loopback address
// ("127.0.0.1:0" when empty). The page is at Server.URL(), which embeds a
// random bearer token; every endpoint requires it. The server stops when
// ctx is cancelled or Close is called.
func Serve(ctx context.Context, addr string, src Source, in Input, opts ServerOptions) (*Server, error) {
	if src == nil {
		return nil, errors.New("browserview: nil source")
	}
	if opts.MaxFPS <= 0 {
		opts.MaxFPS = 30
	}
	if opts.JPEGQuality <= 0 || opts.JPEGQuality > 100 {
		opts.JPEGQuality = 80
	}
	if opts.Title == "" {
		opts.Title = "gta browser"
	}
	if opts.MaxClients <= 0 {
		opts.MaxClients = 8
	}
	if opts.PingInterval <= 0 {
		opts.PingInterval = 20 * time.Second
	}
	if opts.InputTimeout <= 0 {
		opts.InputTimeout = 5 * time.Second
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("browserview: bad listen address %q: %w", addr, err)
	}
	switch {
	case host == "" || strings.EqualFold(host, "localhost"):
		host = "127.0.0.1"
	default:
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return nil, fmt.Errorf("browserview: refusing to listen on non-loopback address %q", addr)
		}
	}
	token := opts.Token
	if token == "" {
		var b [32]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, fmt.Errorf("browserview: token: %w", err)
		}
		token = hex.EncodeToString(b[:])
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, fmt.Errorf("browserview: listen: %w", err)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	sctx, cancel := context.WithCancel(ctx)
	s := &Server{
		opts: opts, src: src, in: in, token: token, ln: ln,
		ctx: sctx, cancel: cancel, clients: map[*viewerClient]struct{}{},
	}
	_, p, _ := net.SplitHostPort(ln.Addr().String())
	s.hosts = map[string]bool{}
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		s.hosts["["+host+"]:"+p] = true
		s.hosts["[::1]:"+p] = true
	} else {
		s.hosts[host+":"+p] = true
	}
	s.hosts["localhost:"+p] = true

	frames, err := src.Subscribe(sctx)
	if err != nil {
		cancel()
		ln.Close()
		return nil, fmt.Errorf("browserview: subscribe: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.guard(s.handleIndex))
	mux.HandleFunc("/ws", s.guard(s.handleWS))
	mux.HandleFunc("/mjpeg", s.guard(s.handleMJPEG))
	mux.HandleFunc("/frame", s.guard(s.handleFrame))
	mux.HandleFunc("/overlay", s.guard(s.handleOverlay))
	s.http = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		BaseContext:       func(net.Listener) context.Context { return sctx },
	}
	s.wg.Add(2)
	go s.hub(frames)
	go func() {
		defer s.wg.Done()
		if err := s.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			opts.Logf("browserview: serve: %v", err)
		}
	}()
	go func() {
		<-sctx.Done()
		_ = s.shutdown()
	}()
	return s, nil
}

// URL returns the viewer URL including the access token.
func (s *Server) URL() string {
	_, p, _ := net.SplitHostPort(s.ln.Addr().String())
	host := "127.0.0.1"
	if tcp, ok := s.ln.Addr().(*net.TCPAddr); ok {
		host = tcp.IP.String()
		if tcp.IP.To4() == nil {
			host = "[" + host + "]"
		}
	}
	return "http://" + host + ":" + p + "/?token=" + s.token
}

// Addr returns the listening address.
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// Token returns the bearer token.
func (s *Server) Token() string { return s.token }

// Clients returns the number of connected streaming clients.
func (s *Server) Clients() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.clients)
}

// SetOverlay pushes cursor/highlight/label information to every viewer.
func (s *Server) SetOverlay(o Overlay) {
	o = o.Clone()
	s.overlay.Store(&o)
	s.mu.Lock()
	for c := range s.clients {
		c.overlay.Put(o)
	}
	s.mu.Unlock()
}

// Close stops the server and disconnects all viewers.
func (s *Server) Close() error {
	s.cancel()
	err := s.shutdown()
	s.wg.Wait()
	return err
}

func (s *Server) shutdown() error {
	s.closeOnce.Do(func() {
		s.cancel()
		s.mu.Lock()
		for c := range s.clients {
			c.end()
		}
		s.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := s.http.Shutdown(ctx); err != nil {
			s.closeErr = s.http.Close()
		}
	})
	return s.closeErr
}

// hub fans the single source subscription out to all clients.
func (s *Server) hub(frames <-chan Frame) {
	defer s.wg.Done()
	for {
		select {
		case <-s.ctx.Done():
			return
		case f, ok := <-frames:
			if !ok {
				s.ended.Store(true)
				s.mu.Lock()
				for c := range s.clients {
					c.end()
				}
				s.mu.Unlock()
				return
			}
			hf := &hubFrame{Frame: f}
			if hf.Width == 0 || hf.Height == 0 {
				if cfg, err := decodeConfig(f); err == nil {
					hf.Width, hf.Height = cfg.Width, cfg.Height
				}
			}
			s.latest.Store(hf)
			s.mu.Lock()
			for c := range s.clients {
				c.frames.Put(hf)
			}
			s.mu.Unlock()
		}
	}
}

func (s *Server) addClient() (*viewerClient, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.clients) >= s.opts.MaxClients || s.ctx.Err() != nil {
		return nil, false
	}
	c := &viewerClient{frames: NewLatest[*hubFrame](), overlay: NewLatest[Overlay](), ended: make(chan struct{})}
	if f := s.latest.Load(); f != nil {
		c.frames.Put(f)
	}
	if o := s.overlay.Load(); o != nil {
		c.overlay.Put(*o)
	}
	if s.ended.Load() {
		c.end()
	}
	s.clients[c] = struct{}{}
	return c, true
}

func (s *Server) removeClient(c *viewerClient) {
	s.mu.Lock()
	delete(s.clients, c)
	s.mu.Unlock()
}

// guard enforces Host, Origin and token checks and sets security headers.
func (s *Server) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hdr := w.Header()
		hdr.Set("Cache-Control", "no-store")
		hdr.Set("X-Content-Type-Options", "nosniff")
		hdr.Set("X-Frame-Options", "DENY")
		hdr.Set("Referrer-Policy", "no-referrer")
		hdr.Set("Cross-Origin-Resource-Policy", "same-origin")
		// DNS-rebinding defence: only our exact loopback authority.
		if !s.hosts[strings.ToLower(r.Host)] {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			if !s.originAllowed(origin) {
				http.Error(w, "forbidden origin", http.StatusForbidden)
				return
			}
		}
		if !s.authorized(r) {
			hdr.Set("WWW-Authenticate", `Bearer realm="gta-browser"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func (s *Server) originAllowed(origin string) bool {
	rest, ok := strings.CutPrefix(strings.ToLower(origin), "http://")
	return ok && s.hosts[rest]
}

func (s *Server) authorized(r *http.Request) bool {
	got := r.URL.Query().Get("token")
	if a := r.Header.Get("Authorization"); got == "" && a != "" {
		if t, ok := strings.CutPrefix(a, "Bearer "); ok {
			got = strings.TrimSpace(t)
		}
	}
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var nb [16]byte
	_, _ = rand.Read(nb[:])
	nonce := hex.EncodeToString(nb[:])
	cfg, _ := json.Marshal(map[string]any{
		"allowInput": s.opts.AllowInput && s.in != nil,
		"title":      s.opts.Title,
		"maxFps":     s.opts.MaxFPS,
	})
	page := strings.NewReplacer(
		"{{NONCE}}", nonce,
		"{{TITLE}}", html.EscapeString(s.opts.Title),
		"{{CONFIG}}", string(cfg),
	).Replace(viewerHTML)
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'nonce-"+nonce+"'; style-src 'nonce-"+nonce+
		"'; img-src 'self' blob: data:; connect-src 'self' ws://"+r.Host+"; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	_, _ = w.Write([]byte(page))
}

func (s *Server) handleFrame(w http.ResponseWriter, r *http.Request) {
	f := s.latest.Load()
	if f == nil {
		http.Error(w, "no frame yet", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", f.MIME())
	w.Header().Set("X-Frame-Seq", strconv.FormatUint(f.Seq, 10))
	_, _ = w.Write(f.Data)
}

func (s *Server) handleOverlay(w http.ResponseWriter, r *http.Request) {
	var o Overlay
	if p := s.overlay.Load(); p != nil {
		o = *p
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(overlayJSON(o))
}

const mjpegBoundary = "gtaframe"

func (s *Server) handleMJPEG(w http.ResponseWriter, r *http.Request) {
	c, ok := s.addClient()
	if !ok {
		http.Error(w, "too many viewers", http.StatusServiceUnavailable)
		return
	}
	defer s.removeClient(c)
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+mjpegBoundary)
	w.WriteHeader(http.StatusOK)
	// Every part is followed immediately by the next boundary so clients
	// can display a frame as soon as it arrives instead of waiting for the
	// following frame to delimit it.
	if _, err := w.Write([]byte("--" + mjpegBoundary + "\r\n")); err != nil {
		return
	}
	_ = rc.Flush()
	pacer := NewPacer(s.opts.MaxFPS)
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.ended:
			return
		case <-c.frames.Ready():
		}
		if !pacer.Wait(ctx) {
			return
		}
		f, ok := c.frames.Take()
		if !ok {
			continue
		}
		data, err := f.jpegBytes(s.opts.JPEGQuality)
		if err != nil {
			s.opts.Logf("browserview: mjpeg transcode: %v", err)
			continue
		}
		pacer.Mark()
		_ = rc.SetWriteDeadline(time.Now().Add(10 * time.Second))
		head := "Content-Type: image/jpeg\r\nContent-Length: " +
			strconv.Itoa(len(data)) + "\r\nX-Frame-Seq: " + strconv.FormatUint(f.Seq, 10) + "\r\n\r\n"
		if _, err := w.Write([]byte(head)); err != nil {
			return
		}
		if _, err := w.Write(data); err != nil {
			return
		}
		if _, err := w.Write([]byte("\r\n--" + mjpegBoundary + "\r\n")); err != nil {
			return
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}
}

// Binary frame message layout (big endian), followed by the image bytes:
//
//	0  u8   message kind (1 = frame)
//	1  u8   format (0 = jpeg, 1 = png)
//	2  u16  reserved
//	4  u32  width
//	8  u32  height
//	12 u64  sequence
//	20 f64  capture time, Unix milliseconds
const frameHeaderLen = 28

func encodeFrameHeader(f *hubFrame) []byte {
	b := make([]byte, frameHeaderLen, frameHeaderLen+len(f.Data))
	b[0] = 1
	if f.format() == "png" {
		b[1] = 1
	}
	binary.BigEndian.PutUint32(b[4:], uint32(max(f.Width, 0)))
	binary.BigEndian.PutUint32(b[8:], uint32(max(f.Height, 0)))
	binary.BigEndian.PutUint64(b[12:], f.Seq)
	var ms float64
	if !f.Timestamp.IsZero() {
		ms = float64(f.Timestamp.UnixNano()) / 1e6
	}
	binary.BigEndian.PutUint64(b[20:], math.Float64bits(ms))
	return b
}

type jsonPoint struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type jsonRect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

func overlayJSON(o Overlay) []byte {
	m := struct {
		T         string     `json:"t"`
		Cursor    *jsonPoint `json:"cursor"`
		Highlight *jsonRect  `json:"highlight"`
		Label     string     `json:"label"`
		URL       string     `json:"url"`
		Status    string     `json:"status"`
	}{T: "overlay", Label: o.Label, URL: o.URL, Status: o.Status}
	if o.Cursor != nil {
		m.Cursor = &jsonPoint{o.Cursor.X, o.Cursor.Y}
	}
	if o.Highlight != nil {
		h := o.Highlight.Canon()
		m.Highlight = &jsonRect{h.Min.X, h.Min.Y, h.Dx(), h.Dy()}
	}
	b, _ := json.Marshal(m)
	return b
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	c, ok := s.addClient()
	if !ok {
		http.Error(w, "too many viewers", http.StatusServiceUnavailable)
		return
	}
	defer s.removeClient(c)
	ws, err := upgradeWebSocket(w, r, 64<<10)
	if err != nil {
		s.opts.Logf("browserview: %v", err)
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()

	readTimeout := 3 * s.opts.PingInterval
	_ = ws.SetReadDeadline(time.Now().Add(readTimeout))
	ws.onPong = func() { _ = ws.SetReadDeadline(time.Now().Add(readTimeout)) }

	hello, _ := json.Marshal(map[string]any{
		"t": "hello", "allowInput": s.opts.AllowInput && s.in != nil, "title": s.opts.Title, "maxFps": s.opts.MaxFPS,
	})
	if err := ws.WriteMessage(wsText, hello); err != nil {
		ws.Close(wsCloseGoingAway, "")
		return
	}

	// Reader: input events and control toggles.
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		defer cancel()
		s.readLoop(ctx, ws)
	}()

	pacer := NewPacer(s.opts.MaxFPS)
	ping := time.NewTicker(s.opts.PingInterval)
	defer ping.Stop()
	defer func() {
		ws.Close(wsCloseGoingAway, "")
		<-readerDone
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.ended:
			end, _ := json.Marshal(map[string]string{"t": "end"})
			_ = ws.WriteMessage(wsText, end)
			ws.Close(wsCloseNormal, "stream ended")
			return
		case <-ping.C:
			if err := ws.Ping(); err != nil {
				return
			}
			continue
		case <-c.overlay.Ready():
			if o, ok := c.overlay.Take(); ok {
				if err := ws.WriteMessage(wsText, overlayJSON(o)); err != nil {
					return
				}
			}
			continue
		case <-c.frames.Ready():
		}
		if !pacer.Wait(ctx) {
			return
		}
		f, ok := c.frames.Take()
		if !ok {
			continue
		}
		pacer.Mark()
		msg := append(encodeFrameHeader(f), f.Data...)
		if err := ws.WriteMessage(wsBinary, msg); err != nil {
			return
		}
	}
}

// inputEvent is a message from the page.
type inputEvent struct {
	T      string  `json:"t"`
	On     bool    `json:"on"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	DX     float64 `json:"dx"`
	DY     float64 `json:"dy"`
	Button string  `json:"button"`
	Clicks int     `json:"clicks"`
	Key    string  `json:"key"`
	Text   string  `json:"text"`
}

func (s *Server) readLoop(ctx context.Context, ws *wsConn) {
	control := false
	warned := false
	for {
		op, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		_ = ws.SetReadDeadline(time.Now().Add(3 * s.opts.PingInterval))
		if op != wsText {
			continue
		}
		var ev inputEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			continue
		}
		if ev.T == "control" {
			control = ev.On && s.opts.AllowInput && s.in != nil
			ack, _ := json.Marshal(map[string]any{"t": "control", "on": control})
			_ = ws.WriteMessage(wsText, ack)
			continue
		}
		if !s.opts.AllowInput || s.in == nil || !control {
			if !warned {
				warned = true
				msg, _ := json.Marshal(map[string]string{"t": "error", "msg": "input forwarding is disabled"})
				_ = ws.WriteMessage(wsText, msg)
			}
			continue
		}
		if err := s.dispatchInput(ctx, ev); err != nil {
			msg, _ := json.Marshal(map[string]string{"t": "error", "msg": err.Error()})
			_ = ws.WriteMessage(wsText, msg)
		}
	}
}

var errBadInput = errors.New("invalid input event")

var allowedButtons = map[string]bool{"left": true, "middle": true, "right": true, "back": true, "forward": true, "none": true}

// dispatchInput validates ev and forwards it to Input.
func (s *Server) dispatchInput(ctx context.Context, ev inputEvent) error {
	finite := func(v ...float64) bool {
		for _, f := range v {
			if math.IsNaN(f) || math.IsInf(f, 0) {
				return false
			}
		}
		return true
	}
	if !finite(ev.X, ev.Y, ev.DX, ev.DY) {
		return errBadInput
	}
	// Clamp coordinates into the current frame.
	maxX, maxY := 1e5, 1e5
	if f := s.latest.Load(); f != nil && f.Width > 0 && f.Height > 0 {
		maxX, maxY = float64(f.Width-1), float64(f.Height-1)
	}
	x := math.Max(0, math.Min(ev.X, maxX))
	y := math.Max(0, math.Min(ev.Y, maxY))
	ictx, cancel := context.WithTimeout(ctx, s.opts.InputTimeout)
	defer cancel()
	switch ev.T {
	case "mousemove":
		return s.in.MouseMove(ictx, x, y)
	case "mousedown", "mouseup":
		btn := ev.Button
		if btn == "" {
			btn = "left"
		}
		if !allowedButtons[btn] || ev.Clicks < 0 || ev.Clicks > 3 {
			return errBadInput
		}
		if ev.T == "mousedown" {
			return s.in.MouseDown(ictx, x, y, btn, max(ev.Clicks, 1))
		}
		return s.in.MouseUp(ictx, x, y, btn, max(ev.Clicks, 1))
	case "wheel":
		dx := math.Max(-1e4, math.Min(ev.DX, 1e4))
		dy := math.Max(-1e4, math.Min(ev.DY, 1e4))
		return s.in.Wheel(ictx, x, y, dx, dy)
	case "keydown", "keyup":
		if ev.Key == "" || len(ev.Key) > 64 || !utf8.ValidString(ev.Key) {
			return errBadInput
		}
		if ev.T == "keydown" {
			return s.in.KeyDown(ictx, ev.Key)
		}
		return s.in.KeyUp(ictx, ev.Key)
	case "text":
		if ev.Text == "" || len(ev.Text) > 4096 || !utf8.ValidString(ev.Text) {
			return errBadInput
		}
		return s.in.InsertText(ictx, ev.Text)
	}
	return errBadInput
}
