package identity

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
	"time"
)

const testIssuer = "https://idp.example.com"

func allTestKeys(t testing.TB) (map[Algorithm]*testKey, *StaticKeySet) {
	t.Helper()
	keys := map[Algorithm]*testKey{}
	var jwks []*JWK
	for _, alg := range SupportedAlgorithms {
		k := keyFor(t, alg)
		keys[alg] = k
		jwks = append(jwks, k.jwk(t))
	}
	set, err := NewStaticKeySet(jwks...)
	if err != nil {
		t.Fatal(err)
	}
	return keys, set
}

func newTestVerifier(t testing.TB, keys KeyResolver, clk *fakeClock, mut func(*ValidationOptions)) *JWTVerifier {
	t.Helper()
	o := ValidationOptions{Issuers: []string{testIssuer}, Audiences: []string{"gta-api"}, Leeway: 30 * time.Second, Now: clk.Now}
	if mut != nil {
		mut(&o)
	}
	v, err := NewJWTVerifier(keys, o)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func validClaims(now time.Time) map[string]any {
	return map[string]any{
		"iss": testIssuer, "aud": []string{"gta-api", "other"}, "sub": "user-1",
		"iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(time.Hour).Unix(), "jti": "id-1",
	}
}

func TestVerifyEveryAlgorithm(t *testing.T) {
	keys, set := allTestKeys(t)
	clk := newFakeClock()
	v := newTestVerifier(t, set, clk, nil)
	for _, alg := range SupportedAlgorithms {
		t.Run(string(alg), func(t *testing.T) {
			tok := signJWT(t, keys[alg], validClaims(clk.Now()), nil)
			c, err := v.Verify(context.Background(), tok)
			if err != nil {
				t.Fatalf("verify: %v", err)
			}
			if c.Subject != "user-1" || !c.Audience.Contains("gta-api") || c.ID != "id-1" {
				t.Fatalf("claims = %+v", c)
			}
			// Without kid every compatible key is tried.
			tok = signJWT(t, keys[alg], validClaims(clk.Now()), map[string]any{"kid": nil})
			if _, err := v.Verify(context.Background(), tok); err != nil {
				t.Fatalf("verify without kid: %v", err)
			}
		})
	}
}

func hs256Token(t *testing.T, secret []byte, kid string, claims map[string]any) string {
	h := map[string]any{"alg": "HS256", "typ": "JWT"}
	if kid != "" {
		h["kid"] = kid
	}
	in := encodeSegments(t, h, claims)
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(in))
	return in + "." + b64.EncodeToString(m.Sum(nil))
}

