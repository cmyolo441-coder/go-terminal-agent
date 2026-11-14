package vault

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strings"
)

// Shamir secret sharing over GF(2^8) with the AES reduction polynomial
// x^8 + x^4 + x^3 + x + 1 (0x11b), applied independently to every byte of the
// secret.
//
// Constant time: multiplication is a fixed eight-round shift-and-add with
// masks instead of branches, and inversion is a fixed addition chain for
// a^254, so neither depends on secret values for timing or memory access.
// Log/exp tables would be ~8x faster but index memory by secret bytes, which
// leaks through the cache; secrets here are tens of bytes, so the slower
// arithmetic costs microseconds. Share x-coordinates are public, so the
// Lagrange coefficients (computed from x only) need no protection.
//
// Integrity: every share carries a checksum over its encoding (detects
// corruption and typos) and a set-wide commitment HMAC-SHA256(secret, label
// || set id || threshold || length). After reconstruction the commitment is
// verified, and any shares beyond the threshold must lie on the same
// polynomial, so substituted or mixed-up shares are detected. The commitment
// is only computationally hiding: k-1 shares reveal nothing about the secret
// information-theoretically, but together with the commitment they allow
// checking a guess of the secret, which is infeasible for a random 256-bit
// master key. For a passphrase-derived master key the commitment is no better
// an oracle than vault.json itself.
//
// Encoding (share format version 1):
//
//	"GTSS" || 0x01 || threshold (1) || x (1) || set id (16) || length (2) || y (length) ||
//	commitment (32) || checksum (16) where checksum = SHA-256(label || all preceding bytes)[:16]
const (
	shareMagic        = "GTSS"
	shareFormatV1     = 0x01
	shareSetIDSize    = 16
	shareCommitSize   = 32
	shareChecksumSize = 16
	shareOverhead     = 4 + 1 + 1 + 1 + shareSetIDSize + 2 + shareCommitSize + shareChecksumSize
	shareTextPrefix   = "gtass1-"

	labelShareCommit   = "gta-vault/v1/shamir/commit"
	labelShareChecksum = "gta-vault/v1/shamir/checksum"

	// MaxShareSecretSize bounds the secret length accepted by SplitSecret.
	MaxShareSecretSize = 1024
)

// Share is one Shamir share. Y is secret material.
type Share struct {
	SetID      [shareSetIDSize]byte
	Threshold  uint8
	X          uint8
	Y          []byte
	Commitment [shareCommitSize]byte
}

// String redacts the share so it is safe to log.
func (s Share) String() string {
	return fmt.Sprintf("vault.Share{set=%x, x=%d, k=%d, len=%d}", s.SetID[:4], s.X, s.Threshold, len(s.Y))
}

// GoString redacts the share for %#v.
func (s Share) GoString() string { return s.String() }

// gfMul multiplies in GF(2^8) without secret-dependent branches or lookups.
func gfMul(a, b byte) byte {
	var p byte
	for range 8 {
		p ^= -(b & 1) & a
		hi := -(a >> 7)
		a = (a << 1) ^ (hi & 0x1b)
		b >>= 1
	}
	return p
}

// gfInv returns a^254 = a^-1 (and 0 for 0) with a fixed addition chain.
func gfInv(a byte) byte {
	a2 := gfMul(a, a)
	a3 := gfMul(a2, a)
	a6 := gfMul(a3, a3)
	a12 := gfMul(a6, a6)
	a15 := gfMul(a12, a3)
	a30 := gfMul(a15, a15)
	a60 := gfMul(a30, a30)
	a63 := gfMul(a60, a3)
	a126 := gfMul(a63, a63)
	a127 := gfMul(a126, a)
	return gfMul(a127, a127)
}

func shareCommitment(secret []byte, setID [shareSetIDSize]byte, k uint8) [shareCommitSize]byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(labelShareCommit))
	m.Write(setID[:])
	m.Write([]byte{k})
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], uint16(len(secret)))
	m.Write(l[:])
	var c [shareCommitSize]byte
	m.Sum(c[:0])
	return c
}

