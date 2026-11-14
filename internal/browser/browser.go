package browser

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

// Default viewport and timeouts.
const (
	DefaultWidth  = 1280
	DefaultHeight = 800

	defaultLaunchTimeout = 45 * time.Second
	gracefulCloseTimeout = 3 * time.Second
	processExitTimeout   = 5 * time.Second
	stderrTailLines      = 30
)

// Options configure Launch.
type Options struct {
	// ExecPath is the browser executable. Empty uses FindExecutable.
	ExecPath string
	// Headless runs Chromium with --headless=new. When false and no
	// display server is available (Linux and BSD without DISPLAY or
	// WAYLAND_DISPLAY), the browser is still started headless.
	Headless bool
	// Width and Height set the window size and the default viewport of
	// new pages. Zero means 1280x800.
	Width, Height int
	// UserDataDir is the profile directory. Empty creates a temporary one
	// that Close removes; a directory given here is left in place.
	UserDataDir string
	// ExtraArgs are appended to the command line after the defaults, so a
	// repeated switch overrides the default. Including
	// "--remote-debugging-pipe" selects the pipe transport instead of a
	// WebSocket on a loopback port.
	ExtraArgs []string
	// Logf receives diagnostics, including the browser's stderr. Nil
	// discards them.
	Logf func(string, ...any)
}

// Browser is a running browser process and its DevTools connection.
type Browser struct {
	opts    Options
	logf    func(string, ...any)
	cmd     *exec.Cmd
	conn    *cdpConn
	tempDir string // profile directory to remove on Close, if we made it
	wsURL   string

	exited     chan struct{} // closed when the process has been reaped
	stderr     *os.File      // our read end of the stderr pipe
	stderrDone chan struct{}
	watchDone  chan struct{}

	tailMu sync.Mutex
	tail   []string // last stderr lines, for launch errors

	mu     sync.Mutex
	pages  []*Page
	closed bool

	closeOnce sync.Once
	closeErr  error
}

var devtoolsURLRE = regexp.MustCompile(`DevTools listening on (ws://\S+)`)

// Launch starts a browser and connects to it. ctx bounds the launch only;
// the browser keeps running until Close. Without a ctx deadline, launching
// gives up after 45 seconds.
func Launch(ctx context.Context, opts Options) (*Browser, error) {
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if opts.Width <= 0 {
		opts.Width = DefaultWidth
	}
	if opts.Height <= 0 {
		opts.Height = DefaultHeight
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultLaunchTimeout)
		defer cancel()
	}

	execPath := opts.ExecPath
	if execPath == "" {
		p, err := FindExecutable()
		if err != nil {
			return nil, err
		}
		execPath = p
	}
	if !opts.Headless && !displayAvailable() {
		logf("browser: no display server found; running headless")
		opts.Headless = true
	}

	b := &Browser{
		opts:       opts,
		logf:       logf,
		exited:     make(chan struct{}),
		stderrDone: make(chan struct{}),
		watchDone:  make(chan struct{}),
	}
	if opts.UserDataDir == "" {
		dir, err := os.MkdirTemp("", "gta-browser-")
		if err != nil {
			return nil, fmt.Errorf("browser: create profile dir: %w", err)
		}
		b.tempDir = dir
		opts.UserDataDir = dir
		b.opts.UserDataDir = dir
	}

	usePipe := slices.Contains(opts.ExtraArgs, "--remote-debugging-pipe")
	args := buildArgs(opts, usePipe, runningAsRoot())

	cmd := exec.Command(execPath, args...)
	configureProcess(cmd)
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		b.removeTempDir()
		return nil, fmt.Errorf("browser: stderr pipe: %w", err)
	}
	cmd.Stdout = stderrW
	cmd.Stderr = stderrW
	b.stderr = stderrR

	// Pipe transport: the browser reads commands on fd 3 and writes
	// responses on fd 4.
	var toBrowserR, toBrowserW, fromBrowserR, fromBrowserW *os.File
	closeChildEnds := func() {
		for _, f := range []*os.File{stderrW, toBrowserR, fromBrowserW} {
			if f != nil {
				_ = f.Close()
			}
		}
	}
	if usePipe {
		if toBrowserR, toBrowserW, err = os.Pipe(); err == nil {
			fromBrowserR, fromBrowserW, err = os.Pipe()
		}
		if err != nil {
			closeChildEnds()
			for _, f := range []*os.File{stderrR, toBrowserW} {
				if f != nil {
					_ = f.Close()
				}
			}
			b.removeTempDir()
			return nil, fmt.Errorf("browser: debugging pipes: %w", err)
		}
		cmd.ExtraFiles = []*os.File{toBrowserR, fromBrowserW}
	}

	logf("browser: starting %s %s", execPath, strings.Join(args, " "))
	if err := cmd.Start(); err != nil {
		closeChildEnds()
		_ = stderrR.Close()
		if usePipe {
			_ = toBrowserW.Close()
			_ = fromBrowserR.Close()
		}
		b.removeTempDir()
		return nil, fmt.Errorf("browser: start %s: %w", execPath, err)
	}
	closeChildEnds()
	b.cmd = cmd

	go func() {
		_ = cmd.Wait()
		close(b.exited)
	}()

	urlCh := make(chan string, 1)
	go b.readStderr(urlCh, !usePipe)

	var tr transport
	if usePipe {
		tr = newPipeTransport(fromBrowserR, toBrowserW)
	} else {
		var wsURL string
		select {
		case wsURL = <-urlCh:
		case <-b.exited:
			// Let the stderr reader catch the final lines before quoting them.
			select {
			case <-b.stderrDone:
			case <-time.After(time.Second):
			}
			b.teardown(false)
			return nil, fmt.Errorf("browser: %s exited before DevTools was ready%s", execPath, b.stderrTail())
		case <-ctx.Done():
			b.teardown(false)
			return nil, fmt.Errorf("browser: waiting for DevTools endpoint: %w%s", ctx.Err(), b.stderrTail())
		}
		b.wsURL = wsURL
		wc, err := wsDial(ctx, wsURL, wsConfig{})
		if err != nil {
			b.teardown(false)
			return nil, fmt.Errorf("browser: connect to %s: %w", wsURL, err)
		}
		tr = wsTransport{wc}
	}
	b.conn = newCDPConn(tr, logf)

	var version struct {
		Product         string `json:"product"`
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := b.conn.call(ctx, "", "Browser.getVersion", nil, &version); err != nil {
		_ = b.conn.Close()
		b.teardown(false)
		return nil, fmt.Errorf("browser: handshake: %w%s", err, b.stderrTail())
	}
	logf("browser: connected to %s (protocol %s)", version.Product, version.ProtocolVersion)

	b.conn.subscribe("", "Target.detachedFromTarget", func(_ string, params json.RawMessage) {
		var ev struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(params, &ev) == nil {
			if p := b.pageBySession(ev.SessionID); p != nil {
				p.markClosed(ErrPageClosed)
			}
		}
	})
	go b.watchConn()
	return b, nil
}

