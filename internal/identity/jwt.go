package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Claim validation errors. They are wrapped with detail; test with errors.Is.
var (
	ErrTokenExpired           = errors.New("identity: token is expired")
	ErrTokenNotYetValid       = errors.New("identity: token is not valid yet")
	ErrTokenIssuedInFuture    = errors.New("identity: token issued in the future")
	ErrTokenTooOld            = errors.New("identity: token exceeds maximum age")
	ErrInvalidIssuer          = errors.New("identity: invalid issuer")
	ErrInvalidAudience        = errors.New("identity: invalid audience")
	ErrMissingClaim           = errors.New("identity: required claim missing")
	ErrInvalidAuthorizedParty = errors.New("identity: invalid authorized party")
	ErrInvalidTokenType       = errors.New("identity: invalid token type")
	ErrInvalidNonce           = errors.New("identity: invalid nonce")
)

const (
	// DefaultLeeway is the clock skew tolerated when none is configured.
	DefaultLeeway = 60 * time.Second
	// MaxLeeway is the largest clock skew a verifier may be configured with.
	MaxLeeway = 5 * time.Minute
	// maxNumericDate rejects absurd timestamps (year > 5000).
	maxNumericDate = 95617584000
)

// NumericDate is a JWT NumericDate: seconds since the epoch, possibly
// fractional.
type NumericDate struct {
	time.Time
}

// UnmarshalJSON implements json.Unmarshaler.
func (d *NumericDate) UnmarshalJSON(b []byte) error {
	f, err := strconv.ParseFloat(string(bytes.TrimSpace(b)), 64)
	if err != nil {
		return fmt.Errorf("NumericDate: %s is not a number", b)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > maxNumericDate {
		return fmt.Errorf("NumericDate: %s out of range", b)
	}
	sec, frac := math.Modf(f)
	d.Time = time.Unix(int64(sec), int64(frac*1e9)).UTC()
	return nil
}

// MarshalJSON implements json.Marshaler.
func (d NumericDate) MarshalJSON() ([]byte, error) {
	return strconv.AppendInt(nil, d.Unix(), 10), nil
}

// Audience is the "aud" claim, which may be a string or an array.
type Audience []string

// UnmarshalJSON implements json.Unmarshaler.
func (a *Audience) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*a = Audience{s}
		return nil
	}
	var l []string
	if err := json.Unmarshal(b, &l); err != nil {
		return errors.New("aud must be a string or an array of strings")
	}
	*a = l
	return nil
}

// Contains reports whether aud includes s.
func (a Audience) Contains(s string) bool { return slices.Contains(a, s) }

// Claims are the claims of a verified JWT: the registered claims decoded,
// and every claim available in Raw (numbers as json.Number).
type Claims struct {
	Issuer          string       `json:"iss,omitempty"`
	Subject         string       `json:"sub,omitempty"`
	Audience        Audience     `json:"aud,omitempty"`
	ExpiresAt       *NumericDate `json:"exp,omitempty"`
	NotBefore       *NumericDate `json:"nbf,omitempty"`
	IssuedAt        *NumericDate `json:"iat,omitempty"`
	ID              string       `json:"jti,omitempty"`
	AuthorizedParty string       `json:"azp,omitempty"`
	Nonce           string       `json:"nonce,omitempty"`

	Raw map[string]any `json:"-"`
}

