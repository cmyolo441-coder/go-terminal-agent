// Package server runs GTA as a daemon that exposes agent sessions over a
// local HTTP API, for editors, scripts and other front ends.
//
// The server binds to 127.0.0.1 by default and requires a bearer token on
// every request except GET /v1/health. The token is random unless given,
// and is written to a file only its owner can read. Requests whose Host
// header is not a loopback name are refused while bound to loopback, which
// defeats DNS-rebinding attacks from web pages.
//
// Endpoints:
//
//	GET    /v1/health                                  liveness and version (no token needed)
//	POST   /v1/sessions                                create a session {"model","mode","title"}
//	GET    /v1/sessions                                list sessions
//	GET    /v1/sessions/{id}                           one session, with ?history=1 for its messages
//	DELETE /v1/sessions/{id}                           interrupt and delete a session
//	POST   /v1/sessions/{id}/messages                  run a turn {"text","images"}; replies with an SSE stream
//	POST   /v1/sessions/{id}/approvals/{request_id}    answer an approval {"decision": "allow_once"|"allow_session"|"deny"}
//	POST   /v1/sessions/{id}/interrupt                 cancel the running turn
//
// A session runs one turn at a time; a second message while a turn runs is
// refused with 409. The message stream carries the events named by the
// Event* constants, each with a JSON payload, and always ends with "done".
// A tool call that needs approval emits approval_required and waits for the
// approvals endpoint; it is denied when the approval timeout passes or the
// turn is cancelled. A client that disconnects mid-stream interrupts its
// turn.
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/agent"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permission"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/version"
)

// Defaults for Options fields left zero.
const (
	DefaultAddr            = "127.0.0.1:7878"
	DefaultMaxBodyBytes    = 12 << 20
	DefaultMaxSessions     = 64
	DefaultShutdownTimeout = 15 * time.Second
	DefaultKeepAlive       = 15 * time.Second
	bodyReadTimeout        = 30 * time.Second
)

// SessionOptions are what a client may ask for when creating a session.
type SessionOptions struct {
	// Model overrides the configured model.
	Model string `json:"model,omitempty"`
	// Mode is a permission mode: ask, auto-edit, plan or yolo.
	Mode string `json:"mode,omitempty"`
	// Title is a free-form label shown in listings.
	Title string `json:"title,omitempty"`
}

// Factory builds the agent for a new session. The server owns the agent
// afterwards and runs at most one turn on it at a time.
type Factory func(ctx context.Context, opts SessionOptions) (*agent.Agent, error)

// Options configure a Server.
type Options struct {
	// Addr is the listen address. It must be a loopback address unless
	// AllowNonLoopback is set.
	Addr             string
	AllowNonLoopback bool
	// Token is the bearer token. Empty generates a random one.
	Token string
	// TokenFile, when set, receives the token (mode 0600) once the server
	// listens, and is removed on shutdown.
	TokenFile string
	// Factory builds a session's agent. Required.
	Factory Factory
	// ApprovalTimeout bounds the wait for an approval decision.
	ApprovalTimeout time.Duration
	// MaxBodyBytes limits request bodies (images make messages large).
	MaxBodyBytes int64
	// MaxSessions caps concurrent sessions.
	MaxSessions int
	// SessionIdleTTL deletes sessions idle for longer than this. Zero
	// keeps sessions until deleted or the server stops.
	SessionIdleTTL time.Duration
	// ShutdownTimeout bounds graceful shutdown.
	ShutdownTimeout time.Duration
	// KeepAlive is the interval of SSE comment pings.
	KeepAlive time.Duration
	// CORSOrigins lists browser origins allowed to call the API. Empty
	// (the default) sends no CORS headers, so browsers cannot.
	CORSOrigins []string
	// MaxEventOutput caps tool output in tool_done events.
	MaxEventOutput int
	// Logger receives access and lifecycle logs; nil discards them.
	Logger *slog.Logger
}

