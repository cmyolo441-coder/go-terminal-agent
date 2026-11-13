package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeEnv selects helper-process mode: the test binary re-executes itself
// with this variable set and behaves as a stdio MCP server.
const fakeEnv = "GTA_MCP_FAKE_SERVER"

func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeEnv); mode != "" {
		os.Exit(runFakeStdio(mode))
	}
	os.Exit(m.Run())
}

// stdioConfig runs this test binary as a fake server in the given mode.
func stdioConfig(mode string, env map[string]string) ServerConfig {
	e := map[string]string{fakeEnv: mode}
	for k, v := range env {
		e[k] = v
	}
	return ServerConfig{Command: os.Args[0], Args: []string{"-test.run=^$"}, Env: e, TimeoutSec: 10}
}

// fakeCore implements the tools shared by the stdio and HTTP fakes.
type fakeCore struct {
	mu        sync.Mutex
	extra     bool
	cancelled []string
	pageSize  int
}

func (f *fakeCore) toolList() []map[string]any {
	obj := func(props map[string]any, req ...string) map[string]any {
		s := map[string]any{"type": "object", "properties": props}
		if len(req) > 0 {
			s["required"] = req
		}
		return s
	}
	str := map[string]any{"type": "string"}
	list := []map[string]any{
		{"name": "echo", "description": "Echo text back.", "inputSchema": obj(map[string]any{"text": str}, "text")},
		{"name": "add", "title": "Add numbers", "inputSchema": obj(map[string]any{"a": map[string]any{"type": "number"}, "b": map[string]any{"type": "number"}}, "a", "b")},
		{"name": "fail", "description": "Always fails.", "inputSchema": obj(map[string]any{})},
		{"name": "media", "description": "Returns mixed content.", "inputSchema": obj(map[string]any{})},
		{"name": "slow", "description": "Sleeps.", "inputSchema": obj(map[string]any{"ms": map[string]any{"type": "integer"}})},
		{"name": "cancelled", "description": "Lists cancelled request ids.", "inputSchema": obj(map[string]any{})},
		{"name": "getenv", "description": "Reads an env var.", "inputSchema": obj(map[string]any{"name": str}, "name")},
		{"name": "change", "description": "Toggles the extra tool.", "inputSchema": obj(map[string]any{})},
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.extra {
		list = append(list, map[string]any{"name": "extra", "description": "Appears after change.", "inputSchema": obj(map[string]any{})})
	}
	return list
}

func (f *fakeCore) initResult(version string) map[string]any {
	if version == "" {
		version = ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": true}},
		"serverInfo":      map[string]any{"name": "fake", "version": "1.2.3"},
	}
}

func (f *fakeCore) list(params json.RawMessage) (any, *RPCError) {
	var p struct {
		Cursor string `json:"cursor"`
	}
	_ = json.Unmarshal(params, &p)
	all := f.toolList()
	start := 0
	if p.Cursor != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(p.Cursor, "page-"))
		if err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "bad cursor"}
		}
		start = n
	}
	size := f.pageSize
	if size <= 0 {
		size = 3
	}
	end := min(start+size, len(all))
	res := map[string]any{"tools": all[start:end]}
	if end < len(all) {
		res["nextCursor"] = fmt.Sprintf("page-%d", end)
	}
	return res, nil
}

func text(s string) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": s}}}
}

