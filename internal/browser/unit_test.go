package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDescribeKey(t *testing.T) {
	cases := []struct {
		key  string
		mods int
		want keyDescription
	}{
		{"Enter", 0, keyDescription{Key: "Enter", Code: "Enter", KeyCode: 13, Text: "\r"}},
		{"\n", 0, keyDescription{Key: "Enter", Code: "Enter", KeyCode: 13, Text: "\r"}},
		{"Tab", 0, keyDescription{Key: "Tab", Code: "Tab", KeyCode: 9}},
		{"Backspace", 0, keyDescription{Key: "Backspace", Code: "Backspace", KeyCode: 8}},
		{"ArrowDown", 0, keyDescription{Key: "ArrowDown", Code: "ArrowDown", KeyCode: 40}},
		{"Escape", 0, keyDescription{Key: "Escape", Code: "Escape", KeyCode: 27}},
		{"a", 0, keyDescription{Key: "a", Code: "KeyA", KeyCode: 65, Text: "a"}},
		{"a", ModShift, keyDescription{Key: "A", Code: "KeyA", KeyCode: 65, Text: "A"}},
		{"KeyZ", 0, keyDescription{Key: "z", Code: "KeyZ", KeyCode: 90, Text: "z"}},
		{"A", 0, keyDescription{Key: "A", Code: "KeyA", KeyCode: 65, Text: "A"}},
		{"a", ModControl, keyDescription{Key: "a", Code: "KeyA", KeyCode: 65}},
		{"a", ModControl | ModShift, keyDescription{Key: "A", Code: "KeyA", KeyCode: 65}},
		{"1", ModShift, keyDescription{Key: "!", Code: "Digit1", KeyCode: 49, Text: "!"}},
		{"!", 0, keyDescription{Key: "!", Code: "Digit1", KeyCode: 49, Text: "!"}},
		{"/", ModShift, keyDescription{Key: "?", Code: "Slash", KeyCode: 191, Text: "?"}},
		{" ", 0, keyDescription{Key: " ", Code: "Space", KeyCode: 32, Text: " "}},
		{"Shift", 0, keyDescription{Key: "Shift", Code: "ShiftLeft", KeyCode: 16, Location: locLeft}},
		{"ControlRight", 0, keyDescription{Key: "Control", Code: "ControlRight", KeyCode: 17, Location: locRight}},
		{"Meta", 0, keyDescription{Key: "Meta", Code: "MetaLeft", KeyCode: 91, Location: locLeft}},
		{"Numpad5", 0, keyDescription{Key: "5", Code: "Numpad5", KeyCode: 101, Text: "5", Location: locNumpad}},
		{"NumpadEnter", 0, keyDescription{Key: "Enter", Code: "NumpadEnter", KeyCode: 13, Text: "\r", Location: locNumpad}},
		{"F5", 0, keyDescription{Key: "F5", Code: "F5", KeyCode: 116}},
		{"é", 0, keyDescription{Key: "é", Text: "é"}},
		{"€", ModAlt, keyDescription{Key: "€"}},
	}
	for _, tc := range cases {
		got, err := describeKey(tc.key, tc.mods)
		if err != nil {
			t.Errorf("describeKey(%q, %d): %v", tc.key, tc.mods, err)
			continue
		}
		if got != tc.want {
			t.Errorf("describeKey(%q, %d) = %+v, want %+v", tc.key, tc.mods, got, tc.want)
		}
	}
	if _, err := describeKey("NoSuchKey", 0); err == nil {
		t.Error("unknown multi-character key accepted")
	}
	if _, err := describeKey("", 0); err == nil {
		t.Error("empty key accepted")
	}
}

func TestKeyTableConsistency(t *testing.T) {
	for name, d := range keyDefinitions {
		if d.Key == "" {
			t.Errorf("%q has no key value", name)
		}
		if d.Code == "" || d.KeyCode == 0 {
			t.Errorf("%q lacks code or keyCode: %+v", name, d)
		}
	}
	// Every printable ASCII character can be typed.
	for c := rune(' '); c <= '~'; c++ {
		d, err := describeKey(string(c), 0)
		if err != nil || d.Code == "" {
			t.Errorf("ASCII %q: %+v %v", c, d, err)
		}
	}
	for _, mod := range []string{"Shift", "Control", "Alt", "Meta"} {
		if modifierBit(mod) == 0 {
			t.Errorf("%s has no modifier bit", mod)
		}
	}
}

func TestParseButton(t *testing.T) {
	for in, want := range map[string]int{"": 1, "left": 1, "Right": 2, "middle": 4, "back": 8, "forward": 16, "none": 0} {
		_, bit, err := parseButton(in)
		if err != nil || bit != want {
			t.Errorf("parseButton(%q) = %d, %v", in, bit, err)
		}
	}
	if _, _, err := parseButton("thumb"); err == nil {
		t.Error("unknown button accepted")
	}
	if got := pressedButtonName(4 | 2); got != "right" {
		t.Errorf("pressedButtonName = %q", got)
	}
	if got := pressedButtonName(0); got != "none" {
		t.Errorf("pressedButtonName(0) = %q", got)
	}
}

