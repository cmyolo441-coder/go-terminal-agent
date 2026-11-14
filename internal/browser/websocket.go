package browser

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// This file is a client implementation of RFC 6455 (The WebSocket
// Protocol), without extensions or subprotocols, which is all the DevTools
// endpoint speaks. The same connection type can run as the server end of a
// connection, which the package tests use to exercise the client.

// WebSocket opcodes (RFC 6455 section 5.2).
const (
	wsOpContinuation = 0x0
	wsOpText         = 0x1
	wsOpBinary       = 0x2
	wsOpClose        = 0x8
	wsOpPing         = 0x9
	wsOpPong         = 0xA
)

// WebSocket close status codes (RFC 6455 section 7.4.1).
const (
	wsCloseNormal          = 1000
	wsCloseGoingAway       = 1001
	wsCloseProtocolError   = 1002
	wsCloseUnsupportedData = 1003
	wsCloseNoStatus        = 1005
	wsCloseAbnormal        = 1006
	wsCloseInvalidPayload  = 1007
	wsClosePolicyViolation = 1008
	wsCloseMessageTooBig   = 1009
	wsCloseInternalError   = 1011
)

// wsGUID is the fixed string appended to the client key to derive
// Sec-WebSocket-Accept (RFC 6455 section 1.3).
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

const (
	defaultWSMaxMessage   = 256 << 20
	defaultWSCloseTimeout = 2 * time.Second
	defaultWSWriteTimeout = 30 * time.Second
	maxControlPayload     = 125
)

// errWSClosed is returned by operations on a connection after the close
// handshake started or the underlying connection failed.
var errWSClosed = errors.New("websocket: connection closed")

// wsCloseError reports the close frame the peer sent. Code is
// wsCloseNoStatus when the frame carried no status.
type wsCloseError struct {
	Code   int
	Reason string
}

func (e *wsCloseError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("websocket: closed by peer with status %d", e.Code)
	}
	return fmt.Sprintf("websocket: closed by peer with status %d: %s", e.Code, e.Reason)
}

// wsProtocolError is a violation of RFC 6455 by the peer. The connection is
// failed with Code after it is detected.
type wsProtocolError struct {
	Code int
	Msg  string
}

func (e *wsProtocolError) Error() string { return "websocket: " + e.Msg }

// wsConfig tunes a connection. The zero value is usable.
type wsConfig struct {
	// Header holds extra handshake request headers (for example Origin).
	Header http.Header
	// TLSConfig is used for wss:// URLs.
	TLSConfig *tls.Config
	// MaxMessageSize bounds a reassembled message; larger messages fail
	// the connection with status 1009. Zero means 256 MiB.
	MaxMessageSize int64
	// WriteFragmentSize splits outgoing data messages into continuation
	// frames of at most this many payload bytes. Zero sends one frame.
	WriteFragmentSize int
	// WriteTimeout bounds each frame write. A write that times out leaves
	// the stream mid-frame, so the connection is closed. Zero means 30s;
	// negative disables the deadline.
	WriteTimeout time.Duration
	// CloseTimeout is how long Close waits for the peer's close frame.
	// Zero means 2s.
	CloseTimeout time.Duration
}

// wsConn is one WebSocket connection. ReadMessage must be called from one
// goroutine at a time; WriteMessage and Close are safe for concurrent use.
type wsConn struct {
	nc     net.Conn
	br     *bufio.Reader
	server bool
	cfg    wsConfig
	rand   io.Reader // mask key source; crypto/rand unless a test overrides it

	rmu         sync.Mutex // serializes readers
	readErr     error      // sticky, guarded by rmu
	msgBuf      []byte     // reassembly buffer, guarded by rmu
	fragmenting bool       // a fragmented message is in progress, guarded by rmu

	wmu       sync.Mutex  // serializes frame writes
	closeSent atomic.Bool // written with wmu held
	writeErr  error       // sticky, guarded by wmu

	readDone     chan struct{} // closed once reading has ended for good
	readDoneOnce sync.Once
	closeOnce    sync.Once
	ncCloseOnce  sync.Once
}

