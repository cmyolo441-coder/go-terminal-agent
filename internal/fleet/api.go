package fleet

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"time"
)

// API paths.
const (
	APIPrefix           = "/v1/fleet"
	DefaultMaxBodyBytes = 8 << 20
)

// ServerConfig configures NewHandler.
type ServerConfig struct {
	Coordinator *Coordinator
	// Auth verifies requests. It is required unless Insecure is set.
	Auth *Authenticator
	// Insecure serves without authentication (tests and loopback-only
	// development); every caller is an admin.
	Insecure     bool
	MaxBodyBytes int64
	Logger       *slog.Logger
}

// SubmitRequest submits one or more tasks.
type SubmitRequest struct {
	Tasks []TaskSpec `json:"tasks"`
}

// SubmitResponse returns one result per submitted task.
type SubmitResponse struct {
	Results []SubmitResult `json:"results"`
}

// CancelRequest carries an optional reason.
type CancelRequest struct {
	Reason string `json:"reason,omitempty"`
}

// HealthResponse is the unsigned leadership probe.
type HealthResponse struct {
	NodeID       string `json:"node_id"`
	Leader       bool   `json:"leader"`
	FencingToken uint64 `json:"fencing_token"`
	LeaderHint   string `json:"leader_hint,omitempty"`
}

// TaskList is a page of tasks.
type TaskList struct {
	Tasks []*Task `json:"tasks"`
	// Next is the cursor for the following page (0 when done).
	Next uint64 `json:"next,omitempty"`
}

type apiError struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

type apiServer struct {
	c       *Coordinator
	auth    *Authenticator
	insec   bool
	maxBody int64
	log     *slog.Logger
}

type apiFunc func(r *http.Request, p Principal, body []byte) (any, error)

// NewHandler serves the fleet API for c. See the package documentation
// for the endpoints.
func NewHandler(cfg ServerConfig) (http.Handler, error) {
	if cfg.Coordinator == nil {
		return nil, fmt.Errorf("%w: handler needs a coordinator", ErrInvalid)
	}
	if cfg.Auth == nil && !cfg.Insecure {
		return nil, fmt.Errorf("%w: handler needs an authenticator (or Insecure)", ErrInvalid)
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	s := &apiServer{c: cfg.Coordinator, auth: cfg.Auth, insec: cfg.Insecure, maxBody: cfg.MaxBodyBytes, log: cfg.Logger}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+APIPrefix+"/healthz", s.health)
	read := []Role{RoleReader, RoleSubmitter}
	s.route(mux, "POST /tasks", []Role{RoleSubmitter}, s.submit)
	s.route(mux, "GET /tasks", read, s.list)
	s.route(mux, "GET /tasks/{id}", read, s.get)
	s.route(mux, "POST /tasks/{id}/cancel", []Role{RoleSubmitter}, s.cancel)
	s.route(mux, "POST /tasks/{id}/complete", []Role{RoleWorker}, s.complete)
	s.route(mux, "POST /tasks/{id}/fail", []Role{RoleWorker}, s.fail)
	s.route(mux, "POST /tasks/{id}/release", []Role{RoleWorker}, s.release)
	s.route(mux, "GET /dlq", read, s.dlq)
	s.route(mux, "POST /dlq/{id}/redrive", []Role{RoleAdmin}, s.redrive)
	s.route(mux, "GET /workers", []Role{RoleReader}, s.workers)
	s.route(mux, "POST /workers/register", []Role{RoleWorker}, s.register)
	s.route(mux, "POST /workers/{id}/heartbeat", []Role{RoleWorker}, s.heartbeat)
	s.route(mux, "POST /workers/{id}/lease", []Role{RoleWorker}, s.lease)
	s.route(mux, "POST /workers/{id}/drain", []Role{RoleWorker}, s.drain)
	s.route(mux, "POST /workers/{id}/deregister", []Role{RoleWorker}, s.deregister)
	s.route(mux, "GET /metrics", []Role{RoleReader}, s.metrics)
	return mux, nil
}

// NewHTTPServer wraps h in an http.Server with timeouts suited to long
// polls; tlsCfg may be nil.
func NewHTTPServer(addr string, h http.Handler, tlsCfg *tls.Config, maxLongPoll time.Duration) *http.Server {
	if maxLongPoll <= 0 {
		maxLongPoll = DefaultMaxLongPoll
	}
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      maxLongPoll + 30*time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
}

