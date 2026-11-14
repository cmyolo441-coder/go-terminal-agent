package vault

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"sync"
)

// KMS is an external key-management service that wraps and unwraps small
// secrets under a key it never releases. Implementations must authenticate
// aad: Unwrap must fail unless aad equals the value given to Wrap. Unwrap
// should return an error wrapping ErrWrongKey when authentication fails so
// callers can tell a wrong key from an outage.
type KMS interface {
	// KeyID names the KMS key; it is recorded in vault.json and checked on unlock.
	KeyID() string
	// Wrap encrypts plaintext bound to aad.
	Wrap(ctx context.Context, plaintext, aad []byte) ([]byte, error)
	// Unwrap reverses Wrap.
	Unwrap(ctx context.Context, wrapped, aad []byte) ([]byte, error)
}

// MemoryKMS is an in-process KMS for tests and single-machine setups. It keeps
// versioned AES-256-GCM keys; Rotate adds a version used for new wraps while
// old versions keep unwrapping. Wrapped format: version (4) || nonce (12) ||
// ciphertext+tag.
type MemoryKMS struct {
	id string

	mu      sync.RWMutex
	keys    map[uint32][]byte
	current uint32
}

// NewMemoryKMS returns a MemoryKMS with one random key version.
func NewMemoryKMS(keyID string) (*MemoryKMS, error) {
	if keyID == "" {
		return nil, invalid("KMS key id must not be empty")
	}
	m := &MemoryKMS{id: keyID, keys: make(map[uint32][]byte)}
	if _, err := m.Rotate(); err != nil {
		return nil, err
	}
	return m, nil
}

// KeyID implements KMS.
func (m *MemoryKMS) KeyID() string { return m.id }

// Rotate adds a new key version and makes it current.
func (m *MemoryKMS) Rotate() (uint32, error) {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return 0, fmt.Errorf("vault: kms random: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.current++
	m.keys[m.current] = k
	return m.current, nil
}

// Destroy erases a key version; wraps under it can no longer be unwrapped.
func (m *MemoryKMS) Destroy(version uint32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if k, ok := m.keys[version]; ok {
		wipe(k)
		delete(m.keys, version)
	}
}

func (m *MemoryKMS) aad(aad []byte, version uint32) []byte {
	b := make([]byte, 0, len("gta-vault/v1/memory-kms")+len(m.id)+12+len(aad))
	b = append(b, "gta-vault/v1/memory-kms"...)
	b = binary.BigEndian.AppendUint32(b, uint32(len(m.id)))
	b = append(b, m.id...)
	b = binary.BigEndian.AppendUint32(b, version)
	return append(b, aad...)
}

// Wrap implements KMS.
func (m *MemoryKMS) Wrap(ctx context.Context, plaintext, aad []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	v := m.current
	aead, err := newAEAD(m.keys[v])
	if err != nil {
		return nil, err
	}
	out := binary.BigEndian.AppendUint32(make([]byte, 0, 4+nonceSize+len(plaintext)+gcmTagSize), v)
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("vault: kms nonce: %w", err)
	}
	out = append(out, nonce...)
	return aead.Seal(out, nonce, plaintext, m.aad(aad, v)), nil
}

// Unwrap implements KMS.
func (m *MemoryKMS) Unwrap(ctx context.Context, wrapped, aad []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(wrapped) < 4+nonceSize+gcmTagSize {
		return nil, fmt.Errorf("%w: kms ciphertext too short", ErrWrongKey)
	}
	v := binary.BigEndian.Uint32(wrapped)
	m.mu.RLock()
	defer m.mu.RUnlock()
	key, ok := m.keys[v]
	if !ok {
		return nil, fmt.Errorf("%w: kms key version %d unavailable", ErrWrongKey, v)
	}
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	pt, err := aead.Open(nil, wrapped[4:4+nonceSize], wrapped[4+nonceSize:], m.aad(aad, v))
	if err != nil {
		return nil, fmt.Errorf("%w: kms unwrap authentication failed", ErrWrongKey)
	}
	return pt, nil
}
