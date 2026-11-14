package vault

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"time"
)

// Root kinds recorded in vault.json.
const (
	rootKindPassphrase = "passphrase"
	rootKindKeyFile    = "keyfile"
	rootKindKMS        = "kms"

	maxKMSKeyIDLen = 512
)

// rootParams describe how the root secret becomes the keyring sealer. They are
// stored in plaintext and authenticated as keyring AAD.
type rootParams struct {
	Kind       string `json:"kind"`
	KDF        string `json:"kdf,omitempty"`
	Iterations int    `json:"iterations,omitempty"`
	Salt       []byte `json:"salt,omitempty"`
	KMSKeyID   string `json:"kms_key_id,omitempty"`
}

func (p *rootParams) validate() error {
	switch p.Kind {
	case rootKindPassphrase:
		if p.KDF != KDFPBKDF2SHA256 {
			return corrupt("unsupported KDF %q", p.KDF)
		}
		if p.Iterations < 1 || p.Iterations > MaxKDFIterations {
			return corrupt("KDF iterations %d outside [1, %d]", p.Iterations, MaxKDFIterations)
		}
		if len(p.Salt) < minKDFSaltSize || len(p.Salt) > maxKDFSaltSize {
			return corrupt("KDF salt length %d outside [%d, %d]", len(p.Salt), minKDFSaltSize, maxKDFSaltSize)
		}
		if p.KMSKeyID != "" {
			return corrupt("passphrase root has a KMS key id")
		}
	case rootKindKeyFile:
		if p.KDF != "" || p.Iterations != 0 || len(p.Salt) != 0 || p.KMSKeyID != "" {
			return corrupt("key-file root has unexpected parameters")
		}
	case rootKindKMS:
		if p.KMSKeyID == "" || len(p.KMSKeyID) > maxKMSKeyIDLen {
			return corrupt("KMS key id length %d outside [1, %d]", len(p.KMSKeyID), maxKMSKeyIDLen)
		}
		if p.KDF != "" || p.Iterations != 0 || len(p.Salt) != 0 {
			return corrupt("KMS root has unexpected parameters")
		}
	default:
		return corrupt("unknown root kind %q", p.Kind)
	}
	return nil
}

// keyringAAD binds the sealed keyring to the vault id and every root parameter.
func keyringAAD(vaultID []byte, p *rootParams) []byte {
	lp := func(b []byte, s []byte) []byte {
		b = binary.BigEndian.AppendUint32(b, uint32(len(s)))
		return append(b, s...)
	}
	b := make([]byte, 0, 128+len(p.Salt)+len(p.KMSKeyID))
	b = append(b, labelKeyring...)
	b = lp(b, vaultID)
	b = lp(b, []byte(p.Kind))
	b = lp(b, []byte(p.KDF))
	b = binary.BigEndian.AppendUint64(b, uint64(p.Iterations))
	b = lp(b, p.Salt)
	b = lp(b, []byte(p.KMSKeyID))
	return b
}

// KDFOptions control passphrase derivation for new vaults and Rekey.
type KDFOptions struct {
	// Iterations fixes the PBKDF2 iteration count; zero calibrates to Target.
	Iterations int
	// Target is the calibration wall time; zero means DefaultKDFTarget.
	Target time.Duration
	// AllowWeakKDF permits Iterations below MinKDFIterations. Tests only.
	AllowWeakKDF bool
}

// newRootParams chooses root parameters for a new or re-keyed vault.
func newRootParams(src KeySource, o KDFOptions) (*rootParams, error) {
	switch s := src.(type) {
	case passphraseSource:
		if len(s.p) == 0 {
			return nil, invalid("empty passphrase")
		}
		iter := o.Iterations
		if iter == 0 {
			iter = CalibratePBKDF2(o.Target)
		}
		if iter < 1 || iter > MaxKDFIterations {
			return nil, invalid("PBKDF2 iterations %d outside [1, %d]", iter, MaxKDFIterations)
		}
		if iter < MinKDFIterations && !o.AllowWeakKDF {
			return nil, invalid("PBKDF2 iterations %d below minimum %d", iter, MinKDFIterations)
		}
		salt := make([]byte, kdfSaltSize)
		if _, err := rand.Read(salt); err != nil {
			return nil, fmt.Errorf("vault: random salt: %w", err)
		}
		return &rootParams{Kind: rootKindPassphrase, KDF: KDFPBKDF2SHA256, Iterations: iter, Salt: salt}, nil
	case keyFileSource:
		return &rootParams{Kind: rootKindKeyFile}, nil
	case kmsSource:
		if s.kms == nil {
			return nil, invalid("nil KMS")
		}
		id := s.kms.KeyID()
		if id == "" || len(id) > maxKMSKeyIDLen {
			return nil, invalid("KMS key id length %d outside [1, %d]", len(id), maxKMSKeyIDLen)
		}
		return &rootParams{Kind: rootKindKMS, KMSKeyID: id}, nil
	case masterKeySource:
		return nil, invalid("a raw master key can unlock or recover a vault but not define a new root; use a key file")
	default:
		return nil, invalid("nil key source")
	}
}

