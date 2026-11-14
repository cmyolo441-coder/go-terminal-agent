package browser

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// wsTestServer is an in-process WebSocket server. Each accepted connection
// is handed to handler on its own goroutine as the server end of a wsConn.
type wsTestServer struct {
	*httptest.Server
	wg sync.WaitGroup
}

// upgradeWS performs the server side of the opening handshake on an HTTP
// request and returns the hijacked connection.
func upgradeWS(w http.ResponseWriter, r *http.Request, cfg wsConfig) (*wsConn, error) {
	if r.Method != http.MethodGet {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return nil, errors.New("not GET")
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || !headerHasToken(r.Header, "Connection", "upgrade") {
		http.Error(w, "not a websocket upgrade", http.StatusBadRequest)
		return nil, errors.New("not an upgrade")
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		http.Error(w, "version", http.StatusUpgradeRequired)
		return nil, errors.New("bad version")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "no key", http.StatusBadRequest)
		return nil, errors.New("no key")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("not hijackable")
	}
	nc, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + wsAcceptKey(key) + "\r\n\r\n"
	if _, err := nc.Write([]byte(resp)); err != nil {
		nc.Close()
		return nil, err
	}
	return newWSConn(nc, rw.Reader, true, cfg), nil
}

func newWSTestServer(t *testing.T, cfg wsConfig, handler func(c *wsConn)) *wsTestServer {
	t.Helper()
	s := &wsTestServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgradeWS(w, r, cfg)
		if err != nil {
			return
		}
		s.wg.Add(1)
		defer s.wg.Done()
		handler(c)
	}))
	t.Cleanup(func() {
		s.Server.CloseClientConnections()
		s.Server.Close()
		s.wg.Wait()
	})
	return s
}

func (s *wsTestServer) wsURL(path string) string {
	return "ws" + strings.TrimPrefix(s.URL, "http") + path
}

// echoHandler echoes every message until the connection ends.
func echoHandler(c *wsConn) {
	defer c.Close()
	for {
		op, data, err := c.ReadMessage()
		if err != nil {
			return
		}
		if err := c.WriteMessage(op, data); err != nil {
			return
		}
	}
}

func dialTest(t *testing.T, url string, cfg wsConfig) *wsConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := wsDial(ctx, url, cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestWSAcceptKeyRFCExample(t *testing.T) {
	// RFC 6455 section 1.3.
	if got := wsAcceptKey("dGhlIHNhbXBsZSBub25jZQ=="); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("accept = %q", got)
	}
}

func TestWSFrameEncodingRFCExamples(t *testing.T) {
	// RFC 6455 section 5.7: single-frame unmasked and masked "Hello".
	unmasked := appendFrameHeader(nil, true, wsOpText, 5, false, [4]byte{})
	unmasked = append(unmasked, "Hello"...)
	if want := []byte{0x81, 0x05, 0x48, 0x65, 0x6c, 0x6c, 0x6f}; !bytes.Equal(unmasked, want) {
		t.Fatalf("unmasked = % x, want % x", unmasked, want)
	}

	key := [4]byte{0x37, 0xfa, 0x21, 0x3d}
	var out bytes.Buffer
	client := newWSConn(&scriptConn{r: bytes.NewReader(nil), w: &out}, nil, false, wsConfig{})
	client.rand = bytes.NewReader(key[:])
	if err := client.WriteMessage(wsOpText, []byte("Hello")); err != nil {
		t.Fatal(err)
	}
	if want := []byte{0x81, 0x85, 0x37, 0xfa, 0x21, 0x3d, 0x7f, 0x9f, 0x4d, 0x51, 0x58}; !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("masked = % x, want % x", out.Bytes(), want)
	}

	// 256-byte binary message: 16-bit extended length.
	h := appendFrameHeader(nil, true, wsOpBinary, 256, false, [4]byte{})
	if want := []byte{0x82, 0x7e, 0x01, 0x00}; !bytes.Equal(h, want) {
		t.Fatalf("256 header = % x", h)
	}
	// 64 KiB binary message: 64-bit extended length.
	h = appendFrameHeader(nil, true, wsOpBinary, 65536, false, [4]byte{})
	if want := []byte{0x82, 0x7f, 0, 0, 0, 0, 0, 1, 0, 0}; !bytes.Equal(h, want) {
		t.Fatalf("65536 header = % x", h)
	}
}

