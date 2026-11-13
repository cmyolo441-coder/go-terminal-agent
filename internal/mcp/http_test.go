package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func connectHTTP(t *testing.T, srv *httptest.Server, headers map[string]string, opts ClientOptions) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Connect(ctx, "remote", ServerConfig{URL: srv.URL + "/mcp", Headers: headers, TimeoutSec: 10}, opts)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func checkHeaders(t *testing.T, f *httpFake) {
	t.Helper()
	if _, _, _, fails, _ := f.snapshot(); len(fails) > 0 {
		t.Fatalf("header problems: %v", fails)
	}
}

func TestHTTPJSONLifecycle(t *testing.T) {
	f, srv := newHTTPFake(t)
	f.token = "tok-123"
	c := connectHTTP(t, srv, map[string]string{"Authorization": "Bearer tok-123"}, testClientOptions(t))

	if got := len(c.Tools()); got != 8 {
		t.Fatalf("tools = %d, want 8 (paginated)", got)
	}
	if sid := c.http.session(); sid != "sess-1" {
		t.Fatalf("session = %q", sid)
	}
	if got := callText(t, c, "echo", `{"text":"over http"}`).Text(); got != "over http" {
		t.Fatalf("echo = %q", got)
	}
	if res := callText(t, c, "fail", `{}`); !res.IsError {
		t.Fatalf("fail not isError: %+v", res)
	}
	var rpc *RPCError
	if _, err := c.CallTool(context.Background(), "missing", nil); !errors.As(err, &rpc) {
		t.Fatalf("unknown tool err = %v", err)
	}
	checkHeaders(t, f)
	c.Close()
	inits, deleted, notes, _, _ := f.snapshot()
	if inits != 1 {
		t.Errorf("initialize count = %d", inits)
	}
	if len(deleted) != 1 || deleted[0] != "sess-1" {
		t.Errorf("DELETE sessions = %v", deleted)
	}
	if len(notes) == 0 || notes[0] != "notifications/initialized" {
		t.Errorf("notifications = %v", notes)
	}
}

func TestHTTPSSEResponses(t *testing.T) {
	f, srv := newHTTPFake(t)
	f.sseAll = true
	var logs []string
	opts := testClientOptions(t)
	logCh := make(chan string, 16)
	opts.Logf = func(format string, args ...any) {
		select {
		case logCh <- format:
		default:
		}
	}
	c := connectHTTP(t, srv, nil, opts)
	if got := len(c.Tools()); got != 8 {
		t.Fatalf("tools over SSE = %d", got)
	}
	if got := callText(t, c, "add", `{"a":1,"b":2}`).Text(); got != "3" {
		t.Fatalf("add over SSE = %q", got)
	}
	// Notification, comment, server request, non-message event, then a
	// response split over two data lines.
	if got := callText(t, c, "sse_chatty", `{}`).Text(); got != "streamed" {
		t.Fatalf("sse_chatty = %q", got)
	}
	waitFor(t, func() bool {
		_, _, _, _, replies := f.snapshot()
		return len(replies) == 1
	})
	_, _, _, _, replies := f.snapshot()
	if replies[0].Error == nil || replies[0].Error.Code != CodeMethodNotFound || string(replies[0].ID) != `"srv-1"` {
		t.Fatalf("reply to server request = %+v", replies[0])
	}
	close(logCh)
	for l := range logCh {
		logs = append(logs, l)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "[%s] %s") {
		t.Errorf("server log notification not forwarded: %v", logs)
	}
	checkHeaders(t, f)
}

func TestHTTPResumesDroppedStream(t *testing.T) {
	f, srv := newHTTPFake(t)
	c := connectHTTP(t, srv, nil, testClientOptions(t))
	if got := callText(t, c, "sse_drop", `{}`).Text(); got != "resumed" {
		t.Fatalf("sse_drop = %q", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	found := false
	for _, s := range f.resumeIDs {
		if s == "resumed:ev-7" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no GET with Last-Event-ID ev-7: %v", f.resumeIDs)
	}
}

func TestHTTPTimeoutSendsCancel(t *testing.T) {
	f, srv := newHTTPFake(t)
	c := connectHTTP(t, srv, nil, testClientOptions(t))
	c.conn.timeout = 150 * time.Millisecond
	start := time.Now()
	_, err := c.CallTool(context.Background(), "slow", json.RawMessage(`{"ms":5000}`))
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("timeout took %s", d)
	}
	waitFor(t, func() bool {
		_, _, notes, _, _ := f.snapshot()
		return strings.Contains(strings.Join(notes, ","), "notifications/cancelled")
	})
}

func TestHTTPSessionExpiryReinitializes(t *testing.T) {
	f, srv := newHTTPFake(t)
	c := connectHTTP(t, srv, nil, testClientOptions(t))
	callText(t, c, "expire", `{}`)
	if got := callText(t, c, "echo", `{"text":"after expiry"}`).Text(); got != "after expiry" {
		t.Fatalf("echo = %q", got)
	}
	inits, _, _, _, _ := f.snapshot()
	if inits != 2 {
		t.Fatalf("initialize count = %d, want 2", inits)
	}
	if sid := c.http.session(); sid != "sess-2" {
		t.Fatalf("session = %q", sid)
	}
	checkHeaders(t, f)
}

func TestHTTPUnauthorized(t *testing.T) {
	f, srv := newHTTPFake(t)
	f.token = "right"
	_, err := Connect(context.Background(), "remote", ServerConfig{URL: srv.URL, Headers: map[string]string{"Authorization": "Bearer wrong"}}, testClientOptions(t))
	if err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "Authorization") {
		t.Fatalf("err = %v", err)
	}
}

func TestHTTPListChangedOverGETStream(t *testing.T) {
	f, srv := newHTTPFake(t)
	f.allowGET = true
	changed := make(chan []ToolInfo, 2)
	opts := testClientOptions(t)
	opts.OnToolsChanged = func(ts []ToolInfo) { changed <- ts }
	c := connectHTTP(t, srv, nil, opts)
	callText(t, c, "change", `{}`)
	select {
	case ts := <-changed:
		if len(ts) != 9 {
			t.Fatalf("tools after change = %d", len(ts))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("list_changed over the GET stream was not handled")
	}
	start := time.Now()
	c.Close()
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("Close with an open GET stream took %s", d)
	}
}

func TestHTTPServerErrors(t *testing.T) {
	// A server that is down.
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	if _, err := Connect(context.Background(), "remote", ServerConfig{URL: url}, testClientOptions(t)); err == nil || !strings.Contains(err.Error(), "POST") {
		t.Fatalf("down server err = %v", err)
	}

	// A server that crashes mid-request (connection dropped).
	crash := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("no hijacker")
			return
		}
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer crash.Close()
	if _, err := Connect(context.Background(), "remote", ServerConfig{URL: crash.URL}, testClientOptions(t)); err == nil || !strings.Contains(err.Error(), "initialize") {
		t.Fatalf("crashing server err = %v", err)
	}

	// A 500 with a JSON-RPC error body, and a wrong content type.
	for _, tc := range []struct {
		h    http.HandlerFunc
		want string
	}{
		{func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"database unavailable"}}`))
		}, "500 Internal Server Error: database unavailable"},
		{func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html>"))
		}, `unexpected response content type "text/html"`},
		{func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusAccepted)
		}, "without returning a response"},
	} {
		s := httptest.NewServer(tc.h)
		_, err := Connect(context.Background(), "remote", ServerConfig{URL: s.URL}, testClientOptions(t))
		s.Close()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("err = %v, want %q", err, tc.want)
		}
	}
}