func (s *apiServer) route(mux *http.ServeMux, pattern string, roles []Role, fn apiFunc) {
	method, path, _ := bytes.Cut([]byte(pattern), []byte(" "))
	mux.HandleFunc(string(method)+" "+APIPrefix+string(path), func(w http.ResponseWriter, r *http.Request) {
		s.serve(w, r, roles, fn)
	})
}

func (s *apiServer) serve(w http.ResponseWriter, r *http.Request, roles []Role, fn apiFunc) {
	defer func() {
		if p := recover(); p != nil {
			s.log.Error("fleet: handler panic", "path", r.URL.Path, "panic", p)
			s.write(w, r, Principal{}, nil, fmt.Errorf("internal error"))
		}
	}()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.maxBody))
	if err != nil {
		s.write(w, r, Principal{}, nil, fmt.Errorf("%w: body: %v", ErrInvalid, err))
		return
	}
	p := Principal{KeyID: "insecure", Roles: []Role{RoleAdmin}}
	if !s.insec {
		if p, err = s.auth.Verify(r, body); err != nil {
			s.log.Warn("fleet: request refused", "path", r.URL.Path, "remote", r.RemoteAddr, "err", err)
			s.write(w, r, Principal{}, nil, err)
			return
		}
	}
	if !slices.ContainsFunc(roles, p.Has) {
		s.write(w, r, p, nil, fmt.Errorf("%w: key %s lacks role %v", ErrForbidden, p.KeyID, roles))
		return
	}
	v, err := fn(r, p, body)
	s.write(w, r, p, v, err)
}