func TestWSParseRFCFragmentedAndPing(t *testing.T) {
	// RFC 6455 section 5.7: fragmented unmasked "Hel" + "lo" with an
	// unmasked ping in the middle, read by the client side.
	stream := []byte{
		0x01, 0x03, 0x48, 0x65, 0x6c, // "Hel", not final
		0x89, 0x05, 0x48, 0x65, 0x6c, 0x6c, 0x6f, // ping "Hello"
		0x80, 0x02, 0x6c, 0x6f, // "lo", final
	}
	var pongs bytes.Buffer
	nc := &scriptConn{r: bytes.NewReader(stream), w: &pongs}
	c := newWSConn(nc, nil, false, wsConfig{})
	c.rand = bytes.NewReader(make([]byte, 64))
	op, data, err := c.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if op != wsOpText || string(data) != "Hello" {
		t.Fatalf("got op=%d %q", op, data)
	}
	// Pong with zero mask key echoes the ping payload.
	if want := []byte{0x8a, 0x85, 0, 0, 0, 0, 'H', 'e', 'l', 'l', 'o'}; !bytes.Equal(pongs.Bytes(), want) {
		t.Fatalf("pong = % x, want % x", pongs.Bytes(), want)
	}
}

func TestWSEchoSizes(t *testing.T) {
	srv := newWSTestServer(t, wsConfig{}, echoHandler)
	c := dialTest(t, srv.wsURL("/echo"), wsConfig{})
	sizes := []int{0, 1, 125, 126, 127, 65535, 65536, 70000, 3 << 20}
	for _, n := range sizes {
		msg := make([]byte, n)
		for i := range msg {
			msg[i] = byte(i * 7)
		}
		if err := c.WriteMessage(wsOpBinary, msg); err != nil {
			t.Fatalf("write %d: %v", n, err)
		}
		op, got, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("read %d: %v", n, err)
		}
		if op != wsOpBinary || !bytes.Equal(got, msg) {
			t.Fatalf("size %d: echo mismatch (op %d, len %d)", n, op, len(got))
		}
	}
	if err := c.WriteMessage(wsOpText, []byte("héllo wörld")); err != nil {
		t.Fatal(err)
	}
	if op, got, err := c.ReadMessage(); err != nil || op != wsOpText || string(got) != "héllo wörld" {
		t.Fatalf("text echo: op=%d %q %v", op, got, err)
	}
}

func TestWSClientFragmentation(t *testing.T) {
	frames := make(chan int, 64)
	srv := newWSTestServer(t, wsConfig{}, func(c *wsConn) {
		defer c.Close()
		op, data, err := c.ReadMessage()
		if err != nil {
			return
		}
		frames <- len(data)
		_ = c.WriteMessage(op, data)
	})
	c := dialTest(t, srv.wsURL("/"), wsConfig{WriteFragmentSize: 1000})
	msg := strings.Repeat("abcdefghij", 1050) // 10500 bytes -> 11 frames
	if err := c.WriteMessage(wsOpText, []byte(msg)); err != nil {
		t.Fatal(err)
	}
	_, got, err := c.ReadMessage()
	if err != nil || string(got) != msg {
		t.Fatalf("echo of fragmented message: %v (len %d)", err, len(got))
	}
	if n := <-frames; n != len(msg) {
		t.Fatalf("server reassembled %d bytes", n)
	}
}

