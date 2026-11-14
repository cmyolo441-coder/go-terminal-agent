package vault

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Object kinds used by the agent. Kind is free-form; these are conventions.
const (
	KindSession = "session"
	KindToken   = "token"
	KindMemory  = "memory"
)

const (
	objectExt      = ".gtav"
	maxIDLen       = 255
	maxKindLen     = 64
	maxContentType = 255
	maxLabels      = 64
	maxLabelKey    = 128
	maxLabelValue  = 1024
)

// Metadata is caller-supplied, encrypted object metadata.
type Metadata struct {
	Kind        string            `json:"kind,omitempty"`
	ContentType string            `json:"content_type,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

// Info describes a stored object.
type Info struct {
	ID string
	Metadata
	Size     int64
	Created  time.Time
	Modified time.Time
	// Version increments on every Put of the id.
	Version uint64
	// KeyVersion is the KEK version wrapping the object's DEK (0 for shared envelopes).
	KeyVersion uint32
}

// metaRecord is the plaintext of the metadata section.
type metaRecord struct {
	ID          string            `json:"id"`
	Kind        string            `json:"kind,omitempty"`
	ContentType string            `json:"content_type,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Size        int64             `json:"size"`
	Created     time.Time         `json:"created"`
	Modified    time.Time         `json:"modified"`
}

func (m *metaRecord) info(h *header) Info {
	return Info{
		ID:         m.ID,
		Metadata:   Metadata{Kind: m.Kind, ContentType: m.ContentType, Labels: m.Labels},
		Size:       m.Size,
		Created:    m.Created,
		Modified:   m.Modified,
		Version:    h.objVersion,
		KeyVersion: h.kekVersion,
	}
}

func validateID(id string) error {
	if id == "" || len(id) > maxIDLen || !utf8.ValidString(id) {
		return fmt.Errorf("%w: length %d", ErrInvalidID, len(id))
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: control character", ErrInvalidID)
		}
	}
	return nil
}

func validateMetadata(md *Metadata) error {
	if len(md.Kind) > maxKindLen || !utf8.ValidString(md.Kind) {
		return invalid("metadata kind too long or not UTF-8")
	}
	if len(md.ContentType) > maxContentType || !utf8.ValidString(md.ContentType) {
		return invalid("metadata content type too long or not UTF-8")
	}
	if len(md.Labels) > maxLabels {
		return invalid("%d labels, max %d", len(md.Labels), maxLabels)
	}
	for k, val := range md.Labels {
		if k == "" || len(k) > maxLabelKey || len(val) > maxLabelValue || !utf8.ValidString(k) || !utf8.ValidString(val) {
			return invalid("label %q invalid", k)
		}
	}
	return nil
}

func encodeMeta(m *metaRecord) ([]byte, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("vault: encode metadata: %w", err)
	}
	if len(b) > maxMetaPlaintext {
		return nil, fmt.Errorf("%w: metadata is %d bytes, max %d", ErrTooLarge, len(b), maxMetaPlaintext)
	}
	return b, nil
}

