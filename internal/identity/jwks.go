package identity

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// JWKSOptions configure a RemoteKeySet. Zero values select the defaults.
type JWKSOptions struct {
	// HTTPClient performs fetches (default: a client with a 15s timeout).
	HTTPClient *http.Client
	// DefaultTTL applies when the response has no usable Cache-Control
	// max-age (default 1h).
	DefaultTTL time.Duration
	// MinTTL and MaxTTL clamp the cache lifetime (defaults 1m and 24h).
	MinTTL, MaxTTL time.Duration
	// MinRefreshInterval rate-limits refreshes triggered by an unknown kid
	// and retries after a failed fetch (default 30s).
	MinRefreshInterval time.Duration
	// MaxStale is how long expired keys keep being served while the
	// endpoint is failing (default 24h). Negative disables stale serving.
	MaxStale time.Duration
	// MaxResponseBytes bounds the JWKS document (default 1 MiB).
	MaxResponseBytes int64
	// FetchTimeout bounds one fetch (default 15s).
	FetchTimeout time.Duration
	// Logger receives refresh failures and skipped keys (default: discard).
	Logger *slog.Logger
	// Now overrides the clock (tests).
	Now func() time.Time
}

func (o JWKSOptions) withDefaults() JWKSOptions {
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	if o.DefaultTTL <= 0 {
		o.DefaultTTL = time.Hour
	}
	if o.MinTTL <= 0 {
		o.MinTTL = time.Minute
	}
	if o.MaxTTL <= 0 {
		o.MaxTTL = 24 * time.Hour
	}
	if o.MaxTTL < o.MinTTL {
		o.MaxTTL = o.MinTTL
	}
	if o.MinRefreshInterval <= 0 {
		o.MinRefreshInterval = 30 * time.Second
	}
	if o.MaxStale == 0 {
		o.MaxStale = 24 * time.Hour
	}
	if o.MaxResponseBytes <= 0 {
		o.MaxResponseBytes = 1 << 20
	}
	if o.FetchTimeout <= 0 {
		o.FetchTimeout = 15 * time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// errRefreshRateLimited is returned when an unknown-kid refresh is refused
// because the previous one was too recent.
var errRefreshRateLimited = errors.New("identity: JWKS refresh rate limited")

// RemoteKeySet is a KeyResolver backed by a JWKS URL. It caches keys per
// the response's Cache-Control max-age, revalidates with ETag, refreshes in
// the background once Start is called, re-fetches (rate limited) when a
// token names an unknown kid, and collapses concurrent fetches into one.
// It is safe for concurrent use.
type RemoteKeySet struct {
	url  string
	opts JWKSOptions

	mu          sync.RWMutex
	keys        []*JWK
	etag        string
	fetchedAt   time.Time // last successful 200 or 304
	expiresAt   time.Time
	lastForced  time.Time
	lastFailAt  time.Time
	lastErr     error
	fetchCount  int
	flight      flight
	baseCtx     context.Context
	cancel      context.CancelFunc
	startOnce   sync.Once
	stoppedDone chan struct{}
}

// NewRemoteKeySet returns a key set for the JWKS at rawURL, which must be
// https (or http to a loopback address). Nothing is fetched until the first
// key lookup, Refresh or Start.
func NewRemoteKeySet(rawURL string, opts JWKSOptions) (*RemoteKeySet, error) {
	if err := validateEndpointURL(rawURL); err != nil {
		return nil, fmt.Errorf("identity: JWKS URL: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &RemoteKeySet{url: rawURL, opts: opts.withDefaults(), baseCtx: ctx, cancel: cancel}, nil
}

// URL returns the JWKS URL.
func (s *RemoteKeySet) URL() string { return s.url }

// Keys returns the currently cached keys without fetching.
func (s *RemoteKeySet) Keys() []*JWK {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]*JWK(nil), s.keys...)
}

// ResolveKeys implements KeyResolver.
func (s *RemoteKeySet) ResolveKeys(ctx context.Context, h Header) ([]*JWK, error) {
	keys, err := s.current(ctx)
	if err != nil {
		return nil, err
	}
	if c := selectKeys(keys, h); len(c) > 0 {
		return c, nil
	}
	// Unknown kid (or no compatible key): the issuer may have rotated keys.
	if err := s.refresh(ctx, refreshForced); err != nil && !errors.Is(err, errRefreshRateLimited) {
		s.opts.Logger.Warn("jwks refresh on unknown kid failed", "url", s.url, "kid", h.KeyID, "err", err)
	}
	s.mu.RLock()
	keys = s.keys
	s.mu.RUnlock()
	if c := selectKeys(keys, h); len(c) > 0 {
		return c, nil
	}
	return nil, keyNotFound(h)
}

// Refresh fetches the key set now, ignoring the cache lifetime but not the
// rate limit on forced refreshes.
func (s *RemoteKeySet) Refresh(ctx context.Context) error {
	return s.refresh(ctx, refreshForced)
}

// refreshMode says why a fetch is requested.
type refreshMode int

const (
	refreshIfExpired refreshMode = iota // a lookup found the cache empty or expired
	refreshScheduled                    // the background loop's early refresh
	refreshForced                       // unknown kid or explicit Refresh
)

// current returns fresh keys, fetching when the cache is empty or expired.
// When the endpoint fails, keys that expired less than MaxStale ago are
// served so an IdP outage does not lock everyone out immediately.
func (s *RemoteKeySet) current(ctx context.Context) ([]*JWK, error) {
	now := s.opts.Now()
	s.mu.RLock()
	keys, exp, fetched := s.keys, s.expiresAt, s.fetchedAt
	s.mu.RUnlock()
	if len(keys) > 0 && now.Before(exp) {
		return keys, nil
	}
	err := s.refresh(ctx, refreshIfExpired)
	s.mu.RLock()
	keys, fetched = s.keys, s.fetchedAt
	exp = s.expiresAt
	s.mu.RUnlock()
	if err == nil && len(keys) > 0 {
		return keys, nil
	}
	if len(keys) > 0 && s.opts.MaxStale > 0 && now.Before(exp.Add(s.opts.MaxStale)) {
		s.opts.Logger.Warn("serving stale JWKS", "url", s.url, "fetched_at", fetched, "err", err)
		return keys, nil
	}
	if err == nil {
		err = errors.New("identity: JWKS has no usable keys")
	}
	return nil, err
}

// refresh performs (or joins) a fetch. Forced refreshes are rate limited
// by MinRefreshInterval; scheduled ones back off by the same interval after
// a failure.
func (s *RemoteKeySet) refresh(ctx context.Context, mode refreshMode) error {
	return s.flight.do(ctx, func() error {
		now := s.opts.Now()
		s.mu.Lock()
		if mode == refreshForced {
			if !s.lastForced.IsZero() && now.Sub(s.lastForced) < s.opts.MinRefreshInterval {
				s.mu.Unlock()
				return errRefreshRateLimited
			}
			s.lastForced = now
		} else if s.lastErr != nil && now.Sub(s.lastFailAt) < s.opts.MinRefreshInterval {
			err := s.lastErr
			s.mu.Unlock()
			return err
		} else if mode == refreshIfExpired && len(s.keys) > 0 && now.Before(s.expiresAt) {
			// Another caller refreshed while we waited for the flight.
			s.mu.Unlock()
			return nil
		}
		etag := s.etag
		if len(s.keys) == 0 {
			etag = ""
		}
		s.mu.Unlock()

		fctx, cancel := context.WithTimeout(s.baseCtx, s.opts.FetchTimeout)
		defer cancel()
		err := s.fetch(fctx, etag)
		s.mu.Lock()
		if err != nil {
			s.lastErr, s.lastFailAt = err, s.opts.Now()
		} else {
			s.lastErr = nil
		}
		s.mu.Unlock()
		return err
	})
}

func (s *RemoteKeySet) fetch(ctx context.Context, etag string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/jwk-set+json, application/json")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := s.opts.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("identity: fetch JWKS: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, s.opts.MaxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("identity: read JWKS: %w", err)
	}
	now := s.opts.Now()
	ttl := s.ttl(resp.Header)
	s.mu.Lock()
	s.fetchCount++
	s.mu.Unlock()
	switch resp.StatusCode {
	case http.StatusNotModified:
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.keys) == 0 {
			return errors.New("identity: JWKS endpoint returned 304 without a cached copy")
		}
		s.fetchedAt, s.expiresAt = now, now.Add(ttl)
		if e := resp.Header.Get("ETag"); e != "" {
			s.etag = e
		}
		return nil
	case http.StatusOK:
	default:
		return fmt.Errorf("identity: fetch JWKS: HTTP %d", resp.StatusCode)
	}
	if int64(len(body)) > s.opts.MaxResponseBytes {
		return fmt.Errorf("identity: JWKS exceeds %d bytes", s.opts.MaxResponseBytes)
	}
	keys, skipped, err := ParseJWKS(body)
	if err != nil {
		return err
	}
	for _, e := range skipped {
		s.opts.Logger.Debug("jwks key skipped", "url", s.url, "err", e)
	}
	if len(keys) == 0 {
		// Never replace a good key set with an empty one.
		return errors.New("identity: JWKS contains no usable verification keys")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys, s.etag = keys, resp.Header.Get("ETag")
	s.fetchedAt, s.expiresAt = now, now.Add(ttl)
	return nil
}

// ttl derives the cache lifetime from Cache-Control (max-age, no-cache,
// no-store) and Age, clamped to [MinTTL, MaxTTL].
func (s *RemoteKeySet) ttl(h http.Header) time.Duration {
	ttl := s.opts.DefaultTTL
	for _, cc := range h.Values("Cache-Control") {
		for _, d := range strings.Split(cc, ",") {
			d = strings.ToLower(strings.TrimSpace(d))
			switch {
			case d == "no-store" || d == "no-cache":
				ttl = 0
			case strings.HasPrefix(d, "max-age="):
				if n, err := strconv.ParseInt(strings.Trim(d[len("max-age="):], `"`), 10, 64); err == nil && n >= 0 {
					ttl = time.Duration(min(n, int64(s.opts.MaxTTL/time.Second)+1)) * time.Second
				}
			}
		}
	}
	if age, err := strconv.ParseInt(strings.TrimSpace(h.Get("Age")), 10, 64); err == nil && age > 0 {
		ttl -= time.Duration(min(age, int64(s.opts.MaxTTL/time.Second))) * time.Second
	}
	return max(s.opts.MinTTL, min(s.opts.MaxTTL, ttl))
}

// Start launches background refresh: keys are re-fetched when 80% of their
// lifetime has passed, and retried after MinRefreshInterval on failure. It
// returns immediately; the loop ends when ctx is done or Close is called.
// Calling Start more than once has no further effect.
func (s *RemoteKeySet) Start(ctx context.Context) {
	s.startOnce.Do(func() {
		done := make(chan struct{})
		s.mu.Lock()
		s.stoppedDone = done
		s.mu.Unlock()
		go s.loop(ctx, done)
	})
}

func (s *RemoteKeySet) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.baseCtx.Done():
			return
		case <-timer.C:
		}
		s.mu.RLock()
		fetched, exp := s.fetchedAt, s.expiresAt
		s.mu.RUnlock()
		if fetched.IsZero() || !s.opts.Now().Before(fetched.Add(exp.Sub(fetched)*8/10)) {
			if err := s.refresh(ctx, refreshScheduled); err != nil && ctx.Err() == nil {
				s.opts.Logger.Warn("background JWKS refresh failed", "url", s.url, "err", err)
			}
		}
		timer.Reset(s.nextWake())
	}
}

