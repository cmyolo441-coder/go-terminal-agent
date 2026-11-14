package vault

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"testing"
	"time"
)

func testRing(t *testing.T) *keyring {
	t.Helper()
	r, err := newKeyring(bytes.Repeat([]byte{7}, vaultIDSize), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func sealTestObject(t *testing.T, r *keyring, id string, body []byte) []byte {
	t.Helper()
	now := time.Now().UTC()
	m := &metaRecord{ID: id, Kind: KindToken, Size: int64(len(body)), Created: now, Modified: now}
	e, err := sealObject(r, r.idTag(id), 1, m, body)
	if err != nil {
		t.Fatal(err)
	}
	return e.marshal()
}

func openTestObject(r *keyring, id string, blob []byte) ([]byte, error) {
	e, err := parseEnvelope(blob, DefaultMaxObjectSize)
	if err != nil {
		return nil, err
	}
	m, body, err := decryptObject(r, r.idTag(id), e, true)
	if err != nil {
		return nil, err
	}
	if m.ID != id {
		return nil, corrupt("id")
	}
	return body, nil
}

func TestEnvelopeRoundTripAndLayout(t *testing.T) {
	r := testRing(t)
	body := []byte("session transcript")
	blob := sealTestObject(t, r, "s1", body)

	if string(blob[:4]) != "GTAV" || blob[4] != 1 || blob[5] != 1 || blob[6] != wrapKindKeyring || blob[7] != 0 {
		t.Fatalf("bad fixed header % x", blob[:8])
	}
	if got := binary.BigEndian.Uint32(blob[8:12]); got != 1 {
		t.Fatalf("KEK version %d", got)
	}
	if got := binary.BigEndian.Uint64(blob[12:20]); got != 1 {
		t.Fatalf("object version %d", got)
	}
	tag := r.idTag("s1")
	if !bytes.Equal(blob[20:52], tag[:]) {
		t.Fatal("id tag not at offset 20")
	}
	if got := binary.BigEndian.Uint16(blob[68:70]); got != keyringWrappedDEKSize {
		t.Fatalf("W = %d", got)
	}
	got, err := openTestObject(r, "s1", blob)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("open: %q %v", got, err)
	}
	e, err := parseEnvelope(blob, DefaultMaxObjectSize)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(e.marshal(), blob) {
		t.Fatal("marshal is not canonical")
	}
	if len(blob) != e.size() {
		t.Fatalf("size() = %d, len = %d", e.size(), len(blob))
	}
}

func TestEnvelopeEmptyBody(t *testing.T) {
	r := testRing(t)
	blob := sealTestObject(t, r, "empty", nil)
	got, err := openTestObject(r, "empty", blob)
	if err != nil || len(got) != 0 {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestEnvelopeTamperEveryByte(t *testing.T) {
	r := testRing(t)
	blob := sealTestObject(t, r, "tamper", []byte("secret body bytes"))
	for i := range blob {
		for _, mask := range []byte{0x01, 0x80, 0xff} {
			mod := bytes.Clone(blob)
			mod[i] ^= mask
			if _, err := openTestObject(r, "tamper", mod); err == nil {
				t.Fatalf("flip %#x at byte %d/%d not detected", mask, i, len(blob))
			}
		}
	}
	// Truncation at every length and any appended byte.
	for n := range len(blob) {
		if _, err := openTestObject(r, "tamper", blob[:n]); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("truncation to %d: %v", n, err)
		}
	}
	if _, err := openTestObject(r, "tamper", append(bytes.Clone(blob), 0)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("trailing byte: %v", err)
	}
}

func TestEnvelopeWrongKeyAndAAD(t *testing.T) {
	r := testRing(t)
	blob := sealTestObject(t, r, "a", []byte("data"))

	// Another keyring: different index key and KEK.
	other := testRing(t)
	if _, err := openTestObject(other, "a", blob); err == nil {
		t.Fatal("opened with foreign keyring")
	}
	// Same keyring, different id: id tag mismatch.
	if _, err := openTestObject(r, "b", blob); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("wrong id: %v", err)
	}
	// Same index key, wrong KEK bytes.
	bad := r.clone()
	bad.keks[0].key[0] ^= 1
	if err := bad.keks[0].deriveWrapKey(bad.vaultID); err != nil {
		t.Fatal(err)
	}
	if _, err := openTestObject(bad, "a", blob); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("wrong KEK: %v", err)
	}

	// Splice: the wrapped DEK of one object into another must fail.
	blob2 := sealTestObject(t, r, "a", []byte("other data"))
	spliced := bytes.Clone(blob2)
	copy(spliced[fixedHeaderSize:fixedHeaderSize+keyringWrappedDEKSize], blob[fixedHeaderSize:fixedHeaderSize+keyringWrappedDEKSize])
	if _, err := openTestObject(r, "a", spliced); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("spliced DEK: %v", err)
	}
	// Bumping the object version (a different AAD) must fail.
	bumped := bytes.Clone(blob)
	binary.BigEndian.PutUint64(bumped[12:20], 2)
	if _, err := openTestObject(r, "a", bumped); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("version change: %v", err)
	}
}