func decodeMeta(b []byte) (*metaRecord, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var m metaRecord
	if err := dec.Decode(&m); err != nil {
		return nil, corrupt("metadata: %v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, corrupt("metadata: trailing data")
	}
	if err := validateID(m.ID); err != nil {
		return nil, corrupt("metadata id: %v", err)
	}
	md := Metadata{Kind: m.Kind, ContentType: m.ContentType, Labels: m.Labels}
	if err := validateMetadata(&md); err != nil {
		return nil, corrupt("metadata: %v", err)
	}
	if m.Size < 0 {
		return nil, corrupt("negative size")
	}
	return &m, nil
}

func (v *Vault) objectsDir() string { return filepath.Join(v.dir, objectsDirName) }

func objectName(tag [idTagSize]byte) string { return hex.EncodeToString(tag[:]) + objectExt }

// parseObjectName returns the tag encoded in an object file name.
func parseObjectName(name string) ([idTagSize]byte, bool) {
	var tag [idTagSize]byte
	base, ok := strings.CutSuffix(name, objectExt)
	if !ok || len(base) != 2*idTagSize || strings.ToLower(base) != base {
		return tag, false
	}
	if _, err := hex.Decode(tag[:], []byte(base)); err != nil {
		return tag, false
	}
	return tag, true
}

func (v *Vault) stripe(tag [idTagSize]byte) *sync.Mutex { return &v.stripes[int(tag[0])%stripeCount] }

// readObjectFile reads a whole object file with a size bound.
func readObjectFile(path string, maxBody int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("vault: open object: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("vault: stat object: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, corrupt("object is not a regular file")
	}
	limit := maxEnvelopeSize(maxBody)
	if fi.Size() > limit {
		return nil, fmt.Errorf("%w: object file is %d bytes", ErrTooLarge, fi.Size())
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fmt.Errorf("vault: read object: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: object file grew past limit", ErrTooLarge)
	}
	return data, nil
}

// readObjectPrefix reads only the header and metadata section of an object.
func readObjectPrefix(path string) (*envelope, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("vault: open object: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("vault: stat object: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, corrupt("object is not a regular file")
	}
	er := &envReader{r: bufio.NewReaderSize(f, 4096), remaining: fi.Size()}
	e := new(envelope)
	if e.hdr, err = er.readHeader(); err != nil {
		return nil, err
	}
	if err := er.readMeta(e); err != nil {
		return nil, err
	}
	return e, nil
}

// decryptObject authenticates an envelope stored under tag and returns its
// metadata and, if wantBody, its body.
func decryptObject(ring *keyring, tag [idTagSize]byte, e *envelope, wantBody bool) (*metaRecord, []byte, error) {
	h := &e.hdr
	if h.wrapKind != wrapKindKeyring {
		return nil, nil, corrupt("object is not keyring-wrapped")
	}
	if subtle.ConstantTimeCompare(h.idTag[:], tag[:]) != 1 {
		return nil, nil, corrupt("object id tag does not match its location")
	}
	k := ring.lookup(h.kekVersion)
	if k == nil {
		return nil, nil, fmt.Errorf("%w: %d", ErrUnknownKeyVersion, h.kekVersion)
	}
	dek, err := unwrapDEK(k.wrapKey, h)
	if err != nil {
		return nil, nil, err
	}
	defer wipe(dek)
	metaPT, body, err := openSections(dek, e, wantBody)
	if err != nil {
		return nil, nil, err
	}
	defer wipe(metaPT)
	m, err := decodeMeta(metaPT)
	if err != nil {
		wipe(body)
		return nil, nil, err
	}
	if ring.idTag(m.ID) != tag {
		wipe(body)
		return nil, nil, corrupt("metadata id does not match id tag")
	}
	if wantBody && int64(len(body)) != m.Size {
		wipe(body)
		return nil, nil, corrupt("body size %d does not match metadata %d", len(body), m.Size)
	}
	return m, body, nil
}

// sealObject encrypts an object under the current KEK with a fresh DEK.
func sealObject(ring *keyring, tag [idTagSize]byte, objVersion uint64, m *metaRecord, body []byte) (*envelope, error) {
	metaPT, err := encodeMeta(m)
	if err != nil {
		return nil, err
	}
	defer wipe(metaPT)
	cur := ring.currentKEK()
	dek, dekID, err := newDEK()
	if err != nil {
		return nil, err
	}
	defer wipe(dek)
	h := header{wrapKind: wrapKindKeyring, kekVersion: cur.version, objVersion: objVersion, idTag: tag, dekID: dekID}
	if h.wrappedDEK, err = wrapDEK(cur.wrapKey, &h, dek); err != nil {
		return nil, err
	}
	return sealSections(dek, &h, metaPT, body)
}

// rewrapEnvelope re-wraps the DEK under the current KEK without touching the
// sections, which do not bind the KEK.
func rewrapEnvelope(ring *keyring, e *envelope) (*envelope, error) {
	old := ring.lookup(e.hdr.kekVersion)
	if old == nil {
		return nil, fmt.Errorf("%w: %d", ErrUnknownKeyVersion, e.hdr.kekVersion)
	}
	dek, err := unwrapDEK(old.wrapKey, &e.hdr)
	if err != nil {
		return nil, err
	}
	defer wipe(dek)
	cur := ring.currentKEK()
	ne := *e
	ne.hdr.kekVersion = cur.version
	if ne.hdr.wrappedDEK, err = wrapDEK(cur.wrapKey, &ne.hdr, dek); err != nil {
		return nil, err
	}
	return &ne, nil
}

func (v *Vault) reportBackground(op, id string, err error) {
	if v.opts.OnBackgroundError != nil {
		v.opts.OnBackgroundError(op, id, err)
	}
}

// Put stores data under id, replacing any previous object. Created is
// preserved across replacements when the previous object is readable.
func (v *Vault) Put(ctx context.Context, id string, data []byte, md Metadata) (Info, error) {
	return v.put(ctx, id, data, md, false)
}

func (v *Vault) put(ctx context.Context, id string, data []byte, md Metadata, mustNotExist bool) (Info, error) {
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	if err := validateID(id); err != nil {
		return Info{}, err
	}
	if err := validateMetadata(&md); err != nil {
		return Info{}, err
	}
	if int64(len(data)) > v.opts.MaxObjectSize {
		return Info{}, fmt.Errorf("%w: %d bytes, max %d", ErrTooLarge, len(data), v.opts.MaxObjectSize)
	}
	ring, release, err := v.acquire()
	if err != nil {
		return Info{}, err
	}
	defer release()
	tag := ring.idTag(id)
	mu := v.stripe(tag)
	mu.Lock()
	defer mu.Unlock()

	name := objectName(tag)
	now := time.Now().UTC()
	created := now
	var version uint64 = 1
	if prev, err := readObjectPrefix(filepath.Join(v.objectsDir(), name)); err == nil {
		if mustNotExist {
			return Info{}, fmt.Errorf("%w: object %q", ErrExists, id)
		}
		if prev.hdr.objVersion == math.MaxUint64 {
			return Info{}, invalid("object version space exhausted")
		}
		version = prev.hdr.objVersion + 1
		if pm, _, err := decryptObject(ring, tag, prev, false); err == nil {
			created = pm.Created
		}
	} else if !errors.Is(err, ErrNotFound) {
		if mustNotExist {
			return Info{}, fmt.Errorf("%w: object %q (unreadable)", ErrExists, id)
		}
		// An unreadable previous object is replaced; the error is surfaced
		// so tampering does not go unnoticed.
		v.reportBackground("put-replace-unreadable", id, err)
	}
	m := &metaRecord{
		ID: id, Kind: md.Kind, ContentType: md.ContentType, Labels: md.Labels,
		Size: int64(len(data)), Created: created, Modified: now,
	}
	e, err := sealObject(ring, tag, version, m, data)
	if err != nil {
		return Info{}, err
	}
	if err := writeFileAtomic(v.objectsDir(), name, e.marshal()); err != nil {
		return Info{}, err
	}
	return m.info(&e.hdr), nil
}

// Get returns the object stored under id. If the object's DEK is wrapped by
// an older KEK, Get re-wraps it under the current KEK unless lazy re-wrap is
// disabled; failures of that step go to OnBackgroundError.
func (v *Vault) Get(ctx context.Context, id string) ([]byte, Info, error) {
	if err := ctx.Err(); err != nil {
		return nil, Info{}, err
	}
	if err := validateID(id); err != nil {
		return nil, Info{}, err
	}
	ring, release, err := v.acquire()
	if err != nil {
		return nil, Info{}, err
	}
	defer release()
	tag := ring.idTag(id)
	mu := v.stripe(tag)
	mu.Lock()
	defer mu.Unlock()

	name := objectName(tag)
	blob, err := readObjectFile(filepath.Join(v.objectsDir(), name), v.opts.MaxObjectSize)
	if err != nil {
		return nil, Info{}, wrapObjectErr(id, err)
	}
	e, err := parseEnvelope(blob, v.opts.MaxObjectSize)
	if err != nil {
		return nil, Info{}, wrapObjectErr(id, err)
	}
	m, body, err := decryptObject(ring, tag, e, true)
	if err != nil {
		return nil, Info{}, wrapObjectErr(id, err)
	}
	if m.ID != id {
		wipe(body)
		return nil, Info{}, wrapObjectErr(id, corrupt("metadata id mismatch"))
	}
	info := m.info(&e.hdr)
	if !v.opts.DisableLazyRewrap && e.hdr.kekVersion != ring.current {
		if ne, err := rewrapEnvelope(ring, e); err != nil {
			v.reportBackground("rewrap", id, err)
		} else if err := writeFileAtomic(v.objectsDir(), name, ne.marshal()); err != nil {
			v.reportBackground("rewrap", id, err)
		} else {
			info.KeyVersion = ne.hdr.kekVersion
		}
	}
	return body, info, nil
}

func wrapObjectErr(id string, err error) error {
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	return fmt.Errorf("vault: object %q: %w", id, err)
}

// Stat returns an object's metadata without decrypting its body.
func (v *Vault) Stat(ctx context.Context, id string) (Info, error) {
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	if err := validateID(id); err != nil {
		return Info{}, err
	}
	ring, release, err := v.acquire()
	if err != nil {
		return Info{}, err
	}
	defer release()
	tag := ring.idTag(id)
	e, err := readObjectPrefix(filepath.Join(v.objectsDir(), objectName(tag)))
	if err != nil {
		return Info{}, wrapObjectErr(id, err)
	}
	m, _, err := decryptObject(ring, tag, e, false)
	if err != nil {
		return Info{}, wrapObjectErr(id, err)
	}
	return m.info(&e.hdr), nil
}

// Delete removes the object stored under id.
func (v *Vault) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateID(id); err != nil {
		return err
	}
	ring, release, err := v.acquire()
	if err != nil {
		return err
	}
	defer release()
	tag := ring.idTag(id)
	mu := v.stripe(tag)
	mu.Lock()
	defer mu.Unlock()
	if err := os.Remove(filepath.Join(v.objectsDir(), objectName(tag))); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %q", ErrNotFound, id)
		}
		return fmt.Errorf("vault: delete %q: %w", id, err)
	}
	return syncDir(v.objectsDir())
}

