package identity

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func testKeySource(t *testing.T, b byte) KeySource {
	t.Helper()
	ks, err := NewStaticKeySource(bytes.Repeat([]byte{b}, MasterKeySize))
	if err != nil {
		t.Fatal(err)
	}
	return ks
}

func TestFileTokenStoreRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tokens")
	s, err := NewFileTokenStore(dir, testKeySource(t, 1))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.Load(ctx, "k"); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("missing: %v", err)
	}
	in := &Token{AccessToken: "secret-access", TokenType: "Bearer", RefreshToken: "secret-refresh", Expiry: time.Unix(1_900_000_000, 0).UTC(), Issuer: "https://i", Subject: "u"}
	if err := s.Save(ctx, "k", in); err != nil {
		t.Fatal(err)
	}
	out, err := s.Load(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if *out != *in {
		t.Fatalf("round trip: %+v != %+v", out, in)
	}
	path := s.path("k")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("secret")) {
		t.Fatal("token stored in plaintext")
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(path)
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v", fi.Mode().Perm())
		}
		di, _ := os.Stat(dir)
		if di.Mode().Perm() != 0o700 {
			t.Fatalf("dir mode = %v", di.Mode().Perm())
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %d entries", len(entries))
	}

	// Wrong master key.
	s2, _ := NewFileTokenStore(dir, testKeySource(t, 2))
	if _, err := s2.Load(ctx, "k"); err == nil || !strings.Contains(err.Error(), "decryption failed") {
		t.Fatalf("wrong key: %v", err)
	}
	// A file copied under another key's name fails (key bound as AAD).
	if err := os.WriteFile(s.path("other"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(ctx, "other"); err == nil {
		t.Fatal("swapped file decrypted")
	}
	// Tampering is detected.
	tampered := bytes.Replace(raw, []byte(`"ct":"`), []byte(`"ct":"AAAA`), 1)
	_ = os.WriteFile(path, tampered, 0o600)
	if _, err := s.Load(ctx, "k"); err == nil {
		t.Fatal("tampered file accepted")
	}
	if runtime.GOOS != "windows" {
		_ = s.Save(ctx, "k", in)
		_ = os.Chmod(path, 0o644)
		if _, err := s.Load(ctx, "k"); err == nil || !strings.Contains(err.Error(), "insecure permissions") {
			t.Fatalf("world-readable file: %v", err)
		}
	}
	if err := s.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "k"); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

func TestFileTokenStoreConcurrent(t *testing.T) {
	s, err := NewFileTokenStore(t.TempDir(), testKeySource(t, 3))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok := &Token{AccessToken: strings.Repeat("a", i+1), TokenType: "Bearer"}
			if err := s.Save(ctx, "shared", tok); err != nil {
				t.Error(err)
			}
			if _, err := s.Load(ctx, "shared"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

func TestKeySources(t *testing.T) {
	if _, err := NewStaticKeySource([]byte("short")); err == nil {
		t.Fatal("short key accepted")
	}
	path := filepath.Join(t.TempDir(), "sub", "master.key")
	fks := &FileKeySource{Path: path, Create: true}
	k1, err := fks.Key(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	k2, err := (&FileKeySource{Path: path}).Key(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(k1, k2) || len(k1) != MasterKeySize {
		t.Fatal("created key not reused")
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(path, 0o640)
		if _, err := (&FileKeySource{Path: path}).Key(context.Background()); err == nil {
			t.Fatal("group-readable key file accepted")
		}
	}
	if _, err := (&FileKeySource{Path: filepath.Join(t.TempDir(), "none")}).Key(context.Background()); err == nil {
		t.Fatal("missing key file without Create accepted")
	}
	t.Setenv("GTA_TEST_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	if k, err := (EnvKeySource{Name: "GTA_TEST_KEY"}).Key(context.Background()); err != nil || k[0] != 7 {
		t.Fatalf("env key: %v", err)
	}
	t.Setenv("GTA_TEST_KEY", "bm90IDMyIGJ5dGVz")
	if _, err := (EnvKeySource{Name: "GTA_TEST_KEY"}).Key(context.Background()); err == nil {
		t.Fatal("short env key accepted")
	}
	if _, err := (EnvKeySource{Name: "GTA_TEST_KEY_UNSET"}).Key(context.Background()); err == nil {
		t.Fatal("unset env key accepted")
	}
}

func TestTokenExpired(t *testing.T) {
	now := time.Now()
	var nilTok *Token
	if !nilTok.Expired(now, 0) {
		t.Fatal("nil token not expired")
	}
	if (&Token{AccessToken: "a"}).Expired(now, time.Hour) {
		t.Fatal("token without expiry reported expired")
	}
	if !(&Token{AccessToken: "a", Expiry: now.Add(30 * time.Second)}).Expired(now, time.Minute) {
		t.Fatal("skew not applied")
	}
}
