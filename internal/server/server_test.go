package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/agent"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permission"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

// ---- scripted fake provider ----

type step func(ctx context.Context, req llm.Request, on func(llm.Event)) (*llm.Response, error)

type fakeProvider struct {
	mu       sync.Mutex
	steps    []step
	requests []llm.Request
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Stream(ctx context.Context, req llm.Request, on func(llm.Event)) (*llm.Response, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	var s step
	if len(f.steps) > 0 {
		s, f.steps = f.steps[0], f.steps[1:]
	}
	f.mu.Unlock()
	if s == nil {
		return textStep("(script finished)")(ctx, req, on)
	}
	return s(ctx, req, on)
}

func (f *fakeProvider) lastRequest() llm.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]
}

func textStep(text string) step {
	return func(ctx context.Context, req llm.Request, on func(llm.Event)) (*llm.Response, error) {
		half := len(text) / 2
		on(llm.Event{Type: llm.EventThinkingDelta, Text: "pondering"})
		on(llm.Event{Type: llm.EventTextDelta, Text: text[:half]})
		on(llm.Event{Type: llm.EventTextDelta, Text: text[half:]})
		return &llm.Response{
			Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: text}}},
			StopReason: llm.StopEndTurn, Model: "fake-model",
			Usage: llm.Usage{InputTokens: 10, OutputTokens: 5},
		}, nil
	}
}

func toolStep(id, name, input string) step {
	return func(ctx context.Context, req llm.Request, on func(llm.Event)) (*llm.Response, error) {
		on(llm.Event{Type: llm.EventToolUseStart, ToolName: name, ToolID: id})
		return &llm.Response{
			Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
				{Type: llm.BlockToolUse, ToolUseID: id, ToolName: name, Input: json.RawMessage(input)},
			}},
			StopReason: llm.StopToolUse, Model: "fake-model", Usage: llm.Usage{InputTokens: 7, OutputTokens: 3},
		}, nil
	}
}

// blockStep waits until the turn is cancelled.
func blockStep(started chan<- struct{}) step {
	return func(ctx context.Context, req llm.Request, on func(llm.Event)) (*llm.Response, error) {
		if started != nil {
			close(started)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
}

// ---- a tool that needs approval ----

type thingTool struct{ runs *atomic.Int32 }

func (thingTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "run_thing", Description: "Runs the thing"}
}
func (thingTool) Kind() tools.Kind { return tools.KindExec }
func (t thingTool) Prepare(_ *tools.Env, _ json.RawMessage) (*tools.Call, error) {
	return tools.NewCall("run_thing", tools.KindExec, "Run the thing", "thing --now", "thing --now", func(context.Context) (string, error) {
		t.runs.Add(1)
		return "ran ok", nil
	}), nil
}

// ---- harness ----

const testToken = "test-token-0123456789abcdef"

type harness struct {
	t         *testing.T
	srv       *Server
	http      *httptest.Server
	providers chan *fakeProvider
	runs      atomic.Int32
	workspace string
}

func newHarness(t *testing.T, mut func(*Options)) *harness {
	t.Helper()
	h := &harness{t: t, providers: make(chan *fakeProvider, 16), workspace: t.TempDir()}
	opts := Options{
		Token: testToken,
		Factory: func(ctx context.Context, so SessionOptions) (*agent.Agent, error) {
			if so.Model == "explode" {
				return nil, errors.New("no such model")
			}
			var p *fakeProvider
			select {
			case p = <-h.providers:
			default:
				p = &fakeProvider{}
			}
			env, err := tools.NewEnv(h.workspace)
			if err != nil {
				return nil, err
			}
			mode := permission.ModeAsk
			if so.Mode != "" {
				mode, _ = permission.ParseMode(so.Mode)
			}
			model := so.Model
			if model == "" {
				model = "fake-model"
			}
			return agent.New(p, tools.NewRegistry(thingTool{runs: &h.runs}), env, permission.NewGate(mode, nil), "system", agent.Settings{Model: model, MaxTokens: 1000}), nil
		},
	}
	if mut != nil {
		mut(&opts)
	}
	srv, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	h.srv = srv
	h.http = httptest.NewServer(srv.Handler())
	t.Cleanup(h.http.Close)
	return h
}

func (h *harness) script(steps ...step) *fakeProvider {
	p := &fakeProvider{steps: steps}
	h.providers <- p
	return p
}

func (h *harness) do(method, path string, body any, hdr ...string) (*http.Response, map[string]any) {
	h.t.Helper()
	return h.doCtx(context.Background(), method, path, body, hdr...)
}

func (h *harness) doCtx(ctx context.Context, method, path string, body any, hdr ...string) (*http.Response, map[string]any) {
	h.t.Helper()
	var r io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		r = strings.NewReader(b)
	default:
		data, _ := json.Marshal(b)
		r = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.http.URL+path, r)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i+1] == "" {
			req.Header.Del(hdr[i])
		} else {
			req.Header.Set(hdr[i], hdr[i+1])
		}
	}
	if hdr != nil && len(hdr) == 1 {
		req.Host = hdr[0]
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return resp, nil
	}
	defer resp.Body.Close()
	var out map[string]any
	data, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(data, &out)
	return resp, out
}

