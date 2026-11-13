package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrSessionExpired is returned when an HTTP server no longer recognises the
// session; the client re-initializes and retries once.
var ErrSessionExpired = errors.New("mcp: session expired")

const (
	headerSessionID       = "Mcp-Session-Id"
	headerProtocolVersion = "MCP-Protocol-Version"
	headerLastEventID     = "Last-Event-ID"
	acceptPost            = "application/json, text/event-stream"
	// maxResumeAttempts bounds reconnects to an SSE stream that ended before
	// it carried the response.
	maxResumeAttempts = 3
	listenMinBackoff  = time.Second
	listenMaxBackoff  = 30 * time.Second
)

// httpStatusError is a non-success HTTP reply.
type httpStatusError struct {
	Method string
	Status int
	Body   string
}

func (e *httpStatusError) Error() string {
	msg := fmt.Sprintf("mcp: HTTP %s returned %d %s", e.Method, e.Status, http.StatusText(e.Status))
	switch e.Status {
	case http.StatusUnauthorized, http.StatusForbidden:
		msg += " (check the server's headers, e.g. Authorization, in the MCP config)"
	}
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

// httpTransport implements the MCP Streamable HTTP transport: every client
// message is POSTed to one endpoint, and the server answers with a JSON body
// or an SSE stream. An optional GET stream carries server-initiated messages.
type httpTransport struct {
	url     string
	headers map[string]string
	client  *http.Client
	handler func([]byte)
	logf    func(string, ...any)

	mu              sync.Mutex
	sessionID       string
	protocolVersion string

	ctx      context.Context
	cancel   context.CancelFunc
	listenWG sync.WaitGroup
	doneCh   chan struct{}
	once     sync.Once
	listenOn sync.Once
}

func newHTTPTransport(cfg ServerConfig, client *http.Client, handler func([]byte), logf func(string, ...any)) *httpTransport {
	if client == nil {
		client = &http.Client{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &httpTransport{
		url:     cfg.URL,
		headers: cfg.Headers,
		client:  client,
		handler: handler,
		logf:    logf,
		ctx:     ctx,
		cancel:  cancel,
		doneCh:  make(chan struct{}),
	}
}

func (t *httpTransport) start() error { return nil }

func (t *httpTransport) done() <-chan struct{} { return t.doneCh }

func (t *httpTransport) err() error {
	select {
	case <-t.doneCh:
		return ErrClosed
	default:
		return nil
	}
}

// setProtocolVersion records the negotiated version, sent on later requests.
func (t *httpTransport) setProtocolVersion(v string) {
	t.mu.Lock()
	t.protocolVersion = v
	t.mu.Unlock()
}

// resetSession forgets the session so the next initialize starts a new one.
func (t *httpTransport) resetSession() {
	t.mu.Lock()
	t.sessionID = ""
	t.protocolVersion = ""
	t.mu.Unlock()
}

// session returns the current session id.
func (t *httpTransport) session() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionID
}

func (t *httpTransport) newRequest(ctx context.Context, method string, body []byte, accept string) (*http.Request, string, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, t.url, rd)
	if err != nil {
		return nil, "", err
	}
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	t.mu.Lock()
	sid, pv := t.sessionID, t.protocolVersion
	t.mu.Unlock()
	if sid != "" {
		req.Header.Set(headerSessionID, sid)
	}
	if pv != "" {
		req.Header.Set(headerProtocolVersion, pv)
	}
	return req, sid, nil
}

// linked returns a context cancelled when either ctx or the transport ends.
func (t *httpTransport) linked(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(t.ctx, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

func (t *httpTransport) send(ctx context.Context, msg []byte) error {
	select {
	case <-t.doneCh:
		return ErrClosed
	default:
	}
	var head wireMessage
	if err := json.Unmarshal(msg, &head); err != nil {
		return fmt.Errorf("mcp: outgoing message is not valid JSON: %w", err)
	}
	wantID, isRequest := "", head.Method != "" && head.hasID()
	if isRequest {
		wantID, _ = idKey(head.ID)
	}

	ctx, cancel := t.linked(ctx)
	defer cancel()
	req, sid, err := t.newRequest(ctx, http.MethodPost, msg, acceptPost)
	if err != nil {
		return fmt.Errorf("mcp: build request: %w", err)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		if t.ctx.Err() != nil {
			return ErrClosed
		}
		return fmt.Errorf("mcp: POST %s: %w", redactURL(t.url), unwrapURLError(err))
	}
	defer resp.Body.Close()

	if id := resp.Header.Get(headerSessionID); id != "" {
		t.mu.Lock()
		t.sessionID = id
		t.mu.Unlock()
	}
	if resp.StatusCode == http.StatusNotFound && sid != "" {
		drain(resp.Body)
		return ErrSessionExpired
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return statusError(http.MethodPost, resp)
	}
	if !isRequest {
		// Notifications and replies are acknowledged with 202 and no body.
		drain(resp.Body)
		return nil
	}
	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent {
		drain(resp.Body)
		return fmt.Errorf("mcp: server acknowledged %s without returning a response", head.Method)
	}

	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch ct {
	case "text/event-stream":
		got, lastID, retry, err := t.readStream(ctx, resp.Body, wantID)
		if got {
			return nil
		}
		if err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		return t.resume(ctx, wantID, lastID, retry, err)
	case "application/json", "":
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxMessageSize+1))
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("mcp: read response: %w", err)
		}
		if len(body) > maxMessageSize {
			return fmt.Errorf("mcp: response larger than %d bytes", maxMessageSize)
		}
		got, err := t.deliver(body, wantID)
		if err != nil {
			return fmt.Errorf("mcp: invalid JSON response: %w", err)
		}
		if !got {
			return fmt.Errorf("mcp: response body did not answer %s", head.Method)
		}
		return nil
	default:
		drain(resp.Body)
		return fmt.Errorf("mcp: unexpected response content type %q", resp.Header.Get("Content-Type"))
	}
}

