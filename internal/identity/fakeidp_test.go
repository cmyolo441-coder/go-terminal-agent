package identity

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testKey is a signing key with the algorithm and kid it signs under.
type testKey struct {
	alg    Algorithm
	kid    string
	signer crypto.Signer
}

func (k *testKey) jwk(t testing.TB) *JWK {
	t.Helper()
	j, err := NewJWK(k.signer.Public(), k.kid, k.alg)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

var (
	keyOnce    sync.Once
	sharedRSA  *rsa.PrivateKey
	sharedRSA2 *rsa.PrivateKey
	sharedP256 *ecdsa.PrivateKey
	sharedP384 *ecdsa.PrivateKey
	sharedP521 *ecdsa.PrivateKey
	sharedEd   ed25519.PrivateKey
)

func genKeys(t testing.TB) {
	t.Helper()
	keyOnce.Do(func() {
		var err error
		must := func(e error) {
			if e != nil {
				panic(e)
			}
		}
		sharedRSA, err = rsa.GenerateKey(rand.Reader, 2048)
		must(err)
		sharedRSA2, err = rsa.GenerateKey(rand.Reader, 2048)
		must(err)
		sharedP256, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		must(err)
		sharedP384, err = ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
		must(err)
		sharedP521, err = ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
		must(err)
		_, sharedEd, err = ed25519.GenerateKey(rand.Reader)
		must(err)
	})
}

// keyFor returns a test key for alg with kid "<alg>-1".
func keyFor(t testing.TB, alg Algorithm) *testKey {
	genKeys(t)
	k := &testKey{alg: alg, kid: string(alg) + "-1"}
	switch alg {
	case RS256, RS384, RS512, PS256, PS384, PS512:
		k.signer = sharedRSA
	case ES256:
		k.signer = sharedP256
	case ES384:
		k.signer = sharedP384
	case ES512:
		k.signer = sharedP521
	case EdDSA:
		k.signer = sharedEd
	default:
		t.Fatalf("no key for %s", alg)
	}
	return k
}

// signRaw produces a JWS signature over input.
func signRaw(t testing.TB, alg Algorithm, signer crypto.Signer, input []byte) []byte {
	t.Helper()
	if alg == EdDSA {
		return ed25519.Sign(signer.(ed25519.PrivateKey), input)
	}
	h := alg.hash().New()
	h.Write(input)
	digest := h.Sum(nil)
	switch alg {
	case RS256, RS384, RS512:
		sig, err := rsa.SignPKCS1v15(rand.Reader, signer.(*rsa.PrivateKey), alg.hash(), digest)
		if err != nil {
			t.Fatal(err)
		}
		return sig
	case PS256, PS384, PS512:
		sig, err := rsa.SignPSS(rand.Reader, signer.(*rsa.PrivateKey), alg.hash(), digest, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
		if err != nil {
			t.Fatal(err)
		}
		return sig
	case ES256, ES384, ES512:
		priv := signer.(*ecdsa.PrivateKey)
		r, s, err := ecdsa.Sign(rand.Reader, priv, digest)
		if err != nil {
			t.Fatal(err)
		}
		size := (priv.Curve.Params().BitSize + 7) / 8
		out := make([]byte, 2*size)
		r.FillBytes(out[:size])
		s.FillBytes(out[size:])
		return out
	}
	t.Fatalf("cannot sign %s", alg)
	return nil
}

// encodeSegments builds header.payload from JSON-able values.
func encodeSegments(t testing.TB, header, claims any) string {
	t.Helper()
	hb, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return b64.EncodeToString(hb) + "." + b64.EncodeToString(cb)
}

// signJWT signs claims with k; extra header members are merged in.
func signJWT(t testing.TB, k *testKey, claims map[string]any, extraHeader map[string]any) string {
	t.Helper()
	h := map[string]any{"alg": string(k.alg), "typ": "JWT"}
	if k.kid != "" {
		h["kid"] = k.kid
	}
	for kk, v := range extraHeader {
		if v == nil {
			delete(h, kk)
		} else {
			h[kk] = v
		}
	}
	input := encodeSegments(t, h, claims)
	return input + "." + b64.EncodeToString(signRaw(t, k.alg, k.signer, []byte(input)))
}

// fakeClock is a manually advanced clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1_800_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeIdP is an in-process OpenID Provider issuing real signed JWTs.
type fakeIdP struct {
	t        testing.TB
	srv      *httptest.Server
	issuer   string
	clientID string

	mu          sync.Mutex
	signing     *testKey
	published   []*testKey
	jwksMaxAge  int
	jwksDelay   time.Duration
	jwksStatus  int // non-zero forces this status
	jwksHits    atomic.Int64
	jwks304     atomic.Int64
	devicePlan  []string // per poll: OAuth error code, or "" to issue
	devicePolls int
	codes       map[string]*authCode
	refresh     map[string]string // refresh token -> subject
	tokenErr    string            // forced token endpoint error
	tokenHits   atomic.Int64
	subject     string
	groups      []string
	rotateRT    bool
	omitIss     bool
	badIss      bool
}

type authCode struct {
	challenge, redirect, nonce string
	used                       bool
}

func newFakeIdP(t testing.TB) *fakeIdP {
	t.Helper()
	idp := &fakeIdP{
		t: t, clientID: "gta-cli", jwksMaxAge: 300,
		codes: map[string]*authCode{}, refresh: map[string]string{},
		subject: "user-123", groups: []string{"eng"},
	}
	idp.signing = keyFor(t, RS256)
	idp.published = []*testKey{idp.signing}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", idp.discovery)
	mux.HandleFunc("GET /jwks", idp.jwks)
	mux.HandleFunc("POST /device", idp.device)
	mux.HandleFunc("GET /authorize", idp.authorize)
	mux.HandleFunc("POST /token", idp.token)
	idp.srv = httptest.NewServer(mux)
	idp.issuer = idp.srv.URL
	t.Cleanup(idp.srv.Close)
	return idp
}

func (idp *fakeIdP) discovery(w http.ResponseWriter, _ *http.Request) {
	m := ProviderMetadata{
		Issuer:                           idp.issuer,
		AuthorizationEndpoint:            idp.issuer + "/authorize",
		TokenEndpoint:                    idp.issuer + "/token",
		DeviceAuthorizationEndpoint:      idp.issuer + "/device",
		JWKSURI:                          idp.issuer + "/jwks",
		CodeChallengeMethodsSupported:    []string{"S256"},
		IDTokenSigningAlgValuesSupported: []string{"RS256", "PS256", "ES256", "ES384", "ES512", "EdDSA", "RS384", "RS512", "PS384", "PS512"},
		GrantTypesSupported:              []string{"authorization_code", "refresh_token", deviceGrantType},
	}
	writeJSONTest(w, http.StatusOK, m)
}

func writeJSONTest(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// rotate makes a new key (alg) the signing key and publishes it alongside
// the existing ones.
func (idp *fakeIdP) rotate(k *testKey) {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	idp.signing = k
	idp.published = append(idp.published, k)
}

func (idp *fakeIdP) jwks(w http.ResponseWriter, r *http.Request) {
	idp.jwksHits.Add(1)
	idp.mu.Lock()
	delay, status, maxAge := idp.jwksDelay, idp.jwksStatus, idp.jwksMaxAge
	var set JWKSet
	ids := ""
	for _, k := range idp.published {
		set.Keys = append(set.Keys, k.jwk(idp.t))
		ids += k.kid + ","
	}
	idp.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if status != 0 {
		http.Error(w, "unavailable", status)
		return
	}
	sum := sha256.Sum256([]byte(ids))
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", maxAge))
	if r.Header.Get("If-None-Match") == etag {
		idp.jwks304.Add(1)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSONTest(w, http.StatusOK, set)
}

func (idp *fakeIdP) device(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || r.PostForm.Get("client_id") != idp.clientID {
		writeJSONTest(w, http.StatusUnauthorized, OAuthError{Code: "invalid_client"})
		return
	}
	writeJSONTest(w, http.StatusOK, map[string]any{
		"device_code":               "dev-code-1",
		"user_code":                 "WDJB-MJHT",
		"verification_uri":          idp.issuer + "/activate",
		"verification_uri_complete": idp.issuer + "/activate?user_code=WDJB-MJHT",
		"expires_in":                600,
		"interval":                  1,
	})
}

func (idp *fakeIdP) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect := q.Get("redirect_uri")
	ru, err := url.Parse(redirect)
	if err != nil || ru.Scheme != "http" || !isLoopback(ru.Hostname()) {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	if q.Get("client_id") != idp.clientID || q.Get("response_type") != "code" ||
		q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("state") == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	code, _ := randomToken(16)
	idp.mu.Lock()
	idp.codes[code] = &authCode{challenge: q.Get("code_challenge"), redirect: redirect, nonce: q.Get("nonce")}
	omitIss, badIss := idp.omitIss, idp.badIss
	idp.mu.Unlock()
	v := url.Values{"code": {code}, "state": {q.Get("state")}}
	switch {
	case badIss:
		v.Set("iss", "https://evil.example")
	case !omitIss:
		v.Set("iss", idp.issuer)
	}
	http.Redirect(w, r, redirect+"?"+v.Encode(), http.StatusFound)
}

func (idp *fakeIdP) issue(w http.ResponseWriter, sub, nonce string) {
	now := time.Now()
	claims := map[string]any{
		"iss": idp.issuer, "aud": idp.clientID, "azp": idp.clientID, "sub": sub,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "groups": idp.groups,
		"email": "dev@example.com", "name": "Dev User",
	}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	idp.mu.Lock()
	k := idp.signing
	rt, _ := randomToken(24)
	idp.refresh[rt] = sub
	idp.mu.Unlock()
	writeJSONTest(w, http.StatusOK, map[string]any{
		"access_token":  signJWT(idp.t, k, claims, nil),
		"token_type":    "Bearer",
		"expires_in":    3600,
		"refresh_token": rt,
		"id_token":      signJWT(idp.t, k, claims, nil),
		"scope":         "openid profile email offline_access",
	})
}

func (idp *fakeIdP) token(w http.ResponseWriter, r *http.Request) {
	idp.tokenHits.Add(1)
	if err := r.ParseForm(); err != nil {
		writeJSONTest(w, http.StatusBadRequest, OAuthError{Code: "invalid_request"})
		return
	}
	f := r.PostForm
	if f.Get("client_id") != idp.clientID {
		writeJSONTest(w, http.StatusUnauthorized, OAuthError{Code: "invalid_client"})
		return
	}
	idp.mu.Lock()
	forced := idp.tokenErr
	idp.mu.Unlock()
	if forced != "" {
		writeJSONTest(w, http.StatusBadRequest, OAuthError{Code: forced})
		return
	}
	switch f.Get("grant_type") {
	case deviceGrantType:
		if f.Get("device_code") != "dev-code-1" {
			writeJSONTest(w, http.StatusBadRequest, OAuthError{Code: "invalid_grant"})
			return
		}
		idp.mu.Lock()
		step := ""
		if idp.devicePolls < len(idp.devicePlan) {
			step = idp.devicePlan[idp.devicePolls]
		}
		idp.devicePolls++
		idp.mu.Unlock()
		if step != "" {
			writeJSONTest(w, http.StatusBadRequest, OAuthError{Code: step})
			return
		}
		idp.issue(w, idp.subject, "")
	case "authorization_code":
		idp.mu.Lock()
		c := idp.codes[f.Get("code")]
		ok := c != nil && !c.used && c.redirect == f.Get("redirect_uri") &&
			S256Challenge(f.Get("code_verifier")) == c.challenge
		if c != nil {
			c.used = true // single use, even on failure
		}
		idp.mu.Unlock()
		if !ok {
			writeJSONTest(w, http.StatusBadRequest, OAuthError{Code: "invalid_grant", Description: "code or verifier mismatch"})
			return
		}
		idp.issue(w, idp.subject, c.nonce)
	case "refresh_token":
		idp.mu.Lock()
		sub, ok := idp.refresh[f.Get("refresh_token")]
		if ok && idp.rotateRT {
			delete(idp.refresh, f.Get("refresh_token"))
		}
		idp.mu.Unlock()
		if !ok {
			writeJSONTest(w, http.StatusBadRequest, OAuthError{Code: "invalid_grant"})
			return
		}
		idp.issue(w, sub, "")
	default:
		writeJSONTest(w, http.StatusBadRequest, OAuthError{Code: "unsupported_grant_type"})
	}
}

// claimsNow returns standard valid claims for idp at now.
func (idp *fakeIdP) claims(now time.Time) map[string]any {
	return map[string]any{
		"iss": idp.issuer, "aud": []string{"gta-api", "other"}, "sub": "user-123",
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "nbf": now.Add(-time.Minute).Unix(),
	}
}