func TestChanMutexHonorsContext(t *testing.T) {
	m := chanMutex{ch: make(chan struct{}, 1)}
	if err := m.lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := m.lock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lock: %v", err)
	}
	m.unlock()
	if err := m.lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.unlock()
}

func TestBuildArgs(t *testing.T) {
	opts := Options{Headless: true, Width: 800, Height: 600, UserDataDir: "/tmp/p", ExtraArgs: []string{"--lang=de"}}
	args := buildArgs(opts, false, true)
	for _, want := range []string{
		"--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
		"--no-sandbox", "--remote-debugging-port=0", "--window-size=800,600", "--user-data-dir=/tmp/p",
	} {
		if !slices.Contains(args, want) {
			t.Errorf("missing %s in %v", want, args)
		}
	}
	if args[len(args)-1] != "about:blank" || args[len(args)-2] != "--lang=de" {
		t.Errorf("extra args must precede the URL: %v", args[len(args)-3:])
	}

	args = buildArgs(Options{Width: 1, Height: 1, UserDataDir: "/x"}, true, false)
	for _, unwanted := range []string{"--headless=new", "--disable-gpu", "--no-sandbox", "--remote-debugging-port=0"} {
		if slices.Contains(args, unwanted) {
			t.Errorf("unexpected %s in %v", unwanted, args)
		}
	}
}

func TestDevtoolsURLPattern(t *testing.T) {
	line := "\nDevTools listening on ws://127.0.0.1:41235/devtools/browser/0b7c6f9e-1d2a-4f1c-9a52-1d1e5ea5f3d1\n"
	m := devtoolsURLRE.FindStringSubmatch(line)
	if m == nil || m[1] != "ws://127.0.0.1:41235/devtools/browser/0b7c6f9e-1d2a-4f1c-9a52-1d1e5ea5f3d1" {
		t.Fatalf("match = %v", m)
	}
}

