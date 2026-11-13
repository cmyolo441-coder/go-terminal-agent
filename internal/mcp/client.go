// Package mcp is a Model Context Protocol client. It connects to MCP servers
// over stdio (a spawned process) or Streamable HTTP, performs the initialize
// handshake, lists the servers' tools and exposes each one as a tools.Tool so
// the agent can call it behind the permission gate.
//
// The package has three layers:
//
//   - Client: one session with one server (JSON-RPC, lifecycle, tools).
//   - Manager: every configured server, started concurrently, with status
//     reporting and on-demand reconnection after a crash.
//   - the tool adapter: wraps a server tool as tools.Tool, named
//     "mcp__<server>__<tool>".
package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/version"
)

// ProtocolVersion is the MCP revision the client requests.
const ProtocolVersion = "2025-06-18"

// ClientName is sent as clientInfo.name during initialize.
const ClientName = "gta"

// supportedVersions are the revisions the client can speak if the server
// answers initialize with an older one.
var supportedVersions = map[string]bool{
	"2025-06-18": true,
	"2025-03-26": true,
	"2024-11-05": true,
}

// maxListPages guards against servers that paginate forever.
const maxListPages = 1000

// Implementation names a client or server.
type Implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

// String renders "name version".
func (i Implementation) String() string {
	return strings.TrimSpace(i.Name + " " + i.Version)
}

// InitializeResult is the server's answer to initialize.
type InitializeResult struct {
	ProtocolVersion string                     `json:"protocolVersion"`
	Capabilities    map[string]json.RawMessage `json:"capabilities"`
	ServerInfo      Implementation             `json:"serverInfo"`
	Instructions    string                     `json:"instructions,omitempty"`
}

// ToolInfo describes one tool a server offers.
type ToolInfo struct {
	Name        string           `json:"name"`
	Title       string           `json:"title,omitempty"`
	Description string           `json:"description,omitempty"`
	InputSchema json.RawMessage  `json:"inputSchema,omitempty"`
	Annotations *ToolAnnotations `json:"annotations,omitempty"`
}

// ToolAnnotations are a server's hints about a tool's behaviour. They are
// informational only: clients must not trust them for safety decisions, so
// every MCP tool still goes through the permission gate.
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

// DisplayTitle prefers the human title, then the annotation title, then the
// name.
func (t ToolInfo) DisplayTitle() string {
	if t.Title != "" {
		return t.Title
	}
	if t.Annotations != nil && t.Annotations.Title != "" {
		return t.Annotations.Title
	}
	return t.Name
}

// ResourceContents is the payload of an embedded resource content block.
type ResourceContents struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
}

// Content is one block of a tool result.
type Content struct {
	Type     string            `json:"type"`
	Text     string            `json:"text,omitempty"`
	Data     string            `json:"data,omitempty"`
	MimeType string            `json:"mimeType,omitempty"`
	Resource *ResourceContents `json:"resource,omitempty"`
	URI      string            `json:"uri,omitempty"`
	Name     string            `json:"name,omitempty"`
}