// List returns the metadata of every object whose id starts with prefix,
// sorted by id. Only headers and metadata sections are read. Objects that
// fail authentication are left out and reported together in the returned
// error, alongside the readable results.
func (v *Vault) List(ctx context.Context, prefix string) ([]Info, error) {
	ring, release, err := v.acquire()
	if err != nil {
		return nil, err
	}
	defer release()
	entries, err := os.ReadDir(v.objectsDir())
	if err != nil {
		return nil, fmt.Errorf("vault: list objects: %w", err)
	}
	var (
		out  []Info
		errs []error
	)
	for _, ent := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		tag, ok := parseObjectName(ent.Name())
		if !ok {
			continue
		}
		e, err := readObjectPrefix(filepath.Join(v.objectsDir(), ent.Name()))
		if errors.Is(err, ErrNotFound) {
			continue // deleted concurrently
		}
		if err == nil {
			var m *metaRecord
			if m, _, err = decryptObject(ring, tag, e, false); err == nil {
				if strings.HasPrefix(m.ID, prefix) {
					out = append(out, m.info(&e.hdr))
				}
				continue
			}
		}
		errs = append(errs, fmt.Errorf("vault: object file %s: %w", ent.Name(), err))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, errors.Join(errs...)
}

// sharedIDTag is the unkeyed id tag of a shared envelope.
func sharedIDTag(id string) [idTagSize]byte {
	h := sha256.New()
	h.Write([]byte(labelSharedID))
	h.Write([]byte{0})
	h.Write([]byte(id))
	var t [idTagSize]byte
	h.Sum(t[:0])
	return t
}

