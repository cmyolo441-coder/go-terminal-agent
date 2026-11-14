package identity

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
)

// JWK is a public JSON Web Key (RFC 7517) usable for signature
// verification. Private and symmetric keys are rejected when parsing.
type JWK struct {
	KeyType   string   `json:"kty"`
	KeyID     string   `json:"kid,omitempty"`
	Use       string   `json:"use,omitempty"`
	KeyOps    []string `json:"key_ops,omitempty"`
	Algorithm string   `json:"alg,omitempty"`
	Curve     string   `json:"crv,omitempty"`
	N         string   `json:"n,omitempty"`
	E         string   `json:"e,omitempty"`
	X         string   `json:"x,omitempty"`
	Y         string   `json:"y,omitempty"`

	// Key is the parsed public key: *rsa.PublicKey, *ecdsa.PublicKey or
	// ed25519.PublicKey.
	Key crypto.PublicKey `json:"-"`
}

// jwkWire adds members that are only inspected to reject a key.
type jwkWire struct {
	JWK
	D   string `json:"d,omitempty"`
	K   string `json:"k,omitempty"`
	P   string `json:"p,omitempty"`
	Oth any    `json:"oth,omitempty"`
}

// ParseJWK parses and validates one public JWK.
func ParseJWK(data []byte) (*JWK, error) {
	var w jwkWire
	if err := decodeStrictObject(data, &w); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsupportedKey, err)
	}
	if w.D != "" || w.P != "" || w.Oth != nil {
		return nil, fmt.Errorf("%w: JWK %q contains private key material", ErrUnsupportedKey, w.KeyID)
	}
	if w.KeyType == "oct" || w.K != "" {
		return nil, fmt.Errorf("%w: symmetric JWK %q is not accepted", ErrUnsupportedKey, w.KeyID)
	}
	k := w.JWK
	if err := k.parse(); err != nil {
		return nil, err
	}
	return &k, nil
}

func (k *JWK) parse() error {
	switch k.KeyType {
	case "RSA":
		nb, err := b64.DecodeString(k.N)
		if err != nil || len(nb) == 0 {
			return fmt.Errorf("%w: RSA key %q: bad modulus", ErrUnsupportedKey, k.KeyID)
		}
		eb, err := b64.DecodeString(k.E)
		if err != nil || len(eb) == 0 || len(eb) > 4 {
			return fmt.Errorf("%w: RSA key %q: bad exponent", ErrUnsupportedKey, k.KeyID)
		}
		e := 0
		for _, b := range eb {
			e = e<<8 | int(b)
		}
		if e < 3 || e%2 == 0 || e > 1<<31-1 {
			return fmt.Errorf("%w: RSA key %q: exponent %d", ErrUnsupportedKey, k.KeyID, e)
		}
		n := new(big.Int).SetBytes(nb)
		if bits := n.BitLen(); bits < minRSABits || bits > maxRSABits {
			return fmt.Errorf("%w: RSA key %q: %d-bit modulus", ErrUnsupportedKey, k.KeyID, bits)
		}
		k.Key = &rsa.PublicKey{N: n, E: e}
	case "EC":
		var curve elliptic.Curve
		switch k.Curve {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return fmt.Errorf("%w: EC key %q: curve %q", ErrUnsupportedKey, k.KeyID, k.Curve)
		}
		size := (curve.Params().BitSize + 7) / 8
		x, errX := b64.DecodeString(k.X)
		y, errY := b64.DecodeString(k.Y)
		if errX != nil || errY != nil || len(x) != size || len(y) != size {
			return fmt.Errorf("%w: EC key %q: bad coordinates", ErrUnsupportedKey, k.KeyID)
		}
		pt := make([]byte, 0, 1+2*size)
		pt = append(pt, 4)
		pt = append(pt, x...)
		pt = append(pt, y...)
		pub, err := ecdsa.ParseUncompressedPublicKey(curve, pt)
		if err != nil {
			return fmt.Errorf("%w: EC key %q: %v", ErrUnsupportedKey, k.KeyID, err)
		}
		k.Key = pub
	case "OKP":
		if k.Curve != "Ed25519" {
			return fmt.Errorf("%w: OKP key %q: curve %q", ErrUnsupportedKey, k.KeyID, k.Curve)
		}
		x, err := b64.DecodeString(k.X)
		if err != nil || len(x) != ed25519.PublicKeySize {
			return fmt.Errorf("%w: OKP key %q: bad public key", ErrUnsupportedKey, k.KeyID)
		}
		k.Key = ed25519.PublicKey(x)
	default:
		return fmt.Errorf("%w: key type %q", ErrUnsupportedKey, k.KeyType)
	}
	if k.Algorithm != "" && !Algorithm(k.Algorithm).supported() {
		return fmt.Errorf("%w: key %q declares algorithm %q", ErrUnsupportedKey, k.KeyID, k.Algorithm)
	}
	if k.Algorithm != "" && Algorithm(k.Algorithm).keyType() != k.KeyType {
		return fmt.Errorf("%w: key %q: algorithm %q does not fit key type %q", ErrUnsupportedKey, k.KeyID, k.Algorithm, k.KeyType)
	}
	return nil
}