// SplitSecret splits secret into n shares, any threshold of which reconstruct
// it. 2 <= threshold <= n <= 255 and 1 <= len(secret) <= MaxShareSecretSize.
func SplitSecret(secret []byte, threshold, n int) ([]Share, error) {
	if len(secret) == 0 || len(secret) > MaxShareSecretSize {
		return nil, invalid("secret length %d outside [1, %d]", len(secret), MaxShareSecretSize)
	}
	if threshold < 2 || threshold > n || n > 255 {
		return nil, invalid("need 2 <= threshold (%d) <= shares (%d) <= 255", threshold, n)
	}
	var setID [shareSetIDSize]byte
	if err := randomBytes(setID[:]); err != nil {
		return nil, err
	}
	k := uint8(threshold)
	commit := shareCommitment(secret, setID, k)

	// coeffs[i*(k-1)+j] is coefficient j+1 of the polynomial for byte i.
	deg := threshold - 1
	coeffs := make([]byte, len(secret)*deg)
	defer wipe(coeffs)
	if err := randomBytes(coeffs); err != nil {
		return nil, err
	}
	shares := make([]Share, n)
	for s := range n {
		x := byte(s + 1)
		y := make([]byte, len(secret))
		for i, c0 := range secret {
			// Horner: ((a_deg x + a_{deg-1}) x + ...) x + a0.
			poly := coeffs[i*deg : (i+1)*deg]
			var acc byte
			for j := deg - 1; j >= 0; j-- {
				acc = gfMul(acc, x) ^ poly[j]
			}
			y[i] = gfMul(acc, x) ^ c0
		}
		shares[s] = Share{SetID: setID, Threshold: k, X: x, Y: y, Commitment: commit}
	}
	return shares, nil
}

// lagrangeAt returns the Lagrange basis coefficients at point t for xs.
func lagrangeAt(xs []byte, t byte) []byte {
	l := make([]byte, len(xs))
	for i, xi := range xs {
		num, den := byte(1), byte(1)
		for j, xj := range xs {
			if i == j {
				continue
			}
			num = gfMul(num, t^xj)
			den = gfMul(den, xi^xj)
		}
		l[i] = gfMul(num, gfInv(den))
	}
	return l
}

func interpolate(xs []byte, ys [][]byte, t byte, out []byte) {
	l := lagrangeAt(xs, t)
	clear(out)
	for i := range xs {
		for b := range out {
			out[b] ^= gfMul(l[i], ys[i][b])
		}
	}
}

// CombineShares reconstructs the secret from at least threshold shares of one
// set. Shares beyond the threshold are checked for consistency.
func CombineShares(shares []Share) ([]byte, error) {
	if len(shares) == 0 {
		return nil, ErrNotEnoughShares
	}
	ref := shares[0]
	if ref.Threshold < 2 || len(ref.Y) == 0 || len(ref.Y) > MaxShareSecretSize {
		return nil, fmt.Errorf("%w: malformed share", ErrShareIntegrity)
	}
	seen := make(map[byte]int, len(shares))
	var uniq []Share
	for i, s := range shares {
		if s.SetID != ref.SetID || s.Threshold != ref.Threshold || len(s.Y) != len(ref.Y) ||
			subtle.ConstantTimeCompare(s.Commitment[:], ref.Commitment[:]) != 1 {
			return nil, fmt.Errorf("%w: share %d belongs to a different set", ErrShareIntegrity, i)
		}
		if s.X == 0 {
			return nil, fmt.Errorf("%w: share %d has x = 0", ErrShareIntegrity, i)
		}
		if j, dup := seen[s.X]; dup {
			if subtle.ConstantTimeCompare(s.Y, shares[j].Y) != 1 {
				return nil, fmt.Errorf("%w: conflicting shares for x = %d", ErrShareIntegrity, s.X)
			}
			continue
		}
		seen[s.X] = i
		uniq = append(uniq, s)
	}
	k := int(ref.Threshold)
	if len(uniq) < k {
		return nil, fmt.Errorf("%w: have %d distinct, need %d", ErrNotEnoughShares, len(uniq), k)
	}
	xs := make([]byte, k)
	ys := make([][]byte, k)
	for i := range k {
		xs[i], ys[i] = uniq[i].X, uniq[i].Y
	}
	secret := make([]byte, len(ref.Y))
	interpolate(xs, ys, 0, secret)

	want := shareCommitment(secret, ref.SetID, ref.Threshold)
	if subtle.ConstantTimeCompare(want[:], ref.Commitment[:]) != 1 {
		wipe(secret)
		return nil, fmt.Errorf("%w: reconstructed secret does not match commitment", ErrShareIntegrity)
	}
	if len(uniq) > k {
		check := make([]byte, len(secret))
		defer wipe(check)
		for _, extra := range uniq[k:] {
			interpolate(xs, ys, extra.X, check)
			if subtle.ConstantTimeCompare(check, extra.Y) != 1 {
				wipe(secret)
				return nil, fmt.Errorf("%w: share x = %d is inconsistent", ErrShareIntegrity, extra.X)
			}
		}
	}
	return secret, nil
}

