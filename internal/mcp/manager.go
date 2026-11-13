package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

// State is a server's connection state.
type State string

const (
	StateDisabled   State = "disabled"
	StateConnecting State = "connecting"
	StateConnected  State = "connected"
	StateFailed     State = "failed"
)

// Status describes one configured server, for the /mcp command.
type Status struct {
	Name      string
	Transport string // "stdio" or "http"
	Target    string // command line, or URL without credentials
	State     State
	// Server and ProtocolVersion come from the last successful initialize.
	Server          Implementation
	ProtocolVersion string
	// Tools are the server's own tool names, sorted.
	Tools []string
	// Err explains a failed state.
	Err error
	// Since is when the state last changed.
	Since time.Time
}

// Options configures a Manager.
type Options struct {
	// Dir is the working directory for stdio servers (the workspace root).
	Dir string
	// HTTPClient is used for HTTP servers; nil uses a default client.
	HTTPClient *http.Client
	// Logf receives diagnostics from every server. Nil discards them.
	Logf func(format string, args ...any)
	// OnChange runs (without any Manager lock held) whenever a server's
	// state or tool list changes, so the app can rebuild its tool registry
	// from Tools(). It may be called from several goroutines at once and
	// must not block for long.
	OnChange func()
	// Getenv resolves ${VAR} references; nil uses os.LookupEnv.
	Getenv func(string) (string, bool)

	shutdownGrace time.Duration
}

// Manager owns the connections to every configured MCP server.
type Manager struct {
	opts Options

	// ctx is cancelled by Close so in-flight connection attempts stop.
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.RWMutex
	servers map[string]*server
	started bool
	closed  bool
}

// NewManager returns an idle manager; call Start to connect.
func NewManager(opts Options) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{opts: opts, servers: map[string]*server{}, ctx: ctx, cancel: cancel}
}

// server is the Manager's handle on one configured server. It implements
// Caller, reconnecting on demand when the previous session died.
type server struct {
	m    *Manager
	name string
	cfg  ServerConfig

	connectMu sync.Mutex // one connection attempt at a time

	mu     sync.Mutex
	client *Client
	state  State
	err    error
	since  time.Time
}

// Start validates cfg and connects every enabled server concurrently,
// returning once each has connected or failed. A server that fails (bad
// config, missing binary, crash, timeout) is recorded in Status and does not
// affect the others, so Start only errors when called twice or after Close.
// ctx bounds the whole startup; each server is also bounded by its timeout.
func (m *Manager) Start(ctx context.Context, cfg Config) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	if m.started {
		m.mu.Unlock()
		return errors.New("mcp: manager already started")
	}
	m.started = true
	var toConnect []*server
	now := time.Now()
	for _, name := range cfg.Names() {
		sc := cfg[name]
		s := &server{m: m, name: name, cfg: sc, since: now}
		switch err := sc.Validate(name); {
		case err != nil:
			s.state, s.err = StateFailed, err
		case sc.Disabled:
			s.state = StateDisabled
		default:
			s.state = StateConnecting
			toConnect = append(toConnect, s)
		}
		m.servers[name] = s
	}
	m.mu.Unlock()

	var wg sync.WaitGroup
	for _, s := range toConnect {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = s.connect(ctx)
		}()
	}
	wg.Wait()
	m.changed()
	return nil
}

// Status reports every configured server, sorted by name.
func (m *Manager) Status() []Status {
	m.mu.RLock()
	servers := m.sortedServers()
	m.mu.RUnlock()
	out := make([]Status, 0, len(servers))
	for _, s := range servers {
		out = append(out, s.status())
	}
	return out
}

// Tools returns an adapter for every tool of every server that has
// connected, in server then tool order. Tools of a server whose session died
// stay listed; calling one reconnects it. Name collisions after sanitizing
// are resolved with a numeric suffix.
func (m *Manager) Tools() []tools.Tool {
	m.mu.RLock()
	servers := m.sortedServers()
	m.mu.RUnlock()
	var out []tools.Tool
	used := map[string]bool{}
	for _, s := range servers {
		s.mu.Lock()
		c := s.client
		s.mu.Unlock()
		if c == nil {
			continue
		}
		infos := c.Tools()
		sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
		for _, info := range infos {
			name := uniqueName(ToolName(s.name, info.Name), used)
			used[name] = true
			out = append(out, NewTool(s.name, name, info, s))
		}
	}
	return out
}

// Reconnect closes a server's session (if any) and connects it again, for
// a "/mcp reconnect <name>" command. A disabled server stays disabled.
func (m *Manager) Reconnect(ctx context.Context, name string) error {
	m.mu.RLock()
	s, ok := m.servers[name]
	closed := m.closed
	m.mu.RUnlock()
	if closed {
		return ErrClosed
	}
	if !ok {
		return fmt.Errorf("mcp: no server named %q", name)
	}
	if s.cfg.Disabled {
		return fmt.Errorf("mcp: server %q is disabled", name)
	}
	if err := s.cfg.Validate(name); err != nil {
		return err
	}
	s.connectMu.Lock()
	s.mu.Lock()
	old := s.client
	s.client = nil
	s.mu.Unlock()
	s.connectMu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	_, err := s.connect(ctx)
	return err
}

// Close disconnects every server. The manager cannot be restarted.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	servers := m.sortedServers()
	m.mu.Unlock()
	m.cancel()
	var wg sync.WaitGroup
	for _, s := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.mu.Lock()
			c := s.client
			s.client = nil
			if s.state == StateConnected || s.state == StateConnecting {
				s.setState(StateFailed, ErrClosed)
			}
			s.mu.Unlock()
			if c != nil {
				_ = c.Close()
			}
		}()
	}
	wg.Wait()
	return nil
}