func TestWSServerFragmentsWithInterleavedPing(t *testing.T) {
	gotPong := make(chan []byte, 1)
	srv := newWSTestServer(t, wsConfig{}, func(c *wsConn) {
		defer c.Close()
		c.wmu.Lock()
		_ = c.writeFrameLocked(false, wsOpText, []byte("frag-"))
		_ = c.writeFrameLocked(true, wsOpPing, []byte("are you there"))
		_ = c.writeFrameLocked(false, wsOpContinuation, []byte("ment"))
		_ = c.writeFrameLocked(false, wsOpContinuation, nil)
		_ = c.writeFrameLocked(true, wsOpContinuation, []byte("ed"))
		c.wmu.Unlock()
		// The client's pong arrives as a raw frame: read it by hand.
		fin, op, payload, err := readFrameRaw(c)
		if err == nil && fin && op == wsOpPong {
			gotPong <- payload
		}
		_, _, _ = c.ReadMessage()
	})
	c := dialTest(t, srv.wsURL("/"), wsConfig{})
	op, data, err := c.ReadMessage()
	if err != nil || op != wsOpText || string(data) != "frag-mented" {
		t.Fatalf("got op=%d %q %v", op, data, err)
	}
	select {
	case p := <-gotPong:
		if string(p) != "are you there" {
			t.Fatalf("pong payload %q", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no pong")
	}
}

func TestWSClientPing(t *testing.T) {
	srv := newWSTestServer(t, wsConfig{}, echoHandler)
	c := dialTest(t, srv.wsURL("/"), wsConfig{})
	if err := c.WritePing([]byte("p")); err != nil {
		t.Fatal(err)
	}
	if err := c.WritePing(make([]byte, 126)); err == nil {
		t.Fatal("oversized ping accepted")
	}
	// The server answers the ping with a pong, which ReadMessage skips.
	if err := c.WriteMessage(wsOpText, []byte("after ping")); err != nil {
		t.Fatal(err)
	}
	if _, got, err := c.ReadMessage(); err != nil || string(got) != "after ping" {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestWSCloseHandshakeClientInitiated(t *testing.T) {
	serverErr := make(chan error, 1)
	srv := newWSTestServer(t, wsConfig{}, func(c *wsConn) {
		_, _, err := c.ReadMessage()
		serverErr <- err
		c.Close()
	})
	c := dialTest(t, srv.wsURL("/"), wsConfig{})
	start := time.Now()
	if err := c.CloseWithStatus(wsCloseGoingAway, "bye"); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("close handshake took %v; peer close not observed", d)
	}
	var cerr *wsCloseError
	if err := <-serverErr; !errors.As(err, &cerr) || cerr.Code != wsCloseGoingAway || cerr.Reason != "bye" {
		t.Fatalf("server saw %v", err)
	}
	if err := c.WriteMessage(wsOpText, []byte("x")); !errors.Is(err, errWSClosed) {
		t.Fatalf("write after close: %v", err)
	}
	if _, _, err := c.ReadMessage(); err == nil {
		t.Fatal("read after close succeeded")
	}
	c.Close() // idempotent
}

func TestWSCloseWhileReaderActive(t *testing.T) {
	srv := newWSTestServer(t, wsConfig{}, echoHandler)
	c := dialTest(t, srv.wsURL("/"), wsConfig{})
	readErr := make(chan error, 1)
	go func() {
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				readErr <- err
				return
			}
		}
	}()
	time.Sleep(20 * time.Millisecond)
	start := time.Now()
	c.Close()
	if d := time.Since(start); d > time.Second {
		t.Fatalf("close took %v", d)
	}
	select {
	case err := <-readErr:
		if err == nil {
			t.Fatal("nil read error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reader did not end")
	}
}

func TestWSServerInitiatedClose(t *testing.T) {
	echoed := make(chan error, 1)
	srv := newWSTestServer(t, wsConfig{}, func(c *wsConn) {
		c.wmu.Lock()
		c.closeSent.Store(true)
		_ = c.writeFrameLocked(true, wsOpClose, closePayload(4001, "custom"))
		c.wmu.Unlock()
		// The client must echo the close frame.
		_, op, _, err := readFrameRaw(c)
		if err == nil && op != wsOpClose {
			err = fmt.Errorf("got opcode %d", op)
		}
		echoed <- err
		c.closeNetConn()
	})
	c := dialTest(t, srv.wsURL("/"), wsConfig{})
	_, _, err := c.ReadMessage()
	var cerr *wsCloseError
	if !errors.As(err, &cerr) || cerr.Code != 4001 || cerr.Reason != "custom" {
		t.Fatalf("read err = %v", err)
	}
	if err := <-echoed; err != nil {
		t.Fatalf("close echo: %v", err)
	}
	if _, _, err2 := c.ReadMessage(); !errors.As(err2, &cerr) {
		t.Fatalf("error not sticky: %v", err2)
	}
}

func TestWSProtocolViolations(t *testing.T) {
	cases := []struct {
		name  string
		raw   []byte
		code  int
		match string
	}{
		{"masked server frame", []byte{0x81, 0x81, 1, 2, 3, 4, 'x'}, wsCloseProtocolError, "masked"},
		{"reserved bits", []byte{0xC1, 0x01, 'x'}, wsCloseProtocolError, "reserved"},
		{"unknown opcode", []byte{0x83, 0x00}, wsCloseProtocolError, "opcode"},
		{"fragmented ping", []byte{0x09, 0x00}, wsCloseProtocolError, "fragmented control"},
		{"long control", append([]byte{0x89, 0x7e, 0x00, 0x7e}, make([]byte, 126)...), wsCloseProtocolError, "control frame payload"},
		{"orphan continuation", []byte{0x80, 0x01, 'x'}, wsCloseProtocolError, "continuation"},
		{"nested data frame", []byte{0x01, 0x01, 'a', 0x81, 0x01, 'b'}, wsCloseProtocolError, "in progress"},
		{"invalid utf8", []byte{0x81, 0x02, 0xc3, 0x28}, wsCloseInvalidPayload, "UTF-8"},
		{"64-bit msb", []byte{0x82, 0x7f, 0x80, 0, 0, 0, 0, 0, 0, 1}, wsCloseProtocolError, "most significant"},
		{"too big", []byte{0x82, 0x7f, 0, 0, 0, 0, 0, 0x11, 0, 0}, wsCloseMessageTooBig, "exceeds"},
		{"bad close code", []byte{0x88, 0x02, 0x03, 0xed}, wsCloseProtocolError, "invalid close status"},
		{"one byte close", []byte{0x88, 0x01, 0x03}, wsCloseProtocolError, "1-byte"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			nc := &scriptConn{r: bytes.NewReader(tc.raw), w: &out}
			c := newWSConn(nc, nil, false, wsConfig{MaxMessageSize: 1 << 20})
			_, _, err := c.ReadMessage()
			var perr *wsProtocolError
			if !errors.As(err, &perr) {
				t.Fatalf("err = %v, want protocol error", err)
			}
			if perr.Code != tc.code || !strings.Contains(perr.Msg, tc.match) {
				t.Fatalf("got code %d %q", perr.Code, perr.Msg)
			}
			// The client failed the connection with a masked close frame
			// carrying the status.
			b := out.Bytes()
			if len(b) < 8 || b[0] != 0x88 || b[1]&0x80 == 0 {
				t.Fatalf("no close frame sent: % x", b)
			}
			var key [4]byte
			copy(key[:], b[2:6])
			body := append([]byte(nil), b[6:]...)
			maskBytes(key, 0, body)
			if got := int(binary.BigEndian.Uint16(body)); got != tc.code {
				t.Fatalf("close status %d, want %d", got, tc.code)
			}
			if !nc.closed {
				t.Fatal("connection not closed")
			}
		})
	}
}

func TestWSDialRejectsBadAccept(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj := w.(http.Hijacker)
		nc, _, _ := hj.Hijack()
		defer nc.Close()
		nc.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: bm9wZQ==\r\n\r\n"))
	}))
	defer srv.Close()
	_, err := wsDial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), wsConfig{})
	if err == nil || !strings.Contains(err.Error(), "Sec-WebSocket-Accept") {
		t.Fatalf("err = %v", err)
	}
}

