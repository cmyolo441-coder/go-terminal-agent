package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// JSON-RPC 2.0 error codes used by the client.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

var (
	// ErrClosed is returned for requests on a connection that was closed by
	// the client.
	ErrClosed = errors.New("mcp: connection closed")
	// ErrTimeout wraps every request that ran past its deadline.
	ErrTimeout = errors.New("mcp: request timed out")
)

// RPCError is a JSON-RPC error object returned by a server.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	msg := fmt.Sprintf("mcp: server error %d: %s", e.Code, e.Message)
	if d := bytes.TrimSpace(e.Data); len(d) > 0 && !bytes.Equal(d, []byte("null")) {
		msg += " (" + clip(string(d), 300) + ")"
	}
	return msg
}

// wireMessage is any JSON-RPC 2.0 message: request, notification or response.
type wireMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

func (m *wireMessage) hasID() bool {
	id := bytes.TrimSpace(m.ID)
	return len(id) > 0 && !bytes.Equal(id, []byte("null"))
}

// idKey canonicalises a JSON-RPC id so that a numeric id matches however the
// server formats it, and a string id matches its unquoted text.
func idKey(raw json.RawMessage) (string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", false
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "", false
		}
		return s, true
	}
	var n json.Number
	if json.Unmarshal(raw, &n) != nil {
		return "", false
	}
	if i, err := n.Int64(); err == nil {
		return strconv.FormatInt(i, 10), true
	}
	if f, err := n.Float64(); err == nil {
		return strconv.FormatFloat(f, 'f', -1, 64), true
	}
	return n.String(), true
}

// splitBatch returns the messages in a body that may be a single JSON object
// or a JSON-RPC batch array.
func splitBatch(body []byte) ([]json.RawMessage, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, nil
	}
	if body[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, err
		}
		return batch, nil
	}
	if !json.Valid(body) {
		return nil, errors.New("invalid JSON")
	}
	return []json.RawMessage{body}, nil
}

// transport moves raw JSON-RPC messages to and from a server. Inbound
// messages are passed to the handler given at construction.
type transport interface {
	// start begins delivering inbound messages.
	start() error
	// send delivers one message. For transports where the response travels
	// on the same exchange (HTTP), send returns only once the response has
	// been handed to the handler or the exchange has failed.
	send(ctx context.Context, msg []byte) error
	// close shuts the transport down and releases its resources.
	close() error
	// done is closed when the transport can no longer carry messages.
	done() <-chan struct{}
	// err explains why done was closed.
	err() error
}

// conn is a JSON-RPC 2.0 client endpoint: it correlates responses with
// requests, answers server requests and fans out notifications.
type conn struct {
	t       transport
	timeout time.Duration
	logf    func(format string, args ...any)
	// onNotify receives server notifications. It runs on the transport's read
	// path and must not block.
	onNotify func(method string, params json.RawMessage)

	nextID atomic.Int64

	mu       sync.Mutex
	pending  map[string]chan *wireMessage
	closed   bool
	closeErr error
	// doneCh is closed once closed and closeErr are set.
	doneCh chan struct{}

	handlers sync.WaitGroup
	stop     chan struct{}
	stopOnce sync.Once
}

func newConn(timeout time.Duration, logf func(string, ...any)) *conn {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &conn{timeout: timeout, logf: logf, pending: map[string]chan *wireMessage{}, stop: make(chan struct{}), doneCh: make(chan struct{})}
}

// bind attaches the transport and starts watching for its failure.
func (c *conn) bind(t transport) {
	c.t = t
	go func() {
		select {
		case <-t.done():
			err := t.err()
			if err == nil {
				err = ErrClosed
			}
			c.fail(err)
		case <-c.stop:
		}
	}()
}

// fail marks the connection unusable and wakes every pending request.
func (c *conn) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	c.closeErr = err
	for k, ch := range c.pending {
		ch <- &wireMessage{Error: nil}
		delete(c.pending, k)
	}
	close(c.doneCh)
}

// done is closed when the connection stops; err then says why.
func (c *conn) done() <-chan struct{} { return c.doneCh }

// alive reports whether requests can still be sent.
func (c *conn) alive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.closed
}

// err returns why the connection stopped, or nil while it is alive.
func (c *conn) err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		return nil
	}
	return c.closeErr
}

// close shuts down the transport and fails pending requests with ErrClosed.
func (c *conn) close() error {
	c.fail(ErrClosed)
	var err error
	c.stopOnce.Do(func() {
		close(c.stop)
		if c.t != nil {
			err = c.t.close()
		}
	})
	waitTimeout(&c.handlers, 5*time.Second)
	return err
}

