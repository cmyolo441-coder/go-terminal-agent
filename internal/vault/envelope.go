package vault

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Envelope constants; see the package documentation for the byte layout.
const (
	envelopeMagic         = "GTAV"
	envelopeFormatV1 byte = 0x01
	suiteAES256GCM   byte = 0x01

	wrapKindKeyring byte = 0x01
	wrapKindHybrid  byte = 0x02

	idTagSize  = 32
	dekIDSize  = 16
	dekSize    = 32
	nonceSize  = 12
	gcmTagSize = 16

	// fixedHeaderSize covers magic through the wrapped-DEK length field.
	fixedHeaderSize       = 4 + 1 + 1 + 1 + 1 + 4 + 8 + idTagSize + dekIDSize + 2
	keyringWrappedDEKSize = nonceSize + dekSize + gcmTagSize

	maxMetaPlaintext  = 64 << 10
	maxMetaCiphertext = maxMetaPlaintext + gcmTagSize

	sectionMeta byte = 'M'
	sectionBody byte = 'B'

	// DefaultMaxObjectSize is the default limit on an object body.
	DefaultMaxObjectSize int64 = 256 << 20
	// MaxObjectSizeLimit is the largest configurable body limit.
	MaxObjectSizeLimit int64 = 1 << 30
)

// header is the authenticated-by-construction prefix of an envelope.
type header struct {
	wrapKind   byte
	kekVersion uint32
	objVersion uint64
	idTag      [idTagSize]byte
	dekID      [dekIDSize]byte
	wrappedDEK []byte
}

// envelope is a parsed object file.
type envelope struct {
	hdr       header
	metaNonce [nonceSize]byte
	metaCT    []byte
	bodyNonce [nonceSize]byte
	bodyCT    []byte
}

func wrappedDEKSize(kind byte) (int, bool) {
	switch kind {
	case wrapKindKeyring:
		return keyringWrappedDEKSize, true
	case wrapKindHybrid:
		return hybridWrappedDEKSize, true
	}
	return 0, false
}

// maxEnvelopeSize bounds the encoded size of an envelope with body limit maxBody.
func maxEnvelopeSize(maxBody int64) int64 {
	return fixedHeaderSize + hybridWrappedDEKSize + nonceSize + 4 + maxMetaCiphertext +
		nonceSize + 8 + maxBody + gcmTagSize
}

func (h *header) validate() error {
	n, ok := wrappedDEKSize(h.wrapKind)
	if !ok {
		return corrupt("unknown wrap kind %#x", h.wrapKind)
	}
	if len(h.wrappedDEK) != n {
		return corrupt("wrapped DEK is %d bytes, want %d", len(h.wrappedDEK), n)
	}
	switch h.wrapKind {
	case wrapKindKeyring:
		if h.kekVersion == 0 {
			return corrupt("keyring envelope with KEK version 0")
		}
	case wrapKindHybrid:
		if h.kekVersion != 0 {
			return corrupt("hybrid envelope with KEK version %d", h.kekVersion)
		}
	}
	if h.objVersion == 0 {
		return corrupt("object version 0")
	}
	return nil
}

func (h *header) appendTo(dst []byte) []byte {
	dst = append(dst, envelopeMagic...)
	dst = append(dst, envelopeFormatV1, suiteAES256GCM, h.wrapKind, 0)
	dst = binary.BigEndian.AppendUint32(dst, h.kekVersion)
	dst = binary.BigEndian.AppendUint64(dst, h.objVersion)
	dst = append(dst, h.idTag[:]...)
	dst = append(dst, h.dekID[:]...)
	dst = binary.BigEndian.AppendUint16(dst, uint16(len(h.wrappedDEK)))
	return append(dst, h.wrappedDEK...)
}

func (e *envelope) size() int {
	return fixedHeaderSize + len(e.hdr.wrappedDEK) + nonceSize + 4 + len(e.metaCT) + nonceSize + 8 + len(e.bodyCT)
}

// marshal encodes the envelope in its canonical form.
func (e *envelope) marshal() []byte {
	out := e.hdr.appendTo(make([]byte, 0, e.size()))
	out = append(out, e.metaNonce[:]...)
	out = binary.BigEndian.AppendUint32(out, uint32(len(e.metaCT)))
	out = append(out, e.metaCT...)
	out = append(out, e.bodyNonce[:]...)
	out = binary.BigEndian.AppendUint64(out, uint64(len(e.bodyCT)))
	return append(out, e.bodyCT...)
}