// Server is the HTTP daemon.
type Server struct {
	opts      Options
	tokenHash [32]byte
	log       *slog.Logger
	started   time.Time
	handler   http.Handler

	mu       sync.Mutex
	sessions map[string]*session
	addr     string
	closing  atomic.Bool
	turns    sync.WaitGroup
}

type session struct {
	id      string
	created time.Time
	opts    SessionOptions
	agent   *agent.Agent

	mu         sync.Mutex
	busy       bool
	cancel     context.CancelFunc
	sink       *EventSink
	lastActive time.Time
	turns      int
	lastError  string
}

// New validates options and builds a server.
func New(o Options) (*Server, error) {
	if o.Factory == nil {
		return nil, errors.New("server: a session factory is required")
	}
	if o.Addr == "" {
		o.Addr = DefaultAddr
	}
	if o.Token == "" {
		o.Token = newID("gta_")
	} else if len(o.Token) < 16 {
		return nil, errors.New("server: the token must be at least 16 characters")
	}
	if o.ApprovalTimeout <= 0 {
		o.ApprovalTimeout = DefaultApprovalTimeout
	}
	if o.MaxBodyBytes <= 0 {
		o.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if o.MaxSessions <= 0 {
		o.MaxSessions = DefaultMaxSessions
	}
	if o.ShutdownTimeout <= 0 {
		o.ShutdownTimeout = DefaultShutdownTimeout
	}
	if o.KeepAlive <= 0 {
		o.KeepAlive = DefaultKeepAlive
	}
	if o.MaxEventOutput <= 0 {
		o.MaxEventOutput = DefaultMaxEventOutput
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	for _, origin := range o.CORSOrigins {
		if origin == "*" {
			return nil, errors.New(`server: CORS origin "*" is not allowed; list the exact origins`)
		}
	}
	s := &Server{opts: o, tokenHash: sha256.Sum256([]byte(o.Token)), log: o.Logger, started: time.Now(), sessions: map[string]*session{}}
	s.handler = s.routes()
	return s, nil
}

// Token is the bearer token clients must send.
func (s *Server) Token() string { return s.opts.Token }

// Addr is the address the server listens on, once it does.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Handler is the HTTP handler with every middleware applied.
func (s *Server) Handler() http.Handler { return s.handler }

// ListenAndServe listens on Options.Addr and serves until ctx is done.
func (s *Server) ListenAndServe(ctx context.Context) error {
	if err := checkAddr(s.opts.Addr, s.opts.AllowNonLoopback); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", s.opts.Addr)
	if err != nil {
		return fmt.Errorf("server: listen on %s: %w", s.opts.Addr, err)
	}
	return s.Serve(ctx, ln)
}

// checkAddr refuses listen addresses other machines could reach unless
// explicitly allowed.
func checkAddr(addr string, allowRemote bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("server: bad listen address %q: %w", addr, err)
	}
	if allowRemote || isLoopbackHost(host) {
		return nil
	}
	return fmt.Errorf("server: refusing to listen on %q, which is reachable from other machines; use 127.0.0.1 or allow non-loopback addresses explicitly", addr)
}

func isLoopbackHost(host string) bool {
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Serve serves on ln until ctx is done, then shuts down gracefully: new
// turns are refused, running turns are interrupted, open streams finish,
// and the token file is removed.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	s.mu.Lock()
	s.addr = ln.Addr().String()
	s.mu.Unlock()
	if s.opts.TokenFile != "" {
		if err := writeTokenFile(s.opts.TokenFile, s.opts.Token); err != nil {
			ln.Close()
			return err
		}
		defer os.Remove(s.opts.TokenFile)
	}
	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	stopJanitor := s.startJanitor()
	defer stopJanitor()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	s.log.Info("gta server listening", "addr", s.addr, "version", version.Version)
	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	s.log.Info("gta server shutting down")
	s.mu.Lock()
	s.closing.Store(true)
	s.mu.Unlock()
	s.interruptAll()
	sctx, cancel := context.WithTimeout(context.Background(), s.opts.ShutdownTimeout)
	defer cancel()
	err := srv.Shutdown(sctx)
	if err != nil {
		srv.Close()
	}
	// Turns were interrupted; wait (bounded) for their goroutines.
	done := make(chan struct{})
	go func() { s.turns.Wait(); close(done) }()
	select {
	case <-done:
	case <-sctx.Done():
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("server: shutdown timed out after %s", s.opts.ShutdownTimeout)
	}
	return nil
}

// writeTokenFile writes the token atomically with owner-only permissions.
func writeTokenFile(path, token string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("server: token file: %w", err)
	}
	f, err := os.CreateTemp(dir, ".token-*")
	if err != nil {
		return fmt.Errorf("server: token file: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return fmt.Errorf("server: token file: %w", err)
	}
	if _, err := f.WriteString(token + "\n"); err != nil {
		f.Close()
		return fmt.Errorf("server: token file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("server: token file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("server: token file: %w", err)
	}
	return nil
}

func (s *Server) interruptAll() {
	s.mu.Lock()
	list := make([]*session, 0, len(s.sessions))
	for _, ss := range s.sessions {
		list = append(list, ss)
	}
	s.mu.Unlock()
	for _, ss := range list {
		ss.interrupt()
	}
}

func (s *Server) startJanitor() func() {
	ttl := s.opts.SessionIdleTTL
	if ttl <= 0 {
		return func() {}
	}
	every := min(ttl/2, time.Minute)
	if every <= 0 {
		every = ttl
	}
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-t.C:
				s.evictIdle(now)
			}
		}
	}()
	return func() { close(stop) }
}

