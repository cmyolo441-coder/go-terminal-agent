// Package lsp is a Language Server Protocol client for the agent: JSON-RPC
// over stdio with Content-Length framing, a manager that starts the right
// server per file type lazily and restarts it after a crash, document
// sync, diagnostics after edits, and definition, references and hover.
package lsp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Defaults for Options.
const (
	DefaultRequestTimeout    = 20 * time.Second
	DefaultStartTimeout      = 60 * time.Second
	DefaultDiagnosticsWait   = 4 * time.Second
	DefaultDiagnosticsSettle = 150 * time.Millisecond
	DefaultMaxRestarts       = 3
	DefaultRestartWindow     = 3 * time.Minute
	DefaultAfterEditLimit    = 20
)

var (
	// ErrNoServer means no server is configured for the file type.
	ErrNoServer = errors.New("lsp: no language server configured for this file type")
	// ErrUnavailable means the server's executable is not installed.
	ErrUnavailable = errors.New("lsp: language server not installed")
	// ErrManagerClosed is returned after Close.
	ErrManagerClosed = errors.New("lsp: manager closed")
)

// Options configure a Manager.
type Options struct {
	// Root is the workspace root; servers never start for files outside it.
	Root string
	// Servers overrides the server table. Entries whose Name matches a
	// default replace it (set Disabled to turn it off); others are added
	// in front of the defaults. Nil uses DefaultServers.
	Servers []ServerConfig
	// NoDefaults uses only Servers.
	NoDefaults bool
	// LookPath finds executables (default exec.LookPath).
	LookPath func(string) (string, error)
	// RequestTimeout bounds definition, references and hover calls.
	RequestTimeout time.Duration
	// StartTimeout bounds a server's initialize handshake.
	StartTimeout time.Duration
	// DiagnosticsWait is how long AfterEdit waits for fresh diagnostics.
	DiagnosticsWait time.Duration
	// DiagnosticsSettle is the quiet period after a publish during which
	// follow-up publishes are still collected.
	DiagnosticsSettle time.Duration
	// MaxRestarts within RestartWindow before a crashing server is given
	// up on.
	MaxRestarts   int
	RestartWindow time.Duration
	// AfterEditLimit caps the diagnostics AfterEdit reports.
	AfterEditLimit int
	Logf           func(format string, args ...any)
}

// Manager routes files to language servers.
type Manager struct {
	opts    Options
	root    string
	servers []ServerConfig

	mu     sync.Mutex
	slots  map[string]*slot
	closed bool
}

// slot holds one server instance for one workspace root.
type slot struct {
	cfg    ServerConfig
	root   string
	binary string

	mu      sync.Mutex
	client  *Client
	pending *startOp
	starts  int
	crashes []time.Time
	failed  error
	lastErr error
	stderr  string
}

// startOp is a server start in progress. It runs with its own timeout so a
// caller with a short deadline does not abort a slow but healthy start.
type startOp struct {
	done   chan struct{}
	client *Client
	err    error
}

// NewManager builds a manager; no server starts until a file needs one.
func NewManager(opts Options) (*Manager, error) {
	if strings.TrimSpace(opts.Root) == "" {
		return nil, fmt.Errorf("lsp: root is required")
	}
	root, err := filepath.Abs(opts.Root)
	if err != nil {
		return nil, err
	}
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	if opts.LookPath == nil {
		opts.LookPath = exec.LookPath
	}
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = DefaultRequestTimeout
	}
	if opts.StartTimeout <= 0 {
		opts.StartTimeout = DefaultStartTimeout
	}
	if opts.DiagnosticsWait <= 0 {
		opts.DiagnosticsWait = DefaultDiagnosticsWait
	}
	if opts.DiagnosticsSettle < 0 {
		opts.DiagnosticsSettle = 0
	} else if opts.DiagnosticsSettle == 0 {
		opts.DiagnosticsSettle = DefaultDiagnosticsSettle
	}
	if opts.MaxRestarts <= 0 {
		opts.MaxRestarts = DefaultMaxRestarts
	}
	if opts.RestartWindow <= 0 {
		opts.RestartWindow = DefaultRestartWindow
	}
	if opts.AfterEditLimit <= 0 {
		opts.AfterEditLimit = DefaultAfterEditLimit
	}
	servers, err := mergeServers(opts.Servers, opts.NoDefaults)
	if err != nil {
		return nil, err
	}
	return &Manager{opts: opts, root: root, servers: servers, slots: map[string]*slot{}}, nil
}

