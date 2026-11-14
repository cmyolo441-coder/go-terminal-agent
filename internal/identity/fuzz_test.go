package identity

import (
	"context"
	"strings"
	"testing"
	"time"
)

// FuzzParseJWS checks that the parser never panics, never accepts "none"
// or symmetric algorithms, and that whatever it accepts is canonical: the
// payload and signature re-encode to exactly the input segments.
func FuzzParseJWS(f *testing.F) {
	now := time.Unix(1_800_000_000, 0)
	for _, alg := range []Algorithm{RS256, PS256, ES256, ES512, EdDSA} {
		f.Add(signJWT(f, keyFor(f, alg), validClaims(now), nil))
	}
	f.Add("eyJhbGciOiJub25lIn0.e30.")
	f.Add("eyJhbGciOiJIUzI1NiJ9.e30.c2ln")
	f.Add("eyJhbGciOiJSUzI1NiIsImFsZyI6Im5vbmUifQ.e30.c2ln")
	f.Add("..")
	f.Add("a.b.c.d")
	f.Add("")
	_, set := allTestKeys(f)
	v, err := NewJWTVerifier(set, ValidationOptions{Issuers: []string{testIssuer}, Audiences: []string{"gta-api"}, Now: func() time.Time { return now }})
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, tok string) {
		jws, err := ParseJWS(tok)
		if err != nil {
			if jws != nil {
				t.Fatal("non-nil JWS with error")
			}
			return
		}
		if !jws.Header.Algorithm.supported() {
			t.Fatalf("accepted algorithm %q", jws.Header.Algorithm)
		}
		parts := strings.Split(tok, ".")
		if len(parts) != 3 || b64.EncodeToString(jws.Payload) != parts[1] || b64.EncodeToString(jws.Signature) != parts[2] {
			t.Fatalf("non-canonical token accepted: %q", tok)
		}
		if jws.SigningInput != parts[0]+"."+parts[1] {
			t.Fatal("signing input mismatch")
		}
		// Full verification must not panic either.
		_, _ = v.Verify(context.Background(), tok)
	})
}

// FuzzParsePolicy checks that policy parsing never panics and that any
// accepted policy can be evaluated.
func FuzzParsePolicy(f *testing.F) {
	f.Add([]byte(testPolicy))
	f.Add([]byte(`{"version":1,"roles":{"a":{"inherits":["a"]}}}`))
	f.Add([]byte(`{"version":1,"roles":{"a":{"permissions":["*"]}}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := ParsePolicy(data)
		if err != nil {
			return
		}
		e, err := NewEngine(p)
		if err != nil {
			t.Fatalf("parsed policy failed to compile: %v", err)
		}
		var roles []string
		for r := range p.Roles {
			roles = append(roles, r)
		}
		d := e.Authorize(Request{Principal: &Principal{Subject: "s", Roles: roles}, Permission: "repo:read", Workspace: "/w", Environment: "dev"})
		if d.Reason == "" {
			t.Fatal("decision without reason")
		}
	})
}

// FuzzGlobMatch cross-checks globMatch against a simple recursive matcher.
func FuzzGlobMatch(f *testing.F) {
	f.Add("browser_*", "browser_click")
	f.Add("a*b?c", "aXXbYc")
	f.Add("**", "")
	f.Fuzz(func(t *testing.T, p, s string) {
		if len(p) > 12 || len(s) > 24 {
			return
		}
		if got, want := globMatch(p, s), refGlob(p, s); got != want {
			t.Fatalf("globMatch(%q, %q) = %v, want %v", p, s, got, want)
		}
	})
}

func refGlob(p, s string) bool {
	if p == "" {
		return s == ""
	}
	switch p[0] {
	case '*':
		for i := 0; i <= len(s); i++ {
			if refGlob(p[1:], s[i:]) {
				return true
			}
		}
		return false
	case '?':
		return s != "" && refGlob(p[1:], s[1:])
	default:
		return s != "" && s[0] == p[0] && refGlob(p[1:], s[1:])
	}
}
