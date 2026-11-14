package dlp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode"
)

// Token format: [[PII:<TYPE>:<hex>]]. The hex part is a prefix of
// HMAC-SHA256(key, type || 0x00 || normalized value): 12 digits (48 bits),
// lengthened on the (astronomically unlikely) collision of two different
// values within one session.
const (
	tokenPrefix   = "[[PII:"
	tokenSuffix   = "]]"
	tokenHexMin   = 12
	tokenHexMax   = 32
	maxTokenLen   = len(tokenPrefix) + 40 + 1 + tokenHexMax + len(tokenSuffix)
	defaultVaultN = 1 << 20
)

// ErrVaultFull is returned by Tokenize once the vault holds its maximum
// number of values; the engine then falls back to redaction.
var ErrVaultFull = errors.New("dlp: token vault is full")

// Vault holds the reversible mapping between sensitive values and tokens
// for one session. Tokens are deterministic: the same value of the same
// type always yields the same token for the life of the key, so a model can
// correlate repeated mentions without seeing the value. Values never leave
// the vault except through Detokenize. A Vault is safe for concurrent use.
type Vault struct {
	key []byte
	max int

	mu      sync.RWMutex
	byValue map[string]string // type \x00 normalized → token
	byToken map[string]string // token → original value
}

// NewVault creates a vault keyed with key (at least 16 bytes); an empty key
// draws 32 random bytes. maxEntries bounds memory (0 means 1,048,576).
func NewVault(key []byte, maxEntries int) (*Vault, error) {
	if len(key) == 0 {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("dlp: vault key: %w", err)
		}
	} else if len(key) < 16 {
		return nil, errors.New("dlp: vault key must be at least 16 bytes")
	} else {
		key = append([]byte(nil), key...)
	}
	if maxEntries <= 0 {
		maxEntries = defaultVaultN
	}
	return &Vault{key: key, max: maxEntries, byValue: map[string]string{}, byToken: map[string]string{}}, nil
}

// normalizeValue canonicalises a value so formatting differences map to
// the same token: digits-only for numeric identifiers, upper-case
// alphanumerics for IBAN/PAN/NINO/passport, lower case for email.
func normalizeValue(t InfoType, v string) string {
	switch t {
	case CreditCard, USSSN, Aadhaar, Phone:
		var b strings.Builder
		for i := 0; i < len(v); i++ {
			if isDigit(v[i]) {
				b.WriteByte(v[i])
			}
		}
		return b.String()
	case IBAN, IndiaPAN, UKNINO, Passport:
		var b strings.Builder
		for i := 0; i < len(v); i++ {
			if isAlnum(v[i]) {
				b.WriteByte(toUpper(v[i]))
			}
		}
		return b.String()
	case Email:
		return strings.ToLower(v)
	}
	return strings.TrimFunc(v, unicode.IsSpace)
}

// Tokenize returns the token for value, creating it if needed.
func (v *Vault) Tokenize(t InfoType, value string) (string, error) {
	if !validTokenType(string(t)) {
		return "", fmt.Errorf("dlp: cannot tokenize type %q", t)
	}
	key := string(t) + "\x00" + normalizeValue(t, value)
	v.mu.RLock()
	tok, ok := v.byValue[key]
	v.mu.RUnlock()
	if ok {
		return tok, nil
	}
	mac := hmac.New(sha256.New, v.key)
	mac.Write([]byte(key))
	sum := hex.EncodeToString(mac.Sum(nil))

	v.mu.Lock()
	defer v.mu.Unlock()
	if tok, ok := v.byValue[key]; ok {
		return tok, nil
	}
	if len(v.byValue) >= v.max {
		return "", ErrVaultFull
	}
	for n := tokenHexMin; n <= tokenHexMax; n += 4 {
		cand := tokenPrefix + string(t) + ":" + sum[:n] + tokenSuffix
		if _, taken := v.byToken[cand]; !taken {
			v.byValue[key] = cand
			v.byToken[cand] = value
			return cand, nil
		}
	}
	return "", errors.New("dlp: token collision could not be resolved")
}

// Lookup returns the original value of a token issued by this vault.
func (v *Vault) Lookup(token string) (string, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	s, ok := v.byToken[token]
	return s, ok
}

// Detokenize replaces every token this vault issued with its original
// value and returns the count replaced. Unknown or malformed tokens are
// left untouched.
func (v *Vault) Detokenize(text string) (string, int) {
	i := strings.Index(text, tokenPrefix)
	if i < 0 {
		return text, 0
	}
	var b strings.Builder
	b.Grow(len(text))
	n := 0
	pos := 0
	v.mu.RLock()
	defer v.mu.RUnlock()
	for i >= 0 {
		i += pos
		l := parseToken(text[i:])
		if l > 0 {
			if val, ok := v.byToken[text[i:i+l]]; ok {
				b.WriteString(text[pos:i])
				b.WriteString(val)
				n++
				pos = i + l
				i = strings.Index(text[pos:], tokenPrefix)
				continue
			}
		}
		b.WriteString(text[pos : i+1])
		pos = i + 1
		i = strings.Index(text[pos:], tokenPrefix)
	}
	if n == 0 {
		return text, 0
	}
	b.WriteString(text[pos:])
	return b.String(), n
}

// parseToken returns the length of a well-formed token at the start of s,
// or 0.
func parseToken(s string) int {
	if !strings.HasPrefix(s, tokenPrefix) {
		return 0
	}
	k := len(tokenPrefix)
	ts := k
	for k < len(s) && k-ts <= 40 && (isUpper(s[k]) || isDigit(s[k]) || s[k] == '_') {
		k++
	}
	if k == ts || k >= len(s) || s[k] != ':' {
		return 0
	}
	k++
	hs := k
	for k < len(s) && k-hs <= tokenHexMax && (isDigit(s[k]) || (s[k] >= 'a' && s[k] <= 'f')) {
		k++
	}
	if h := k - hs; h < tokenHexMin || h > tokenHexMax || !strings.HasPrefix(s[k:], tokenSuffix) {
		return 0
	}
	return k + len(tokenSuffix)
}

func validTokenType(s string) bool {
	if s == "" || len(s) > 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isUpper(s[i]) && !isDigit(s[i]) && s[i] != '_' {
			return false
		}
	}
	return true
}

// Len returns the number of values held.
func (v *Vault) Len() int {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return len(v.byValue)
}

// Purge forgets every value. Tokens issued earlier can no longer be
// restored; the key is kept, so values tokenized again get the same tokens.
func (v *Vault) Purge() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.byValue = map[string]string{}
	v.byToken = map[string]string{}
}