// mergeServers combines configured servers with the defaults.
func mergeServers(custom []ServerConfig, noDefaults bool) ([]ServerConfig, error) {
	var out []ServerConfig
	byName := map[string]bool{}
	for _, s := range custom {
		s.Extensions = append([]string(nil), s.Extensions...)
		if strings.TrimSpace(s.Name) == "" {
			return nil, fmt.Errorf("lsp: a server needs a name")
		}
		if byName[s.Name] {
			return nil, fmt.Errorf("lsp: server %q configured twice", s.Name)
		}
		byName[s.Name] = true
		if s.Disabled {
			continue
		}
		if strings.TrimSpace(s.Command) == "" {
			return nil, fmt.Errorf("lsp: server %q needs a command", s.Name)
		}
		if len(s.Extensions) == 0 {
			return nil, fmt.Errorf("lsp: server %q needs extensions", s.Name)
		}
		for i, e := range s.Extensions {
			if !strings.HasPrefix(e, ".") {
				s.Extensions[i] = "." + e
			}
		}
		out = append(out, s)
	}
	if !noDefaults {
		for _, d := range DefaultServers() {
			if !byName[d.Name] {
				out = append(out, d)
			}
		}
	}
	return out, nil
}

func (m *Manager) logf(format string, args ...any) {
	if m.opts.Logf != nil {
		m.opts.Logf(format, args...)
	}
}

// Root is the workspace root.
func (m *Manager) Root() string { return m.root }

// resolve makes path absolute and checks it is inside the workspace.
func (m *Manager) resolve(path string) (string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(m.root, path)
	}
	path = filepath.Clean(path)
	if r, err := filepath.EvalSymlinks(path); err == nil {
		path = r
	} else if r, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		path = filepath.Join(r, filepath.Base(path))
	}
	rel, err := filepath.Rel(m.root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside the workspace", path)
	}
	return path, nil
}

// ServerFor returns the server configured for a path's extension.
func (m *Manager) ServerFor(path string) (ServerConfig, bool) {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == "" {
		return ServerConfig{}, false
	}
	for _, s := range m.servers {
		if s.handles(ext) {
			return s, true
		}
	}
	return ServerConfig{}, false
}

// detectRoot finds the nearest directory at or above path's directory,
// within the workspace, that contains one of the markers.
func (m *Manager) detectRoot(path string, markers []string) string {
	dir := filepath.Dir(path)
	for {
		for _, mk := range markers {
			if _, err := os.Stat(filepath.Join(dir, mk)); err == nil {
				return dir
			}
		}
		if dir == m.root {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir || !strings.HasPrefix(parent, m.root) {
			break
		}
		dir = parent
	}
	return m.root
}

// client returns a running client for path, starting or restarting the
// server as needed.
func (m *Manager) client(ctx context.Context, path string) (*Client, *slot, error) {
	cfg, ok := m.ServerFor(path)
	if !ok {
		return nil, nil, ErrNoServer
	}
	binary, err := m.opts.LookPath(cfg.Command)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %s (%s not found on PATH)", ErrUnavailable, cfg.Name, cfg.Command)
	}
	root := m.detectRoot(path, cfg.RootMarkers)
	key := cfg.Name + "\x00" + root
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, nil, ErrManagerClosed
	}
	s := m.slots[key]
	if s == nil {
		s = &slot{cfg: cfg, root: root}
		m.slots[key] = s
	}
	m.mu.Unlock()

	s.mu.Lock()
	if s.client != nil && s.client.Alive() {
		c := s.client
		s.mu.Unlock()
		return c, s, nil
	}
	if s.failed != nil {
		err := s.failed
		s.mu.Unlock()
		return nil, s, err
	}
	if s.pending == nil {
		if s.client != nil {
			// The server died: count the crash and maybe give up.
			s.stderr = s.client.Stderr()
			s.client = nil
			if err := m.recordCrash(s); err != nil {
				s.mu.Unlock()
				return nil, s, err
			}
			m.logf("lsp: %s exited unexpectedly; restarting (%d/%d)", cfg.Name, len(s.crashes), m.opts.MaxRestarts)
		}
		s.binary = binary
		s.pending = &startOp{done: make(chan struct{})}
		go m.start(s, s.pending)
	}
	op := s.pending
	s.mu.Unlock()
	select {
	case <-op.done:
		return op.client, s, op.err
	case <-ctx.Done():
		return nil, s, ctx.Err()
	}
}

// recordCrash notes a crash or failed start; past MaxRestarts within the
// window the slot is marked failed. s.mu must be held.
func (m *Manager) recordCrash(s *slot) error {
	now := time.Now()
	s.crashes = append(s.crashes, now)
	recent := s.crashes[:0]
	for _, t := range s.crashes {
		if now.Sub(t) <= m.opts.RestartWindow {
			recent = append(recent, t)
		}
	}
	s.crashes = recent
	if len(recent) > m.opts.MaxRestarts {
		cause := ""
		if s.lastErr != nil {
			cause = ": " + s.lastErr.Error()
		}
		s.failed = fmt.Errorf("lsp: %s failed %d times in %s; not restarting%s%s", s.cfg.Name, len(recent),
			m.opts.RestartWindow, cause, stderrNote(s.stderr))
		m.logf("%v", s.failed)
		return s.failed
	}
	return nil
}

