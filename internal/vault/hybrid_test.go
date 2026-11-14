package vault

import (
	"bytes"
	"errors"
	"testing"
)

func TestHybridKEMRoundTrip(t *testing.T) {
	k, err := GenerateHybridKey()
	if err != nil {
		t.Fatal(err)
	}
	ctx := []byte("context")
	key, enc, err := HybridEncapsulate(k.Public(), ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != 32 || len(enc) != HybridEncapsulationSize {
		t.Fatalf("sizes %d %d", len(key), len(enc))
	}
	got, err := HybridDecapsulate(k, enc, ctx)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("decapsulate: %v", err)
	}
	// A different context, recipient or tampered component changes the key.
	if g, _ := HybridDecapsulate(k, enc, []byte("other")); bytes.Equal(g, key) {
		t.Fatal("context not bound")
	}
	other, _ := GenerateHybridKey()
	if g, _ := HybridDecapsulate(other, enc, ctx); bytes.Equal(g, key) {
		t.Fatal("wrong recipient derived the same key")
	}
	for _, i := range []int{0, 31, 32, 500, HybridEncapsulationSize - 1} {
		mod := bytes.Clone(enc)
		mod[i] ^= 1
		if g, err := HybridDecapsulate(k, mod, ctx); err == nil && bytes.Equal(g, key) {
			t.Fatalf("tamper at %d yields same key", i)
		}
	}
	// Two encapsulations to the same key differ.
	key2, enc2, _ := HybridEncapsulate(k.Public(), ctx)
	if bytes.Equal(key, key2) || bytes.Equal(enc, enc2) {
		t.Fatal("encapsulation is not randomized")
	}
}

func TestHybridKeySerialization(t *testing.T) {
	k, err := GenerateHybridKey()
	if err != nil {
		t.Fatal(err)
	}
	priv := k.Bytes()
	if len(priv) != HybridPrivateKeySize {
		t.Fatalf("private size %d", len(priv))
	}
	k2, err := ParseHybridPrivateKey(priv)
	if err != nil || !k2.Public().Equal(k.Public()) {
		t.Fatalf("private round trip: %v", err)
	}
	pub := k.Public().Bytes()
	if len(pub) != HybridPublicKeySize {
		t.Fatalf("public size %d", len(pub))
	}
	p2, err := ParseHybridPublicKey(pub)
	if err != nil || !p2.Equal(k.Public()) || p2.Fingerprint() != k.Public().Fingerprint() {
		t.Fatalf("public round trip: %v", err)
	}
	if _, err := ParseHybridPublicKey(pub[:len(pub)-1]); !errors.Is(err, ErrCorrupt) {
		t.Fatal("short public key accepted")
	}
	bad := bytes.Clone(pub)
	bad[0] = 'X'
	if _, err := ParseHybridPublicKey(bad); !errors.Is(err, ErrCorrupt) {
		t.Fatal("bad magic accepted")
	}
	// Decapsulation with a parsed key matches the original.
	key, enc, _ := HybridEncapsulate(p2, nil)
	got, err := HybridDecapsulate(k2, enc, nil)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatal("parsed keys disagree")
	}
}

func TestSealForRecipient(t *testing.T) {
	bob, _ := GenerateHybridKey()
	eve, _ := GenerateHybridKey()
	md := Metadata{Kind: KindSession, Labels: map[string]string{"from": "alice"}}
	blob, err := SealForRecipient(bob.Public(), "session/7", []byte("shared transcript"), md)
	if err != nil {
		t.Fatal(err)
	}
	if blob[6] != wrapKindHybrid || len(blob) < fixedHeaderSize+hybridWrappedDEKSize {
		t.Fatal("unexpected layout")
	}
	info, body, err := OpenSealed(bob, blob)
	if err != nil || string(body) != "shared transcript" || info.ID != "session/7" || info.Labels["from"] != "alice" {
		t.Fatalf("open: %+v %q %v", info, body, err)
	}
	_, _, err = OpenSealed(eve, blob)
	wantErr(t, err, ErrWrongRecipient)

	// Every byte is covered.
	for i := range blob {
		mod := bytes.Clone(blob)
		mod[i] ^= 0x01
		if _, _, err := OpenSealed(bob, mod); err == nil {
			t.Fatalf("tamper at byte %d not detected", i)
		}
	}
	// A wrong-recipient attempt with a forged fingerprint still fails.
	forged := bytes.Clone(blob)
	fp := eve.Public().Fingerprint()
	copy(forged[fixedHeaderSize:], fp[:])
	if _, _, err := OpenSealed(eve, forged); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("forged fingerprint: %v", err)
	}
}
