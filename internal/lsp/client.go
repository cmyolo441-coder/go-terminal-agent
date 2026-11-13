package lsp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// maxDocumentSize bounds the files synchronised to a server.
const maxDocumentSize = 16 << 20

// ServerConfig describes one language server.
type ServerConfig struct {
	// Name identifies the server, e.g. "gopls".
	Name string `json:"name"`
	// Command is the executable, looked up on PATH; Args follow it.
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	// Env adds environment variables for the server process.
	Env map[string]string `json:"env,omitempty"`
	// Extensions are the file extensions the server handles (".go").
	Extensions []string `json:"extensions"`
	// LanguageIDs overrides the languageId sent for an extension.
	LanguageIDs map[string]string `json:"language_ids,omitempty"`
	// RootMarkers are files whose directory is the server's workspace
	// root (go.mod, package.json, Cargo.toml …). The nearest one above a
	// file, within the workspace, wins; without one the workspace root is
	// used.
	RootMarkers []string `json:"root_markers,omitempty"`
	// InitializationOptions are sent with initialize.
	InitializationOptions json.RawMessage `json:"initialization_options,omitempty"`
	// Settings answer workspace/configuration requests, looked up by
	// section ("gopls", "python.analysis").
	Settings json.RawMessage `json:"settings,omitempty"`
	// Disabled turns a default server off.
	Disabled bool `json:"disabled,omitempty"`
}

// handles reports whether the server is configured for a file extension.
func (c ServerConfig) handles(ext string) bool {
	for _, e := range c.Extensions {
		if strings.EqualFold(e, ext) || strings.EqualFold("."+strings.TrimPrefix(e, "."), ext) {
			return true
		}
	}
	return false
}

// DefaultServers is the built-in server table.
func DefaultServers() []ServerConfig {
	return []ServerConfig{
		{Name: "gopls", Command: "gopls", Extensions: []string{".go"}, RootMarkers: []string{"go.work", "go.mod"}},
		{Name: "typescript-language-server", Command: "typescript-language-server", Args: []string{"--stdio"},
			Extensions:  []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".mts", ".cts"},
			RootMarkers: []string{"tsconfig.json", "jsconfig.json", "package.json"}},
		{Name: "pyright", Command: "pyright-langserver", Args: []string{"--stdio"}, Extensions: []string{".py", ".pyi"},
			RootMarkers: []string{"pyrightconfig.json", "pyproject.toml", "setup.py", "setup.cfg", "requirements.txt"}},
		{Name: "rust-analyzer", Command: "rust-analyzer", Extensions: []string{".rs"}, RootMarkers: []string{"Cargo.toml"}},
		{Name: "clangd", Command: "clangd", Extensions: []string{".c", ".h", ".cc", ".cpp", ".cxx", ".c++", ".hpp", ".hh", ".hxx"},
			RootMarkers: []string{"compile_commands.json", "compile_flags.txt", ".clangd", "CMakeLists.txt", "Makefile"}},
	}
}

// serverCaps is the subset of server capabilities the client uses.
type serverCaps struct {
	openClose  bool
	change     int // 0 none, 1 full, 2 incremental
	save       bool
	saveText   bool
	pull       bool
	definition bool
	references bool
	hover      bool
	encoding   string
}