// write renders v or err as JSON and signs the response for an
// authenticated caller.
func (s *apiServer) write(w http.ResponseWriter, r *http.Request, p Principal, v any, err error) {
	status := http.StatusOK
	if err != nil {
		code, st := errorCode(err)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			code, st = "canceled", 499
		}
		status = st
		v = apiError{Error: err.Error(), Code: code}
		if errors.Is(err, ErrNotLeader) {
			if hint := s.c.LeaderHint(r.Context()); hint != "" {
				w.Header().Set(HeaderLeader, hint)
			}
		}
		if status >= 500 && !errors.Is(err, ErrNotLeader) {
			s.log.Error("fleet: request failed", "path", r.URL.Path, "err", err)
		}
	}
	b, merr := json.Marshal(v)
	if merr != nil {
		status = 500
		b, _ = json.Marshal(apiError{Error: "encode response: " + merr.Error(), Code: "internal"})
	}
	if s.auth != nil && p.KeyID != "" && p.KeyID != "insecure" {
		s.auth.SignResponse(w.Header(), status, p.KeyID, r.Header.Get(HeaderNonce), b)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func decode(body []byte, v any) error {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return nil
}

func (s *apiServer) health(w http.ResponseWriter, r *http.Request) {
	leader, tok := s.c.IsLeader()
	resp := HealthResponse{NodeID: s.c.NodeID(), Leader: leader, FencingToken: tok}
	if !leader {
		resp.LeaderHint = s.c.LeaderHint(r.Context())
	}
	b, _ := json.Marshal(resp)
	w.Header().Set("Content-Type", "application/json")
	if !leader {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_, _ = w.Write(b)
}

// visible hides tasks of other tenants from tenant-confined keys.
func visible(p Principal, t *Task) error {
	if p.Tenant != "" && t.Tenant != p.Tenant {
		return fmt.Errorf("%w: task %s", ErrNotFound, t.ID)
	}
	return nil
}

func (s *apiServer) submit(r *http.Request, p Principal, body []byte) (any, error) {
	var req SubmitRequest
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	if len(req.Tasks) == 0 || len(req.Tasks) > 10000 {
		return nil, fmt.Errorf("%w: submit 1..10000 tasks", ErrInvalid)
	}
	for i := range req.Tasks {
		t := &req.Tasks[i]
		if p.Tenant != "" {
			if t.Tenant != "" && t.Tenant != p.Tenant {
				return nil, fmt.Errorf("%w: key is confined to tenant %s", ErrForbidden, p.Tenant)
			}
			t.Tenant = p.Tenant
		}
	}
	res, err := s.c.SubmitBatch(r.Context(), req.Tasks)
	if err != nil {
		return nil, err
	}
	return SubmitResponse{Results: res}, nil
}

func (s *apiServer) list(r *http.Request, p Principal, _ []byte) (any, error) {
	q := r.URL.Query()
	f := ListFilter{Tenant: q.Get("tenant"), State: TaskState(q.Get("state"))}
	if p.Tenant != "" {
		f.Tenant = p.Tenant
	}
	var err error
	if v := q.Get("after"); v != "" {
		if f.After, err = strconv.ParseUint(v, 10, 64); err != nil {
			return nil, fmt.Errorf("%w: after", ErrInvalid)
		}
	}
	if v := q.Get("limit"); v != "" {
		if f.Limit, err = strconv.Atoi(v); err != nil {
			return nil, fmt.Errorf("%w: limit", ErrInvalid)
		}
	}
	ts, err := s.c.List(r.Context(), f)
	if err != nil {
		return nil, err
	}
	out := TaskList{Tasks: ts}
	if f.Limit > 0 && len(ts) == f.Limit {
		out.Next = ts[len(ts)-1].Seq
	}
	return out, nil
}

func (s *apiServer) get(r *http.Request, p Principal, _ []byte) (any, error) {
	t, err := s.c.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	if err := visible(p, t); err != nil {
		return nil, err
	}
	return t, nil
}

func (s *apiServer) cancel(r *http.Request, p Principal, body []byte) (any, error) {
	var req CancelRequest
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	id := r.PathValue("id")
	if p.Tenant != "" {
		t, err := s.c.Get(r.Context(), id)
		if err != nil {
			return nil, err
		}
		if err := visible(p, t); err != nil {
			return nil, err
		}
	}
	return s.c.Cancel(r.Context(), id, req.Reason)
}

type ack struct {
	OK bool `json:"ok"`
}

func (s *apiServer) complete(r *http.Request, _ Principal, body []byte) (any, error) {
	var req CompleteRequest
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	req.TaskID = r.PathValue("id")
	return ack{true}, s.c.Complete(r.Context(), req)
}

func (s *apiServer) fail(r *http.Request, _ Principal, body []byte) (any, error) {
	var req FailRequest
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	req.TaskID = r.PathValue("id")
	return ack{true}, s.c.Fail(r.Context(), req)
}

func (s *apiServer) release(r *http.Request, _ Principal, body []byte) (any, error) {
	var req ReleaseRequest
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	req.TaskID = r.PathValue("id")
	return ack{true}, s.c.Release(r.Context(), req)
}

func (s *apiServer) dlq(r *http.Request, p Principal, _ []byte) (any, error) {
	tenant := r.URL.Query().Get("tenant")
	if p.Tenant != "" {
		tenant = p.Tenant
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	ts, err := s.c.DeadLetters(r.Context(), tenant, limit)
	if err != nil {
		return nil, err
	}
	return TaskList{Tasks: ts}, nil
}

func (s *apiServer) redrive(r *http.Request, _ Principal, _ []byte) (any, error) {
	return s.c.Redrive(r.Context(), r.PathValue("id"))
}

func (s *apiServer) workers(r *http.Request, _ Principal, _ []byte) (any, error) {
	return s.c.Workers(r.Context())
}

func (s *apiServer) register(r *http.Request, _ Principal, body []byte) (any, error) {
	var req RegisterRequest
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	return s.c.Register(r.Context(), req)
}

func (s *apiServer) heartbeat(r *http.Request, _ Principal, body []byte) (any, error) {
	var req HeartbeatRequest
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	req.WorkerID = r.PathValue("id")
	return s.c.Heartbeat(r.Context(), req)
}

func (s *apiServer) lease(r *http.Request, _ Principal, body []byte) (any, error) {
	var req LeaseRequest
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	req.WorkerID = r.PathValue("id")
	return s.c.Lease(r.Context(), req)
}

func (s *apiServer) drain(r *http.Request, _ Principal, _ []byte) (any, error) {
	return ack{true}, s.c.Drain(r.Context(), r.PathValue("id"))
}

func (s *apiServer) deregister(r *http.Request, _ Principal, _ []byte) (any, error) {
	return ack{true}, s.c.Deregister(r.Context(), r.PathValue("id"))
}

func (s *apiServer) metrics(r *http.Request, _ Principal, _ []byte) (any, error) {
	snap := s.c.Metrics(r.Context())
	if !snap.Leader {
		return nil, ErrNotLeader
	}
	return snap, nil
}