func (h *harness) create(opts map[string]any) string {
	h.t.Helper()
	resp, body := h.do("POST", "/v1/sessions", opts)
	if resp.StatusCode != http.StatusCreated {
		h.t.Fatalf("create: %d %v", resp.StatusCode, body)
	}
	return body["id"].(string)
}

type sseEvent struct {
	ID   string
	Name string
	Data map[string]any
}

type sseReader struct {
	sc *bufio.Scanner
}

func newSSE(r io.Reader) *sseReader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	return &sseReader{sc: sc}
}

func (s *sseReader) next() (sseEvent, error) {
	var ev sseEvent
	for s.sc.Scan() {
		line := s.sc.Text()
		switch {
		case line == "":
			if ev.Name != "" {
				return ev, nil
			}
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "id: "):
			ev.ID = line[4:]
		case strings.HasPrefix(line, "event: "):
			ev.Name = line[7:]
		case strings.HasPrefix(line, "data: "):
			if err := json.Unmarshal([]byte(line[6:]), &ev.Data); err != nil {
				return ev, err
			}
		}
	}
	if err := s.sc.Err(); err != nil {
		return ev, err
	}
	return ev, io.EOF
}

// until reads events up to and including the first one named name.
func (s *sseReader) until(t *testing.T, name string) []sseEvent {
	t.Helper()
	var out []sseEvent
	for {
		ev, err := s.next()
		if err != nil {
			t.Fatalf("waiting for %s: %v (got %v)", name, err, names(out))
		}
		out = append(out, ev)
		if ev.Name == name {
			return out
		}
	}
}

func names(evs []sseEvent) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Name)
	}
	return out
}

func (h *harness) send(id string, body any) (*http.Response, *sseReader) {
	h.t.Helper()
	resp, _ := h.do("POST", "/v1/sessions/"+id+"/messages", body)
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("message: status %d", resp.StatusCode)
	}
	h.t.Cleanup(func() { resp.Body.Close() })
	return resp, newSSE(resp.Body)
}

// ---- tests ----

