package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// maxOAuthResponseBytes bounds discovery and token endpoint responses.
const maxOAuthResponseBytes = 1 << 20

// ProviderMetadata is the subset of OpenID Provider metadata (OpenID
// Connect Discovery 1.0, RFC 8414) that gta uses.
type ProviderMetadata struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint,omitempty"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	DeviceAuthorizationEndpoint       string   `json:"device_authorization_endpoint,omitempty"`
	JWKSURI                           string   `json:"jwks_uri"`
	UserinfoEndpoint                  string   `json:"userinfo_endpoint,omitempty"`
	RevocationEndpoint                string   `json:"revocation_endpoint,omitempty"`
	EndSessionEndpoint                string   `json:"end_session_endpoint,omitempty"`
	ScopesSupported                   []string `json:"scopes_supported,omitempty"`
	ResponseTypesSupported            []string `json:"response_types_supported,omitempty"`
	GrantTypesSupported               []string `json:"grant_types_supported,omitempty"`
	IDTokenSigningAlgValuesSupported  []string `json:"id_token_signing_alg_values_supported,omitempty"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported,omitempty"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported,omitempty"`
	AuthorizationResponseIssParameter bool     `json:"authorization_response_iss_parameter_supported,omitempty"`
}

// validate checks that every endpoint gta may call is a safe absolute URL.
func (m *ProviderMetadata) validate() error {
	if m.Issuer == "" {
		return errors.New("missing issuer")
	}
	if err := validateEndpointURL(m.Issuer); err != nil {
		return fmt.Errorf("issuer: %w", err)
	}
	if m.TokenEndpoint == "" {
		return errors.New("missing token_endpoint")
	}
	if m.JWKSURI == "" {
		return errors.New("missing jwks_uri")
	}
	for name, u := range map[string]string{
		"authorization_endpoint":        m.AuthorizationEndpoint,
		"token_endpoint":                m.TokenEndpoint,
		"device_authorization_endpoint": m.DeviceAuthorizationEndpoint,
		"jwks_uri":                      m.JWKSURI,
		"userinfo_endpoint":             m.UserinfoEndpoint,
		"revocation_endpoint":           m.RevocationEndpoint,
	} {
		if u == "" {
			continue
		}
		if err := validateEndpointURL(u); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

// validateEndpointURL accepts absolute https URLs, and http URLs whose host
// is a loopback address (local IdPs and tests). Userinfo and fragments are
// refused.
func validateEndpointURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Host == "" || u.Hostname() == "" {
		return fmt.Errorf("%q is not an absolute URL", raw)
	}
	if u.User != nil {
		return fmt.Errorf("%q must not contain credentials", raw)
	}
	if u.Fragment != "" {
		return fmt.Errorf("%q must not contain a fragment", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopback(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("%q must use https", raw)
	}
	return fmt.Errorf("%q: unsupported scheme %q", raw, u.Scheme)
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Discover fetches and validates the issuer's OpenID Provider metadata. The
// document's "issuer" must equal issuer exactly (OIDC Discovery §4.3),
// which defeats metadata substitution.
func Discover(ctx context.Context, issuer string, client *http.Client) (*ProviderMetadata, error) {
	if err := validateEndpointURL(issuer); err != nil {
		return nil, fmt.Errorf("identity: issuer: %w", err)
	}
	if client == nil {
		client = defaultHTTPClient()
	}
	wellKnown := strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wellKnown, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("identity: discovery: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxOAuthResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("identity: discovery: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("identity: discovery: HTTP %d from %s", resp.StatusCode, wellKnown)
	}
	if len(body) > maxOAuthResponseBytes {
		return nil, errors.New("identity: discovery document too large")
	}
	var m ProviderMetadata
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("identity: discovery: %w", err)
	}
	if m.Issuer != issuer {
		return nil, fmt.Errorf("identity: discovery: issuer %q does not match %q", m.Issuer, issuer)
	}
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("identity: discovery: %w", err)
	}
	return &m, nil
}

func defaultHTTPClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

// ClientConfig configures an OIDC Client.
type ClientConfig struct {
	// Issuer is the OpenID Provider's issuer URL; required.
	Issuer string
	// ClientID is the OAuth client identifier; required.
	ClientID string
	// ClientSecret authenticates confidential clients (HTTP Basic). CLI
	// clients are public and leave it empty, relying on PKCE.
	ClientSecret string
	// Scopes requested (default "openid", "profile", "email",
	// "offline_access"). "openid" is always included.
	Scopes []string
	// HTTPClient performs all requests (default: 30s timeout).
	HTTPClient *http.Client
	// AllowedAlgorithms narrows ID token algorithms (default: the
	// provider's advertised algorithms intersected with SupportedAlgorithms).
	AllowedAlgorithms []Algorithm
	// Leeway is the clock skew for ID token validation.
	Leeway time.Duration
	// JWKS tunes the provider's key cache.
	JWKS JWKSOptions
	// Metadata skips discovery when set (it is still validated).
	Metadata *ProviderMetadata
	// Logger receives diagnostics (default: discard).
	Logger *slog.Logger
}

// Client is an OpenID Connect relying party for terminal sign-in. It is
// safe for concurrent use.
type Client struct {
	cfg        ClientConfig
	meta       *ProviderMetadata
	keys       *RemoteKeySet
	idVerifier *JWTVerifier
	scopes     []string

	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

// NewClient discovers the provider (unless cfg.Metadata is set) and
// prepares ID token verification.
func NewClient(ctx context.Context, cfg ClientConfig) (*Client, error) {
	if cfg.ClientID == "" {
		return nil, errors.New("identity: ClientConfig.ClientID is required")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = defaultHTTPClient()
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	meta := cfg.Metadata
	if meta == nil {
		if cfg.Issuer == "" {
			return nil, errors.New("identity: ClientConfig.Issuer is required")
		}
		var err error
		if meta, err = Discover(ctx, cfg.Issuer, cfg.HTTPClient); err != nil {
			return nil, err
		}
	} else {
		m := *meta
		meta = &m
		if cfg.Issuer != "" && meta.Issuer != cfg.Issuer {
			return nil, fmt.Errorf("identity: metadata issuer %q does not match %q", meta.Issuer, cfg.Issuer)
		}
		if err := meta.validate(); err != nil {
			return nil, fmt.Errorf("identity: metadata: %w", err)
		}
	}
	jo := cfg.JWKS
	if jo.HTTPClient == nil {
		jo.HTTPClient = cfg.HTTPClient
	}
	if jo.Logger == nil {
		jo.Logger = cfg.Logger
	}
	keys, err := NewRemoteKeySet(meta.JWKSURI, jo)
	if err != nil {
		return nil, err
	}
	algs := cfg.AllowedAlgorithms
	if len(algs) == 0 && len(meta.IDTokenSigningAlgValuesSupported) > 0 {
		for _, a := range meta.IDTokenSigningAlgValuesSupported {
			if Algorithm(a).supported() {
				algs = append(algs, Algorithm(a))
			}
		}
		if len(algs) == 0 {
			return nil, fmt.Errorf("identity: provider signs ID tokens only with unsupported algorithms %q", meta.IDTokenSigningAlgValuesSupported)
		}
	}
	idv, err := NewJWTVerifier(keys, ValidationOptions{
		Issuers:           []string{meta.Issuer},
		Audiences:         []string{cfg.ClientID},
		AllowedAlgorithms: algs,
		Leeway:            cfg.Leeway,
		AuthorizedParty:   cfg.ClientID,
		RequiredClaims:    []string{"sub", "iat"},
	})
	if err != nil {
		return nil, err
	}
	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = []string{"openid", "profile", "email", "offline_access"}
	}
	if !slices.Contains(scopes, "openid") {
		scopes = append([]string{"openid"}, scopes...)
	}
	return &Client{
		cfg: cfg, meta: meta, keys: keys, idVerifier: idv, scopes: slices.Clone(scopes),
		now: time.Now, sleep: sleepCtx,
	}, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Metadata returns a copy of the provider metadata.
func (c *Client) Metadata() ProviderMetadata { return *c.meta }

// KeySet returns the provider's JWKS cache, which can also back a
// JWTVerifier for access tokens (see NewAccessTokenVerifier).
func (c *Client) KeySet() *RemoteKeySet { return c.keys }

// Close stops the key set's background refresh.
func (c *Client) Close() { c.keys.Close() }

// VerifyIDToken verifies an ID token issued to this client. When nonce is
// non-empty the token's "nonce" must equal it.
func (c *Client) VerifyIDToken(ctx context.Context, raw, nonce string) (*Claims, error) {
	claims, err := c.idVerifier.Verify(ctx, raw)
	if err != nil {
		return nil, err
	}
	if len(claims.Audience) > 1 && claims.AuthorizedParty == "" {
		return nil, fmt.Errorf("%w: azp required when the ID token has several audiences", ErrInvalidAuthorizedParty)
	}
	if nonce != "" && !constantTimeEqual(claims.Nonce, nonce) {
		return nil, ErrInvalidNonce
	}
	return claims, nil
}

// NewAccessTokenVerifier returns a JWT verifier for access tokens from this
// provider (RFC 9068 style) accepted by a resource server whose audience is
// audience.
func (c *Client) NewAccessTokenVerifier(audience string, opts ValidationOptions) (*JWTVerifier, error) {
	opts.Issuers = []string{c.meta.Issuer}
	opts.Audiences = []string{audience}
	return NewJWTVerifier(c.keys, opts)
}

// OAuthError is an error response from an OAuth endpoint (RFC 6749 §5.2).
type OAuthError struct {
	StatusCode  int    `json:"-"`
	Code        string `json:"error"`
	Description string `json:"error_description,omitempty"`
	URI         string `json:"error_uri,omitempty"`
}

func (e *OAuthError) Error() string {
	if e.Description != "" {
		return fmt.Sprintf("oauth error %s: %s", e.Code, e.Description)
	}
	return "oauth error " + e.Code
}

// HTTPError is an unexpected, non-OAuth HTTP failure (for example a 503 from
// a proxy). It is treated as transient by polling loops.
type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("identity: unexpected HTTP %d: %s", e.StatusCode, e.Body)
}

// Token is an OAuth 2.0 token set. Its String method redacts secrets so a
// token accidentally passed to a logger does not leak.
type Token struct {
	AccessToken  string    `json:"access_token"`
	TokenType    string    `json:"token_type"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	IDToken      string    `json:"id_token,omitempty"`
	Scope        string    `json:"scope,omitempty"`
	Expiry       time.Time `json:"expiry,omitzero"`
	Issuer       string    `json:"issuer,omitempty"`
	Subject      string    `json:"subject,omitempty"`
}

// Expired reports whether the access token expires within skew of now. A
// token without an expiry never reports expired.
func (t *Token) Expired(now time.Time, skew time.Duration) bool {
	if t == nil || t.AccessToken == "" {
		return true
	}
	return !t.Expiry.IsZero() && !now.Add(skew).Before(t.Expiry)
}

// String implements fmt.Stringer without revealing secrets.
func (t Token) String() string {
	return fmt.Sprintf("Token{type=%s subject=%q expiry=%s refresh=%t id_token=%t}",
		t.TokenType, t.Subject, t.Expiry.Format(time.RFC3339), t.RefreshToken != "", t.IDToken != "")
}

// GoString implements fmt.GoStringer without revealing secrets.
func (t Token) GoString() string { return t.String() }

// LogValue implements slog.LogValuer without revealing secrets.
func (t Token) LogValue() slog.Value { return slog.StringValue(t.String()) }

type tokenResponse struct {
	AccessToken  string          `json:"access_token"`
	TokenType    string          `json:"token_type"`
	RefreshToken string          `json:"refresh_token"`
	IDToken      string          `json:"id_token"`
	Scope        string          `json:"scope"`
	ExpiresIn    json.RawMessage `json:"expires_in"`
}

// postForm sends an application/x-www-form-urlencoded request to endpoint
// with client authentication and decodes the JSON body into out. OAuth
// error bodies become *OAuthError; other failures become *HTTPError.
func (c *Client) postForm(ctx context.Context, endpoint string, form url.Values, out any) error {
	if c.cfg.ClientSecret == "" {
		form.Set("client_id", c.cfg.ClientID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if c.cfg.ClientSecret != "" {
		// RFC 6749 §2.3.1: form-encode both parts before Basic encoding.
		req.SetBasicAuth(url.QueryEscape(c.cfg.ClientID), url.QueryEscape(c.cfg.ClientSecret))
	}
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxOAuthResponseBytes+1))
	if err != nil {
		return err
	}
	if len(body) > maxOAuthResponseBytes {
		return &HTTPError{StatusCode: resp.StatusCode, Body: "response too large"}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("identity: decode response from %s: %w", endpoint, err)
		}
		return nil
	}
	var oe OAuthError
	if json.Unmarshal(body, &oe) == nil && oe.Code != "" {
		oe.StatusCode = resp.StatusCode
		return &oe
	}
	return &HTTPError{StatusCode: resp.StatusCode, Body: truncate(string(body), 200)}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// tokenRequest calls the token endpoint and converts the response. When the
// response carries an ID token it is verified (with nonce when non-empty).
func (c *Client) tokenRequest(ctx context.Context, form url.Values, nonce string) (*Token, error) {
	var tr tokenResponse
	if err := c.postForm(ctx, c.meta.TokenEndpoint, form, &tr); err != nil {
		return nil, err
	}
	if tr.AccessToken == "" {
		return nil, errors.New("identity: token response without access_token")
	}
	if !strings.EqualFold(tr.TokenType, "Bearer") {
		return nil, fmt.Errorf("identity: unsupported token_type %q", tr.TokenType)
	}
	t := &Token{
		AccessToken: tr.AccessToken, TokenType: "Bearer", RefreshToken: tr.RefreshToken,
		IDToken: tr.IDToken, Scope: tr.Scope, Issuer: c.meta.Issuer,
	}
	if secs, ok := parseExpiresIn(tr.ExpiresIn); ok && secs > 0 {
		t.Expiry = c.now().Add(time.Duration(secs) * time.Second)
	}
	if t.IDToken != "" {
		claims, err := c.VerifyIDToken(ctx, t.IDToken, nonce)
		if err != nil {
			return nil, fmt.Errorf("identity: ID token: %w", err)
		}
		t.Subject = claims.Subject
	}
	return t, nil
}

// parseExpiresIn accepts a JSON number or a numeric string (some providers
// send the latter).
func parseExpiresIn(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	s := string(raw)
	if unq, err := strconv.Unquote(s); err == nil {
		s = unq
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 || f > 10*365*24*3600 {
		return 0, false
	}
	return int64(f), true
}

// Refresh exchanges t's refresh token for a new token. A provider that does
// not rotate refresh tokens omits one; the old refresh token is kept.
func (c *Client) Refresh(ctx context.Context, t *Token) (*Token, error) {
	if t == nil || t.RefreshToken == "" {
		return nil, errors.New("identity: no refresh token")
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {t.RefreshToken}}
	nt, err := c.tokenRequest(ctx, form, "")
	if err != nil {
		return nil, err
	}
	if nt.RefreshToken == "" {
		nt.RefreshToken = t.RefreshToken
	}
	if nt.IDToken == "" {
		nt.IDToken, nt.Subject = t.IDToken, t.Subject
	} else if t.Subject != "" && nt.Subject != t.Subject {
		// OIDC Core §12.2: the refreshed ID token must be for the same user.
		return nil, fmt.Errorf("identity: refreshed ID token subject %q differs from %q", nt.Subject, t.Subject)
	}
	return nt, nil
}
