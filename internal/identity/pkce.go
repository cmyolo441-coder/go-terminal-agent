package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// ErrAuthCodeUnsupported is returned when the provider has no
// authorization endpoint or does not support PKCE S256.
var ErrAuthCodeUnsupported = errors.New("identity: provider does not support authorization code with PKCE S256")

// PKCE holds a code verifier and its S256 challenge (RFC 7636).
type PKCE struct {
	Verifier  string
	Challenge string
	Method    string
}

// NewPKCE returns a fresh verifier (32 random bytes, 43 characters) and its
// S256 challenge.
func NewPKCE() (PKCE, error) {
	v, err := randomToken(32)
	if err != nil {
		return PKCE{}, err
	}
	return PKCE{Verifier: v, Challenge: S256Challenge(v), Method: "S256"}, nil
}

// S256Challenge returns BASE64URL(SHA256(verifier)).
func S256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return b64.EncodeToString(sum[:])
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("identity: random: %w", err)
	}
	return b64.EncodeToString(b), nil
}

// AuthCodeOptions configure AuthCodeLogin.
type AuthCodeOptions struct {
	// Scopes override the client's default scopes.
	Scopes []string
	// ListenAddr is the loopback address for the redirect listener
	// (default "127.0.0.1:0", an ephemeral port). It must be a loopback IP
	// literal (RFC 8252 §7.3, §8.3).
	ListenAddr string
	// RedirectPath is the callback path (default "/callback").
	RedirectPath string
	// OpenBrowser is given the authorization URL; it should open the
	// user's browser and return promptly. Required.
	OpenBrowser func(authURL string) error
	// Timeout bounds the wait for the browser callback (default 5m).
	Timeout time.Duration
	// ExtraParams are added to the authorization request (for example
	// "prompt", "login_hint", "acr_values"). Protocol parameters set by
	// this package cannot be overridden.
	ExtraParams map[string]string
}

// AuthCodeRequest is a prepared authorization request.
type AuthCodeRequest struct {
	URL         string
	State       string
	Nonce       string
	PKCE        PKCE
	RedirectURI string
}

// NewAuthCodeRequest builds an authorization URL with fresh state, nonce
// and PKCE S256 parameters for redirectURI.
func (c *Client) NewAuthCodeRequest(redirectURI string, scopes []string, extra map[string]string) (*AuthCodeRequest, error) {
	if c.meta.AuthorizationEndpoint == "" {
		return nil, ErrAuthCodeUnsupported
	}
	if m := c.meta.CodeChallengeMethodsSupported; len(m) > 0 && !slices.Contains(m, "S256") {
		return nil, ErrAuthCodeUnsupported
	}
	pk, err := NewPKCE()
	if err != nil {
		return nil, err
	}
	state, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	nonce, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	if len(scopes) == 0 {
		scopes = c.scopes
	}
	u, err := url.Parse(c.meta.AuthorizationEndpoint)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	for k, v := range extra {
		q.Set(k, v)
	}
	q.Set("response_type", "code")
	q.Set("client_id", c.cfg.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", strings.Join(scopes, " "))
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", pk.Challenge)
	q.Set("code_challenge_method", pk.Method)
	u.RawQuery = q.Encode()
	return &AuthCodeRequest{URL: u.String(), State: state, Nonce: nonce, PKCE: pk, RedirectURI: redirectURI}, nil
}

// ExchangeCode redeems an authorization code with its PKCE verifier and
// verifies the returned ID token against nonce.
func (c *Client) ExchangeCode(ctx context.Context, code, verifier, redirectURI, nonce string) (*Token, error) {
	if code == "" || verifier == "" {
		return nil, errors.New("identity: ExchangeCode: code and verifier are required")
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	}
	t, err := c.tokenRequest(ctx, form, nonce)
	if err != nil {
		return nil, err
	}
	if nonce != "" && t.IDToken == "" {
		return nil, errors.New("identity: provider returned no ID token for an openid request")
	}
	return t, nil
}

type callbackResult struct {
	code string
	err  error
}

// AuthCodeLogin runs the Authorization Code + PKCE flow with a loopback
// redirect listener: it listens on a loopback port, opens the browser at
// the authorization URL, waits for the redirect, checks state (and the
// RFC 9207 "iss" parameter when present), and exchanges the code.
func (c *Client) AuthCodeLogin(ctx context.Context, opts AuthCodeOptions) (*Token, error) {
	if opts.OpenBrowser == nil {
		return nil, errors.New("identity: AuthCodeOptions.OpenBrowser is required")
	}
	addr := opts.ListenAddr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("identity: listen address: %w", err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("identity: listen address %q must be a loopback IP literal", addr)
	}
	path := opts.RedirectPath
	if path == "" {
		path = "/callback"
	}
	if !strings.HasPrefix(path, "/") {
		return nil, errors.New("identity: RedirectPath must start with /")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("identity: redirect listener: %w", err)
	}
	redirectURI := (&url.URL{Scheme: "http", Host: ln.Addr().String(), Path: path}).String()
	req, err := c.NewAuthCodeRequest(redirectURI, opts.Scopes, opts.ExtraParams)
	if err != nil {
		ln.Close()
		return nil, err
	}

	results := make(chan callbackResult, 1)
	var once sync.Once
	deliver := func(r callbackResult) { once.Do(func() { results <- r }) }
	srv := &http.Server{
		Handler:           c.callbackHandler(path, req.State, deliver),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer scancel()
		_ = srv.Shutdown(sctx)
	}()

	if err := opts.OpenBrowser(req.URL); err != nil {
		return nil, fmt.Errorf("identity: open browser: %w", err)
	}
	var res callbackResult
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("identity: waiting for browser sign-in: %w", ctx.Err())
	case res = <-results:
	}
	if res.err != nil {
		return nil, res.err
	}
	return c.ExchangeCode(ctx, res.code, req.PKCE.Verifier, redirectURI, req.Nonce)
}