// ParseClaims decodes a JWT payload. It rejects payloads that are not a
// single JSON object, have duplicate claim names, or carry registered
// claims of the wrong type.
func ParseClaims(payload []byte) (*Claims, error) {
	var c Claims
	if err := decodeStrictObject(payload, &c); err != nil {
		return nil, fmt.Errorf("%w: claims: %v", ErrMalformedToken, err)
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	if err := dec.Decode(&c.Raw); err != nil {
		return nil, fmt.Errorf("%w: claims: %v", ErrMalformedToken, err)
	}
	return &c, nil
}

// Lookup returns the claim at path; see LookupClaim.
func (c *Claims) Lookup(path string) (any, bool) {
	if c == nil {
		return nil, false
	}
	return LookupClaim(c.Raw, path)
}

// Strings returns the claim at path as a list of strings: a string claim is
// one element (or, for "scope"/"scp" style claims, split on spaces when
// splitSpaces is true) and an array contributes its string elements.
func (c *Claims) Strings(path string, splitSpaces bool) []string {
	v, ok := c.Lookup(path)
	if !ok {
		return nil
	}
	return claimStrings(v, splitSpaces)
}

// String returns a string claim at path.
func (c *Claims) String(path string) string {
	v, _ := c.Lookup(path)
	s, _ := v.(string)
	return s
}

func claimStrings(v any, splitSpaces bool) []string {
	switch t := v.(type) {
	case string:
		if splitSpaces {
			return strings.Fields(t)
		}
		if t == "" {
			return nil
		}
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return slices.Clone(t)
	}
	return nil
}

// LookupClaim resolves a claim path in claims. A path starting with "/" is
// a JSON Pointer (RFC 6901), which handles claim names containing dots
// (for example "/https:~1~1example.com~1roles"). Otherwise the whole path
// is first tried as a top-level claim name (so "https://example.com/roles"
// works) and then as a dot-separated path ("realm_access.roles").
func LookupClaim(claims map[string]any, path string) (any, bool) {
	if claims == nil || path == "" {
		return nil, false
	}
	if strings.HasPrefix(path, "/") {
		var cur any = claims
		for _, tok := range strings.Split(path[1:], "/") {
			tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
			m, ok := cur.(map[string]any)
			if !ok {
				return nil, false
			}
			if cur, ok = m[tok]; !ok {
				return nil, false
			}
		}
		return cur, true
	}
	if v, ok := claims[path]; ok {
		return v, true
	}
	var cur any = claims
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[part]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// ValidationOptions configure JWTVerifier.
type ValidationOptions struct {
	// Issuers lists accepted "iss" values; required.
	Issuers []string
	// Audiences lists accepted audiences; the token's "aud" must contain at
	// least one. Required unless SkipAudienceCheck is set.
	Audiences []string
	// SkipAudienceCheck disables the audience check. Only for tokens whose
	// audience is enforced elsewhere; never for ID tokens.
	SkipAudienceCheck bool
	// AllowedAlgorithms narrows the accepted algorithms (default: all
	// supported asymmetric algorithms).
	AllowedAlgorithms []Algorithm
	// Leeway is the tolerated clock skew (default DefaultLeeway, at most
	// MaxLeeway). Use a negative value for zero leeway.
	Leeway time.Duration
	// MaxTokenAge, when positive, requires "iat" and rejects tokens issued
	// longer ago than this.
	MaxTokenAge time.Duration
	// AllowMissingExpiration accepts tokens without "exp". Off by default:
	// a bearer token that never expires is almost always a mistake.
	AllowMissingExpiration bool
	// RequiredClaims must be present and non-null (claim paths as in
	// LookupClaim).
	RequiredClaims []string
	// ExpectedTypes, when set, requires the header "typ" to match one of
	// them, case-insensitively and ignoring an "application/" prefix (for
	// example "at+jwt" per RFC 9068).
	ExpectedTypes []string
	// AuthorizedParty, when set, must equal "azp" whenever "azp" is present.
	AuthorizedParty string
	// Now overrides the clock (tests).
	Now func() time.Time
}

// JWTVerifier verifies signed JWTs and validates their claims. It is safe
// for concurrent use.
type JWTVerifier struct {
	keys   KeyResolver
	opts   ValidationOptions
	leeway time.Duration
	now    func() time.Time
}

// NewJWTVerifier returns a verifier that resolves keys with keys and
// validates claims per opts.
func NewJWTVerifier(keys KeyResolver, opts ValidationOptions) (*JWTVerifier, error) {
	if keys == nil {
		return nil, errors.New("identity: NewJWTVerifier: nil key resolver")
	}
	if len(opts.Issuers) == 0 || slices.Contains(opts.Issuers, "") {
		return nil, errors.New("identity: NewJWTVerifier: at least one non-empty issuer is required")
	}
	if !opts.SkipAudienceCheck && (len(opts.Audiences) == 0 || slices.Contains(opts.Audiences, "")) {
		return nil, errors.New("identity: NewJWTVerifier: at least one non-empty audience is required")
	}
	for _, a := range opts.AllowedAlgorithms {
		if !a.supported() {
			return nil, fmt.Errorf("identity: NewJWTVerifier: %w: %q", ErrUnsupportedAlgorithm, a)
		}
	}
	leeway := opts.Leeway
	switch {
	case leeway == 0:
		leeway = DefaultLeeway
	case leeway < 0:
		leeway = 0
	case leeway > MaxLeeway:
		return nil, fmt.Errorf("identity: NewJWTVerifier: leeway %s exceeds %s", leeway, MaxLeeway)
	}
	if opts.MaxTokenAge < 0 {
		return nil, errors.New("identity: NewJWTVerifier: negative MaxTokenAge")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	o := opts
	o.Issuers = slices.Clone(opts.Issuers)
	o.Audiences = slices.Clone(opts.Audiences)
	o.AllowedAlgorithms = slices.Clone(opts.AllowedAlgorithms)
	o.RequiredClaims = slices.Clone(opts.RequiredClaims)
	o.ExpectedTypes = slices.Clone(opts.ExpectedTypes)
	return &JWTVerifier{keys: keys, opts: o, leeway: leeway, now: now}, nil
}

// Verify verifies raw's signature and validates its claims.
func (v *JWTVerifier) Verify(ctx context.Context, raw string) (*Claims, error) {
	jws, _, err := VerifyJWS(ctx, raw, v.keys, v.opts.AllowedAlgorithms)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(jws.Header.ContentType, "JWT") {
		return nil, fmt.Errorf("%w: nested JWTs are not supported", ErrMalformedToken)
	}
	if len(v.opts.ExpectedTypes) > 0 {
		got := normalizeTyp(jws.Header.Type)
		ok := false
		for _, t := range v.opts.ExpectedTypes {
			if normalizeTyp(t) == got {
				ok = true
				break
			}
		}
		if !ok {
			return nil, fmt.Errorf("%w: %q", ErrInvalidTokenType, jws.Header.Type)
		}
	}
	c, err := ParseClaims(jws.Payload)
	if err != nil {
		return nil, err
	}
	if err := v.validate(c); err != nil {
		return nil, err
	}
	return c, nil
}

func normalizeTyp(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	return strings.TrimPrefix(t, "application/")
}

func (v *JWTVerifier) validate(c *Claims) error {
	if !slices.Contains(v.opts.Issuers, c.Issuer) {
		return fmt.Errorf("%w: %q", ErrInvalidIssuer, c.Issuer)
	}
	if !v.opts.SkipAudienceCheck {
		ok := false
		for _, a := range v.opts.Audiences {
			if c.Audience.Contains(a) {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("%w: %q", ErrInvalidAudience, []string(c.Audience))
		}
	}
	now := v.now()
	if c.ExpiresAt == nil {
		if !v.opts.AllowMissingExpiration {
			return fmt.Errorf("%w: exp", ErrMissingClaim)
		}
	} else if !now.Before(c.ExpiresAt.Add(v.leeway)) {
		return fmt.Errorf("%w: expired at %s", ErrTokenExpired, c.ExpiresAt.Format(time.RFC3339))
	}
	if c.NotBefore != nil && now.Add(v.leeway).Before(c.NotBefore.Time) {
		return fmt.Errorf("%w: not before %s", ErrTokenNotYetValid, c.NotBefore.Format(time.RFC3339))
	}
	if c.IssuedAt != nil && now.Add(v.leeway).Before(c.IssuedAt.Time) {
		return fmt.Errorf("%w: issued at %s", ErrTokenIssuedInFuture, c.IssuedAt.Format(time.RFC3339))
	}
	if v.opts.MaxTokenAge > 0 {
		if c.IssuedAt == nil {
			return fmt.Errorf("%w: iat", ErrMissingClaim)
		}
		if now.Sub(c.IssuedAt.Time) > v.opts.MaxTokenAge+v.leeway {
			return fmt.Errorf("%w: issued %s ago", ErrTokenTooOld, now.Sub(c.IssuedAt.Time).Round(time.Second))
		}
	}
	for _, name := range v.opts.RequiredClaims {
		val, ok := c.Lookup(name)
		if !ok || val == nil {
			return fmt.Errorf("%w: %s", ErrMissingClaim, name)
		}
		if s, isStr := val.(string); isStr && s == "" {
			return fmt.Errorf("%w: %s is empty", ErrMissingClaim, name)
		}
	}
	if v.opts.AuthorizedParty != "" && c.AuthorizedParty != "" && c.AuthorizedParty != v.opts.AuthorizedParty {
		return fmt.Errorf("%w: %q", ErrInvalidAuthorizedParty, c.AuthorizedParty)
	}
	return nil
}