func TestVerifyNegative(t *testing.T) {
	keys, set := allTestKeys(t)
	clk := newFakeClock()
	now := clk.Now()
	rs := keys[RS256]
	v := newTestVerifier(t, set, clk, nil)

	with := func(mut func(map[string]any)) map[string]any {
		c := validClaims(now)
		mut(c)
		return c
	}
	good := signJWT(t, rs, validClaims(now), nil)
	parts := strings.Split(good, ".")

	der, err := x509.MarshalPKIXPublicKey(&sharedRSA.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	tamperedPayload := b64.EncodeToString([]byte(`{"iss":"` + testIssuer + `","aud":"gta-api","sub":"admin","exp":` + jsonNum(now.Add(time.Hour).Unix()) + `}`))
	sig := []byte(parts[2])
	if sig[0] == 'A' {
		sig[0] = 'B'
	} else {
		sig[0] = 'A'
	}
	es384AsES256 := func() string {
		h := map[string]any{"alg": "ES256", "kid": keys[ES384].kid}
		in := encodeSegments(t, h, validClaims(now))
		return in + "." + b64.EncodeToString(signRaw(t, ES384, sharedP384, []byte(in)))
	}()

	cases := []struct {
		name  string
		token string
		v     *JWTVerifier
		want  error
	}{
		{"expired", signJWT(t, rs, with(func(c map[string]any) { c["exp"] = now.Add(-time.Minute).Unix() }), nil), nil, ErrTokenExpired},
		{"not yet valid", signJWT(t, rs, with(func(c map[string]any) { c["nbf"] = now.Add(time.Hour).Unix() }), nil), nil, ErrTokenNotYetValid},
		{"issued in future", signJWT(t, rs, with(func(c map[string]any) { c["iat"] = now.Add(time.Hour).Unix() }), nil), nil, ErrTokenIssuedInFuture},
		{"wrong audience", signJWT(t, rs, with(func(c map[string]any) { c["aud"] = "someone-else" }), nil), nil, ErrInvalidAudience},
		{"missing audience", signJWT(t, rs, with(func(c map[string]any) { delete(c, "aud") }), nil), nil, ErrInvalidAudience},
		{"wrong issuer", signJWT(t, rs, with(func(c map[string]any) { c["iss"] = "https://evil.example.com" }), nil), nil, ErrInvalidIssuer},
		{"missing exp", signJWT(t, rs, with(func(c map[string]any) { delete(c, "exp") }), nil), nil, ErrMissingClaim},
		{"alg none no sig", encodeSegments(t, map[string]any{"alg": "none"}, validClaims(now)) + ".", nil, ErrAlgNone},
		{"alg none with sig", encodeSegments(t, map[string]any{"alg": "none"}, validClaims(now)) + ".c2ln", nil, ErrAlgNone},
		{"alg NoNe", encodeSegments(t, map[string]any{"alg": "NoNe"}, validClaims(now)) + ".c2ln", nil, ErrAlgNone},
		{"tampered payload", parts[0] + "." + tamperedPayload + "." + parts[2], nil, ErrInvalidSignature},
		{"tampered signature", parts[0] + "." + parts[1] + "." + string(sig), nil, ErrInvalidSignature},
		{"HS256 with RSA PEM as secret", hs256Token(t, pemKey, rs.kid, validClaims(now)), nil, ErrUnsupportedAlgorithm},
		{"HS256 with RSA DER as secret", hs256Token(t, der, "", validClaims(now)), nil, ErrUnsupportedAlgorithm},
		{"unknown kid", signJWT(t, rs, validClaims(now), map[string]any{"kid": "nope"}), nil, ErrKeyNotFound},
		{"kid of EC key with RS256", signJWT(t, &testKey{alg: RS256, kid: keys[ES256].kid, signer: sharedRSA}, validClaims(now), nil), nil, ErrKeyNotFound},
		{"ES384 signature claimed as ES256", es384AsES256, nil, ErrKeyNotFound},
		{"critical header", signJWT(t, rs, validClaims(now), map[string]any{"crit": []string{"exp"}}), nil, ErrMalformedToken},
		{"nested JWT", signJWT(t, rs, validClaims(now), map[string]any{"cty": "JWT"}), nil, ErrMalformedToken},
		{"two segments", parts[0] + "." + parts[1], nil, ErrMalformedToken},
		{"four segments", good + ".x", nil, ErrMalformedToken},
		{"padded base64", parts[0] + "=." + parts[1] + "." + parts[2], nil, ErrMalformedToken},
		{"empty", "", nil, ErrMalformedToken},
		{"header not object", b64.EncodeToString([]byte(`["RS256"]`)) + "." + parts[1] + "." + parts[2], nil, ErrMalformedToken},
		{"alg not allowed", signJWT(t, keys[ES256], validClaims(now), nil), newTestVerifier(t, set, clk, func(o *ValidationOptions) { o.AllowedAlgorithms = []Algorithm{RS256} }), ErrAlgorithmNotAllowed},
		{"required claim", good, newTestVerifier(t, set, clk, func(o *ValidationOptions) { o.RequiredClaims = []string{"email"} }), ErrMissingClaim},
		{"max age", signJWT(t, rs, with(func(c map[string]any) { c["iat"] = now.Add(-2 * time.Hour).Unix() }), nil), newTestVerifier(t, set, clk, func(o *ValidationOptions) { o.MaxTokenAge = time.Hour }), ErrTokenTooOld},
		{"typ mismatch", good, newTestVerifier(t, set, clk, func(o *ValidationOptions) { o.ExpectedTypes = []string{"at+jwt"} }), ErrInvalidTokenType},
		{"azp mismatch", signJWT(t, rs, with(func(c map[string]any) { c["azp"] = "other-client" }), nil), newTestVerifier(t, set, clk, func(o *ValidationOptions) { o.AuthorizedParty = "gta-cli" }), ErrInvalidAuthorizedParty},
		{"exp wrong type", signJWT(t, rs, with(func(c map[string]any) { c["exp"] = "tomorrow" }), nil), nil, ErrMalformedToken},
		{"iss wrong type", signJWT(t, rs, with(func(c map[string]any) { c["iss"] = 7 }), nil), nil, ErrMalformedToken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vv := tc.v
			if vv == nil {
				vv = v
			}
			_, err := vv.Verify(context.Background(), tc.token)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func jsonNum(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestTamperedSignatureBytes(t *testing.T) {
	keys, set := allTestKeys(t)
	clk := newFakeClock()
	v := newTestVerifier(t, set, clk, nil)
	for _, alg := range SupportedAlgorithms {
		tok := signJWT(t, keys[alg], validClaims(clk.Now()), nil)
		i := strings.LastIndexByte(tok, '.')
		sig, err := b64.DecodeString(tok[i+1:])
		if err != nil {
			t.Fatal(err)
		}
		sig[len(sig)/2] ^= 0x80
		bad := tok[:i+1] + b64.EncodeToString(sig)
		if _, err := v.Verify(context.Background(), bad); !errors.Is(err, ErrInvalidSignature) {
			t.Errorf("%s: err = %v, want ErrInvalidSignature", alg, err)
		}
	}
}

func TestDuplicateMembersRejected(t *testing.T) {
	_, set := allTestKeys(t)
	clk := newFakeClock()
	v := newTestVerifier(t, set, clk, nil)
	k := keyFor(t, RS256)
	now := clk.Now()
	payload := `{"iss":"` + testIssuer + `","aud":"gta-api","sub":"user","sub":"admin","exp":` + jsonNum(now.Add(time.Hour).Unix()) + `}`
	hdr := `{"alg":"RS256","kid":"` + k.kid + `"}`
	in := b64.EncodeToString([]byte(hdr)) + "." + b64.EncodeToString([]byte(payload))
	tok := in + "." + b64.EncodeToString(signRaw(t, RS256, k.signer, []byte(in)))
	if _, err := v.Verify(context.Background(), tok); !errors.Is(err, ErrMalformedToken) {
		t.Fatalf("duplicate claim: err = %v", err)
	}
	hdr = `{"alg":"RS256","alg":"none"}`
	if _, err := ParseJWS(b64.EncodeToString([]byte(hdr)) + ".e30.c2ln"); !errors.Is(err, ErrMalformedToken) {
		t.Fatalf("duplicate header: err = %v", err)
	}
}

func TestLeeway(t *testing.T) {
	keys, set := allTestKeys(t)
	clk := newFakeClock()
	now := clk.Now()
	c := validClaims(now)
	c["exp"] = now.Add(-10 * time.Second).Unix()
	tok := signJWT(t, keys[ES256], c, nil)
	if _, err := newTestVerifier(t, set, clk, nil).Verify(context.Background(), tok); err != nil {
		t.Fatalf("within 30s leeway: %v", err)
	}
	strict := newTestVerifier(t, set, clk, func(o *ValidationOptions) { o.Leeway = -1 })
	if _, err := strict.Verify(context.Background(), tok); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("zero leeway: %v", err)
	}
	if _, err := NewJWTVerifier(set, ValidationOptions{Issuers: []string{testIssuer}, Audiences: []string{"a"}, Leeway: time.Hour}); err == nil {
		t.Fatal("excessive leeway accepted")
	}
	if _, err := NewJWTVerifier(set, ValidationOptions{Issuers: []string{testIssuer}}); err == nil {
		t.Fatal("missing audience accepted")
	}
	if _, err := NewJWTVerifier(set, ValidationOptions{Audiences: []string{"a"}}); err == nil {
		t.Fatal("missing issuer accepted")
	}
}

func TestVerifySignatureKeyTypeConfusion(t *testing.T) {
	genKeys(t)
	in := []byte("a.b")
	if err := VerifySignature(RS256, []byte("secret"), in, []byte("x")); !errors.Is(err, ErrKeyAlgMismatch) {
		t.Fatalf("[]byte key: %v", err)
	}
	if err := VerifySignature(RS256, &sharedP256.PublicKey, in, []byte("x")); !errors.Is(err, ErrKeyAlgMismatch) {
		t.Fatalf("EC key for RS256: %v", err)
	}
	if err := VerifySignature(ES256, &sharedP384.PublicKey, in, make([]byte, 64)); !errors.Is(err, ErrKeyAlgMismatch) {
		t.Fatalf("P-384 key for ES256: %v", err)
	}
	if err := VerifySignature(EdDSA, &sharedRSA.PublicKey, in, nil); !errors.Is(err, ErrKeyAlgMismatch) {
		t.Fatalf("RSA key for EdDSA: %v", err)
	}
	if err := VerifySignature("HS256", &sharedRSA.PublicKey, in, nil); !errors.Is(err, ErrUnsupportedAlgorithm) {
		t.Fatalf("HS256: %v", err)
	}
	if err := VerifySignature("none", nil, in, nil); !errors.Is(err, ErrAlgNone) {
		t.Fatalf("none: %v", err)
	}
	weak, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifySignature(RS256, &weak.PublicKey, in, nil); !errors.Is(err, ErrUnsupportedKey) {
		t.Fatalf("1024-bit RSA: %v", err)
	}
}

func TestJWKParsing(t *testing.T) {
	genKeys(t)
	for _, pub := range []any{&sharedRSA.PublicKey, &sharedP256.PublicKey, &sharedP384.PublicKey, &sharedP521.PublicKey, sharedEd.Public()} {
		j, err := NewJWK(pub, "k", "")
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(j)
		back, err := ParseJWK(raw)
		if err != nil {
			t.Fatalf("round trip %T: %v", pub, err)
		}
		if back.KeyType != j.KeyType {
			t.Fatalf("kty %s != %s", back.KeyType, j.KeyType)
		}
	}
	bad := []string{
		`{"kty":"oct","k":"c2VjcmV0"}`,
		`{"kty":"RSA","n":"AQAB","e":"AQAB"}`, // tiny modulus
		`{"kty":"EC","crv":"P-256","x":"AAAA","y":"AAAA"}`,
		`{"kty":"EC","crv":"secp256k1","x":"AA","y":"AA"}`,
		`{"kty":"OKP","crv":"X25519","x":"AA"}`,
		`{"kty":"RSA","n":"x","e":"AQAB","d":"AQAB"}`,
		`{"kty":"RSA","kty":"EC"}`,
	}
	for _, b := range bad {
		if _, err := ParseJWK([]byte(b)); err == nil {
			t.Errorf("ParseJWK(%s) accepted", b)
		}
	}
	// A point not on the curve is rejected.
	x := make([]byte, 32)
	x[31] = 1
	pt := `{"kty":"EC","crv":"P-256","x":"` + b64.EncodeToString(x) + `","y":"` + b64.EncodeToString(x) + `"}`
	if _, err := ParseJWK([]byte(pt)); err == nil {
		t.Error("off-curve point accepted")
	}
	// Declared alg must fit the key type.
	j, _ := NewJWK(&sharedRSA.PublicKey, "k", "")
	j.Algorithm = "ES256"
	raw, _ := json.Marshal(j)
	if _, err := ParseJWK(raw); err == nil {
		t.Error("RSA key declaring ES256 accepted")
	}
	// A JWK pinned to RS256 is not usable for PS256; use=enc is never usable.
	k, _ := NewJWK(&sharedRSA.PublicKey, "k", RS256)
	if k.usableWith(PS256) || !k.usableWith(RS256) {
		t.Error("alg pinning not enforced")
	}
	k.Use = "enc"
	if k.usableWith(RS256) {
		t.Error("enc key usable for signatures")
	}
	ek, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ej, _ := NewJWK(ek, "e", "")
	if !ej.usableWith(ES256) || ej.usableWith(ES384) {
		t.Error("EC curve binding wrong")
	}
	_, edPriv, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := NewJWK(edPriv, "ed", EdDSA); err != nil {
		t.Errorf("NewJWK from private key: %v", err)
	}
}

func TestParseJWKS(t *testing.T) {
	genKeys(t)
	k, _ := NewJWK(&sharedRSA.PublicKey, "a", RS256)
	raw, _ := json.Marshal(map[string]any{"keys": []any{k, map[string]string{"kty": "oct", "k": "AA"}, map[string]string{"kty": "weird"}}})
	keys, skipped, err := ParseJWKS(raw)
	if err != nil || len(keys) != 1 || len(skipped) != 2 {
		t.Fatalf("keys=%d skipped=%d err=%v", len(keys), len(skipped), err)
	}
	if _, _, err := ParseJWKS([]byte(`{}`)); err == nil {
		t.Fatal("missing keys accepted")
	}
	if _, _, err := ParseJWKS([]byte(`{"keys":[]} x`)); err == nil {
		t.Fatal("trailing data accepted")
	}
}

func TestClaimsHelpers(t *testing.T) {
	c, err := ParseClaims([]byte(`{"sub":"u","aud":"a","realm_access":{"roles":["r1","r2"]},"https://ex.com/roles":["x"],"scope":"a b  c","n":3}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Strings("realm_access.roles", false); len(got) != 2 || got[1] != "r2" {
		t.Fatalf("dot path: %v", got)
	}
	if got := c.Strings("https://ex.com/roles", false); len(got) != 1 {
		t.Fatalf("literal key: %v", got)
	}
	if got := c.Strings("/https:~1~1ex.com~1roles", false); len(got) != 1 {
		t.Fatalf("pointer: %v", got)
	}
	if got := c.Strings("scope", true); len(got) != 3 {
		t.Fatalf("scope split: %v", got)
	}
	if _, ok := c.Lookup("realm_access.missing"); ok {
		t.Fatal("missing path found")
	}
	if c.Raw["n"].(json.Number).String() != "3" {
		t.Fatal("numbers should be json.Number")
	}
	var d NumericDate
	if err := d.UnmarshalJSON([]byte("1.5")); err != nil || d.UnixMilli() != 1500 {
		t.Fatalf("fractional date: %v %v", d, err)
	}
	for _, bad := range []string{"-1", "1e300", `"1"`, "NaN"} {
		if err := d.UnmarshalJSON([]byte(bad)); err == nil {
			t.Errorf("NumericDate(%s) accepted", bad)
		}
	}
}