func newWSConn(nc net.Conn, br *bufio.Reader, server bool, cfg wsConfig) *wsConn {
	if cfg.MaxMessageSize <= 0 {
		cfg.MaxMessageSize = defaultWSMaxMessage
	}
	if cfg.CloseTimeout <= 0 {
		cfg.CloseTimeout = defaultWSCloseTimeout
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = defaultWSWriteTimeout
	}
	if br == nil {
		br = bufio.NewReaderSize(nc, 64<<10)
	}
	return &wsConn{
		nc:       nc,
		br:       br,
		server:   server,
		cfg:      cfg,
		rand:     rand.Reader,
		readDone: make(chan struct{}),
	}
}

// wsAcceptKey derives Sec-WebSocket-Accept from Sec-WebSocket-Key.
func wsAcceptKey(key string) string {
	h := sha1.New()
	h.Write([]byte(key))
	h.Write([]byte(wsGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// wsDial opens a client connection to a ws:// or wss:// URL. ctx bounds the
// TCP connect, TLS and opening handshake; it has no effect afterwards.
func wsDial(ctx context.Context, rawURL string, cfg wsConfig) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("websocket: parse url: %w", err)
	}
	var secure bool
	switch strings.ToLower(u.Scheme) {
	case "ws":
	case "wss":
		secure = true
	default:
		return nil, fmt.Errorf("websocket: unsupported scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("websocket: url has no host")
	}
	if u.User != nil {
		return nil, errors.New("websocket: credentials in url are not supported")
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "80"
		if secure {
			port = "443"
		}
	}
	addr := net.JoinHostPort(host, port)

	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("websocket: dial %s: %w", addr, err)
	}
	// Abort any blocking handshake I/O when ctx ends.
	if dl, ok := ctx.Deadline(); ok {
		_ = nc.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { _ = nc.SetDeadline(time.Unix(1, 0)) })
	fail := func(err error) (*wsConn, error) {
		stop()
		_ = nc.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("websocket: handshake: %w", ctxErr)
		}
		// The connection deadline mirrors ctx's and can fire a moment
		// before ctx reports it.
		if dl, ok := ctx.Deadline(); ok && !time.Now().Before(dl) {
			return nil, fmt.Errorf("websocket: handshake: %w", context.DeadlineExceeded)
		}
		return nil, err
	}

	if secure {
		tc := cfg.TLSConfig.Clone()
		if tc == nil {
			tc = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		if tc.ServerName == "" {
			tc.ServerName = host
		}
		tlsConn := tls.Client(nc, tc)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return fail(fmt.Errorf("websocket: tls handshake: %w", err))
		}
		nc = tlsConn
	}

	var keyBytes [16]byte
	if _, err := io.ReadFull(rand.Reader, keyBytes[:]); err != nil {
		return fail(fmt.Errorf("websocket: generate key: %w", err))
	}
	key := base64.StdEncoding.EncodeToString(keyBytes[:])

	reqURI := u.RequestURI()
	var req bytes.Buffer
	fmt.Fprintf(&req, "GET %s HTTP/1.1\r\n", reqURI)
	fmt.Fprintf(&req, "Host: %s\r\n", u.Host)
	req.WriteString("Upgrade: websocket\r\n")
	req.WriteString("Connection: Upgrade\r\n")
	fmt.Fprintf(&req, "Sec-WebSocket-Key: %s\r\n", key)
	req.WriteString("Sec-WebSocket-Version: 13\r\n")
	if cfg.Header != nil {
		// WriteSubset strips CR/LF from values, so caller headers cannot
		// smuggle extra lines into the request.
		reserved := map[string]bool{
			"Host": true, "Upgrade": true, "Connection": true,
			"Sec-Websocket-Key": true, "Sec-Websocket-Version": true,
			"Sec-Websocket-Extensions": true,
		}
		if err := cfg.Header.WriteSubset(&req, reserved); err != nil {
			return fail(fmt.Errorf("websocket: write headers: %w", err))
		}
	}
	req.WriteString("\r\n")
	if _, err := nc.Write(req.Bytes()); err != nil {
		return fail(fmt.Errorf("websocket: send handshake: %w", err))
	}

	br := bufio.NewReaderSize(nc, 64<<10)
	httpReq := &http.Request{Method: http.MethodGet, URL: u}
	resp, err := http.ReadResponse(br, httpReq)
	if err != nil {
		return fail(fmt.Errorf("websocket: read handshake response: %w", err))
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		msg := strings.TrimSpace(string(body))
		if msg != "" {
			return fail(fmt.Errorf("websocket: handshake failed: %s: %s", resp.Status, msg))
		}
		return fail(fmt.Errorf("websocket: handshake failed: %s", resp.Status))
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") {
		return fail(errors.New("websocket: handshake response lacks Upgrade: websocket"))
	}
	if !headerHasToken(resp.Header, "Connection", "upgrade") {
		return fail(errors.New("websocket: handshake response lacks Connection: Upgrade"))
	}
	if got, want := resp.Header.Get("Sec-WebSocket-Accept"), wsAcceptKey(key); got != want {
		return fail(fmt.Errorf("websocket: bad Sec-WebSocket-Accept %q (want %q)", got, want))
	}
	if ext := resp.Header.Get("Sec-WebSocket-Extensions"); ext != "" {
		return fail(fmt.Errorf("websocket: server selected unrequested extension %q", ext))
	}
	if proto := resp.Header.Get("Sec-WebSocket-Protocol"); proto != "" {
		return fail(fmt.Errorf("websocket: server selected unrequested subprotocol %q", proto))
	}

	if !stop() {
		// ctx ended while the handshake was finishing.
		_ = nc.Close()
		return nil, fmt.Errorf("websocket: handshake: %w", ctx.Err())
	}
	_ = nc.SetDeadline(time.Time{})
	return newWSConn(nc, br, false, cfg), nil
}