// MarshalBinary encodes the share with its checksum.
func (s Share) MarshalBinary() ([]byte, error) {
	if len(s.Y) == 0 || len(s.Y) > MaxShareSecretSize {
		return nil, invalid("share length %d", len(s.Y))
	}
	b := make([]byte, 0, shareOverhead+len(s.Y))
	b = append(b, shareMagic...)
	b = append(b, shareFormatV1, s.Threshold, s.X)
	b = append(b, s.SetID[:]...)
	b = binary.BigEndian.AppendUint16(b, uint16(len(s.Y)))
	b = append(b, s.Y...)
	b = append(b, s.Commitment[:]...)
	sum := shareChecksum(b)
	return append(b, sum[:shareChecksumSize]...), nil
}

func shareChecksum(b []byte) [32]byte {
	h := sha256.New()
	h.Write([]byte(labelShareChecksum))
	h.Write(b)
	var out [32]byte
	h.Sum(out[:0])
	return out
}

// ParseShare decodes and checks a share produced by MarshalBinary.
func ParseShare(b []byte) (Share, error) {
	var s Share
	if len(b) < shareOverhead+1 || string(b[:4]) != shareMagic || b[4] != shareFormatV1 {
		return s, fmt.Errorf("%w: malformed share", ErrShareIntegrity)
	}
	n := int(binary.BigEndian.Uint16(b[7+shareSetIDSize:]))
	if n == 0 || n > MaxShareSecretSize || len(b) != shareOverhead+n {
		return s, fmt.Errorf("%w: share length mismatch", ErrShareIntegrity)
	}
	body := b[:len(b)-shareChecksumSize]
	sum := shareChecksum(body)
	if subtle.ConstantTimeCompare(sum[:shareChecksumSize], b[len(body):]) != 1 {
		return s, fmt.Errorf("%w: checksum mismatch", ErrShareIntegrity)
	}
	s.Threshold, s.X = b[5], b[6]
	if s.Threshold < 2 || s.X == 0 {
		return s, fmt.Errorf("%w: invalid threshold or x", ErrShareIntegrity)
	}
	copy(s.SetID[:], b[7:7+shareSetIDSize])
	off := 7 + shareSetIDSize + 2
	s.Y = append([]byte(nil), b[off:off+n]...)
	copy(s.Commitment[:], b[off+n:])
	return s, nil
}

// Encode returns a copy-pasteable text form: "gtass1-" + base64url(binary).
func (s Share) Encode() (string, error) {
	b, err := s.MarshalBinary()
	if err != nil {
		return "", err
	}
	defer wipe(b)
	return shareTextPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// ParseShareString decodes the text form produced by Encode. Surrounding
// whitespace is ignored.
func ParseShareString(text string) (Share, error) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, shareTextPrefix) {
		return Share{}, fmt.Errorf("%w: missing %q prefix", ErrShareIntegrity, shareTextPrefix)
	}
	b, err := base64.RawURLEncoding.DecodeString(text[len(shareTextPrefix):])
	if err != nil {
		return Share{}, fmt.Errorf("%w: %v", ErrShareIntegrity, err)
	}
	defer wipe(b)
	return ParseShare(b)
}
