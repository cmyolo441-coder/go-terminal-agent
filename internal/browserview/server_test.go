package browserview

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image/jpeg"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeInput struct {
	mu    sync.Mutex
	calls []string
	got   chan string
}

func newFakeInput() *fakeInput { return &fakeInput{got: make(chan string, 64)} }

func (f *fakeInput) rec(s string) error {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
	f.got <- s
	return nil
}

func (f *fakeInput) MouseMove(_ context.Context, x, y float64) error {
	return f.rec(fmt.Sprintf("move %g,%g", x, y))
}
func (f *fakeInput) MouseDown(_ context.Context, x, y float64, b string, n int) error {
	return f.rec(fmt.Sprintf("down %g,%g %s %d", x, y, b, n))
}
func (f *fakeInput) MouseUp(_ context.Context, x, y float64, b string, n int) error {
	return f.rec(fmt.Sprintf("up %g,%g %s %d", x, y, b, n))
}
func (f *fakeInput) Wheel(_ context.Context, x, y, dx, dy float64) error {
	return f.rec(fmt.Sprintf("wheel %g,%g %g,%g", x, y, dx, dy))
}
func (f *fakeInput) KeyDown(_ context.Context, k string) error { return f.rec("keydown " + k) }
func (f *fakeInput) KeyUp(_ context.Context, k string) error   { return f.rec("keyup " + k) }
func (f *fakeInput) InsertText(_ context.Context, s string) error {
	return f.rec("text " + s)
}

func (f *fakeInput) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func startServer(t *testing.T, src Source, in Input, opts ServerOptions) *Server {
	t.Helper()
	opts.Token = "secret-token"
	s, err := Serve(context.Background(), "127.0.0.1:0", src, in, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func get(t *testing.T, s *Server, path string, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", "http://"+s.Addr().String()+path, nil)
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestServeRejectsNonLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", "8.8.8.8:0", "example.com:0", "bad"} {
		if s, err := Serve(context.Background(), addr, newFakeSource(), nil, ServerOptions{}); err == nil {
			s.Close()
			t.Fatalf("%s accepted", addr)
		}
	}
	if _, err := Serve(context.Background(), "", nil, nil, ServerOptions{}); err == nil {
		t.Fatal("nil source accepted")
	}
}

func TestServerAuth(t *testing.T) {
	s := startServer(t, newFakeSource(), nil, ServerOptions{Title: "<gta>"})
	if !strings.Contains(s.URL(), "/?token=secret-token") || !strings.HasPrefix(s.URL(), "http://127.0.0.1:") {
		t.Fatalf("URL = %s", s.URL())
	}
	for _, tc := range []struct {
		path string
		hdr  map[string]string
		code int
	}{
		{"/", nil, 401},
		{"/?token=wrong", nil, 401},
		{"/?token=secret-tokenX", nil, 401},
		{"/ws", nil, 401},
		{"/mjpeg?token=nope", nil, 401},
		{"/?token=secret-token", map[string]string{"Host": "evil.example:80"}, 403},
		{"/?token=secret-token", map[string]string{"Origin": "http://evil.example"}, 403},
		{"/", map[string]string{"Authorization": "Bearer secret-token"}, 200},
		{"/frame?token=secret-token", nil, 503},
		{"/nope?token=secret-token", nil, 404},
	} {
		resp := get(t, s, tc.path, tc.hdr)
		if resp.StatusCode != tc.code {
			t.Errorf("%s %v: status %d, want %d", tc.path, tc.hdr, resp.StatusCode, tc.code)
		}
	}
	resp := get(t, s, "/?token=secret-token", nil)
	body, _ := io.ReadAll(resp.Body)
	csp := resp.Header.Get("Content-Security-Policy")
	if resp.StatusCode != 200 || !strings.Contains(csp, "script-src 'nonce-") || strings.Contains(string(body), "{{") {
		t.Fatalf("index: %d csp=%q", resp.StatusCode, csp)
	}
	nonce := strings.TrimSuffix(strings.SplitN(strings.SplitN(csp, "'nonce-", 2)[1], "'", 2)[0], "'")
	if !strings.Contains(string(body), `<script nonce="`+nonce+`">`) {
		t.Fatal("script nonce mismatch")
	}
	if !strings.Contains(string(body), "&lt;gta&gt;") || strings.Contains(string(body), "<gta>") {
		t.Fatal("title not escaped")
	}
	if resp.Header.Get("Referrer-Policy") != "no-referrer" || resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatal("security headers missing")
	}
}