// buildArgs assembles the command line.
func buildArgs(opts Options, usePipe, root bool) []string {
	args := []string{
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-background-networking",
		"--disable-background-timer-throttling",
		"--disable-backgrounding-occluded-windows",
		"--disable-renderer-backgrounding",
		"--disable-breakpad",
		"--disable-crash-reporter",
		"--disable-client-side-phishing-detection",
		"--disable-component-update",
		"--disable-default-apps",
		"--disable-extensions",
		"--disable-hang-monitor",
		"--disable-ipc-flooding-protection",
		"--disable-popup-blocking",
		"--disable-prompt-on-repost",
		"--disable-sync",
		"--disable-domain-reliability",
		"--disable-features=Translate,BackForwardCache,MediaRouter,OptimizationHints,AcceptCHFrame,DialMediaRouteProvider",
		"--enable-automation",
		"--metrics-recording-only",
		"--password-store=basic",
		"--use-mock-keychain",
		"--mute-audio",
		"--force-color-profile=srgb",
		fmt.Sprintf("--window-size=%d,%d", opts.Width, opts.Height),
		"--user-data-dir=" + opts.UserDataDir,
	}
	if runtime.GOOS == "linux" {
		// /dev/shm is tiny in many containers; Chromium crashes when it
		// fills up.
		args = append(args, "--disable-dev-shm-usage")
	}
	if opts.Headless {
		args = append(args, "--headless=new", "--disable-gpu", "--hide-scrollbars")
	}
	if root {
		args = append(args, "--no-sandbox")
	}
	if !usePipe {
		args = append(args, "--remote-debugging-port=0", "--remote-debugging-address=127.0.0.1")
	}
	args = append(args, opts.ExtraArgs...)
	return append(args, "about:blank")
}

// displayAvailable reports whether a headful browser could open a window.
func displayAvailable() bool {
	switch runtime.GOOS {
	case "windows", "darwin":
		return true
	}
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
}

// readStderr drains the browser's stderr until EOF, publishing the
// DevTools URL when it appears and keeping a tail for error messages.
func (b *Browser) readStderr(urlCh chan<- string, wantURL bool) {
	defer close(b.stderrDone)
	r := bufio.NewReaderSize(b.stderr, 16<<10)
	for {
		line, err := r.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if line != "" {
			if wantURL {
				if m := devtoolsURLRE.FindStringSubmatch(line); m != nil {
					urlCh <- m[1]
					wantURL = false
				}
			}
			b.tailMu.Lock()
			b.tail = append(b.tail, line)
			if len(b.tail) > stderrTailLines {
				b.tail = b.tail[len(b.tail)-stderrTailLines:]
			}
			b.tailMu.Unlock()
			b.logf("browser stderr: %s", line)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
				b.logf("browser: reading stderr: %v", err)
			}
			return
		}
	}
}

func (b *Browser) stderrTail() string {
	b.tailMu.Lock()
	defer b.tailMu.Unlock()
	if len(b.tail) == 0 {
		return ""
	}
	return "\nbrowser output:\n  " + strings.Join(b.tail, "\n  ")
}

