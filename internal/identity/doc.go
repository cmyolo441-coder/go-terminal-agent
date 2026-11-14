// Package identity provides enterprise identity for gta: verification of
// signed JSON Web Tokens, OpenID Connect sign-in from a terminal, encrypted
// token storage, API keys for service accounts, and a role- and
// attribute-based authorization engine with explainable decisions.
//
// # Token verification
//
// ParseJWS and VerifyJWS implement the compact JWS serialization (RFC 7515)
// for the asymmetric algorithms RS256/384/512, PS256/384/512,
// ES256/384/512 and EdDSA (Ed25519). Symmetric (HS*) algorithms and the
// unsecured "none" algorithm are never accepted, which removes the classic
// algorithm-confusion attack in which an RSA public key is used as an HMAC
// secret. A key is only ever used with the algorithm family its JWK type
// declares, and with the exact algorithm when the JWK carries "alg".
//
// JWTVerifier layers RFC 7519 claim validation on top: issuer, audience
// (single or list), exp/nbf/iat with a bounded leeway, a maximum token age,
// required claims, "typ" and "azp". RemoteKeySet fetches a JWKS document,
// honours ETag and Cache-Control max-age, refreshes in the background,
// re-fetches at most once per MinRefreshInterval when a token names an
// unknown "kid" (key rotation), and collapses concurrent fetches into one.
//
// # OpenID Connect
//
// Client performs discovery and the two flows a terminal needs: the Device
// Authorization Grant (RFC 8628), which works over SSH and on headless
// machines, and the Authorization Code grant with PKCE S256 (RFC 7636) and
// a loopback redirect listener (RFC 8252). Refresh tokens are supported and
// ID tokens are verified, including the nonce. Tokens are persisted through
// the TokenStore interface; FileTokenStore writes each token atomically with
// mode 0600, encrypted with AES-256-GCM under a key derived (HKDF-SHA256)
// from a KeySource.
//
// # Authorization
//
// Engine evaluates a JSON policy of roles (with inheritance and cycle
// detection), permissions of the form "resource:action" with glob
// wildcards ("tools:browser_*"), identity-provider claim to role mappings,
// and allow/deny rules with attribute conditions (time windows, workspace
// path globs, environment, arbitrary attributes). Deny overrides allow, a
// deny rule whose condition cannot be evaluated because an attribute is
// missing still applies (fail closed), and every Decision explains itself.
//
// # HTTP
//
// Authenticator.Authenticate accepts either a Bearer JWT or an API key and
// stores the resulting Principal in the request context;
// Authenticator.Require enforces a permission with the Engine.
package identity
