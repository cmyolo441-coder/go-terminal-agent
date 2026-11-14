package browser

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
)

// Errors reported by the package.
var (
	// ErrClosed is returned once the browser connection has shut down.
	ErrClosed = errors.New("browser: connection closed")
	// ErrPageClosed is returned by Page methods after the page was closed
	// or its target went away.
	ErrPageClosed = errors.New("browser: page closed")
	// ErrPageCrashed is returned by Page methods after the renderer crashed.
	ErrPageCrashed = errors.New("browser: page crashed")
	// ErrNotFound is returned by FindExecutable when no browser is found.
	ErrNotFound = errors.New("browser: no Chrome, Chromium or Edge executable found")
)

// ProtocolError is an error response to a CDP command.
type ProtocolError struct {
	Method  string
	Code    int
	Message string
	Data    string
}

func (e *ProtocolError) Error() string {
	s := fmt.Sprintf("cdp %s: %s (%d)", e.Method, e.Message, e.Code)
	if e.Data != "" {
		s += ": " + e.Data
	}
	return s
}

// cdpMessage is any message on the wire: a command, a response or an event.
type cdpMessage struct {
	ID        int64           `json:"id,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    string `json:"data,omitempty"`
	} `json:"error,omitempty"`
}

// eventKey selects events by flattened session ("" is the browser session)
// and method ("" matches every method of the session).
type eventKey struct {
	session string
	method  string
}

// eventHandler receives an event's params on the connection's reader
// goroutine. It must not block and must not wait for a command response.
// Handlers for one event run in the order they were subscribed.
type eventHandler func(method string, params json.RawMessage)

// cdpConn multiplexes CDP commands over one transport. Responses are
// matched to callers by id; events are dispatched to subscribers by
// session id and method. All methods are safe for concurrent use.
type cdpConn struct {
	t    transport
	logf func(string, ...any)

	nextID atomic.Int64

	mu      sync.Mutex
	pending map[int64]chan cdpMessage
	subs    map[eventKey]map[uint64]eventHandler
	subSeq  uint64
	err     error // terminal error, set once done is closed

	done       chan struct{} // closed when the connection is down
	readerDone chan struct{}
	closeOnce  sync.Once
}

func newCDPConn(t transport, logf func(string, ...any)) *cdpConn {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	c := &cdpConn{
		t:          t,
		logf:       logf,
		pending:    make(map[int64]chan cdpMessage),
		subs:       make(map[eventKey]map[uint64]eventHandler),
		done:       make(chan struct{}),
		readerDone: make(chan struct{}),
	}
	go c.readLoop()
	return c
}

// Done is closed when the connection has shut down.
func (c *cdpConn) Done() <-chan struct{} { return c.done }

// Err returns why the connection shut down, or nil while it is up.
func (c *cdpConn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *cdpConn) readLoop() {
	defer close(c.readerDone)
	for {
		data, err := c.t.ReadMessage()
		if err != nil {
			c.shutdown(fmt.Errorf("%w: %w", ErrClosed, err))
			return
		}
		var msg cdpMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			c.logf("browser: dropping malformed CDP message: %v", err)
			continue
		}
		if msg.ID != 0 {
			c.mu.Lock()
			ch, ok := c.pending[msg.ID]
			delete(c.pending, msg.ID)
			c.mu.Unlock()
			if ok {
				ch <- msg // buffered; never blocks
			}
			continue
		}
		if msg.Method != "" {
			c.dispatch(msg.SessionID, msg.Method, msg.Params)
		}
	}
}

func (c *cdpConn) dispatch(session, method string, params json.RawMessage) {
	type sub struct {
		id uint64
		h  eventHandler
	}
	var handlers []sub
	c.mu.Lock()
	for _, key := range [2]eventKey{{session, method}, {session, ""}} {
		for id, h := range c.subs[key] {
			handlers = append(handlers, sub{id, h})
		}
	}
	c.mu.Unlock()
	// Run handlers in subscription order, so state kept by long-lived
	// subscribers (a page's URL) is updated before later, short-lived
	// waiters observe the same event.
	slices.SortFunc(handlers, func(a, b sub) int { return cmp.Compare(a.id, b.id) })
	for _, s := range handlers {
		s.h(method, params)
	}
}

// subscribe registers h for events of method on session (method "" for
// all of the session's events) and returns an idempotent function that
// removes it. After the unsubscribe function returns, h is not called
// again except by a dispatch already in progress on the reader goroutine.
func (c *cdpConn) subscribe(session, method string, h eventHandler) (unsubscribe func()) {
	key := eventKey{session, method}
	c.mu.Lock()
	c.subSeq++
	id := c.subSeq
	m := c.subs[key]
	if m == nil {
		m = make(map[uint64]eventHandler)
		c.subs[key] = m
	}
	m[id] = h
	c.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			if m := c.subs[key]; m != nil {
				delete(m, id)
				if len(m) == 0 {
					delete(c.subs, key)
				}
			}
			c.mu.Unlock()
		})
	}
}

func (c *cdpConn) encode(id int64, session, method string, params any) ([]byte, error) {
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("cdp %s: encode params: %w", method, err)
		}
		raw = b
	}
	return json.Marshal(cdpMessage{ID: id, SessionID: session, Method: method, Params: raw})
}

// call sends a command and waits for its response, ctx, or shutdown. When
// result is non-nil the response's result object is decoded into it.
func (c *cdpConn) call(ctx context.Context, session, method string, params, result any) error {
	return c.callAbort(ctx, nil, nil, session, method, params, result)
}

// callAbort is call that also gives up with abortErr when abort is closed.
func (c *cdpConn) callAbort(ctx context.Context, abort <-chan struct{}, abortErr error, session, method string, params, result any) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("cdp %s: %w", method, err)
	}
	id := c.nextID.Add(1)
	data, err := c.encode(id, session, method, params)
	if err != nil {
		return err
	}
	ch := make(chan cdpMessage, 1)
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return fmt.Errorf("cdp %s: %w", method, err)
	}
	c.pending[id] = ch
	c.mu.Unlock()
	forget := func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}

	if err := c.t.WriteMessage(data); err != nil {
		forget()
		return fmt.Errorf("cdp %s: send: %w", method, err)
	}

	select {
	case msg := <-ch:
		if msg.Error != nil {
			return &ProtocolError{Method: method, Code: msg.Error.Code, Message: msg.Error.Message, Data: msg.Error.Data}
		}
		if result != nil && len(msg.Result) > 0 {
			if err := json.Unmarshal(msg.Result, result); err != nil {
				return fmt.Errorf("cdp %s: decode result: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		forget()
		return fmt.Errorf("cdp %s: %w", method, ctx.Err())
	case <-abort:
		forget()
		return fmt.Errorf("cdp %s: %w", method, abortErr)
	case <-c.done:
		return fmt.Errorf("cdp %s: %w", method, c.Err())
	}
}

// send writes a command without waiting for its response, which the
// reader discards. It suits acknowledgements whose result is empty.
func (c *cdpConn) send(session, method string, params any) error {
	select {
	case <-c.done:
		return fmt.Errorf("cdp %s: %w", method, c.Err())
	default:
	}
	data, err := c.encode(c.nextID.Add(1), session, method, params)
	if err != nil {
		return err
	}
	if err := c.t.WriteMessage(data); err != nil {
		return fmt.Errorf("cdp %s: send: %w", method, err)
	}
	return nil
}

func (c *cdpConn) shutdown(err error) {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return
	}
	c.err = err
	c.pending = make(map[int64]chan cdpMessage)
	c.mu.Unlock()
	close(c.done)
}

// Close shuts the transport down and waits for the reader to exit. Calls
// in flight fail with ErrClosed. It is idempotent.
func (c *cdpConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		c.shutdown(ErrClosed)
		err = c.t.Close()
		<-c.readerDone
	})
	return err
}