// SealForRecipient encrypts data for the holder of to's private key using the
// hybrid X25519 + ML-KEM-768 KEM. The result is a self-contained envelope
// (wrap kind 0x02) that OpenSealed decrypts; no vault is needed on either side.
func SealForRecipient(to *HybridPublicKey, id string, data []byte, md Metadata) ([]byte, error) {
	if to == nil {
		return nil, invalid("nil recipient")
	}
	if err := validateID(id); err != nil {
		return nil, err
	}
	if err := validateMetadata(&md); err != nil {
		return nil, err
	}
	if int64(len(data)) > MaxObjectSizeLimit {
		return nil, fmt.Errorf("%w: %d bytes", ErrTooLarge, len(data))
	}
	now := time.Now().UTC()
	m := &metaRecord{
		ID: id, Kind: md.Kind, ContentType: md.ContentType, Labels: md.Labels,
		Size: int64(len(data)), Created: now, Modified: now,
	}
	metaPT, err := encodeMeta(m)
	if err != nil {
		return nil, err
	}
	defer wipe(metaPT)
	dek, dekID, err := newDEK()
	if err != nil {
		return nil, err
	}
	defer wipe(dek)
	h := header{wrapKind: wrapKindHybrid, objVersion: 1, idTag: sharedIDTag(id), dekID: dekID}
	if h.wrappedDEK, err = hybridWrapDEK(to, &h, dek); err != nil {
		return nil, err
	}
	e, err := sealSections(dek, &h, metaPT, data)
	if err != nil {
		return nil, err
	}
	return e.marshal(), nil
}