func parseCaps(raw json.RawMessage) serverCaps {
	caps := serverCaps{openClose: true, change: 1, encoding: EncodingUTF16}
	var c struct {
		PositionEncoding   string          `json:"positionEncoding"`
		TextDocumentSync   json.RawMessage `json:"textDocumentSync"`
		DiagnosticProvider json.RawMessage `json:"diagnosticProvider"`
		DefinitionProvider json.RawMessage `json:"definitionProvider"`
		ReferencesProvider json.RawMessage `json:"referencesProvider"`
		HoverProvider      json.RawMessage `json:"hoverProvider"`
	}
	if json.Unmarshal(raw, &c) != nil {
		return caps
	}
	switch c.PositionEncoding {
	case EncodingUTF8, EncodingUTF32:
		caps.encoding = c.PositionEncoding
	}
	sync := bytes.TrimSpace(c.TextDocumentSync)
	if len(sync) > 0 && sync[0] == '{' {
		var s struct {
			OpenClose *bool           `json:"openClose"`
			Change    *int            `json:"change"`
			Save      json.RawMessage `json:"save"`
		}
		if json.Unmarshal(sync, &s) == nil {
			if s.OpenClose != nil {
				caps.openClose = *s.OpenClose
			}
			if s.Change != nil {
				caps.change = *s.Change
			}
			save := bytes.TrimSpace(s.Save)
			switch {
			case bytes.Equal(save, []byte("true")):
				caps.save = true
			case len(save) > 0 && save[0] == '{':
				caps.save = true
				var so struct {
					IncludeText bool `json:"includeText"`
				}
				if json.Unmarshal(save, &so) == nil {
					caps.saveText = so.IncludeText
				}
			}
		}
	} else if len(sync) > 0 {
		var kind int
		if json.Unmarshal(sync, &kind) == nil {
			caps.change = kind
			caps.openClose = kind != 0
		}
	}
	caps.pull = provided(c.DiagnosticProvider)
	caps.definition = provided(c.DefinitionProvider)
	caps.references = provided(c.ReferencesProvider)
	caps.hover = provided(c.HoverProvider)
	return caps
}

// provided interprets a capability that is a boolean or an options object.
func provided(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && !bytes.Equal(raw, []byte("false")) && !bytes.Equal(raw, []byte("null"))
}

// document is the client's view of an open file.
type document struct {
	uri     string
	version int
	text    string
}

// diagState is the latest published diagnostics for a file.
type diagState struct {
	version int // -1 when the server did not say
	seq     uint64
	items   []Diagnostic
	at      time.Time
}

// ringBuffer keeps the tail of a server's stderr for error reports.
type ringBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.max {
		r.buf = append([]byte(nil), r.buf[len(r.buf)-r.max:]...)
	}
	return len(p), nil
}

func (r *ringBuffer) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.buf)
}

// Client is a connection to one running language server.
type Client struct {
	cfg    ServerConfig
	root   string
	cmd    *exec.Cmd
	conn   *Conn
	caps   serverCaps
	stderr *ringBuffer
	logf   func(string, ...any)

	exited  chan struct{}
	exitErr error

	docMu sync.Mutex // serializes document sync so versions stay ordered
	docs  map[string]*document

	mu      sync.Mutex
	diags   map[string]*diagState
	diagSeq uint64
	diagCh  chan struct{}
	started time.Time
}

// startClient launches the server and completes the initialize handshake.
func startClient(ctx context.Context, cfg ServerConfig, binary, root string, logf func(string, ...any)) (*Client, error) {
	cmd := exec.Command(binary, cfg.Args...)
	cmd.Dir = root
	cmd.Env = os.Environ()
	for k, v := range cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := &ringBuffer{max: 16 << 10}
	cmd.Stderr = stderr
	configureProcess(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w", cfg.Name, err)
	}
	c := &Client{
		cfg:     cfg,
		root:    root,
		cmd:     cmd,
		stderr:  stderr,
		logf:    logf,
		exited:  make(chan struct{}),
		docs:    map[string]*document{},
		diags:   map[string]*diagState{},
		diagCh:  make(chan struct{}),
		started: time.Now(),
	}
	c.conn = NewConn(stdout, stdin, stdin, ConnOptions{
		OnRequest:      c.handleRequest,
		OnNotification: c.handleNotification,
		Logf:           logf,
	})
	go func() {
		err := cmd.Wait()
		c.mu.Lock()
		c.exitErr = err
		c.mu.Unlock()
		c.conn.fail(fmt.Errorf("lsp: %s exited: %v", cfg.Name, exitDescription(err)))
		close(c.exited)
	}()
	if err := c.initialize(ctx); err != nil {
		c.kill()
		if tail := strings.TrimSpace(c.stderr.String()); tail != "" {
			err = fmt.Errorf("%w (stderr: %s)", err, clipTail(tail, 500))
		}
		return nil, fmt.Errorf("initializing %s: %w", cfg.Name, err)
	}
	return c, nil
}

func exitDescription(err error) string {
	if err == nil {
		return "exit status 0"
	}
	return err.Error()
}

func clipTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

