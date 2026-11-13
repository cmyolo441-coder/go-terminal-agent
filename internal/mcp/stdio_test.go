package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testClientOptions(t *testing.T) ClientOptions {
	return ClientOptions{
		Logf:          func(format string, args ...any) { t.Logf(format, args...) },
		shutdownGrace: 200 * time.Millisecond,
	}
}

func connectStdio(t *testing.T, mode string, env map[string]string, opts ClientOptions) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Connect(ctx, "fake", stdioConfig(mode, env), opts)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func callText(t *testing.T, c *Client, tool string, args string) *ToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, tool, json.RawMessage(args))
	if err != nil {
		t.Fatalf("CallTool(%s): %v", tool, err)
	}
	return res
}

func TestStdioLifecycleAndTools(t *testing.T) {
	opts := testClientOptions(t)
	opts.Getenv = func(name string) (string, bool) {
		if name == "TOKEN_SOURCE" {
			return "s3cr3t", true
		}
		return "", false
	}
	cfg := stdioConfig("ok", map[string]string{"FAKE_TOKEN": "${TOKEN_SOURCE}", "FAKE_DEFAULT": "${MISSING:-fallback}"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Connect(ctx, "fake", cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	info := c.ServerInfo()
	if info.ServerInfo.Name != "fake" || info.ProtocolVersion != ProtocolVersion {
		t.Fatalf("server info = %+v", info)
	}
	// 8 tools served 3 per page: pagination must collect all of them.
	if got := len(c.Tools()); got != 8 {
		t.Fatalf("got %d tools, want 8: %+v", got, c.Tools())
	}
	if res := callText(t, c, "echo", `{"text":"hi"}`); res.Text() != "hi" || res.IsError {
		t.Fatalf("echo = %+v", res)
	}
	if res := callText(t, c, "add", `{"a":2,"b":3.5}`); res.Text() != "5.5" {
		t.Fatalf("add = %q", res.Text())
	}
	if res := callText(t, c, "fail", ``); !res.IsError || res.Text() != "something broke" {
		t.Fatalf("fail = %+v", res)
	}
	media := callText(t, c, "media", `{}`).Text()
	for _, want := range []string{
		"chart follows",
		"[image: image/png, 10 bytes]",
		"[resource: file:///notes.txt]\nnote body",
		"[resource: file:///bin.dat, application/octet-stream, 3 bytes binary]",
		"[resource link: doc <https://example.com/doc>]",
	} {
		if !strings.Contains(media, want) {
			t.Errorf("media output missing %q:\n%s", want, media)
		}
	}
	if got := callText(t, c, "getenv", `{"name":"FAKE_TOKEN"}`).Text(); got != "s3cr3t" {
		t.Errorf("expanded env = %q", got)
	}
	if got := callText(t, c, "getenv", `{"name":"FAKE_DEFAULT"}`).Text(); got != "fallback" {
		t.Errorf("default env = %q", got)
	}
	if err := c.Ping(ctx); err != nil {
		t.Errorf("ping: %v", err)
	}
	var rpc *RPCError
	if _, err := c.CallTool(ctx, "nope", nil); !errors.As(err, &rpc) || rpc.Code != CodeInvalidParams {
		t.Errorf("unknown tool err = %v", err)
	}
	if !strings.Contains(c.Stderr(), "fake server ready") {
		t.Errorf("stderr not captured: %q", c.Stderr())
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if c.Alive() {
		t.Fatal("client alive after Close")
	}
	if _, err := c.CallTool(ctx, "echo", json.RawMessage(`{"text":"x"}`)); !errors.Is(err, ErrClosed) {
		t.Fatalf("call after close = %v", err)
	}
}

func TestStdioMissingVariable(t *testing.T) {
	cfg := stdioConfig("ok", map[string]string{"X": "${GTA_TEST_SURELY_UNSET_VAR}"})
	_, err := Connect(context.Background(), "fake", cfg, testClientOptions(t))
	if err == nil || !strings.Contains(err.Error(), "GTA_TEST_SURELY_UNSET_VAR is not set") {
		t.Fatalf("err = %v", err)
	}
}

func TestStdioConcurrentRequests(t *testing.T) {
	c := connectStdio(t, "ok", nil, testClientOptions(t))
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	// A slow call must not hold up the others.
	wg.Add(1)
	go func() {
		defer wg.Done()
		res, err := c.CallTool(context.Background(), "slow", json.RawMessage(`{"ms":300}`))
		if err != nil || res.Text() != "done" {
			errs <- fmt.Errorf("slow: %v %v", res, err)
		}
	}()
	start := time.Now()
	for i := range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			want := fmt.Sprintf("msg-%d", i)
			res, err := c.CallTool(context.Background(), "echo", json.RawMessage(fmt.Sprintf(`{"text":%q}`, want)))
			if err != nil {
				errs <- err
				return
			}
			if res.Text() != want {
				errs <- fmt.Errorf("got %q want %q", res.Text(), want)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("concurrent calls took %s", time.Since(start))
	}
}

func TestStdioCancellationNotifiesServer(t *testing.T) {
	c := connectStdio(t, "ok", nil, testClientOptions(t))
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, err := c.CallTool(ctx, "slow", json.RawMessage(`{"ms":5000}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("cancellation took %s", d)
	}
	waitFor(t, func() bool { return callText(t, c, "cancelled", `{}`).Text() != "" })
}

func TestStdioRequestTimeout(t *testing.T) {
	c := connectStdio(t, "ok", nil, testClientOptions(t))
	c.conn.timeout = 150 * time.Millisecond
	_, err := c.CallTool(context.Background(), "slow", json.RawMessage(`{"ms":5000}`))
	if !errors.Is(err, ErrTimeout) || !strings.Contains(err.Error(), "tools/call got no response within 150ms") {
		t.Fatalf("err = %v", err)
	}
	c.conn.timeout = 5 * time.Second
	waitFor(t, func() bool { return callText(t, c, "cancelled", `{}`).Text() != "" })
	// The connection stays healthy after a timeout.
	if res := callText(t, c, "echo", `{"text":"still here"}`); res.Text() != "still here" {
		t.Fatalf("echo after timeout = %q", res.Text())
	}
}

func TestStdioParentDeadlineIsTimeout(t *testing.T) {
	c := connectStdio(t, "ok", nil, testClientOptions(t))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := c.CallTool(ctx, "slow", json.RawMessage(`{"ms":5000}`))
	if !errors.Is(err, ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestStdioServerRequestsGetAnswers(t *testing.T) {
	c := connectStdio(t, "ok", nil, testClientOptions(t))
	got := callText(t, c, "ask_client", `{}`).Text()
	if got != fmt.Sprintf("roots/list:%d ping:ok", CodeMethodNotFound) {
		t.Fatalf("ask_client = %q", got)
	}
}

func TestStdioToolsListChanged(t *testing.T) {
	changed := make(chan []ToolInfo, 4)
	opts := testClientOptions(t)
	opts.OnToolsChanged = func(ts []ToolInfo) { changed <- ts }
	c := connectStdio(t, "ok", nil, opts)
	callText(t, c, "change", `{}`)
	select {
	case ts := <-changed:
		if len(ts) != 9 || ts[8].Name != "extra" {
			t.Fatalf("refreshed tools = %+v", ts)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no refresh after list_changed")
	}
	if len(c.Tools()) != 9 {
		t.Fatalf("cached tools = %d", len(c.Tools()))
	}
	if got := callText(t, c, "extra", `{}`).Text(); got != "extra ran" {
		t.Fatalf("extra = %q", got)
	}
}

func TestStdioCrashFailsPendingPromptly(t *testing.T) {
	c := connectStdio(t, "ok", nil, testClientOptions(t))
	slowErr := make(chan error, 1)
	go func() {
		_, err := c.CallTool(context.Background(), "slow", json.RawMessage(`{"ms":20000}`))
		slowErr <- err
	}()
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	_, err := c.CallTool(context.Background(), "crash", nil)
	if err == nil || !strings.Contains(err.Error(), "exit status 3") || !strings.Contains(err.Error(), "fatal: boom") {
		t.Fatalf("crash err = %v", err)
	}
	select {
	case err := <-slowErr:
		if err == nil || !strings.Contains(err.Error(), "fatal: boom") {
			t.Fatalf("pending call err = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pending call not failed after crash")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("crash detection too slow")
	}
	if c.Alive() {
		t.Fatal("client alive after crash")
	}
	if err := c.Err(); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Err() = %v", err)
	}
	select {
	case <-c.Done():
	default:
		t.Fatal("Done not closed")
	}
}

func TestStdioStartFailures(t *testing.T) {
	_, err := Connect(context.Background(), "fake", stdioConfig("crash_start", nil), testClientOptions(t))
	if err == nil || !strings.Contains(err.Error(), "cannot start: missing token") || !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("crash_start err = %v", err)
	}
	_, err = Connect(context.Background(), "fake", ServerConfig{Command: "/nonexistent/gta-mcp-server"}, testClientOptions(t))
	if err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("missing binary err = %v", err)
	}
	_, err = Connect(context.Background(), "fake", stdioConfig("old_version", nil), testClientOptions(t))
	if err == nil || !strings.Contains(err.Error(), "unsupported protocol version") {
		t.Fatalf("old_version err = %v", err)
	}
}

func TestStdioInitializeHangHonoursContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Connect(ctx, "fake", stdioConfig("hang_init", nil), testClientOptions(t))
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %s", d)
	}
}

func TestStdioNoisyStdoutIsTolerated(t *testing.T) {
	c := connectStdio(t, "noisy", nil, testClientOptions(t))
	if got := callText(t, c, "echo", `{"text":"ok"}`).Text(); got != "ok" {
		t.Fatalf("echo = %q", got)
	}
	if !strings.Contains(c.Stderr(), "[stdout] this is not JSON") {
		t.Fatalf("stray stdout not recorded: %q", c.Stderr())
	}
}

func TestStdioShutdownKillsStubbornServer(t *testing.T) {
	opts := testClientOptions(t)
	opts.shutdownGrace = 100 * time.Millisecond
	c := connectStdio(t, "stubborn", nil, opts)
	start := time.Now()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Close took %s", d)
	}
	if st := c.stdio.cmd.ProcessState; st == nil || st.Success() {
		t.Fatalf("process state = %v", st)
	}
}

func TestStdioWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	opts := testClientOptions(t)
	opts.Dir = dir
	c := connectStdio(t, "ok", nil, opts)
	got := callText(t, c, "cwd", `{}`).Text()
	want, _ := filepath.EvalSymlinks(dir)
	if gotReal, _ := filepath.EvalSymlinks(got); gotReal != want {
		t.Fatalf("server cwd = %q, want %q", got, dir)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met within 5s")
}
