package fleet

import (
	"container/heap"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Request authentication headers.
const (
	HeaderKeyID     = "X-Fleet-Key"
	HeaderTimestamp = "X-Fleet-Timestamp"
	HeaderNonce     = "X-Fleet-Nonce"
	HeaderSignature = "X-Fleet-Signature"
	HeaderLeader    = "X-Fleet-Leader"

	signatureScheme = "FLEET-HMAC-SHA256"
	minSecretBytes  = 32
)

// DefaultMaxSkew is how far a request timestamp may be from the server's
// clock.
const DefaultMaxSkew = 5 * time.Minute

// Role scopes what a key may do.
type Role string

// Roles. Admin implies every other role.
const (
	RoleWorker    Role = "worker"    // lease, heartbeat, acknowledge
	RoleSubmitter Role = "submitter" // submit, cancel, read own tenant's tasks
	RoleReader    Role = "reader"    // read tasks, workers, metrics
	RoleAdmin     Role = "admin"     // everything, including DLQ redrive and drain
)

// Key is a shared secret held by a client and the coordinators.
type Key struct {
	ID     string
	Secret []byte
	Roles  []Role
	// Tenant, if set, confines the key to one tenant: submissions are
	// forced into it and other tenants' tasks are invisible.
	Tenant string
}

// Principal is an authenticated caller.
type Principal struct {
	KeyID  string
	Roles  []Role
	Tenant string
}

// Has reports whether p holds role r (admins hold every role).
func (p Principal) Has(r Role) bool {
	return slices.Contains(p.Roles, r) || slices.Contains(p.Roles, RoleAdmin)
}

// Keyring holds the keys a server accepts; several keys may be live at
// once for rotation.
type Keyring struct {
	mu   sync.RWMutex
	keys map[string]Key
}

// NewKeyring returns a keyring holding keys.
func NewKeyring(keys ...Key) (*Keyring, error) {
	k := &Keyring{keys: map[string]Key{}}
	for _, key := range keys {
		if err := k.Add(key); err != nil {
			return nil, err
		}
	}
	return k, nil
}

// Add installs or replaces a key.
func (k *Keyring) Add(key Key) error {
	if !validID(key.ID) {
		return fmt.Errorf("%w: key id %q", ErrInvalid, key.ID)
	}
	if len(key.Secret) < minSecretBytes {
		return fmt.Errorf("%w: key %s secret shorter than %d bytes", ErrInvalid, key.ID, minSecretBytes)
	}
	if key.Tenant != "" && !validTenant(key.Tenant) {
		return fmt.Errorf("%w: key %s tenant %q", ErrInvalid, key.ID, key.Tenant)
	}
	key.Secret = slices.Clone(key.Secret)
	key.Roles = slices.Clone(key.Roles)
	k.mu.Lock()
	k.keys[key.ID] = key
	k.mu.Unlock()
	return nil
}

// Remove revokes a key.
func (k *Keyring) Remove(id string) {
	k.mu.Lock()
	delete(k.keys, id)
	k.mu.Unlock()
}

func (k *Keyring) get(id string) (Key, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	key, ok := k.keys[id]
	return key, ok
}

// GenerateSecret returns a random 32-byte secret, hex encoded.
func GenerateSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func bodyDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// canonicalQuery sorts parameters so encoding differences do not break
// signatures.
func canonicalQuery(raw string) string {
	v, err := url.ParseQuery(raw)
	if err != nil {
		return raw
	}
	return v.Encode()
}

// canonicalRequest is the string a request signature covers.
func canonicalRequest(method, escapedPath, rawQuery, keyID, ts, nonce string, body []byte) string {
	return strings.Join([]string{
		signatureScheme, strings.ToUpper(method), escapedPath, canonicalQuery(rawQuery),
		keyID, ts, nonce, bodyDigest(body),
	}, "\n")
}

// canonicalResponse is the string a response signature covers. Binding
// the request nonce makes a captured response useless for any other
// request.
func canonicalResponse(status int, keyID, reqNonce, ts string, body []byte) string {
	return strings.Join([]string{
		signatureScheme + "-RESPONSE", strconv.Itoa(status), keyID, reqNonce, ts, bodyDigest(body),
	}, "\n")
}

func sign(secret []byte, s string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(s))
	return hex.EncodeToString(m.Sum(nil))
}

func verifySig(secret []byte, s, sig string) bool {
	want, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(s))
	return hmac.Equal(m.Sum(nil), want)
}

// SignRequest adds authentication headers to req for body (which must be
// exactly what is sent). It returns the nonce, needed to verify the
// response.
func SignRequest(req *http.Request, keyID string, secret, body []byte, now time.Time) string {
	nb := make([]byte, 16)
	_, _ = rand.Read(nb)
	nonce := hex.EncodeToString(nb)
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	req.Header.Set(HeaderKeyID, keyID)
	req.Header.Set(HeaderTimestamp, ts)
	req.Header.Set(HeaderNonce, nonce)
	req.Header.Set(HeaderSignature, sign(secret, canonicalRequest(req.Method, req.URL.EscapedPath(), req.URL.RawQuery, keyID, ts, nonce, body)))
	return nonce
}

// VerifyResponse checks a response signature made for the request with
// reqNonce.
func VerifyResponse(h http.Header, status int, keyID string, secret []byte, reqNonce string, body []byte, now time.Time, maxSkew time.Duration) error {
	ts, sig := h.Get(HeaderTimestamp), h.Get(HeaderSignature)
	if ts == "" || sig == "" {
		return fmt.Errorf("%w: unsigned response", ErrBadSignature)
	}
	if err := checkSkew(ts, now, maxSkew); err != nil {
		return err
	}
	if h.Get(HeaderNonce) != reqNonce || !verifySig(secret, canonicalResponse(status, keyID, reqNonce, ts, body), sig) {
		return fmt.Errorf("%w: response", ErrBadSignature)
	}
	return nil
}

