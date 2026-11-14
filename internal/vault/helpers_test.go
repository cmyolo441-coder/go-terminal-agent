package vault

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

const testIterations = 1000

var testKDF = KDFOptions{Iterations: testIterations, AllowWeakKDF: true}

func testPass() []byte { return []byte("correct horse battery staple") }

func newTestVault(t *testing.T, opts Options) (*Vault, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "vault")
	v, err := Create(context.Background(), dir, Passphrase(testPass()), CreateOptions{Options: opts, KDF: testKDF})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = v.Close() })
	return v, dir
}

func mustPut(t *testing.T, v *Vault, id string, data []byte) Info {
	t.Helper()
	info, err := v.Put(context.Background(), id, data, Metadata{Kind: KindSession})
	if err != nil {
		t.Fatalf("Put(%q): %v", id, err)
	}
	return info
}

func mustGet(t *testing.T, v *Vault, id string) ([]byte, Info) {
	t.Helper()
	b, info, err := v.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get(%q): %v", id, err)
	}
	return b, info
}

func wantErr(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("error = %v, want %v", err, target)
	}
}

// objectPathFor returns the file path of id; the vault must be unlocked.
func objectPathFor(t *testing.T, v *Vault, id string) string {
	t.Helper()
	ring, release, err := v.acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	return filepath.Join(v.objectsDir(), objectName(ring.idTag(id)))
}