// OpenSealed decrypts an envelope produced by SealForRecipient. It returns
// ErrWrongRecipient if the envelope is addressed to a different key.
func OpenSealed(identity *HybridPrivateKey, blob []byte) (Info, []byte, error) {
	if identity == nil {
		return Info{}, nil, invalid("nil identity")
	}
	e, err := parseEnvelope(blob, MaxObjectSizeLimit)
	if err != nil {
		return Info{}, nil, err
	}
	if e.hdr.wrapKind != wrapKindHybrid {
		return Info{}, nil, corrupt("not a shared envelope")
	}
	dek, err := hybridUnwrapDEK(identity, &e.hdr)
	if err != nil {
		return Info{}, nil, err
	}
	defer wipe(dek)
	metaPT, body, err := openSections(dek, e, true)
	if err != nil {
		return Info{}, nil, err
	}
	defer wipe(metaPT)
	m, err := decodeMeta(metaPT)
	if err != nil {
		wipe(body)
		return Info{}, nil, err
	}
	if sharedIDTag(m.ID) != e.hdr.idTag || int64(len(body)) != m.Size {
		wipe(body)
		return Info{}, nil, corrupt("shared envelope identity or size mismatch")
	}
	return m.info(&e.hdr), body, nil
}

// Export decrypts the object id and re-encrypts it, under a fresh DEK, for a
// teammate's hybrid public key.
func (v *Vault) Export(ctx context.Context, id string, to *HybridPublicKey) ([]byte, error) {
	body, info, err := v.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	defer wipe(body)
	return SealForRecipient(to, id, body, info.Metadata)
}

// ImportOptions control Import.
type ImportOptions struct {
	// ID stores the object under a different id than the one it was shared with.
	ID string
	// Overwrite replaces an existing object instead of failing with ErrExists.
	Overwrite bool
}

// Import decrypts a shared envelope with identity and stores it in the vault.
func (v *Vault) Import(ctx context.Context, identity *HybridPrivateKey, blob []byte, opts ImportOptions) (Info, error) {
	shared, body, err := OpenSealed(identity, blob)
	if err != nil {
		return Info{}, err
	}
	defer wipe(body)
	id := shared.ID
	if opts.ID != "" {
		id = opts.ID
	}
	return v.put(ctx, id, body, shared.Metadata, !opts.Overwrite)
}
