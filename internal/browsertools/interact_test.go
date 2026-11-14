package browsertools

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClickNaturalMotionAndEventOrder(t *testing.T) {
	f := loginPage()
	clicked := 0
	f.find("e6").onClick = func(*fakeDriver) { clicked++ }
	s, clk := newTestSession(f, SpeedNatural)
	var live []Action
	s.OnAction(func(a Action) { live = append(live, a) })
	out, err := s.Click(context.Background(), Target{Ref: "e6"}, ClickOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if clicked != 1 {
		t.Fatalf("button clicked %d times", clicked)
	}
	if !strings.HasPrefix(out, `Clicked [e6] button "Sign in" at (`) {
		t.Fatalf("result = %q", out)
	}
	ev := f.Events()
	// hover (several moves) → down → up, in that order, nothing after up.
	firstDown, moves := -1, 0
	for i, e := range ev {
		if e == "move" {
			if firstDown >= 0 {
				t.Fatalf("pointer moved during the click: %v", ev)
			}
			moves++
		}
		if e == "down left 1" {
			firstDown = i
		}
	}
	if moves < 3 {
		t.Fatalf("natural motion should take several moves, got %d", moves)
	}
	if firstDown < 0 || ev[firstDown+1] != "up left 1" {
		t.Fatalf("event order: %v", ev)
	}
	end := f.moves[len(f.moves)-1]
	if box := f.viewBox(f.find("e6")); !box.Contains(end) {
		t.Fatalf("pointer ended at %v outside %v", end, box)
	}
	if p, ok := s.Cursor(); !ok || p != end {
		t.Fatalf("cursor %v, want %v", p, end)
	}
	if len(clk.Sleeps()) == 0 {
		t.Fatal("natural speed should pause")
	}
	if len(live) != 1 || live[0].Kind != "click" || !live[0].HasPoint || live[0].X != end.X || !strings.Contains(live[0].Target, "e6") {
		t.Fatalf("action log = %+v", live)
	}
}

func TestClickInstantHasNoDelays(t *testing.T) {
	f := loginPage()
	s, clk := newTestSession(f, SpeedInstant)
	if _, err := s.Click(context.Background(), Target{Selector: "button[type=submit]"}, ClickOptions{Count: 2, Modifiers: []string{"Shift"}}); err != nil {
		t.Fatal(err)
	}
	if n := len(clk.Sleeps()); n != 0 {
		t.Fatalf("instant speed slept %d times", n)
	}
	ev := strings.Join(f.Events(), "|")
	want := "move|keydown Shift|down left 1|up left 1|down left 2|up left 2|keyup Shift"
	if !strings.Contains(ev, want) {
		t.Fatalf("events %s\nwant %s", ev, want)
	}
	// The pointer goes to the exact center in instant mode.
	if c := f.viewBox(f.find("e6")).Center(); f.moves[0] != c {
		t.Fatalf("instant click at %v, want center %v", f.moves[0], c)
	}
}

func TestClickDeterministicWithSeed(t *testing.T) {
	paths := make([][]Point, 2)
	for i := range paths {
		f := loginPage()
		s, _ := newTestSession(f, SpeedNatural)
		if _, err := s.Click(context.Background(), Target{Ref: "e6"}, ClickOptions{}); err != nil {
			t.Fatal(err)
		}
		paths[i] = f.moves
	}
	if len(paths[0]) != len(paths[1]) {
		t.Fatal("same seed, different paths")
	}
	for i := range paths[0] {
		if paths[0][i] != paths[1][i] {
			t.Fatalf("same seed differs at %d", i)
		}
	}
}

func TestClickOverlayError(t *testing.T) {
	f := loginPage()
	f.find("e6").blocker = `div#consent.modal "We use cookies" [e40]`
	s, _ := newTestSession(f, SpeedInstant)
	_, err := s.Click(context.Background(), Target{Ref: "e6"}, ClickOptions{})
	var oe *OverlayError
	if !errors.As(err, &oe) {
		t.Fatalf("want OverlayError, got %v", err)
	}
	msg := err.Error()
	for _, want := range []string{`[e6] button "Sign in" is covered`, `div#consent.modal "We use cookies" [e40]`, "Close or deal with the overlay"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q lacks %q", msg, want)
		}
	}
	for _, e := range f.Events() {
		if strings.HasPrefix(e, "down") {
			t.Fatal("must not click through an overlay")
		}
	}
	acts := s.Actions()
	if len(acts) != 1 || acts[0].Err == "" {
		t.Fatalf("failure not logged: %+v", acts)
	}
}

