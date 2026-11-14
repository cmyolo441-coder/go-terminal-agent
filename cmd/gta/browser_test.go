package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/browser"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/browserview"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/config"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

const formPage = `<!doctype html><html><head><title>Signup</title></head><body>
<h1>Create account</h1>
<form id="f" action="/done" method="get">
  <label for="name">Full name</label> <input id="name" name="name" required>
  <label for="email">Email</label> <input id="email" name="email" type="email">
  <label for="plan">Plan</label>
  <select id="plan" name="plan"><option value="free">Free</option><option value="pro">Pro</option></select>
  <label><input id="tos" name="tos" type="checkbox"> I agree</label>
  <button type="submit">Sign up</button>
</form></body></html>`

// TestBrowserEndToEnd drives a real Chromium through the agent's tools,
// exactly as the model would, while the live dock renders the page.
func TestBrowserEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("launches Chromium")
	}
	if _, err := browser.FindExecutable(); err != nil {
		t.Skip("no Chrome/Chromium:", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, formPage) })
	mux.HandleFunc("/done", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		fmt.Fprintf(w, "<!doctype html><title>Done</title><p id=r>welcome %s &lt;%s&gt; plan=%s tos=%s</p>", q.Get("name"), q.Get("email"), q.Get("plan"), q.Get("tos"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	h := newBrowserHost(config.Config{})
	defer h.close()
	env, err := tools.NewEnv(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry(h.tools()...)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// The live view subscribes before the browser exists, like the dock.
	frames, err := h.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}

	run := func(name string, input any) string {
		t.Helper()
		tool, ok := reg.Get(name)
		if !ok {
			t.Fatalf("tool %s not registered", name)
		}
		raw, _ := json.Marshal(input)
		call, err := tool.Prepare(env, raw)
		if err != nil {
			t.Fatalf("%s prepare: %v", name, err)
		}
		out, err := call.Run(ctx)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return out
	}

	run("browser_navigate", map[string]any{"url": srv.URL})
	snap := run("browser_snapshot", map[string]any{})
	for _, want := range []string{`"Full name"`, `"Email"`, `"Sign up"`} {
		if !strings.Contains(snap, want) {
			t.Fatalf("snapshot lacks %s:\n%s", want, snap)
		}
	}

	select {
	case f, ok := <-frames:
		if !ok || len(f.Data) == 0 || f.Width == 0 {
			t.Fatalf("bad first frame: ok=%v %dx%d", ok, f.Width, f.Height)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no screencast frame within 20s")
	}

	run("browser_fill", map[string]any{"fields": []map[string]any{
		{"match_text": "Full name", "value": "Asha Rao"},
		{"selector": "#email", "value": "asha@example.org"},
		{"selector": "#plan", "value": "Pro"},
	}})
	run("browser_check", map[string]any{"selector": "#tos", "checked": true})
	run("browser_click", map[string]any{"match_text": "Sign up"})
	run("browser_wait", map[string]any{"text": "welcome", "timeout_ms": 15000})
	got := run("browser_extract", map[string]any{"selector": "#r"})
	if !strings.Contains(got, "welcome Asha Rao <asha@example.org> plan=pro tos=on") {
		t.Fatalf("form result = %q", got)
	}
	if acts := h.sess.Actions(); len(acts) < 6 {
		t.Fatalf("action log has %d entries", len(acts))
	}
	if o, ok := h.overlay.Peek(); !ok || o.Label == "" {
		t.Fatalf("no overlay published: %+v", o)
	}
}

// TestBrowserDockRendersFrames checks the dock turns a frame into
// half-block cells inside the given box.
func TestBrowserDockRendersFrames(t *testing.T) {
	if testing.Short() {
		t.Skip("launches Chromium")
	}
	if _, err := browser.FindExecutable(); err != nil {
		t.Skip("no Chrome/Chromium:", err)
	}
	h := newBrowserHost(config.Config{})
	defer h.close()
	d := newBrowserDock(h)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := h.sess.Navigate(ctx, "about:blank"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.page.Evaluate(ctx, `document.body.style.background="rgb(200,30,30)"`); err != nil {
		t.Fatal(err)
	}

	// Drive the dock's command loop by hand, as bubbletea would.
	cmds := []tea.Cmd{d.Init()}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		view := d.View(60, 24)
		// Truecolor (48;2;200;30;30) or the 256-colour fallback (48;5;160).
		if strings.Contains(view, "48;2;200;30;30") || strings.Contains(view, "48;5;160") {
			if lines := strings.Split(view, "\n"); len(lines) != 24 {
				t.Fatalf("dock is %d rows, want 24", len(lines))
			}
			return
		}
		next := cmds[:0:0]
		for _, c := range cmds {
			msgs := []tea.Msg{}
			if c != nil {
				done := make(chan tea.Msg, 1)
				go func() { done <- c() }()
				select {
				case m := <-done:
					msgs = append(msgs, m)
				case <-time.After(2 * time.Second):
					next = append(next, func() tea.Msg { return <-done })
					continue
				}
			}
			for len(msgs) > 0 {
				m := msgs[0]
				msgs = msgs[1:]
				if b, ok := m.(tea.BatchMsg); ok {
					next = append(next, b...)
					continue
				}
				if m != nil {
					next = append(next, d.Update(m))
				}
			}
		}
		cmds = next
	}
	t.Fatalf("dock never showed the page:\n%s", d.View(60, 24))
}

var _ browserview.Source = (*browserHost)(nil)
var _ browserview.Input = pageInput{}