// --- minimal WebSocket client --------------------------------------------

type wsClient struct {
	c  net.Conn
	br *bufio.Reader
}

func dialWS(t *testing.T, s *Server, path string, origin string) (*wsClient, *http.Response) {
	t.Helper()
	c, err := net.Dial("tcp", s.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	var k [16]byte
	_, _ = rand.Read(k[:])
	key := base64.StdEncoding.EncodeToString(k[:])
	req := "GET " + path + " HTTP/1.1\r\nHost: " + s.Addr().String() +
		"\r\nUpgrade: websocket\r\nConnection: keep-alive, Upgrade\r\nSec-WebSocket-Key: " + key +
		"\r\nSec-WebSocket-Version: 13\r\n"
	if origin != "" {
		req += "Origin: " + origin + "\r\n"
	}
	req += "\r\n"
	if _, err := io.WriteString(c, req); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode == http.StatusSwitchingProtocols && resp.Header.Get("Sec-WebSocket-Accept") != wsAccept(key) {
		t.Fatal("bad Sec-WebSocket-Accept")
	}
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	return &wsClient{c: c, br: br}, resp
}

func (w *wsClient) read(t *testing.T) (byte, []byte) {
	t.Helper()
	var h [2]byte
	if _, err := io.ReadFull(w.br, h[:]); err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if h[1]&0x80 != 0 {
		t.Fatal("server frames must not be masked")
	}
	n := uint64(h[1] & 0x7F)
	switch n {
	case 126:
		var e [2]byte
		_, _ = io.ReadFull(w.br, e[:])
		n = uint64(binary.BigEndian.Uint16(e[:]))
	case 127:
		var e [8]byte
		_, _ = io.ReadFull(w.br, e[:])
		n = binary.BigEndian.Uint64(e[:])
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(w.br, p); err != nil {
		t.Fatal(err)
	}
	return h[0] & 0x0F, p
}

// readUntil skips frames until one with op is found whose payload satisfies ok.
func (w *wsClient) readUntil(t *testing.T, op byte, ok func([]byte) bool) []byte {
	t.Helper()
	for i := 0; i < 50; i++ {
		o, p := w.read(t)
		if o == op && (ok == nil || ok(p)) {
			return p
		}
	}
	t.Fatalf("no matching frame with op %d", op)
	return nil
}

func (w *wsClient) write(t *testing.T, op byte, payload []byte, masked bool) {
	t.Helper()
	var b []byte
	b = append(b, 0x80|op)
	mbit := byte(0)
	if masked {
		mbit = 0x80
	}
	switch n := len(payload); {
	case n < 126:
		b = append(b, mbit|byte(n))
	case n <= 0xFFFF:
		b = append(b, mbit|126, byte(n>>8), byte(n))
	default:
		b = append(b, mbit|127)
		b = binary.BigEndian.AppendUint64(b, uint64(n))
	}
	if masked {
		mask := []byte{1, 2, 3, 4}
		b = append(b, mask...)
		for i, c := range payload {
			b = append(b, c^mask[i&3])
		}
	} else {
		b = append(b, payload...)
	}
	if _, err := w.c.Write(b); err != nil {
		t.Fatal(err)
	}
}

func (w *wsClient) sendJSON(t *testing.T, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	w.write(t, wsText, b, true)
}

func isType(typ string) func([]byte) bool {
	return func(p []byte) bool {
		var m map[string]any
		return json.Unmarshal(p, &m) == nil && m["t"] == typ
	}
}

func TestServerWebSocketFrames(t *testing.T) {
	src := newFakeSource()
	s := startServer(t, src, nil, ServerOptions{MaxFPS: 1000})

	// Origin from another site is refused before the upgrade.
	if _, resp := dialWS(t, s, "/ws?token=secret-token", "http://evil.example"); resp.StatusCode != 403 {
		t.Fatalf("cross-origin upgrade: %d", resp.StatusCode)
	}
	if _, resp := dialWS(t, s, "/ws", ""); resp.StatusCode != 401 {
		t.Fatalf("unauthenticated upgrade: %d", resp.StatusCode)
	}

	c, resp := dialWS(t, s, "/ws?token=secret-token", "http://"+s.Addr().String())
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status %d", resp.StatusCode)
	}
	hello := c.readUntil(t, wsText, isType("hello"))
	if !strings.Contains(string(hello), `"allowInput":false`) {
		t.Fatalf("hello = %s", hello)
	}

	f := testFrame(t, 7, red)
	src.ch <- f
	msg := c.readUntil(t, wsBinary, nil)
	if len(msg) != frameHeaderLen+len(f.Data) || msg[0] != 1 || msg[1] != 1 {
		t.Fatalf("bad frame header % x", msg[:4])
	}
	if w, h := binary.BigEndian.Uint32(msg[4:]), binary.BigEndian.Uint32(msg[8:]); w != 160 || h != 100 {
		t.Fatalf("size %dx%d", w, h)
	}
	if seq := binary.BigEndian.Uint64(msg[12:]); seq != 7 {
		t.Fatalf("seq %d", seq)
	}
	if ms := math.Float64frombits(binary.BigEndian.Uint64(msg[20:])); math.Abs(ms-float64(f.Timestamp.UnixNano())/1e6) > 1 {
		t.Fatalf("timestamp %f", ms)
	}
	if !bytes.Equal(msg[frameHeaderLen:], f.Data) {
		t.Fatal("payload mismatch")
	}

	s.SetOverlay(Overlay{URL: "https://example.com", Label: "typing"})
	ov := c.readUntil(t, wsText, isType("overlay"))
	if !strings.Contains(string(ov), `"label":"typing"`) {
		t.Fatalf("overlay = %s", ov)
	}

	// Ping from the client is answered with a pong carrying the payload.
	c.write(t, wsPing, []byte("hi"), true)
	if p := c.readUntil(t, wsPong, nil); string(p) != "hi" {
		t.Fatalf("pong payload %q", p)
	}
	if s.Clients() != 1 {
		t.Fatalf("clients = %d", s.Clients())
	}

	// A new viewer immediately receives the latest frame.
	c2, _ := dialWS(t, s, "/ws?token=secret-token", "")
	if m := c2.readUntil(t, wsBinary, nil); binary.BigEndian.Uint64(m[12:]) != 7 {
		t.Fatal("late joiner did not get latest frame")
	}

	// Close handshake.
	c.write(t, wsClose, []byte{0x03, 0xE8}, true)
	if p := c.readUntil(t, wsClose, nil); binary.BigEndian.Uint16(p) != wsCloseNormal {
		t.Fatalf("close code %v", p)
	}
}

func TestServerWebSocketProtocolErrors(t *testing.T) {
	s := startServer(t, newFakeSource(), nil, ServerOptions{})
	c, _ := dialWS(t, s, "/ws?token=secret-token", "")
	c.readUntil(t, wsText, isType("hello"))
	c.write(t, wsText, []byte(`{"t":"x"}`), false) // unmasked: protocol error
	if p := c.readUntil(t, wsClose, nil); binary.BigEndian.Uint16(p) != wsCloseProtocolError {
		t.Fatalf("close code %v", binary.BigEndian.Uint16(p))
	}

	c2, _ := dialWS(t, s, "/ws?token=secret-token", "")
	c2.readUntil(t, wsText, isType("hello"))
	c2.write(t, wsText, []byte{0xff, 0xfe}, true) // invalid UTF-8
	if p := c2.readUntil(t, wsClose, nil); binary.BigEndian.Uint16(p) != wsCloseInvalidData {
		t.Fatalf("close code %v", binary.BigEndian.Uint16(p))
	}

	// Wrong version is rejected with 426.
	req, _ := http.NewRequest("GET", "http://"+s.Addr().String()+"/ws?token=secret-token", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "8")
	req.Header.Set("Sec-WebSocket-Key", base64.StdEncoding.EncodeToString(make([]byte, 16)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUpgradeRequired || resp.Header.Get("Sec-WebSocket-Version") != "13" {
		t.Fatalf("version mismatch: %d", resp.StatusCode)
	}
}

func TestServerFragmentedMessage(t *testing.T) {
	in := newFakeInput()
	src := newFakeSource()
	s := startServer(t, src, in, ServerOptions{AllowInput: true})
	src.ch <- testFrame(t, 1, red)
	c, _ := dialWS(t, s, "/ws?token=secret-token", "")
	c.readUntil(t, wsText, isType("hello"))
	c.sendJSON(t, map[string]any{"t": "control", "on": true})
	c.readUntil(t, wsText, isType("control"))
	msg := []byte(`{"t":"text","text":"fragmented"}`)
	// First fragment (text, FIN=0), a ping in between, then continuation.
	mask := []byte{9, 8, 7, 6}
	frame := func(b0 byte, p []byte) []byte {
		out := []byte{b0, 0x80 | byte(len(p))}
		out = append(out, mask...)
		for i, ch := range p {
			out = append(out, ch^mask[i&3])
		}
		return out
	}
	var all []byte
	all = append(all, frame(wsText, msg[:10])...)
	all = append(all, frame(0x80|wsPing, nil)...)
	all = append(all, frame(0x80|wsContinuation, msg[10:])...)
	if _, err := c.c.Write(all); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-in.got:
		if got != "text fragmented" {
			t.Fatalf("got %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fragmented message not delivered")
	}
}

func TestServerMJPEGFallback(t *testing.T) {
	src := newFakeSource()
	s := startServer(t, src, nil, ServerOptions{MaxFPS: 1000})
	src.ch <- testFrame(t, 1, red) // PNG: transcoded to JPEG
	resp := get(t, s, "/mjpeg?token=secret-token", nil)
	mt, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/x-mixed-replace" {
		t.Fatalf("content type %q", resp.Header.Get("Content-Type"))
	}
	mr := multipart.NewReader(resp.Body, params["boundary"])
	part, err := mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if part.Header.Get("Content-Type") != "image/jpeg" || part.Header.Get("X-Frame-Seq") != "1" {
		t.Fatalf("part header %v", part.Header)
	}
	img, err := jpeg.Decode(part)
	if err != nil || img.Bounds().Dx() != 160 {
		t.Fatalf("decode part: %v", err)
	}
	jf := testFrame(t, 2, blue)
	jf.Data, jf.Format = encodeJPEG(t, gradient(32, 16)), "jpeg"
	src.ch <- jf
	part, err = mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(part)
	if part.Header.Get("X-Frame-Seq") != "2" || !bytes.Equal(data, jf.Data) {
		t.Fatal("JPEG frames must pass through untouched")
	}

	snap := get(t, s, "/frame?token=secret-token", nil)
	if snap.StatusCode != 200 || snap.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("snapshot %d %s", snap.StatusCode, snap.Header.Get("Content-Type"))
	}
	s.SetOverlay(Overlay{Label: "scroll"})
	ov := get(t, s, "/overlay?token=secret-token", nil)
	b, _ := io.ReadAll(ov.Body)
	if !strings.Contains(string(b), `"label":"scroll"`) {
		t.Fatalf("overlay endpoint %s", b)
	}
}

func TestServerInputDisabled(t *testing.T) {
	in := newFakeInput()
	src := newFakeSource()
	s := startServer(t, src, in, ServerOptions{AllowInput: false})
	c, _ := dialWS(t, s, "/ws?token=secret-token", "")
	c.readUntil(t, wsText, isType("hello"))
	c.sendJSON(t, map[string]any{"t": "control", "on": true})
	ack := c.readUntil(t, wsText, isType("control"))
	if !strings.Contains(string(ack), `"on":false`) {
		t.Fatalf("control must be refused: %s", ack)
	}
	c.sendJSON(t, map[string]any{"t": "mousedown", "x": 1, "y": 1, "button": "left", "clicks": 1})
	errMsg := c.readUntil(t, wsText, isType("error"))
	if !strings.Contains(string(errMsg), "disabled") {
		t.Fatalf("error = %s", errMsg)
	}
	time.Sleep(50 * time.Millisecond)
	if in.count() != 0 {
		t.Fatal("input forwarded although AllowInput is false")
	}
}

func TestServerInputForwarding(t *testing.T) {
	in := newFakeInput()
	src := newFakeSource()
	s := startServer(t, src, in, ServerOptions{AllowInput: true})
	src.ch <- testFrame(t, 1, red) // 160x100 bounds for clamping
	c, _ := dialWS(t, s, "/ws?token=secret-token", "")
	c.readUntil(t, wsText, isType("hello"))
	c.readUntil(t, wsBinary, nil)

	// Not yet in control: ignored.
	c.sendJSON(t, map[string]any{"t": "keydown", "key": "Enter"})
	c.readUntil(t, wsText, isType("error"))

	c.sendJSON(t, map[string]any{"t": "control", "on": true})
	if ack := c.readUntil(t, wsText, isType("control")); !strings.Contains(string(ack), `"on":true`) {
		t.Fatalf("ack = %s", ack)
	}
	c.sendJSON(t, map[string]any{"t": "mousedown", "x": 5000, "y": -3, "button": "right", "clicks": 2})
	c.sendJSON(t, map[string]any{"t": "mouseup", "x": 10.5, "y": 20, "button": "right", "clicks": 2})
	c.sendJSON(t, map[string]any{"t": "mousemove", "x": 1, "y": 2})
	c.sendJSON(t, map[string]any{"t": "wheel", "x": 3, "y": 4, "dx": 0, "dy": 120})
	c.sendJSON(t, map[string]any{"t": "keydown", "key": "Control+a"})
	c.sendJSON(t, map[string]any{"t": "keyup", "key": "Control+a"})
	c.sendJSON(t, map[string]any{"t": "text", "text": "héllo"})
	want := []string{
		"down 159,0 right 2", "up 10.5,20 right 2", "move 1,2", "wheel 3,4 0,120",
		"keydown Control+a", "keyup Control+a", "text héllo",
	}
	for _, w := range want {
		select {
		case got := <-in.got:
			if got != w {
				t.Fatalf("got %q, want %q", got, w)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout waiting for %q", w)
		}
	}
	// Invalid events are rejected, not forwarded.
	c.sendJSON(t, map[string]any{"t": "mousedown", "x": 1, "y": 1, "button": "laser"})
	if e := c.readUntil(t, wsText, isType("error")); !strings.Contains(string(e), "invalid") {
		t.Fatalf("error = %s", e)
	}
	c.sendJSON(t, map[string]any{"t": "text", "text": strings.Repeat("x", 5000)})
	c.readUntil(t, wsText, isType("error"))
	if in.count() != len(want) {
		t.Fatalf("forwarded %d events, want %d", in.count(), len(want))
	}
}

func TestServerCloseEndsClients(t *testing.T) {
	src := newFakeSource()
	s, err := Serve(context.Background(), "127.0.0.1:0", src, nil, ServerOptions{Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := dialWS(t, s, "/ws?token=t", "")
	c.readUntil(t, wsText, isType("hello"))
	done := make(chan error, 1)
	go func() { done <- s.Close() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung")
	}
	_ = c.c.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		var b [512]byte
		if _, err := c.br.Read(b[:]); err != nil {
			break
		}
	}
	if _, err := net.DialTimeout("tcp", s.Addr().String(), time.Second); err == nil {
		t.Fatal("listener still open")
	}
}

func TestServerSourceEnd(t *testing.T) {
	src := newFakeSource()
	s := startServer(t, src, nil, ServerOptions{})
	c, _ := dialWS(t, s, "/ws?token=secret-token", "")
	c.readUntil(t, wsText, isType("hello"))
	close(src.ch)
	c.readUntil(t, wsText, isType("end"))
}