// call runs the shared tools; ok is false for a tool the caller handles.
func (f *fakeCore) call(ctx context.Context, params json.RawMessage) (res any, rpcErr *RPCError, ok bool) {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()}, true
	}
	switch p.Name {
	case "echo":
		return text(fmt.Sprint(p.Arguments["text"])), nil, true
	case "add":
		a, _ := p.Arguments["a"].(float64)
		b, _ := p.Arguments["b"].(float64)
		return text(strconv.FormatFloat(a+b, 'f', -1, 64)), nil, true
	case "fail":
		return map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "something broke"}}}, nil, true
	case "media":
		return map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "chart follows"},
			map[string]any{"type": "image", "mimeType": "image/png", "data": "AAECAwQFBgcICQ=="},
			map[string]any{"type": "resource", "resource": map[string]any{"uri": "file:///notes.txt", "mimeType": "text/plain", "text": "note body"}},
			map[string]any{"type": "resource", "resource": map[string]any{"uri": "file:///bin.dat", "mimeType": "application/octet-stream", "blob": "AAAA"}},
			map[string]any{"type": "resource_link", "uri": "https://example.com/doc", "name": "doc"},
		}}, nil, true
	case "slow":
		ms, _ := p.Arguments["ms"].(float64)
		select {
		case <-time.After(time.Duration(ms) * time.Millisecond):
			return text("done"), nil, true
		case <-ctx.Done():
			return nil, &RPCError{Code: CodeInternalError, Message: "cancelled"}, true
		}
	case "cancelled":
		f.mu.Lock()
		ids := append([]string(nil), f.cancelled...)
		f.mu.Unlock()
		sort.Strings(ids)
		return text(strings.Join(ids, ",")), nil, true
	case "getenv":
		return text(os.Getenv(fmt.Sprint(p.Arguments["name"]))), nil, true
	case "change":
		f.mu.Lock()
		f.extra = !f.extra
		f.mu.Unlock()
		return text("changed"), nil, true
	case "extra":
		return text("extra ran"), nil, true
	case "cwd": // unlisted: used to check the working directory
		wd, _ := os.Getwd()
		return text(wd), nil, true
	}
	return nil, nil, false
}

func (f *fakeCore) noteCancelled(id string) {
	f.mu.Lock()
	f.cancelled = append(f.cancelled, id)
	f.mu.Unlock()
}

// ---------------------------------------------------------------- stdio fake

type stdioFake struct {
	core    fakeCore
	mode    string
	out     *bufio.Writer
	outMu   sync.Mutex
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
	waiting map[string]chan wireMessage
	nextID  int
}

func (s *stdioFake) write(v any) {
	b, _ := json.Marshal(v)
	s.outMu.Lock()
	defer s.outMu.Unlock()
	s.out.Write(b)
	s.out.WriteByte('\n')
	s.out.Flush()
}

func (s *stdioFake) respond(id json.RawMessage, result any, rpcErr *RPCError) {
	m := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		m["error"] = rpcErr
	} else {
		m["result"] = result
	}
	s.write(m)
}

// ask sends a request to the client and waits for its reply.
func (s *stdioFake) ask(method string) wireMessage {
	s.mu.Lock()
	s.nextID++
	id := fmt.Sprintf("srv-%d", s.nextID)
	ch := make(chan wireMessage, 1)
	s.waiting[id] = ch
	s.mu.Unlock()
	s.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method})
	select {
	case m := <-ch:
		return m
	case <-time.After(5 * time.Second):
		return wireMessage{Error: &RPCError{Code: -1, Message: "no reply"}}
	}
}

func runFakeStdio(mode string) int {
	s := &stdioFake{mode: mode, out: bufio.NewWriter(os.Stdout), cancels: map[string]context.CancelFunc{}, waiting: map[string]chan wireMessage{}}
	switch mode {
	case "crash_start":
		fmt.Fprintln(os.Stderr, "cannot start: missing token")
		return 1
	case "noisy":
		fmt.Println("this is not JSON")
	case "stubborn":
		signal.Ignore(syscall.SIGTERM)
	}
	in := bufio.NewReader(os.Stdin)
	var wg sync.WaitGroup
	for {
		line, err := in.ReadBytes('\n')
		if err != nil {
			break
		}
		var m wireMessage
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		if m.Method == "" {
			key, _ := idKey(m.ID)
			s.mu.Lock()
			ch := s.waiting[key]
			delete(s.waiting, key)
			s.mu.Unlock()
			if ch != nil {
				ch <- m
			}
			continue
		}
		if !m.hasID() {
			if m.Method == "notifications/cancelled" {
				var p struct {
					RequestID json.RawMessage `json:"requestId"`
				}
				_ = json.Unmarshal(m.Params, &p)
				key, _ := idKey(p.RequestID)
				s.core.noteCancelled(key)
				s.mu.Lock()
				if c := s.cancels[key]; c != nil {
					c()
				}
				s.mu.Unlock()
			}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.serve(m)
		}()
	}
	if mode == "stubborn" {
		time.Sleep(time.Hour)
	}
	wg.Wait()
	return 0
}

