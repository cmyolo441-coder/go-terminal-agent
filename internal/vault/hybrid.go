package vault

import (
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/mlkem"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha3"
	"crypto/subtle"
	"encoding/binary"
	"fmt"
)

// Hybrid KEM: X25519 + ML-KEM-768.
//
// Encapsulation to a recipient (pkX, pkM):
//
//	(ssM, ctM) = ML-KEM-768.Encaps(pkM)
//	(eX, EX)   = fresh X25519 key pair;  ssX = X25519(eX, pkX)
//	T   = SHA3-256(label || len(context) || context || pkX || pkM || EX || ctM)
//	PRK = HKDF-Extract(SHA-256, salt = label, ikm = ssM || ssX)
//	K   = HKDF-Expand(PRK, info = T, 32)
//	encapsulation = EX (32) || ctM (1088)
//
// K stays secret as long as either X25519 or ML-KEM-768 is unbroken: both
// shared secrets feed the extractor, and the transcript binds both
// ciphertexts and both public keys, so neither component can be swapped or
// re-used against another recipient. The caller-supplied context binds the
// derived key to its use (for DEK wrapping, the envelope header fields).
const (
	hybridKEMLabel = "gta-vault/v1/hybrid-kem/X25519+ML-KEM-768"
	hybridFPLabel  = "gta-vault/v1/hybrid-fingerprint"

	hybridPublicMagic  = "GTHP"
	hybridPrivateMagic = "GTHS"
	hybridKeyFormatV1  = 0x01

	x25519Size = 32

	// HybridPublicKeySize is the encoded size of a HybridPublicKey.
	HybridPublicKeySize = 4 + 1 + x25519Size + mlkem.EncapsulationKeySize768
	// HybridPrivateKeySize is the encoded size of a HybridPrivateKey.
	HybridPrivateKeySize = 4 + 1 + x25519Size + mlkem.SeedSize
	// HybridEncapsulationSize is the size of a hybrid encapsulation.
	HybridEncapsulationSize = x25519Size + mlkem.CiphertextSize768
	// HybridFingerprintSize is the size of a public key fingerprint.
	HybridFingerprintSize = 32

	hybridWrappedDEKSize = HybridFingerprintSize + HybridEncapsulationSize + nonceSize + dekSize + gcmTagSize
)

// HybridPublicKey is a recipient's X25519 + ML-KEM-768 public key.
type HybridPublicKey struct {
	x   *ecdh.PublicKey
	m   *mlkem.EncapsulationKey768
	raw []byte
	fp  [HybridFingerprintSize]byte
}

// HybridPrivateKey is the matching decapsulation key.
type HybridPrivateKey struct {
	x   *ecdh.PrivateKey
	m   *mlkem.DecapsulationKey768
	pub *HybridPublicKey
}

// GenerateHybridKey returns a new random hybrid key pair.
func GenerateHybridKey() (*HybridPrivateKey, error) {
	x, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("vault: x25519 keygen: %w", err)
	}
	m, err := mlkem.GenerateKey768()
	if err != nil {
		return nil, fmt.Errorf("vault: ml-kem keygen: %w", err)
	}
	return newHybridPrivateKey(x, m)
}

func newHybridPrivateKey(x *ecdh.PrivateKey, m *mlkem.DecapsulationKey768) (*HybridPrivateKey, error) {
	pub, err := newHybridPublicKey(x.PublicKey(), m.EncapsulationKey())
	if err != nil {
		return nil, err
	}
	return &HybridPrivateKey{x: x, m: m, pub: pub}, nil
}

func newHybridPublicKey(x *ecdh.PublicKey, m *mlkem.EncapsulationKey768) (*HybridPublicKey, error) {
	raw := make([]byte, 0, HybridPublicKeySize)
	raw = append(raw, hybridPublicMagic...)
	raw = append(raw, hybridKeyFormatV1)
	raw = append(raw, x.Bytes()...)
	raw = append(raw, m.Bytes()...)
	if len(raw) != HybridPublicKeySize {
		return nil, invalid("hybrid public key encodes to %d bytes", len(raw))
	}
	h := sha3.New256()
	h.Write([]byte(hybridFPLabel))
	h.Write(raw)
	p := &HybridPublicKey{x: x, m: m, raw: raw}
	h.Sum(p.fp[:0])
	return p, nil
}

