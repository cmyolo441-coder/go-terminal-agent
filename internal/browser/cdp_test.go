package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCDP is a scripted CDP endpoint on the in-process WebSocket server.
// Each command is passed to handle, which may reply through the
// connection immediately, later, or never.
type fakeCDP struct {
	t      *testing.T
	srv    *wsTestServer
	connCh chan *wsConn
}

type fakeCmd struct {
	ID        int64           `json:"id"`
	SessionID string          `json:"sessionId"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params"`
}

func newFakeCDP(t *testing.T, handle func(c *wsConn, cmd fakeCmd)) *fakeCDP {
	t.Helper()
	f := &fakeCDP{t: t, connCh: make(chan *wsConn, 1)}
	f.srv = newWSTestServer(t, wsConfig{}, func(c *wsConn) {
		f.connCh <- c
		defer c.Close()
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			var cmd fakeCmd
			if err := json.Unmarshal(data, &cmd); err != nil {
				t.Errorf("fake CDP: bad command %s: %v", data, err)
				return
			}
			handle(c, cmd)
		}
	})
	return f
}

func (f *fakeCDP) dial() (*cdpConn, *wsConn) {
	f.t.Helper()
	ws := dialTest(f.t, f.srv.wsURL("/devtools/browser/x"), wsConfig{})
	conn := newCDPConn(wsTransport{ws}, f.t.Logf)
	f.t.Cleanup(func() { conn.Close() })
	select {
	case sc := <-f.connCh:
		return conn, sc
	case <-time.After(5 * time.Second):
		f.t.Fatal("fake server saw no connection")
		return nil, nil
	}
}

func reply(c *wsConn, v any) {
	b, _ := json.Marshal(v)
	_ = c.WriteMessage(wsOpText, b)
}