// resume reconnects with GET and Last-Event-ID after a response stream ended
// before carrying the response, as the Streamable HTTP spec allows.
func (t *httpTransport) resume(ctx context.Context, wantID, lastID string, retry time.Duration, cause error) error {
	if cause == nil {
		cause = errors.New("stream ended before the server responded")
	}
	for attempt := 0; attempt < maxResumeAttempts && lastID != ""; attempt++ {
		if retry > 0 && !sleepCtx(ctx, min(retry, listenMaxBackoff)) {
			return ctx.Err()
		}
		resp, err := t.openStream(ctx, lastID)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			cause = err
			continue
		}
		got, id, r, err := t.readStream(ctx, resp.Body, wantID)
		resp.Body.Close()
		if got {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if id != "" {
			lastID = id
		}
		if r > 0 {
			retry = r
		}
		if err != nil {
			cause = err
		}
	}
	return fmt.Errorf("mcp: %w", cause)
}

// openStream issues a GET for an SSE stream. It returns an error for any
// reply that is not an event stream.
func (t *httpTransport) openStream(ctx context.Context, lastEventID string) (*http.Response, error) {
	req, sid, err := t.newRequest(ctx, http.MethodGet, nil, "text/event-stream")
	if err != nil {
		return nil, err
	}
	if lastEventID != "" {
		req.Header.Set(headerLastEventID, lastEventID)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", redactURL(t.url), unwrapURLError(err))
	}
	if resp.StatusCode == http.StatusNotFound && sid != "" {
		drain(resp.Body)
		return nil, ErrSessionExpired
	}
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(http.MethodGet, resp)
	}
	if ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); ct != "text/event-stream" {
		drain(resp.Body)
		return nil, fmt.Errorf("GET returned content type %q, want text/event-stream", resp.Header.Get("Content-Type"))
	}
	return resp, nil
}