func (c *Client) initialize(ctx context.Context) error {
	rootURI := PathToURI(c.root)
	params := map[string]any{
		"processId":  os.Getpid(),
		"clientInfo": map[string]any{"name": "gta"},
		"rootUri":    rootURI,
		"rootPath":   c.root,
		"workspaceFolders": []map[string]any{
			{"uri": rootURI, "name": filepath.Base(c.root)},
		},
		"capabilities": map[string]any{
			"general": map[string]any{"positionEncodings": []string{EncodingUTF8, EncodingUTF16}},
			"workspace": map[string]any{
				"configuration":          true,
				"workspaceFolders":       true,
				"didChangeConfiguration": map[string]any{"dynamicRegistration": false},
			},
			"window": map[string]any{"workDoneProgress": true},
			"textDocument": map[string]any{
				"synchronization": map[string]any{"didSave": true, "dynamicRegistration": false},
				"publishDiagnostics": map[string]any{
					"relatedInformation": true,
					"versionSupport":     true,
					"tagSupport":         map[string]any{"valueSet": []int{1, 2}},
				},
				"diagnostic": map[string]any{"dynamicRegistration": false},
				"definition": map[string]any{"linkSupport": true},
				"references": map[string]any{},
				"hover":      map[string]any{"contentFormat": []string{"markdown", "plaintext"}},
			},
		},
	}
	if len(bytes.TrimSpace(c.cfg.InitializationOptions)) > 0 {
		params["initializationOptions"] = c.cfg.InitializationOptions
	}
	var res struct {
		Capabilities json.RawMessage `json:"capabilities"`
	}
	if err := c.conn.Call(ctx, "initialize", params, &res); err != nil {
		return err
	}
	c.caps = parseCaps(res.Capabilities)
	if err := c.conn.Notify("initialized", map[string]any{}); err != nil {
		return err
	}
	if len(bytes.TrimSpace(c.cfg.Settings)) > 0 {
		if err := c.conn.Notify("workspace/didChangeConfiguration", map[string]any{"settings": c.cfg.Settings}); err != nil {
			return err
		}
	}
	return nil
}

// Alive reports whether the server process and connection are up.
func (c *Client) Alive() bool {
	select {
	case <-c.exited:
		return false
	case <-c.conn.Done():
		return false
	default:
		return true
	}
}

// Done is closed when the server exits.
func (c *Client) Done() <-chan struct{} { return c.exited }

// Encoding is the negotiated position encoding.
func (c *Client) Encoding() string { return c.caps.encoding }

