package browserview

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Minimal RFC 6455 server implementation: handshake, framing, fragmentation,
// control frames and close handshake. No extensions are negotiated.

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// WebSocket opcodes.
const (
	wsContinuation = 0x0
	wsText         = 0x1
	wsBinary       = 0x2
	wsClose        = 0x8
	wsPing         = 0x9
	wsPong         = 0xA
)

// WebSocket close codes.
const (
	wsCloseNormal        = 1000
	wsCloseGoingAway     = 1001
	wsCloseProtocolError = 1002
	wsCloseInvalidData   = 1007
	wsCloseTooBig        = 1009
)

var errWSClosed = errors.New("websocket: closed")

// wsCloseError reports the peer's close frame.
type wsCloseError struct {
	Code   int
	Reason string
}

func (e *wsCloseError) Error() string {
	return fmt.Sprintf("websocket: closed by peer (%d %s)", e.Code, e.Reason)
}

// wsAccept computes Sec-WebSocket-Accept for a client key.
func wsAccept(key string) string {
	h := sha1.New()
	h.Write([]byte(key))
	h.Write([]byte(wsGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// wsConn is a server-side WebSocket connection. Reads must happen on one
// goroutine; writes are serialised internally and may come from any
// goroutine.
type wsConn struct {
	conn       net.Conn
	br         *bufio.Reader
	wmu        sync.Mutex
	maxMessage int
	closed     bool
	closeOnce  sync.Once
	// onPong is invoked (on the reader goroutine) for each pong.
	onPong func()
}

// upgradeWebSocket validates the opening handshake and hijacks the
// connection. On failure an HTTP error has already been written.
func upgradeWebSocket(w http.ResponseWriter, r *http.Request, maxMessage int) (*wsConn, error) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return nil, errors.New("websocket: method not GET")
	}
	if !headerHasToken(r.Header, "Connection", "upgrade") || !headerHasToken(r.Header, "Upgrade", "websocket") {
		http.Error(w, "websocket upgrade required", http.StatusUpgradeRequired)
		return nil, errors.New("websocket: missing upgrade headers")
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		http.Error(w, "unsupported websocket version", http.StatusUpgradeRequired)
		return nil, errors.New("websocket: unsupported version")
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if raw, err := base64.StdEncoding.DecodeString(key); err != nil || len(raw) != 16 {
		http.Error(w, "bad websocket key", http.StatusBadRequest)
		return nil, errors.New("websocket: bad key")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
		return nil, errors.New("websocket: response writer cannot hijack")
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return nil, fmt.Errorf("websocket: hijack: %w", err)
	}
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + wsAccept(key) + "\r\n\r\n"
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(conn, resp); err != nil {
		conn.Close()
		return nil, fmt.Errorf("websocket: handshake write: %w", err)
	}
	_ = conn.SetDeadline(time.Time{})
	if maxMessage <= 0 {
		maxMessage = 1 << 20
	}
	return &wsConn{conn: conn, br: brw.Reader, maxMessage: maxMessage}, nil
}

// writeFrame writes one unfragmented, unmasked frame.
func (c *wsConn) writeFrame(op byte, payload []byte, timeout time.Duration) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed {
		return errWSClosed
	}
	var hdr [10]byte
	hdr[0] = 0x80 | op
	n := len(payload)
	hl := 2
	switch {
	case n < 126:
		hdr[1] = byte(n)
	case n <= 0xFFFF:
		hdr[1] = 126
		binary.BigEndian.PutUint16(hdr[2:], uint16(n))
		hl = 4
	default:
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:], uint64(n))
		hl = 10
	}
	if timeout > 0 {
		_ = c.conn.SetWriteDeadline(time.Now().Add(timeout))
	}
	bufs := net.Buffers{hdr[:hl], payload}
	_, err := bufs.WriteTo(c.conn)
	if op == wsClose {
		c.closed = true
	}
	return err
}

// WriteMessage sends a complete text or binary message.
func (c *wsConn) WriteMessage(op byte, data []byte) error {
	return c.writeFrame(op, data, 10*time.Second)
}