// rootSealer seals and opens the keyring.
type rootSealer interface {
	seal(ctx context.Context, plaintext, aad []byte) ([]byte, error)
	open(ctx context.Context, sealed, aad []byte) ([]byte, error)
	wipe()
}

// localSealer seals with AES-256-GCM under a key derived from the master key.
// Sealed format: nonce (12) || ciphertext+tag.
type localSealer struct{ key []byte }

func newLocalSealer(master, vaultID []byte) (*localSealer, error) {
	key, err := DeriveKey(master, vaultID, labelRootSeal, 32)
	if err != nil {
		return nil, err
	}
	return &localSealer{key: key}, nil
}

func (s *localSealer) seal(_ context.Context, plaintext, aad []byte) ([]byte, error) {
	aead, err := newAEAD(s.key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, nonceSize, nonceSize+len(plaintext)+gcmTagSize)
	if _, err := rand.Read(out); err != nil {
		return nil, fmt.Errorf("vault: random nonce: %w", err)
	}
	return aead.Seal(out, out[:nonceSize], plaintext, aad), nil
}

func (s *localSealer) open(_ context.Context, sealed, aad []byte) ([]byte, error) {
	if len(sealed) < nonceSize+gcmTagSize {
		return nil, fmt.Errorf("%w: sealed keyring too short", ErrWrongKey)
	}
	aead, err := newAEAD(s.key)
	if err != nil {
		return nil, err
	}
	pt, err := aead.Open(nil, sealed[:nonceSize], sealed[nonceSize:], aad)
	if err != nil {
		return nil, fmt.Errorf("%w: keyring authentication failed", ErrWrongKey)
	}
	return pt, nil
}

func (s *localSealer) wipe() { wipe(s.key) }

// kmsSealer delegates sealing to a KMS.
type kmsSealer struct{ kms KMS }

func (s *kmsSealer) seal(ctx context.Context, plaintext, aad []byte) ([]byte, error) {
	out, err := s.kms.Wrap(ctx, plaintext, aad)
	if err != nil {
		return nil, fmt.Errorf("vault: kms wrap: %w", err)
	}
	return out, nil
}

func (s *kmsSealer) open(ctx context.Context, sealed, aad []byte) ([]byte, error) {
	out, err := s.kms.Unwrap(ctx, sealed, aad)
	if err != nil {
		return nil, fmt.Errorf("vault: kms unwrap: %w", err)
	}
	return out, nil
}

func (s *kmsSealer) wipe() {}

// newSealer turns a key source into the sealer for a vault with params p.
func newSealer(ctx context.Context, src KeySource, p *rootParams, vaultID []byte) (rootSealer, error) {
	if p.Kind == rootKindKMS {
		s, ok := src.(kmsSource)
		if !ok {
			return nil, fmt.Errorf("%w: %s source for kms vault", ErrKeySourceMismatch, sourceName(src))
		}
		if s.kms == nil {
			return nil, invalid("nil KMS")
		}
		if s.kms.KeyID() != p.KMSKeyID {
			return nil, fmt.Errorf("%w: KMS key %q, vault expects %q", ErrKeySourceMismatch, s.kms.KeyID(), p.KMSKeyID)
		}
		return &kmsSealer{kms: s.kms}, nil
	}
	master, err := masterFromSource(ctx, src, p)
	if err != nil {
		return nil, err
	}
	defer wipe(master)
	return newLocalSealer(master, vaultID)
}