func TestTextTurnEndToEnd(t *testing.T) {
	h := newHarness(t, nil)
	h.script(textStep("Hello there"))

	resp, body := h.do("GET", "/v1/health", nil, "Authorization", "")
	if resp.StatusCode != 200 || body["status"] != "ok" || body["version"] == "" {
		t.Fatalf("health: %d %v", resp.StatusCode, body)
	}
	id := h.create(map[string]any{"title": "demo"})
	if !strings.HasPrefix(id, "ses_") {
		t.Fatalf("id %q", id)
	}
	resp, stream := h.send(id, map[string]any{"text": "hi"})
	if resp.Header.Get("Cache-Control") != "no-cache, no-store" {
		t.Fatalf("headers: %v", resp.Header)
	}
	evs := stream.until(t, EventDone)
	got := strings.Join(names(evs), ",")
	if got != "turn_start,thinking,text,text,usage,done" {
		t.Fatalf("events: %s", got)
	}
	if evs[0].ID != "1" || evs[5].ID != "6" {
		t.Fatalf("ids: %s %s", evs[0].ID, evs[5].ID)
	}
	if evs[2].Data["text"].(string)+evs[3].Data["text"].(string) != "Hello there" || evs[1].Data["text"] != "pondering" {
		t.Fatalf("deltas: %v", evs)
	}
	if u := evs[4].Data["usage"].(map[string]any); u["input_tokens"] != float64(10) || evs[4].Data["model"] != "fake-model" {
		t.Fatalf("usage: %v", evs[4].Data)
	}
	done := evs[5].Data
	if done["stop_reason"] != "end_turn" || done["text"] != "Hello there" || done["turn"] != float64(1) {
		t.Fatalf("done: %v", done)
	}
	if _, err := stream.next(); err != io.EOF {
		t.Fatalf("stream should end after done: %v", err)
	}

	_, info := h.do("GET", "/v1/sessions/"+id, nil)
	if info["turns"] != float64(1) || info["messages"] != float64(2) || info["busy"] != false || info["title"] != "demo" || info["history"] != nil {
		t.Fatalf("info: %v", info)
	}
	_, info = h.do("GET", "/v1/sessions/"+id+"?history=1", nil)
	if hist, ok := info["history"].([]any); !ok || len(hist) != 2 {
		t.Fatalf("history: %v", info["history"])
	}
	_, list := h.do("GET", "/v1/sessions", nil)
	if ss := list["sessions"].([]any); len(ss) != 1 || ss[0].(map[string]any)["id"] != id {
		t.Fatalf("list: %v", list)
	}
}

func TestApprovalAllowsTool(t *testing.T) {
	h := newHarness(t, nil)
	h.script(toolStep("tu_1", "run_thing", `{}`), textStep("All done."))
	id := h.create(nil)
	_, stream := h.send(id, map[string]any{"text": "do the thing"})
	evs := stream.until(t, EventApprovalRequired)
	req := evs[len(evs)-1].Data
	if req["tool"] != "run_thing" || req["kind"] != "exec" || req["command"] != "thing --now" || req["summary"] != "Run the thing" || req["expires_at"] == "" || req["destructive"] != false {
		t.Fatalf("approval: %v", req)
	}
	if strings.Join(names(evs), ",") != "turn_start,tool_call,usage,approval_required" {
		t.Fatalf("before approval: %v", names(evs))
	}
	reqID := req["request_id"].(string)
	resp, body := h.do("POST", "/v1/sessions/"+id+"/approvals/"+reqID, map[string]any{"decision": "maybe"})
	if resp.StatusCode != 400 {
		t.Fatalf("bad decision: %d %v", resp.StatusCode, body)
	}
	resp, body = h.do("POST", "/v1/sessions/"+id+"/approvals/"+reqID, map[string]any{"decision": "allow_once"})
	if resp.StatusCode != 200 || body["decision"] != "allow_once" {
		t.Fatalf("approve: %d %v", resp.StatusCode, body)
	}
	evs = stream.until(t, EventDone)
	if got := strings.Join(names(evs), ","); got != "approval_resolved,tool_start,tool_done,thinking,text,text,usage,done" {
		t.Fatalf("after approval: %s", got)
	}
	if evs[0].Data["by"] != "api" || evs[0].Data["decision"] != "allow_once" {
		t.Fatalf("resolved: %v", evs[0].Data)
	}
	if evs[2].Data["output"] != "ran ok" || evs[2].Data["is_error"] != false {
		t.Fatalf("tool_done: %v", evs[2].Data)
	}
	if d := evs[len(evs)-1].Data; d["text"] != "All done." || d["usage"].(map[string]any)["input_tokens"] != float64(17) {
		t.Fatalf("done: %v", d)
	}
	if h.runs.Load() != 1 {
		t.Fatalf("tool ran %d times", h.runs.Load())
	}
	// The approval is gone once answered.
	resp, _ = h.do("POST", "/v1/sessions/"+id+"/approvals/"+reqID, map[string]any{"decision": "deny"})
	if resp.StatusCode != 404 {
		t.Fatalf("stale approval: %d", resp.StatusCode)
	}
}