// ParseHybridPrivateKey decodes a key produced by HybridPrivateKey.Bytes.
func ParseHybridPrivateKey(b []byte) (*HybridPrivateKey, error) {
	if len(b) != HybridPrivateKeySize || string(b[:4]) != hybridPrivateMagic || b[4] != hybridKeyFormatV1 {
		return nil, corrupt("malformed hybrid private key")
	}
	x, err := ecdh.X25519().NewPrivateKey(b[5 : 5+x25519Size])
	if err != nil {
		return nil, corrupt("hybrid private key: %v", err)
	}
	m, err := mlkem.NewDecapsulationKey768(b[5+x25519Size:])
	if err != nil {
		return nil, corrupt("hybrid private key: %v", err)
	}
	return newHybridPrivateKey(x, m)
}

// Bytes encodes the private key: "GTHS" || 0x01 || X25519 scalar (32) ||
// ML-KEM-768 seed (64). The result is secret; wipe it after storing.
func (k *HybridPrivateKey) Bytes() []byte {
	out := make([]byte, 0, HybridPrivateKeySize)
	out = append(out, hybridPrivateMagic...)
	out = append(out, hybridKeyFormatV1)
	out = append(out, k.x.Bytes()...)
	return append(out, k.m.Bytes()...)
}

// Public returns the public half.
func (k *HybridPrivateKey) Public() *HybridPublicKey { return k.pub }

// ParseHybridPublicKey decodes a key produced by HybridPublicKey.Bytes.
func ParseHybridPublicKey(b []byte) (*HybridPublicKey, error) {
	if len(b) != HybridPublicKeySize || string(b[:4]) != hybridPublicMagic || b[4] != hybridKeyFormatV1 {
		return nil, corrupt("malformed hybrid public key")
	}
	x, err := ecdh.X25519().NewPublicKey(b[5 : 5+x25519Size])
	if err != nil {
		return nil, corrupt("hybrid public key: %v", err)
	}
	m, err := mlkem.NewEncapsulationKey768(b[5+x25519Size:])
	if err != nil {
		return nil, corrupt("hybrid public key: %v", err)
	}
	return newHybridPublicKey(x, m)
}

// Bytes encodes the public key: "GTHP" || 0x01 || X25519 (32) || ML-KEM-768 (1184).
func (p *HybridPublicKey) Bytes() []byte { return append([]byte(nil), p.raw...) }

// Fingerprint is SHA3-256("gta-vault/v1/hybrid-fingerprint" || Bytes()).
func (p *HybridPublicKey) Fingerprint() [HybridFingerprintSize]byte { return p.fp }

// Equal reports whether p and o encode the same key.
func (p *HybridPublicKey) Equal(o *HybridPublicKey) bool {
	return o != nil && subtle.ConstantTimeCompare(p.raw, o.raw) == 1
}

// combineHybrid derives the hybrid shared key; see the comment at the top of
// this file.
func combineHybrid(ssM, ssX, ephX, ctM []byte, to *HybridPublicKey, context []byte) ([]byte, error) {
	t := sha3.New256()
	t.Write([]byte(hybridKEMLabel))
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(context)))
	t.Write(l[:])
	t.Write(context)
	t.Write(to.raw[5:]) // pkX || pkM
	t.Write(ephX)
	t.Write(ctM)
	transcript := t.Sum(nil)

	ikm := make([]byte, 0, len(ssM)+len(ssX))
	ikm = append(ikm, ssM...)
	ikm = append(ikm, ssX...)
	defer wipe(ikm)
	prk, err := hkdf.Extract(sha256.New, ikm, []byte(hybridKEMLabel))
	if err != nil {
		return nil, err
	}
	defer wipe(prk)
	return hkdf.Expand(sha256.New, prk, string(transcript), 32)
}