func TestWSDialRejectsNon101(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no websockets here", http.StatusForbidden)
	}))
	defer srv.Close()
	_, err := wsDial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), wsConfig{})
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "no websockets here") {
		t.Fatalf("err = %v", err)
	}
}

func TestWSDialSendsHeadersAndPath(t *testing.T) {
	got := make(chan *http.Request, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Clone(context.Background())
		c, err := upgradeWS(w, r, wsConfig{})
		if err == nil {
			c.Close()
		}
	}))
	defer srv.Close()
	h := http.Header{}
	h.Set("Origin", "http://example.test")
	h.Set("X-Evil", "a\r\nInjected: yes")
	c, err := wsDial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http")+"/devtools/browser/abc?x=1", wsConfig{Header: h})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	r := <-got
	if r.URL.RequestURI() != "/devtools/browser/abc?x=1" {
		t.Fatalf("path %q", r.URL.RequestURI())
	}
	if r.Header.Get("Origin") != "http://example.test" {
		t.Fatalf("origin %q", r.Header.Get("Origin"))
	}
	if r.Header.Get("Injected") != "" {
		t.Fatal("header injection")
	}
}

func TestWSDialContextCancel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var conns sync.WaitGroup
	conns.Add(1)
	go func() {
		defer conns.Done()
		nc, err := ln.Accept()
		if err != nil {
			return
		}
		// Never answer the handshake.
		io.Copy(io.Discard, nc)
		nc.Close()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = wsDial(ctx, "ws://"+ln.Addr().String()+"/", wsConfig{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("dial did not honor ctx")
	}
	ln.Close()
	conns.Wait()
}

func TestWSDialBadURL(t *testing.T) {
	for _, u := range []string{"http://x/", "ws://", "ws://u:p@host/", "::"} {
		if _, err := wsDial(context.Background(), u, wsConfig{}); err == nil {
			t.Errorf("%q: no error", u)
		}
	}
}

func TestWSCloseWithoutPeerTimesOut(t *testing.T) {
	// A peer that never answers the close frame: Close gives up after
	// CloseTimeout and still closes the connection.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		nc, err := ln.Accept()
		if err != nil {
			return
		}
		io.Copy(io.Discard, nc)
		nc.Close()
	}()
	nc, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c := newWSConn(nc, nil, false, wsConfig{CloseTimeout: 100 * time.Millisecond})
	start := time.Now()
	c.Close()
	if d := time.Since(start); d < 90*time.Millisecond || d > 2*time.Second {
		t.Fatalf("close took %v", d)
	}
	<-done
}