func TestApprovalTimeoutDenies(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.ApprovalTimeout = 100 * time.Millisecond })
	p := h.script(toolStep("tu_9", "run_thing", `{}`))
	id := h.create(nil)
	_, stream := h.send(id, map[string]any{"text": "go"})
	evs := stream.until(t, EventDone)
	var resolved, failed map[string]any
	for _, e := range evs {
		switch e.Name {
		case EventApprovalResolved:
			resolved = e.Data
		case EventToolFailed:
			failed = e.Data
		}
	}
	if resolved["by"] != "timeout" || resolved["decision"] != "deny" {
		t.Fatalf("resolved: %v", resolved)
	}
	if failed["reason"] != "declined" || failed["tool"] != "run_thing" {
		t.Fatalf("failed: %v", failed)
	}
	if h.runs.Load() != 0 {
		t.Fatal("a denied tool must not run")
	}
	if n := len(p.requests); n != 1 {
		t.Fatalf("declining ends the turn; model calls %d", n)
	}
}

func TestBusyInterruptAndDisconnect(t *testing.T) {
	h := newHarness(t, nil)
	started := make(chan struct{})
	h.script(blockStep(started))
	id := h.create(nil)
	_, stream := h.send(id, map[string]any{"text": "long task"})
	<-started
	resp, body := h.do("POST", "/v1/sessions/"+id+"/messages", map[string]any{"text": "again"})
	if resp.StatusCode != http.StatusConflict || body["error"].(map[string]any)["code"] != "session_busy" {
		t.Fatalf("busy: %d %v", resp.StatusCode, body)
	}
	_, body = h.do("POST", "/v1/sessions/"+id+"/interrupt", nil)
	if body["interrupted"] != true {
		t.Fatalf("interrupt: %v", body)
	}
	evs := stream.until(t, EventDone)
	if d := evs[len(evs)-1].Data; d["stop_reason"] != "interrupted" {
		t.Fatalf("done: %v", d)
	}
	_, body = h.do("POST", "/v1/sessions/"+id+"/interrupt", "{}")
	if body["interrupted"] != false {
		t.Fatalf("idle interrupt: %v", body)
	}

	// A client that disconnects cancels its turn.
	started2 := make(chan struct{})
	h.script(blockStep(started2))
	id2 := h.create(nil)
	ctx, cancel := context.WithCancel(context.Background())
	resp, _ = h.doCtx(ctx, "POST", "/v1/sessions/"+id2+"/messages", map[string]any{"text": "x"})
	<-started2
	cancel()
	resp.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, info := h.do("GET", "/v1/sessions/"+id2, nil)
		if info["busy"] == false {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the turn kept running after the client left")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func pngData(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	png.Encode(&buf, image.NewGray(image.Rect(0, 0, 3, 3)))
	return buf.Bytes()
}

func TestImagesReachTheModel(t *testing.T) {
	h := newHarness(t, nil)
	p := h.script(textStep("I see it"))
	raw := pngData(t)
	if err := os.WriteFile(filepath.Join(h.workspace, "shot.png"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	img, _ := llm.ImageFromBytes(raw)
	id := h.create(nil)
	_, stream := h.send(id, map[string]any{"text": "look", "images": []map[string]any{
		{"path": "shot.png"},
		{"media_type": "image/png", "data": img.Data},
	}})
	stream.until(t, EventDone)
	msg := p.lastRequest().Messages[0]
	if len(msg.Blocks) != 3 || msg.Blocks[1].Type != llm.BlockImage || msg.Blocks[1].Image.Data != img.Data || msg.Blocks[2].Image.MediaType != "image/png" {
		t.Fatalf("blocks: %+v", msg.Blocks)
	}
}

func TestRequestValidation(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.MaxBodyBytes = 2048 })
	id := h.create(nil)
	cases := []struct {
		method, path string
		body         any
		status       int
		code         string
	}{
		{"POST", "/v1/sessions/" + id + "/messages", map[string]any{"text": strings.Repeat("x", 4096)}, 413, "too_large"},
		{"POST", "/v1/sessions/" + id + "/messages", `{"text": `, 400, "bad_request"},
		{"POST", "/v1/sessions/" + id + "/messages", map[string]any{"text": "x", "extra": 1}, 400, "bad_request"},
		{"POST", "/v1/sessions/" + id + "/messages", map[string]any{"text": "  "}, 400, "bad_request"},
		{"POST", "/v1/sessions/" + id + "/messages", nil, 400, "bad_request"},
		{"POST", "/v1/sessions/" + id + "/messages", map[string]any{"text": "x", "images": []map[string]any{{"media_type": "image/png", "data": "Zm9v"}}}, 400, "bad_request"},
		{"POST", "/v1/sessions/" + id + "/messages", map[string]any{"text": "x", "images": []map[string]any{{"path": "missing.png"}}}, 400, "bad_request"},
		{"POST", "/v1/sessions/nope/messages", map[string]any{"text": "x"}, 404, "no_session"},
		{"POST", "/v1/sessions", map[string]any{"mode": "chaos"}, 400, "bad_request"},
		{"POST", "/v1/sessions", map[string]any{"model": "explode"}, 400, "session_failed"},
		{"POST", "/v1/sessions/" + id + "/approvals/apr_x", map[string]any{"decision": "deny"}, 404, "no_approval"},
		{"GET", "/v1/nothing", nil, 404, "not_found"},
	}
	for _, c := range cases {
		resp, body := h.do(c.method, c.path, c.body)
		code := ""
		if e, ok := body["error"].(map[string]any); ok {
			code, _ = e["code"].(string)
		}
		if resp.StatusCode != c.status || code != c.code {
			t.Errorf("%s %s: got %d %v, want %d %s", c.method, c.path, resp.StatusCode, body, c.status, c.code)
		}
	}
	resp, _ := h.do("POST", "/v1/sessions", `{}`, "Content-Type", "text/plain")
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("content type: %d", resp.StatusCode)
	}
	resp, body := h.do("PUT", "/v1/sessions/"+id+"/interrupt", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") == "" || body["error"].(map[string]any)["code"] != "method_not_allowed" {
		t.Fatalf("method: %d %v", resp.StatusCode, body)
	}
}