// envReader reads exact-length fields and refuses to allocate more than the
// input can still hold.
type envReader struct {
	r         io.Reader
	remaining int64 // bytes left in the input; -1 if unknown
}

func (er *envReader) read(n int) ([]byte, error) {
	if er.remaining >= 0 && int64(n) > er.remaining {
		return nil, corrupt("truncated envelope")
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(er.r, buf); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, corrupt("truncated envelope")
		}
		return nil, fmt.Errorf("vault: read envelope: %w", err)
	}
	if er.remaining >= 0 {
		er.remaining -= int64(n)
	}
	return buf, nil
}

func (er *envReader) readHeader() (header, error) {
	var h header
	fixed, err := er.read(fixedHeaderSize)
	if err != nil {
		return h, err
	}
	if string(fixed[0:4]) != envelopeMagic {
		return h, corrupt("bad magic")
	}
	if fixed[4] != envelopeFormatV1 {
		return h, corrupt("unsupported format version %d", fixed[4])
	}
	if fixed[5] != suiteAES256GCM {
		return h, corrupt("unsupported cipher suite %d", fixed[5])
	}
	h.wrapKind = fixed[6]
	if fixed[7] != 0 {
		return h, corrupt("reserved flags set")
	}
	h.kekVersion = binary.BigEndian.Uint32(fixed[8:12])
	h.objVersion = binary.BigEndian.Uint64(fixed[12:20])
	copy(h.idTag[:], fixed[20:52])
	copy(h.dekID[:], fixed[52:68])
	w := int(binary.BigEndian.Uint16(fixed[68:70]))
	want, ok := wrappedDEKSize(h.wrapKind)
	if !ok {
		return h, corrupt("unknown wrap kind %#x", h.wrapKind)
	}
	if w != want {
		return h, corrupt("wrapped DEK length %d, want %d", w, want)
	}
	if h.wrappedDEK, err = er.read(w); err != nil {
		return h, err
	}
	return h, h.validate()
}

func (er *envReader) readMeta(e *envelope) error {
	nonce, err := er.read(nonceSize + 4)
	if err != nil {
		return err
	}
	copy(e.metaNonce[:], nonce[:nonceSize])
	m := binary.BigEndian.Uint32(nonce[nonceSize:])
	if m < gcmTagSize || m > maxMetaCiphertext {
		return corrupt("metadata length %d outside [%d, %d]", m, gcmTagSize, maxMetaCiphertext)
	}
	e.metaCT, err = er.read(int(m))
	return err
}

func (er *envReader) readBody(e *envelope, maxBody int64) error {
	nonce, err := er.read(nonceSize + 8)
	if err != nil {
		return err
	}
	copy(e.bodyNonce[:], nonce[:nonceSize])
	b := binary.BigEndian.Uint64(nonce[nonceSize:])
	if b < gcmTagSize || b > uint64(maxBody)+gcmTagSize {
		return corrupt("body length %d outside [%d, %d]", b, gcmTagSize, maxBody+gcmTagSize)
	}
	if e.bodyCT, err = er.read(int(b)); err != nil {
		return err
	}
	if er.remaining > 0 {
		return corrupt("%d trailing bytes", er.remaining)
	}
	var one [1]byte
	if n, _ := io.ReadFull(er.r, one[:]); n != 0 {
		return corrupt("trailing bytes")
	}
	return nil
}

// parseEnvelope strictly parses a complete envelope.
func parseEnvelope(b []byte, maxBody int64) (*envelope, error) {
	er := &envReader{r: bytes.NewReader(b), remaining: int64(len(b))}
	e := new(envelope)
	var err error
	if e.hdr, err = er.readHeader(); err != nil {
		return nil, err
	}
	if err := er.readMeta(e); err != nil {
		return nil, err
	}
	if err := er.readBody(e, maxBody); err != nil {
		return nil, err
	}
	return e, nil
}

// sectionAAD is the associated data for the metadata and body sections.
func sectionAAD(section byte, h *header) []byte {
	b := make([]byte, 0, len(labelSection)+4+3+8+idTagSize+dekIDSize)
	b = append(b, labelSection...)
	b = append(b, envelopeMagic...)
	b = append(b, envelopeFormatV1, suiteAES256GCM, section)
	b = binary.BigEndian.AppendUint64(b, h.objVersion)
	b = append(b, h.idTag[:]...)
	return append(b, h.dekID[:]...)
}

