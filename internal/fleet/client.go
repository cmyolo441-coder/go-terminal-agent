package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxResponseBytes = 64 << 20

// ClientConfig configures a Client.
type ClientConfig struct {
	// Endpoints are coordinator base URLs, e.g. "https://coord-1:7979".
	// The client follows leader hints among them and fails over between
	// them.
	Endpoints []string
	// KeyID and Secret sign requests; leave both empty only for a server
	// running with Insecure.
	KeyID  string
	Secret []byte
	// HTTPClient defaults to a client with no overall timeout (per-request
	// timeouts come from RequestTimeout). Set its Transport's TLS config
	// for mTLS.
	HTTPClient *http.Client
	Clock      Clock
	// MaxRetries bounds retries of transient failures (default 5).
	MaxRetries int
	// RetryBackoff is the first retry delay, doubling (default 100ms).
	RetryBackoff time.Duration
	// RequestTimeout bounds each request beyond its long-poll wait
	// (default 30s).
	RequestTimeout time.Duration
	// MaxSkew bounds response timestamps (default DefaultMaxSkew).
	MaxSkew time.Duration
	// InsecureSkipResponseVerification accepts unsigned responses.
	InsecureSkipResponseVerification bool
	UserAgent                        string
}

// Client talks to coordinators over HTTP. It implements WorkerAPI.
type Client struct {
	cfg ClientConfig
	hc  *http.Client

	mu  sync.Mutex
	cur int
}

// RemoteError is an error reported by the coordinator. It unwraps to the
// matching sentinel (ErrNotFound, ErrLeaseLost, ...).
type RemoteError struct {
	Status  int
	Code    string
	Message string
	base    error
}

func (e *RemoteError) Error() string { return e.Message }

// Unwrap returns the sentinel for Code, if any.
func (e *RemoteError) Unwrap() error { return e.base }

// NewClient validates cfg.
func NewClient(cfg ClientConfig) (*Client, error) {
	if len(cfg.Endpoints) == 0 {
		return nil, fmt.Errorf("%w: client needs at least one endpoint", ErrInvalid)
	}
	eps := make([]string, 0, len(cfg.Endpoints))
	for _, e := range cfg.Endpoints {
		u, err := url.Parse(e)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("%w: endpoint %q", ErrInvalid, e)
		}
		eps = append(eps, strings.TrimRight(e, "/"))
	}
	cfg.Endpoints = eps
	if (cfg.KeyID == "") != (len(cfg.Secret) == 0) {
		return nil, fmt.Errorf("%w: set both KeyID and Secret, or neither", ErrInvalid)
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{}
	}
	if cfg.Clock == nil {
		cfg.Clock = RealClock()
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 5
	}
	if cfg.RetryBackoff <= 0 {
		cfg.RetryBackoff = 100 * time.Millisecond
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 30 * time.Second
	}
	if cfg.MaxSkew <= 0 {
		cfg.MaxSkew = DefaultMaxSkew
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "gta-fleet-client"
	}
	return &Client{cfg: cfg, hc: cfg.HTTPClient}, nil
}

func (c *Client) endpoint() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg.Endpoints[c.cur]
}

// failover moves to hint if it is a configured endpoint, else to the
// endpoint after from.
func (c *Client) failover(from, hint string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if hint != "" {
		if i := slices.Index(c.cfg.Endpoints, strings.TrimRight(hint, "/")); i >= 0 {
			c.cur = i
			return
		}
	}
	if c.cfg.Endpoints[c.cur] == from {
		c.cur = (c.cur + 1) % len(c.cfg.Endpoints)
	}
}

func retryable(err error) bool {
	return errors.Is(err, ErrNotLeader) || errors.Is(err, ErrStaleFencingToken) || errors.Is(err, ErrClosed) ||
		errors.Is(err, ErrOverloaded)
}

// do performs one API call with signing, response verification, leader
// following and retries. Every fleet operation is safe to retry: task
// submission carries an idempotency key and worker acknowledgements are
// idempotent per lease.
func (c *Client) do(ctx context.Context, method, path string, q url.Values, in, out any, extra time.Duration) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return fmt.Errorf("fleet: encode request: %w", err)
		}
	}
	var lastErr error
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			d := c.cfg.RetryBackoff << min(attempt-1, 6)
			d = d/2 + time.Duration(mrand.Int64N(int64(d/2)+1))
			if err := sleepCtx(ctx, c.cfg.Clock, d); err != nil {
				return errors.Join(err, lastErr)
			}
		}
		base := c.endpoint()
		again, err := c.once(ctx, base, method, path, q, body, out, extra)
		if err == nil {
			return nil
		}
		lastErr = err
		if !again || attempt >= c.cfg.MaxRetries || ctx.Err() != nil {
			return err
		}
	}
}