func TestEnvelopeStrictParsing(t *testing.T) {
	r := testRing(t)
	blob := sealTestObject(t, r, "x", []byte("y"))
	cases := map[string]func(b []byte){
		"magic":        func(b []byte) { b[0] = 'X' },
		"version":      func(b []byte) { b[4] = 2 },
		"suite":        func(b []byte) { b[5] = 9 },
		"wrap kind":    func(b []byte) { b[6] = 9 },
		"flags":        func(b []byte) { b[7] = 1 },
		"kek zero":     func(b []byte) { binary.BigEndian.PutUint32(b[8:12], 0) },
		"obj zero":     func(b []byte) { binary.BigEndian.PutUint64(b[12:20], 0) },
		"W":            func(b []byte) { binary.BigEndian.PutUint16(b[68:70], 61) },
		"meta len big": func(b []byte) { binary.BigEndian.PutUint32(b[70+60+12:], maxMetaCiphertext+1) },
		"meta len 0":   func(b []byte) { binary.BigEndian.PutUint32(b[70+60+12:], 0) },
	}
	for name, mut := range cases {
		mod := bytes.Clone(blob)
		mut(mod)
		if _, err := parseEnvelope(mod, DefaultMaxObjectSize); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// Body length above the configured limit.
	if _, err := parseEnvelope(blob, 0); !errors.Is(err, ErrCorrupt) {
		t.Errorf("body limit: %v", err)
	}
}

func TestVaultFileTamperDetected(t *testing.T) {
	v, dir := newTestVault(t, Options{})
	mustPut(t, v, "k", []byte("v"))
	v.Lock()
	path := dir + "/" + vaultFileName
	orig, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	vf, err := decodeVaultFile(orig)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := range vf.Keyring {
		mod := *vf
		mod.Keyring = bytes.Clone(vf.Keyring)
		mod.Keyring[i] ^= 0x01
		s, err := newSealer(ctx, Passphrase(testPass()), &mod.Root, mustID(t, &mod))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := openKeyring(ctx, &mod, mustID(t, &mod), s); !errors.Is(err, ErrWrongKey) {
			t.Fatalf("keyring byte %d: %v", i, err)
		}
	}
	// Changing the salt or iteration count must not authenticate either.
	mod := *vf
	mod.Root.Iterations++
	s, _ := newSealer(ctx, Passphrase(testPass()), &mod.Root, mustID(t, &mod))
	if _, err := openKeyring(ctx, &mod, mustID(t, &mod), s); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("iterations change: %v", err)
	}
	// Unknown fields and trailing data are rejected.
	if _, err := decodeVaultFile(append(bytes.Clone(orig), []byte(" {}")...)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("trailing: %v", err)
	}
	if _, err := decodeVaultFile(bytes.Replace(orig, []byte(`"format"`), []byte(`"extra":1,"format"`), 1)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("unknown field: %v", err)
	}
}

func mustID(t *testing.T, vf *vaultFile) []byte {
	t.Helper()
	id, err := vf.id()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestKeyringEncoding(t *testing.T) {
	r := testRing(t)
	if _, err := r.addVersion(time.Now()); err != nil {
		t.Fatal(err)
	}
	b := r.marshal()
	got, err := decodeKeyring(b, r.vaultID)
	if err != nil {
		t.Fatal(err)
	}
	if got.current != 2 || len(got.keks) != 2 || !bytes.Equal(got.keks[1].wrapKey, r.keks[1].wrapKey) {
		t.Fatal("keyring did not round-trip")
	}
	if !bytes.Equal(got.marshal(), b) {
		t.Fatal("keyring encoding not canonical")
	}
	for n := range len(b) {
		if _, err := decodeKeyring(b[:n], r.vaultID); err == nil {
			t.Fatalf("truncated keyring (%d) accepted", n)
		}
	}
	// Versions out of order.
	swapped := bytes.Clone(b)
	binary.BigEndian.PutUint32(swapped[keyringFixedSize:], 5)
	if _, err := decodeKeyring(swapped, r.vaultID); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("unordered versions: %v", err)
	}
	// Missing current version.
	cur := bytes.Clone(b)
	binary.BigEndian.PutUint32(cur[13:17], 9)
	if _, err := decodeKeyring(cur, r.vaultID); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("missing current: %v", err)
	}
}