func checkSkew(ts string, now time.Time, maxSkew time.Duration) error {
	ms, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: bad timestamp", ErrUnauthorized)
	}
	d := now.Sub(time.UnixMilli(ms))
	if d > maxSkew || d < -maxSkew {
		return fmt.Errorf("%w: timestamp outside the %v window", ErrUnauthorized, maxSkew)
	}
	return nil
}

// AuthConfig configures an Authenticator.
type AuthConfig struct {
	Keyring *Keyring
	Clock   Clock
	// MaxSkew bounds clock difference (default DefaultMaxSkew). Nonces
	// are remembered for twice this long.
	MaxSkew time.Duration
	// MaxNonces caps remembered nonces (default 1,000,000); beyond it new
	// requests are refused with ErrOverloaded rather than forgetting
	// nonces that could then be replayed.
	MaxNonces int
}

// Authenticator verifies signed requests and signs responses.
type Authenticator struct {
	keys    *Keyring
	clock   Clock
	maxSkew time.Duration
	nonces  *nonceCache
}

// NewAuthenticator validates cfg.
func NewAuthenticator(cfg AuthConfig) (*Authenticator, error) {
	if cfg.Keyring == nil {
		return nil, fmt.Errorf("%w: authenticator needs a keyring", ErrInvalid)
	}
	if cfg.Clock == nil {
		cfg.Clock = RealClock()
	}
	if cfg.MaxSkew <= 0 {
		cfg.MaxSkew = DefaultMaxSkew
	}
	if cfg.MaxNonces <= 0 {
		cfg.MaxNonces = 1_000_000
	}
	return &Authenticator{keys: cfg.Keyring, clock: cfg.Clock, maxSkew: cfg.MaxSkew, nonces: newNonceCache(cfg.MaxNonces)}, nil
}

// Verify authenticates a request whose body has been read into body. Each
// nonce is accepted once within the skew window; a request outside the
// window is refused, so nonces need only be remembered that long.
func (a *Authenticator) Verify(r *http.Request, body []byte) (Principal, error) {
	keyID, ts, nonce, sig := r.Header.Get(HeaderKeyID), r.Header.Get(HeaderTimestamp), r.Header.Get(HeaderNonce), r.Header.Get(HeaderSignature)
	if keyID == "" || ts == "" || nonce == "" || sig == "" {
		return Principal{}, fmt.Errorf("%w: missing signature headers", ErrUnauthorized)
	}
	if len(nonce) < 16 || len(nonce) > 128 {
		return Principal{}, fmt.Errorf("%w: bad nonce", ErrUnauthorized)
	}
	key, ok := a.keys.get(keyID)
	if !ok {
		return Principal{}, fmt.Errorf("%w: unknown key", ErrUnauthorized)
	}
	now := a.clock.Now()
	if err := checkSkew(ts, now, a.maxSkew); err != nil {
		return Principal{}, err
	}
	if !verifySig(key.Secret, canonicalRequest(r.Method, r.URL.EscapedPath(), r.URL.RawQuery, keyID, ts, nonce, body), sig) {
		return Principal{}, fmt.Errorf("%w: signature mismatch", ErrUnauthorized)
	}
	// Only a correctly signed request may consume a nonce slot.
	if err := a.nonces.use(keyID+"\x00"+nonce, now.Add(2*a.maxSkew), now); err != nil {
		return Principal{}, err
	}
	return Principal{KeyID: keyID, Roles: slices.Clone(key.Roles), Tenant: key.Tenant}, nil
}

// SignResponse adds response signature headers for a request
// authenticated as keyID with reqNonce.
func (a *Authenticator) SignResponse(h http.Header, status int, keyID, reqNonce string, body []byte) {
	key, ok := a.keys.get(keyID)
	if !ok {
		return
	}
	ts := strconv.FormatInt(a.clock.Now().UnixMilli(), 10)
	h.Set(HeaderTimestamp, ts)
	h.Set(HeaderNonce, reqNonce)
	h.Set(HeaderSignature, sign(key.Secret, canonicalResponse(status, keyID, reqNonce, ts, body)))
}

// nonceCache remembers nonces until they expire.
type nonceCache struct {
	mu   sync.Mutex
	max  int
	seen map[string]time.Time
	exp  nonceHeap
}

func newNonceCache(max int) *nonceCache {
	return &nonceCache{max: max, seen: map[string]time.Time{}}
}

func (c *nonceCache) use(n string, expires, now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.exp) > 0 && !c.exp[0].at.After(now) {
		e := heap.Pop(&c.exp).(nonceEntry)
		if at, ok := c.seen[e.n]; ok && at.Equal(e.at) {
			delete(c.seen, e.n)
		}
	}
	if _, ok := c.seen[n]; ok {
		return ErrReplay
	}
	if len(c.seen) >= c.max {
		return fmt.Errorf("%w: nonce cache full", ErrOverloaded)
	}
	c.seen[n] = expires
	heap.Push(&c.exp, nonceEntry{n, expires})
	return nil
}

func (c *nonceCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.seen)
}

type nonceEntry struct {
	n  string
	at time.Time
}

type nonceHeap []nonceEntry

func (h nonceHeap) Len() int           { return len(h) }
func (h nonceHeap) Less(i, j int) bool { return h[i].at.Before(h[j].at) }
func (h nonceHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *nonceHeap) Push(x any)        { *h = append(*h, x.(nonceEntry)) }
func (h *nonceHeap) Pop() any {
	old := *h
	e := old[len(old)-1]
	*h = old[:len(old)-1]
	return e
}