// evictIdle deletes sessions that have been idle for longer than the TTL.
func (s *Server) evictIdle(now time.Time) int {
	ttl := s.opts.SessionIdleTTL
	if ttl <= 0 {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, ss := range s.sessions {
		ss.mu.Lock()
		idle := !ss.busy && now.Sub(ss.lastActive) > ttl
		ss.mu.Unlock()
		if idle {
			delete(s.sessions, id)
			n++
			s.log.Info("session expired", "session", id)
		}
	}
	return n
}

// routes builds the mux and middleware chain.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.HandleFunc("POST /v1/sessions", s.handleCreate)
	mux.HandleFunc("GET /v1/sessions", s.handleList)
	mux.HandleFunc("GET /v1/sessions/{id}", s.handleGet)
	mux.HandleFunc("DELETE /v1/sessions/{id}", s.handleDelete)
	mux.HandleFunc("POST /v1/sessions/{id}/messages", s.handleMessage)
	mux.HandleFunc("POST /v1/sessions/{id}/approvals/{reqid}", s.handleApproval)
	mux.HandleFunc("POST /v1/sessions/{id}/interrupt", s.handleInterrupt)
	return s.recoverer(s.logRequests(s.checkHost(s.cors(s.auth(jsonMuxErrors(mux))))))
}

// jsonMuxErrors turns the mux's plain-text 404 and 405 replies into the
// API's JSON error format (keeping the Allow header of a 405).
func jsonMuxErrors(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}
		mux.ServeHTTP(&muxErrorWriter{ResponseWriter: w, r: r}, r)
	})
}

type muxErrorWriter struct {
	http.ResponseWriter
	r       *http.Request
	written bool
}

func (m *muxErrorWriter) WriteHeader(code int) {
	if m.written {
		return
	}
	m.written = true
	m.Header().Del("X-Content-Type-Options")
	switch code {
	case http.StatusMethodNotAllowed:
		writeError(m.ResponseWriter, code, "method_not_allowed", m.r.Method+" is not allowed on "+m.r.URL.Path)
	case http.StatusNotFound:
		writeError(m.ResponseWriter, code, "not_found", "no such endpoint: "+m.r.Method+" "+m.r.URL.Path)
	default:
		m.ResponseWriter.WriteHeader(code)
		m.written = false
	}
}