// usableWith reports whether k may verify a signature made with alg: the
// key type and curve must fit the algorithm, a declared "alg" must equal it,
// and "use"/"key_ops" must permit verification.
func (k *JWK) usableWith(alg Algorithm) bool {
	if k == nil || k.Key == nil {
		return false
	}
	if alg.keyType() != k.KeyType {
		return false
	}
	if k.Algorithm != "" && Algorithm(k.Algorithm) != alg {
		return false
	}
	if k.Use != "" && k.Use != "sig" {
		return false
	}
	if len(k.KeyOps) > 0 && !slices.Contains(k.KeyOps, "verify") {
		return false
	}
	switch pub := k.Key.(type) {
	case *ecdsa.PublicKey:
		return pub.Curve == alg.curve()
	case ed25519.PublicKey:
		return alg == EdDSA
	case *rsa.PublicKey:
		return alg.keyType() == "RSA"
	}
	return false
}

// NewJWK builds a public JWK for pub (an *rsa.PublicKey, *ecdsa.PublicKey
// or ed25519.PublicKey, or the matching private key, whose public half is
// used). alg may be empty.
func NewJWK(pub crypto.PublicKey, kid string, alg Algorithm) (*JWK, error) {
	if s, ok := pub.(crypto.Signer); ok {
		pub = s.Public()
	}
	k := &JWK{KeyID: kid, Use: "sig", Algorithm: string(alg)}
	switch p := pub.(type) {
	case *rsa.PublicKey:
		k.KeyType = "RSA"
		k.N = b64.EncodeToString(p.N.Bytes())
		k.E = b64.EncodeToString(big.NewInt(int64(p.E)).Bytes())
	case *ecdsa.PublicKey:
		k.KeyType = "EC"
		k.Curve = p.Curve.Params().Name
		raw, err := p.Bytes()
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnsupportedKey, err)
		}
		size := (len(raw) - 1) / 2
		k.X = b64.EncodeToString(raw[1 : 1+size])
		k.Y = b64.EncodeToString(raw[1+size:])
	case ed25519.PublicKey:
		k.KeyType = "OKP"
		k.Curve = "Ed25519"
		k.X = b64.EncodeToString(p)
	default:
		return nil, fmt.Errorf("%w: %T", ErrUnsupportedKey, pub)
	}
	if err := k.parse(); err != nil {
		return nil, err
	}
	return k, nil
}

// JWKSet is the wire form of a JWK Set document.
type JWKSet struct {
	Keys []*JWK `json:"keys"`
}

// ParseJWKS parses a JWK Set. Keys that cannot be used for verification
// (unknown types, private or symmetric material, weak RSA) are skipped and
// reported in skipped; err is non-nil only when the document itself is
// malformed. Several keys may share a kid (for example one per key type);
// verification tries each compatible one.
func ParseJWKS(data []byte) (keys []*JWK, skipped []error, err error) {
	var doc struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := decodeStrictObject(data, &doc); err != nil {
		return nil, nil, fmt.Errorf("identity: JWKS: %w", err)
	}
	if doc.Keys == nil {
		return nil, nil, errors.New("identity: JWKS: missing \"keys\" array")
	}
	for i, raw := range doc.Keys {
		k, err := ParseJWK(raw)
		if err != nil {
			skipped = append(skipped, fmt.Errorf("key %d: %w", i, err))
			continue
		}
		keys = append(keys, k)
	}
	return keys, skipped, nil
}