func TestAuthHostAndCORS(t *testing.T) {
	h := newHarness(t, nil)
	for _, auth := range []string{"", "Bearer wrong-token-xxxxxxxxxxxxxxx", "Basic " + testToken, "Bearer " + testToken + "x"} {
		resp, body := h.do("GET", "/v1/sessions", nil, "Authorization", auth)
		if resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") == "" || body["error"] == nil {
			t.Errorf("auth %q: %d", auth, resp.StatusCode)
		}
	}
	if resp, _ := h.do("GET", "/v1/sessions", nil, "Authorization", "bearer "+testToken); resp.StatusCode != 200 {
		t.Fatalf("scheme is case-insensitive: %d", resp.StatusCode)
	}
	if resp, _ := h.do("GET", "/v1/health", nil, "evil.example:7878"); resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("foreign Host header: %d", resp.StatusCode)
	}
	if resp, _ := h.do("GET", "/v1/health", nil, "localhost:7878"); resp.StatusCode != 200 {
		t.Fatalf("localhost Host header: %d", resp.StatusCode)
	}
	resp, _ := h.do("GET", "/v1/sessions", nil, "Origin", "https://evil.example")
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("CORS must be off by default")
	}
	resp, _ = h.do("OPTIONS", "/v1/sessions", nil, "Origin", "https://evil.example", "Access-Control-Request-Method", "POST")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("preflight without CORS: %d", resp.StatusCode)
	}

	c := newHarness(t, func(o *Options) { o.CORSOrigins = []string{"http://localhost:3000"} })
	resp, _ = c.do("OPTIONS", "/v1/sessions", nil, "Origin", "http://localhost:3000", "Access-Control-Request-Method", "POST")
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Access-Control-Allow-Origin") != "http://localhost:3000" || !strings.Contains(resp.Header.Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Fatalf("preflight: %d %v", resp.StatusCode, resp.Header)
	}
	resp, _ = c.do("GET", "/v1/sessions", nil, "Origin", "http://localhost:3000")
	if resp.Header.Get("Access-Control-Allow-Origin") != "http://localhost:3000" {
		t.Fatal("allowed origin must be echoed")
	}
	resp, _ = c.do("GET", "/v1/sessions", nil, "Origin", "http://localhost:4000")
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("other origins get no CORS headers")
	}
}

