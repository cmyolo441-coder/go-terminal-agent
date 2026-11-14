package identity

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	_ "crypto/sha256" // register SHA-256 for crypto.Hash
	_ "crypto/sha512" // register SHA-384/512 for crypto.Hash
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"
)

// Algorithm is a JWS "alg" header value.
type Algorithm string

// Supported asymmetric signature algorithms (RFC 7518, RFC 8037).
const (
	RS256 Algorithm = "RS256"
	RS384 Algorithm = "RS384"
	RS512 Algorithm = "RS512"
	PS256 Algorithm = "PS256"
	PS384 Algorithm = "PS384"
	PS512 Algorithm = "PS512"
	ES256 Algorithm = "ES256"
	ES384 Algorithm = "ES384"
	ES512 Algorithm = "ES512"
	EdDSA Algorithm = "EdDSA"
)

// SupportedAlgorithms lists every algorithm this package can verify. It is
// the default allow-list when a caller does not narrow it.
var SupportedAlgorithms = []Algorithm{RS256, RS384, RS512, PS256, PS384, PS512, ES256, ES384, ES512, EdDSA}

// Limits that bound the work an attacker-supplied token can cause.
const (
	// MaxTokenBytes is the largest compact JWS accepted.
	MaxTokenBytes = 64 << 10
	// maxKeyCandidates bounds signature attempts for a token without "kid".
	maxKeyCandidates = 16
	minRSABits       = 2048
	maxRSABits       = 16384
)

// Errors returned by parsing and verification. They are wrapped with
// detail; test with errors.Is.
var (
	ErrMalformedToken       = errors.New("identity: malformed token")
	ErrAlgNone              = errors.New("identity: unsecured JWS (alg \"none\") rejected")
	ErrUnsupportedAlgorithm = errors.New("identity: unsupported signature algorithm")
	ErrAlgorithmNotAllowed  = errors.New("identity: signature algorithm not allowed")
	ErrKeyNotFound          = errors.New("identity: no matching verification key")
	ErrKeyAlgMismatch       = errors.New("identity: key cannot be used with algorithm")
	ErrInvalidSignature     = errors.New("identity: invalid signature")
	ErrUnsupportedKey       = errors.New("identity: unsupported or invalid key")
)

var b64 = base64.RawURLEncoding.Strict()

// Header is the protected JOSE header of a JWS.
type Header struct {
	Algorithm   Algorithm `json:"alg"`
	KeyID       string    `json:"kid,omitempty"`
	Type        string    `json:"typ,omitempty"`
	ContentType string    `json:"cty,omitempty"`
	// Critical is non-nil when the header carried "crit". No extensions are
	// understood, so any such token is rejected (RFC 7515 §4.1.11).
	Critical []string `json:"crit,omitempty"`
}

// JWS is a parsed, not yet verified, compact JWS.
type JWS struct {
	Header    Header
	Payload   []byte
	Signature []byte
	// SigningInput is BASE64URL(header) || '.' || BASE64URL(payload).
	SigningInput string
}

// ParseJWS parses a compact JWS without verifying its signature. It is
// strict: exactly three non-empty-signature segments, canonical unpadded
// base64url, a header that is a JSON object without duplicate members, a
// known asymmetric algorithm and no "crit" extensions. "none" in any letter
// case yields ErrAlgNone.
func ParseJWS(token string) (*JWS, error) {
	if token == "" {
		return nil, fmt.Errorf("%w: empty token", ErrMalformedToken)
	}
	if len(token) > MaxTokenBytes {
		return nil, fmt.Errorf("%w: token exceeds %d bytes", ErrMalformedToken, MaxTokenBytes)
	}
	if strings.Count(token, ".") != 2 {
		return nil, fmt.Errorf("%w: compact JWS must have three segments", ErrMalformedToken)
	}
	first := strings.IndexByte(token, '.')
	last := strings.LastIndexByte(token, '.')
	rawHeader, rawPayload, rawSig := token[:first], token[first+1:last], token[last+1:]
	if rawHeader == "" {
		return nil, fmt.Errorf("%w: empty header", ErrMalformedToken)
	}
	hb, err := b64.DecodeString(rawHeader)
	if err != nil {
		return nil, fmt.Errorf("%w: header encoding: %v", ErrMalformedToken, err)
	}
	var h Header
	if err := decodeStrictObject(hb, &h); err != nil {
		return nil, fmt.Errorf("%w: header: %v", ErrMalformedToken, err)
	}
	if strings.EqualFold(string(h.Algorithm), "none") {
		return nil, ErrAlgNone
	}
	if !h.Algorithm.supported() {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedAlgorithm, h.Algorithm)
	}
	if h.Critical != nil {
		return nil, fmt.Errorf("%w: critical header extensions %q are not supported", ErrMalformedToken, h.Critical)
	}
	if rawSig == "" {
		return nil, fmt.Errorf("%w: empty signature", ErrMalformedToken)
	}
	payload, err := b64.DecodeString(rawPayload)
	if err != nil {
		return nil, fmt.Errorf("%w: payload encoding: %v", ErrMalformedToken, err)
	}
	sig, err := b64.DecodeString(rawSig)
	if err != nil {
		return nil, fmt.Errorf("%w: signature encoding: %v", ErrMalformedToken, err)
	}
	return &JWS{Header: h, Payload: payload, Signature: sig, SigningInput: token[:last]}, nil
}