// headerHasToken reports whether a comma-separated header contains token,
// compared case-insensitively.
func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for part := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// maskBytes XORs b with the mask key starting at key offset pos and returns
// the offset for the next byte.
func maskBytes(key [4]byte, pos int, b []byte) int {
	for i := range b {
		b[i] ^= key[pos&3]
		pos++
	}
	return pos & 3
}

// appendFrameHeader appends a frame header for a payload of n bytes.
func appendFrameHeader(dst []byte, fin bool, op byte, n int, masked bool, key [4]byte) []byte {
	b0 := op & 0x0f
	if fin {
		b0 |= 0x80
	}
	var maskBit byte
	if masked {
		maskBit = 0x80
	}
	switch {
	case n <= 125:
		dst = append(dst, b0, maskBit|byte(n))
	case n <= 0xffff:
		dst = append(dst, b0, maskBit|126)
		dst = binary.BigEndian.AppendUint16(dst, uint16(n))
	default:
		dst = append(dst, b0, maskBit|127)
		dst = binary.BigEndian.AppendUint64(dst, uint64(n))
	}
	if masked {
		dst = append(dst, key[:]...)
	}
	return dst
}

// writeFrameLocked writes one frame. wmu must be held.
func (c *wsConn) writeFrameLocked(fin bool, op byte, payload []byte) error {
	if c.writeErr != nil {
		return c.writeErr
	}
	masked := !c.server
	var key [4]byte
	if masked {
		if _, err := io.ReadFull(c.rand, key[:]); err != nil {
			return fmt.Errorf("websocket: mask key: %w", err)
		}
	}
	buf := make([]byte, 0, 14+len(payload))
	buf = appendFrameHeader(buf, fin, op, len(payload), masked, key)
	start := len(buf)
	buf = append(buf, payload...)
	if masked {
		maskBytes(key, 0, buf[start:])
	}
	if c.cfg.WriteTimeout > 0 {
		_ = c.nc.SetWriteDeadline(time.Now().Add(c.cfg.WriteTimeout))
	}
	if _, err := c.nc.Write(buf); err != nil {
		// A partial frame leaves the stream unusable.
		c.writeErr = fmt.Errorf("websocket: write: %w", err)
		c.closeNetConn()
		return c.writeErr
	}
	return nil
}

