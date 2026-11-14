package vault

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"math"
	"slices"
	"strconv"
	"time"
)

const (
	keyringMagic         = "GTKR"
	keyringFormatV1 byte = 0x01
	indexKeySize         = 32
	maxKEKVersions       = 4096

	keyringFixedSize = 4 + 1 + 8 + 4 + indexKeySize + 2
	keyringEntrySize = 4 + 8 + MasterKeySize
)

// kek is one key-encryption-key version.
type kek struct {
	version uint32
	created int64 // unix seconds
	key     []byte
	wrapKey []byte // HKDF(key, vault id, "gta-vault/v1/dek-wrap/kek-<version>")
}

func (k *kek) deriveWrapKey(vaultID []byte) error {
	wk, err := DeriveKey(k.key, vaultID, labelDEKWrap+strconv.FormatUint(uint64(k.version), 10), 32)
	if err != nil {
		return err
	}
	k.wrapKey = wk
	return nil
}

func (k *kek) wipe() { wipeAll(k.key, k.wrapKey) }

// keyring is the decrypted set of KEKs plus the object index key.
type keyring struct {
	vaultID    []byte
	generation uint64
	current    uint32
	indexKey   []byte
	keks       []*kek // ascending by version
}

func newKeyring(vaultID []byte, now time.Time) (*keyring, error) {
	r := &keyring{vaultID: vaultID, indexKey: make([]byte, indexKeySize)}
	if err := randomBytes(r.indexKey); err != nil {
		return nil, err
	}
	if _, err := r.addVersion(now); err != nil {
		r.wipe()
		return nil, err
	}
	return r, nil
}

// addVersion appends a random KEK with the next version and makes it current.
func (r *keyring) addVersion(now time.Time) (*kek, error) {
	if len(r.keks) >= maxKEKVersions {
		return nil, invalid("keyring holds %d versions; migrate and prune first", len(r.keks))
	}
	var next uint32 = 1
	if n := len(r.keks); n > 0 {
		last := r.keks[n-1].version
		if last == math.MaxUint32 {
			return nil, invalid("KEK version space exhausted")
		}
		next = last + 1
	}
	k := &kek{version: next, created: now.Unix(), key: make([]byte, MasterKeySize)}
	if err := randomBytes(k.key); err != nil {
		return nil, err
	}
	if err := k.deriveWrapKey(r.vaultID); err != nil {
		k.wipe()
		return nil, err
	}
	r.keks = append(r.keks, k)
	r.current = next
	return k, nil
}

func (r *keyring) lookup(version uint32) *kek {
	i, ok := slices.BinarySearchFunc(r.keks, version, func(k *kek, v uint32) int {
		switch {
		case k.version < v:
			return -1
		case k.version > v:
			return 1
		}
		return 0
	})
	if !ok {
		return nil
	}
	return r.keks[i]
}

func (r *keyring) currentKEK() *kek { return r.lookup(r.current) }

// idTag is the keyed object identifier and file name of id.
func (r *keyring) idTag(id string) [idTagSize]byte {
	m := hmac.New(sha256.New, r.indexKey)
	m.Write([]byte(labelObjectID))
	m.Write([]byte{0})
	m.Write([]byte(id))
	var t [idTagSize]byte
	m.Sum(t[:0])
	return t
}

// clone deep-copies the keyring for a transactional update.
func (r *keyring) clone() *keyring {
	c := &keyring{
		vaultID:    r.vaultID,
		generation: r.generation,
		current:    r.current,
		indexKey:   append([]byte(nil), r.indexKey...),
		keks:       make([]*kek, len(r.keks)),
	}
	for i, k := range r.keks {
		c.keks[i] = &kek{
			version: k.version,
			created: k.created,
			key:     append([]byte(nil), k.key...),
			wrapKey: append([]byte(nil), k.wrapKey...),
		}
	}
	return c
}

func (r *keyring) wipe() {
	if r == nil {
		return
	}
	wipe(r.indexKey)
	for _, k := range r.keks {
		k.wipe()
	}
	r.keks = nil
}

// marshal encodes the keyring. The result holds key material; wipe it.
func (r *keyring) marshal() []byte {
	b := make([]byte, 0, keyringFixedSize+len(r.keks)*keyringEntrySize)
	b = append(b, keyringMagic...)
	b = append(b, keyringFormatV1)
	b = binary.BigEndian.AppendUint64(b, r.generation)
	b = binary.BigEndian.AppendUint32(b, r.current)
	b = append(b, r.indexKey...)
	b = binary.BigEndian.AppendUint16(b, uint16(len(r.keks)))
	for _, k := range r.keks {
		b = binary.BigEndian.AppendUint32(b, k.version)
		b = binary.BigEndian.AppendUint64(b, uint64(k.created))
		b = append(b, k.key...)
	}
	return b
}

// decodeKeyring strictly parses a keyring plaintext.
func decodeKeyring(b []byte, vaultID []byte) (*keyring, error) {
	if len(b) < keyringFixedSize || string(b[:4]) != keyringMagic {
		return nil, corrupt("bad keyring header")
	}
	if b[4] != keyringFormatV1 {
		return nil, corrupt("unsupported keyring version %d", b[4])
	}
	r := &keyring{
		vaultID:    vaultID,
		generation: binary.BigEndian.Uint64(b[5:13]),
		current:    binary.BigEndian.Uint32(b[13:17]),
		indexKey:   append([]byte(nil), b[17:17+indexKeySize]...),
	}
	count := int(binary.BigEndian.Uint16(b[17+indexKeySize:]))
	if count == 0 || count > maxKEKVersions {
		r.wipe()
		return nil, corrupt("keyring has %d versions", count)
	}
	if len(b) != keyringFixedSize+count*keyringEntrySize {
		r.wipe()
		return nil, corrupt("keyring length mismatch")
	}
	off := keyringFixedSize
	for i := range count {
		k := &kek{
			version: binary.BigEndian.Uint32(b[off:]),
			created: int64(binary.BigEndian.Uint64(b[off+4:])),
			key:     append([]byte(nil), b[off+12:off+keyringEntrySize]...),
		}
		off += keyringEntrySize
		if k.version == 0 || (i > 0 && k.version <= r.keks[i-1].version) {
			k.wipe()
			r.wipe()
			return nil, corrupt("keyring versions not strictly ascending")
		}
		if err := k.deriveWrapKey(vaultID); err != nil {
			k.wipe()
			r.wipe()
			return nil, err
		}
		r.keks = append(r.keks, k)
	}
	if r.currentKEK() == nil {
		r.wipe()
		return nil, corrupt("keyring current version %d missing", r.current)
	}
	return r, nil
}
