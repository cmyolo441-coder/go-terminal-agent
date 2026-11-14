package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Integration tests: they launch a real headless Chromium and skip when
// none is installed.

const formPage = `<!doctype html>
<html><head><title>Test Form</title>
<style>
  body { margin: 0; height: 3000px; font: 16px sans-serif; }
  #name { position: absolute; left: 20px; top: 20px; width: 240px; height: 30px; }
  #btn  { position: absolute; left: 20px; top: 80px; width: 120px; height: 40px; }
  #ctx  { position: absolute; left: 200px; top: 80px; width: 120px; height: 40px; }
</style></head>
<body>
<form id="f" action="/submit" method="GET">
  <input id="name" name="name" autocomplete="off">
  <button id="btn" type="button">Click me</button>
</form>
<div id="ctx">right-click</div>
<script>
  window.clicks = 0;
  window.keys = [];
  document.getElementById('btn').addEventListener('click', e => {
    window.clicks++;
    e.target.textContent = 'clicked ' + window.clicks;
  });
  document.getElementById('ctx').addEventListener('contextmenu', e => {
    e.preventDefault();
    window.rightClicked = e.button;
  });
  document.addEventListener('keydown', e => {
    window.keys.push({key: e.key, code: e.code, keyCode: e.keyCode, shift: e.shiftKey, ctrl: e.ctrlKey});
  });
</script>
</body></html>`

type testSite struct {
	*httptest.Server
	slowHits atomic.Int64
}

