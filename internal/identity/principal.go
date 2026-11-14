package identity

import (
	"context"
	"crypto/subtle"
	"slices"
	"time"
)

// PrincipalType classifies an authenticated caller.
type PrincipalType string

// Principal types.
const (
	PrincipalUser           PrincipalType = "user"
	PrincipalServiceAccount PrincipalType = "service_account"
)

// Principal is an authenticated caller.
type Principal struct {
	Type PrincipalType `json:"type"`
	// Subject uniquely identifies the principal within Issuer ("sub", or
	// the service account name for API keys).
	Subject string `json:"subject"`
	Issuer  string `json:"issuer,omitempty"`
	Name    string `json:"name,omitempty"`
	Email   string `json:"email,omitempty"`
	// Roles are the principal's direct roles: mapped from IdP claims for
	// tokens, configured for API keys. Inherited and default roles are
	// added by the Engine.
	Roles  []string `json:"roles,omitempty"`
	Groups []string `json:"groups,omitempty"`
	// Scopes, when non-nil, cap what the credential may do: a permission
	// must match one of these patterns in addition to being granted by a
	// role or rule. Nil means unrestricted.
	Scopes []string `json:"scopes,omitempty"`
	// Attributes feed attribute conditions in policy rules.
	Attributes map[string]string `json:"attributes,omitempty"`
	// Claims are the raw token claims (nil for API keys).
	Claims map[string]any `json:"-"`
	// AuthMethod is "jwt" or "api_key".
	AuthMethod string `json:"auth_method"`
	// CredentialID identifies the credential (token "jti" or API key ID).
	CredentialID string    `json:"credential_id,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitzero"`
}

// HasRole reports whether role is one of p's direct roles.
func (p *Principal) HasRole(role string) bool {
	return p != nil && slices.Contains(p.Roles, role)
}

type principalKey struct{}

// WithPrincipal returns a context carrying p.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the principal stored by WithPrincipal.
func PrincipalFrom(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(*Principal)
	return p, ok && p != nil
}

// constantTimeEqual compares two strings without leaking, through timing,
// where they first differ.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
