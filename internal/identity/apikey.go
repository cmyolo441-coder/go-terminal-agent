package identity

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// API key errors.
var (
	ErrInvalidAPIKey  = errors.New("identity: invalid API key")
	ErrAPIKeyExpired  = errors.New("identity: API key expired")
	ErrAPIKeyDisabled = errors.New("identity: API key disabled")
)

// API keys look like "gta_<16 hex id>_<43 char secret>". The ID allows an
// O(1) lookup; the whole key is verified against a stored SHA-256 hash with
// a constant-time comparison. A fast hash is appropriate because the secret
// carries 256 bits of entropy: unlike passwords it cannot be brute-forced.
const (
	APIKeyPrefix   = "gta_"
	apiKeyIDLen    = 16
	apiKeySecretSz = 32
)

var apiKeyIDRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

// GenerateAPIKey returns a new API key, its ID and the hex SHA-256 hash to
// store. Show the key to its owner once; store only the hash.
func GenerateAPIKey() (key, id, hash string, err error) {
	idb := make([]byte, apiKeyIDLen/2)
	sec := make([]byte, apiKeySecretSz)
	if _, err := rand.Read(idb); err != nil {
		return "", "", "", err
	}
	if _, err := rand.Read(sec); err != nil {
		return "", "", "", err
	}
	id = hex.EncodeToString(idb)
	key = APIKeyPrefix + id + "_" + b64.EncodeToString(sec)
	return key, id, HashAPIKey(key), nil
}

// HashAPIKey returns the hex SHA-256 of key.
func HashAPIKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// ParseAPIKeyID extracts the key ID from an API key, validating its shape.
func ParseAPIKeyID(key string) (string, error) {
	if !strings.HasPrefix(key, APIKeyPrefix) || len(key) != len(APIKeyPrefix)+apiKeyIDLen+1+43 {
		return "", ErrInvalidAPIKey
	}
	id := key[len(APIKeyPrefix) : len(APIKeyPrefix)+apiKeyIDLen]
	if !apiKeyIDRe.MatchString(id) || key[len(APIKeyPrefix)+apiKeyIDLen] != '_' {
		return "", ErrInvalidAPIKey
	}
	if _, err := b64.DecodeString(key[len(APIKeyPrefix)+apiKeyIDLen+1:]); err != nil {
		return "", ErrInvalidAPIKey
	}
	return id, nil
}

// LooksLikeAPIKey reports whether s has the API key prefix.
func LooksLikeAPIKey(s string) bool { return strings.HasPrefix(s, APIKeyPrefix) }

