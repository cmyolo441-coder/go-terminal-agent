package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"
)

// TokenVerifier verifies a bearer JWT. *JWTVerifier implements it.
type TokenVerifier interface {
	Verify(ctx context.Context, raw string) (*Claims, error)
}

// RequestAttributes are the attributes Require passes to the Authorizer.
type RequestAttributes struct {
	Workspace   string
	Environment string
	Attributes  map[string]string
}

// AuthenticatorOptions configure an Authenticator. At least one of
// Tokens and APIKeys is required.
type AuthenticatorOptions struct {
	// Tokens verifies Bearer JWTs.
	Tokens TokenVerifier
	// APIKeys verifies API keys, sent as "Authorization: Bearer gta_..." or
	// in the X-API-Key header.
	APIKeys *APIKeyVerifier
	// Authorizer maps claims to roles and decides Require checks.
	Authorizer Authorizer
	// PrincipalFromClaims overrides how a verified token becomes a
	// Principal. The default uses sub, iss, name/preferred_username, email,
	// groups, and roles from Authorizer.RolesFromClaims.
	PrincipalFromClaims func(*Claims) (*Principal, error)
	// GroupsClaim is the claim path read for Principal.Groups (default
	// "groups").
	GroupsClaim string
	// Attributes supplies request attributes for Require (default: only
	// Environment below).
	Attributes func(*http.Request) RequestAttributes
	// Environment is the default deployment environment attribute.
	Environment string
	// Realm is used in WWW-Authenticate (default "gta").
	Realm string
	// ExposeDecisionReason includes the decision reason in 403 bodies.
	// Off by default so policy internals are not revealed to callers.
	ExposeDecisionReason bool
	// OnAuthenticated is called after successful authentication (for audit).
	OnAuthenticated func(*http.Request, *Principal)
	// OnAuthFailure is called when authentication fails (for audit).
	OnAuthFailure func(*http.Request, error)
	// OnDecision is called with every Require decision (for audit).
	OnDecision func(*http.Request, *Principal, string, Decision)
	// Logger receives authentication failures at debug level.
	Logger *slog.Logger
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Authenticator is HTTP middleware that authenticates requests and enforces
// permissions.
type Authenticator struct {
	o AuthenticatorOptions
}

// NewAuthenticator validates opts and returns an Authenticator.
func NewAuthenticator(opts AuthenticatorOptions) (*Authenticator, error) {
	if opts.Tokens == nil && opts.APIKeys == nil {
		return nil, errors.New("identity: Authenticator needs a token verifier or an API key verifier")
	}
	if opts.Realm == "" {
		opts.Realm = "gta"
	}
	if strings.ContainsAny(opts.Realm, "\"\\\r\n") {
		return nil, errors.New("identity: invalid realm")
	}
	if opts.GroupsClaim == "" {
		opts.GroupsClaim = "groups"
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Authenticator{o: opts}, nil
}

// errAuth carries a client-safe description and the RFC 6750 error code.
type errAuth struct {
	code   string // "invalid_request" or "invalid_token"
	status int
	msg    string
	cause  error
}

func (e *errAuth) Error() string {
	if e.cause != nil {
		return e.msg + ": " + e.cause.Error()
	}
	return e.msg
}

func (e *errAuth) Unwrap() error { return e.cause }

var errNoCredentials = errors.New("identity: no credentials")

// credential extracts the presented credential. Presenting both an
// Authorization header and X-API-Key is refused as ambiguous.
func credential(r *http.Request) (string, error) {
	authz := r.Header.Values("Authorization")
	apiKey := r.Header.Values("X-API-Key")
	if len(authz) > 1 || len(apiKey) > 1 {
		return "", &errAuth{code: "invalid_request", status: http.StatusBadRequest, msg: "multiple credentials"}
	}
	if len(authz) == 1 && len(apiKey) == 1 {
		return "", &errAuth{code: "invalid_request", status: http.StatusBadRequest, msg: "both Authorization and X-API-Key present"}
	}
	if len(apiKey) == 1 {
		k := strings.TrimSpace(apiKey[0])
		if k == "" {
			return "", &errAuth{code: "invalid_request", status: http.StatusBadRequest, msg: "empty X-API-Key"}
		}
		return k, nil
	}
	if len(authz) == 0 {
		return "", errNoCredentials
	}
	scheme, tok, ok := strings.Cut(strings.TrimSpace(authz[0]), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(tok) == "" {
		return "", &errAuth{code: "invalid_request", status: http.StatusBadRequest, msg: "Authorization must use the Bearer scheme"}
	}
	return strings.TrimSpace(tok), nil
}

// authenticate resolves the request's principal.
func (a *Authenticator) authenticate(r *http.Request) (*Principal, error) {
	cred, err := credential(r)
	if err != nil {
		return nil, err
	}
	if LooksLikeAPIKey(cred) {
		if a.o.APIKeys == nil {
			return nil, &errAuth{code: "invalid_token", status: http.StatusUnauthorized, msg: "API keys are not accepted"}
		}
		p, err := a.o.APIKeys.Verify(r.Context(), cred)
		if err != nil {
			return nil, &errAuth{code: "invalid_token", status: http.StatusUnauthorized, msg: "invalid API key", cause: err}
		}
		return p, nil
	}
	if a.o.Tokens == nil {
		return nil, &errAuth{code: "invalid_token", status: http.StatusUnauthorized, msg: "bearer tokens are not accepted"}
	}
	claims, err := a.o.Tokens.Verify(r.Context(), cred)
	if err != nil {
		msg := "invalid token"
		if errors.Is(err, ErrTokenExpired) {
			msg = "token expired"
		}
		return nil, &errAuth{code: "invalid_token", status: http.StatusUnauthorized, msg: msg, cause: err}
	}
	var p *Principal
	if a.o.PrincipalFromClaims != nil {
		p, err = a.o.PrincipalFromClaims(claims)
	} else {
		p, err = a.principalFromClaims(claims)
	}
	if err != nil {
		return nil, &errAuth{code: "invalid_token", status: http.StatusUnauthorized, msg: "token not acceptable", cause: err}
	}
	return p, nil
}

func (a *Authenticator) principalFromClaims(c *Claims) (*Principal, error) {
	if c.Subject == "" {
		return nil, fmt.Errorf("%w: sub", ErrMissingClaim)
	}
	p := &Principal{
		Type:         PrincipalUser,
		Subject:      c.Subject,
		Issuer:       c.Issuer,
		Email:        c.String("email"),
		Name:         c.String("name"),
		Groups:       c.Strings(a.o.GroupsClaim, false),
		Claims:       c.Raw,
		AuthMethod:   "jwt",
		CredentialID: c.ID,
	}
	if p.Name == "" {
		p.Name = c.String("preferred_username")
	}
	if c.ExpiresAt != nil {
		p.ExpiresAt = c.ExpiresAt.Time
	}
	if a.o.Authorizer != nil {
		p.Roles = a.o.Authorizer.RolesFromClaims(c.Raw)
	}
	return p, nil
}

// Authenticate returns middleware that authenticates the request with a
// Bearer JWT or API key and stores the Principal in the request context
// (see PrincipalFrom). Requests without valid credentials get 401 with an
// RFC 6750 WWW-Authenticate challenge; malformed credentials get 400.
func (a *Authenticator) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := a.authenticate(r)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		if a.o.OnAuthenticated != nil {
			a.o.OnAuthenticated(r, p)
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

func (a *Authenticator) fail(w http.ResponseWriter, r *http.Request, err error) {
	if a.o.OnAuthFailure != nil {
		a.o.OnAuthFailure(r, err)
	}
	a.o.Logger.Debug("authentication failed", "path", r.URL.Path, "err", err)
	var ea *errAuth
	if errors.Is(err, errNoCredentials) || !errors.As(err, &ea) {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q`, a.o.Realm))
		writeAuthError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q, error=%q, error_description=%q`, a.o.Realm, ea.code, ea.msg))
	code := "unauthorized"
	if ea.status == http.StatusBadRequest {
		code = "bad_request"
	}
	writeAuthError(w, ea.status, code, ea.msg)
}

// decide evaluates perm for p with the request's attributes.
func (a *Authenticator) decide(r *http.Request, p *Principal, perm string) Decision {
	if a.o.Authorizer == nil {
		return Decision{Reason: "no authorizer configured"}
	}
	attrs := RequestAttributes{Environment: a.o.Environment}
	if a.o.Attributes != nil {
		attrs = a.o.Attributes(r)
		if attrs.Environment == "" {
			attrs.Environment = a.o.Environment
		}
	}
	return a.o.Authorizer.Authorize(Request{
		Principal: p, Permission: perm, Workspace: attrs.Workspace,
		Environment: attrs.Environment, Attributes: attrs.Attributes, Time: a.o.Now(),
	})
}

// Require returns middleware that allows the request only when the
// context principal (set by Authenticate) holds perm according to the
// Authorizer. Without a principal it answers 401; on denial, 403. It
// panics if perm is not a valid concrete permission, which is a
// programming error caught at route registration.
func (a *Authenticator) Require(perm string) func(http.Handler) http.Handler {
	return a.RequireAny(perm)
}

// RequireAny is like Require but allows the request when the principal
// holds any of perms (checked in order).
func (a *Authenticator) RequireAny(perms ...string) func(http.Handler) http.Handler {
	if len(perms) == 0 {
		panic("identity: RequireAny: no permissions")
	}
	for _, perm := range perms {
		if _, err := parsePermission(perm); err != nil {
			panic("identity: Require: " + err.Error())
		}
	}
	perms = slices.Clone(perms)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := PrincipalFrom(r.Context())
			if !ok {
				a.fail(w, r, errNoCredentials)
				return
			}
			var first Decision
			for i, perm := range perms {
				d := a.decide(r, p, perm)
				if a.o.OnDecision != nil {
					a.o.OnDecision(r, p, perm, d)
				}
				if d.Allowed {
					next.ServeHTTP(w, r)
					return
				}
				if i == 0 {
					first = d
				}
			}
			msg := "permission " + strings.Join(perms, " or ") + " denied"
			if a.o.ExposeDecisionReason {
				msg = first.Reason
			}
			writeAuthError(w, http.StatusForbidden, "forbidden", msg)
		})
	}
}

func writeAuthError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]map[string]string{"error": {"code": code, "message": msg}})
}