// Write drops the mux's plain-text body once a JSON error was written.
func (m *muxErrorWriter) Write(b []byte) (int, error) {
	if m.written {
		return len(b), nil
	}
	return m.ResponseWriter.Write(b)
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.Error("panic in handler", "path", r.URL.Path, "panic", fmt.Sprint(v))
				writeError(w, http.StatusInternalServerError, "internal", "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach Flush and deadlines.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		s.log.Debug("request", "method", r.Method, "path", r.URL.Path, "status", rec.status, "duration", time.Since(start))
	})
}

// checkHost refuses requests addressed to a non-loopback host name while
// the server is bound to loopback, so a web page that rebinds its DNS name
// to 127.0.0.1 cannot reach the API.
func (s *Server) checkHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.opts.AllowNonLoopback {
			host := r.Host
			if h, _, err := net.SplitHostPort(host); err == nil {
				host = h
			}
			if !isLoopbackHost(host) {
				writeError(w, http.StatusMisdirectedRequest, "bad_host", "requests must be addressed to localhost or a loopback address")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) cors(next http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, o := range s.opts.CORSOrigins {
		allowed[strings.TrimRight(o, "/")] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		ok := origin != "" && allowed[origin]
		if ok {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Expose-Headers", "Content-Type")
		}
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			if !ok {
				writeError(w, http.StatusForbidden, "cors", "cross-origin requests are not allowed")
				return
			}
			h := w.Header()
			h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			h.Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/health" {
			next.ServeHTTP(w, r)
			return
		}
		const prefix = "bearer "
		h := r.Header.Get("Authorization")
		if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="gta"`)
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
			return
		}
		// Comparing fixed-size hashes keeps the comparison constant-time
		// regardless of the presented token's length.
		got := sha256.Sum256([]byte(strings.TrimSpace(h[len(prefix):])))
		if subtle.ConstantTimeCompare(got[:], s.tokenHash[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="gta", error="invalid_token"`)
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// apiError is the JSON error body.
type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	var e apiError
	e.Error.Code, e.Error.Message = code, msg
	writeJSON(w, status, e)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// decodeBody strictly decodes a JSON request body. An empty body is
// allowed when allowEmpty is set and leaves v unchanged.
func (s *Server) decodeBody(w http.ResponseWriter, r *http.Request, v any, allowEmpty bool) bool {
	r.Body = http.MaxBytesReader(w, r.Body, s.opts.MaxBodyBytes)
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(bodyReadTimeout))
	defer rc.SetReadDeadline(time.Time{})
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("request body is larger than %d bytes", s.opts.MaxBodyBytes))
			return false
		}
		writeError(w, http.StatusBadRequest, "bad_request", "could not read the request body")
		return false
	}
	if len(bytes.TrimSpace(data)) == 0 {
		if allowEmpty {
			return true
		}
		writeError(w, http.StatusBadRequest, "bad_request", "a JSON request body is required")
		return false
	}
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "bad_content_type", "send the body as application/json")
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return false
	}
	if dec.More() {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: trailing data")
		return false
	}
	return true
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "ok",
		"version":    version.Version,
		"uptime_sec": int64(time.Since(s.started).Seconds()),
	})
}

// SessionInfo describes a session in API responses.
type SessionInfo struct {
	ID           string        `json:"id"`
	Title        string        `json:"title,omitempty"`
	Model        string        `json:"model"`
	Mode         string        `json:"mode,omitempty"`
	CreatedAt    time.Time     `json:"created_at"`
	LastActiveAt time.Time     `json:"last_active_at"`
	Busy         bool          `json:"busy"`
	Turns        int           `json:"turns"`
	Messages     int           `json:"messages"`
	Usage        llm.Usage     `json:"usage"`
	CostUSD      float64       `json:"cost_usd"`
	Context      int64         `json:"context_tokens"`
	LastError    string        `json:"last_error,omitempty"`
	History      []llm.Message `json:"history,omitempty"`
}

