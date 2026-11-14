package vault

import (
	"context"
	"crypto/hkdf"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
	"unsafe"
)

// KDF and key-size parameters.
const (
	// KDFPBKDF2SHA256 names the passphrase KDF recorded in vault.json.
	KDFPBKDF2SHA256 = "pbkdf2-hmac-sha256"
	// DefaultKDFTarget is the wall time CalibratePBKDF2 aims for.
	DefaultKDFTarget = 250 * time.Millisecond
	// MinKDFIterations is the floor for new passphrase vaults unless
	// AllowWeakKDF is set (tests only).
	MinKDFIterations = 210_000
	// MaxKDFIterations bounds the work an attacker-edited vault.json can
	// demand before the keyring authentication fails.
	MaxKDFIterations = 50_000_000

	// MasterKeySize is the size of a master key, key file and KEK.
	MasterKeySize = 32

	kdfSaltSize    = 32
	minKDFSaltSize = 16
	maxKDFSaltSize = 64
)

// HKDF purpose labels. Each derived key has exactly one purpose.
const (
	labelRootSeal  = "gta-vault/v1/root-seal"
	labelDEKWrap   = "gta-vault/v1/dek-wrap/kek-"
	labelKeyring   = "gta-vault/v1/keyring"
	labelObjectID  = "gta-vault/v1/object-id"
	labelSharedID  = "gta-vault/v1/shared-object-id"
	labelSection   = "gta-vault/v1/section"
	labelDEKWrapAD = "gta-vault/v1/dek-wrap"
)

// DeriveKey derives a length-byte key for one purpose from secret using
// HKDF-SHA256 (RFC 5869). Distinct purposes yield independent keys. salt may
// be nil; length must be between 16 and 64 bytes.
func DeriveKey(secret, salt []byte, purpose string, length int) ([]byte, error) {
	if len(secret) < 16 {
		return nil, invalid("HKDF secret must be at least 16 bytes")
	}
	if purpose == "" {
		return nil, invalid("HKDF purpose must not be empty")
	}
	if length < 16 || length > 64 {
		return nil, invalid("HKDF output length %d outside [16, 64]", length)
	}
	return hkdf.Key(sha256.New, secret, salt, purpose, length)
}

// pbkdf2Key derives the master key from a passphrase.
func pbkdf2Key(pass, salt []byte, iterations int) ([]byte, error) {
	if len(pass) == 0 {
		return nil, invalid("empty passphrase")
	}
	if iterations < 1 || iterations > MaxKDFIterations {
		return nil, invalid("PBKDF2 iterations %d outside [1, %d]", iterations, MaxKDFIterations)
	}
	// crypto/pbkdf2 takes the password as a string. Converting would leave
	// an unwipeable copy of the passphrase on the heap, so alias the
	// caller's buffer instead. The string does not escape this call:
	// pbkdf2.Key only feeds it to HMAC and returns before the caller may
	// wipe pass.
	s := unsafe.String(unsafe.SliceData(pass), len(pass))
	return pbkdf2.Key(sha256.New, s, salt, iterations, MasterKeySize)
}

// CalibratePBKDF2 returns the PBKDF2-HMAC-SHA256 iteration count that takes
// about target on this machine, rounded up to a multiple of 1000 and clamped
// to [MinKDFIterations, MaxKDFIterations]. A non-positive target means
// DefaultKDFTarget.
func CalibratePBKDF2(target time.Duration) int {
	if target <= 0 {
		target = DefaultKDFTarget
	}
	salt := make([]byte, kdfSaltSize)
	const minSample = 25 * time.Millisecond
	iter := 4096
	for {
		start := time.Now()
		_, _ = pbkdf2.Key(sha256.New, "gta-vault calibration", salt, iter, MasterKeySize)
		elapsed := time.Since(start)
		if elapsed >= minSample || iter >= MaxKDFIterations {
			if elapsed <= 0 {
				elapsed = time.Nanosecond
			}
			est := float64(iter) * float64(target) / float64(elapsed)
			return clampIterations(est)
		}
		iter *= 4
	}
}

func clampIterations(est float64) int {
	if est < MinKDFIterations {
		return MinKDFIterations
	}
	if est > MaxKDFIterations {
		return MaxKDFIterations
	}
	n := int(est)
	if r := n % 1000; r != 0 {
		n += 1000 - r
	}
	return min(n, MaxKDFIterations)
}