// start runs one server start and publishes the result.
func (m *Manager) start(s *slot, op *startOp) {
	ctx, cancel := context.WithTimeout(context.Background(), m.opts.StartTimeout)
	c, err := startClient(ctx, s.cfg, s.binary, s.root, m.opts.Logf)
	cancel()
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	s.mu.Lock()
	s.pending = nil
	s.starts++
	switch {
	case err != nil:
		s.lastErr = err
		if ferr := m.recordCrash(s); ferr != nil {
			err = ferr
		}
	case closed:
		go c.kill()
		c, err = nil, ErrManagerClosed
	default:
		s.client = c
		s.lastErr = nil
	}
	op.client, op.err = c, err
	s.mu.Unlock()
	close(op.done)
}

func stderrNote(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return " (stderr: " + clipTail(s, 400) + ")"
}

// withClient runs f against the server for path, retrying once on a fresh
// server when the connection dies mid-request (a crash).
func (m *Manager) withClient(ctx context.Context, path string, f func(*Client) error) error {
	for attempt := 0; ; attempt++ {
		c, _, err := m.client(ctx, path)
		if err != nil {
			return err
		}
		err = f(c)
		if err == nil || attempt > 0 || ctx.Err() != nil || !errors.Is(err, ErrConnClosed) {
			return err
		}
		// Wait for the process to be reaped so the restart sees it dead.
		select {
		case <-c.Done():
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Diagnostics returns the diagnostics for a file after syncing its current
// content to the server, waiting up to wait for the server to report on
// that content.
func (m *Manager) Diagnostics(ctx context.Context, path string, wait time.Duration) (*FileDiagnostics, error) {
	abs, err := m.resolve(path)
	if err != nil {
		return nil, err
	}
	if wait <= 0 {
		wait = m.opts.DiagnosticsWait
	}
	var res *FileDiagnostics
	err = m.withClient(ctx, abs, func(c *Client) error {
		r, err := c.Diagnostics(ctx, abs, wait, m.opts.DiagnosticsSettle)
		res = r
		return err
	})
	return res, err
}

// requestCtx bounds a navigation request.
func (m *Manager) requestCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, m.opts.RequestTimeout)
}

// Definition returns the definition locations of the symbol at a 1-based
// line and column.
func (m *Manager) Definition(ctx context.Context, path string, line, col int) ([]Location, error) {
	abs, err := m.resolve(path)
	if err != nil {
		return nil, err
	}
	ctx, cancel := m.requestCtx(ctx)
	defer cancel()
	var out []Location
	err = m.withClient(ctx, abs, func(c *Client) error {
		var err error
		out, err = c.Definition(ctx, abs, line, col)
		return err
	})
	return out, err
}

// References returns the references to the symbol at a 1-based line and
// column.
func (m *Manager) References(ctx context.Context, path string, line, col int, includeDecl bool) ([]Location, error) {
	abs, err := m.resolve(path)
	if err != nil {
		return nil, err
	}
	ctx, cancel := m.requestCtx(ctx)
	defer cancel()
	var out []Location
	err = m.withClient(ctx, abs, func(c *Client) error {
		var err error
		out, err = c.References(ctx, abs, line, col, includeDecl)
		return err
	})
	return out, err
}

// Hover returns hover documentation for the symbol at a 1-based line and
// column.
func (m *Manager) Hover(ctx context.Context, path string, line, col int) (string, error) {
	abs, err := m.resolve(path)
	if err != nil {
		return "", err
	}
	ctx, cancel := m.requestCtx(ctx)
	defer cancel()
	var out string
	err = m.withClient(ctx, abs, func(c *Client) error {
		var err error
		out, err = c.Hover(ctx, abs, line, col)
		return err
	})
	return out, err
}

// encodingFor reports the position encoding of the server for path, for
// rendering locations in other files.
func (m *Manager) encodingFor(path string) string {
	cfg, ok := m.ServerFor(path)
	if !ok {
		return EncodingUTF16
	}
	key := cfg.Name + "\x00" + m.detectRoot(path, cfg.RootMarkers)
	m.mu.Lock()
	s := m.slots[key]
	m.mu.Unlock()
	if s == nil {
		return EncodingUTF16
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client == nil {
		return EncodingUTF16
	}
	return s.client.Encoding()
}

// Available reports whether a server is configured and installed for path.
func (m *Manager) Available(path string) bool {
	cfg, ok := m.ServerFor(path)
	if !ok {
		return false
	}
	_, err := m.opts.LookPath(cfg.Command)
	return err == nil
}

// Warm starts the server for path in the background so the first
// diagnostics request does not pay for its startup. It never blocks.
func (m *Manager) Warm(path string) {
	abs, err := m.resolve(path)
	if err != nil || !m.Available(abs) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), m.opts.StartTimeout)
		defer cancel()
		if _, _, err := m.client(ctx, abs); err != nil && !errors.Is(err, ErrManagerClosed) {
			m.logf("lsp: warming %s: %v", filepath.Base(abs), err)
		}
	}()
}