func TestSessionLimitsDeleteAndExpiry(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.MaxSessions = 1; o.SessionIdleTTL = time.Hour })
	id := h.create(nil)
	resp, body := h.do("POST", "/v1/sessions", nil)
	if resp.StatusCode != http.StatusServiceUnavailable || body["error"].(map[string]any)["code"] != "too_many_sessions" {
		t.Fatalf("limit: %d %v", resp.StatusCode, body)
	}
	if n := h.srv.evictIdle(time.Now().Add(30 * time.Minute)); n != 0 {
		t.Fatal("not idle long enough")
	}
	if n := h.srv.evictIdle(time.Now().Add(2 * time.Hour)); n != 1 {
		t.Fatal("expired session must go")
	}
	if resp, _ := h.do("GET", "/v1/sessions/"+id, nil); resp.StatusCode != 404 {
		t.Fatalf("expired session still there: %d", resp.StatusCode)
	}
	id = h.create(map[string]any{"mode": "plan", "model": "other"})
	_, info := h.do("GET", "/v1/sessions/"+id, nil)
	if info["mode"] != "plan" || info["model"] != "other" {
		t.Fatalf("options: %v", info)
	}
	_, body = h.do("DELETE", "/v1/sessions/"+id, nil)
	if body["deleted"] != true || body["interrupted"] != false {
		t.Fatalf("delete: %v", body)
	}
	if resp, _ := h.do("DELETE", "/v1/sessions/"+id, nil); resp.StatusCode != 404 {
		t.Fatalf("double delete: %d", resp.StatusCode)
	}
}