// dekWrapAAD is the associated data (and, for hybrid wraps, the KEM context)
// for wrapping a DEK.
func dekWrapAAD(h *header) []byte {
	b := make([]byte, 0, len(labelDEKWrapAD)+4+4+4+8+idTagSize+dekIDSize)
	b = append(b, labelDEKWrapAD...)
	b = append(b, envelopeMagic...)
	b = append(b, envelopeFormatV1, suiteAES256GCM, h.wrapKind, 0)
	b = binary.BigEndian.AppendUint32(b, h.kekVersion)
	b = binary.BigEndian.AppendUint64(b, h.objVersion)
	b = append(b, h.idTag[:]...)
	return append(b, h.dekID[:]...)
}

// newAEAD returns AES-256-GCM for a 32-byte key. The cipher holds an expanded
// key schedule that cannot be wiped, so AEADs are built per operation rather
// than cached alongside long-lived keys.
func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, invalid("AES-256 key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func randomBytes(b []byte) error {
	if _, err := rand.Read(b); err != nil {
		return fmt.Errorf("vault: random: %w", err)
	}
	return nil
}

// newDEK returns a fresh DEK and DEK id.
func newDEK() (dek []byte, id [dekIDSize]byte, err error) {
	dek = make([]byte, dekSize)
	if err = randomBytes(dek); err != nil {
		return nil, id, err
	}
	if err = randomBytes(id[:]); err != nil {
		wipe(dek)
		return nil, id, err
	}
	return dek, id, nil
}

// sealSections encrypts meta and body under dek. h must be complete
// (including the wrapped DEK) because the envelope copies it.
func sealSections(dek []byte, h *header, meta, body []byte) (*envelope, error) {
	aead, err := newAEAD(dek)
	if err != nil {
		return nil, err
	}
	e := &envelope{hdr: *h}
	if err := randomBytes(e.metaNonce[:]); err != nil {
		return nil, err
	}
	if err := randomBytes(e.bodyNonce[:]); err != nil {
		return nil, err
	}
	e.metaCT = aead.Seal(nil, e.metaNonce[:], meta, sectionAAD(sectionMeta, h))
	e.bodyCT = aead.Seal(nil, e.bodyNonce[:], body, sectionAAD(sectionBody, h))
	return e, nil
}

// openSections decrypts the metadata and, if wantBody, the body.
func openSections(dek []byte, e *envelope, wantBody bool) (meta, body []byte, err error) {
	aead, err := newAEAD(dek)
	if err != nil {
		return nil, nil, err
	}
	meta, err = aead.Open(nil, e.metaNonce[:], e.metaCT, sectionAAD(sectionMeta, &e.hdr))
	if err != nil {
		return nil, nil, corrupt("metadata authentication failed")
	}
	if !wantBody {
		return meta, nil, nil
	}
	body, err = aead.Open(nil, e.bodyNonce[:], e.bodyCT, sectionAAD(sectionBody, &e.hdr))
	if err != nil {
		wipe(meta)
		return nil, nil, corrupt("body authentication failed")
	}
	return meta, body, nil
}

// wrapDEK wraps dek under a KEK's wrapping key: nonce || GCM(dek).
func wrapDEK(wrapKey []byte, h *header, dek []byte) ([]byte, error) {
	aead, err := newAEAD(wrapKey)
	if err != nil {
		return nil, err
	}
	out := make([]byte, nonceSize, keyringWrappedDEKSize)
	if err := randomBytes(out); err != nil {
		return nil, err
	}
	return aead.Seal(out, out[:nonceSize], dek, dekWrapAAD(h)), nil
}

// unwrapDEK reverses wrapDEK.
func unwrapDEK(wrapKey []byte, h *header) ([]byte, error) {
	if len(h.wrappedDEK) != keyringWrappedDEKSize {
		return nil, corrupt("wrapped DEK length %d", len(h.wrappedDEK))
	}
	aead, err := newAEAD(wrapKey)
	if err != nil {
		return nil, err
	}
	dek, err := aead.Open(nil, h.wrappedDEK[:nonceSize], h.wrappedDEK[nonceSize:], dekWrapAAD(h))
	if err != nil {
		return nil, corrupt("DEK unwrap authentication failed")
	}
	return dek, nil
}
