package lsp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// JSON-RPC and LSP error codes.
const (
	CodeParseError       = -32700
	CodeInvalidRequest   = -32600
	CodeMethodNotFound   = -32601
	CodeInvalidParams    = -32602
	CodeInternalError    = -32603
	CodeServerNotReady   = -32002
	CodeContentModified  = -32801
	CodeRequestCancelled = -32800
)

// DefaultMaxMessage bounds one incoming message.
const DefaultMaxMessage = 64 << 20

// ErrConnClosed is returned for calls on a closed connection.
var ErrConnClosed = errors.New("lsp: connection closed")

// ResponseError is a JSON-RPC error returned by the peer.
type ResponseError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *ResponseError) Error() string {
	return fmt.Sprintf("lsp: error %d: %s", e.Code, e.Message)
}

// IsMethodNotFound reports whether err is the peer saying it does not
// implement a method.
func IsMethodNotFound(err error) bool {
	var re *ResponseError
	return errors.As(err, &re) && re.Code == CodeMethodNotFound
}

// message is any incoming JSON-RPC message.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *ResponseError  `json:"error,omitempty"`
}

func (m *message) hasID() bool {
	id := bytes.TrimSpace(m.ID)
	return len(id) > 0 && !bytes.Equal(id, []byte("null"))
}

type outRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type outNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type outResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result"`
}

type outError struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   *ResponseError  `json:"error"`
}

// RequestHandler answers a request from the peer. Returning a
// *ResponseError sends that error; any other error becomes an internal
// error.
type RequestHandler func(ctx context.Context, method string, params json.RawMessage) (any, error)

// NotificationHandler receives notifications in arrival order on the read
// loop; it must not block.
type NotificationHandler func(method string, params json.RawMessage)

// ConnOptions configure a Conn.
type ConnOptions struct {
	OnRequest      RequestHandler
	OnNotification NotificationHandler
	// MaxMessage bounds incoming messages (default 64 MiB).
	MaxMessage int
	Logf       func(format string, args ...any)
}

// Conn is a JSON-RPC 2.0 connection with LSP base-protocol framing
// (Content-Length headers). It correlates responses with requests,
// dispatches notifications in order and answers peer requests
// concurrently.
type Conn struct {
	r    *bufio.Reader
	w    io.Writer
	c    io.Closer
	opts ConnOptions

	writeMu sync.Mutex
	nextID  atomic.Int64

	mu       sync.Mutex
	pending  map[int64]chan *message
	incoming map[string]context.CancelFunc // peer requests being handled
	closed   bool
	err      error

	done   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
}

// NewConn starts a connection reading from r and writing to w. closer, if
// not nil, is closed with the connection (typically the process pipes).
func NewConn(r io.Reader, w io.Writer, closer io.Closer, opts ConnOptions) *Conn {
	if opts.MaxMessage <= 0 {
		opts.MaxMessage = DefaultMaxMessage
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Conn{
		r:        bufio.NewReaderSize(r, 64<<10),
		w:        w,
		c:        closer,
		opts:     opts,
		pending:  map[int64]chan *message{},
		incoming: map[string]context.CancelFunc{},
		done:     make(chan struct{}),
		ctx:      ctx,
		cancel:   cancel,
	}
	go c.readLoop()
	return c
}

func (c *Conn) logf(format string, args ...any) {
	if c.opts.Logf != nil {
		c.opts.Logf(format, args...)
	}
}

// Done is closed when the connection ends.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err is the reason the connection ended, or nil while it is open.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Close ends the connection and fails every pending call.
func (c *Conn) Close() error {
	c.fail(ErrConnClosed)
	return nil
}

func (c *Conn) fail(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.err = err
	pending := c.pending
	c.pending = map[int64]chan *message{}
	c.mu.Unlock()
	c.cancel()
	if c.c != nil {
		c.c.Close()
	}
	for _, ch := range pending {
		close(ch)
	}
	close(c.done)
}

// write frames and sends one message.
func (c *Conn) write(v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	buf.Grow(len(body) + 32)
	fmt.Fprintf(&buf, "Content-Length: %d\r\n\r\n", len(body))
	buf.Write(body)
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	select {
	case <-c.done:
		return c.closedErr()
	default:
	}
	if _, err := c.w.Write(buf.Bytes()); err != nil {
		c.fail(fmt.Errorf("lsp: write: %w", err))
		return c.closedErr()
	}
	return nil
}

func (c *Conn) closedErr() error {
	if err := c.Err(); err != nil {
		if errors.Is(err, ErrConnClosed) {
			return err
		}
		return fmt.Errorf("%w: %v", ErrConnClosed, err)
	}
	return ErrConnClosed
}

// Notify sends a notification.
func (c *Conn) Notify(method string, params any) error {
	return c.write(outNotification{JSONRPC: "2.0", Method: method, Params: params})
}

// Call sends a request and decodes the result into result (which may be
// nil). Cancelling ctx sends $/cancelRequest to the peer.
func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
	id := c.nextID.Add(1)
	ch := make(chan *message, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return c.closedErr()
	}
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.write(outRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		c.forget(id)
		return err
	}
	select {
	case resp, ok := <-ch:
		if !ok {
			return c.closedErr()
		}
		if resp.Error != nil {
			return resp.Error
		}
		if result == nil {
			return nil
		}
		raw := bytes.TrimSpace(resp.Result)
		if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
			return nil
		}
		if err := json.Unmarshal(raw, result); err != nil {
			return fmt.Errorf("lsp: %s: decoding result: %w", method, err)
		}
		return nil
	case <-ctx.Done():
		c.forget(id)
		// Best effort: the peer may stop working on it.
		go c.Notify("$/cancelRequest", map[string]any{"id": id})
		return ctx.Err()
	case <-c.done:
		return c.closedErr()
	}
}