// ToolResult is the result of tools/call.
type ToolResult struct {
	Content           []Content       `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
}

// Text renders the result for the model: text blocks verbatim, embedded
// text resources with a header line, and binary content (images, audio,
// blobs) as a short note, joined by newlines. Structured content is used
// when there are no content blocks.
func (r *ToolResult) Text() string {
	parts := make([]string, 0, len(r.Content))
	for _, c := range r.Content {
		switch c.Type {
		case "text":
			parts = append(parts, c.Text)
		case "image", "audio":
			parts = append(parts, fmt.Sprintf("[%s: %s, %s]", c.Type, orUnknown(c.MimeType), sizeOfBase64(c.Data)))
		case "resource":
			if c.Resource == nil {
				parts = append(parts, "[resource: (empty)]")
				continue
			}
			res := c.Resource
			switch {
			case res.Text != "":
				parts = append(parts, fmt.Sprintf("[resource: %s]\n%s", res.URI, res.Text))
			case res.Blob != "":
				parts = append(parts, fmt.Sprintf("[resource: %s, %s, %s binary]", res.URI, orUnknown(res.MimeType), sizeOfBase64(res.Blob)))
			default:
				parts = append(parts, fmt.Sprintf("[resource: %s]", res.URI))
			}
		case "resource_link":
			label := c.Name
			if label == "" {
				label = c.URI
			}
			note := fmt.Sprintf("[resource link: %s <%s>", label, c.URI)
			if c.MimeType != "" {
				note += ", " + c.MimeType
			}
			parts = append(parts, note+"]")
		default:
			parts = append(parts, fmt.Sprintf("[unsupported content type %q]", c.Type))
		}
	}
	if len(parts) == 0 && len(r.StructuredContent) > 0 && string(r.StructuredContent) != "null" {
		return string(r.StructuredContent)
	}
	return strings.Join(parts, "\n")
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown type"
	}
	return s
}

func sizeOfBase64(data string) string {
	n := base64.StdEncoding.DecodedLen(len(strings.TrimRight(data, "=")))
	if b, err := base64.StdEncoding.DecodeString(data); err == nil {
		n = len(b)
	}
	return humanBytes(n)
}

func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}

// ClientOptions tunes a Client.
type ClientOptions struct {
	// Dir is the working directory for stdio servers; empty inherits ours.
	Dir string
	// HTTPClient is used for HTTP servers; nil uses a default client.
	HTTPClient *http.Client
	// Logf receives diagnostic messages (server log notifications, dropped
	// messages, retries). Nil discards them.
	Logf func(format string, args ...any)
	// OnToolsChanged runs after the tool list was refreshed because the
	// server sent notifications/tools/list_changed.
	OnToolsChanged func(tools []ToolInfo)
	// Getenv resolves ${VAR} references in the config; nil uses os.LookupEnv.
	Getenv func(string) (string, bool)

	// shutdownGrace overrides the stdio shutdown grace period (tests).
	shutdownGrace time.Duration
}

// Client is a session with one MCP server. It is safe for concurrent use.
type Client struct {
	name    string
	cfg     ServerConfig
	opts    ClientOptions
	timeout time.Duration
	logf    func(string, ...any)

	conn  *conn
	http  *httpTransport
	stdio *stdioTransport

	// ctx is cancelled by Close; background refreshes use it.
	ctx    context.Context
	cancel context.CancelFunc
	bg     sync.WaitGroup

	initMu sync.Mutex // serialises (re-)initialization

	mu         sync.RWMutex
	server     InitializeResult
	tools      []ToolInfo
	refreshing bool
	dirty      bool
	closed     bool
}

// Connect starts the transport, performs the initialize handshake and loads
// the tool list. The returned client must be closed. The handshake is bounded
// by ctx and by the server's timeout.
func Connect(ctx context.Context, name string, cfg ServerConfig, opts ClientOptions) (*Client, error) {
	if err := cfg.Validate(name); err != nil {
		return nil, err
	}
	getenv := opts.Getenv
	if getenv == nil {
		getenv = osLookup
	}
	expanded, err := cfg.expanded(getenv)
	if err != nil {
		return nil, fmt.Errorf("mcp server %q: %w", name, err)
	}
	logf := func(format string, args ...any) {
		if opts.Logf != nil {
			opts.Logf("mcp[%s]: "+format, append([]any{name}, args...)...)
		}
	}
	c := &Client{name: name, cfg: expanded, opts: opts, timeout: expanded.Timeout(), logf: logf}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	c.conn = newConn(c.timeout, logf)
	c.conn.onNotify = c.onNotify

	var t transport
	if expanded.URL != "" {
		c.http = newHTTPTransport(expanded, opts.HTTPClient, c.conn.handle, logf)
		t = c.http
	} else {
		c.stdio = newStdioTransport(expanded, opts.Dir, opts.shutdownGrace, c.conn.handle, logf)
		t = c.stdio
	}
	c.conn.bind(t)
	if err := t.start(); err != nil {
		c.Close()
		return nil, fmt.Errorf("mcp server %q: %w", name, err)
	}

	hctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := c.initialize(hctx); err != nil {
		err = c.explain(err)
		c.Close()
		return nil, fmt.Errorf("mcp server %q: initialize: %w", name, err)
	}
	if _, err := c.RefreshTools(hctx); err != nil {
		err = c.explain(err)
		c.Close()
		return nil, fmt.Errorf("mcp server %q: list tools: %w", name, err)
	}
	if c.http != nil {
		c.http.listen()
	}
	return c, nil
}

// explain makes a stdio failure self-describing: when the process died, its
// exit status and recent stderr replace the generic error, and a timeout
// carries the stderr tail too, since it often says why the server hung.
func (c *Client) explain(err error) error {
	if c.stdio == nil || err == nil || strings.Contains(err.Error(), "stderr:") {
		return err
	}
	if terr := c.stdio.err(); terr != nil && !errors.Is(terr, ErrClosed) {
		return terr
	}
	if errors.Is(err, ErrTimeout) {
		if tail := c.stdio.stderrTail(10); tail != "" {
			return fmt.Errorf("%w; stderr:\n%s", err, tail)
		}
	}
	return err
}

// initialize runs the handshake: initialize, then notifications/initialized.
func (c *Client) initialize(ctx context.Context) error {
	c.initMu.Lock()
	defer c.initMu.Unlock()
	return c.initializeLocked(ctx)
}

func (c *Client) initializeLocked(ctx context.Context) error {
	params := map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      Implementation{Name: ClientName, Title: "GTA terminal agent", Version: version.Version},
	}
	var res InitializeResult
	if err := c.conn.call(ctx, "initialize", params, &res, 0); err != nil {
		return err
	}
	if !supportedVersions[res.ProtocolVersion] {
		return fmt.Errorf("server speaks unsupported protocol version %q (client supports %s)", res.ProtocolVersion, ProtocolVersion)
	}
	if c.http != nil {
		c.http.setProtocolVersion(res.ProtocolVersion)
	}
	if err := c.conn.notify(ctx, "notifications/initialized", nil); err != nil {
		return fmt.Errorf("notifications/initialized: %w", err)
	}
	c.mu.Lock()
	c.server = res
	c.mu.Unlock()
	return nil
}

// reinitialize starts a new HTTP session after the server expired the old
// one. Concurrent callers share one handshake: whoever arrives after it has
// completed sees a new session id and returns at once.
func (c *Client) reinitialize(ctx context.Context, staleSession string) error {
	if c.http == nil {
		return ErrSessionExpired
	}
	c.initMu.Lock()
	defer c.initMu.Unlock()
	if c.http.session() != staleSession {
		return nil
	}
	c.http.resetSession()
	c.logf("session expired; starting a new one")
	return c.initializeLocked(ctx)
}

// request runs a call, transparently re-initializing once if an HTTP server
// reports that the session expired.
func (c *Client) request(ctx context.Context, method string, params, out any) error {
	sid := ""
	if c.http != nil {
		sid = c.http.session()
	}
	err := c.conn.call(ctx, method, params, out, 0)
	if errors.Is(err, ErrSessionExpired) {
		if rerr := c.reinitialize(ctx, sid); rerr != nil {
			return fmt.Errorf("%w; re-initialize failed: %v", err, rerr)
		}
		err = c.conn.call(ctx, method, params, out, 0)
	}
	return c.explain(err)
}

// Name is the configured server name.
func (c *Client) Name() string { return c.name }

// Config returns the server's config with variables expanded.
func (c *Client) Config() ServerConfig { return c.cfg }

// ServerInfo returns what the server reported during initialize.
func (c *Client) ServerInfo() InitializeResult {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.server
}

// Tools returns the cached tool list.
func (c *Client) Tools() []ToolInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]ToolInfo(nil), c.tools...)
}

// Alive reports whether the connection is still usable.
func (c *Client) Alive() bool { return c.conn.alive() }

// Err explains why the connection stopped, or returns nil while it is alive.
func (c *Client) Err() error { return c.explain(c.conn.err()) }

// Done is closed when the connection stops.
func (c *Client) Done() <-chan struct{} { return c.conn.done() }

// Stderr returns the last lines a stdio server wrote to stderr.
func (c *Client) Stderr() string {
	if c.stdio == nil {
		return ""
	}
	return c.stdio.stderrTail(20)
}

// ListTools fetches every page of tools/list.
func (c *Client) ListTools(ctx context.Context) ([]ToolInfo, error) {
	var all []ToolInfo
	seen := map[string]bool{}
	cursor := ""
	for page := 0; ; page++ {
		if page >= maxListPages {
			return nil, fmt.Errorf("tools/list returned more than %d pages", maxListPages)
		}
		var params any
		if cursor != "" {
			params = map[string]any{"cursor": cursor}
		}
		var res struct {
			Tools      []ToolInfo `json:"tools"`
			NextCursor string     `json:"nextCursor,omitempty"`
		}
		if err := c.request(ctx, "tools/list", params, &res); err != nil {
			return nil, err
		}
		all = append(all, res.Tools...)
		if res.NextCursor == "" {
			break
		}
		if seen[res.NextCursor] {
			return nil, fmt.Errorf("tools/list repeated cursor %q", clip(res.NextCursor, 60))
		}
		seen[res.NextCursor] = true
		cursor = res.NextCursor
	}
	valid := all[:0]
	names := map[string]bool{}
	for _, t := range all {
		if strings.TrimSpace(t.Name) == "" {
			c.logf("ignoring a tool without a name")
			continue
		}
		if names[t.Name] {
			c.logf("ignoring duplicate tool %q", t.Name)
			continue
		}
		names[t.Name] = true
		valid = append(valid, t)
	}
	return valid, nil
}

// RefreshTools reloads the tool list into the cache. A server that does not
// declare the tools capability and rejects tools/list has no tools.
func (c *Client) RefreshTools(ctx context.Context) ([]ToolInfo, error) {
	ts, err := c.ListTools(ctx)
	if err != nil {
		var rpc *RPCError
		_, declared := c.ServerInfo().Capabilities["tools"]
		if !declared && errors.As(err, &rpc) && rpc.Code == CodeMethodNotFound {
			ts, err = nil, nil
		} else {
			return nil, err
		}
	}
	c.mu.Lock()
	c.tools = ts
	c.mu.Unlock()
	return append([]ToolInfo(nil), ts...), nil
}

// CallTool runs a tool. args must be a JSON object (or empty for none). A
// tool-level failure is reported in the result's IsError, not as an error.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (*ToolResult, error) {
	params := map[string]any{"name": name}
	if len(args) > 0 {
		params["arguments"] = args
	} else {
		params["arguments"] = map[string]any{}
	}
	var res ToolResult
	if err := c.request(ctx, "tools/call", params, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Ping checks that the server is responsive.
func (c *Client) Ping(ctx context.Context) error {
	return c.request(ctx, "ping", nil, nil)
}

// Close ends the session and releases its process or connections.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	c.cancel()
	err := c.conn.close()
	waitTimeout(&c.bg, 5*time.Second)
	return err
}

// onNotify handles server notifications. It runs on the transport's read
// path, so anything slow happens in a goroutine.
func (c *Client) onNotify(method string, params json.RawMessage) {
	switch method {
	case "notifications/tools/list_changed":
		c.scheduleRefresh()
	case "notifications/message":
		var m struct {
			Level  string          `json:"level"`
			Logger string          `json:"logger"`
			Data   json.RawMessage `json:"data"`
		}
		if json.Unmarshal(params, &m) == nil {
			data := string(m.Data)
			var s string
			if json.Unmarshal(m.Data, &s) == nil {
				data = s
			}
			if m.Logger != "" {
				data = m.Logger + ": " + data
			}
			c.logf("[%s] %s", orUnknown(m.Level), clip(data, 2000))
		}
	default:
		// progress, cancelled, resources/prompts changes: nothing to do for
		// a client that only uses tools.
	}
}

// scheduleRefresh coalesces list_changed bursts into as few tools/list
// round trips as possible, one at a time.
func (c *Client) scheduleRefresh() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	if c.refreshing {
		c.dirty = true
		c.mu.Unlock()
		return
	}
	c.refreshing = true
	c.bg.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.bg.Done()
		for {
			ctx, cancel := context.WithTimeout(c.ctx, c.timeout)
			ts, err := c.RefreshTools(ctx)
			cancel()
			if err != nil {
				if c.ctx.Err() == nil {
					c.logf("refresh tools after list_changed: %v", err)
				}
			} else if c.opts.OnToolsChanged != nil {
				c.opts.OnToolsChanged(ts)
			}
			c.mu.Lock()
			if !c.dirty || c.closed {
				c.refreshing = false
				c.mu.Unlock()
				return
			}
			c.dirty = false
			c.mu.Unlock()
		}
	}()
}