// callbackHandler serves the redirect. Requests with a wrong state are
// answered with 400 and ignored (they may be forged by another local
// process or web page), so the genuine redirect can still arrive.
func (c *Client) callbackHandler(path, state string, deliver func(callbackResult)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'")
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		q := r.URL.Query()
		if !constantTimeEqual(q.Get("state"), state) {
			http.Error(w, "invalid state", http.StatusBadRequest)
			return
		}
		if iss := q.Get("iss"); iss != "" && iss != c.meta.Issuer {
			deliver(callbackResult{err: fmt.Errorf("identity: authorization response from issuer %q, expected %q (mix-up attack?)", iss, c.meta.Issuer)})
			writeCallbackPage(w, http.StatusBadRequest, "Sign-in failed", "The response came from an unexpected issuer.")
			return
		} else if iss == "" && c.meta.AuthorizationResponseIssParameter {
			deliver(callbackResult{err: errors.New("identity: authorization response lacks the iss parameter the provider advertises")})
			writeCallbackPage(w, http.StatusBadRequest, "Sign-in failed", "The response is missing the issuer.")
			return
		}
		if e := q.Get("error"); e != "" {
			oe := &OAuthError{Code: e, Description: q.Get("error_description"), URI: q.Get("error_uri")}
			var err error = oe
			if e == "access_denied" {
				err = fmt.Errorf("%w: %s", ErrAccessDenied, oe.Description)
			}
			deliver(callbackResult{err: err})
			writeCallbackPage(w, http.StatusOK, "Sign-in failed", "The identity provider reported: "+e+". You can close this window.")
			return
		}
		code := q.Get("code")
		if code == "" {
			deliver(callbackResult{err: errors.New("identity: authorization response without code")})
			writeCallbackPage(w, http.StatusBadRequest, "Sign-in failed", "No authorization code was received.")
			return
		}
		deliver(callbackResult{code: code})
		writeCallbackPage(w, http.StatusOK, "Signed in", "You are signed in to gta. You can close this window and return to the terminal.")
	})
}

func writeCallbackPage(w http.ResponseWriter, status int, title, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<!doctype html><meta charset=utf-8><title>%s</title><h1>%s</h1><p>%s</p>",
		html.EscapeString(title), html.EscapeString(title), html.EscapeString(msg))
}