// watchConn closes every page once the connection drops, so their
// methods fail fast instead of waiting on a dead browser.
func (b *Browser) watchConn() {
	defer close(b.watchDone)
	<-b.conn.Done()
	for _, p := range b.Pages() {
		p.markClosed(ErrClosed)
	}
}

// NewPage opens a new tab, attaches to it and applies the default
// viewport.
func (b *Browser) NewPage(ctx context.Context) (*Page, error) {
	b.mu.Lock()
	closed := b.closed
	b.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	var created struct {
		TargetID string `json:"targetId"`
	}
	if err := b.conn.call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &created); err != nil {
		return nil, fmt.Errorf("browser: new page: %w", err)
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := b.conn.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": created.TargetID, "flatten": true}, &attached); err != nil {
		b.closeTargetQuietly(created.TargetID)
		return nil, fmt.Errorf("browser: attach to page: %w", err)
	}
	p := newPage(b, created.TargetID, attached.SessionID)
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		p.markClosed(ErrClosed)
		return nil, ErrClosed
	}
	b.pages = append(b.pages, p)
	b.mu.Unlock()
	if err := p.init(ctx); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("browser: initialize page: %w", err)
	}
	return p, nil
}

func (b *Browser) closeTargetQuietly(targetID string) {
	ctx, cancel := context.WithTimeout(context.Background(), gracefulCloseTimeout)
	defer cancel()
	_ = b.conn.call(ctx, "", "Target.closeTarget", map[string]any{"targetId": targetID}, nil)
}

// Pages returns the open pages created by NewPage, oldest first.
func (b *Browser) Pages() []*Page {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.pages)
}

func (b *Browser) pageBySession(id string) *Page {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, p := range b.pages {
		if p.sessionID == id {
			return p
		}
	}
	return nil
}

func (b *Browser) removePage(p *Page) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pages = slices.DeleteFunc(b.pages, func(q *Page) bool { return q == p })
}

// Close shuts the browser down: it asks the browser to exit, kills the
// whole process group if it has not exited in time (and any helper left
// behind afterwards), and removes the temporary profile directory. It is
// idempotent and safe to call concurrently.
func (b *Browser) Close() error {
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		pages := slices.Clone(b.pages)
		b.mu.Unlock()
		for _, p := range pages {
			p.markClosed(ErrClosed)
		}

		select {
		case <-b.conn.Done():
		default:
			ctx, cancel := context.WithTimeout(context.Background(), gracefulCloseTimeout)
			err := b.conn.call(ctx, "", "Browser.close", nil, nil)
			cancel()
			if err != nil && !errors.Is(err, ErrClosed) {
				b.logf("browser: Browser.close: %v", err)
			}
		}
		_ = b.conn.Close()
		<-b.watchDone
		b.closeErr = b.teardown(true)
	})
	return b.closeErr
}

// teardown makes sure the process tree is gone, releases the stderr pipe
// and removes the temporary profile. With graceful set it first gives the
// browser time to exit on its own (after Browser.close).
func (b *Browser) teardown(graceful bool) error {
	var errs []error
	if b.cmd != nil && b.cmd.Process != nil {
		if graceful {
			select {
			case <-b.exited:
			case <-time.After(processExitTimeout):
				b.logf("browser: process did not exit; killing process group")
			}
		}
		// Kill the group even after a clean exit: helper processes
		// (zygotes, GPU and network services) may outlive the leader.
		if err := killProcessTree(b.cmd.Process); err != nil {
			errs = append(errs, fmt.Errorf("browser: kill: %w", err))
		}
		select {
		case <-b.exited:
		case <-time.After(processExitTimeout):
			errs = append(errs, errors.New("browser: process did not exit after SIGKILL"))
		}
		// SIGKILL is delivered asynchronously; wait until no member of
		// the group is left running.
		pgid := b.cmd.Process.Pid
		deadline := time.Now().Add(processExitTimeout)
		for processGroupAlive(pgid) {
			if time.Now().After(deadline) {
				errs = append(errs, fmt.Errorf("browser: processes remain in group %d", pgid))
				break
			}
			_ = killProcessTree(b.cmd.Process)
			time.Sleep(10 * time.Millisecond)
		}
	}
	if b.stderr != nil {
		select {
		case <-b.stderrDone:
		case <-time.After(2 * time.Second):
		}
		_ = b.stderr.Close()
		<-b.stderrDone
	}
	if err := b.removeTempDir(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// removeTempDir deletes the profile directory created by Launch, retrying
// briefly while exiting helpers release files.
func (b *Browser) removeTempDir() error {
	if b.tempDir == "" {
		return nil
	}
	var err error
	for attempt := range 10 {
		if err = os.RemoveAll(b.tempDir); err == nil {
			b.tempDir = ""
			return nil
		}
		time.Sleep(time.Duration(attempt+1) * 50 * time.Millisecond)
	}
	return fmt.Errorf("browser: remove profile dir: %w", err)
}