// Ping sends a ping control frame.
func (c *wsConn) Ping() error { return c.writeFrame(wsPing, nil, 5*time.Second) }

// Close performs the closing handshake (best effort) and closes the socket.
func (c *wsConn) Close(code int, reason string) {
	c.closeOnce.Do(func() {
		if len(reason) > 123 {
			reason = reason[:123]
		}
		p := make([]byte, 2+len(reason))
		binary.BigEndian.PutUint16(p, uint16(code))
		copy(p[2:], reason)
		_ = c.writeFrame(wsClose, p, time.Second)
		_ = c.conn.Close()
	})
}

// SetReadDeadline bounds the next reads.
func (c *wsConn) SetReadDeadline(t time.Time) error { return c.conn.SetReadDeadline(t) }

// ReadMessage returns the next complete data message, transparently
// answering pings and handling the close handshake.
func (c *wsConn) ReadMessage() (op byte, data []byte, err error) {
	var msgOp byte
	var buf []byte
	inMsg := false
	for {
		fin, opcode, payload, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}
		switch opcode {
		case wsPing:
			if err := c.writeFrame(wsPong, payload, 5*time.Second); err != nil {
				return 0, nil, err
			}
			continue
		case wsPong:
			if c.onPong != nil {
				c.onPong()
			}
			continue
		case wsClose:
			code, reason := wsCloseNormal, ""
			if len(payload) >= 2 {
				code = int(binary.BigEndian.Uint16(payload))
				reason = string(payload[2:])
			} else if len(payload) == 1 {
				c.Close(wsCloseProtocolError, "bad close payload")
				return 0, nil, errors.New("websocket: bad close payload")
			}
			c.Close(wsCloseNormal, "")
			return 0, nil, &wsCloseError{Code: code, Reason: reason}
		case wsText, wsBinary:
			if inMsg {
				c.Close(wsCloseProtocolError, "expected continuation")
				return 0, nil, errors.New("websocket: new message inside fragmented message")
			}
			msgOp, buf, inMsg = opcode, append(buf[:0], payload...), true
		case wsContinuation:
			if !inMsg {
				c.Close(wsCloseProtocolError, "unexpected continuation")
				return 0, nil, errors.New("websocket: unexpected continuation frame")
			}
			if len(buf)+len(payload) > c.maxMessage {
				c.Close(wsCloseTooBig, "message too big")
				return 0, nil, errors.New("websocket: message too big")
			}
			buf = append(buf, payload...)
		default:
			c.Close(wsCloseProtocolError, "bad opcode")
			return 0, nil, fmt.Errorf("websocket: unknown opcode %#x", opcode)
		}
		if fin {
			if msgOp == wsText && !utf8.Valid(buf) {
				c.Close(wsCloseInvalidData, "invalid utf-8")
				return 0, nil, errors.New("websocket: invalid utf-8 in text message")
			}
			return msgOp, buf, nil
		}
	}
}

func (c *wsConn) readFrame() (fin bool, op byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(c.br, h[:]); err != nil {
		return
	}
	fin = h[0]&0x80 != 0
	if h[0]&0x70 != 0 {
		c.Close(wsCloseProtocolError, "reserved bits set")
		return false, 0, nil, errors.New("websocket: reserved bits set")
	}
	op = h[0] & 0x0F
	masked := h[1]&0x80 != 0
	if !masked {
		c.Close(wsCloseProtocolError, "client frames must be masked")
		return false, 0, nil, errors.New("websocket: unmasked client frame")
	}
	n := uint64(h[1] & 0x7F)
	switch n {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(ext[:])
	}
	if op >= 0x8 && (n > 125 || !fin) {
		c.Close(wsCloseProtocolError, "bad control frame")
		return false, 0, nil, errors.New("websocket: invalid control frame")
	}
	if n > uint64(c.maxMessage) {
		c.Close(wsCloseTooBig, "frame too big")
		return false, 0, nil, errors.New("websocket: frame too big")
	}
	var mask [4]byte
	if _, err = io.ReadFull(c.br, mask[:]); err != nil {
		return
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return
	}
	for i := range payload {
		payload[i] ^= mask[i&3]
	}
	return fin, op, payload, nil
}