// APIKeyRecord is the stored form of an API key: never the key itself.
type APIKeyRecord struct {
	ID string `json:"id"`
	// Hash is the hex SHA-256 of the full key.
	Hash string `json:"hash"`
	// Principal is the service account (or user) the key acts as.
	Principal string        `json:"principal"`
	Type      PrincipalType `json:"type,omitempty"`
	Name      string        `json:"name,omitempty"`
	Roles     []string      `json:"roles,omitempty"`
	// Scopes, when set, cap the key's permissions (patterns like
	// "repo:read" or "tools:browser_*"). Omit for no cap.
	Scopes     []string          `json:"scopes,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
	CreatedAt  time.Time         `json:"created_at,omitzero"`
	ExpiresAt  time.Time         `json:"expires_at,omitzero"`
	Disabled   bool              `json:"disabled,omitempty"`
}

func (r *APIKeyRecord) validate() error {
	if !apiKeyIDRe.MatchString(r.ID) {
		return fmt.Errorf("id %q must be 16 lowercase hex characters", r.ID)
	}
	if h, err := hex.DecodeString(r.Hash); err != nil || len(h) != sha256.Size {
		return fmt.Errorf("key %s: hash must be 64 hex characters", r.ID)
	}
	if r.Principal == "" {
		return fmt.Errorf("key %s: principal is required", r.ID)
	}
	switch r.Type {
	case "", PrincipalServiceAccount, PrincipalUser:
	default:
		return fmt.Errorf("key %s: unknown type %q", r.ID, r.Type)
	}
	for _, s := range r.Scopes {
		if _, err := parsePermissionPattern(s); err != nil {
			return fmt.Errorf("key %s: scope: %w", r.ID, err)
		}
	}
	return nil
}

// APIKeyStore looks up API key records by ID. It returns (nil, nil) for an
// unknown ID.
type APIKeyStore interface {
	LookupAPIKey(ctx context.Context, id string) (*APIKeyRecord, error)
}

// MemoryAPIKeyStore is an APIKeyStore over records held in memory.
type MemoryAPIKeyStore struct {
	mu sync.RWMutex
	m  map[string]APIKeyRecord
}

// NewMemoryAPIKeyStore validates records and returns a store over them.
func NewMemoryAPIKeyStore(records ...APIKeyRecord) (*MemoryAPIKeyStore, error) {
	s := &MemoryAPIKeyStore{m: make(map[string]APIKeyRecord, len(records))}
	for _, r := range records {
		if err := s.Put(r); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Put adds or replaces a record.
func (s *MemoryAPIKeyStore) Put(r APIKeyRecord) error {
	if err := r.validate(); err != nil {
		return fmt.Errorf("identity: API key: %w", err)
	}
	r.Hash = strings.ToLower(r.Hash)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[r.ID] = r
	return nil
}

// Remove deletes a record (revocation).
func (s *MemoryAPIKeyStore) Remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
}

// LookupAPIKey implements APIKeyStore.
func (s *MemoryAPIKeyStore) LookupAPIKey(_ context.Context, id string) (*APIKeyRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.m[id]
	if !ok {
		return nil, nil
	}
	r.Roles = slices.Clone(r.Roles)
	r.Scopes = slices.Clone(r.Scopes)
	return &r, nil
}

// LoadAPIKeysFile reads a JSON file {"keys": [APIKeyRecord...]} strictly
// (unknown fields and trailing data are errors) into a store. Duplicate IDs
// are rejected.
func LoadAPIKeysFile(path string) (*MemoryAPIKeyStore, error) {
	data, err := readPrivateFile(path, 16<<20)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Keys []APIKeyRecord `json:"keys"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("identity: %s: %w", path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("identity: %s: trailing data", path)
	}
	seen := map[string]bool{}
	for _, r := range doc.Keys {
		if seen[r.ID] {
			return nil, fmt.Errorf("identity: %s: duplicate key id %q", path, r.ID)
		}
		seen[r.ID] = true
	}
	s, err := NewMemoryAPIKeyStore(doc.Keys...)
	if err != nil {
		return nil, fmt.Errorf("identity: %s: %w", path, err)
	}
	return s, nil
}

// APIKeyVerifier authenticates API keys against an APIKeyStore.
type APIKeyVerifier struct {
	store APIKeyStore
	now   func() time.Time
}

// NewAPIKeyVerifier returns a verifier over store.
func NewAPIKeyVerifier(store APIKeyStore) (*APIKeyVerifier, error) {
	if store == nil {
		return nil, errors.New("identity: NewAPIKeyVerifier: nil store")
	}
	return &APIKeyVerifier{store: store, now: time.Now}, nil
}

// dummyHash is compared against when the key ID is unknown so that the
// response time does not reveal whether an ID exists.
var dummyHash = sha256.Sum256([]byte("gta-api-key-dummy"))

// Verify authenticates key and returns its principal.
func (v *APIKeyVerifier) Verify(ctx context.Context, key string) (*Principal, error) {
	id, err := ParseAPIKeyID(key)
	if err != nil {
		return nil, err
	}
	rec, err := v.store.LookupAPIKey(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("identity: API key lookup: %w", err)
	}
	got := sha256.Sum256([]byte(key))
	want := dummyHash[:]
	if rec != nil {
		if h, err := hex.DecodeString(rec.Hash); err == nil && len(h) == sha256.Size {
			want = h
		}
	}
	if subtle.ConstantTimeCompare(got[:], want) != 1 || rec == nil {
		return nil, ErrInvalidAPIKey
	}
	if rec.Disabled {
		return nil, ErrAPIKeyDisabled
	}
	if !rec.ExpiresAt.IsZero() && !v.now().Before(rec.ExpiresAt) {
		return nil, ErrAPIKeyExpired
	}
	typ := rec.Type
	if typ == "" {
		typ = PrincipalServiceAccount
	}
	p := &Principal{
		Type: typ, Subject: rec.Principal, Name: rec.Name,
		Roles: slices.Clone(rec.Roles), AuthMethod: "api_key", CredentialID: rec.ID,
		ExpiresAt: rec.ExpiresAt,
	}
	if rec.Scopes != nil {
		p.Scopes = slices.Clone(rec.Scopes)
	}
	if len(rec.Attributes) > 0 {
		p.Attributes = make(map[string]string, len(rec.Attributes))
		for k, val := range rec.Attributes {
			p.Attributes[k] = val
		}
	}
	return p, nil
}