func (s *RemoteKeySet) nextWake() time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := s.opts.Now()
	if s.lastErr != nil || s.fetchedAt.IsZero() {
		return s.opts.MinRefreshInterval
	}
	due := s.fetchedAt.Add(s.expiresAt.Sub(s.fetchedAt) * 8 / 10)
	return max(due.Sub(now), 10*time.Millisecond)
}

// Close stops background refresh and aborts in-flight fetches.
func (s *RemoteKeySet) Close() {
	s.cancel()
	s.mu.RLock()
	done := s.stoppedDone
	s.mu.RUnlock()
	if done != nil {
		<-done
	}
}

// FetchCount reports how many HTTP responses the set has processed (for
// metrics and tests).
func (s *RemoteKeySet) FetchCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.fetchCount
}

// flight collapses concurrent calls into one execution. The function runs
// on its own goroutine so a caller whose context ends can stop waiting
// without cancelling the work other callers share.
type flight struct {
	mu   sync.Mutex
	call *flightCall
}

type flightCall struct {
	done chan struct{}
	err  error
}

func (f *flight) do(ctx context.Context, fn func() error) error {
	f.mu.Lock()
	c := f.call
	if c == nil {
		c = &flightCall{done: make(chan struct{})}
		f.call = c
		go func() {
			c.err = fn()
			f.mu.Lock()
			f.call = nil
			f.mu.Unlock()
			close(c.done)
		}()
	}
	f.mu.Unlock()
	select {
	case <-c.done:
		return c.err
	case <-ctx.Done():
		return ctx.Err()
	}
}
