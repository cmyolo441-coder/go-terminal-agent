package identity

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ErrTokenNotFound is returned by TokenStore.Load for an unknown key.
var ErrTokenNotFound = errors.New("identity: token not found")

// TokenStore persists tokens under a caller-chosen key (for example the
// issuer and client ID). Implementations must be safe for concurrent use.
type TokenStore interface {
	Load(ctx context.Context, key string) (*Token, error)
	Save(ctx context.Context, key string, t *Token) error
	Delete(ctx context.Context, key string) error
}

// KeySource supplies the 32-byte master key that encrypts stored tokens.
// Implementations can wrap an OS keychain, a KMS or a hardware token.
type KeySource interface {
	Key(ctx context.Context) ([]byte, error)
}

// MasterKeySize is the length of a KeySource key.
const MasterKeySize = 32

// StaticKeySource is a KeySource over a fixed key.
type StaticKeySource struct{ key []byte }

// NewStaticKeySource copies key, which must be MasterKeySize bytes.
func NewStaticKeySource(key []byte) (*StaticKeySource, error) {
	if len(key) != MasterKeySize {
		return nil, fmt.Errorf("identity: master key must be %d bytes, got %d", MasterKeySize, len(key))
	}
	return &StaticKeySource{key: append([]byte(nil), key...)}, nil
}

// Key implements KeySource.
func (s *StaticKeySource) Key(context.Context) ([]byte, error) {
	return append([]byte(nil), s.key...), nil
}

// EnvKeySource reads a base64-encoded key from an environment variable.
type EnvKeySource struct{ Name string }

// Key implements KeySource.
func (s EnvKeySource) Key(context.Context) ([]byte, error) {
	v, ok := os.LookupEnv(s.Name)
	if !ok || strings.TrimSpace(v) == "" {
		return nil, fmt.Errorf("identity: environment variable %s is not set", s.Name)
	}
	return decodeMasterKey([]byte(v))
}

// FileKeySource reads the key from a file holding either 32 raw bytes or
// their base64 encoding. With Create set, a missing file is created with a
// random key (mode 0600). On Unix a key file readable by group or others is
// refused.
type FileKeySource struct {
	Path   string
	Create bool

	mu sync.Mutex
}

// Key implements KeySource.
func (s *FileKeySource) Key(context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := readPrivateFile(s.Path, 4096)
	if errors.Is(err, fs.ErrNotExist) && s.Create {
		key := make([]byte, MasterKeySize)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		enc := []byte(base64.StdEncoding.EncodeToString(key) + "\n")
		if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
			return nil, err
		}
		if err := writeFileAtomic(s.Path, enc, true); err != nil {
			return nil, err
		}
		return key, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeMasterKey(data)
}

func decodeMasterKey(data []byte) ([]byte, error) {
	if len(data) == MasterKeySize {
		return append([]byte(nil), data...), nil
	}
	s := strings.TrimSpace(string(data))
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if k, err := enc.DecodeString(s); err == nil && len(k) == MasterKeySize {
			return k, nil
		}
	}
	return nil, fmt.Errorf("identity: master key must be %d bytes (raw or base64)", MasterKeySize)
}

// readPrivateFile reads a file of at most limit bytes and, on Unix, refuses
// it when group or others can access it.
func readPrivateFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("identity: %s is not a regular file", path)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("identity: %s has insecure permissions %v (want 0600)", path, fi.Mode().Perm())
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("identity: %s exceeds %d bytes", path, limit)
	}
	return data, nil
}

// writeFileAtomic writes data to path via a temporary file in the same
// directory, fsyncs it, sets mode 0600 and renames it into place, so
// readers see either the old or the new content. With exclusive set an
// existing file is not replaced.
func writeFileAtomic(path string, data []byte, exclusive bool) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		tmp.Close()
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if exclusive {
		// Link fails if path exists, giving create-if-absent semantics.
		if err := os.Link(tmpName, path); err != nil {
			cleanup()
			return err
		}
		cleanup()
	} else if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// FileTokenStore stores each token in its own file under Dir, encrypted
// with AES-256-GCM. The file name is a hash of the key (so keys cannot
// traverse paths), and the key is bound into the ciphertext as additional
// authenticated data, so a file copied under another name fails to decrypt.
type FileTokenStore struct {
	dir  string
	keys KeySource
	mu   sync.Mutex
}

const (
	tokenFileVersion = 1
	tokenFileMax     = 1 << 20
	tokenKDFInfo     = "gta identity token store v1"
)

type tokenEnvelope struct {
	Version int    `json:"v"`
	Alg     string `json:"alg"`
	Nonce   string `json:"nonce"`
	Data    string `json:"ct"`
}

