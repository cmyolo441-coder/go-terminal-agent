package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/permission"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

func newTestManager(t *testing.T, onChange func()) *Manager {
	m := NewManager(Options{
		Logf:          func(format string, args ...any) { t.Logf(format, args...) },
		OnChange:      onChange,
		shutdownGrace: 200 * time.Millisecond,
	})
	t.Cleanup(func() { m.Close() })
	return m
}

func findTool(t *testing.T, ts []tools.Tool, name string) tools.Tool {
	t.Helper()
	for _, tool := range ts {
		if tool.Spec().Name == name {
			return tool
		}
	}
	t.Fatalf("tool %s not found", name)
	return nil
}

func runTool(t *testing.T, tool tools.Tool, input string) (string, error) {
	t.Helper()
	env, err := tools.NewEnv(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	call, err := tool.Prepare(env, json.RawMessage(input))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return call.Run(ctx)
}

func TestManagerStartsServersIndependently(t *testing.T) {
	_, srv := newHTTPFake(t)
	var changes atomic.Int32
	m := newTestManager(t, func() { changes.Add(1) })
	cfg := Config{
		"good":     stdioConfig("ok", nil),
		"broken":   stdioConfig("crash_start", nil),
		"off":      ServerConfig{Command: "/nonexistent", Disabled: true},
		"bad name": ServerConfig{Command: "x"},
		"both":     ServerConfig{Command: "x", URL: "http://example.com"},
		"remote":   ServerConfig{URL: srv.URL},
	}
	if err := m.Start(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background(), cfg); err == nil {
		t.Fatal("second Start succeeded")
	}
	want := map[string]State{"good": StateConnected, "broken": StateFailed, "off": StateDisabled, "bad name": StateFailed, "both": StateFailed, "remote": StateConnected}
	for _, st := range m.Status() {
		if st.State != want[st.Name] {
			t.Errorf("%s: state %s (err %v), want %s", st.Name, st.State, st.Err, want[st.Name])
		}
		switch st.Name {
		case "good":
			if len(st.Tools) != 8 || st.Server.Name != "fake" || st.Transport != "stdio" {
				t.Errorf("good status = %+v", st)
			}
		case "broken":
			if st.Err == nil || !strings.Contains(st.Err.Error(), "missing token") {
				t.Errorf("broken err = %v", st.Err)
			}
		case "remote":
			if st.Transport != "http" || len(st.Tools) != 8 {
				t.Errorf("remote status = %+v", st)
			}
		}
	}
	if changes.Load() == 0 {
		t.Error("OnChange never called")
	}

	ts := m.Tools()
	if len(ts) != 16 {
		t.Fatalf("got %d tools, want 16", len(ts))
	}
	echo := findTool(t, ts, "mcp__good__echo")
	if echo.Kind() != tools.KindExec {
		t.Fatal("MCP tools must be KindExec")
	}
	spec := echo.Spec()
	if spec.Description != "Echo text back." || len(spec.Required) != 1 || spec.Required[0] != "text" || spec.Properties["text"] == nil {
		t.Fatalf("spec = %+v", spec)
	}
	if out, err := runTool(t, echo, `{"text":"via adapter"}`); err != nil || out != "via adapter" {
		t.Fatalf("run = %q, %v", out, err)
	}
	out, err := runTool(t, findTool(t, ts, "mcp__remote__fail"), `{}`)
	if err == nil || out != "something broke" {
		t.Fatalf("isError run = %q, %v", out, err)
	}
	report := FormatStatus(m.Status())
	for _, s := range []string{"✓ good  connected", "✗ broken  failed", "- off  disabled", "8 tools", "missing token"} {
		if !strings.Contains(report, s) {
			t.Errorf("report missing %q:\n%s", s, report)
		}
	}
}

func TestToolPrepareValidatesAndGates(t *testing.T) {
	m := newTestManager(t, nil)
	if err := m.Start(context.Background(), Config{"good": stdioConfig("ok", nil)}); err != nil {
		t.Fatal(err)
	}
	echo := findTool(t, m.Tools(), "mcp__good__echo")
	env, _ := tools.NewEnv(t.TempDir())
	for _, bad := range []string{`[1,2]`, `"text"`, `null`, `{"text":"a"} {}`, `{"other":1}`, `{bad json`} {
		if _, err := echo.Prepare(env, json.RawMessage(bad)); err == nil {
			t.Errorf("Prepare(%s) accepted", bad)
		}
	}
	call, err := echo.Prepare(env, json.RawMessage(`{"text": "hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	if call.Tool != "mcp__good__echo" || call.Kind != tools.KindExec || !strings.Contains(call.Preview, `"text": "hi"`) || !strings.Contains(call.Summary, "good") {
		t.Fatalf("call = %+v", call)
	}
	gate := permission.NewGate(permission.ModeAsk, nil)
	if v, _ := gate.Check(call); v != permission.Ask {
		t.Fatalf("ask mode verdict = %v, want Ask", v)
	}
	gate.Grant(call)
	if v, _ := gate.Check(call); v != permission.Allow {
		t.Fatal("session grant did not apply to the same MCP tool")
	}
	add, _ := findTool(t, m.Tools(), "mcp__good__add").Prepare(env, json.RawMessage(`{"a":1,"b":2}`))
	if v, _ := gate.Check(add); v != permission.Ask {
		t.Fatal("grant for one MCP tool leaked to another")
	}
	if v, _ := permission.NewGate(permission.ModePlan, nil).Check(call); v != permission.Deny {
		t.Fatal("plan mode must refuse MCP tools")
	}
	// Output is truncated to the environment's limit.
	env.OutputLimit = 50
	long, _ := echo.Prepare(env, json.RawMessage(`{"text":"`+strings.Repeat("x", 500)+`"}`))
	out, err := long.Run(context.Background())
	if err != nil || len(out) > 120 || !strings.Contains(out, "characters omitted") {
		t.Fatalf("truncated output = %q, %v", out, err)
	}
}

func TestManagerReconnectsAfterCrash(t *testing.T) {
	m := newTestManager(t, nil)
	if err := m.Start(context.Background(), Config{"good": stdioConfig("ok", nil)}); err != nil {
		t.Fatal(err)
	}
	ts := m.Tools()
	crashCall, _ := NewTool("good", "", ToolInfo{Name: "crash"}, m.servers["good"]).Prepare(nil, nil)
	if _, err := crashCall.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("crash err = %v", err)
	}
	waitFor(t, func() bool { return m.Status()[0].State == StateFailed })
	if st := m.Status()[0]; st.Err == nil || !strings.Contains(st.Err.Error(), "exit status 3") {
		t.Fatalf("status after crash = %+v", st)
	}
	// Tools stay listed and the next call reconnects.
	if len(m.Tools()) != len(ts) {
		t.Fatal("tools disappeared after crash")
	}
	if out, err := runTool(t, findTool(t, ts, "mcp__good__echo"), `{"text":"back"}`); err != nil || out != "back" {
		t.Fatalf("after reconnect = %q, %v", out, err)
	}
	if st := m.Status()[0]; st.State != StateConnected {
		t.Fatalf("state after reconnect = %s", st.State)
	}
	if err := m.Reconnect(context.Background(), "good"); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	if err := m.Reconnect(context.Background(), "nope"); err == nil {
		t.Fatal("Reconnect of unknown server succeeded")
	}
	m.Close()
	if err := m.Reconnect(context.Background(), "good"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Reconnect after Close = %v", err)
	}
	if out, err := runTool(t, findTool(t, ts, "mcp__good__echo"), `{"text":"x"}`); err == nil {
		t.Fatalf("call after Close succeeded: %q", out)
	}
}

func TestManagerCloseAbortsSlowStart(t *testing.T) {
	m := newTestManager(t, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Start(context.Background(), Config{"hang": stdioConfig("hang_init", nil)})
	}()
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	m.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return after Close")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("Close took %s", d)
	}
	if st := m.Status()[0]; st.State != StateFailed || !errors.Is(st.Err, ErrClosed) {
		t.Fatalf("status = %+v", st)
	}
}

func TestManagerListChangedRebuildsTools(t *testing.T) {
	changed := make(chan struct{}, 8)
	m := newTestManager(t, func() { changed <- struct{}{} })
	if err := m.Start(context.Background(), Config{"good": stdioConfig("ok", nil)}); err != nil {
		t.Fatal(err)
	}
	for len(changed) > 0 {
		<-changed
	}
	runTool(t, findTool(t, m.Tools(), "mcp__good__change"), `{}`)
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("OnChange not called after list_changed")
	}
	if out, err := runTool(t, findTool(t, m.Tools(), "mcp__good__extra"), `{}`); err != nil || out != "extra ran" {
		t.Fatalf("extra = %q, %v", out, err)
	}
}