func (c *Conn) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// readLoop reads messages until the stream ends.
func (c *Conn) readLoop() {
	for {
		body, err := readFrame(c.r, c.opts.MaxMessage)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				c.fail(fmt.Errorf("lsp: server closed the connection"))
			} else {
				c.fail(fmt.Errorf("lsp: read: %w", err))
			}
			return
		}
		var m message
		if err := json.Unmarshal(body, &m); err != nil {
			c.logf("lsp: dropping malformed message: %v", err)
			continue
		}
		c.dispatch(&m)
	}
}

func (c *Conn) dispatch(m *message) {
	switch {
	case m.Method != "" && m.hasID():
		c.handleRequest(m)
	case m.Method != "":
		if m.Method == "$/cancelRequest" {
			var p struct {
				ID json.RawMessage `json:"id"`
			}
			if json.Unmarshal(m.Params, &p) == nil {
				c.mu.Lock()
				cancel := c.incoming[idKey(p.ID)]
				c.mu.Unlock()
				if cancel != nil {
					cancel()
				}
			}
			return
		}
		if c.opts.OnNotification != nil {
			c.opts.OnNotification(m.Method, m.Params)
		}
	case m.hasID():
		id, err := strconv.ParseInt(strings.Trim(string(bytes.TrimSpace(m.ID)), `"`), 10, 64)
		if err != nil {
			c.logf("lsp: response with unknown id %s", m.ID)
			return
		}
		c.mu.Lock()
		ch := c.pending[id]
		delete(c.pending, id)
		c.mu.Unlock()
		if ch != nil {
			ch <- m
		}
	default:
		c.logf("lsp: dropping message without method or id")
	}
}

func idKey(raw json.RawMessage) string { return string(bytes.TrimSpace(raw)) }

// handleRequest answers a peer request on its own goroutine so a slow
// handler never stalls the read loop.
func (c *Conn) handleRequest(m *message) {
	key := idKey(m.ID)
	ctx, cancel := context.WithCancel(c.ctx)
	c.mu.Lock()
	c.incoming[key] = cancel
	c.mu.Unlock()
	go func() {
		defer func() {
			cancel()
			c.mu.Lock()
			delete(c.incoming, key)
			c.mu.Unlock()
		}()
		var result any
		var err error
		if c.opts.OnRequest != nil {
			result, err = c.safeHandle(ctx, m.Method, m.Params)
		} else {
			err = &ResponseError{Code: CodeMethodNotFound, Message: "method not found: " + m.Method}
		}
		if err != nil {
			var re *ResponseError
			if !errors.As(err, &re) {
				re = &ResponseError{Code: CodeInternalError, Message: err.Error()}
			}
			if ctx.Err() != nil && c.ctx.Err() == nil {
				re = &ResponseError{Code: CodeRequestCancelled, Message: "request cancelled"}
			}
			_ = c.write(outError{JSONRPC: "2.0", ID: m.ID, Error: re})
			return
		}
		_ = c.write(outResponse{JSONRPC: "2.0", ID: m.ID, Result: result})
	}()
}

func (c *Conn) safeHandle(ctx context.Context, method string, params json.RawMessage) (result any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &ResponseError{Code: CodeInternalError, Message: fmt.Sprintf("handler panic: %v", r)}
		}
	}()
	return c.opts.OnRequest(ctx, method, params)
}

// readFrame reads one Content-Length framed message body.
func readFrame(r *bufio.Reader, max int) ([]byte, error) {
	length := -1
	sawHeader := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if sawHeader && errors.Is(err, io.EOF) {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		if len(line) > 8192 {
			return nil, fmt.Errorf("header line too long")
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if !sawHeader {
				continue // tolerate stray blank lines between messages
			}
			break
		}
		sawHeader = true
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("malformed header %q", line)
		}
		if strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil || n < 0 {
				return nil, fmt.Errorf("bad Content-Length %q", value)
			}
			length = n
		}
	}
	if length < 0 {
		return nil, fmt.Errorf("missing Content-Length header")
	}
	if length > max {
		return nil, fmt.Errorf("message of %d bytes exceeds the %d byte limit", length, max)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return body, nil
}