func TestCloseReasonTruncation(t *testing.T) {
	reason := strings.Repeat("é", 100) // 200 bytes
	p := closePayload(1000, reason)
	if len(p) > maxControlPayload {
		t.Fatalf("payload %d bytes", len(p))
	}
	if !strings.HasPrefix(reason, string(p[2:])) {
		t.Fatal("reason split a rune")
	}
}

// readFrameRaw reads one frame without control-frame handling, for tests
// that inspect what the client sent.
func readFrameRaw(c *wsConn) (fin bool, op byte, payload []byte, err error) {
	var hdr [2]byte
	if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
		return false, 0, nil, err
	}
	fin = hdr[0]&0x80 != 0
	op = hdr[0] & 0x0f
	n := int(hdr[1] & 0x7f)
	if n > 125 {
		return false, 0, nil, errors.New("raw reader handles small frames only")
	}
	var key [4]byte
	if hdr[1]&0x80 != 0 {
		if _, err := io.ReadFull(c.br, key[:]); err != nil {
			return false, 0, nil, err
		}
	}
	payload = make([]byte, n)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return false, 0, nil, err
	}
	maskBytes(key, 0, payload)
	return fin, op, payload, nil
}

// scriptConn is a net.Conn reading from a fixed script and recording writes.
type scriptConn struct {
	r      io.Reader
	w      io.Writer
	mu     sync.Mutex
	closed bool
}

func (s *scriptConn) Read(p []byte) (int, error) { return s.r.Read(p) }
func (s *scriptConn) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, net.ErrClosed
	}
	return s.w.Write(p)
}
func (s *scriptConn) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}
func (s *scriptConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (s *scriptConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (s *scriptConn) SetDeadline(time.Time) error      { return nil }
func (s *scriptConn) SetReadDeadline(time.Time) error  { return nil }
func (s *scriptConn) SetWriteDeadline(time.Time) error { return nil }
func (s *scriptConn) String() string                   { return "scriptConn" }