func (s *stdioFake) serve(m wireMessage) {
	key, _ := idKey(m.ID)
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancels[key] = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.cancels, key)
		s.mu.Unlock()
		cancel()
	}()
	switch m.Method {
	case "initialize":
		switch s.mode {
		case "hang_init":
			return
		case "old_version":
			s.respond(m.ID, s.core.initResult("1999-01-01"), nil)
			return
		}
		fmt.Fprintln(os.Stderr, "fake server ready")
		s.respond(m.ID, s.core.initResult(""), nil)
	case "tools/list":
		res, e := s.core.list(m.Params)
		s.respond(m.ID, res, e)
	case "tools/call":
		var p struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(m.Params, &p)
		switch p.Name {
		case "crash":
			fmt.Fprintln(os.Stderr, "fatal: boom")
			os.Exit(3)
		case "ask_client":
			roots := s.ask("roots/list")
			ping := s.ask("ping")
			rs := "ok"
			if roots.Error != nil {
				rs = strconv.Itoa(roots.Error.Code)
			}
			ps := "ok"
			if ping.Error != nil {
				ps = strconv.Itoa(ping.Error.Code)
			}
			s.respond(m.ID, text("roots/list:"+rs+" ping:"+ps), nil)
			return
		case "change":
			res, _, _ := s.core.call(ctx, m.Params)
			s.respond(m.ID, res, nil)
			s.write(map[string]any{"jsonrpc": "2.0", "method": "notifications/tools/list_changed"})
			return
		}
		res, e, ok := s.core.call(ctx, m.Params)
		if !ok {
			e = &RPCError{Code: CodeInvalidParams, Message: "unknown tool " + p.Name}
		}
		s.respond(m.ID, res, e)
	case "ping":
		s.respond(m.ID, map[string]any{}, nil)
	default:
		s.respond(m.ID, nil, &RPCError{Code: CodeMethodNotFound, Message: "Method not found"})
	}
}

// ----------------------------------------------------------------- HTTP fake

// httpFake is a Streamable HTTP MCP server.
type httpFake struct {
	t    *testing.T
	core fakeCore

	token    string // required bearer token, if set
	sseAll   bool   // answer every request as an SSE stream
	allowGET bool   // offer the standalone GET stream

	mu          sync.Mutex
	session     string
	sessions    int
	inits       int
	deleted     []string
	replies     []wireMessage // client replies to server requests
	notes       []string      // notification methods received
	headerFails []string
	expireNext  bool
	resumeIDs   []string
	push        chan []byte
}

