package e2e_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// serveToken is the obviously fake bearer token for gta serve.
const serveToken = "e2e-serve-token-0123456789abcdef"

// gtaServer is a running `gta serve`.
type gtaServer struct {
	base   string
	stderr *syncBuffer
}

// syncBuffer is a bytes.Buffer safe for a writer goroutine and a reader.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// freePort asks the OS for a free loopback port. Another process can grab
// it before gta binds it, so startServe retries.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// startServe runs `gta serve` until the test ends and waits for it to
// answer /v1/health.
func (e *gtaEnv) startServe(args ...string) *gtaServer {
	e.t.Helper()
	e.extra = append(e.extra, "GTA_SERVE_TOKEN="+serveToken)
	var lastErr string
	for attempt := 0; attempt < 3; attempt++ {
		addr := fmt.Sprintf("127.0.0.1:%d", freePort(e.t))
		ctx, cancel := context.WithCancel(context.Background())
		all := append([]string{"serve", "--addr", addr, "--token-file", filepath.Join(e.cfgDir, "serve.token")}, args...)
		cmd := e.command(ctx, all...)
		stderr := &syncBuffer{}
		cmd.Stdout, cmd.Stderr = io.Discard, stderr
		// Stop gracefully first; the context kill is the fallback.
		cmd.Cancel = func() error {
			if runtime.GOOS == "windows" {
				return cmd.Process.Kill()
			}
			return cmd.Process.Signal(os.Interrupt)
		}
		if err := cmd.Start(); err != nil {
			cancel()
			e.t.Fatal(err)
		}
		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait() }()
		srv := &gtaServer{base: "http://" + addr, stderr: stderr}
		if srv.waitHealthy(exited) {
			e.t.Cleanup(func() {
				cancel()
				select {
				case <-exited:
				case <-time.After(15 * time.Second):
					_ = cmd.Process.Kill()
					<-exited
					e.t.Errorf("gta serve did not stop on interrupt\nstderr:\n%s", stderr.String())
				}
				if _, err := os.Stat(filepath.Join(e.cfgDir, "serve.token")); err == nil && runtime.GOOS != "windows" {
					e.t.Error("gta serve left its token file behind")
				}
			})
			return srv
		}
		cancel()
		<-exited
		lastErr = stderr.String()
	}
	e.t.Fatalf("gta serve did not start\nstderr:\n%s", lastErr)
	return nil
}

// waitHealthy polls /v1/health until it answers, the process exits, or 20s
// pass.
func (s *gtaServer) waitHealthy(exited <-chan error) bool {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			return false
		default:
		}
		resp, err := http.Get(s.base + "/v1/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// do sends an authenticated JSON request and decodes a JSON reply into out.
func (s *gtaServer) do(t *testing.T, method, path string, body, out any) int {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, s.base+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+serveToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s %s: %v\n%s", method, path, err, data)
		}
	}
	return resp.StatusCode
}

// sseEvent is one Server-Sent Event of a turn.
type sseEvent struct {
	Type string
	Data map[string]any
}

func TestServeSessionWithApproval(t *testing.T) {
	t.Parallel()
	mock := newMock(t,
		func(r *apiRequest) reply {
			if r.Model != "claude-opus-5-5" {
				r.errorf("model = %q", r.Model)
			}
			return toolReply("Creating it.", toolUse("toolu_srv", "write_file", map[string]any{"path": "served.txt", "content": "from serve\n"}))
		},
		func(r *apiRequest) reply {
			expectToolResult(r, "toolu_srv", false, "created served.txt")
			return textReply("Done: served.txt exists.")
		},
	)
	e := newEnv(t, mock, nil)
	srv := e.startServe()

	// Without the token the API refuses.
	resp, err := http.Post(srv.base+"/v1/sessions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("POST /v1/sessions without a token: %d, want 401", resp.StatusCode)
	}

	var sess struct {
		ID   string `json:"id"`
		Mode string `json:"mode"`
	}
	if code := srv.do(t, "POST", "/v1/sessions", map[string]string{"mode": "ask", "title": "e2e"}, &sess); code != http.StatusCreated || sess.ID == "" || sess.Mode != "ask" {
		t.Fatalf("create session: %d %+v", code, sess)
	}

	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.base+"/v1/sessions/"+sess.ID+"/messages", strings.NewReader(`{"text":"create served.txt"}`))
	req.Header.Set("Authorization", "Bearer "+serveToken)
	req.Header.Set("Content-Type", "application/json")
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != http.StatusOK || !strings.HasPrefix(stream.Header.Get("Content-Type"), "text/event-stream") {
		b, _ := io.ReadAll(stream.Body)
		t.Fatalf("messages: %d %s", stream.StatusCode, b)
	}

	var events []sseEvent
	var cur sseEvent
	sc := bufio.NewScanner(stream.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	done := false
	for !done && sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			cur.Type = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &cur.Data); err != nil {
				t.Fatalf("bad event data %q: %v", line, err)
			}
		case line == "" && cur.Type != "":
			events = append(events, cur)
			switch cur.Type {
			case "approval_required":
				if cur.Data["tool"] != "write_file" {
					t.Errorf("approval for %v, want write_file", cur.Data["tool"])
				}
				// Before the answer the file must not exist.
				if e.exists("served.txt") {
					t.Error("the write ran before it was approved")
				}
				id, _ := cur.Data["request_id"].(string)
				var ans map[string]any
				if code := srv.do(t, "POST", "/v1/sessions/"+sess.ID+"/approvals/"+id, map[string]string{"decision": "allow_once"}, &ans); code != http.StatusOK || ans["decision"] != "allow_once" {
					t.Fatalf("approval: %d %v", code, ans)
				}
			case "done":
				done = true
			}
			cur = sseEvent{}
		}
	}
	if !done {
		t.Fatalf("stream ended without done (err %v); events: %v\nstderr:\n%s", sc.Err(), types(events), srv.stderr.String())
	}

	want := []string{"turn_start", "tool_call", "approval_required", "approval_resolved", "tool_start", "tool_done", "usage", "done"}
	if got := types(events); !subsequence(got, want) {
		t.Errorf("events %v do not contain %v in order", got, want)
	}
	last := events[len(events)-1].Data
	if last["stop_reason"] != "end_turn" || last["text"] != "Done: served.txt exists." {
		t.Errorf("done event = %v", last)
	}
	if b, err := os.ReadFile(filepath.Join(e.root, "served.txt")); err != nil || string(b) != "from serve\n" {
		t.Errorf("served.txt = %q, %v", b, err)
	}

	var info struct {
		Turns    int `json:"turns"`
		Messages int `json:"messages"`
		Busy     bool
		Usage    struct {
			InputTokens int64 `json:"input_tokens"`
		} `json:"usage"`
	}
	if code := srv.do(t, "GET", "/v1/sessions/"+sess.ID, nil, &info); code != http.StatusOK {
		t.Fatalf("get session: %d", code)
	}
	if info.Turns != 1 || info.Messages != 4 || info.Busy || info.Usage.InputTokens != 2*mockInputTokens {
		t.Errorf("session info = %+v", info)
	}
	if n := len(mock.Requests()); n != 2 {
		t.Errorf("mock got %d requests, want 2", n)
	}
}

func types(evs []sseEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}
	return out
}

// subsequence reports whether want appears in got in order.
func subsequence(got, want []string) bool {
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	return i == len(want)
}
