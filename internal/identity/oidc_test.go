package identity

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestClient(t *testing.T, idp *fakeIdP) (*Client, *[]time.Duration) {
	t.Helper()
	c, err := NewClient(context.Background(), ClientConfig{Issuer: idp.issuer, ClientID: idp.clientID, HTTPClient: idp.srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	var mu sync.Mutex
	slept := &[]time.Duration{}
	c.sleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		*slept = append(*slept, d)
		mu.Unlock()
		return ctx.Err()
	}
	return c, slept
}

func TestDiscovery(t *testing.T) {
	idp := newFakeIdP(t)
	m, err := Discover(context.Background(), idp.issuer, idp.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if m.TokenEndpoint != idp.issuer+"/token" || m.DeviceAuthorizationEndpoint == "" {
		t.Fatalf("metadata = %+v", m)
	}
	// Issuer mismatch (metadata substitution) is refused.
	if _, err := Discover(context.Background(), idp.issuer+"/", idp.srv.Client()); err == nil {
		t.Fatal("issuer mismatch accepted")
	}
	if _, err := Discover(context.Background(), "http://idp.example.com", nil); err == nil {
		t.Fatal("plain http issuer accepted")
	}
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONTest(w, 200, map[string]string{"issuer": "http://" + r.Host, "token_endpoint": "http://evil.example.com/token", "jwks_uri": "http://" + r.Host + "/jwks"})
	}))
	defer evil.Close()
	if _, err := Discover(context.Background(), evil.URL, evil.Client()); err == nil || !strings.Contains(err.Error(), "token_endpoint") {
		t.Fatalf("non-https token endpoint accepted: %v", err)
	}
}