func newTestSite(t *testing.T) *testSite {
	t.Helper()
	s := &testSite{}
	mux := http.NewServeMux()
	mux.HandleFunc("/form", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, formPage)
	})
	mux.HandleFunc("/submit", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		name := html.EscapeString(r.URL.Query().Get("name"))
		fmt.Fprintf(w, "<!doctype html><title>Submitted: %s</title><p id=out>%s</p>", name, name)
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(400 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		s.slowHits.Add(1)
		fmt.Fprint(w, "ok")
	})
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><title>R</title><script>location.replace('/submit?name=redirected')</script>`)
	})
	mux.HandleFunc("/anim", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><title>Anim</title><div id=d style="width:100px;height:100px;background:red"></div>
<script>let n=0;setInterval(()=>{n++;document.getElementById('d').style.background=n%2?'blue':'green';document.getElementById('d').style.marginLeft=(n%50)+'px'},30)</script>`)
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func launchForTest(t *testing.T, opts Options) *Browser {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping browser integration test in -short mode")
	}
	if opts.ExecPath == "" {
		p, err := FindExecutable()
		if err != nil {
			t.Skipf("no browser available: %v", err)
		}
		opts.ExecPath = p
	}
	opts.Headless = true
	if opts.Logf == nil {
		opts.Logf = func(format string, args ...any) {
			if testing.Verbose() {
				t.Logf(format, args...)
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	b, err := Launch(ctx, opts)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

func testCtx(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func evalInto(t *testing.T, ctx context.Context, p *Page, expr string, v any) {
	t.Helper()
	raw, err := p.Evaluate(ctx, expr)
	if err != nil {
		t.Fatalf("Evaluate(%q): %v", expr, err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("Evaluate(%q) = %s: %v", expr, raw, err)
	}
}

// waitFor polls cond until it is true or the deadline passes.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

type rect struct{ X, Y, W, H float64 }

func boundingBox(t *testing.T, ctx context.Context, p *Page, selector string) rect {
	t.Helper()
	var r rect
	evalInto(t, ctx, p, fmt.Sprintf(`(() => { const r = document.querySelector(%q).getBoundingClientRect(); return {X: r.x, Y: r.y, W: r.width, H: r.height}; })()`, selector), &r)
	if r.W == 0 || r.H == 0 {
		t.Fatalf("%s has an empty bounding box", selector)
	}
	return r
}

func click(t *testing.T, ctx context.Context, p *Page, selector, button string) {
	t.Helper()
	r := boundingBox(t, ctx, p, selector)
	x, y := r.X+r.W/2, r.Y+r.H/2
	if err := p.MouseMove(ctx, x, y); err != nil {
		t.Fatal(err)
	}
	if err := p.MouseDown(ctx, x, y, button, 1); err != nil {
		t.Fatal(err)
	}
	if err := p.MouseUp(ctx, x, y, button, 1); err != nil {
		t.Fatal(err)
	}
}

func press(t *testing.T, ctx context.Context, p *Page, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if err := p.KeyDown(ctx, k); err != nil {
			t.Fatalf("KeyDown(%q): %v", k, err)
		}
		if err := p.KeyUp(ctx, k); err != nil {
			t.Fatalf("KeyUp(%q): %v", k, err)
		}
	}
}

func inputValue(t *testing.T, ctx context.Context, p *Page) string {
	t.Helper()
	var v string
	evalInto(t, ctx, p, `document.getElementById('name').value`, &v)
	return v
}

func TestBrowserIntegration(t *testing.T) {
	site := newTestSite(t)
	b := launchForTest(t, Options{Width: 1024, Height: 700})
	ctx := testCtx(t, 90*time.Second)

	p, err := b.NewPage(ctx)
	if err != nil {
		t.Fatalf("NewPage: %v", err)
	}
	if got := b.Pages(); len(got) != 1 || got[0] != p {
		t.Fatalf("Pages() = %v", got)
	}
	if w, h := p.Viewport(); w != 1024 || h != 700 {
		t.Fatalf("Viewport = %dx%d", w, h)
	}

	formURL := site.URL + "/form"
	if err := p.Navigate(ctx, formURL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	if p.URL() != formURL {
		t.Fatalf("URL() = %q, want %q", p.URL(), formURL)
	}
	if title, err := p.Title(ctx); err != nil || title != "Test Form" {
		t.Fatalf("Title = %q, %v", title, err)
	}

	t.Run("Evaluate", func(t *testing.T) {
		cases := map[string]string{
			`1 + 2`:                                `3`,
			`"he" + "llo"`:                         `"hello"`,
			`Promise.resolve({a: [1, "x", true]})`: `{"a":[1,"x",true]}`,
			`new Promise(r => setTimeout(() => r(42), 50))`: `42`,
			`undefined`:  `null`,
			`null`:       `null`,
			`NaN`:        `"NaN"`,
			`-Infinity`:  `"-Infinity"`,
			`innerWidth`: `1024`,
		}
		for expr, want := range cases {
			raw, err := p.Evaluate(ctx, expr)
			if err != nil {
				t.Errorf("Evaluate(%q): %v", expr, err)
				continue
			}
			if string(raw) != want {
				t.Errorf("Evaluate(%q) = %s, want %s", expr, raw, want)
			}
		}
		_, err := p.Evaluate(ctx, `throw new Error("boom")`)
		var ee *EvalError
		if !errors.As(err, &ee) || !strings.Contains(ee.Message, "boom") {
			t.Fatalf("thrown error: %v", err)
		}
		_, err = p.Evaluate(ctx, `throw "plain"`)
		if !errors.As(err, &ee) || !strings.Contains(ee.Message, "plain") {
			t.Fatalf("thrown string: %v", err)
		}
		_, err = p.Evaluate(ctx, `Promise.reject(new TypeError("nope"))`)
		if !errors.As(err, &ee) || !strings.Contains(ee.Message, "nope") {
			t.Fatalf("rejected promise: %v", err)
		}
		_, err = p.Evaluate(ctx, `this is not javascript`)
		if !errors.As(err, &ee) || !strings.Contains(ee.Message, "SyntaxError") {
			t.Fatalf("syntax error: %v", err)
		}
	})

	t.Run("EvaluateContextCancel", func(t *testing.T) {
		cctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := p.Evaluate(cctx, `new Promise(() => {})`)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v", err)
		}
		if time.Since(start) > 2*time.Second {
			t.Fatal("cancel not honored")
		}
		var n int
		evalInto(t, ctx, p, `7*6`, &n)
		if n != 42 {
			t.Fatalf("page unusable after cancel: %d", n)
		}
	})

	t.Run("ClickByCoordinates", func(t *testing.T) {
		click(t, ctx, p, "#btn", "left")
		var clicks int
		evalInto(t, ctx, p, `window.clicks`, &clicks)
		if clicks != 1 {
			t.Fatalf("clicks = %d", clicks)
		}
		var text string
		evalInto(t, ctx, p, `document.getElementById('btn').textContent`, &text)
		if text != "clicked 1" {
			t.Fatalf("button text %q", text)
		}
		click(t, ctx, p, "#ctx", "right")
		var button int
		evalInto(t, ctx, p, `window.rightClicked`, &button)
		if button != 2 {
			t.Fatalf("contextmenu button = %d", button)
		}
	})

	t.Run("TypeIntoInput", func(t *testing.T) {
		click(t, ctx, p, "#name", "left")
		var focused string
		evalInto(t, ctx, p, `document.activeElement.id`, &focused)
		if focused != "name" {
			t.Fatalf("focused element %q", focused)
		}
		press(t, ctx, p, "h", "i", "x")
		if err := p.KeyDown(ctx, "Shift"); err != nil {
			t.Fatal(err)
		}
		if p.Modifiers() != ModShift {
			t.Fatalf("modifiers = %d", p.Modifiers())
		}
		press(t, ctx, p, "a", "1")
		if err := p.KeyUp(ctx, "Shift"); err != nil {
			t.Fatal(err)
		}
		if got := inputValue(t, ctx, p); got != "hixA!" {
			t.Fatalf("value after typing = %q", got)
		}
		press(t, ctx, p, "Backspace", "Backspace", "ArrowLeft", "Backspace", "ArrowRight")
		if got := inputValue(t, ctx, p); got != "hx" {
			t.Fatalf("value after editing = %q", got)
		}
		// Select all with Control+a, then type over it.
		if err := p.KeyDown(ctx, "Control"); err != nil {
			t.Fatal(err)
		}
		press(t, ctx, p, "a")
		if err := p.KeyUp(ctx, "Control"); err != nil {
			t.Fatal(err)
		}
		if got := inputValue(t, ctx, p); got != "hx" {
			t.Fatalf("Control+a typed text: %q", got)
		}
		press(t, ctx, p, "KeyH", "i")
		if err := p.InsertText(ctx, " thére"); err != nil {
			t.Fatal(err)
		}
		if got := inputValue(t, ctx, p); got != "hi thére" {
			t.Fatalf("value = %q", got)
		}

		var keys []struct {
			Key     string
			Code    string
			KeyCode int
			Shift   bool
			Ctrl    bool
		}
		evalInto(t, ctx, p, `window.keys`, &keys)
		var sawShiftA, sawBackspace, sawCtrlA bool
		for _, k := range keys {
			switch {
			case k.Key == "A" && k.Code == "KeyA" && k.KeyCode == 65 && k.Shift:
				sawShiftA = true
			case k.Key == "Backspace" && k.Code == "Backspace" && k.KeyCode == 8:
				sawBackspace = true
			case k.Key == "a" && k.Ctrl:
				sawCtrlA = true
			}
		}
		if !sawShiftA || !sawBackspace || !sawCtrlA {
			t.Fatalf("key events not as expected: %+v", keys)
		}
	})

	t.Run("EnterSubmits", func(t *testing.T) {
		w := p.watchNavigation()
		defer w.stop()
		press(t, ctx, p, "Enter")
		if err := w.wait(ctx, "submit", (*navWatcher).anyCommittedLoaded); err != nil {
			t.Fatal(err)
		}
		title, err := p.Title(ctx)
		if err != nil || title != "Submitted: hi thére" {
			t.Fatalf("title after submit = %q, %v", title, err)
		}
		if !strings.Contains(p.URL(), "/submit?name=hi+th") {
			t.Fatalf("URL after submit = %q", p.URL())
		}
	})

	t.Run("History", func(t *testing.T) {
		if err := p.Back(ctx); err != nil {
			t.Fatalf("Back: %v", err)
		}
		if p.URL() != formURL {
			t.Fatalf("URL after Back = %q", p.URL())
		}
		if err := p.Forward(ctx); err != nil {
			t.Fatalf("Forward: %v", err)
		}
		if !strings.Contains(p.URL(), "/submit") {
			t.Fatalf("URL after Forward = %q", p.URL())
		}
		if err := p.Forward(ctx); !errors.Is(err, ErrNoHistory) {
			t.Fatalf("Forward at end: %v", err)
		}
		// Same-document navigation records history too.
		if err := p.Navigate(ctx, formURL+"#frag"); err != nil {
			t.Fatal(err)
		}
		if err := p.Navigate(ctx, formURL+"#frag2"); err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(p.URL(), "#frag2") {
			t.Fatalf("URL = %q", p.URL())
		}
		if err := p.Back(ctx); err != nil {
			t.Fatalf("Back within document: %v", err)
		}
		if !strings.HasSuffix(p.URL(), "#frag") {
			t.Fatalf("URL after same-document Back = %q", p.URL())
		}
	})

	t.Run("Reload", func(t *testing.T) {
		if _, err := p.Evaluate(ctx, `window.marker = 1`); err != nil {
			t.Fatal(err)
		}
		if err := p.Reload(ctx); err != nil {
			t.Fatalf("Reload: %v", err)
		}
		raw, err := p.Evaluate(ctx, `typeof window.marker`)
		if err != nil || string(raw) != `"undefined"` {
			t.Fatalf("marker survived reload: %s %v", raw, err)
		}
	})

	t.Run("ScriptRedirect", func(t *testing.T) {
		if err := p.Navigate(ctx, site.URL+"/redirect"); err != nil {
			t.Fatal(err)
		}
		waitFor(t, 10*time.Second, "redirect", func() bool { return strings.Contains(p.URL(), "name=redirected") })
	})

	t.Run("NavigateError", func(t *testing.T) {
		dead := httptest.NewServer(http.NotFoundHandler())
		addr := dead.URL
		dead.Close()
		err := p.Navigate(ctx, addr+"/gone")
		if err == nil || !strings.Contains(err.Error(), "ERR_") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("ScreenshotAndViewport", func(t *testing.T) {
		if err := p.Navigate(ctx, formURL); err != nil {
			t.Fatal(err)
		}
		shot, err := p.Screenshot(ctx, "png", 0)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(shot))
		if err != nil {
			t.Fatalf("not a PNG: %v", err)
		}
		if cfg.Width != 1024 || cfg.Height != 700 {
			t.Fatalf("png is %dx%d", cfg.Width, cfg.Height)
		}
		if err := p.SetViewport(ctx, 640, 480); err != nil {
			t.Fatal(err)
		}
		if w, h := p.Viewport(); w != 640 || h != 480 {
			t.Fatalf("Viewport = %dx%d", w, h)
		}
		shot, err = p.Screenshot(ctx, "jpg", 70)
		if err != nil {
			t.Fatal(err)
		}
		img, err := jpeg.Decode(bytes.NewReader(shot))
		if err != nil {
			t.Fatalf("not a JPEG: %v", err)
		}
		if b := img.Bounds(); b.Dx() != 640 || b.Dy() != 480 {
			t.Fatalf("jpeg is %v", b)
		}
		var iw int
		evalInto(t, ctx, p, `innerWidth`, &iw)
		if iw != 640 {
			t.Fatalf("innerWidth = %d", iw)
		}
		if _, err := p.Screenshot(ctx, "gif", 0); err == nil {
			t.Fatal("gif accepted")
		}
		if err := p.SetViewport(ctx, 0, 10); err == nil {
			t.Fatal("zero viewport accepted")
		}
	})

	t.Run("Wheel", func(t *testing.T) {
		if err := p.Wheel(ctx, 100, 100, 0, 600); err != nil {
			t.Fatal(err)
		}
		waitFor(t, 5*time.Second, "scroll", func() bool {
			var y float64
			evalInto(t, ctx, p, `scrollY`, &y)
			return y > 0
		})
	})

	t.Run("WaitIdle", func(t *testing.T) {
		before := site.slowHits.Load()
		if _, err := p.Evaluate(ctx, `fetch('/slow').then(r => r.text()); fetch('/slow'); 1`); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		if err := p.WaitIdle(ctx, 150*time.Millisecond); err != nil {
			t.Fatal(err)
		}
		if got := site.slowHits.Load() - before; got != 2 {
			t.Fatalf("WaitIdle returned with %d of 2 slow requests done after %v", got, time.Since(start))
		}
		// An idle page returns after the quiet period.
		start = time.Now()
		if err := p.WaitIdle(ctx, 100*time.Millisecond); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("idle wait took %v", d)
		}
		// A request that never finishes times out.
		if _, err := p.Evaluate(ctx, `fetch('/slow'); window.__s = new EventSource('/slow'); 1`); err != nil {
			t.Fatal(err)
		}
		short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		if err := p.WaitIdle(short, time.Second); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("busy page: %v", err)
		}
	})

	t.Run("Screencast", func(t *testing.T) {
		if err := p.Navigate(ctx, site.URL+"/anim"); err != nil {
			t.Fatal(err)
		}
		frames, stop, err := p.StartScreencast(ctx, ScreencastOptions{Format: "jpeg", Quality: 60, MaxWidth: 320, MaxHeight: 240})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := p.StartScreencast(ctx, ScreencastOptions{}); err == nil {
			t.Fatal("second screencast started")
		}
		var got []Frame
		timeout := time.After(15 * time.Second)
	collect:
		for len(got) < 3 {
			select {
			case f, ok := <-frames:
				if !ok {
					break collect
				}
				got = append(got, f)
			case <-timeout:
				break collect
			}
		}
		stop()
		stop() // idempotent
		if len(got) == 0 {
			t.Fatal("no screencast frames")
		}
		for i, f := range got {
			if _, err := jpeg.DecodeConfig(bytes.NewReader(f.Data)); err != nil {
				t.Fatalf("frame %d not JPEG: %v", i, err)
			}
			if f.Format != "jpeg" || f.Width <= 0 || f.Width > 320 || f.Height <= 0 || f.Height > 240 {
				t.Fatalf("frame %d: format %s %dx%d", i, f.Format, f.Width, f.Height)
			}
			if f.Timestamp.IsZero() || time.Since(f.Timestamp) > time.Minute {
				t.Fatalf("frame %d timestamp %v", i, f.Timestamp)
			}
			if i > 0 && f.Seq <= got[i-1].Seq {
				t.Fatalf("sequence not increasing: %d then %d", got[i-1].Seq, f.Seq)
			}
		}
		// Channel closes after stop.
		for range frames {
		}
		// A new screencast can start once the old one stopped.
		frames2, stop2, err := p.StartScreencast(ctx, ScreencastOptions{Format: "png", MaxWidth: 200})
		if err != nil {
			t.Fatal(err)
		}
		select {
		case f := <-frames2:
			if _, _, err := image.DecodeConfig(bytes.NewReader(f.Data)); err != nil || f.Format != "png" {
				t.Fatalf("png frame: %v %s", err, f.Format)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("no png frame")
		}
		stop2()
	})

	t.Run("SlowConsumerDropsOldest", func(t *testing.T) {
		frames, stop, err := p.StartScreencast(ctx, ScreencastOptions{MaxWidth: 160})
		if err != nil {
			t.Fatal(err)
		}
		defer stop()
		// Do not read for a while: the browser must keep producing
		// (frames are acked) and the reader must not block.
		time.Sleep(1500 * time.Millisecond)
		var n int
		evalInto(t, ctx, p, `1+1`, &n)
		if n != 2 {
			t.Fatal("connection blocked by screencast")
		}
		var last uint64
		deadline := time.After(10 * time.Second)
		for last <= screencastBuffer+1 {
			select {
			case f := <-frames:
				last = f.Seq
			case <-deadline:
				t.Fatalf("frames stopped at seq %d", last)
			}
		}
	})

	t.Run("ClosePage", func(t *testing.T) {
		p2, err := b.NewPage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(b.Pages()) != 2 {
			t.Fatalf("Pages() has %d", len(b.Pages()))
		}
		frames, _, err := p2.StartScreencast(ctx, ScreencastOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := p2.Close(); err != nil {
			t.Fatal(err)
		}
		if err := p2.Close(); err != nil {
			t.Fatalf("second Close: %v", err)
		}
		for range frames {
		}
		if _, err := p2.Evaluate(ctx, "1"); !errors.Is(err, ErrPageClosed) {
			t.Fatalf("Evaluate on closed page: %v", err)
		}
		if got := b.Pages(); len(got) != 1 || got[0] != p {
			t.Fatalf("Pages() after close = %v", got)
		}
	})
}

func TestBrowserCloseLeavesNothing(t *testing.T) {
	b := launchForTest(t, Options{})
	ctx := testCtx(t, 60*time.Second)
	p, err := b.NewPage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Navigate(ctx, "data:text/html,<title>x</title>"); err != nil {
		t.Fatal(err)
	}
	pid := b.cmd.Process.Pid
	dir := b.tempDir
	if dir == "" {
		t.Fatal("no temp profile dir")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
	if !processGroupAlive(pid) {
		t.Fatal("process group not running")
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if processGroupAlive(pid) {
		t.Fatalf("processes remain in group %d", pid)
	}
	select {
	case <-b.exited:
	default:
		t.Fatal("browser process not reaped")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("profile dir still exists: %v", err)
	}
	if _, err := p.Evaluate(ctx, "1"); err == nil {
		t.Fatal("Evaluate after browser Close succeeded")
	}
	if _, err := b.NewPage(ctx); !errors.Is(err, ErrClosed) {
		t.Fatalf("NewPage after Close: %v", err)
	}
	if len(b.Pages()) != 0 {
		t.Fatal("pages remain after Close")
	}
}

func TestBrowserPipeTransport(t *testing.T) {
	b := launchForTest(t, Options{ExtraArgs: []string{"--remote-debugging-pipe"}})
	if b.wsURL != "" {
		t.Fatalf("pipe mode used a WebSocket: %s", b.wsURL)
	}
	ctx := testCtx(t, 60*time.Second)
	p, err := b.NewPage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Navigate(ctx, "data:text/html,<title>piped</title>"); err != nil {
		t.Fatal(err)
	}
	title, err := p.Title(ctx)
	if err != nil || title != "piped" {
		t.Fatalf("title %q %v", title, err)
	}
	// A large result crosses many pipe buffer boundaries.
	raw, err := p.Evaluate(ctx, `"x".repeat(3 * 1024 * 1024)`)
	if err != nil || len(raw) != 3*1024*1024+2 {
		t.Fatalf("large evaluate: len %d, %v", len(raw), err)
	}
	pid := b.cmd.Process.Pid
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if processGroupAlive(pid) {
		t.Fatal("processes remain after Close")
	}
}

func TestBrowserProcessDeath(t *testing.T) {
	b := launchForTest(t, Options{})
	ctx := testCtx(t, 60*time.Second)
	p, err := b.NewPage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := killProcessTree(b.cmd.Process); err != nil {
		t.Fatal(err)
	}
	select {
	case <-b.conn.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("connection did not notice the browser dying")
	}
	if _, err := p.Evaluate(ctx, "1"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Evaluate after death: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close after death: %v", err)
	}
}

func TestLaunchFailures(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses shell scripts as fake browsers")
	}
	ctx := testCtx(t, 30*time.Second)
	dir := t.TempDir()
	// Profiles go to a private TMPDIR so leftovers are detectable.
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	defer func() {
		if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
			t.Errorf("failed launches left %d entries in TMPDIR", len(entries))
		}
	}()
	// An executable that exits immediately with a message.
	script := dir + "/fake-chrome"
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'fatal: cannot open display' >&2\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Launch(ctx, Options{ExecPath: script, Headless: true})
	if err == nil || !strings.Contains(err.Error(), "exited before DevTools was ready") || !strings.Contains(err.Error(), "cannot open display") {
		t.Fatalf("err = %v", err)
	}

	// An executable that never prints the DevTools URL: ctx bounds launch.
	hang := dir + "/hang-chrome"
	if err := os.WriteFile(hang, []byte("#!/bin/sh\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = Launch(short, Options{ExecPath: hang, Headless: true})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("launch timeout not honored")
	}

	if _, err := Launch(ctx, Options{ExecPath: dir + "/missing", Headless: true}); err == nil {
		t.Fatal("missing executable accepted")
	}
}