func TestClickErrors(t *testing.T) {
	f := loginPage()
	f.find("e6").disabled = true
	f.add(&fakeEl{ref: "e7", role: "link", name: "Sign in", box: Rect{400, 20, 80, 20}})
	s, _ := newTestSession(f, SpeedInstant)
	ctx := context.Background()
	if _, err := s.Click(ctx, Target{Ref: "e6"}, ClickOptions{}); err == nil || !strings.Contains(err.Error(), "is disabled") {
		t.Errorf("disabled: %v", err)
	}
	_, err := s.Click(ctx, Target{Text: "Sign in"}, ClickOptions{})
	if err == nil || !strings.Contains(err.Error(), "3 elements match") || !strings.Contains(err.Error(), "[e7] link") {
		t.Errorf("ambiguous: %v", err)
	}
	if _, err := s.Click(ctx, Target{Ref: "e99"}, ClickOptions{}); err == nil || !strings.Contains(err.Error(), "not on the page any more") {
		t.Errorf("stale before snapshot: %v", err)
	}
	if _, err := s.Snapshot(ctx, SnapshotOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Click(ctx, Target{Ref: "e99"}, ClickOptions{}); err == nil || !strings.Contains(err.Error(), "unknown ref e99") {
		t.Errorf("unknown ref: %v", err)
	}
	if _, err := s.Click(ctx, Target{Selector: "#nope"}, ClickOptions{}); err == nil || !strings.Contains(err.Error(), "no element matches selector") {
		t.Errorf("selector: %v", err)
	}
	if _, err := s.Click(ctx, Target{}, ClickOptions{}); err == nil {
		t.Error("empty target accepted")
	}
}

func TestScrollIntoViewWithWheel(t *testing.T) {
	f := loginPage()
	far := f.add(&fakeEl{ref: "e20", role: "button", name: "Load more", box: Rect{100, 3000, 120, 40}})
	s, _ := newTestSession(f, SpeedNatural)
	if _, err := s.Click(context.Background(), Target{Ref: "e20"}, ClickOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(f.wheels) < 5 {
		t.Fatalf("expected notched wheel scrolling, got %d events", len(f.wheels))
	}
	for _, w := range f.wheels {
		if w[1] <= 0 || w[1] > 120 {
			t.Fatalf("bad notch %v", w)
		}
	}
	for _, e := range f.Events() {
		if strings.HasPrefix(e, "scrollIntoView") {
			t.Fatal("wheel scrolling should have sufficed")
		}
	}
	if vb := f.viewBox(far); vb.Y < 0 || vb.Y+vb.H > float64(f.vh) {
		t.Fatalf("element not in view after scrolling: %v", vb)
	}
}

func TestScrollIntoViewInstantUsesPage(t *testing.T) {
	f := loginPage()
	f.add(&fakeEl{ref: "e20", role: "button", name: "Load more", box: Rect{100, 3000, 120, 40}})
	s, _ := newTestSession(f, SpeedInstant)
	out, err := s.Scroll(context.Background(), ScrollOptions{Target: Target{Ref: "e20"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `Scrolled [e20] button "Load more" into view`) {
		t.Fatalf("result %q", out)
	}
	if len(f.wheels) != 0 || !strings.Contains(strings.Join(f.Events(), "|"), "scrollIntoView e20") {
		t.Fatalf("events %v", f.Events())
	}
}

func TestScrollPage(t *testing.T) {
	f := loginPage()
	f.sh = 5000
	s, _ := newTestSession(f, SpeedNatural)
	ctx := context.Background()
	out, err := s.Scroll(ctx, ScrollOptions{Direction: "down", Amount: 700})
	if err != nil {
		t.Fatal(err)
	}
	if f.sy != 700 || !strings.Contains(out, "Scrolled the page to x=0 y=700") {
		t.Fatalf("sy=%v out=%q", f.sy, out)
	}
	if _, err := s.Scroll(ctx, ScrollOptions{To: "bottom"}); err != nil {
		t.Fatal(err)
	}
	if f.sy != f.sh-float64(f.vh) {
		t.Fatalf("not at bottom: %v", f.sy)
	}
	out, err = s.Scroll(ctx, ScrollOptions{Direction: "down"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "The page did not move") {
		t.Fatalf("at end: %q", out)
	}
	if _, err := s.Scroll(ctx, ScrollOptions{To: "top"}); err != nil || f.sy != 0 {
		t.Fatalf("top: %v sy=%v", err, f.sy)
	}
	out, _ = s.Scroll(ctx, ScrollOptions{To: "top"})
	if !strings.Contains(out, "Already at the top") {
		t.Fatalf("already top: %q", out)
	}
}

func TestTypeNaturalCadence(t *testing.T) {
	f := loginPage()
	s, clk := newTestSession(f, SpeedNatural)
	out, err := s.Type(context.Background(), Target{Ref: "e2"}, "ab c.\nd", TypeOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.find("e2").value; got != "ab c.d" {
		t.Fatalf("value %q", got)
	}
	if strings.Join(f.inserts, "") != "ab c.d" {
		t.Fatalf("inserts %q", f.inserts)
	}
	for _, in := range f.inserts {
		if len([]rune(in)) != 1 {
			t.Fatalf("natural typing should insert one character at a time: %q", f.inserts)
		}
	}
	ev := strings.Join(f.Events(), "|")
	if !strings.Contains(ev, "insert .|keydown Enter|keyup Enter|insert d") || !strings.HasSuffix(ev, "keydown Enter|keyup Enter") {
		t.Fatalf("events %s", ev)
	}
	if !strings.Contains(out, `Typed into [e2] textbox "Email" value="ab c.d"; pressed Enter`) {
		t.Fatalf("out %q", out)
	}
	// The typing pauses follow the cadence model for the session's WPM.
	base := time.Minute / time.Duration(DefaultWPM*5)
	typing := 0
	for _, d := range clk.Sleeps() {
		if d >= base*3/10 && d <= base*8 {
			typing++
		}
	}
	if typing < 5 {
		t.Fatalf("expected per-character pauses, sleeps: %v", clk.Sleeps())
	}
}

func TestTypeInstantBatches(t *testing.T) {
	f := loginPage()
	s, _ := newTestSession(f, SpeedInstant)
	if _, err := s.Type(context.Background(), Target{Ref: "e2"}, "hello\tworld", TypeOptions{}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.inserts, "|") != "hello|world" {
		t.Fatalf("inserts %q", f.inserts)
	}
	if !strings.Contains(strings.Join(f.Events(), "|"), "insert hello|keydown Tab|keyup Tab|insert world") {
		t.Fatalf("events %v", f.Events())
	}
}

func TestTypeClearUsesSelectAll(t *testing.T) {
	f := loginPage()
	f.find("e2").value = "old@example.org"
	s, _ := newTestSession(f, SpeedInstant)
	if _, err := s.Type(context.Background(), Target{Ref: "e2"}, "new@x.org", TypeOptions{Clear: true}); err != nil {
		t.Fatal(err)
	}
	if v := f.find("e2").value; v != "new@x.org" {
		t.Fatalf("value %q", v)
	}
	ev := strings.Join(f.Events(), "|")
	if !strings.Contains(ev, "keydown Control|keydown a|keyup a|keyup Control|keydown Backspace|keyup Backspace|insert new@x.org") {
		t.Fatalf("events %s", ev)
	}
}

func TestTypeErrors(t *testing.T) {
	f := loginPage()
	s, _ := newTestSession(f, SpeedInstant)
	_, err := s.Type(context.Background(), Target{Ref: "e6"}, "x", TypeOptions{})
	if err == nil || !strings.Contains(err.Error(), "is not a text field") {
		t.Fatalf("got %v", err)
	}
	// Typing with no target goes to the focused element.
	f.focused = f.find("e3")
	if _, err := s.Type(context.Background(), Target{}, "pw", TypeOptions{}); err != nil || f.find("e3").value != "pw" {
		t.Fatalf("focused typing: %v %q", err, f.find("e3").value)
	}
}

func TestFocusFallback(t *testing.T) {
	f := loginPage()
	f.find("e2").noFocus = true // a wrapper swallows the click's focus
	s, _ := newTestSession(f, SpeedInstant)
	if _, err := s.Type(context.Background(), Target{Ref: "e2"}, "x", TypeOptions{}); err != nil {
		t.Fatal(err)
	}
	if f.find("e2").value != "x" {
		t.Fatal("focus fallback did not focus the field")
	}
}

func TestPress(t *testing.T) {
	f := loginPage()
	s, _ := newTestSession(f, SpeedInstant)
	out, err := s.Press(context.Background(), "ctrl+shift+k", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "Pressed Control+Shift+K 2 times") {
		t.Fatalf("out %q", out)
	}
	one := "keydown Control|keydown Shift|keydown K|keyup K|keyup Shift|keyup Control"
	if got := strings.Join(f.Events(), "|"); got != one+"|"+one {
		t.Fatalf("events %s", got)
	}
	if _, err := s.Press(context.Background(), "Nope+X", 1); err == nil {
		t.Fatal("bad chord accepted")
	}
}

func TestSelectNativeAndCustom(t *testing.T) {
	f := loginPage()
	s, _ := newTestSession(f, SpeedInstant)
	ctx := context.Background()
	out, err := s.Select(ctx, Target{Ref: "e5"}, []string{"de"}, "value")
	if err != nil || !strings.Contains(out, `now has "Germany" selected`) {
		t.Fatalf("native: %v %q", err, out)
	}
	for _, e := range f.Events() {
		if strings.HasPrefix(e, "down") {
			t.Fatal("native select must not be clicked open")
		}
	}
	if _, err := s.Select(ctx, Target{Ref: "e5"}, []string{"France"}, "auto"); err == nil || !strings.Contains(err.Error(), `no option matching "France"`) {
		t.Fatalf("missing option: %v", err)
	}

	opened := false
	trigger := f.add(&fakeEl{ref: "e30", role: "combobox", name: "Size", box: Rect{400, 100, 150, 30}})
	opt := f.add(&fakeEl{ref: "e31", role: "option", name: "Large", box: Rect{400, 130, 150, 30}, hidden: true})
	trigger.onClick = func(fd *fakeDriver) { fd.mu.Lock(); opt.hidden = false; opened = true; fd.mu.Unlock() }
	opt.onClick = func(fd *fakeDriver) { fd.mu.Lock(); trigger.name = "Size: Large"; opt.hidden = true; fd.mu.Unlock() }
	out, err = s.Select(ctx, Target{Ref: "e30"}, []string{"large"}, "auto")
	if err != nil {
		t.Fatal(err)
	}
	if !opened || !strings.Contains(out, `chose "Large" in [e30] combobox "Size"`) {
		t.Fatalf("custom: %q", out)
	}
	if !strings.Contains(out, `[e30] combobox "Size: Large"`) {
		t.Fatalf("change not reported: %q", out)
	}
	if _, err := s.Select(ctx, Target{Ref: "e30"}, []string{"x"}, "value"); err == nil || !strings.Contains(err.Error(), "not a native <select>") {
		t.Fatalf("by=value on custom: %v", err)
	}
}

func TestCheck(t *testing.T) {
	f := loginPage()
	f.add(&fakeEl{ref: "e40", role: "radio", typ: "radio", name: "Express", box: Rect{20, 400, 20, 20}, checked: "true"})
	s, _ := newTestSession(f, SpeedInstant)
	ctx := context.Background()
	out, err := s.Check(ctx, Target{Ref: "e4"}, true)
	if err != nil || !strings.Contains(out, "is now checked") || f.find("e4").checked != "true" {
		t.Fatalf("check: %v %q", err, out)
	}
	downs := 0
	for _, e := range f.Events() {
		if strings.HasPrefix(e, "down") {
			downs++
		}
	}
	out, err = s.Check(ctx, Target{Ref: "e4"}, true)
	if err != nil || !strings.Contains(out, "was already checked") {
		t.Fatalf("idempotent: %v %q", err, out)
	}
	for _, e := range f.Events() {
		if strings.HasPrefix(e, "down") {
			downs--
		}
	}
	if downs != 0 {
		t.Fatal("an already-checked box must not be clicked")
	}
	if _, err := s.Check(ctx, Target{Ref: "e4"}, false); err != nil || f.find("e4").checked != "false" {
		t.Fatalf("uncheck: %v", err)
	}
	if _, err := s.Check(ctx, Target{Ref: "e40"}, false); err == nil || !strings.Contains(err.Error(), "radio button") {
		t.Fatalf("radio uncheck: %v", err)
	}
	if _, err := s.Check(ctx, Target{Ref: "e2"}, true); err == nil || !strings.Contains(err.Error(), "not a checkbox") {
		t.Fatalf("textbox: %v", err)
	}
	// A box whose click does nothing is reported, not assumed.
	f.add(&fakeEl{ref: "e41", role: "checkbox", name: "Stuck", box: Rect{20, 450, 20, 20}, checked: "false", disabled: false})
	f.find("e41").onClick = func(fd *fakeDriver) { fd.mu.Lock(); fd.find("e41").checked = "false"; fd.mu.Unlock() }
	if _, err := s.Check(ctx, Target{Ref: "e41"}, true); err == nil || !strings.Contains(err.Error(), "still unchecked") {
		t.Fatalf("stuck: %v", err)
	}
}

func TestFillReportsPartialProgress(t *testing.T) {
	f := loginPage()
	s, _ := newTestSession(f, SpeedInstant)
	_, err := s.Fill(context.Background(), []FillField{
		{Target: Target{Ref: "e2"}, Value: "a@b.c"},
		{Target: Target{Ref: "e6"}, Value: "x"},
	}, false)
	if err == nil {
		t.Fatal("filling a button should fail")
	}
	for _, want := range []string{"field 2 (e6)", "not a text field", "filled before the error", `1. [e2] textbox "Email" value="a@b.c"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if _, err := s.Fill(context.Background(), []FillField{{Target: Target{Ref: "e4"}, Value: "maybe"}}, false); err == nil || !strings.Contains(err.Error(), "must be true or false") {
		t.Fatalf("bad bool: %v", err)
	}
}

func TestFillAppendAndSubmit(t *testing.T) {
	f := loginPage()
	f.find("e2").value = "abc"
	submitted := false
	f.onEnter = func(*fakeDriver) { submitted = true }
	s, _ := newTestSession(f, SpeedInstant)
	out, err := s.Fill(context.Background(), []FillField{{Target: Target{Ref: "e2"}, Value: "def", NoClear: true}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if f.find("e2").value != "abcdef" || !submitted || !strings.Contains(out, "Pressed Enter to submit") {
		t.Fatalf("value %q submitted %v out %q", f.find("e2").value, submitted, out)
	}
}

func TestHoverAndDiff(t *testing.T) {
	f := loginPage()
	menu := f.add(&fakeEl{ref: "e50", role: "menu", name: "Account", box: Rect{600, 60, 200, 100}, hidden: true})
	s, _ := newTestSession(f, SpeedInstant)
	if _, err := s.Snapshot(context.Background(), SnapshotOptions{}); err != nil {
		t.Fatal(err)
	}
	f.evalHook = func(method string) {
		if method == "snapshot" && len(f.moves) > 0 {
			f.mu.Lock()
			menu.hidden = false
			f.mu.Unlock()
		}
	}
	out, err := s.Hover(context.Background(), Target{Ref: "e1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `Hovering over [e1] h1 "Sign in"`) && !strings.Contains(out, `Hovering over [e1] heading "Sign in"`) {
		t.Fatalf("out %q", out)
	}
	if !strings.Contains(out, `+ [e50] menu "Account"`) {
		t.Fatalf("diff missing the opened menu:\n%s", out)
	}
	if _, ok := s.Refs()["e50"]; !ok {
		t.Fatal("refs not refreshed after the action")
	}
}

func TestNavigationViaClickRendersFullPage(t *testing.T) {
	f := loginPage()
	f.find("e6").onClick = func(fd *fakeDriver) {
		fd.mu.Lock()
		fd.url, fd.title = "https://example.test/home", "Home"
		fd.mu.Unlock()
	}
	s, _ := newTestSession(f, SpeedInstant)
	out, err := s.Click(context.Background(), Target{Ref: "e6"}, ClickOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Page is now:\nPage: Home\nURL: https://example.test/home") {
		t.Fatalf("out %q", out)
	}
}

func TestWait(t *testing.T) {
	f := loginPage()
	s, clk := newTestSession(f, SpeedInstant)
	ctx := context.Background()
	f.pageText = "Loading…"
	polls := 0
	f.evalHook = func(method string) {
		if method == "probe" {
			polls++
			if polls == 4 {
				f.mu.Lock()
				f.pageText = "Welcome back"
				f.mu.Unlock()
			}
		}
	}
	out, err := s.Wait(ctx, WaitOptions{Text: "welcome", TextGone: "Loading", NetworkIdle: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, `Waited 400ms for text "welcome", text "Loading" to disappear, network idle`) {
		t.Fatalf("out %q", out)
	}
	f.evalHook = nil
	start := clk.Now()
	_, err = s.Wait(ctx, WaitOptions{Selector: ".never", URLContains: "/done", Timeout: 2 * time.Second})
	if err == nil || !strings.Contains(err.Error(), `timed out after 2s waiting for URL containing "/done", selector ".never" to be visible`) {
		t.Fatalf("timeout: %v", err)
	}
	if el := clk.Now().Sub(start); el < 2*time.Second || el > 3*time.Second {
		t.Fatalf("waited %v", el)
	}
	if _, err := s.Wait(ctx, WaitOptions{Duration: 1500 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.Wait(cctx, WaitOptions{Text: "never"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestExtractTarget(t *testing.T) {
	f := loginPage()
	f.find("e1").text = strings.Repeat("abc ", 100)
	s, _ := newTestSession(f, SpeedInstant)
	out, err := s.Extract(context.Background(), Target{Ref: "e1"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, `Text of [e1] heading "Sign in":`) || !strings.Contains(out, "showing 100 of 400 characters") {
		t.Fatalf("out %q", out)
	}
}

func TestProviderLazyAndRetried(t *testing.T) {
	f := loginPage()
	calls := 0
	s := NewSession(func(context.Context) (Driver, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("chrome not found")
		}
		return f, nil
	}, Options{Speed: SpeedInstant})
	if s.Started() || calls != 0 {
		t.Fatal("provider must be lazy")
	}
	if _, err := s.Snapshot(context.Background(), SnapshotOptions{}); err == nil || !strings.Contains(err.Error(), "start browser: chrome not found") {
		t.Fatalf("first: %v", err)
	}
	if _, err := s.Snapshot(context.Background(), SnapshotOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Snapshot(context.Background(), SnapshotOptions{}); err != nil || calls != 2 {
		t.Fatalf("driver not cached: calls=%d err=%v", calls, err)
	}
	if _, err := NewSession(nil, Options{}).Driver(context.Background()); err == nil {
		t.Fatal("nil provider should fail")
	}
}

func TestNavigateError(t *testing.T) {
	f := loginPage()
	f.navErr = errors.New("net::ERR_NAME_NOT_RESOLVED")
	s, _ := newTestSession(f, SpeedInstant)
	if _, err := s.Navigate(context.Background(), "https://nope.invalid/"); err == nil || !strings.Contains(err.Error(), "ERR_NAME_NOT_RESOLVED") {
		t.Fatalf("got %v", err)
	}
}

func TestActionLogBounded(t *testing.T) {
	f := loginPage()
	clk := newFakeClock()
	s := NewSession(func(context.Context) (Driver, error) { return f, nil }, Options{Speed: SpeedInstant, ActionLogSize: 5, Now: clk.Now, Sleep: clk.Sleep})
	for i := 0; i < 12; i++ {
		if _, err := s.Snapshot(context.Background(), SnapshotOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	acts := s.Actions()
	if len(acts) != 5 || acts[0].Seq != 8 || acts[4].Seq != 12 {
		t.Fatalf("log = %+v", acts)
	}
}

func TestConcurrentActionsSerialize(t *testing.T) {
	f := loginPage()
	s, _ := newTestSession(f, SpeedNatural)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var err error
			if i%2 == 0 {
				_, err = s.Hover(context.Background(), Target{Ref: "e6"})
			} else {
				_, err = s.Snapshot(context.Background(), SnapshotOptions{})
			}
			if err != nil {
				t.Error(err)
			}
			_ = s.Actions()
			_, _ = s.Cursor()
		}(i)
	}
	wg.Wait()
	if n := len(s.Actions()); n != 8 {
		t.Fatalf("logged %d actions", n)
	}
}

func TestParseSpeed(t *testing.T) {
	for in, want := range map[string]Speed{"": SpeedNatural, "Natural": SpeedNatural, "instant": SpeedInstant, "fast": SpeedInstant} {
		if got, err := ParseSpeed(in); err != nil || got != want {
			t.Errorf("ParseSpeed(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := ParseSpeed("ludicrous"); err == nil {
		t.Error("unknown speed accepted")
	}
}
