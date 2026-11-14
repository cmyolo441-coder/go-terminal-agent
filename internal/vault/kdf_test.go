package vault

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestCalibratePBKDF2(t *testing.T) {
	start := time.Now()
	n := CalibratePBKDF2(time.Millisecond)
	if n != MinKDFIterations {
		t.Fatalf("tiny target gave %d, want floor %d", n, MinKDFIterations)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("calibration too slow")
	}
	if got := clampIterations(1e12); got != MaxKDFIterations {
		t.Fatalf("clamp high = %d", got)
	}
	if got := clampIterations(300_001); got != 301_000 || got%1000 != 0 {
		t.Fatalf("rounding = %d", got)
	}
}

func TestPBKDF2Deterministic(t *testing.T) {
	salt := bytes.Repeat([]byte{9}, kdfSaltSize)
	a, err := pbkdf2Key([]byte("pw"), salt, 10)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := pbkdf2Key([]byte("pw"), salt, 10)
	c, _ := pbkdf2Key([]byte("pw"), salt, 11)
	if !bytes.Equal(a, b) || bytes.Equal(a, c) || len(a) != MasterKeySize {
		t.Fatal("pbkdf2 output inconsistent")
	}
	if _, err := pbkdf2Key(nil, salt, 10); !errors.Is(err, ErrInvalidArgument) {
		t.Fatal("empty passphrase accepted")
	}
	if _, err := pbkdf2Key([]byte("pw"), salt, MaxKDFIterations+1); !errors.Is(err, ErrInvalidArgument) {
		t.Fatal("excessive iterations accepted")
	}
	// RFC 7914 section 11 PBKDF2-HMAC-SHA256 test vector (c=1, dkLen=64), first 32 bytes.
	want := []byte{
		0x55, 0xac, 0x04, 0x6e, 0x56, 0xe3, 0x08, 0x9f, 0xec, 0x16, 0x91, 0xc2, 0x25, 0x44, 0xb6, 0x05,
		0xf9, 0x41, 0x85, 0x21, 0x6d, 0xde, 0x04, 0x65, 0xe6, 0x8b, 0x9d, 0x57, 0xc2, 0x0d, 0xac, 0xbc,
	}
	got, err := pbkdf2Key([]byte("passwd"), []byte("salt"), 1)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("RFC 7914 vector mismatch: %x", got)
	}
}

func TestDeriveKeySeparation(t *testing.T) {
	secret := bytes.Repeat([]byte{3}, 32)
	a, err := DeriveKey(secret, nil, "purpose-a", 32)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := DeriveKey(secret, nil, "purpose-b", 32)
	c, _ := DeriveKey(secret, []byte("salt"), "purpose-a", 32)
	if bytes.Equal(a, b) || bytes.Equal(a, c) {
		t.Fatal("purposes or salts not separated")
	}
	for _, tc := range []struct {
		s []byte
		p string
		n int
	}{{secret[:8], "p", 32}, {secret, "", 32}, {secret, "p", 8}, {secret, "p", 65}} {
		if _, err := DeriveKey(tc.s, nil, tc.p, tc.n); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("%+v accepted", tc)
		}
	}
}

func TestMemoryKMS(t *testing.T) {
	ctx := context.Background()
	k, err := NewMemoryKMS("k1")
	if err != nil {
		t.Fatal(err)
	}
	w, err := k.Wrap(ctx, []byte("secret"), []byte("aad"))
	if err != nil {
		t.Fatal(err)
	}
	if pt, err := k.Unwrap(ctx, w, []byte("aad")); err != nil || string(pt) != "secret" {
		t.Fatalf("unwrap %v", err)
	}
	if _, err := k.Unwrap(ctx, w, []byte("other")); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("aad not bound: %v", err)
	}
	w[len(w)-1] ^= 1
	if _, err := k.Unwrap(ctx, w, []byte("aad")); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("tamper: %v", err)
	}
	if _, err := NewMemoryKMS(""); !errors.Is(err, ErrInvalidArgument) {
		t.Fatal("empty id accepted")
	}
}