// NewFileTokenStore returns a store in dir (created 0700 if missing).
func NewFileTokenStore(dir string, keys KeySource) (*FileTokenStore, error) {
	if dir == "" {
		return nil, errors.New("identity: token store directory is required")
	}
	if keys == nil {
		return nil, errors.New("identity: token store needs a KeySource")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &FileTokenStore{dir: dir, keys: keys}, nil
}

func (s *FileTokenStore) path(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(s.dir, hex.EncodeToString(sum[:16])+".token")
}

func (s *FileTokenStore) aead(ctx context.Context) (cipher.AEAD, error) {
	master, err := s.keys.Key(ctx)
	if err != nil {
		return nil, fmt.Errorf("identity: token store key: %w", err)
	}
	if len(master) != MasterKeySize {
		return nil, fmt.Errorf("identity: token store key must be %d bytes", MasterKeySize)
	}
	k, err := hkdf.Key(sha256.New, master, nil, tokenKDFInfo, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func tokenAAD(key string) []byte { return []byte("gta-token-v1\x00" + key) }

// Save implements TokenStore.
func (s *FileTokenStore) Save(ctx context.Context, key string, t *Token) error {
	if t == nil {
		return errors.New("identity: Save: nil token")
	}
	plain, err := json.Marshal(t)
	if err != nil {
		return err
	}
	aead, err := s.aead(ctx)
	if err != nil {
		return err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	ct := aead.Seal(nil, nonce, plain, tokenAAD(key))
	clear(plain)
	env, err := json.Marshal(tokenEnvelope{
		Version: tokenFileVersion, Alg: "A256GCM",
		Nonce: base64.StdEncoding.EncodeToString(nonce), Data: base64.StdEncoding.EncodeToString(ct),
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeFileAtomic(s.path(key), env, false)
}

// Load implements TokenStore.
func (s *FileTokenStore) Load(ctx context.Context, key string) (*Token, error) {
	s.mu.Lock()
	data, err := readPrivateFile(s.path(key), tokenFileMax)
	s.mu.Unlock()
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrTokenNotFound
	}
	if err != nil {
		return nil, err
	}
	var env tokenEnvelope
	if err := decodeStrictObject(data, &env); err != nil {
		return nil, fmt.Errorf("identity: token file: %w", err)
	}
	if env.Version != tokenFileVersion || env.Alg != "A256GCM" {
		return nil, fmt.Errorf("identity: token file: unsupported version %d/%q", env.Version, env.Alg)
	}
	nonce, err1 := base64.StdEncoding.DecodeString(env.Nonce)
	ct, err2 := base64.StdEncoding.DecodeString(env.Data)
	if err1 != nil || err2 != nil {
		return nil, errors.New("identity: token file: bad encoding")
	}
	aead, err := s.aead(ctx)
	if err != nil {
		return nil, err
	}
	if len(nonce) != aead.NonceSize() {
		return nil, errors.New("identity: token file: bad nonce")
	}
	plain, err := aead.Open(nil, nonce, ct, tokenAAD(key))
	if err != nil {
		return nil, errors.New("identity: token file: decryption failed (wrong key or tampered file)")
	}
	defer clear(plain)
	var t Token
	if err := json.Unmarshal(plain, &t); err != nil {
		return nil, fmt.Errorf("identity: token file: %w", err)
	}
	return &t, nil
}

// Delete implements TokenStore. Deleting a missing token is not an error.
func (s *FileTokenStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path(key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// MemoryTokenStore is an in-process TokenStore.
type MemoryTokenStore struct {
	mu sync.Mutex
	m  map[string]Token
}

// NewMemoryTokenStore returns an empty store.
func NewMemoryTokenStore() *MemoryTokenStore { return &MemoryTokenStore{m: map[string]Token{}} }

// Load implements TokenStore.
func (s *MemoryTokenStore) Load(_ context.Context, key string) (*Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.m[key]
	if !ok {
		return nil, ErrTokenNotFound
	}
	return &t, nil
}

// Save implements TokenStore.
func (s *MemoryTokenStore) Save(_ context.Context, key string, t *Token) error {
	if t == nil {
		return errors.New("identity: Save: nil token")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = *t
	return nil
}

// Delete implements TokenStore.
func (s *MemoryTokenStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, key)
	return nil
}

// StoreKey is the conventional TokenStore key for an issuer and client.
func StoreKey(issuer, clientID string) string { return issuer + "\x00" + clientID }

// TokenSource returns valid tokens for one stored login, refreshing and
// re-saving them shortly before expiry. Concurrent callers share one
// refresh.
type TokenSource struct {
	client *Client
	store  TokenStore
	key    string
	skew   time.Duration

	mu sync.Mutex
}

// ErrLoginRequired means no usable token is stored and the user must sign
// in again (for example with "gta login").
var ErrLoginRequired = errors.New("identity: login required")

// TokenSource returns a TokenSource for this client's login in store.
func (c *Client) TokenSource(store TokenStore) *TokenSource {
	return &TokenSource{client: c, store: store, key: StoreKey(c.meta.Issuer, c.cfg.ClientID), skew: time.Minute}
}

// StoreKey is the key this client's tokens are stored under.
func (c *Client) StoreKey() string { return StoreKey(c.meta.Issuer, c.cfg.ClientID) }

// Token returns a non-expired token, refreshing it if necessary. It returns
// ErrLoginRequired when nothing usable is stored or the refresh token was
// rejected (the stale token is then deleted).
func (ts *TokenSource) Token(ctx context.Context) (*Token, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	t, err := ts.store.Load(ctx, ts.key)
	if errors.Is(err, ErrTokenNotFound) {
		return nil, ErrLoginRequired
	}
	if err != nil {
		return nil, err
	}
	now := ts.client.now()
	if !t.Expired(now, ts.skew) {
		return t, nil
	}
	if t.RefreshToken == "" {
		return nil, ErrLoginRequired
	}
	nt, err := ts.client.Refresh(ctx, t)
	if err != nil {
		var oe *OAuthError
		if errors.As(err, &oe) && oe.Code == "invalid_grant" {
			_ = ts.store.Delete(ctx, ts.key)
			return nil, fmt.Errorf("%w: %v", ErrLoginRequired, err)
		}
		return nil, err
	}
	if err := ts.store.Save(ctx, ts.key, nt); err != nil {
		return nil, err
	}
	return nt, nil
}