// HybridEncapsulate derives a fresh 32-byte key shared with the holder of
// to's private key and returns it with the encapsulation to send. context is
// bound into the key and must be presented again on decapsulation.
func HybridEncapsulate(to *HybridPublicKey, context []byte) (key, encapsulation []byte, err error) {
	if to == nil {
		return nil, nil, invalid("nil recipient")
	}
	ssM, ctM := to.m.Encapsulate()
	defer wipe(ssM)
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("vault: x25519 keygen: %w", err)
	}
	ssX, err := eph.ECDH(to.x)
	if err != nil {
		return nil, nil, fmt.Errorf("vault: x25519: %w", err)
	}
	defer wipe(ssX)
	ephX := eph.PublicKey().Bytes()
	key, err = combineHybrid(ssM, ssX, ephX, ctM, to, context)
	if err != nil {
		return nil, nil, err
	}
	enc := make([]byte, 0, HybridEncapsulationSize)
	enc = append(enc, ephX...)
	enc = append(enc, ctM...)
	return key, enc, nil
}

// HybridDecapsulate recovers the key from an encapsulation. A wrong private
// key does not fail here (ML-KEM uses implicit rejection); it yields an
// unrelated key that fails the caller's authentication.
func HybridDecapsulate(k *HybridPrivateKey, encapsulation, context []byte) ([]byte, error) {
	if k == nil {
		return nil, invalid("nil identity")
	}
	if len(encapsulation) != HybridEncapsulationSize {
		return nil, corrupt("hybrid encapsulation is %d bytes, want %d", len(encapsulation), HybridEncapsulationSize)
	}
	ephX, ctM := encapsulation[:x25519Size], encapsulation[x25519Size:]
	ssM, err := k.m.Decapsulate(ctM)
	if err != nil {
		return nil, corrupt("ml-kem decapsulation: %v", err)
	}
	defer wipe(ssM)
	eph, err := ecdh.X25519().NewPublicKey(ephX)
	if err != nil {
		return nil, corrupt("x25519 ephemeral key: %v", err)
	}
	ssX, err := k.x.ECDH(eph)
	if err != nil {
		return nil, corrupt("x25519: %v", err)
	}
	defer wipe(ssX)
	return combineHybrid(ssM, ssX, ephX, ctM, k.pub, context)
}

// hybridWrapDEK wraps dek to a recipient:
// fingerprint || encapsulation || nonce || GCM(K, dek).
func hybridWrapDEK(to *HybridPublicKey, h *header, dek []byte) ([]byte, error) {
	ad := dekWrapAAD(h)
	key, enc, err := HybridEncapsulate(to, ad)
	if err != nil {
		return nil, err
	}
	defer wipe(key)
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, hybridWrappedDEKSize)
	out = append(out, to.fp[:]...)
	out = append(out, enc...)
	nonce := make([]byte, nonceSize)
	if err := randomBytes(nonce); err != nil {
		return nil, err
	}
	out = append(out, nonce...)
	return aead.Seal(out, nonce, dek, ad), nil
}

// hybridUnwrapDEK reverses hybridWrapDEK.
func hybridUnwrapDEK(k *HybridPrivateKey, h *header) ([]byte, error) {
	w := h.wrappedDEK
	if len(w) != hybridWrappedDEKSize {
		return nil, corrupt("hybrid wrapped DEK length %d", len(w))
	}
	if subtle.ConstantTimeCompare(w[:HybridFingerprintSize], k.pub.fp[:]) != 1 {
		return nil, ErrWrongRecipient
	}
	ad := dekWrapAAD(h)
	off := HybridFingerprintSize
	key, err := HybridDecapsulate(k, w[off:off+HybridEncapsulationSize], ad)
	if err != nil {
		return nil, err
	}
	defer wipe(key)
	off += HybridEncapsulationSize
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	dek, err := aead.Open(nil, w[off:off+nonceSize], w[off+nonceSize:], ad)
	if err != nil {
		return nil, corrupt("hybrid DEK unwrap authentication failed")
	}
	return dek, nil
}
