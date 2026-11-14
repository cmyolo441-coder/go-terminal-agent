package identity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type mwFixture struct {
	idp     *fakeIdP
	auth    *Authenticator
	handler http.Handler
	apiKey  string
	scoped  string
	clk     *fakeClock

	mu        sync.Mutex
	decisions []Decision
	failures  []error
}

func newMWFixture(t *testing.T, expose bool) *mwFixture {
	t.Helper()
	f := &mwFixture{idp: newFakeIdP(t), clk: newFakeClock()}
	keys := newRemoteSet(t, f.idp, f.clk, nil)
	tv, err := NewJWTVerifier(keys, ValidationOptions{Issuers: []string{f.idp.issuer}, Audiences: []string{"gta-api"}, Now: f.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	key, id, hash, err := GenerateAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	skey, sid, shash, _ := GenerateAPIKey()
	store, err := NewMemoryAPIKeyStore(
		APIKeyRecord{ID: id, Hash: hash, Principal: "svc-ci", Roles: []string{"developer"}},
		APIKeyRecord{ID: sid, Hash: shash, Principal: "svc-reader", Roles: []string{"admin"}, Scopes: []string{"repo:read"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	f.apiKey, f.scoped = key, skey
	akv, _ := NewAPIKeyVerifier(store)
	f.auth, err = NewAuthenticator(AuthenticatorOptions{
		Tokens: tv, APIKeys: akv, Authorizer: testEngine(t), Environment: "dev",
		Attributes: func(r *http.Request) RequestAttributes {
			return RequestAttributes{Workspace: r.Header.Get("X-Workspace")}
		},
		ExposeDecisionReason: expose,
		OnDecision: func(_ *http.Request, _ *Principal, _ string, d Decision) {
			f.mu.Lock()
			f.decisions = append(f.decisions, d)
			f.mu.Unlock()
		},
		OnAuthFailure: func(_ *http.Request, err error) {
			f.mu.Lock()
			f.failures = append(f.failures, err)
			f.mu.Unlock()
		},
		Now: f.clk.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	whoami := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := PrincipalFrom(r.Context())
		if !ok {
			t.Error("no principal in context")
		}
		_ = json.NewEncoder(w).Encode(p)
	})
	mux.Handle("/whoami", whoami)
	mux.Handle("/repo/write", f.auth.Require("repo:write")(whoami))
	mux.Handle("/repo/read", f.auth.Require("repo:read")(whoami))
	mux.Handle("/either", f.auth.RequireAny("billing:read", "repo:read")(whoami))
	f.handler = f.auth.Authenticate(mux)
	return f
}

func (f *mwFixture) token(t *testing.T, mut func(map[string]any)) string {
	c := f.idp.claims(f.clk.Now())
	c["groups"] = []string{"eng"}
	c["email"] = "a@example.com"
	c["preferred_username"] = "alice"
	if mut != nil {
		mut(c)
	}
	return signJWT(t, f.idp.signing, c, nil)
}

func (f *mwFixture) do(path string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func TestAuthenticateJWT(t *testing.T) {
	f := newMWFixture(t, false)
	rec := f.do("/whoami", map[string]string{"Authorization": "Bearer " + f.token(t, nil)})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var p Principal
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Subject != "user-123" || p.AuthMethod != "jwt" || p.Name != "alice" || p.Email != "a@example.com" ||
		strings.Join(p.Roles, ",") != "developer" || strings.Join(p.Groups, ",") != "eng" || p.Issuer != f.idp.issuer {
		t.Fatalf("principal = %+v", p)
	}
	// Lower-case scheme is accepted.
	if rec := f.do("/repo/write", map[string]string{"Authorization": "bearer " + f.token(t, nil), "X-Workspace": "/srv/app"}); rec.Code != http.StatusOK {
		t.Fatalf("developer write: %d %s", rec.Code, rec.Body)
	}
}

func TestAuthenticateFailures(t *testing.T) {
	f := newMWFixture(t, false)
	expired := f.token(t, func(c map[string]any) { c["exp"] = f.clk.Now().Add(-time.Hour).Unix() })
	wrongAud := f.token(t, func(c map[string]any) { c["aud"] = "x" })
	noSub := f.token(t, func(c map[string]any) { delete(c, "sub") })
	cases := []struct {
		name   string
		hdr    map[string]string
		status int
		desc   string
	}{
		{"none", nil, http.StatusUnauthorized, ""},
		{"basic scheme", map[string]string{"Authorization": "Basic dTpw"}, http.StatusBadRequest, "Bearer scheme"},
		{"empty bearer", map[string]string{"Authorization": "Bearer "}, http.StatusBadRequest, ""},
		{"garbage", map[string]string{"Authorization": "Bearer abc"}, http.StatusUnauthorized, "invalid token"},
		{"expired", map[string]string{"Authorization": "Bearer " + expired}, http.StatusUnauthorized, "token expired"},
		{"wrong aud", map[string]string{"Authorization": "Bearer " + wrongAud}, http.StatusUnauthorized, "invalid token"},
		{"no sub", map[string]string{"Authorization": "Bearer " + noSub}, http.StatusUnauthorized, "not acceptable"},
		{"bad api key", map[string]string{"Authorization": "Bearer gta_0123456789abcdef_" + strings.Repeat("A", 43)}, http.StatusUnauthorized, "invalid API key"},
		{"both headers", map[string]string{"Authorization": "Bearer " + f.token(t, nil), "X-API-Key": f.apiKey}, http.StatusBadRequest, "both"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.do("/whoami", tc.hdr)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.status, rec.Body)
			}
			ch := rec.Header().Get("WWW-Authenticate")
			if !strings.HasPrefix(ch, `Bearer realm="gta"`) {
				t.Fatalf("challenge = %q", ch)
			}
			if tc.desc != "" && !strings.Contains(ch+rec.Body.String(), tc.desc) {
				t.Fatalf("response lacks %q: %s %s", tc.desc, ch, rec.Body)
			}
		})
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.failures) != len(cases) {
		t.Fatalf("OnAuthFailure called %d times", len(f.failures))
	}
}

func TestAPIKeyAuthAndRequire(t *testing.T) {
	f := newMWFixture(t, true)
	rec := f.do("/whoami", map[string]string{"X-API-Key": f.apiKey})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"service_account"`) || !strings.Contains(rec.Body.String(), `"api_key"`) {
		t.Fatalf("api key whoami: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("/repo/write", map[string]string{"Authorization": "Bearer " + f.apiKey, "X-Workspace": "/srv/app"}); rec.Code != http.StatusOK {
		t.Fatalf("developer key write: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("/repo/write", map[string]string{"Authorization": "Bearer " + f.apiKey, "X-Workspace": "/srv/secrets/db"}); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "protect-secrets") {
		t.Fatalf("secrets write: %d %s", rec.Code, rec.Body)
	}
	// Admin roles are capped by the key's scopes.
	if rec := f.do("/repo/read", map[string]string{"X-API-Key": f.scoped}); rec.Code != http.StatusOK {
		t.Fatalf("scoped read: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("/repo/write", map[string]string{"X-API-Key": f.scoped, "X-Workspace": "/srv/app"}); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "scopes") {
		t.Fatalf("scoped write: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("/either", map[string]string{"X-API-Key": f.scoped}); rec.Code != http.StatusOK {
		t.Fatalf("RequireAny: %d %s", rec.Code, rec.Body)
	}
	f.mu.Lock()
	n := len(f.decisions)
	f.mu.Unlock()
	if n < 5 {
		t.Fatalf("OnDecision called %d times", n)
	}
}

func TestRequireHidesReasonByDefault(t *testing.T) {
	f := newMWFixture(t, false)
	rec := f.do("/repo/write", map[string]string{"X-API-Key": f.scoped, "X-Workspace": "/srv/app"})
	if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "scopes") || !strings.Contains(rec.Body.String(), "repo:write") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// Require without Authenticate is 401, not a panic or a pass.
	h := f.auth.Require("repo:read")(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("handler reached") }))
	r := httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/", nil))
	if r.Code != http.StatusUnauthorized {
		t.Fatalf("status %d", r.Code)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("Require with an invalid permission did not panic")
		}
	}()
	f.auth.Require("repo:*")
}

func TestAPIKeyVerifier(t *testing.T) {
	key, id, hash, err := GenerateAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, "gta_"+id+"_") || len(hash) != 64 {
		t.Fatalf("key %q id %q", key, id)
	}
	if got, err := ParseAPIKeyID(key); err != nil || got != id {
		t.Fatalf("ParseAPIKeyID = %q, %v", got, err)
	}
	clk := newFakeClock()
	_, id2, hash2, _ := GenerateAPIKey()
	key3, id3, hash3, _ := GenerateAPIKey()
	key4, id4, hash4, _ := GenerateAPIKey()
	store, err := NewMemoryAPIKeyStore(
		APIKeyRecord{ID: id, Hash: hash, Principal: "svc", Attributes: map[string]string{"team": "a"}},
		APIKeyRecord{ID: id2, Hash: hash2, Principal: "svc2"},
		APIKeyRecord{ID: id3, Hash: hash3, Principal: "svc3", Disabled: true},
		APIKeyRecord{ID: id4, Hash: hash4, Principal: "svc4", ExpiresAt: clk.Now().Add(-time.Second)},
	)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := NewAPIKeyVerifier(store)
	v.now = clk.Now
	ctx := context.Background()
	p, err := v.Verify(ctx, key)
	if err != nil || p.Subject != "svc" || p.Type != PrincipalServiceAccount || p.CredentialID != id || p.Attributes["team"] != "a" || p.Scopes != nil {
		t.Fatalf("verify: %+v %v", p, err)
	}
	// Right ID, wrong secret.
	last := "A"
	if key[len(key)-5] == 'A' {
		last = "B"
	}
	// Change a character away from the end so the base64 stays canonical.
	forged := key[:len(key)-5] + last + key[len(key)-4:]
	if _, err := v.Verify(ctx, forged); !errors.Is(err, ErrInvalidAPIKey) {
		t.Fatalf("forged: %v", err)
	}
	unknown, _, _, _ := GenerateAPIKey()
	for name, k := range map[string]string{"unknown": unknown, "short": "gta_x", "no prefix": strings.TrimPrefix(key, "gta_"), "upper id": "gta_" + strings.ToUpper(id) + key[20:]} {
		if _, err := v.Verify(ctx, k); !errors.Is(err, ErrInvalidAPIKey) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := v.Verify(ctx, key3); !errors.Is(err, ErrAPIKeyDisabled) {
		t.Fatalf("disabled: %v", err)
	}
	if _, err := v.Verify(ctx, key4); !errors.Is(err, ErrAPIKeyExpired) {
		t.Fatalf("expired: %v", err)
	}
	store.Remove(id)
	if _, err := v.Verify(ctx, key); !errors.Is(err, ErrInvalidAPIKey) {
		t.Fatalf("revoked: %v", err)
	}
	for _, bad := range []APIKeyRecord{
		{ID: "xyz", Hash: hash, Principal: "p"},
		{ID: id, Hash: "abc", Principal: "p"},
		{ID: id, Hash: hash},
		{ID: id, Hash: hash, Principal: "p", Scopes: []string{"bad scope"}},
		{ID: id, Hash: hash, Principal: "p", Type: "robot"},
	} {
		if _, err := NewMemoryAPIKeyStore(bad); err == nil {
			t.Errorf("record %+v accepted", bad)
		}
	}
}

func TestLoadAPIKeysFile(t *testing.T) {
	_, id, hash, _ := GenerateAPIKey()
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")
	good := `{"keys":[{"id":"` + id + `","hash":"` + hash + `","principal":"svc","roles":["viewer"],"scopes":["repo:read"]}]}`
	if err := os.WriteFile(path, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadAPIKeysFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := s.LookupAPIKey(context.Background(), id); r == nil || r.Principal != "svc" {
		t.Fatal("record not loaded")
	}
	for name, src := range map[string]string{
		"unknown field": `{"keys":[{"id":"` + id + `","hash":"` + hash + `","principal":"svc","role":"x"}]}`,
		"duplicate":     `{"keys":[{"id":"` + id + `","hash":"` + hash + `","principal":"a"},{"id":"` + id + `","hash":"` + hash + `","principal":"b"}]}`,
		"trailing":      good + "{}",
	} {
		_ = os.WriteFile(path, []byte(src), 0o600)
		if _, err := LoadAPIKeysFile(path); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if runtime.GOOS != "windows" {
		_ = os.WriteFile(path, []byte(good), 0o600)
		_ = os.Chmod(path, 0o644)
		if _, err := LoadAPIKeysFile(path); err == nil {
			t.Error("world-readable key file accepted")
		}
	}
}

func TestNewAuthenticatorValidation(t *testing.T) {
	if _, err := NewAuthenticator(AuthenticatorOptions{}); err == nil {
		t.Fatal("no verifiers accepted")
	}
	store, _ := NewMemoryAPIKeyStore()
	akv, _ := NewAPIKeyVerifier(store)
	if _, err := NewAuthenticator(AuthenticatorOptions{APIKeys: akv, Realm: `a"b`}); err == nil {
		t.Fatal("bad realm accepted")
	}
	a, err := NewAuthenticator(AuthenticatorOptions{APIKeys: akv})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer a.b.c")
	a.Authenticate(http.NotFoundHandler()).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "not accepted") {
		t.Fatalf("JWT without token verifier: %d %s", rec.Code, rec.Body)
	}
}