// decodeStrictObject unmarshals a JSON object into v after checking that
// data is exactly one object with no duplicate top-level member names and
// no trailing data. Duplicate names are rejected because different parsers
// resolve them differently, which is a known source of signature-bypass bugs.
func decodeStrictObject(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return errors.New("not a JSON object")
	}
	seen := make(map[string]struct{})
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := kt.(string)
		if !ok {
			return errors.New("invalid member name")
		}
		if _, dup := seen[key]; dup {
			return fmt.Errorf("duplicate member %q", key)
		}
		seen[key] = struct{}{}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil { // closing '}'
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after JSON object")
	}
	return json.Unmarshal(data, v)
}

func (a Algorithm) supported() bool {
	for _, s := range SupportedAlgorithms {
		if a == s {
			return true
		}
	}
	return false
}

func (a Algorithm) hash() crypto.Hash {
	switch a {
	case RS256, PS256, ES256:
		return crypto.SHA256
	case RS384, PS384, ES384:
		return crypto.SHA384
	case RS512, PS512, ES512:
		return crypto.SHA512
	}
	return 0
}

func (a Algorithm) curve() elliptic.Curve {
	switch a {
	case ES256:
		return elliptic.P256()
	case ES384:
		return elliptic.P384()
	case ES512:
		return elliptic.P521()
	}
	return nil
}

// keyType returns the JWK "kty" this algorithm requires.
func (a Algorithm) keyType() string {
	switch a {
	case RS256, RS384, RS512, PS256, PS384, PS512:
		return "RSA"
	case ES256, ES384, ES512:
		return "EC"
	case EdDSA:
		return "OKP"
	}
	return ""
}

// VerifySignature checks sig over signingInput with key under alg. The key
// must be of the type alg requires (*rsa.PublicKey, *ecdsa.PublicKey on the
// matching curve, or ed25519.PublicKey); anything else, including []byte
// secrets, yields ErrKeyAlgMismatch.
func VerifySignature(alg Algorithm, key crypto.PublicKey, signingInput, sig []byte) error {
	if strings.EqualFold(string(alg), "none") {
		return ErrAlgNone
	}
	if !alg.supported() {
		return fmt.Errorf("%w: %q", ErrUnsupportedAlgorithm, alg)
	}
	switch alg {
	case RS256, RS384, RS512, PS256, PS384, PS512:
		pub, ok := key.(*rsa.PublicKey)
		if !ok || pub == nil || pub.N == nil {
			return fmt.Errorf("%w: %s requires an RSA public key, got %T", ErrKeyAlgMismatch, alg, key)
		}
		if bits := pub.N.BitLen(); bits < minRSABits || bits > maxRSABits {
			return fmt.Errorf("%w: RSA modulus of %d bits", ErrUnsupportedKey, bits)
		}
		h := alg.hash()
		hh := h.New()
		hh.Write(signingInput)
		digest := hh.Sum(nil)
		var err error
		if alg[0] == 'P' {
			err = rsa.VerifyPSS(pub, h, digest, sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: h})
		} else {
			err = rsa.VerifyPKCS1v15(pub, h, digest, sig)
		}
		if err != nil {
			return ErrInvalidSignature
		}
		return nil
	case ES256, ES384, ES512:
		pub, ok := key.(*ecdsa.PublicKey)
		if !ok || pub == nil {
			return fmt.Errorf("%w: %s requires an ECDSA public key, got %T", ErrKeyAlgMismatch, alg, key)
		}
		want := alg.curve()
		if pub.Curve != want {
			return fmt.Errorf("%w: %s requires curve %s", ErrKeyAlgMismatch, alg, want.Params().Name)
		}
		size := (want.Params().BitSize + 7) / 8
		if len(sig) != 2*size {
			return ErrInvalidSignature
		}
		r := new(big.Int).SetBytes(sig[:size])
		s := new(big.Int).SetBytes(sig[size:])
		hh := alg.hash().New()
		hh.Write(signingInput)
		if !ecdsa.Verify(pub, hh.Sum(nil), r, s) {
			return ErrInvalidSignature
		}
		return nil
	case EdDSA:
		pub, ok := key.(ed25519.PublicKey)
		if !ok || len(pub) != ed25519.PublicKeySize {
			return fmt.Errorf("%w: EdDSA requires an Ed25519 public key, got %T", ErrKeyAlgMismatch, key)
		}
		if !ed25519.Verify(pub, signingInput, sig) {
			return ErrInvalidSignature
		}
		return nil
	}
	return fmt.Errorf("%w: %q", ErrUnsupportedAlgorithm, alg)
}

