package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// peer is the far end of a Conn under test, speaking raw frames.
type peer struct {
	r  *bufio.Reader
	w  io.WriteCloser
	mu sync.Mutex
}

func (p *peer) read(t *testing.T) fakeMsg {
	t.Helper()
	body, err := fakeReadFrame(p.r)
	if err != nil {
		t.Fatalf("peer read: %v", err)
	}
	var m fakeMsg
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("peer got bad json %s: %v", body, err)
	}
	return m
}

func (p *peer) send(t *testing.T, v any) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := fakeWriteFrame(p.w, v); err != nil {
		t.Errorf("peer write: %v", err)
	}
}

func (p *peer) raw(t *testing.T, s string) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, err := io.WriteString(p.w, s); err != nil {
		t.Errorf("peer raw write: %v", err)
	}
}

// connPair returns a Conn and the peer at the other end of in-memory pipes.
func connPair(t *testing.T, opts ConnOptions) (*Conn, *peer) {
	t.Helper()
	toConnR, toConnW := io.Pipe()
	fromConnR, fromConnW := io.Pipe()
	c := NewConn(toConnR, fromConnW, closerFunc(func() error {
		toConnR.Close()
		return fromConnW.Close()
	}), opts)
	p := &peer{r: bufio.NewReader(fromConnR), w: toConnW}
	t.Cleanup(func() {
		c.Close()
		toConnW.Close()
		fromConnR.Close()
	})
	return c, p
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

func TestConnCorrelatesConcurrentCalls(t *testing.T) {
	c, p := connPair(t, ConnOptions{})
	const n = 40
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var out struct{ Echo int }
			if err := c.Call(context.Background(), "echo", map[string]int{"v": i}, &out); err != nil {
				errs <- err
				return
			}
			if out.Echo != i {
				errs <- fmt.Errorf("call %d got %d", i, out.Echo)
			}
		}(i)
	}
	// Collect every request, then answer in reverse order.
	var reqs []fakeMsg
	for i := 0; i < n; i++ {
		reqs = append(reqs, p.read(t))
	}
	for i := len(reqs) - 1; i >= 0; i-- {
		var params struct{ V int }
		if err := json.Unmarshal(reqs[i].Params, &params); err != nil {
			t.Fatal(err)
		}
		p.send(t, map[string]any{"jsonrpc": "2.0", "id": reqs[i].ID, "result": map[string]int{"echo": params.V}})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestConnErrorsAndNullResults(t *testing.T) {
	c, p := connPair(t, ConnOptions{})
	done := make(chan error, 1)
	go func() { done <- c.Call(context.Background(), "nope", nil, nil) }()
	m := p.read(t)
	if m.Method != "nope" || len(m.Params) != 0 {
		t.Fatalf("request %+v", m)
	}
	p.send(t, map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": CodeMethodNotFound, "message": "no such method"}})
	err := <-done
	var re *ResponseError
	if !errors.As(err, &re) || re.Message != "no such method" || !IsMethodNotFound(err) {
		t.Fatalf("error = %v", err)
	}

	// A null result leaves the target untouched; a string id is accepted.
	go func() {
		out := map[string]int{"keep": 1}
		err := c.Call(context.Background(), "null", nil, &out)
		if err == nil && out["keep"] != 1 {
			err = fmt.Errorf("target changed: %v", out)
		}
		done <- err
	}()
	m = p.read(t)
	p.send(t, map[string]any{"jsonrpc": "2.0", "id": string(m.ID), "result": nil})
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	// A result that does not decode is an error naming the method.
	go func() {
		var out []int
		done <- c.Call(context.Background(), "typed", nil, &out)
	}()
	m = p.read(t)
	p.send(t, map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": "not a list"})
	if err := <-done; err == nil || !strings.Contains(err.Error(), "typed") {
		t.Fatalf("decode error = %v", err)
	}
}

func TestConnNotificationsInOrder(t *testing.T) {
	got := make(chan string, 100)
	_, p := connPair(t, ConnOptions{OnNotification: func(method string, params json.RawMessage) {
		var v struct{ N int }
		_ = json.Unmarshal(params, &v)
		got <- fmt.Sprintf("%s:%d", method, v.N)
	}})
	for i := 0; i < 50; i++ {
		p.send(t, map[string]any{"jsonrpc": "2.0", "method": "note", "params": map[string]int{"n": i}})
	}
	for i := 0; i < 50; i++ {
		select {
		case s := <-got:
			if want := fmt.Sprintf("note:%d", i); s != want {
				t.Fatalf("got %s, want %s", s, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("notification lost")
		}
	}
}

func TestConnAnswersPeerRequests(t *testing.T) {
	block := make(chan struct{})
	_, p := connPair(t, ConnOptions{OnRequest: func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		switch method {
		case "add":
			var v []int
			if err := json.Unmarshal(params, &v); err != nil {
				return nil, err
			}
			return v[0] + v[1], nil
		case "boom":
			panic("handler exploded")
		case "fail":
			return nil, errors.New("plain failure")
		case "slow":
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-block:
				return "late", nil
			}
		}
		return nil, &ResponseError{Code: CodeMethodNotFound, Message: "unknown"}
	}})
	defer close(block)

	p.send(t, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "slow"})
	p.send(t, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "add", "params": []int{2, 3}})
	// The slow handler must not block the fast one.
	m := p.read(t)
	if string(m.ID) != "2" || string(m.Result) != "5" {
		t.Fatalf("add response %+v", m)
	}
	p.send(t, map[string]any{"jsonrpc": "2.0", "method": "$/cancelRequest", "params": map[string]any{"id": 1}})
	m = p.read(t)
	if string(m.ID) != "1" || !strings.Contains(string(m.Error), fmt.Sprint(CodeRequestCancelled)) {
		t.Fatalf("cancelled response %+v", m)
	}

	for _, tc := range []struct {
		method string
		code   int
		text   string
	}{
		{"boom", CodeInternalError, "handler exploded"},
		{"fail", CodeInternalError, "plain failure"},
		{"other", CodeMethodNotFound, "unknown"},
	} {
		p.send(t, map[string]any{"jsonrpc": "2.0", "id": "s-" + tc.method, "method": tc.method})
		m := p.read(t)
		var e ResponseError
		if err := json.Unmarshal(m.Error, &e); err != nil || e.Code != tc.code || !strings.Contains(e.Message, tc.text) {
			t.Errorf("%s answered %+v (%s)", tc.method, m, m.Error)
		}
		if string(m.ID) != `"s-`+tc.method+`"` {
			t.Errorf("%s: id echoed as %s", tc.method, m.ID)
		}
	}
}

func TestConnWithoutHandlerRejectsRequests(t *testing.T) {
	_, p := connPair(t, ConnOptions{})
	p.send(t, map[string]any{"jsonrpc": "2.0", "id": 7, "method": "anything"})
	m := p.read(t)
	if !strings.Contains(string(m.Error), fmt.Sprint(CodeMethodNotFound)) {
		t.Fatalf("response %+v", m)
	}
}

func TestConnCancelSendsCancelRequest(t *testing.T) {
	c, p := connPair(t, ConnOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Call(ctx, "slow", nil, nil) }()
	req := p.read(t)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("call = %v", err)
	}
	m := p.read(t)
	var params struct {
		ID json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(m.Params, &params)
	if m.Method != "$/cancelRequest" || string(params.ID) != string(req.ID) {
		t.Fatalf("cancel notification %+v", m)
	}
	// A late response to the cancelled call is dropped harmlessly.
	p.send(t, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": 1})
	go func() { done <- c.Call(context.Background(), "next", nil, nil) }()
	m = p.read(t)
	p.send(t, map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": nil})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestConnCloseFailsPending(t *testing.T) {
	c, p := connPair(t, ConnOptions{})
	done := make(chan error, 1)
	go func() { done <- c.Call(context.Background(), "wait", nil, nil) }()
	p.read(t)
	c.Close()
	if err := <-done; !errors.Is(err, ErrConnClosed) {
		t.Fatalf("pending call = %v", err)
	}
	select {
	case <-c.Done():
	default:
		t.Fatal("Done not closed")
	}
	if err := c.Call(context.Background(), "after", nil, nil); !errors.Is(err, ErrConnClosed) {
		t.Fatalf("call after close = %v", err)
	}
	if err := c.Notify("after", nil); !errors.Is(err, ErrConnClosed) {
		t.Fatalf("notify after close = %v", err)
	}
	c.Close() // idempotent
}

func TestConnPeerHangupFailsPending(t *testing.T) {
	c, p := connPair(t, ConnOptions{})
	done := make(chan error, 1)
	go func() { done <- c.Call(context.Background(), "wait", nil, nil) }()
	p.read(t)
	p.w.Close()
	err := <-done
	if !errors.Is(err, ErrConnClosed) || !strings.Contains(err.Error(), "closed the connection") {
		t.Fatalf("pending call = %v", err)
	}
	if c.Err() == nil {
		t.Fatal("Err not set")
	}
}

func TestConnSkipsMalformedJSON(t *testing.T) {
	var mu sync.Mutex
	var logs []string
	c, p := connPair(t, ConnOptions{Logf: func(f string, a ...any) {
		mu.Lock()
		logs = append(logs, fmt.Sprintf(f, a...))
		mu.Unlock()
	}})
	p.raw(t, "Content-Length: 9\r\n\r\n{not json")
	p.raw(t, "\r\n") // a stray blank line between messages
	p.raw(t, "Content-Length: 2\r\n\r\n{}")
	done := make(chan error, 1)
	go func() { done <- c.Call(context.Background(), "still", nil, nil) }()
	m := p.read(t)
	p.send(t, map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": true})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(logs) < 2 || !strings.Contains(strings.Join(logs, "\n"), "malformed") {
		t.Fatalf("logs %v", logs)
	}
}

func TestConnRejectsOversizedMessage(t *testing.T) {
	c, p := connPair(t, ConnOptions{MaxMessage: 64})
	go p.raw(t, "Content-Length: 1000\r\n\r\n")
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("oversized message accepted")
	}
	if err := c.Err(); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v", err)
	}
}

func TestReadFrame(t *testing.T) {
	cases := []struct {
		in, body, err string
	}{
		{in: "Content-Length: 2\r\n\r\n{}", body: "{}"},
		{in: "content-length:2\r\nContent-Type: x\r\n\r\n[]", body: "[]"},
		{in: "\r\n\r\nContent-Length: 1\n\n7", body: "7"},
		{in: "Content-Type: x\r\n\r\n{}", err: "missing Content-Length"},
		{in: "Content-Length: -4\r\n\r\n", err: "bad Content-Length"},
		{in: "Content-Length: abc\r\n\r\n", err: "bad Content-Length"},
		{in: "garbage\r\n\r\n", err: "malformed header"},
		{in: "Content-Length: 10\r\n\r\n{}", err: io.ErrUnexpectedEOF.Error()},
		{in: "Content-Length: 10\r\n", err: io.ErrUnexpectedEOF.Error()},
		{in: "", err: io.EOF.Error()},
		{in: "X: " + strings.Repeat("a", 9000) + "\r\n\r\n", err: "too long"},
		{in: "Content-Length: 100\r\n\r\n", err: "exceeds"},
	}
	for _, tc := range cases {
		body, err := readFrame(bufio.NewReader(strings.NewReader(tc.in)), 50)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%q: err = %v, want %q", clipLine(tc.in, 40), err, tc.err)
			}
			continue
		}
		if err != nil || string(body) != tc.body {
			t.Errorf("%q: body %q err %v", tc.in, body, err)
		}
	}
}