func (ss *session) info(withHistory bool) SessionInfo {
	ss.mu.Lock()
	info := SessionInfo{ID: ss.id, Title: ss.opts.Title, CreatedAt: ss.created, LastActiveAt: ss.lastActive, Busy: ss.busy, Turns: ss.turns, LastError: ss.lastError}
	ss.mu.Unlock()
	info.Model = ss.agent.Settings().Model
	if ss.agent.Gate != nil {
		info.Mode = string(ss.agent.Gate.Mode())
	}
	info.Usage, info.CostUSD, info.Context = ss.agent.Stats()
	h := ss.agent.History()
	info.Messages = len(h)
	if withHistory {
		info.History = h
	}
	return info
}

func (ss *session) interrupt() bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.cancel == nil {
		return false
	}
	ss.cancel()
	return true
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	if s.closing.Load() {
		writeError(w, http.StatusServiceUnavailable, "shutting_down", "the server is shutting down")
		return
	}
	var opts SessionOptions
	if !s.decodeBody(w, r, &opts, true) {
		return
	}
	if opts.Mode != "" {
		if _, err := permission.ParseMode(opts.Mode); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
	}
	if len(opts.Title) > 200 {
		writeError(w, http.StatusBadRequest, "bad_request", "title is longer than 200 characters")
		return
	}
	s.mu.Lock()
	full := len(s.sessions) >= s.opts.MaxSessions
	s.mu.Unlock()
	if full && s.evictIdle(time.Now()) == 0 {
		writeError(w, http.StatusServiceUnavailable, "too_many_sessions", fmt.Sprintf("the server already holds %d sessions; delete some first", s.opts.MaxSessions))
		return
	}
	ag, err := s.opts.Factory(r.Context(), opts)
	if err != nil {
		writeError(w, http.StatusBadRequest, "session_failed", "could not create the session: "+err.Error())
		return
	}
	if ag == nil {
		writeError(w, http.StatusInternalServerError, "session_failed", "the session factory returned no agent")
		return
	}
	now := time.Now()
	ss := &session{id: newID("ses_"), created: now, lastActive: now, opts: opts, agent: ag}
	s.mu.Lock()
	if len(s.sessions) >= s.opts.MaxSessions {
		s.mu.Unlock()
		writeError(w, http.StatusServiceUnavailable, "too_many_sessions", fmt.Sprintf("the server already holds %d sessions; delete some first", s.opts.MaxSessions))
		return
	}
	s.sessions[ss.id] = ss
	s.mu.Unlock()
	s.log.Info("session created", "session", ss.id, "model", ag.Settings().Model)
	writeJSON(w, http.StatusCreated, ss.info(false))
}

func (s *Server) session(w http.ResponseWriter, r *http.Request) (*session, bool) {
	id := r.PathValue("id")
	s.mu.Lock()
	ss, ok := s.sessions[id]
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "no_session", "no session with id "+id)
		return nil, false
	}
	return ss, true
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	list := make([]*session, 0, len(s.sessions))
	for _, ss := range s.sessions {
		list = append(list, ss)
	}
	s.mu.Unlock()
	infos := make([]SessionInfo, len(list))
	for i, ss := range list {
		infos[i] = ss.info(false)
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].CreatedAt.Before(infos[j].CreatedAt) })
	writeJSON(w, http.StatusOK, map[string]any{"sessions": infos})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	ss, ok := s.session(w, r)
	if !ok {
		return
	}
	q := r.URL.Query().Get("history")
	writeJSON(w, http.StatusOK, ss.info(q == "1" || q == "true"))
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	ss, ok := s.session(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	delete(s.sessions, ss.id)
	s.mu.Unlock()
	interrupted := ss.interrupt()
	s.log.Info("session deleted", "session", ss.id)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "interrupted": interrupted})
}