// KeySource supplies the root secret that unlocks a vault. Construct one with
// Passphrase, KeyFile, MasterKey or KMSKey.
type KeySource interface {
	sourceKind() string
}

type passphraseSource struct{ p []byte }
type keyFileSource struct{ path string }
type masterKeySource struct{ k []byte }
type kmsSource struct{ kms KMS }

func (passphraseSource) sourceKind() string { return rootKindPassphrase }
func (keyFileSource) sourceKind() string    { return rootKindKeyFile }
func (masterKeySource) sourceKind() string  { return "master-key" }
func (kmsSource) sourceKind() string        { return rootKindKMS }

// Passphrase returns a source that derives the master key from p with
// PBKDF2-HMAC-SHA256. The vault reads p only during the call that consumes the
// source and never retains it; wipe p afterwards.
func Passphrase(p []byte) KeySource { return passphraseSource{p: p} }

// KeyFile returns a source that reads a 32-byte master key from path. The file
// must be a regular file of exactly 32 bytes with no group or other access.
func KeyFile(path string) KeySource { return keyFileSource{path: path} }

// MasterKey returns a source holding a raw 32-byte master key, typically one
// recovered with CombineShares. It unlocks passphrase and key-file vaults.
// The vault copies what it needs; wipe k afterwards.
func MasterKey(k []byte) KeySource { return masterKeySource{k: k} }

// KMSKey returns a source that seals the keyring with an external KMS.
func KMSKey(k KMS) KeySource { return kmsSource{kms: k} }

// GenerateKeyFile writes a new random 32-byte key file at path with mode 0600.
// It fails with ErrExists if path exists.
func GenerateKeyFile(path string) error {
	key := make([]byte, MasterKeySize)
	defer wipe(key)
	if _, err := rand.Read(key); err != nil {
		return fmt.Errorf("vault: random key: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("%w: %s", ErrExists, path)
		}
		return fmt.Errorf("vault: create key file: %w", err)
	}
	_, werr := f.Write(key)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(path)
		return fmt.Errorf("vault: write key file: %w", werr)
	}
	return syncDir(filepath.Dir(path))
}

// readKeyFile reads and validates a key file.
func readKeyFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("vault: open key file: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("vault: stat key file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, invalid("key file %s is not a regular file", path)
	}
	if err := checkPrivate(path, fi); err != nil {
		return nil, err
	}
	if fi.Size() != MasterKeySize {
		return nil, invalid("key file %s is %d bytes, want %d", path, fi.Size(), MasterKeySize)
	}
	key := make([]byte, MasterKeySize)
	if _, err := io.ReadFull(f, key); err != nil {
		wipe(key)
		return nil, fmt.Errorf("vault: read key file: %w", err)
	}
	var extra [1]byte
	if n, _ := f.Read(extra[:]); n != 0 {
		wipe(key)
		return nil, invalid("key file %s grew while being read", path)
	}
	if isZero(key) {
		return nil, invalid("key file %s is all zeros", path)
	}
	return key, nil
}

// isZero reports in constant time whether b is all zero bytes.
func isZero(b []byte) bool {
	var acc byte
	for _, c := range b {
		acc |= c
	}
	return subtle.ConstantTimeByteEq(acc, 0) == 1
}

// masterFromSource produces the 32-byte master key for a local-root vault.
func masterFromSource(_ context.Context, src KeySource, p *rootParams) ([]byte, error) {
	switch s := src.(type) {
	case masterKeySource:
		if p.Kind != rootKindPassphrase && p.Kind != rootKindKeyFile {
			break
		}
		if len(s.k) != MasterKeySize {
			return nil, invalid("master key must be %d bytes", MasterKeySize)
		}
		return append([]byte(nil), s.k...), nil
	case passphraseSource:
		if p.Kind == rootKindPassphrase {
			return pbkdf2Key(s.p, p.Salt, p.Iterations)
		}
	case keyFileSource:
		if p.Kind == rootKindKeyFile {
			return readKeyFile(s.path)
		}
	}
	return nil, fmt.Errorf("%w: %s source for %s vault", ErrKeySourceMismatch, sourceName(src), p.Kind)
}

func sourceName(src KeySource) string {
	if src == nil {
		return "nil"
	}
	return src.sourceKind()
}