func newHTTPFake(t *testing.T) (*httpFake, *httptest.Server) {
	f := &httpFake{t: t, push: make(chan []byte, 8)}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *httpFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.token != "" && r.Header.Get("Authorization") != "Bearer "+f.token {
		http.Error(w, "bad token", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodGet:
		f.serveGET(w, r)
	case http.MethodDelete:
		f.mu.Lock()
		f.deleted = append(f.deleted, r.Header.Get(headerSessionID))
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case http.MethodPost:
		f.servePOST(w, r)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *httpFake) failHeader(msg string) {
	f.mu.Lock()
	f.headerFails = append(f.headerFails, msg)
	f.mu.Unlock()
}

func (f *httpFake) servePOST(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var m wireMessage
	if err := json.Unmarshal(body, &m); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if acc := r.Header.Get("Accept"); !strings.Contains(acc, "application/json") || !strings.Contains(acc, "text/event-stream") {
		f.failHeader("accept: " + acc)
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/json" {
		f.failHeader("content-type: " + ct)
	}
	if m.Method == "initialize" {
		f.mu.Lock()
		f.inits++
		f.sessions++
		f.session = fmt.Sprintf("sess-%d", f.sessions)
		sid := f.session
		f.mu.Unlock()
		if r.Header.Get(headerSessionID) != "" {
			f.failHeader("session id sent on initialize")
		}
		w.Header().Set(headerSessionID, sid)
		f.reply(w, m.ID, f.core.initResult(""), nil)
		return
	}
	f.mu.Lock()
	sid := f.session
	expire := f.expireNext
	f.expireNext = false
	if expire {
		f.session = "expired"
		sid = "expired"
	}
	f.mu.Unlock()
	got := r.Header.Get(headerSessionID)
	if got == "" {
		http.Error(w, "missing session", http.StatusBadRequest)
		return
	}
	if got != sid {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	if pv := r.Header.Get(headerProtocolVersion); pv != ProtocolVersion {
		f.failHeader("protocol version: " + pv)
	}

	switch {
	case m.Method == "" && m.hasID():
		f.mu.Lock()
		f.replies = append(f.replies, m)
		f.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		return
	case m.Method != "" && !m.hasID():
		f.mu.Lock()
		f.notes = append(f.notes, m.Method)
		f.mu.Unlock()
		if m.Method == "notifications/cancelled" {
			var p struct {
				RequestID json.RawMessage `json:"requestId"`
			}
			_ = json.Unmarshal(m.Params, &p)
			key, _ := idKey(p.RequestID)
			f.core.noteCancelled(key)
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}

	switch m.Method {
	case "tools/list":
		res, e := f.core.list(m.Params)
		f.reply(w, m.ID, res, e)
	case "ping":
		f.reply(w, m.ID, map[string]any{}, nil)
	case "tools/call":
		var p struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(m.Params, &p)
		switch p.Name {
		case "sse_chatty":
			// A log notification and a server request precede the response,
			// which is split over several data lines.
			f.sse(w, []string{
				"event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\"params\":{\"level\":\"info\",\"data\":\"working\"}}\n\n",
				": keep-alive\n\n",
				"data: {\"jsonrpc\":\"2.0\",\"id\":\"srv-1\",\"method\":\"roots/list\"}\n\n",
				"event: ignored\ndata: {\"jsonrpc\":\"2.0\",\"id\":" + string(m.ID) + ",\"result\":{}}\n\n",
				"id: ev-1\ndata: {\"jsonrpc\":\"2.0\",\ndata: \"id\":" + string(m.ID) + ",\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"streamed\"}]}}\n\n",
			})
			return
		case "sse_drop":
			// The stream ends after an event id but before the response; the
			// client must resume with GET and Last-Event-ID.
			f.mu.Lock()
			f.resumeIDs = append(f.resumeIDs, "pending:"+string(m.ID))
			f.mu.Unlock()
			f.sse(w, []string{"id: ev-7\nretry: 10\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n"})
			return
		case "expire":
			f.mu.Lock()
			f.expireNext = true
			f.mu.Unlock()
			f.reply(w, m.ID, text("will expire"), nil)
			return
		case "change":
			res, _, _ := f.core.call(r.Context(), m.Params)
			f.reply(w, m.ID, res, nil)
			f.push <- []byte(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`)
			return
		}
		res, e, ok := f.core.call(r.Context(), m.Params)
		if !ok {
			e = &RPCError{Code: CodeInvalidParams, Message: "unknown tool " + p.Name}
		}
		f.reply(w, m.ID, res, e)
	default:
		f.reply(w, m.ID, nil, &RPCError{Code: CodeMethodNotFound, Message: "Method not found"})
	}
}

func (f *httpFake) reply(w http.ResponseWriter, id json.RawMessage, result any, rpcErr *RPCError) {
	m := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		m["error"] = rpcErr
	} else {
		m["result"] = result
	}
	b, _ := json.Marshal(m)
	if f.sseAll {
		f.sse(w, []string{"data: " + string(b) + "\n\n"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func (f *httpFake) sse(w http.ResponseWriter, events []string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	for _, e := range events {
		io.WriteString(w, e)
		if fl != nil {
			fl.Flush()
		}
	}
}

func (f *httpFake) serveGET(w http.ResponseWriter, r *http.Request) {
	if last := r.Header.Get(headerLastEventID); last != "" {
		// Resumption of an interrupted response stream.
		f.mu.Lock()
		var pending string
		for _, s := range f.resumeIDs {
			if strings.HasPrefix(s, "pending:") {
				pending = strings.TrimPrefix(s, "pending:")
			}
		}
		f.resumeIDs = append(f.resumeIDs, "resumed:"+last)
		f.mu.Unlock()
		if pending == "" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		f.sse(w, []string{"id: ev-8\ndata: {\"jsonrpc\":\"2.0\",\"id\":" + pending + ",\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"resumed\"}]}}\n\n"})
		return
	}
	if !f.allowGET {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl := w.(http.Flusher)
	fl.Flush()
	for {
		select {
		case msg := <-f.push:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			fl.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (f *httpFake) snapshot() (inits int, deleted, notes, headerFails []string, replies []wireMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inits, append([]string(nil), f.deleted...), append([]string(nil), f.notes...),
		append([]string(nil), f.headerFails...), append([]wireMessage(nil), f.replies...)
}