// call sends a request and decodes its result into out (which may be nil).
// timeout overrides the connection default when positive.
func (c *conn) call(ctx context.Context, method string, params any, out any, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = c.timeout
	}
	var timedOut error
	if timeout > 0 {
		timedOut = fmt.Errorf("%w: %s got no response within %s", ErrTimeout, method, timeout)
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, timeout, timedOut)
		defer cancel()
	}
	id := c.nextID.Add(1)
	key := strconv.FormatInt(id, 10)
	body, err := encodeMessage(&wireMessage{JSONRPC: "2.0", ID: json.RawMessage(key), Method: method}, params)
	if err != nil {
		return fmt.Errorf("mcp: encode %s: %w", method, err)
	}

	ch := make(chan *wireMessage, 1)
	c.mu.Lock()
	if c.closed {
		err := c.closeErr
		c.mu.Unlock()
		return err
	}
	c.pending[key] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
	}()

	abandon := func() error {
		// The initialize request must never be cancelled (MCP lifecycle).
		if method != "initialize" && c.alive() {
			c.cancelRemote(id, ctx.Err())
		}
		if cause := context.Cause(ctx); timedOut != nil && cause == timedOut {
			return timedOut
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("%w: %s: %w", ErrTimeout, method, ctx.Err())
		}
		return ctx.Err()
	}

	if err := c.t.send(ctx, body); err != nil {
		// A response may have raced in before the transport reported failure.
		select {
		case resp := <-ch:
			return c.result(method, resp, out)
		default:
		}
		if ctx.Err() != nil {
			return abandon()
		}
		if cerr := c.err(); cerr != nil {
			return cerr
		}
		return err
	}
	select {
	case resp := <-ch:
		return c.result(method, resp, out)
	case <-ctx.Done():
		select {
		case resp := <-ch:
			return c.result(method, resp, out)
		default:
		}
		return abandon()
	}
}

func (c *conn) result(method string, resp *wireMessage, out any) error {
	if resp.Error != nil {
		return resp.Error
	}
	if resp.Result == nil {
		// fail() wakes waiters with an empty message.
		if err := c.err(); err != nil {
			return err
		}
		return fmt.Errorf("mcp: %s: response has neither result nor error", method)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(resp.Result, out); err != nil {
		return fmt.Errorf("mcp: decode %s result: %w", method, err)
	}
	return nil
}

// notify sends a notification.
func (c *conn) notify(ctx context.Context, method string, params any) error {
	if !c.alive() {
		return c.err()
	}
	body, err := encodeMessage(&wireMessage{JSONRPC: "2.0", Method: method}, params)
	if err != nil {
		return fmt.Errorf("mcp: encode %s: %w", method, err)
	}
	return c.t.send(ctx, body)
}

// cancelRemote tells the server to stop working on request id. It runs in
// the background so the caller is not held up by a slow server.
func (c *conn) cancelRemote(id int64, cause error) {
	reason := "request cancelled by client"
	if errors.Is(cause, context.DeadlineExceeded) {
		reason = "request timed out"
	}
	c.spawn(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.notify(ctx, "notifications/cancelled", map[string]any{"requestId": id, "reason": reason}); err != nil {
			c.logf("send notifications/cancelled for request %d: %v", id, err)
		}
	})
}

// spawn runs f in a tracked goroutine unless the connection is closed, so
// close can wait for in-flight replies without racing new ones.
func (c *conn) spawn(f func()) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.handlers.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.handlers.Done()
		f()
	}()
}

// handle processes one inbound payload (a message or a batch of them).
func (c *conn) handle(raw []byte) {
	msgs, err := splitBatch(raw)
	if err != nil {
		c.logf("ignoring malformed message from server: %v", err)
		return
	}
	for _, m := range msgs {
		c.handleOne(m)
	}
}

func (c *conn) handleOne(raw json.RawMessage) {
	var m wireMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		c.logf("ignoring malformed message from server: %v", err)
		return
	}
	switch {
	case m.Method == "" && (m.Result != nil || m.Error != nil):
		key, ok := idKey(m.ID)
		if !ok {
			if m.Error != nil {
				c.logf("server reported an error without a request id: %s", m.Error.Message)
			}
			return
		}
		c.mu.Lock()
		ch, found := c.pending[key]
		if found {
			delete(c.pending, key)
		}
		c.mu.Unlock()
		if !found {
			c.logf("ignoring response to unknown or abandoned request %s", key)
			return
		}
		if m.Result == nil && m.Error == nil {
			m.Result = json.RawMessage("null")
		}
		ch <- &m
	case m.Method != "" && m.hasID():
		c.spawn(func() { c.answer(&m) })
	case m.Method != "":
		if c.onNotify != nil {
			c.onNotify(m.Method, m.Params)
		}
	default:
		c.logf("ignoring message that is neither request, response nor notification")
	}
}

// answer replies to a server-to-client request. The client declares no
// capabilities, so only ping is supported; everything else is answered with
// a method-not-found error as JSON-RPC requires.
func (c *conn) answer(req *wireMessage) {
	resp := &wireMessage{JSONRPC: "2.0", ID: req.ID}
	if req.Method == "ping" {
		resp.Result = json.RawMessage("{}")
	} else {
		resp.Error = &RPCError{Code: CodeMethodNotFound, Message: "Method not found: " + req.Method}
	}
	body, err := json.Marshal(resp)
	if err != nil {
		c.logf("encode reply to %s: %v", req.Method, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !c.alive() {
		return
	}
	if err := c.t.send(ctx, body); err != nil && c.alive() {
		c.logf("reply to server request %s: %v", req.Method, err)
	}
}

func encodeMessage(m *wireMessage, params any) ([]byte, error) {
	if params != nil {
		p, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		m.Params = p
	}
	return json.Marshal(m)
}

// waitTimeout waits for wg but gives up after d.
func waitTimeout(wg *sync.WaitGroup, d time.Duration) bool {
	ch := make(chan struct{})
	go func() {
		wg.Wait()
		close(ch)
	}()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ch:
		return true
	case <-t.C:
		return false
	}
}

// clip shortens s to at most n bytes, marking the cut.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && cut < len(s) && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}