func TestDeviceFlowHappyPath(t *testing.T) {
	idp := newFakeIdP(t)
	idp.devicePlan = []string{"authorization_pending", "authorization_pending"}
	c, slept := newTestClient(t, idp)
	var shown *DeviceAuthorization
	tok, err := c.DeviceLogin(context.Background(), func(d *DeviceAuthorization) error { shown = d; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if shown == nil || shown.UserCode != "WDJB-MJHT" || !strings.HasPrefix(shown.VerificationURIComplete, idp.issuer) {
		t.Fatalf("shown = %+v", shown)
	}
	if strings.Contains(shown.String(), "dev-code-1") {
		t.Fatal("device code leaked by String")
	}
	if tok.AccessToken == "" || tok.RefreshToken == "" || tok.Subject != "user-123" || tok.Expiry.IsZero() {
		t.Fatalf("token = %v", tok)
	}
	if want := []time.Duration{time.Second, time.Second, time.Second}; fmt.Sprint(*slept) != fmt.Sprint(want) {
		t.Fatalf("poll intervals = %v, want %v", *slept, want)
	}
	if s := fmt.Sprintf("%v %+v %#v", tok, tok, *tok); strings.Contains(s, tok.AccessToken) || strings.Contains(s, tok.RefreshToken) {
		t.Fatal("token secrets leaked through fmt")
	}
}

func TestDeviceFlowSlowDown(t *testing.T) {
	idp := newFakeIdP(t)
	idp.devicePlan = []string{"authorization_pending", "slow_down", "authorization_pending", "slow_down"}
	c, slept := newTestClient(t, idp)
	if _, err := c.DeviceLogin(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{1 * time.Second, 1 * time.Second, 6 * time.Second, 6 * time.Second, 11 * time.Second}
	if fmt.Sprint(*slept) != fmt.Sprint(want) {
		t.Fatalf("intervals = %v, want %v", *slept, want)
	}
}

func TestDeviceFlowTerminalErrors(t *testing.T) {
	for _, tc := range []struct {
		plan []string
		want error
	}{
		{[]string{"authorization_pending", "expired_token"}, ErrDeviceCodeExpired},
		{[]string{"access_denied"}, ErrAccessDenied},
	} {
		idp := newFakeIdP(t)
		idp.devicePlan = tc.plan
		c, _ := newTestClient(t, idp)
		if _, err := c.DeviceLogin(context.Background(), nil); !errors.Is(err, tc.want) {
			t.Errorf("plan %v: err = %v, want %v", tc.plan, err, tc.want)
		}
	}
	// Local expiry also ends polling without hitting the server.
	idp := newFakeIdP(t)
	c, _ := newTestClient(t, idp)
	da, err := c.StartDeviceAuthorization(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	da.ExpiresAt = time.Now().Add(-time.Second)
	if _, err := c.PollDeviceToken(context.Background(), da); !errors.Is(err, ErrDeviceCodeExpired) {
		t.Fatalf("err = %v", err)
	}
}

func TestDeviceFlowTransientBackoff(t *testing.T) {
	var n int
	var mu sync.Mutex
	idp := newFakeIdP(t)
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		cur := n
		mu.Unlock()
		if cur <= 2 {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		// Proxy to the real token endpoint.
		body, _ := io.ReadAll(r.Body)
		resp, err := idp.srv.Client().Post(idp.issuer+"/token", "application/x-www-form-urlencoded", strings.NewReader(string(body)))
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer flaky.Close()
	m, err := Discover(context.Background(), idp.issuer, idp.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	m.TokenEndpoint = flaky.URL
	c, err := NewClient(context.Background(), ClientConfig{ClientID: idp.clientID, HTTPClient: idp.srv.Client(), Metadata: m})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var slept []time.Duration
	c.sleep = func(ctx context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	if _, err := c.DeviceLogin(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}; fmt.Sprint(slept) != fmt.Sprint(want) {
		t.Fatalf("backoff = %v, want %v", slept, want)
	}
}

func browserGet(t *testing.T) (func(string) error, chan error) {
	done := make(chan error, 1)
	return func(u string) error {
		go func() {
			resp, err := http.Get(u)
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					err = fmt.Errorf("callback status %d", resp.StatusCode)
				}
			}
			done <- err
		}()
		return nil
	}, done
}

func TestAuthCodePKCE(t *testing.T) {
	idp := newFakeIdP(t)
	c, _ := newTestClient(t, idp)
	open, done := browserGet(t)
	tok, err := c.AuthCodeLogin(context.Background(), AuthCodeOptions{OpenBrowser: open, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if tok.IDToken == "" || tok.Subject != "user-123" {
		t.Fatalf("token = %v", tok)
	}
	// Refresh keeps the subject and works with the stored refresh token.
	nt, err := c.Refresh(context.Background(), tok)
	if err != nil {
		t.Fatal(err)
	}
	if nt.AccessToken == tok.AccessToken && nt.RefreshToken == tok.RefreshToken {
		t.Fatal("refresh returned the same token")
	}
}

func TestAuthCodeWrongVerifier(t *testing.T) {
	idp := newFakeIdP(t)
	c, _ := newTestClient(t, idp)
	req, err := c.NewAuthCodeRequest("http://127.0.0.1:1/callback", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(req.URL)
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") != S256Challenge(req.PKCE.Verifier) || len(req.PKCE.Verifier) != 43 {
		t.Fatalf("bad PKCE parameters: %v", q)
	}
	// Drive the authorize endpoint without following the redirect.
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Get(req.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	code := loc.Query().Get("code")
	other, _ := NewPKCE()
	_, err = c.ExchangeCode(context.Background(), code, other.Verifier, req.RedirectURI, req.Nonce)
	var oe *OAuthError
	if !errors.As(err, &oe) || oe.Code != "invalid_grant" {
		t.Fatalf("wrong verifier: err = %v", err)
	}
}

func TestAuthCodeCallbackDefenses(t *testing.T) {
	t.Run("forged state ignored", func(t *testing.T) {
		idp := newFakeIdP(t)
		c, _ := newTestClient(t, idp)
		var forgedStatus int
		open := func(u string) error {
			pu, _ := url.Parse(u)
			redirect := pu.Query().Get("redirect_uri")
			go func() {
				resp, err := http.Get(redirect + "?code=attacker&state=wrong")
				if err == nil {
					forgedStatus = resp.StatusCode
					resp.Body.Close()
				}
				resp, err = http.Get(u)
				if err == nil {
					resp.Body.Close()
				}
			}()
			return nil
		}
		tok, err := c.AuthCodeLogin(context.Background(), AuthCodeOptions{OpenBrowser: open, Timeout: 10 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		if forgedStatus != http.StatusBadRequest || tok.AccessToken == "" {
			t.Fatalf("forged status %d", forgedStatus)
		}
	})
	t.Run("issuer mix-up", func(t *testing.T) {
		idp := newFakeIdP(t)
		idp.badIss = true
		c, _ := newTestClient(t, idp)
		open, _ := browserGet(t)
		_, err := c.AuthCodeLogin(context.Background(), AuthCodeOptions{OpenBrowser: open, Timeout: 10 * time.Second})
		if err == nil || !strings.Contains(err.Error(), "mix-up") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("access denied", func(t *testing.T) {
		idp := newFakeIdP(t)
		c, _ := newTestClient(t, idp)
		open := func(u string) error {
			pu, _ := url.Parse(u)
			q := pu.Query()
			go func() {
				resp, err := http.Get(q.Get("redirect_uri") + "?" + url.Values{"error": {"access_denied"}, "state": {q.Get("state")}}.Encode())
				if err == nil {
					resp.Body.Close()
				}
			}()
			return nil
		}
		if _, err := c.AuthCodeLogin(context.Background(), AuthCodeOptions{OpenBrowser: open, Timeout: 10 * time.Second}); !errors.Is(err, ErrAccessDenied) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		idp := newFakeIdP(t)
		c, _ := newTestClient(t, idp)
		_, err := c.AuthCodeLogin(context.Background(), AuthCodeOptions{OpenBrowser: func(string) error { return nil }, Timeout: 50 * time.Millisecond})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("non-loopback listener", func(t *testing.T) {
		idp := newFakeIdP(t)
		c, _ := newTestClient(t, idp)
		if _, err := c.AuthCodeLogin(context.Background(), AuthCodeOptions{OpenBrowser: func(string) error { return nil }, ListenAddr: "0.0.0.0:0"}); err == nil {
			t.Fatal("wildcard listener accepted")
		}
	})
}

func TestIDTokenNonceAndRotation(t *testing.T) {
	idp := newFakeIdP(t)
	c, _ := newTestClient(t, idp)
	now := time.Now()
	claims := map[string]any{"iss": idp.issuer, "aud": idp.clientID, "sub": "u", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "nonce": "n1"}
	tok := signJWT(t, idp.signing, claims, nil)
	if _, err := c.VerifyIDToken(context.Background(), tok, "n1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.VerifyIDToken(context.Background(), tok, "n2"); !errors.Is(err, ErrInvalidNonce) {
		t.Fatalf("nonce mismatch: %v", err)
	}
	claims["aud"] = []string{idp.clientID, "other"}
	if _, err := c.VerifyIDToken(context.Background(), signJWT(t, idp.signing, claims, nil), ""); !errors.Is(err, ErrInvalidAuthorizedParty) {
		t.Fatalf("multi-aud without azp: %v", err)
	}
	// Key rotation is picked up through the client's key set.
	idp.rotate(keyFor(t, ES384))
	claims["aud"] = idp.clientID
	if _, err := c.VerifyIDToken(context.Background(), signJWT(t, idp.signing, claims, nil), "n1"); err != nil {
		t.Fatalf("rotated key: %v", err)
	}
}

func TestTokenSourceRefreshAndRelogin(t *testing.T) {
	idp := newFakeIdP(t)
	idp.rotateRT = true
	c, _ := newTestClient(t, idp)
	idp.devicePlan = nil
	tok, err := c.DeviceLogin(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryTokenStore()
	ts := c.TokenSource(store)
	if _, err := ts.Token(context.Background()); !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("empty store: %v", err)
	}
	tok.Expiry = time.Now().Add(-time.Minute)
	if err := store.Save(context.Background(), c.StoreKey(), tok); err != nil {
		t.Fatal(err)
	}
	got, err := ts.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.RefreshToken == tok.RefreshToken || got.Expired(time.Now(), 0) {
		t.Fatal("token not refreshed")
	}
	saved, _ := store.Load(context.Background(), c.StoreKey())
	if saved.RefreshToken != got.RefreshToken {
		t.Fatal("refreshed token not persisted")
	}
	// Valid token: no refresh call.
	hits := idp.tokenHits.Load()
	if _, err := ts.Token(context.Background()); err != nil || idp.tokenHits.Load() != hits {
		t.Fatalf("valid token refreshed: %v", err)
	}
	// Revoked refresh token: login required and stale token removed.
	saved.Expiry = time.Now().Add(-time.Minute)
	saved.RefreshToken = "revoked"
	_ = store.Save(context.Background(), c.StoreKey(), saved)
	if _, err := ts.Token(context.Background()); !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("revoked: %v", err)
	}
	if _, err := store.Load(context.Background(), c.StoreKey()); !errors.Is(err, ErrTokenNotFound) {
		t.Fatal("revoked token not deleted")
	}
}

func TestClientConfigValidation(t *testing.T) {
	if _, err := NewClient(context.Background(), ClientConfig{Issuer: "https://x"}); err == nil {
		t.Fatal("missing client id accepted")
	}
	m := &ProviderMetadata{Issuer: "https://idp.example.com", TokenEndpoint: "https://idp.example.com/token", JWKSURI: "https://idp.example.com/jwks", IDTokenSigningAlgValuesSupported: []string{"HS256"}}
	if _, err := NewClient(context.Background(), ClientConfig{ClientID: "x", Metadata: m}); err == nil {
		t.Fatal("HS256-only provider accepted")
	}
	m.IDTokenSigningAlgValuesSupported = nil
	m.TokenEndpoint = "http://idp.example.com/token"
	if _, err := NewClient(context.Background(), ClientConfig{ClientID: "x", Metadata: m}); err == nil {
		t.Fatal("http token endpoint accepted")
	}
}

func TestParseExpiresIn(t *testing.T) {
	for in, want := range map[string]int64{`3600`: 3600, `"3600"`: 3600, `3600.0`: 3600} {
		if got, ok := parseExpiresIn([]byte(in)); !ok || got != want {
			t.Errorf("parseExpiresIn(%s) = %d, %v", in, got, ok)
		}
	}
	for _, in := range []string{``, `null`, `-1`, `"x"`} {
		if _, ok := parseExpiresIn([]byte(in)); ok {
			t.Errorf("parseExpiresIn(%s) accepted", in)
		}
	}
}