// WriteMessage sends a text or binary message, fragmenting it when
// WriteFragmentSize is set. Text payloads must be valid UTF-8.
func (c *wsConn) WriteMessage(op byte, data []byte) error {
	if op != wsOpText && op != wsOpBinary {
		return fmt.Errorf("websocket: WriteMessage with control or invalid opcode %#x", op)
	}
	if op == wsOpText && !utf8.Valid(data) {
		return errors.New("websocket: text message is not valid UTF-8")
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closeSent.Load() {
		return errWSClosed
	}
	frag := c.cfg.WriteFragmentSize
	if frag <= 0 || len(data) <= frag {
		return c.writeFrameLocked(true, op, data)
	}
	frameOp := op
	for len(data) > 0 {
		n := min(frag, len(data))
		if err := c.writeFrameLocked(n == len(data), frameOp, data[:n]); err != nil {
			return err
		}
		data = data[n:]
		frameOp = wsOpContinuation
	}
	return nil
}

// WritePing sends a ping with an application payload of at most 125 bytes.
func (c *wsConn) WritePing(payload []byte) error {
	return c.writeControl(wsOpPing, payload)
}

func (c *wsConn) writeControl(op byte, payload []byte) error {
	if len(payload) > maxControlPayload {
		return errors.New("websocket: control frame payload exceeds 125 bytes")
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closeSent.Load() {
		return errWSClosed
	}
	return c.writeFrameLocked(true, op, payload)
}

// closePayload builds a close frame body, truncating the reason so the
// payload fits a control frame without splitting a UTF-8 sequence.
func closePayload(code int, reason string) []byte {
	if code == wsCloseNoStatus {
		return nil
	}
	if len(reason) > maxControlPayload-2 {
		reason = reason[:maxControlPayload-2]
		for !utf8.ValidString(reason) {
			reason = reason[:len(reason)-1]
		}
	}
	p := binary.BigEndian.AppendUint16(make([]byte, 0, 2+len(reason)), uint16(code))
	return append(p, reason...)
}

// sendClose writes a close frame unless one was already sent. It reports
// whether this call sent it.
func (c *wsConn) sendClose(code int, reason string) bool {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closeSent.Load() {
		return false
	}
	c.closeSent.Store(true)
	_ = c.writeFrameLocked(true, wsOpClose, closePayload(code, reason))
	return true
}

func (c *wsConn) closeNetConn() {
	c.ncCloseOnce.Do(func() { _ = c.nc.Close() })
}

func (c *wsConn) markReadDone() {
	c.readDoneOnce.Do(func() { close(c.readDone) })
}

// Close runs the closing handshake with status 1000: it sends a close
// frame, waits up to CloseTimeout for the peer's close frame (reading it
// itself when no ReadMessage call is in progress), then closes the
// network connection. It is idempotent.
func (c *wsConn) Close() error {
	return c.CloseWithStatus(wsCloseNormal, "")
}

// CloseWithStatus is Close with an explicit status code and reason.
func (c *wsConn) CloseWithStatus(code int, reason string) error {
	c.closeOnce.Do(func() {
		c.sendClose(code, reason)
		timer := time.NewTimer(c.cfg.CloseTimeout)
		defer timer.Stop()
		if c.rmu.TryLock() {
			// Nobody is reading: drain frames ourselves until the peer's
			// close frame arrives or the timeout passes.
			_ = c.nc.SetReadDeadline(time.Now().Add(c.cfg.CloseTimeout))
			for c.readErr == nil {
				c.readErr = c.readOneLocked()
			}
			c.markReadDone()
			c.rmu.Unlock()
		} else {
			select {
			case <-c.readDone:
			case <-timer.C:
			}
		}
		c.closeNetConn()
		c.markReadDone()
	})
	return nil
}

// ReadMessage returns the next complete text or binary message. Pings are
// answered and pongs discarded transparently. When the peer closes, the
// close is echoed and a *wsCloseError is returned; every later call returns
// the same error.
func (c *wsConn) ReadMessage() (op byte, data []byte, err error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	if c.readErr != nil {
		return 0, nil, c.readErr
	}
	c.msgBuf = c.msgBuf[:0]
	var msgOp byte
	for {
		fin, frameOp, err := c.readFrameLocked()
		if err != nil {
			c.setReadErr(err)
			return 0, nil, err
		}
		switch frameOp {
		case wsOpText, wsOpBinary:
			msgOp = frameOp
		case wsOpContinuation:
		default:
			continue // control frame already handled
		}
		if !fin {
			continue
		}
		if msgOp == wsOpText && !utf8.Valid(c.msgBuf) {
			err := &wsProtocolError{Code: wsCloseInvalidPayload, Msg: "text message is not valid UTF-8"}
			c.failLocked(err)
			return 0, nil, err
		}
		out := make([]byte, len(c.msgBuf))
		copy(out, c.msgBuf)
		if cap(c.msgBuf) > 1<<20 {
			c.msgBuf = nil // do not pin a huge buffer between messages
		}
		return msgOp, out, nil
	}
}

func (c *wsConn) setReadErr(err error) {
	c.readErr = err
	c.markReadDone()
}

// readOneLocked reads and handles a single frame, discarding data; it is
// used while draining during Close. It returns a non-nil error once
// reading has ended.
func (c *wsConn) readOneLocked() error {
	fin, op, err := c.readFrameLocked()
	if err == nil && fin && op&0x8 == 0 {
		c.msgBuf = c.msgBuf[:0]
	}
	return err
}

// failLocked fails the connection after a protocol violation by the peer:
// send a close frame with the violation's status and drop the connection.
func (c *wsConn) failLocked(perr *wsProtocolError) {
	c.sendClose(perr.Code, perr.Msg)
	c.closeNetConn()
	c.setReadErr(perr)
}

// readFrameLocked reads one frame. Data frame payloads are appended to
// msgBuf; control frames are handled here. rmu must be held.
func (c *wsConn) readFrameLocked() (fin bool, op byte, err error) {
	protoErr := func(code int, format string, args ...any) (bool, byte, error) {
		perr := &wsProtocolError{Code: code, Msg: fmt.Sprintf(format, args...)}
		c.failLocked(perr)
		return false, 0, perr
	}

	var hdr [8]byte
	if _, err := io.ReadFull(c.br, hdr[:2]); err != nil {
		return false, 0, c.readFailure(err)
	}
	fin = hdr[0]&0x80 != 0
	if hdr[0]&0x70 != 0 {
		return protoErr(wsCloseProtocolError, "reserved bits set without a negotiated extension")
	}
	op = hdr[0] & 0x0f
	masked := hdr[1]&0x80 != 0
	length := uint64(hdr[1] & 0x7f)
	switch length {
	case 126:
		if _, err := io.ReadFull(c.br, hdr[:2]); err != nil {
			return false, 0, c.readFailure(err)
		}
		length = uint64(binary.BigEndian.Uint16(hdr[:2]))
	case 127:
		if _, err := io.ReadFull(c.br, hdr[:8]); err != nil {
			return false, 0, c.readFailure(err)
		}
		length = binary.BigEndian.Uint64(hdr[:8])
		if length&(1<<63) != 0 {
			return protoErr(wsCloseProtocolError, "64-bit payload length has its most significant bit set")
		}
	}
	if c.server && !masked {
		return protoErr(wsCloseProtocolError, "client frame is not masked")
	}
	if !c.server && masked {
		return protoErr(wsCloseProtocolError, "server frame is masked")
	}
	var key [4]byte
	if masked {
		if _, err := io.ReadFull(c.br, key[:]); err != nil {
			return false, 0, c.readFailure(err)
		}
	}

	isControl := op&0x8 != 0
	if isControl {
		if !fin {
			return protoErr(wsCloseProtocolError, "fragmented control frame")
		}
		if length > maxControlPayload {
			return protoErr(wsCloseProtocolError, "control frame payload of %d bytes", length)
		}
		var buf [maxControlPayload]byte
		payload := buf[:length]
		if _, err := io.ReadFull(c.br, payload); err != nil {
			return false, 0, c.readFailure(err)
		}
		if masked {
			maskBytes(key, 0, payload)
		}
		switch op {
		case wsOpPing:
			if err := c.writeControl(wsOpPong, payload); err != nil && !errors.Is(err, errWSClosed) {
				return false, 0, err
			}
		case wsOpPong:
		case wsOpClose:
			return false, 0, c.handleClose(payload)
		default:
			return protoErr(wsCloseProtocolError, "unknown control opcode %#x", op)
		}
		return fin, op, nil
	}

	switch op {
	case wsOpText, wsOpBinary:
		if c.fragmenting {
			return protoErr(wsCloseProtocolError, "new data frame while a fragmented message is in progress")
		}
	case wsOpContinuation:
		if !c.fragmenting {
			return protoErr(wsCloseProtocolError, "continuation frame without a message in progress")
		}
	default:
		return protoErr(wsCloseProtocolError, "unknown data opcode %#x", op)
	}
	if uint64(len(c.msgBuf))+length > uint64(c.cfg.MaxMessageSize) {
		return protoErr(wsCloseMessageTooBig, "message exceeds %d bytes", c.cfg.MaxMessageSize)
	}
	start := len(c.msgBuf)
	c.msgBuf = slices.Grow(c.msgBuf, int(length))[:start+int(length)]
	if _, err := io.ReadFull(c.br, c.msgBuf[start:]); err != nil {
		return false, 0, c.readFailure(err)
	}
	if masked {
		maskBytes(key, 0, c.msgBuf[start:])
	}
	c.fragmenting = !fin
	return fin, op, nil
}

// handleClose processes a received close frame: validate it, echo it when
// we have not sent one, close the network connection and return the
// resulting error.
func (c *wsConn) handleClose(payload []byte) error {
	code := wsCloseNoStatus
	reason := ""
	switch {
	case len(payload) == 1:
		perr := &wsProtocolError{Code: wsCloseProtocolError, Msg: "close frame with a 1-byte payload"}
		c.failLocked(perr)
		return perr
	case len(payload) >= 2:
		code = int(binary.BigEndian.Uint16(payload[:2]))
		reason = string(payload[2:])
		if !validCloseCode(code) {
			perr := &wsProtocolError{Code: wsCloseProtocolError, Msg: fmt.Sprintf("invalid close status %d", code)}
			c.failLocked(perr)
			return perr
		}
		if !utf8.ValidString(reason) {
			perr := &wsProtocolError{Code: wsCloseInvalidPayload, Msg: "close reason is not valid UTF-8"}
			c.failLocked(perr)
			return perr
		}
	}
	echo := code
	if echo == wsCloseNoStatus {
		echo = wsCloseNormal
	}
	c.sendClose(echo, "")
	c.closeNetConn()
	cerr := &wsCloseError{Code: code, Reason: reason}
	c.setReadErr(cerr)
	return cerr
}

// validCloseCode reports whether a peer may send code in a close frame.
func validCloseCode(code int) bool {
	switch {
	case code >= 1000 && code <= 1003:
		return true
	case code >= 1007 && code <= 1014:
		return true
	case code >= 3000 && code <= 4999:
		return true
	}
	return false
}

// readFailure converts an I/O error into the connection's terminal read
// error. After our own close frame went out, EOF or a closed connection is
// the expected end of the handshake.
func (c *wsConn) readFailure(err error) error {
	if c.closeSent.Load() {
		return errWSClosed
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		err = io.EOF
	}
	return fmt.Errorf("websocket: read: %w", err)
}