// sortedServers lists servers by name; callers hold m.mu.
func (m *Manager) sortedServers() []*server {
	out := make([]*server, 0, len(m.servers))
	for _, name := range sortedKeys(m.servers) {
		out = append(out, m.servers[name])
	}
	return out
}

func (m *Manager) isClosed() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.closed
}

func (m *Manager) changed() {
	if m.opts.OnChange != nil {
		m.opts.OnChange()
	}
}

func (m *Manager) logf(format string, args ...any) {
	if m.opts.Logf != nil {
		m.opts.Logf(format, args...)
	}
}

// setState records a transition; callers hold s.mu.
func (s *server) setState(st State, err error) {
	s.state, s.err, s.since = st, err, time.Now()
}

func (s *server) status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{
		Name:      s.name,
		Transport: s.cfg.Transport(),
		Target:    s.cfg.Target(),
		State:     s.state,
		Err:       s.err,
		Since:     s.since,
	}
	if s.client != nil {
		info := s.client.ServerInfo()
		st.Server, st.ProtocolVersion = info.ServerInfo, info.ProtocolVersion
		for _, t := range s.client.Tools() {
			st.Tools = append(st.Tools, t.Name)
		}
		sort.Strings(st.Tools)
	}
	return st
}

// connect returns the live client, starting a new session if there is none
// or the previous one died.
func (s *server) connect(ctx context.Context) (*Client, error) {
	s.connectMu.Lock()
	defer s.connectMu.Unlock()
	if s.m.isClosed() {
		return nil, ErrClosed
	}
	s.mu.Lock()
	if s.client != nil && s.client.Alive() {
		c := s.client
		s.mu.Unlock()
		return c, nil
	}
	old := s.client
	s.setState(StateConnecting, nil)
	s.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(s.m.ctx, cancel)
	defer stop()
	c, err := Connect(ctx, s.name, s.cfg, ClientOptions{
		Dir:            s.m.opts.Dir,
		HTTPClient:     s.m.opts.HTTPClient,
		Logf:           s.m.opts.Logf,
		Getenv:         s.m.opts.Getenv,
		OnToolsChanged: func([]ToolInfo) { s.m.changed() },
		shutdownGrace:  s.m.opts.shutdownGrace,
	})

	s.mu.Lock()
	if err != nil {
		if s.m.isClosed() {
			err = ErrClosed
		}
		s.setState(StateFailed, err)
		s.mu.Unlock()
		s.m.logf("mcp[%s]: %v", s.name, err)
		return nil, err
	}
	if s.m.isClosed() {
		s.mu.Unlock()
		_ = c.Close()
		return nil, ErrClosed
	}
	s.client = c
	s.setState(StateConnected, nil)
	s.mu.Unlock()
	go s.watch(c)
	s.m.changed()
	return c, nil
}

// watch records the failure when a session dies on its own.
func (s *server) watch(c *Client) {
	<-c.Done()
	s.mu.Lock()
	if s.client != c || s.m.isClosed() {
		s.mu.Unlock()
		return
	}
	err := c.Err()
	if err == nil {
		err = errors.New("mcp: connection lost")
	}
	s.setState(StateFailed, err)
	s.mu.Unlock()
	s.m.logf("mcp[%s]: disconnected: %v", s.name, err)
	s.m.changed()
}

// CallTool implements Caller. If the session died since the last call, it
// reconnects once first. A call whose session dies mid-flight is not
// retried, because the tool may already have had side effects.
func (s *server) CallTool(ctx context.Context, name string, args json.RawMessage) (*ToolResult, error) {
	s.mu.Lock()
	c := s.client
	s.mu.Unlock()
	if c == nil || !c.Alive() {
		var err error
		if c, err = s.connect(ctx); err != nil {
			return nil, fmt.Errorf("MCP server %q is not connected: %w", s.name, err)
		}
	}
	return c.CallTool(ctx, name, args)
}

// uniqueName appends _2, _3, … (keeping within MaxToolNameLen) until name is
// unused.
func uniqueName(name string, used map[string]bool) string {
	if !used[name] {
		return name
	}
	for i := 2; ; i++ {
		suffix := fmt.Sprintf("_%d", i)
		base := name
		if len(base)+len(suffix) > MaxToolNameLen {
			base = base[:MaxToolNameLen-len(suffix)]
		}
		if cand := base + suffix; !used[cand] {
			return cand
		}
	}
}

// FormatStatus renders statuses as a plain-text report for /mcp.
func FormatStatus(statuses []Status) string {
	if len(statuses) == 0 {
		return "No MCP servers configured. Add them under \"mcp_servers\" in the config."
	}
	var b strings.Builder
	for i, st := range statuses {
		if i > 0 {
			b.WriteByte('\n')
		}
		mark := map[State]string{StateConnected: "✓", StateFailed: "✗", StateConnecting: "…", StateDisabled: "-"}[st.State]
		fmt.Fprintf(&b, "%s %s  %s  %s %s", mark, st.Name, st.State, st.Transport, st.Target)
		if st.State == StateConnected {
			fmt.Fprintf(&b, "  %d tool%s", len(st.Tools), plural(len(st.Tools)))
			if srv := st.Server.String(); srv != "" {
				fmt.Fprintf(&b, "  (%s, protocol %s)", srv, st.ProtocolVersion)
			}
			if len(st.Tools) > 0 {
				fmt.Fprintf(&b, "\n    %s", strings.Join(st.Tools, ", "))
			}
		}
		if st.Err != nil && st.State == StateFailed {
			msg := strings.ReplaceAll(strings.TrimSpace(st.Err.Error()), "\n", "\n    ")
			fmt.Fprintf(&b, "\n    error: %s", msg)
		}
	}
	return b.String()
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
