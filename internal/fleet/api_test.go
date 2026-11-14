package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var (
	adminKey  = Key{ID: "admin", Secret: []byte(strings.Repeat("a", 32)), Roles: []Role{RoleAdmin}}
	workerKey = Key{ID: "worker", Secret: []byte(strings.Repeat("w", 32)), Roles: []Role{RoleWorker}}
	acmeKey   = Key{ID: "acme", Secret: []byte(strings.Repeat("s", 32)), Roles: []Role{RoleSubmitter}, Tenant: "acme"}
	readerKey = Key{ID: "reader", Secret: []byte(strings.Repeat("r", 32)), Roles: []Role{RoleReader}}
)

func newAPIServer(t *testing.T, c *Coordinator, clk Clock) *httptest.Server {
	t.Helper()
	kr, err := NewKeyring(adminKey, workerKey, acmeKey, readerKey)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := NewAuthenticator(AuthConfig{Keyring: kr, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewHandler(ServerConfig{Coordinator: c, Auth: auth})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func newTestClient(t *testing.T, key Key, endpoints ...string) *Client {
	t.Helper()
	cl, err := NewClient(ClientConfig{Endpoints: endpoints, KeyID: key.ID, Secret: key.Secret, RetryBackoff: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return cl
}

func signedRequest(t *testing.T, method, url string, key Key, body []byte, now time.Time) *http.Request {
	t.Helper()
	req, _ := http.NewRequest(method, url, bytes.NewReader(body))
	SignRequest(req, key.ID, key.Secret, body, now)
	return req
}

func TestHMACVerification(t *testing.T) {
	clk := NewFakeClock(t0)
	kr, _ := NewKeyring(adminKey)
	auth, _ := NewAuthenticator(AuthConfig{Keyring: kr, Clock: clk, MaxSkew: time.Minute, MaxNonces: 3})
	body := []byte(`{"x":1}`)
	verify := func(r *http.Request, b []byte) error {
		_, err := auth.Verify(r, b)
		return err
	}

	req := signedRequest(t, "POST", "http://h/v1/fleet/tasks?b=2&a=1", adminKey, body, t0)
	if err := verify(req, body); err != nil {
		t.Fatalf("valid request refused: %v", err)
	}
	// Replaying the identical request is refused.
	wantErr(t, verify(req, body), ErrReplay)

	// Any tampering breaks the signature.
	req = signedRequest(t, "POST", "http://h/v1/fleet/tasks", adminKey, body, t0)
	wantErr(t, verify(req, []byte(`{"x":2}`)), ErrUnauthorized)
	req = signedRequest(t, "POST", "http://h/v1/fleet/tasks", adminKey, body, t0)
	req.URL.Path = "/v1/fleet/dlq"
	wantErr(t, verify(req, body), ErrUnauthorized)
	req = signedRequest(t, "POST", "http://h/v1/fleet/tasks", adminKey, body, t0)
	req.Method = "GET"
	wantErr(t, verify(req, body), ErrUnauthorized)
	req = signedRequest(t, "POST", "http://h/v1/fleet/tasks", adminKey, body, t0)
	req.Header.Set(HeaderNonce, strings.Repeat("0", 32))
	wantErr(t, verify(req, body), ErrUnauthorized)

	// Stale and future timestamps are refused, so old captures cannot be
	// replayed after their nonces are forgotten.
	wantErr(t, verify(signedRequest(t, "GET", "http://h/x", adminKey, nil, t0.Add(-2*time.Minute)), nil), ErrUnauthorized)
	wantErr(t, verify(signedRequest(t, "GET", "http://h/x", adminKey, nil, t0.Add(2*time.Minute)), nil), ErrUnauthorized)

	// Unknown key, wrong secret, missing headers.
	bad := Key{ID: "admin", Secret: []byte(strings.Repeat("z", 32))}
	wantErr(t, verify(signedRequest(t, "GET", "http://h/x", bad, nil, t0), nil), ErrUnauthorized)
	ghost := Key{ID: "ghost", Secret: adminKey.Secret}
	wantErr(t, verify(signedRequest(t, "GET", "http://h/x", ghost, nil, t0), nil), ErrUnauthorized)
	plain, _ := http.NewRequest("GET", "http://h/x", nil)
	wantErr(t, verify(plain, nil), ErrUnauthorized)

	// The nonce cache is bounded: when full of live nonces it refuses
	// rather than forgetting; once they expire there is room again.
	for range 2 {
		if err := verify(signedRequest(t, "GET", "http://h/x", adminKey, nil, t0), nil); err != nil {
			t.Fatal(err)
		}
	}
	err := verify(signedRequest(t, "GET", "http://h/x", adminKey, nil, t0), nil)
	wantErr(t, err, ErrOverloaded)
	clk.Advance(3 * time.Minute)
	if err := verify(signedRequest(t, "GET", "http://h/x", adminKey, nil, clk.Now()), nil); err != nil {
		t.Fatalf("after nonce expiry: %v", err)
	}
	if n := auth.nonces.len(); n != 1 {
		t.Fatalf("expired nonces kept: %d", n)
	}

	// Short secrets are rejected at configuration time.
	_, err = NewKeyring(Key{ID: "weak", Secret: []byte("short")})
	wantErr(t, err, ErrInvalid)
}

func TestAPIAuthAndReplayOverHTTP(t *testing.T) {
	c := openTest(t, testConfig("", RealClock()))
	srv := newAPIServer(t, c, RealClock())
	body := []byte(`{"tasks":[{"kind":"k","idempotency_key":"x"}]}`)
	req := signedRequest(t, "POST", srv.URL+APIPrefix+"/tasks", adminKey, body, time.Now())
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("signed submit: %v %v", resp.Status, err)
	}
	resp.Body.Close()
	// Replay of the captured request.
	req2, _ := http.NewRequest("POST", srv.URL+APIPrefix+"/tasks", bytes.NewReader(body))
	req2.Header = req.Header.Clone()
	resp, err = http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 401 || !strings.Contains(string(b), `"replay"`) {
		t.Fatalf("replay got %d %s", resp.StatusCode, b)
	}
	// Unsigned requests are refused; health is open.
	resp, _ = http.Post(srv.URL+APIPrefix+"/tasks", "application/json", bytes.NewReader(body))
	if resp.StatusCode != 401 {
		t.Fatalf("unsigned got %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp, _ = http.Get(srv.URL + APIPrefix + "/healthz")
	if resp.StatusCode != 200 {
		t.Fatalf("healthz got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAPIRolesTenantsAndResponseSignatures(t *testing.T) {
	c := openTest(t, testConfig("", RealClock()))
	srv := newAPIServer(t, c, RealClock())
	ctx := context.Background()
	acme := newTestClient(t, acmeKey, srv.URL)
	admin := newTestClient(t, adminKey, srv.URL)
	reader := newTestClient(t, readerKey, srv.URL)
	worker := newTestClient(t, workerKey, srv.URL)

	r, err := acme.Submit(ctx, TaskSpec{Kind: "migrate", Payload: json.RawMessage(`{"repo":"a"}`)})
	if err != nil || r.Task.Tenant != "acme" {
		t.Fatalf("tenant-confined submit: %+v %v", r, err)
	}
	_, err = acme.Submit(ctx, TaskSpec{Kind: "migrate", Tenant: "globex"})
	wantErr(t, err, ErrForbidden)
	other, err := admin.Submit(ctx, TaskSpec{Kind: "migrate", Tenant: "globex"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = acme.Get(ctx, other.Task.ID)
	wantErr(t, err, ErrNotFound)
	list, err := acme.List(ctx, ListFilter{})
	if err != nil || len(list.Tasks) != 1 || list.Tasks[0].Tenant != "acme" {
		t.Fatalf("acme list = %+v %v", list, err)
	}
	_, err = acme.Cancel(ctx, other.Task.ID, "")
	wantErr(t, err, ErrNotFound)

	// Roles.
	_, err = reader.Submit(ctx, TaskSpec{Kind: "k"})
	wantErr(t, err, ErrForbidden)
	_, err = acme.Register(ctx, RegisterRequest{WorkerID: "sneaky"})
	wantErr(t, err, ErrForbidden)
	_, err = worker.Redrive(ctx, r.Task.ID)
	wantErr(t, err, ErrForbidden)
	if _, err := reader.Metrics(ctx); err != nil {
		t.Fatalf("reader metrics: %v", err)
	}

	// Client-side errors map back to sentinels.
	_, err = admin.Get(ctx, "nope")
	wantErr(t, err, ErrNotFound)

	// Responses are signed and bound to the request: a tampered or
	// re-targeted response is rejected by the client.
	tamper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rec := httptest.NewRecorder()
		pr, _ := http.NewRequest(req.Method, srv.URL+req.URL.RequestURI(), req.Body)
		pr.Header = req.Header.Clone()
		resp, err := http.DefaultClient.Do(pr)
		if err != nil {
			t.Error(err)
			return
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		for k, v := range resp.Header {
			rec.Header()[k] = v
		}
		b = bytes.Replace(b, []byte(`"acme"`), []byte(`"evil"`), 1)
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		w.Write(b)
	}))
	defer tamper.Close()
	mitm := newTestClient(t, acmeKey, tamper.URL)
	_, err = mitm.Get(ctx, r.Task.ID)
	wantErr(t, err, ErrBadSignature)
}

func TestClientFollowsLeader(t *testing.T) {
	leaderC := openTest(t, testConfig("", RealClock()))
	follower, _ := New(testConfig("", RealClock()))
	defer follower.Close()
	leaderSrv := newAPIServer(t, leaderC, RealClock())
	followerSrv := newAPIServer(t, follower, RealClock())
	follower.SetLeaderHint(func(context.Context) string { return leaderSrv.URL })

	cl := newTestClient(t, adminKey, followerSrv.URL, leaderSrv.URL)
	r, err := cl.Submit(context.Background(), TaskSpec{Kind: "k"})
	if err != nil {
		t.Fatalf("submit through follower: %v", err)
	}
	if cl.endpoint() != leaderSrv.URL {
		t.Fatalf("client did not switch to the leader")
	}
	if _, err := leaderC.Get(context.Background(), r.Task.ID); err != nil {
		t.Fatal(err)
	}
	// The follower's health probe reports the hint.
	h, err := newTestClient(t, adminKey, followerSrv.URL).Health(context.Background())
	if err != nil || h.Leader || h.LeaderHint != leaderSrv.URL {
		t.Fatalf("follower health = %+v %v", h, err)
	}
}

// TestWorkersOverHTTP runs real workers against the HTTP API: tasks are
// leased, executed, failed and retried, and completed through signed
// requests.
func TestWorkersOverHTTP(t *testing.T) {
	cfg := testConfig(t.TempDir(), RealClock())
	cfg.HeartbeatInterval = 50 * time.Millisecond
	cfg.TickInterval = 10 * time.Millisecond
	c := openTest(t, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	srv := newAPIServer(t, c, RealClock())
	admin := newTestClient(t, adminKey, srv.URL)

	var specs []TaskSpec
	for i := range 60 {
		specs = append(specs, TaskSpec{ID: fmt.Sprintf("h%d", i), Kind: "echo", Payload: json.RawMessage(fmt.Sprint(i)), MaxAttempts: 3})
	}
	if _, err := admin.SubmitBatch(ctx, specs); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	failedOnce := map[string]bool{}
	exec := ExecutorFunc(func(ctx context.Context, a Assignment) (json.RawMessage, error) {
		mu.Lock()
		defer mu.Unlock()
		// Every fifth task fails its first attempt.
		var n int
		json.Unmarshal(a.Task.Payload, &n)
		if n%5 == 0 && !failedOnce[a.Task.ID] {
			failedOnce[a.Task.ID] = true
			return nil, fmt.Errorf("transient")
		}
		return json.RawMessage(fmt.Sprintf(`{"double":%d}`, 2*n)), nil
	})
	var wg sync.WaitGroup
	wctx, wcancel := context.WithCancel(ctx)
	var workers []*Worker
	for i := range 3 {
		w, err := NewWorker(WorkerConfig{
			ID: fmt.Sprintf("hw%d", i), API: newTestClient(t, workerKey, srv.URL), Executor: exec,
			Capabilities: Capabilities{MaxConcurrency: 4}, PollWait: 200 * time.Millisecond, RetryMin: 10 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		workers = append(workers, w)
		wg.Add(1)
		go func() { defer wg.Done(); w.Run(wctx) }()
	}
	waitFor(t, 20*time.Second, "all tasks to succeed", func() bool {
		return c.Metrics(ctx).States[StateSucceeded] == 60
	})
	wcancel()
	wg.Wait()
	got, _ := admin.Get(ctx, "h7")
	if string(got.Result) != `{"double":14}` {
		t.Fatalf("result = %s", got.Result)
	}
	if got, _ := admin.Get(ctx, "h5"); got.Attempts != 2 {
		t.Fatalf("retried task attempts = %d", got.Attempts)
	}
	ws, _ := admin.Workers(ctx)
	if len(ws) != 0 {
		t.Fatalf("workers did not deregister on drain: %+v", ws)
	}
	var total uint64
	for _, w := range workers {
		total += w.Stats().Succeeded
	}
	if total != 60 {
		t.Fatalf("worker successes = %d", total)
	}
	dlq, err := admin.DeadLetters(ctx, "", 0)
	if err != nil || len(dlq) != 0 {
		t.Fatalf("dlq = %v %v", dlq, err)
	}
}

func TestWorkerGracefulDrainReleasesSlowTasks(t *testing.T) {
	c := openTest(t, testConfig("", RealClock()))
	mustSubmit(t, c, TaskSpec{ID: "slow"})
	var started atomic.Bool
	w, _ := NewWorker(WorkerConfig{
		ID: "dw", API: c, PollWait: 50 * time.Millisecond, DrainTimeout: 50 * time.Millisecond,
		Executor: ExecutorFunc(func(ctx context.Context, a Assignment) (json.RawMessage, error) {
			started.Store(true)
			<-ctx.Done()
			return nil, ctx.Err()
		}),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- w.Run(ctx) }()
	waitFor(t, 5*time.Second, "task start", started.Load)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := mustState(t, c, "slow", StateReady); got.Attempts != 0 {
		t.Fatalf("released task attempts = %d", got.Attempts)
	}
	if w.Stats().Released != 1 {
		t.Fatalf("stats = %+v", w.Stats())
	}
}

func TestWorkerAbortsRevokedTask(t *testing.T) {
	cfg := testConfig("", RealClock())
	c := openTest(t, cfg)
	mustSubmit(t, c, TaskSpec{ID: "victim"})
	aborted := make(chan struct{})
	w, _ := NewWorker(WorkerConfig{
		ID: "rw", API: c, PollWait: 50 * time.Millisecond, HeartbeatInterval: 20 * time.Millisecond,
		Executor: ExecutorFunc(func(ctx context.Context, a Assignment) (json.RawMessage, error) {
			<-ctx.Done()
			close(aborted)
			return nil, ctx.Err()
		}),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	waitFor(t, 5*time.Second, "lease", func() bool { return w.Running() == 1 })
	if _, err := c.Cancel(context.Background(), "victim", "stop"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-aborted:
	case <-time.After(5 * time.Second):
		t.Fatal("revoked task was not aborted")
	}
	waitFor(t, 5*time.Second, "revocation stat", func() bool { return w.Stats().Revoked == 1 })
}