func TestServeLifecycle(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "run", "token")
	started := make(chan struct{})
	p := &fakeProvider{steps: []step{blockStep(started)}}
	srv, err := New(Options{
		Addr:            "127.0.0.1:0",
		TokenFile:       tokenFile,
		ShutdownTimeout: 5 * time.Second,
		KeepAlive:       20 * time.Millisecond,
		Factory: func(ctx context.Context, so SessionOptions) (*agent.Agent, error) {
			env, _ := tools.NewEnv(dir)
			return agent.New(p, tools.NewRegistry(), env, permission.NewGate(permission.ModeAsk, nil), "", agent.Settings{Model: "m"}), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(srv.Token()) < 32 {
		t.Fatalf("generated token too short: %q", srv.Token())
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- srv.ListenAndServe(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(tokenFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("token file never appeared")
		}
		time.Sleep(5 * time.Millisecond)
	}
	st, _ := os.Stat(tokenFile)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode %v", st.Mode().Perm())
	}
	data, _ := os.ReadFile(tokenFile)
	if strings.TrimSpace(string(data)) != srv.Token() {
		t.Fatal("token file content")
	}
	base := "http://" + srv.Addr()
	call := func(method, path, body string) *http.Response {
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+srv.Token())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := call("POST", "/v1/sessions", "")
	var created map[string]any
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	resp = call("POST", fmt.Sprintf("/v1/sessions/%s/messages", created["id"]), `{"text":"work"}`)
	defer resp.Body.Close()
	stream := newSSE(resp.Body)
	<-started
	// Keep-alive pings flow while the turn runs.
	time.Sleep(60 * time.Millisecond)
	cancel()
	evs := stream.until(t, EventDone)
	if d := evs[len(evs)-1].Data; d["stop_reason"] != "interrupted" {
		t.Fatalf("shutdown must interrupt turns: %v", d)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("server did not shut down")
	}
	if _, err := os.Stat(tokenFile); !os.IsNotExist(err) {
		t.Fatal("token file must be removed on shutdown")
	}
}

func TestOptionsValidation(t *testing.T) {
	f := func(context.Context, SessionOptions) (*agent.Agent, error) { return nil, nil }
	if _, err := New(Options{}); err == nil {
		t.Error("factory is required")
	}
	if _, err := New(Options{Factory: f, Token: "short"}); err == nil {
		t.Error("short token")
	}
	if _, err := New(Options{Factory: f, CORSOrigins: []string{"*"}}); err == nil {
		t.Error("wildcard origin")
	}
	for addr, ok := range map[string]bool{"127.0.0.1:0": true, "[::1]:7878": true, "localhost:1": true, "0.0.0.0:7878": false, ":7878": false, "10.0.0.2:1": false, "nonsense": false} {
		if err := checkAddr(addr, false); (err == nil) != ok {
			t.Errorf("checkAddr(%s) = %v", addr, err)
		}
	}
	if err := checkAddr("0.0.0.0:7878", true); err != nil {
		t.Error("explicitly allowed remote address")
	}
	srv, _ := New(Options{Factory: f, Addr: "0.0.0.0:0"})
	if err := srv.ListenAndServe(context.Background()); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Errorf("listen on all interfaces: %v", err)
	}
	h := newHarness(t, func(o *Options) { o.Factory = f })
	if resp, _ := h.do("POST", "/v1/sessions", nil); resp.StatusCode != 500 {
		t.Errorf("nil agent: %d", resp.StatusCode)
	}
}

func TestEventSinkUnit(t *testing.T) {
	var mu sync.Mutex
	var got []Event
	sink := NewEventSink(func(e Event) { mu.Lock(); got = append(got, e); mu.Unlock() }, time.Minute)
	sink.SetMaxOutput(4)
	call := tools.NewCall("bash", tools.KindExec, "Remove all", "rm -rf /", "rm -rf /", nil)
	if err := sink.Resolve("apr_missing", permission.DecisionAllowOnce); !errors.Is(err, ErrUnknownApproval) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan permission.Decision)
	go func() { result <- sink.Approve(ctx, call, "looks destructive") }()
	for len(sink.Pending()) == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if d := <-result; d != permission.DecisionDeny {
		t.Fatalf("cancelled approval: %v", d)
	}
	sink.ToolDone("t", call, "héllo world", errors.New("boom"))
	sink.Stream(llm.Event{Type: llm.EventServerToolStart, ToolName: "web_search", ToolID: "s1"})
	sink.Stream(llm.Event{Type: llm.EventToolInputDelta, Bytes: 5})
	sink.Notice("note")
	sink.SubagentProgress(0, "explore", "started")
	sink.ToolFailed("x", "y", "z")
	mu.Lock()
	defer mu.Unlock()
	var evNames []string
	for _, e := range got {
		evNames = append(evNames, e.Type)
	}
	if strings.Join(evNames, ",") != "approval_required,approval_resolved,tool_done,server_tool,notice,subagent,tool_failed" {
		t.Fatalf("events: %v", evNames)
	}
	if got[0].Data.(map[string]any)["destructive"] != true || got[1].Data.(map[string]any)["by"] != "cancelled" {
		t.Fatalf("approval events: %v %v", got[0].Data, got[1].Data)
	}
	done := got[2].Data.(map[string]any)
	if done["output"] != "hél" || done["truncated"] != true || done["error"] != "boom" {
		t.Fatalf("tool_done: %v", done)
	}
	for in, want := range map[string]permission.Decision{"allow_once": permission.DecisionAllowOnce, "ALLOW_SESSION": permission.DecisionAllowSession, "deny": permission.DecisionDeny} {
		if d, err := ParseDecision(in); err != nil || d != want {
			t.Errorf("%s: %v %v", in, d, err)
		}
	}
}