func (s *Server) handleInterrupt(w http.ResponseWriter, r *http.Request) {
	ss, ok := s.session(w, r)
	if !ok {
		return
	}
	if !s.decodeBody(w, r, &struct{}{}, true) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"interrupted": ss.interrupt()})
}

// approvalRequest is the body of the approvals endpoint.
type approvalRequest struct {
	Decision string `json:"decision"`
}

func (s *Server) handleApproval(w http.ResponseWriter, r *http.Request) {
	ss, ok := s.session(w, r)
	if !ok {
		return
	}
	var req approvalRequest
	if !s.decodeBody(w, r, &req, false) {
		return
	}
	d, err := ParseDecision(req.Decision)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	ss.mu.Lock()
	sink := ss.sink
	ss.mu.Unlock()
	if sink == nil {
		writeError(w, http.StatusNotFound, "no_approval", "no turn is running in this session")
		return
	}
	if err := sink.Resolve(r.PathValue("reqid"), d); err != nil {
		writeError(w, http.StatusNotFound, "no_approval", "no pending approval "+r.PathValue("reqid")+" (it may have timed out or been answered)")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"request_id": r.PathValue("reqid"), "decision": decisionName(d)})
}

// MessageRequest is the body of the messages endpoint.
type MessageRequest struct {
	Text   string       `json:"text"`
	Images []ImageInput `json:"images,omitempty"`
}

// ImageInput is an image attached to a message: base64 data with its media
// type, or a path on the server's machine (relative paths are resolved
// against the session's workspace).
type ImageInput struct {
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	Path      string `json:"path,omitempty"`
}

const maxImagesPerMessage = 20

func (s *Server) buildMessage(ss *session, req MessageRequest) (llm.Message, error) {
	if strings.TrimSpace(req.Text) == "" && len(req.Images) == 0 {
		return llm.Message{}, errors.New(`"text" or "images" is required`)
	}
	if len(req.Images) > maxImagesPerMessage {
		return llm.Message{}, fmt.Errorf("at most %d images per message", maxImagesPerMessage)
	}
	msg := llm.Message{Role: llm.RoleUser}
	if strings.TrimSpace(req.Text) != "" {
		msg.Blocks = append(msg.Blocks, llm.Block{Type: llm.BlockText, Text: req.Text})
	}
	for i, in := range req.Images {
		img := llm.Image{MediaType: in.MediaType, Data: in.Data}
		if in.Path != "" {
			if in.Data != "" {
				return llm.Message{}, fmt.Errorf("image %d: give either data or path, not both", i+1)
			}
			p, root := in.Path, ""
			if ss.agent.Env != nil {
				p, root = ss.agent.Env.Resolve(p), ss.agent.Env.Root
			}
			// Reading the file is a read like read_file: the managed
			// policy's path rules apply.
			if g := ss.agent.Gov; g != nil && g.Policy.Active() {
				if d := g.Policy.CheckPath(p, root, false); !d.Allowed {
					if g.Audit != nil {
						_ = g.Audit.Log(d.Record("image"))
					}
					return llm.Message{}, fmt.Errorf("image %d: %s", i+1, d.UserText())
				}
			}
			img = llm.Image{Path: p}
		}
		resolved, err := img.Resolve()
		if err != nil {
			return llm.Message{}, fmt.Errorf("image %d: %w", i+1, err)
		}
		msg.Blocks = append(msg.Blocks, llm.Block{Type: llm.BlockImage, Image: &resolved})
	}
	return msg, nil
}

// begin claims the session for a turn.
func (ss *session) begin(cancel context.CancelFunc, sink *EventSink) bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.busy {
		return false
	}
	ss.busy, ss.cancel, ss.sink = true, cancel, sink
	ss.turns++
	ss.lastActive = time.Now()
	return true
}

func (ss *session) end(errMsg string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.busy, ss.cancel, ss.sink = false, nil, nil
	ss.lastActive = time.Now()
	ss.lastError = errMsg
}