func (c *Client) once(ctx context.Context, base, method, path string, q url.Values, body []byte, out any, extra time.Duration) (retry bool, err error) {
	rctx, cancel := context.WithTimeout(ctx, c.cfg.RequestTimeout+extra)
	defer cancel()
	u := base + APIPrefix + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(rctx, method, u, bytes.NewReader(body))
	if err != nil {
		return false, fmt.Errorf("fleet: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	var nonce string
	if c.cfg.KeyID != "" {
		nonce = SignRequest(req, c.cfg.KeyID, c.cfg.Secret, body, c.cfg.Clock.Now())
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		c.failover(base, "")
		return true, fmt.Errorf("fleet: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return true, fmt.Errorf("fleet: read response: %w", err)
	}
	ok := resp.StatusCode/100 == 2
	if c.cfg.KeyID != "" && !c.cfg.InsecureSkipResponseVerification {
		verr := VerifyResponse(resp.Header, resp.StatusCode, c.cfg.KeyID, c.cfg.Secret, nonce, rb, c.cfg.Clock.Now(), c.cfg.MaxSkew)
		// A success must be authentic. Unsigned failures (e.g. refused
		// before authentication) are reported but never trusted as data.
		if verr != nil && ok {
			return false, verr
		}
	}
	if ok {
		if out != nil {
			if err := json.Unmarshal(rb, out); err != nil {
				return false, fmt.Errorf("fleet: decode response: %w", err)
			}
		}
		return false, nil
	}
	var ae apiError
	_ = json.Unmarshal(rb, &ae)
	re := &RemoteError{Status: resp.StatusCode, Code: ae.Code, Message: ae.Error, base: errorFromCode(ae.Code)}
	if re.Message == "" {
		re.Message = fmt.Sprintf("fleet: %s %s: HTTP %d", method, path, resp.StatusCode)
	}
	if errors.Is(re, ErrNotLeader) || errors.Is(re, ErrStaleFencingToken) || errors.Is(re, ErrClosed) {
		c.failover(base, resp.Header.Get(HeaderLeader))
	}
	return retryable(re) || (re.base == nil && resp.StatusCode >= 500), re
}

// Health probes the current endpoint (unsigned).
func (c *Client) Health(ctx context.Context) (HealthResponse, error) {
	var h HealthResponse
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint()+APIPrefix+"/healthz", nil)
	if err != nil {
		return h, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return h, err
	}
	defer resp.Body.Close()
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&h)
	return h, err
}

// Submit enqueues one task. A missing idempotency key is generated so
// that retries cannot enqueue the task twice.
func (c *Client) Submit(ctx context.Context, spec TaskSpec) (SubmitResult, error) {
	res, err := c.SubmitBatch(ctx, []TaskSpec{spec})
	if err != nil {
		return SubmitResult{}, err
	}
	if len(res) != 1 {
		return SubmitResult{}, fmt.Errorf("fleet: expected 1 result, got %d", len(res))
	}
	return res[0], nil
}

// SubmitBatch enqueues tasks in order (later ones may depend on earlier
// ones).
func (c *Client) SubmitBatch(ctx context.Context, specs []TaskSpec) ([]SubmitResult, error) {
	specs = slices.Clone(specs)
	for i := range specs {
		if specs[i].IdempotencyKey == "" {
			specs[i].IdempotencyKey = "auto-" + randHex(12)
		}
	}
	var out SubmitResponse
	if err := c.do(ctx, http.MethodPost, "/tasks", nil, SubmitRequest{Tasks: specs}, &out, 0); err != nil {
		return nil, err
	}
	return out.Results, nil
}

// Get fetches a task.
func (c *Client) Get(ctx context.Context, id string) (*Task, error) {
	var t Task
	if err := c.do(ctx, http.MethodGet, "/tasks/"+url.PathEscape(id), nil, nil, &t, 0); err != nil {
		return nil, err
	}
	return &t, nil
}

// List fetches a page of tasks.
func (c *Client) List(ctx context.Context, f ListFilter) (TaskList, error) {
	q := url.Values{}
	if f.Tenant != "" {
		q.Set("tenant", f.Tenant)
	}
	if f.State != "" {
		q.Set("state", string(f.State))
	}
	if f.After > 0 {
		q.Set("after", strconv.FormatUint(f.After, 10))
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	var out TaskList
	err := c.do(ctx, http.MethodGet, "/tasks", q, nil, &out, 0)
	return out, err
}

// Cancel cancels a task and its dependents.
func (c *Client) Cancel(ctx context.Context, id, reason string) (*Task, error) {
	var t Task
	if err := c.do(ctx, http.MethodPost, "/tasks/"+url.PathEscape(id)+"/cancel", nil, CancelRequest{Reason: reason}, &t, 0); err != nil {
		return nil, err
	}
	return &t, nil
}

// DeadLetters lists dead-lettered tasks.
func (c *Client) DeadLetters(ctx context.Context, tenant string, limit int) ([]*Task, error) {
	q := url.Values{}
	if tenant != "" {
		q.Set("tenant", tenant)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out TaskList
	err := c.do(ctx, http.MethodGet, "/dlq", q, nil, &out, 0)
	return out.Tasks, err
}

// Redrive moves a dead task back to ready.
func (c *Client) Redrive(ctx context.Context, id string) (*Task, error) {
	var t Task
	if err := c.do(ctx, http.MethodPost, "/dlq/"+url.PathEscape(id)+"/redrive", nil, nil, &t, 0); err != nil {
		return nil, err
	}
	return &t, nil
}

// Workers lists workers.
func (c *Client) Workers(ctx context.Context) ([]WorkerStatus, error) {
	var out []WorkerStatus
	err := c.do(ctx, http.MethodGet, "/workers", nil, nil, &out, 0)
	return out, err
}

// Metrics fetches the leader's metrics snapshot.
func (c *Client) Metrics(ctx context.Context) (Snapshot, error) {
	var out Snapshot
	err := c.do(ctx, http.MethodGet, "/metrics", nil, nil, &out, 0)
	return out, err
}

// Register implements WorkerAPI.
func (c *Client) Register(ctx context.Context, req RegisterRequest) (RegisterResponse, error) {
	var out RegisterResponse
	err := c.do(ctx, http.MethodPost, "/workers/register", nil, req, &out, 0)
	return out, err
}

// Heartbeat implements WorkerAPI.
func (c *Client) Heartbeat(ctx context.Context, req HeartbeatRequest) (HeartbeatResponse, error) {
	var out HeartbeatResponse
	err := c.do(ctx, http.MethodPost, "/workers/"+url.PathEscape(req.WorkerID)+"/heartbeat", nil, req, &out, 0)
	return out, err
}

// Lease implements WorkerAPI.
func (c *Client) Lease(ctx context.Context, req LeaseRequest) (LeaseResponse, error) {
	var out LeaseResponse
	err := c.do(ctx, http.MethodPost, "/workers/"+url.PathEscape(req.WorkerID)+"/lease", nil, req, &out, req.Wait.D())
	return out, err
}

// Complete implements WorkerAPI.
func (c *Client) Complete(ctx context.Context, req CompleteRequest) error {
	return c.do(ctx, http.MethodPost, "/tasks/"+url.PathEscape(req.TaskID)+"/complete", nil, req, nil, 0)
}

// Fail implements WorkerAPI.
func (c *Client) Fail(ctx context.Context, req FailRequest) error {
	return c.do(ctx, http.MethodPost, "/tasks/"+url.PathEscape(req.TaskID)+"/fail", nil, req, nil, 0)
}

// Release implements WorkerAPI.
func (c *Client) Release(ctx context.Context, req ReleaseRequest) error {
	return c.do(ctx, http.MethodPost, "/tasks/"+url.PathEscape(req.TaskID)+"/release", nil, req, nil, 0)
}

// Drain implements WorkerAPI.
func (c *Client) Drain(ctx context.Context, workerID string) error {
	return c.do(ctx, http.MethodPost, "/workers/"+url.PathEscape(workerID)+"/drain", nil, nil, nil, 0)
}

// Deregister implements WorkerAPI.
func (c *Client) Deregister(ctx context.Context, workerID string) error {
	return c.do(ctx, http.MethodPost, "/workers/"+url.PathEscape(workerID)+"/deregister", nil, nil, nil, 0)
}

var _ WorkerAPI = (*Client)(nil)