// PID is the server's process id.
func (c *Client) PID() int {
	if c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

// Stderr returns the tail of the server's standard error.
func (c *Client) Stderr() string { return c.stderr.String() }

// Shutdown asks the server to exit cleanly, then kills it if it does not.
func (c *Client) Shutdown(ctx context.Context) error {
	if !c.Alive() {
		return nil
	}
	sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	err := c.conn.Call(sctx, "shutdown", nil, nil)
	cancel()
	if err == nil {
		_ = c.conn.Notify("exit", nil)
	}
	select {
	case <-c.exited:
		return nil
	case <-time.After(2 * time.Second):
	case <-ctx.Done():
	}
	c.kill()
	return err
}

func (c *Client) kill() {
	c.conn.Close()
	if c.cmd.Process != nil {
		killProcess(c.cmd)
	}
	select {
	case <-c.exited:
	case <-time.After(5 * time.Second):
	}
}

// handleRequest answers server-initiated requests.
func (c *Client) handleRequest(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "workspace/configuration":
		var p struct {
			Items []struct {
				ScopeURI string `json:"scopeUri"`
				Section  string `json:"section"`
			} `json:"items"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &ResponseError{Code: CodeInvalidParams, Message: err.Error()}
		}
		out := make([]any, len(p.Items))
		for i, it := range p.Items {
			out[i] = c.settingsSection(it.Section)
		}
		return out, nil
	case "window/workDoneProgress/create", "client/registerCapability", "client/unregisterCapability":
		return nil, nil
	case "workspace/workspaceFolders":
		return []map[string]any{{"uri": PathToURI(c.root), "name": filepath.Base(c.root)}}, nil
	case "window/showMessageRequest":
		return nil, nil
	case "window/showDocument":
		return map[string]any{"success": false}, nil
	case "workspace/applyEdit":
		// The agent edits files through its own audited tools only.
		return map[string]any{"applied": false, "failureReason": "the client does not apply server edits"}, nil
	case "workspace/semanticTokens/refresh", "workspace/inlayHint/refresh", "workspace/codeLens/refresh",
		"workspace/diagnostic/refresh", "workspace/inlineValue/refresh", "workspace/foldingRange/refresh":
		return nil, nil
	}
	return nil, &ResponseError{Code: CodeMethodNotFound, Message: "method not supported: " + method}
}

// settingsSection looks up a dotted section in the configured settings.
func (c *Client) settingsSection(section string) any {
	raw := bytes.TrimSpace(c.cfg.Settings)
	if len(raw) == 0 {
		return nil
	}
	var cur any
	if json.Unmarshal(raw, &cur) != nil {
		return nil
	}
	if section == "" {
		return cur
	}
	for _, part := range strings.Split(section, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur, ok = m[part]
		if !ok {
			// Some configs use the dotted key verbatim.
			if v, ok := m[section]; ok {
				return v
			}
			return nil
		}
	}
	return cur
}

// handleNotification stores diagnostics; other notifications are logs.
func (c *Client) handleNotification(method string, params json.RawMessage) {
	switch method {
	case "textDocument/publishDiagnostics":
		var p publishDiagnosticsParams
		if err := json.Unmarshal(params, &p); err != nil {
			c.logf("lsp: %s: bad publishDiagnostics: %v", c.cfg.Name, err)
			return
		}
		path, err := URIToPath(p.URI)
		if err != nil {
			return
		}
		version := -1
		if p.Version != nil {
			version = *p.Version
		}
		c.storeDiagnostics(path, version, p.Diagnostics)
	case "window/showMessage", "window/logMessage":
		var p struct {
			Type    int    `json:"type"`
			Message string `json:"message"`
		}
		if json.Unmarshal(params, &p) == nil && p.Type == 1 {
			c.logf("lsp: %s: %s", c.cfg.Name, p.Message)
		}
	}
}

func (c *Client) storeDiagnostics(path string, version int, items []Diagnostic) {
	c.mu.Lock()
	c.diagSeq++
	c.diags[path] = &diagState{version: version, seq: c.diagSeq, items: items, at: time.Now()}
	close(c.diagCh)
	c.diagCh = make(chan struct{})
	c.mu.Unlock()
}

// syncResult describes what syncDocument sent.
type syncResult struct {
	version int
	changed bool
	text    string
}

// syncDocument makes the server's copy of path match the disk: didOpen on
// first use, didChange (full text) when the content differs, and didSave
// after a change when the server wants it. A deleted file is closed.
func (c *Client) syncDocument(path string) (syncResult, error) {
	c.docMu.Lock()
	defer c.docMu.Unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if doc := c.docs[path]; doc != nil {
				delete(c.docs, path)
				_ = c.conn.Notify("textDocument/didClose", map[string]any{"textDocument": textDocumentIdentifier{URI: doc.uri}})
			}
			c.mu.Lock()
			delete(c.diags, path)
			c.mu.Unlock()
		}
		return syncResult{}, err
	}
	if len(data) > maxDocumentSize {
		return syncResult{}, fmt.Errorf("%s is too large for the language server (%d bytes)", path, len(data))
	}
	text := string(data)
	doc := c.docs[path]
	if doc == nil {
		doc = &document{uri: PathToURI(path), version: 1, text: text}
		if err := c.conn.Notify("textDocument/didOpen", map[string]any{
			"textDocument": textDocumentItem{URI: doc.uri, LanguageID: languageID(path, c.cfg.LanguageIDs), Version: 1, Text: text},
		}); err != nil {
			return syncResult{}, err
		}
		c.docs[path] = doc
		return syncResult{version: 1, changed: true, text: text}, nil
	}
	if doc.text == text {
		return syncResult{version: doc.version, text: text}, nil
	}
	if c.caps.change == 0 {
		// The server takes no content changes: reopen the document.
		_ = c.conn.Notify("textDocument/didClose", map[string]any{"textDocument": textDocumentIdentifier{URI: doc.uri}})
		doc.version++
		doc.text = text
		err := c.conn.Notify("textDocument/didOpen", map[string]any{
			"textDocument": textDocumentItem{URI: doc.uri, LanguageID: languageID(path, c.cfg.LanguageIDs), Version: doc.version, Text: text},
		})
		return syncResult{version: doc.version, changed: true, text: text}, err
	}
	doc.version++
	doc.text = text
	// A change event without a range replaces the whole document; this is
	// valid for both full and incremental sync servers.
	if err := c.conn.Notify("textDocument/didChange", map[string]any{
		"textDocument":   versionedTextDocumentIdentifier{URI: doc.uri, Version: doc.version},
		"contentChanges": []textDocumentContentChangeEvent{{Text: text}},
	}); err != nil {
		return syncResult{}, err
	}
	if c.caps.save {
		params := map[string]any{"textDocument": textDocumentIdentifier{URI: doc.uri}}
		if c.caps.saveText {
			params["text"] = text
		}
		if err := c.conn.Notify("textDocument/didSave", params); err != nil {
			return syncResult{}, err
		}
	}
	return syncResult{version: doc.version, changed: true, text: text}, nil
}

// OpenDocuments is the number of documents open on the server.
func (c *Client) OpenDocuments() int {
	c.docMu.Lock()
	defer c.docMu.Unlock()
	return len(c.docs)
}

// FileDiagnostics are the diagnostics for one file.
type FileDiagnostics struct {
	Path    string `json:"path"`
	Server  string `json:"server"`
	Version int    `json:"version"`
	// Fresh is true when the server reported on the current content; false
	// means the wait ran out and the diagnostics may be stale or missing.
	Fresh       bool         `json:"fresh"`
	Diagnostics []Diagnostic `json:"diagnostics"`
	// Lines holds the file text, for rendering positions.
	Lines []string `json:"-"`
	// Encoding is the server's position encoding.
	Encoding string `json:"-"`
}

// Diagnostics syncs path and returns the diagnostics the server publishes
// for the new content, waiting at most wait for them and then settle for
// follow-up publishes (servers often report syntax first, types later).
func (c *Client) Diagnostics(ctx context.Context, path string, wait, settle time.Duration) (*FileDiagnostics, error) {
	c.mu.Lock()
	var beforeSeq uint64
	if st := c.diags[path]; st != nil {
		beforeSeq = st.seq
	}
	c.mu.Unlock()
	sr, err := c.syncDocument(path)
	if err != nil {
		return nil, err
	}
	res := &FileDiagnostics{Path: path, Server: c.cfg.Name, Version: sr.version, Lines: splitLines(sr.text), Encoding: c.caps.encoding}
	deadline := time.Now().Add(wait)

	if c.caps.pull {
		pctx, cancel := context.WithDeadline(ctx, deadline)
		items, err := c.pullDiagnostics(pctx, path)
		cancel()
		if err == nil {
			res.Fresh = true
			res.Diagnostics = items
			return res, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !IsMethodNotFound(err) && !errors.Is(err, context.DeadlineExceeded) {
			c.logf("lsp: %s: pull diagnostics failed, waiting for published ones: %v", c.cfg.Name, err)
		}
	}

	fresh := func(st *diagState) bool {
		if st == nil {
			return false
		}
		if st.version >= 0 {
			return st.version >= sr.version
		}
		// Without versions, a publish after our sync is about the new
		// content; with unchanged content, any publish will do.
		return st.seq > beforeSeq || (!sr.changed && st.seq > 0)
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	var settleC <-chan time.Time
	var settleTimer *time.Timer
	defer func() {
		if settleTimer != nil {
			settleTimer.Stop()
		}
	}()
	for {
		c.mu.Lock()
		st := c.diags[path]
		ch := c.diagCh
		if fresh(st) {
			res.Fresh = true
			res.Diagnostics = append([]Diagnostic(nil), st.items...)
		}
		c.mu.Unlock()
		if res.Fresh {
			if settle <= 0 {
				return res, nil
			}
			if settleTimer == nil {
				settleTimer = time.NewTimer(settle)
				settleC = settleTimer.C
			}
		}
		select {
		case <-ch:
			if settleTimer != nil {
				// Another publish: give the server another quiet period.
				if !settleTimer.Stop() {
					select {
					case <-settleTimer.C:
					default:
					}
				}
				settleTimer.Reset(settle)
			}
		case <-settleC:
			return res, nil
		case <-timer.C:
			if !res.Fresh {
				// Report what we have, marked stale.
				c.mu.Lock()
				if st := c.diags[path]; st != nil {
					res.Diagnostics = append([]Diagnostic(nil), st.items...)
				}
				c.mu.Unlock()
			}
			return res, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.exited:
			return nil, c.conn.closedErr()
		}
	}
}

// pullDiagnostics uses textDocument/diagnostic (LSP 3.17).
func (c *Client) pullDiagnostics(ctx context.Context, path string) ([]Diagnostic, error) {
	var rep struct {
		Kind  string       `json:"kind"`
		Items []Diagnostic `json:"items"`
	}
	err := c.conn.Call(ctx, "textDocument/diagnostic", map[string]any{
		"textDocument": textDocumentIdentifier{URI: PathToURI(path)},
	}, &rep)
	if err != nil {
		return nil, err
	}
	if rep.Kind == "unchanged" {
		c.mu.Lock()
		defer c.mu.Unlock()
		if st := c.diags[path]; st != nil {
			return append([]Diagnostic(nil), st.items...), nil
		}
		return nil, nil
	}
	c.storeDiagnostics(path, -1, rep.Items)
	return rep.Items, nil
}

// position syncs path and converts a 1-based line and 1-based rune column
// to a protocol position.
func (c *Client) position(path string, line, col int) (Position, error) {
	sr, err := c.syncDocument(path)
	if err != nil {
		return Position{}, err
	}
	lines := splitLines(sr.text)
	if line < 1 || line > len(lines) {
		return Position{}, fmt.Errorf("line %d is outside %s (%d lines)", line, filepath.Base(path), len(lines))
	}
	if col < 1 {
		col = 1
	}
	return Position{Line: line - 1, Character: toProtocolChar(lines[line-1], col-1, c.caps.encoding)}, nil
}

func (c *Client) positionParams(path string, pos Position) textDocumentPositionParams {
	return textDocumentPositionParams{TextDocument: textDocumentIdentifier{URI: PathToURI(path)}, Position: pos}
}

// Definition returns where the symbol at line:col is defined.
func (c *Client) Definition(ctx context.Context, path string, line, col int) ([]Location, error) {
	if !c.caps.definition {
		return nil, fmt.Errorf("%s does not support go-to-definition", c.cfg.Name)
	}
	pos, err := c.position(path, line, col)
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	if err := c.conn.Call(ctx, "textDocument/definition", c.positionParams(path, pos), &raw); err != nil {
		return nil, err
	}
	return decodeLocations(raw)
}

// References returns the uses of the symbol at line:col.
func (c *Client) References(ctx context.Context, path string, line, col int, includeDecl bool) ([]Location, error) {
	if !c.caps.references {
		return nil, fmt.Errorf("%s does not support find-references", c.cfg.Name)
	}
	pos, err := c.position(path, line, col)
	if err != nil {
		return nil, err
	}
	params := map[string]any{
		"textDocument": textDocumentIdentifier{URI: PathToURI(path)},
		"position":     pos,
		"context":      map[string]any{"includeDeclaration": includeDecl},
	}
	var raw json.RawMessage
	if err := c.conn.Call(ctx, "textDocument/references", params, &raw); err != nil {
		return nil, err
	}
	return decodeLocations(raw)
}

// Hover returns the hover text for the symbol at line:col.
func (c *Client) Hover(ctx context.Context, path string, line, col int) (string, error) {
	if !c.caps.hover {
		return "", fmt.Errorf("%s does not support hover", c.cfg.Name)
	}
	pos, err := c.position(path, line, col)
	if err != nil {
		return "", err
	}
	var res *hoverResult
	if err := c.conn.Call(ctx, "textDocument/hover", c.positionParams(path, pos), &res); err != nil {
		return "", err
	}
	if res == nil {
		return "", nil
	}
	return hoverText(res.Contents), nil
}

func splitLines(text string) []string {
	return strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
}