// readStream parses an SSE stream, handing each message event to the
// handler. It stops once the response to wantID has been delivered (when
// wantID is set) or the stream ends. It reports the last event id and any
// retry hint so the caller can resume.
func (t *httpTransport) readStream(ctx context.Context, body io.Reader, wantID string) (got bool, lastID string, retry time.Duration, err error) {
	r := bufio.NewReaderSize(body, 64<<10)
	var data bytes.Buffer
	event := ""
	hasData := false
	for {
		line, rerr := readLine(r, maxMessageSize)
		if errors.Is(rerr, errLineTooLong) {
			t.logf("dropped an SSE line larger than %d bytes", maxMessageSize)
			data.Reset()
			hasData = false
			continue
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				rerr = nil
			}
			if rerr == nil {
				rerr = errors.New("stream ended before the server responded")
			}
			return false, lastID, retry, rerr
		}
		if len(line) == 0 {
			// Blank line: dispatch the event.
			if hasData && (event == "" || event == "message") {
				ok, derr := t.deliver(data.Bytes(), wantID)
				if derr != nil {
					t.logf("ignoring malformed SSE event: %v", derr)
				}
				if ok {
					return true, lastID, retry, nil
				}
			}
			data.Reset()
			event, hasData = "", false
			continue
		}
		if line[0] == ':' {
			continue // comment / keep-alive
		}
		field, value, _ := bytes.Cut(line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "data":
			if hasData {
				data.WriteByte('\n')
			}
			if data.Len()+len(value) > maxMessageSize {
				t.logf("dropped an SSE event larger than %d bytes", maxMessageSize)
				data.Reset()
				hasData = false
				continue
			}
			data.Write(value)
			hasData = true
		case "event":
			event = string(value)
		case "id":
			if !bytes.ContainsRune(value, 0) {
				lastID = string(value)
			}
		case "retry":
			if ms, perr := strconv.Atoi(string(value)); perr == nil && ms >= 0 {
				retry = time.Duration(ms) * time.Millisecond
			}
		}
		if ctx.Err() != nil {
			return false, lastID, retry, ctx.Err()
		}
	}
}

// deliver passes every message in body to the handler and reports whether
// one of them is the response to wantID.
func (t *httpTransport) deliver(body []byte, wantID string) (bool, error) {
	msgs, err := splitBatch(body)
	if err != nil {
		return false, err
	}
	got := false
	for _, m := range msgs {
		if wantID != "" && !got {
			var head wireMessage
			if json.Unmarshal(m, &head) == nil && head.Method == "" {
				if k, ok := idKey(head.ID); ok && k == wantID {
					got = true
				}
			}
		}
		t.handler(m)
	}
	return got, nil
}

// listen opens the optional GET stream for server-initiated messages (such
// as notifications/tools/list_changed) and keeps it open, reconnecting with
// backoff, until the transport closes. A 405 means the server does not offer
// the stream, which is normal.
func (t *httpTransport) listen() {
	t.listenOn.Do(func() {
		t.listenWG.Add(1)
		go t.listenLoop()
	})
}

func (t *httpTransport) listenLoop() {
	defer t.listenWG.Done()
	backoff := listenMinBackoff
	lastID := ""
	for t.ctx.Err() == nil {
		resp, err := t.openStream(t.ctx, lastID)
		if err != nil {
			if t.ctx.Err() != nil {
				return
			}
			var se *httpStatusError
			if errors.As(err, &se) && se.Status != http.StatusTooManyRequests && se.Status < 500 {
				if se.Status != http.StatusMethodNotAllowed {
					t.logf("server-sent event stream unavailable: %v", err)
				}
				return
			}
			if errors.Is(err, ErrSessionExpired) {
				return
			}
			if !sleepCtx(t.ctx, backoff) {
				return
			}
			backoff = min(backoff*2, listenMaxBackoff)
			continue
		}
		backoff = listenMinBackoff
		_, id, retry, _ := t.readStream(t.ctx, resp.Body, "")
		resp.Body.Close()
		if id != "" {
			lastID = id
		}
		if retry > 0 {
			backoff = min(retry, listenMaxBackoff)
		}
		if !sleepCtx(t.ctx, backoff) {
			return
		}
	}
}

// close stops the listener, ends the session with DELETE and aborts any
// in-flight requests.
func (t *httpTransport) close() error {
	t.once.Do(func() {
		if sid := t.session(); sid != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			if req, _, err := t.newRequest(ctx, http.MethodDelete, nil, ""); err == nil {
				if resp, err := t.client.Do(req); err == nil {
					drain(resp.Body)
					resp.Body.Close()
				}
			}
			cancel()
		}
		t.cancel()
		waitTimeout(&t.listenWG, 5*time.Second)
		close(t.doneCh)
	})
	return nil
}

func statusError(method string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	drain(resp.Body)
	body := strings.Join(strings.Fields(string(b)), " ")
	var rpc struct {
		Error *RPCError `json:"error"`
	}
	if json.Unmarshal(b, &rpc) == nil && rpc.Error != nil && rpc.Error.Message != "" {
		body = rpc.Error.Message
	}
	return &httpStatusError{Method: method, Status: resp.StatusCode, Body: clip(body, 300)}
}

func drain(r io.Reader) { _, _ = io.Copy(io.Discard, io.LimitReader(r, 64<<10)) }

// unwrapURLError drops the *url.Error wrapper, whose message repeats the
// full URL (which may carry a token in its query string).
func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