func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	ss, ok := s.session(w, r)
	if !ok {
		return
	}
	if s.closing.Load() {
		writeError(w, http.StatusServiceUnavailable, "shutting_down", "the server is shutting down")
		return
	}
	var req MessageRequest
	if !s.decodeBody(w, r, &req, false) {
		return
	}
	msg, err := s.buildMessage(ss, req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	// A client that goes away cancels the turn through r.Context().
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	events := make(chan Event, 256)
	writerGone := make(chan struct{})
	emit := func(ev Event) {
		select {
		case events <- ev:
		case <-writerGone:
		}
	}
	sink := NewEventSink(emit, s.opts.ApprovalTimeout)
	sink.SetMaxOutput(s.opts.MaxEventOutput)
	if !ss.begin(cancel, sink) {
		writeError(w, http.StatusConflict, "session_busy", "a turn is already running in this session; wait for it or interrupt it")
		return
	}
	// Registering the turn under s.mu orders it before shutdown's Wait.
	s.mu.Lock()
	if s.closing.Load() {
		s.mu.Unlock()
		ss.end("")
		writeError(w, http.StatusServiceUnavailable, "shutting_down", "the server is shutting down")
		return
	}
	s.turns.Add(1)
	s.mu.Unlock()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-store")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	_ = rc.Flush()

	turn := ss.turnNumber()
	before := len(ss.agent.History())
	start := time.Now()
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		defer s.turns.Done()
		emit(Event{EventTurnStart, map[string]any{"session_id": ss.id, "turn": turn}})
		runErr := ss.agent.Run(ctx, msg, sink)
		done := map[string]any{"turn": turn, "duration_ms": time.Since(start).Milliseconds()}
		errMsg := ""
		switch {
		case runErr == nil:
			done["stop_reason"] = "end_turn"
		case errors.Is(runErr, agent.ErrInterrupted) || ctx.Err() != nil:
			done["stop_reason"] = "interrupted"
		default:
			done["stop_reason"] = "error"
			errMsg = runErr.Error()
			done["error"] = errMsg
			emit(Event{EventError, map[string]any{"message": errMsg}})
		}
		done["text"] = finalText(ss.agent.History(), before)
		u, cost, ctxTokens := ss.agent.Stats()
		done["usage"], done["cost_usd"], done["context_tokens"] = u, cost, ctxTokens
		ss.end(errMsg)
		emit(Event{EventDone, done})
	}()

	ticker := time.NewTicker(s.opts.KeepAlive)
	defer ticker.Stop()
	seq := 0
	writeFailed := false
	write := func(ev Event) {
		if writeFailed {
			return
		}
		seq++
		if err := writeSSE(w, seq, ev); err != nil || rc.Flush() != nil {
			writeFailed = true
			close(writerGone)
			cancel()
		}
	}
	for {
		select {
		case ev := <-events:
			write(ev)
		case <-ticker.C:
			if !writeFailed {
				if _, err := io.WriteString(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
					writeFailed = true
					close(writerGone)
					cancel()
				}
			}
		case <-runDone:
			// Drain what the turn emitted before it finished.
			for {
				select {
				case ev := <-events:
					write(ev)
				default:
					return
				}
			}
		}
	}
}

func (ss *session) turnNumber() int {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.turns
}

// finalText is the text of the last assistant message added since index
// before.
func finalText(h []llm.Message, before int) string {
	for i := len(h) - 1; i >= before && i >= 0; i-- {
		if h[i].Role == llm.RoleAssistant {
			return strings.TrimSpace(h[i].Text())
		}
	}
	return ""
}

// writeSSE writes one event. JSON never contains raw newlines, so the
// payload fits on one data line.
func writeSSE(w io.Writer, id int, ev Event) error {
	data, err := json.Marshal(ev.Data)
	if err != nil {
		data, _ = json.Marshal(map[string]any{"message": "could not encode event: " + err.Error()})
		ev.Type = EventError
	}
	_, err = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", id, ev.Type, data)
	return err
}
