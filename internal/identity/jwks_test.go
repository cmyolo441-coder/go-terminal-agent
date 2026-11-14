package identity

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"
)

func newRemoteSet(t *testing.T, idp *fakeIdP, clk *fakeClock, mut func(*JWKSOptions)) *RemoteKeySet {
	t.Helper()
	o := JWKSOptions{HTTPClient: idp.srv.Client(), MinRefreshInterval: 30 * time.Second}
	if clk != nil {
		o.Now = clk.Now
	}
	if mut != nil {
		mut(&o)
	}
	s, err := NewRemoteKeySet(idp.issuer+"/jwks", o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func remoteVerifier(t *testing.T, idp *fakeIdP, s *RemoteKeySet, clk *fakeClock) *JWTVerifier {
	t.Helper()
	v, err := NewJWTVerifier(s, ValidationOptions{Issuers: []string{idp.issuer}, Audiences: []string{"gta-api"}, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestJWKSRotationAndRateLimit(t *testing.T) {
	idp := newFakeIdP(t)
	clk := newFakeClock()
	s := newRemoteSet(t, idp, clk, nil)
	v := remoteVerifier(t, idp, s, clk)
	ctx := context.Background()

	if _, err := v.Verify(ctx, signJWT(t, idp.signing, idp.claims(clk.Now()), nil)); err != nil {
		t.Fatalf("initial key: %v", err)
	}
	if got := idp.jwksHits.Load(); got != 1 {
		t.Fatalf("hits after first verify = %d", got)
	}
	// Cached: no more fetches.
	for range 5 {
		if _, err := v.Verify(ctx, signJWT(t, idp.signing, idp.claims(clk.Now()), nil)); err != nil {
			t.Fatal(err)
		}
	}
	if got := idp.jwksHits.Load(); got != 1 {
		t.Fatalf("hits with warm cache = %d", got)
	}

	// The IdP rotates to an EC key: the unknown kid triggers one refresh.
	idp.rotate(keyFor(t, ES256))
	if _, err := v.Verify(ctx, signJWT(t, idp.signing, idp.claims(clk.Now()), nil)); err != nil {
		t.Fatalf("rotated key: %v", err)
	}
	if got := idp.jwksHits.Load(); got != 2 {
		t.Fatalf("hits after rotation = %d", got)
	}

	// Garbage kids cannot make us hammer the IdP.
	bogus := signJWT(t, idp.signing, idp.claims(clk.Now()), map[string]any{"kid": "bogus"})
	for range 10 {
		if _, err := v.Verify(ctx, bogus); !errors.Is(err, ErrKeyNotFound) {
			t.Fatalf("bogus kid: %v", err)
		}
	}
	if got := idp.jwksHits.Load(); got != 2 {
		t.Fatalf("unknown kid refresh not rate limited: hits = %d", got)
	}
	clk.Advance(31 * time.Second)
	if _, err := v.Verify(ctx, bogus); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("bogus kid: %v", err)
	}
	if got := idp.jwksHits.Load(); got != 3 {
		t.Fatalf("refresh after interval: hits = %d", got)
	}
}

func TestJWKSETagAndMaxAge(t *testing.T) {
	idp := newFakeIdP(t)
	idp.jwksMaxAge = 120
	clk := newFakeClock()
	s := newRemoteSet(t, idp, clk, nil)
	v := remoteVerifier(t, idp, s, clk)
	ctx := context.Background()
	tok := func() string { return signJWT(t, idp.signing, idp.claims(clk.Now()), nil) }
	if _, err := v.Verify(ctx, tok()); err != nil {
		t.Fatal(err)
	}
	clk.Advance(119 * time.Second)
	if _, err := v.Verify(ctx, tok()); err != nil {
		t.Fatal(err)
	}
	if idp.jwksHits.Load() != 1 {
		t.Fatal("refetched before max-age elapsed")
	}
	clk.Advance(2 * time.Second)
	if _, err := v.Verify(ctx, tok()); err != nil {
		t.Fatal(err)
	}
	if idp.jwksHits.Load() != 2 || idp.jwks304.Load() != 1 {
		t.Fatalf("expected conditional revalidation: hits=%d 304s=%d", idp.jwksHits.Load(), idp.jwks304.Load())
	}
	if len(s.Keys()) != 1 {
		t.Fatal("304 lost cached keys")
	}
}

func TestJWKSTTLClamp(t *testing.T) {
	s, err := NewRemoteKeySet("https://idp.example.com/jwks", JWKSOptions{MinTTL: time.Minute, MaxTTL: time.Hour, DefaultTTL: 10 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cases := []struct {
		cc, age string
		want    time.Duration
	}{
		{"", "", 10 * time.Minute},
		{"max-age=5", "", time.Minute},
		{"public, max-age=600", "", 10 * time.Minute},
		{"max-age=999999999", "", time.Hour},
		{"no-store", "", time.Minute},
		{"max-age=600", "300", 5 * time.Minute},
		{"max-age=bogus", "", 10 * time.Minute},
	}
	for _, c := range cases {
		h := http.Header{}
		if c.cc != "" {
			h.Set("Cache-Control", c.cc)
		}
		if c.age != "" {
			h.Set("Age", c.age)
		}
		if got := s.ttl(h); got != c.want {
			t.Errorf("ttl(%q, age %q) = %s, want %s", c.cc, c.age, got, c.want)
		}
	}
}

func TestJWKSSingleflight(t *testing.T) {
	idp := newFakeIdP(t)
	idp.jwksDelay = 100 * time.Millisecond
	clk := newFakeClock()
	s := newRemoteSet(t, idp, clk, nil)
	h := Header{Algorithm: RS256, KeyID: idp.signing.kid}
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.ResolveKeys(context.Background(), h)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := idp.jwksHits.Load(); got != 1 {
		t.Fatalf("concurrent cold lookups made %d fetches, want 1", got)
	}
}

func TestJWKSCallerCancellationDoesNotAbortSharedFetch(t *testing.T) {
	idp := newFakeIdP(t)
	idp.jwksDelay = 100 * time.Millisecond
	s := newRemoteSet(t, idp, nil, nil)
	h := Header{Algorithm: RS256, KeyID: idp.signing.kid}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := s.ResolveKeys(ctx, h); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if _, err := s.ResolveKeys(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	if got := idp.jwksHits.Load(); got != 1 {
		t.Fatalf("fetches = %d, want 1 (the abandoned fetch should be reused)", got)
	}
}

func TestJWKSStaleOnOutage(t *testing.T) {
	idp := newFakeIdP(t)
	idp.jwksMaxAge = 60
	clk := newFakeClock()
	s := newRemoteSet(t, idp, clk, func(o *JWKSOptions) { o.MaxStale = time.Hour })
	v := remoteVerifier(t, idp, s, clk)
	ctx := context.Background()
	tok := func() string { return signJWT(t, idp.signing, idp.claims(clk.Now()), nil) }
	if _, err := v.Verify(ctx, tok()); err != nil {
		t.Fatal(err)
	}
	idp.mu.Lock()
	idp.jwksStatus = http.StatusServiceUnavailable
	idp.mu.Unlock()
	clk.Advance(2 * time.Minute)
	if _, err := v.Verify(ctx, tok()); err != nil {
		t.Fatalf("stale keys should be served during an outage: %v", err)
	}
	// Failure backoff: no new fetch until MinRefreshInterval passes.
	hits := idp.jwksHits.Load()
	if _, err := v.Verify(ctx, tok()); err != nil {
		t.Fatal(err)
	}
	if idp.jwksHits.Load() != hits {
		t.Fatal("failed fetch retried without backoff")
	}
	clk.Advance(2 * time.Hour)
	if _, err := v.Verify(ctx, tok()); err == nil {
		t.Fatal("keys beyond MaxStale were used")
	}
}

func TestJWKSEmptyDocumentKeepsKeys(t *testing.T) {
	idp := newFakeIdP(t)
	clk := newFakeClock()
	s := newRemoteSet(t, idp, clk, func(o *JWKSOptions) { o.MinRefreshInterval = time.Second })
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	idp.mu.Lock()
	idp.published = nil
	idp.mu.Unlock()
	clk.Advance(2 * time.Second)
	if err := s.Refresh(context.Background()); err == nil {
		t.Fatal("empty JWKS accepted")
	}
	if len(s.Keys()) != 1 {
		t.Fatal("good keys replaced by an empty set")
	}
}

func TestJWKSBackgroundRefresh(t *testing.T) {
	idp := newFakeIdP(t)
	idp.jwksMaxAge = 0
	s := newRemoteSet(t, idp, nil, func(o *JWKSOptions) {
		o.MinTTL = 50 * time.Millisecond
		o.MinRefreshInterval = 50 * time.Millisecond
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	newKey := keyFor(t, EdDSA)
	idp.rotate(newKey)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, k := range s.Keys() {
			if k.KeyID == newKey.kid {
				s.Close()
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("background refresh never picked up the rotated key")
}

func TestJWKSURLValidation(t *testing.T) {
	for _, u := range []string{"http://idp.example.com/jwks", "ftp://x/jwks", "/jwks", "https://user:pw@idp/jwks"} {
		if _, err := NewRemoteKeySet(u, JWKSOptions{}); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
	for _, u := range []string{"https://idp.example.com/jwks", "http://127.0.0.1:8080/jwks", "http://localhost/jwks", "http://[::1]:9/jwks"} {
		s, err := NewRemoteKeySet(u, JWKSOptions{})
		if err != nil {
			t.Errorf("%q rejected: %v", u, err)
			continue
		}
		s.Close()
	}
}