// KeyResolver supplies candidate verification keys for a JWS header.
// Implementations return ErrKeyNotFound when nothing matches.
type KeyResolver interface {
	ResolveKeys(ctx context.Context, h Header) ([]*JWK, error)
}

// StaticKeySet is a fixed KeyResolver, for pinned keys and tests.
type StaticKeySet struct {
	keys []*JWK
}

// NewStaticKeySet returns a resolver over keys. Each key must be parsed
// (have a non-nil Key).
func NewStaticKeySet(keys ...*JWK) (*StaticKeySet, error) {
	for i, k := range keys {
		if k == nil || k.Key == nil {
			return nil, fmt.Errorf("%w: key %d has no public key material", ErrUnsupportedKey, i)
		}
	}
	return &StaticKeySet{keys: append([]*JWK(nil), keys...)}, nil
}

// ResolveKeys implements KeyResolver.
func (s *StaticKeySet) ResolveKeys(_ context.Context, h Header) ([]*JWK, error) {
	c := selectKeys(s.keys, h)
	if len(c) == 0 {
		return nil, keyNotFound(h)
	}
	return c, nil
}

func keyNotFound(h Header) error {
	if h.KeyID != "" {
		return fmt.Errorf("%w: kid %q for %s", ErrKeyNotFound, h.KeyID, h.Algorithm)
	}
	return fmt.Errorf("%w: no key usable with %s", ErrKeyNotFound, h.Algorithm)
}

// selectKeys returns the keys that may verify a token with header h: the
// key with the token's kid (when present) that is compatible with alg.
func selectKeys(keys []*JWK, h Header) []*JWK {
	var out []*JWK
	for _, k := range keys {
		if h.KeyID != "" && k.KeyID != h.KeyID {
			continue
		}
		if !k.usableWith(h.Algorithm) {
			continue
		}
		out = append(out, k)
		if len(out) == maxKeyCandidates {
			break
		}
	}
	return out
}

// VerifyJWS parses token, checks its algorithm against allowed (all
// supported algorithms when empty), resolves keys and verifies the
// signature. It returns the parsed JWS and the key that verified it.
func VerifyJWS(ctx context.Context, token string, keys KeyResolver, allowed []Algorithm) (*JWS, *JWK, error) {
	if keys == nil {
		return nil, nil, errors.New("identity: VerifyJWS: nil key resolver")
	}
	jws, err := ParseJWS(token)
	if err != nil {
		return nil, nil, err
	}
	if len(allowed) == 0 {
		allowed = SupportedAlgorithms
	}
	ok := false
	for _, a := range allowed {
		if a == jws.Header.Algorithm {
			ok = true
			break
		}
	}
	if !ok {
		return nil, nil, fmt.Errorf("%w: %q", ErrAlgorithmNotAllowed, jws.Header.Algorithm)
	}
	cands, err := keys.ResolveKeys(ctx, jws.Header)
	if err != nil {
		return nil, nil, err
	}
	in := []byte(jws.SigningInput)
	var lastErr error = keyNotFound(jws.Header)
	for _, k := range cands {
		if !k.usableWith(jws.Header.Algorithm) {
			continue // defend against resolvers that do not filter
		}
		if err := VerifySignature(jws.Header.Algorithm, k.Key, in, jws.Signature); err != nil {
			lastErr = err
			continue
		}
		return jws, k, nil
	}
	return nil, nil, lastErr
}
