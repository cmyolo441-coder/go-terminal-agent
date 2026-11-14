package vault

import (
	"bytes"
	"testing"
	"time"
)

func FuzzParseEnvelope(f *testing.F) {
	r, err := newKeyring(bytes.Repeat([]byte{1}, vaultIDSize), time.Now())
	if err != nil {
		f.Fatal(err)
	}
	now := time.Now().UTC()
	m := &metaRecord{ID: "seed", Size: 4, Created: now, Modified: now}
	e, err := sealObject(r, r.idTag("seed"), 1, m, []byte("body"))
	if err != nil {
		f.Fatal(err)
	}
	seed := e.marshal()
	f.Add(seed)
	f.Add(seed[:fixedHeaderSize])
	f.Add([]byte("GTAV"))
	f.Add([]byte{})
	bob, err := GenerateHybridKey()
	if err != nil {
		f.Fatal(err)
	}
	shared, err := SealForRecipient(bob.Public(), "s", []byte("x"), Metadata{})
	if err != nil {
		f.Fatal(err)
	}
	f.Add(shared)

	f.Fuzz(func(t *testing.T, b []byte) {
		e, err := parseEnvelope(b, 1<<20)
		if err != nil {
			return
		}
		// Parsing is canonical: a parsed envelope re-encodes to its input.
		if !bytes.Equal(e.marshal(), b) {
			t.Fatalf("non-canonical parse of %x", b)
		}
		// Decryption of arbitrary input must fail cleanly unless it is the seed.
		if _, _, err := decryptObject(r, r.idTag("seed"), e, true); err == nil && !bytes.Equal(b, seed) {
			t.Fatalf("forged envelope authenticated")
		}
		if e.hdr.wrapKind == wrapKindHybrid {
			_, _, _ = OpenSealed(bob, b)
		}
	})
}

func FuzzParseShare(f *testing.F) {
	shares, err := SplitSecret([]byte("fuzz secret"), 2, 3)
	if err != nil {
		f.Fatal(err)
	}
	enc, _ := shares[0].MarshalBinary()
	f.Add(enc)
	f.Add([]byte("GTSS"))
	f.Fuzz(func(t *testing.T, b []byte) {
		s, err := ParseShare(b)
		if err != nil {
			return
		}
		out, err := s.MarshalBinary()
		if err != nil || !bytes.Equal(out, b) {
			t.Fatalf("share re-encoding mismatch: %v", err)
		}
	})
}

func FuzzDecodeKeyring(f *testing.F) {
	id := bytes.Repeat([]byte{2}, vaultIDSize)
	r, err := newKeyring(id, time.Now())
	if err != nil {
		f.Fatal(err)
	}
	f.Add(r.marshal())
	f.Fuzz(func(t *testing.T, b []byte) {
		k, err := decodeKeyring(b, id)
		if err != nil {
			return
		}
		if !bytes.Equal(k.marshal(), b) {
			t.Fatal("keyring re-encoding mismatch")
		}
	})
}

func FuzzDecodeVaultFile(f *testing.F) {
	f.Add([]byte(`{"format":"gta-vault","version":1,"vault_id":"00112233445566778899aabbccddeeff","created":"2026-01-01T00:00:00Z","root":{"kind":"keyfile"},"keyring":"AAAA"}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		vf, err := decodeVaultFile(b)
		if err != nil {
			return
		}
		if err := vf.Root.validate(); err != nil {
			t.Fatalf("accepted invalid root: %v", err)
		}
	})
}