func fakeExecutable(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFindExecutableEnvOverride(t *testing.T) {
	dir := t.TempDir()
	exe := fakeExecutable(t, dir, "my-chrome")
	env := map[string]string{EnvBrowser: exe}
	noPath := func(string) (string, error) { return "", errors.New("not found") }
	got, err := findExecutable(func(k string) string { return env[k] }, "linux", noPath)
	if err != nil || got != exe {
		t.Fatalf("got %q, %v", got, err)
	}

	// A bare name is looked up on PATH.
	env[EnvBrowser] = "brave"
	got, err = findExecutable(func(k string) string { return env[k] }, "linux", func(n string) (string, error) {
		if n == "brave" {
			return "/opt/brave/brave", nil
		}
		return "", errors.New("no")
	})
	if err != nil || got != "/opt/brave/brave" {
		t.Fatalf("got %q, %v", got, err)
	}

	// A bad override is an error, not a silent fallback.
	env[EnvBrowser] = filepath.Join(dir, "missing")
	if _, err := findExecutable(func(k string) string { return env[k] }, "linux", noPath); err == nil || !strings.Contains(err.Error(), EnvBrowser) {
		t.Fatalf("err = %v", err)
	}
	nonExec := filepath.Join(dir, "plain")
	os.WriteFile(nonExec, []byte("x"), 0o644)
	env[EnvBrowser] = nonExec
	if _, err := findExecutable(func(k string) string { return env[k] }, "linux", noPath); err == nil {
		t.Fatal("non-executable override accepted")
	}
}

func TestFindExecutablePathFallbackAndNotFound(t *testing.T) {
	empty := func(string) string { return "" }
	var looked []string
	got, err := findExecutable(empty, "windows", func(n string) (string, error) {
		looked = append(looked, n)
		if n == "msedge" {
			return `C:\Edge\msedge.exe`, nil
		}
		return "", errors.New("no")
	})
	if err != nil || got != `C:\Edge\msedge.exe` {
		t.Fatalf("got %q, %v", got, err)
	}
	if looked[0] != "chromium" || !slices.Contains(looked, "google-chrome") || !slices.Contains(looked, "chrome") {
		t.Fatalf("lookup order %v", looked)
	}
	_, err = findExecutable(empty, "windows", func(string) (string, error) { return "", errors.New("no") })
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestCandidatePaths(t *testing.T) {
	env := map[string]string{
		"HOME":              "/Users/me",
		"LOCALAPPDATA":      `C:\Users\me\AppData\Local`,
		"PROGRAMFILES":      `C:\Program Files`,
		"PROGRAMFILES(X86)": `C:\Program Files (x86)`,
	}
	getenv := func(k string) string { return env[k] }
	mac := candidatePaths(getenv, "darwin")
	if mac[0] != "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" {
		t.Errorf("darwin first candidate %q", mac[0])
	}
	if !slices.Contains(mac, "/Users/me/Applications/Chromium.app/Contents/MacOS/Chromium") ||
		!slices.Contains(mac, "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge") {
		t.Errorf("darwin candidates %v", mac)
	}
	win := candidatePaths(getenv, "windows")
	for _, want := range []string{
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		`C:\Users\me\AppData\Local\Chromium\Application\chrome.exe`,
	} {
		if !slices.Contains(win, want) {
			t.Errorf("windows candidates lack %s", want)
		}
	}
	linux := candidatePaths(getenv, "linux")
	if !slices.Contains(linux, "/usr/bin/chromium") || !slices.Contains(linux, "/usr/bin/google-chrome") {
		t.Errorf("linux candidates %v", linux)
	}
}

func TestPlaywrightPathsNewestFirst(t *testing.T) {
	root := t.TempDir()
	old := fakeExecutable(t, root, "chromium-999/chrome-linux/chrome")
	newer := fakeExecutable(t, root, "chromium-1243/chrome-linux64/chrome")
	fakeExecutable(t, root, "chromium_headless_shell-1243/chrome-linux/headless_shell")
	env := map[string]string{"PLAYWRIGHT_BROWSERS_PATH": root}
	paths := playwrightPaths(func(k string) string { return env[k] }, "linux", "")
	if len(paths) < 2 || paths[0] != newer || paths[1] != old {
		t.Fatalf("paths = %v", paths)
	}
}

func TestDecodeScreencastFrame(t *testing.T) {
	// A 1x1 PNG.
	pngData, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==")
	raw, _ := json.Marshal(map[string]any{
		"data":      base64.StdEncoding.EncodeToString(pngData),
		"sessionId": 7,
		"metadata": map[string]any{
			"deviceWidth": 800, "deviceHeight": 600,
			"scrollOffsetX": 3, "scrollOffsetY": 250.5, "timestamp": 1700000000.25,
		},
	})
	f, ack, err := decodeScreencastFrame(raw, "png")
	if err != nil || ack != 7 {
		t.Fatalf("ack %d, %v", ack, err)
	}
	if f.Width != 1 || f.Height != 1 || f.Format != "png" || f.ScrollY != 250.5 || f.ScrollX != 3 {
		t.Fatalf("frame %+v", f)
	}
	if want := time.Unix(1700000000, 250000000); !f.Timestamp.Equal(want) {
		t.Fatalf("timestamp %v", f.Timestamp)
	}
	// Bad image data is still acknowledged.
	raw, _ = json.Marshal(map[string]any{"data": "!!!", "sessionId": 9})
	if _, ack, err := decodeScreencastFrame(raw, "jpeg"); err == nil || ack != 9 {
		t.Fatalf("bad data: ack %d, %v", ack, err)
	}
	if _, ack, err := decodeScreencastFrame(json.RawMessage(`{`), "jpeg"); err == nil || ack != -1 {
		t.Fatalf("bad json: ack %d, %v", ack, err)
	}
}

func TestDeliverDropOldest(t *testing.T) {
	out := make(chan Frame, 2)
	for i := uint64(1); i <= 5; i++ {
		deliverDropOldest(out, Frame{Seq: i})
	}
	if a, b := <-out, <-out; a.Seq != 4 || b.Seq != 5 {
		t.Fatalf("kept %d and %d, want 4 and 5", a.Seq, b.Seq)
	}
}

func TestNormalizeImageFormat(t *testing.T) {
	for in, want := range map[string]string{"": "png", "PNG": "png", "jpg": "jpeg", "jpeg": "jpeg", "webp": "webp"} {
		if got, err := normalizeImageFormat(in, true); err != nil || got != want {
			t.Errorf("%q -> %q, %v", in, got, err)
		}
	}
	if _, err := normalizeImageFormat("bmp", true); err == nil {
		t.Error("bmp accepted")
	}
	if clampQuality(0) != 80 || clampQuality(150) != 100 || clampQuality(55) != 55 {
		t.Error("clampQuality")
	}
}

func TestNetTracker(t *testing.T) {
	var n netTracker
	n.init()
	_, _, ch := n.state()
	n.start("r1")
	n.start("r1") // redirect reuses the id
	n.start("r2")
	select {
	case <-ch:
	default:
		t.Fatal("change not signalled")
	}
	if c, _, _ := n.state(); c != 2 {
		t.Fatalf("inflight %d", c)
	}
	n.finish("r1")
	n.finish("unknown")
	n.finish("r2")
	if c, _, _ := n.state(); c != 0 {
		t.Fatalf("inflight %d", c)
	}
}