// AfterEdit is meant to run after the agent writes or edits a file. It
// syncs the new content to the file's language server and returns the
// errors the server reports, formatted to append to the tool result, or ""
// when there are none, no server applies, or the server did not answer in
// time. It never fails the edit.
func (m *Manager) AfterEdit(ctx context.Context, absPath string) string {
	if m == nil {
		return ""
	}
	if _, ok := m.ServerFor(absPath); !ok || !m.Available(absPath) {
		return ""
	}
	// The whole call, including a first server start, is bounded so an
	// edit is never held up for long; a slow start keeps going in the
	// background and the next edit benefits.
	ctx, cancel := context.WithTimeout(ctx, m.opts.DiagnosticsWait+time.Second)
	defer cancel()
	res, err := m.Diagnostics(ctx, absPath, m.opts.DiagnosticsWait)
	if err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			m.logf("lsp: diagnostics for %s: %v", filepath.Base(absPath), err)
		}
		return ""
	}
	if !res.Fresh {
		return ""
	}
	var errs []Diagnostic
	for _, d := range res.Diagnostics {
		if d.EffectiveSeverity() == SeverityError {
			errs = append(errs, d)
		}
	}
	if len(errs) == 0 {
		return ""
	}
	res.Diagnostics = errs
	rel := m.rel(absPath)
	var b strings.Builder
	fmt.Fprintf(&b, "\n\n<diagnostics file=%q server=%q>\n", rel, res.Server)
	fmt.Fprintf(&b, "The language server reports %d error(s) in this file after the change:\n", len(errs))
	b.WriteString(FormatDiagnostics(res, m.opts.AfterEditLimit))
	b.WriteString("</diagnostics>")
	return b.String()
}

func (m *Manager) rel(abs string) string {
	if r, err := filepath.Rel(m.root, abs); err == nil && !strings.HasPrefix(r, "..") {
		return filepath.ToSlash(r)
	}
	return abs
}

// ServerStatus describes one server instance.
type ServerStatus struct {
	Name      string `json:"name"`
	Root      string `json:"root"`
	State     string `json:"state"` // running, starting, stopped, crashed, failed
	PID       int    `json:"pid,omitempty"`
	Starts    int    `json:"starts"`
	Crashes   int    `json:"crashes"`
	OpenDocs  int    `json:"open_docs"`
	Encoding  string `json:"encoding,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

// Status lists the servers started so far.
func (m *Manager) Status() []ServerStatus {
	m.mu.Lock()
	slots := make([]*slot, 0, len(m.slots))
	for _, s := range m.slots {
		slots = append(slots, s)
	}
	m.mu.Unlock()
	out := make([]ServerStatus, 0, len(slots))
	for _, s := range slots {
		s.mu.Lock()
		st := ServerStatus{Name: s.cfg.Name, Root: s.root, Starts: s.starts, Crashes: len(s.crashes)}
		switch {
		case s.failed != nil:
			st.State = "failed"
			st.LastError = s.failed.Error()
		case s.pending != nil:
			st.State = "starting"
		case s.client != nil && s.client.Alive():
			st.State = "running"
			st.PID = s.client.PID()
			st.OpenDocs = s.client.OpenDocuments()
			st.Encoding = s.client.Encoding()
		case s.client != nil:
			st.State = "crashed"
		default:
			st.State = "stopped"
		}
		if st.LastError == "" && s.lastErr != nil {
			st.LastError = s.lastErr.Error()
		}
		s.mu.Unlock()
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Root < out[j].Root
	})
	return out
}

// Close shuts every server down gracefully, killing those that do not
// exit in time.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	slots := make([]*slot, 0, len(m.slots))
	for _, s := range m.slots {
		slots = append(slots, s)
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range slots {
		s.mu.Lock()
		op := s.pending
		s.mu.Unlock()
		if op != nil {
			// A start in flight kills its server itself once it sees the
			// manager closed; wait for that so nothing outlives Close.
			select {
			case <-op.done:
			case <-ctx.Done():
			}
		}
		s.mu.Lock()
		c := s.client
		s.client = nil
		s.mu.Unlock()
		if c == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = c.Shutdown(ctx)
		}()
	}
	wg.Wait()
	return nil
}