func TestCDPOutOfOrderResponses(t *testing.T) {
	var mu sync.Mutex
	var held []fakeCmd
	f := newFakeCDP(t, func(c *wsConn, cmd fakeCmd) {
		mu.Lock()
		held = append(held, cmd)
		ready := len(held) == 3
		batch := held
		mu.Unlock()
		if !ready {
			return
		}
		// Answer in reverse order of arrival.
		for i := len(batch) - 1; i >= 0; i-- {
			var p struct{ N int }
			json.Unmarshal(batch[i].Params, &p)
			reply(c, map[string]any{"id": batch[i].ID, "sessionId": batch[i].SessionID, "result": map[string]any{"double": p.N * 2, "method": batch[i].Method}})
		}
	})
	conn, _ := f.dial()
	ctx := testCtx(t, 5*time.Second)

	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for n := 1; n <= 3; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var res struct {
				Double int
				Method string
			}
			method := fmt.Sprintf("Test.m%d", n)
			if err := conn.call(ctx, "S", method, map[string]int{"N": n}, &res); err != nil {
				errs <- err
				return
			}
			if res.Double != n*2 || res.Method != method {
				errs <- fmt.Errorf("call %d got %+v", n, res)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestCDPErrorResponse(t *testing.T) {
	f := newFakeCDP(t, func(c *wsConn, cmd fakeCmd) {
		reply(c, map[string]any{"id": cmd.ID, "error": map[string]any{"code": -32601, "message": "'Nope.method' wasn't found", "data": "extra"}})
	})
	conn, _ := f.dial()
	err := conn.call(testCtx(t, 5*time.Second), "", "Nope.method", nil, nil)
	var perr *ProtocolError
	if !errors.As(err, &perr) || perr.Code != -32601 || perr.Method != "Nope.method" || perr.Data != "extra" {
		t.Fatalf("err = %#v", err)
	}
	if !strings.Contains(perr.Error(), "wasn't found") {
		t.Fatalf("message %q", perr.Error())
	}
}

func TestCDPParamsAndSessionOnTheWire(t *testing.T) {
	got := make(chan fakeCmd, 2)
	f := newFakeCDP(t, func(c *wsConn, cmd fakeCmd) {
		got <- cmd
		reply(c, map[string]any{"id": cmd.ID, "result": map[string]any{}})
	})
	conn, _ := f.dial()
	ctx := testCtx(t, 5*time.Second)
	if err := conn.call(ctx, "SESSION-1", "Page.navigate", map[string]any{"url": "https://x.test/"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := conn.call(ctx, "", "Browser.getVersion", nil, nil); err != nil {
		t.Fatal(err)
	}
	c1, c2 := <-got, <-got
	if c1.SessionID != "SESSION-1" || c1.Method != "Page.navigate" || !bytes.Contains(c1.Params, []byte(`"url":"https://x.test/"`)) {
		t.Fatalf("first command %+v (%s)", c1, c1.Params)
	}
	if c2.SessionID != "" || c2.Params != nil || c2.ID <= c1.ID {
		t.Fatalf("second command %+v (%s)", c2, c2.Params)
	}
}

func TestCDPEventRouting(t *testing.T) {
	f := newFakeCDP(t, func(c *wsConn, cmd fakeCmd) {
		if cmd.Method == "Emit" {
			// Events for two sessions and the browser, then the response.
			reply(c, map[string]any{"method": "Page.loadEventFired", "sessionId": "A", "params": map[string]any{"n": 1}})
			reply(c, map[string]any{"method": "Page.loadEventFired", "sessionId": "B", "params": map[string]any{"n": 2}})
			reply(c, map[string]any{"method": "Target.targetCreated", "params": map[string]any{"n": 3}})
			reply(c, map[string]any{"method": "Network.requestWillBeSent", "sessionId": "A", "params": map[string]any{"n": 4}})
			reply(c, map[string]any{"id": cmd.ID, "result": map[string]any{}})
		}
	})
	conn, _ := f.dial()
	ctx := testCtx(t, 5*time.Second)

	var mu sync.Mutex
	var log []string
	record := func(tag string) eventHandler {
		return func(method string, params json.RawMessage) {
			var p struct{ N int }
			json.Unmarshal(params, &p)
			mu.Lock()
			log = append(log, fmt.Sprintf("%s:%s:%d", tag, method, p.N))
			mu.Unlock()
		}
	}
	unsubA := conn.subscribe("A", "Page.loadEventFired", record("a-load"))
	conn.subscribe("B", "Page.loadEventFired", record("b-load"))
	conn.subscribe("", "Target.targetCreated", record("root"))
	unsubAll := conn.subscribe("A", "", record("a-all"))

	// Events precede the response on the wire, so they are dispatched
	// before call returns.
	if err := conn.call(ctx, "", "Emit", nil, nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := strings.Join(log, " ")
	log = nil
	mu.Unlock()
	for _, want := range []string{
		"a-load:Page.loadEventFired:1", "a-all:Page.loadEventFired:1",
		"b-load:Page.loadEventFired:2", "root:Target.targetCreated:3",
		"a-all:Network.requestWillBeSent:4",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	if strings.Count(got, ":") != 10 {
		t.Errorf("unexpected deliveries: %s", got)
	}

	unsubA()
	unsubA() // idempotent
	unsubAll()
	if err := conn.call(ctx, "", "Emit", nil, nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got = strings.Join(log, " ")
	mu.Unlock()
	if strings.Contains(got, "a-") {
		t.Fatalf("unsubscribed handlers still called: %s", got)
	}
	if !strings.Contains(got, "b-load") || !strings.Contains(got, "root") {
		t.Fatalf("remaining handlers not called: %s", got)
	}
}

func TestCDPContextCancel(t *testing.T) {
	f := newFakeCDP(t, func(c *wsConn, cmd fakeCmd) {
		if cmd.Method == "Hang" {
			return
		}
		reply(c, map[string]any{"id": cmd.ID, "result": map[string]any{"ok": true}})
	})
	conn, _ := f.dial()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := conn.call(ctx, "", "Hang", nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("cancel not honored")
	}
	conn.mu.Lock()
	n := len(conn.pending)
	conn.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d pending entries leaked", n)
	}
	// The connection is still healthy.
	var res struct{ OK bool }
	if err := conn.call(testCtx(t, 5*time.Second), "", "Ping", nil, &res); err != nil || !res.OK {
		t.Fatalf("after cancel: %v %+v", err, res)
	}
	// Already-cancelled contexts fail without sending.
	done, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if err := conn.call(done, "", "Ping", nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled: %v", err)
	}
}

func TestCDPAbortChannel(t *testing.T) {
	f := newFakeCDP(t, func(c *wsConn, cmd fakeCmd) {})
	conn, _ := f.dial()
	abort := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(abort)
	}()
	err := conn.callAbort(testCtx(t, 5*time.Second), abort, ErrPageClosed, "S", "Hang", nil, nil)
	if !errors.Is(err, ErrPageClosed) {
		t.Fatalf("err = %v", err)
	}
}

func TestCDPServerDisconnectFailsPending(t *testing.T) {
	f := newFakeCDP(t, func(c *wsConn, cmd fakeCmd) {
		if cmd.Method == "Die" {
			c.closeNetConn()
		}
	})
	conn, _ := f.dial()
	ctx := testCtx(t, 5*time.Second)
	hung := make(chan error, 1)
	go func() { hung <- conn.call(ctx, "", "Hang", nil, nil) }()
	time.Sleep(30 * time.Millisecond)
	_ = conn.send("", "Die", nil)
	select {
	case err := <-hung:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("pending call: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pending call not failed on disconnect")
	}
	select {
	case <-conn.Done():
	case <-time.After(time.Second):
		t.Fatal("Done not closed")
	}
	if !errors.Is(conn.Err(), ErrClosed) {
		t.Fatalf("Err = %v", conn.Err())
	}
	if err := conn.call(ctx, "", "After", nil, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("call after disconnect: %v", err)
	}
	if err := conn.send("", "After", nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("send after disconnect: %v", err)
	}
}

func TestCDPCloseIsCleanAndIdempotent(t *testing.T) {
	before := runtime.NumGoroutine()
	f := newFakeCDP(t, func(c *wsConn, cmd fakeCmd) {})
	conn, _ := f.dial()
	ctx := testCtx(t, 5*time.Second)
	hung := make(chan error, 1)
	go func() { hung <- conn.call(ctx, "", "Hang", nil, nil) }()
	time.Sleep(30 * time.Millisecond)
	start := time.Now()
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Close took %v", d)
	}
	if err := <-hung; !errors.Is(err, ErrClosed) {
		t.Fatalf("pending call: %v", err)
	}
	conn.Close()
	f.srv.CloseClientConnections()
	waitFor(t, 3*time.Second, "goroutines to exit", func() bool {
		return runtime.NumGoroutine() <= before+2 // httptest keeps idle workers
	})
}

func TestCDPMalformedMessagesAreSkipped(t *testing.T) {
	f := newFakeCDP(t, func(c *wsConn, cmd fakeCmd) {
		_ = c.WriteMessage(wsOpText, []byte("{not json"))
		reply(c, map[string]any{"id": 999999, "result": map[string]any{}}) // unknown id
		reply(c, map[string]any{"id": cmd.ID, "result": map[string]any{"v": 1}})
	})
	conn, _ := f.dial()
	var res struct{ V int }
	if err := conn.call(testCtx(t, 5*time.Second), "", "X", nil, &res); err != nil || res.V != 1 {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestCDPUnencodableParams(t *testing.T) {
	f := newFakeCDP(t, func(c *wsConn, cmd fakeCmd) {})
	conn, _ := f.dial()
	if err := conn.call(testCtx(t, time.Second), "", "X", map[string]any{"f": func() {}}, nil); err == nil {
		t.Fatal("unencodable params accepted")
	}
}

func TestPipeTransportRoundTrip(t *testing.T) {
	// Two OS-pipe-like channels: we write to the "browser" and read what
	// it writes back, framed with NUL bytes.
	toR, toW := io.Pipe()
	fromR, fromW := io.Pipe()
	tr := newPipeTransport(fromR, toW)
	defer tr.Close()

	// The fake browser echoes every NUL-terminated message.
	echoDone := make(chan struct{})
	go func() {
		defer close(echoDone)
		defer fromW.Close()
		peer := newPipeTransport(toR, fromW)
		for {
			msg, err := peer.ReadMessage()
			if err != nil {
				return
			}
			if err := peer.WriteMessage(msg); err != nil {
				return
			}
		}
	}()

	big := `{"data":"` + strings.Repeat("y", 300<<10) + `"}`
	for _, msg := range []string{`{"id":1}`, `{}`, big, `{"id":2,"method":"ü"}`} {
		if err := tr.WriteMessage([]byte(msg)); err != nil {
			t.Fatal(err)
		}
		got, err := tr.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != msg {
			t.Fatalf("round trip mismatch (len %d vs %d)", len(got), len(msg))
		}
	}
	if err := tr.WriteMessage([]byte("a\x00b")); err == nil {
		t.Fatal("NUL accepted")
	}
	tr.Close()
	tr.Close()
	<-echoDone
}

func TestPipeTransportLimitsAndEOF(t *testing.T) {
	r := io.NopCloser(strings.NewReader(strings.Repeat("z", 200) + "\x00partial"))
	tr := newPipeTransport(r, nopWriteCloser{io.Discard})
	tr.maxMsg = 100
	if _, err := tr.ReadMessage(); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized: %v", err)
	}
	tr2 := newPipeTransport(io.NopCloser(strings.NewReader("{}\x00partial")), nopWriteCloser{io.Discard})
	if m, err := tr2.ReadMessage(); err != nil || string(m) != "{}" {
		t.Fatalf("first: %q %v", m, err)
	}
	if _, err := tr2.ReadMessage(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated: %v", err)
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
