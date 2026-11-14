package vault

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestVaultPutGetStatListDelete(t *testing.T) {
	v, dir := newTestVault(t, Options{})
	ctx := context.Background()

	md := Metadata{Kind: KindMemory, ContentType: "application/json", Labels: map[string]string{"project": "gta"}}
	info, err := v.Put(ctx, "memory/1", []byte(`{"fact":"x"}`), md)
	if err != nil {
		t.Fatal(err)
	}
	if info.Version != 1 || info.KeyVersion != 1 || info.Size != 12 {
		t.Fatalf("info = %+v", info)
	}
	mustPut(t, v, "session/a", []byte("hello"))
	mustPut(t, v, "session/b", bytes.Repeat([]byte("z"), 100_000))

	got, gi := mustGet(t, v, "memory/1")
	if string(got) != `{"fact":"x"}` || gi.Kind != KindMemory || gi.Labels["project"] != "gta" || gi.ContentType != "application/json" {
		t.Fatalf("get = %q %+v", got, gi)
	}

	time.Sleep(2 * time.Millisecond)
	info2, err := v.Put(ctx, "memory/1", []byte("v2"), md)
	if err != nil {
		t.Fatal(err)
	}
	if info2.Version != 2 || !info2.Created.Equal(info.Created) || !info2.Modified.After(info.Modified) {
		t.Fatalf("replacement info = %+v (first %+v)", info2, info)
	}
	st, err := v.Stat(ctx, "memory/1")
	if err != nil || st.Size != 2 || st.Version != 2 {
		t.Fatalf("stat = %+v %v", st, err)
	}

	all, err := v.List(ctx, "")
	if err != nil || len(all) != 3 || all[0].ID != "memory/1" || all[2].ID != "session/b" {
		t.Fatalf("list = %+v %v", all, err)
	}
	sessions, err := v.List(ctx, "session/")
	if err != nil || len(sessions) != 2 {
		t.Fatalf("list prefix = %+v %v", sessions, err)
	}

	if err := v.Delete(ctx, "session/a"); err != nil {
		t.Fatal(err)
	}
	_, _, err = v.Get(ctx, "session/a")
	wantErr(t, err, ErrNotFound)
	wantErr(t, v.Delete(ctx, "session/a"), ErrNotFound)

	// File names do not reveal ids, and permissions are private.
	entries, _ := os.ReadDir(filepath.Join(dir, objectsDirName))
	for _, e := range entries {
		if bytes.Contains([]byte(e.Name()), []byte("session")) {
			t.Fatalf("object file name leaks id: %s", e.Name())
		}
		fi, _ := e.Info()
		if permissionsEnforced() && fi.Mode().Perm() != 0o600 {
			t.Fatalf("object mode %v", fi.Mode().Perm())
		}
		raw, _ := os.ReadFile(filepath.Join(dir, objectsDirName, e.Name()))
		if bytes.Contains(raw, []byte("session")) || bytes.Contains(raw, []byte("zzzz")) {
			t.Fatal("plaintext visible in object file")
		}
	}
	for _, p := range []string{dir, filepath.Join(dir, objectsDirName)} {
		fi, _ := os.Stat(p)
		if permissionsEnforced() && fi.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode %v", p, fi.Mode().Perm())
		}
	}
}

func TestVaultValidation(t *testing.T) {
	v, _ := newTestVault(t, Options{MaxObjectSize: 10})
	ctx := context.Background()
	for _, id := range []string{"", string(make([]byte, 256)), "a\nb", "\xff"} {
		_, err := v.Put(ctx, id, nil, Metadata{})
		wantErr(t, err, ErrInvalidID)
	}
	_, err := v.Put(ctx, "big", make([]byte, 11), Metadata{})
	wantErr(t, err, ErrTooLarge)
	_, err = v.Put(ctx, "labels", nil, Metadata{Labels: map[string]string{"": "x"}})
	wantErr(t, err, ErrInvalidArgument)
	_, err = Create(ctx, v.Dir(), Passphrase(testPass()), CreateOptions{KDF: testKDF})
	wantErr(t, err, ErrExists)
	_, err = Create(ctx, filepath.Join(t.TempDir(), "w"), Passphrase(testPass()), CreateOptions{KDF: KDFOptions{Iterations: 1000}})
	wantErr(t, err, ErrInvalidArgument)
}

func TestVaultReopenAndWrongPassphrase(t *testing.T) {
	v, dir := newTestVault(t, Options{})
	mustPut(t, v, "tok", []byte("ghp_secret"))
	v.Lock()
	_, _, err := v.Get(context.Background(), "tok")
	wantErr(t, err, ErrLocked)

	v2, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !v2.Locked() {
		t.Fatal("Open returned an unlocked vault")
	}
	wantErr(t, v2.Unlock(context.Background(), Passphrase([]byte("wrong"))), ErrWrongKey)
	wantErr(t, v2.Unlock(context.Background(), KeyFile("/nonexistent")), ErrKeySourceMismatch)
	if err := v2.Unlock(context.Background(), Passphrase(testPass())); err != nil {
		t.Fatal(err)
	}
	got, _ := mustGet(t, v2, "tok")
	if string(got) != "ghp_secret" {
		t.Fatalf("got %q", got)
	}
}

func TestPBKDF2ParamsPersisted(t *testing.T) {
	_, dir := newTestVault(t, Options{})
	raw, err := os.ReadFile(filepath.Join(dir, vaultFileName))
	if err != nil {
		t.Fatal(err)
	}
	var vf vaultFile
	if err := json.Unmarshal(raw, &vf); err != nil {
		t.Fatal(err)
	}
	if vf.Root.Kind != rootKindPassphrase || vf.Root.KDF != KDFPBKDF2SHA256 ||
		vf.Root.Iterations != testIterations || len(vf.Root.Salt) != kdfSaltSize {
		t.Fatalf("root params = %+v", vf.Root)
	}
	if bytes.Contains(raw, testPass()) {
		t.Fatal("passphrase in vault.json")
	}
	v, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Unlock(context.Background(), Passphrase(testPass())); err != nil {
		t.Fatal(err)
	}
	st, err := v.KeyStatus()
	if err != nil || st.KDFIterations != testIterations || st.RootKind != rootKindPassphrase {
		t.Fatalf("status = %+v %v", st, err)
	}
}

func TestInsecurePermissionsRejected(t *testing.T) {
	if !permissionsEnforced() {
		t.Skip("no unix permissions")
	}
	v, dir := newTestVault(t, Options{})
	v.Lock()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Open(dir, Options{})
	wantErr(t, err, ErrInsecurePermissions)
	_ = os.Chmod(dir, 0o700)
	if err := os.Chmod(filepath.Join(dir, vaultFileName), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Open(dir, Options{})
	wantErr(t, err, ErrInsecurePermissions)
}

func TestKeyFileVault(t *testing.T) {
	tmp := t.TempDir()
	kf := filepath.Join(tmp, "master.key")
	if err := GenerateKeyFile(kf); err != nil {
		t.Fatal(err)
	}
	wantErr(t, GenerateKeyFile(kf), ErrExists)
	ctx := context.Background()
	dir := filepath.Join(tmp, "v")
	v, err := Create(ctx, dir, KeyFile(kf), CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mustPut(t, v, "a", []byte("b"))
	v.Lock()
	if err := v.Unlock(ctx, KeyFile(kf)); err != nil {
		t.Fatal(err)
	}
	mustGet(t, v, "a")

	other := filepath.Join(tmp, "other.key")
	if err := GenerateKeyFile(other); err != nil {
		t.Fatal(err)
	}
	wantErr(t, v.Unlock(ctx, KeyFile(other)), ErrWrongKey)
	wantErr(t, v.Unlock(ctx, Passphrase([]byte("x"))), ErrKeySourceMismatch)
	if permissionsEnforced() {
		if err := os.Chmod(kf, 0o644); err != nil {
			t.Fatal(err)
		}
		wantErr(t, v.Unlock(ctx, KeyFile(kf)), ErrInsecurePermissions)
	}
	short := filepath.Join(tmp, "short.key")
	if err := os.WriteFile(short, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantErr(t, v.Unlock(ctx, KeyFile(short)), ErrInvalidArgument)
}

func TestKMSVault(t *testing.T) {
	ctx := context.Background()
	kms, err := NewMemoryKMS("test-kms-key")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "v")
	v, err := Create(ctx, dir, KMSKey(kms), CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mustPut(t, v, "a", []byte("kms-protected"))
	v.Lock()

	// KMS-side rotation keeps old wraps working; Rotate re-seals with the new KMS version.
	if _, err := kms.Rotate(); err != nil {
		t.Fatal(err)
	}
	if err := v.Unlock(ctx, KMSKey(kms)); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Rotate(ctx); err != nil {
		t.Fatal(err)
	}
	kms.Destroy(1)
	v.Lock()
	if err := v.Unlock(ctx, KMSKey(kms)); err != nil {
		t.Fatalf("unlock after KMS rotation: %v", err)
	}
	got, _ := mustGet(t, v, "a")
	if string(got) != "kms-protected" {
		t.Fatal("bad data")
	}

	sameName, _ := NewMemoryKMS("test-kms-key")
	wantErr(t, v.Unlock(ctx, KMSKey(sameName)), ErrWrongKey)
	otherName, _ := NewMemoryKMS("other")
	wantErr(t, v.Unlock(ctx, KMSKey(otherName)), ErrKeySourceMismatch)
	_, err = v.MasterShares(ctx, KMSKey(kms), 2, 3)
	wantErr(t, err, ErrKeySourceMismatch)
}

func TestIdleTimeoutLocks(t *testing.T) {
	v, _ := newTestVault(t, Options{IdleTimeout: 60 * time.Millisecond})
	// Activity keeps it unlocked.
	for range 6 {
		mustPut(t, v, "k", []byte("v"))
		time.Sleep(20 * time.Millisecond)
	}
	if v.Locked() {
		t.Fatal("locked despite activity")
	}
	deadline := time.Now().Add(2 * time.Second)
	for !v.Locked() {
		if time.Now().After(deadline) {
			t.Fatal("vault did not lock after idle timeout")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, _, err := v.Get(context.Background(), "k")
	wantErr(t, err, ErrLocked)
	if err := v.Unlock(context.Background(), Passphrase(testPass())); err != nil {
		t.Fatal(err)
	}
	mustGet(t, v, "k")
}

func TestLockWipesKeyMaterial(t *testing.T) {
	v, _ := newTestVault(t, Options{})
	v.mu.RLock()
	ring := v.ring
	sealer := v.sealer.(*localSealer)
	v.mu.RUnlock()
	kekKey, wrapKey, indexKey, sealKey := ring.keks[0].key, ring.keks[0].wrapKey, ring.indexKey, sealer.key
	v.Lock()
	for name, b := range map[string][]byte{"kek": kekKey, "wrap": wrapKey, "index": indexKey, "seal": sealKey} {
		if !isZero(b) {
			t.Errorf("%s key not wiped", name)
		}
	}
	if _, err := v.KeyStatus(); !errors.Is(err, ErrLocked) {
		t.Fatal(err)
	}
}

func TestRekeyAndMasterShareRecovery(t *testing.T) {
	ctx := context.Background()
	v, dir := newTestVault(t, Options{})
	mustPut(t, v, "a", []byte("payload"))

	newPass := []byte("new passphrase 2026")
	if err := v.Rekey(ctx, Passphrase(newPass), testKDF); err != nil {
		t.Fatal(err)
	}
	v.Lock()
	wantErr(t, v.Unlock(ctx, Passphrase(testPass())), ErrWrongKey)
	if err := v.Unlock(ctx, Passphrase(newPass)); err != nil {
		t.Fatal(err)
	}
	mustGet(t, v, "a")

	_, err := v.MasterShares(ctx, Passphrase(testPass()), 2, 3)
	wantErr(t, err, ErrWrongKey)
	shares, err := v.MasterShares(ctx, Passphrase(newPass), 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	v.Lock()

	// Round-trip through the text form, then recover with shares 1 and 3.
	var texts []string
	for _, s := range shares {
		txt, err := s.Encode()
		if err != nil {
			t.Fatal(err)
		}
		texts = append(texts, txt)
	}
	var subset []Share
	for _, i := range []int{0, 2} {
		s, err := ParseShareString(texts[i])
		if err != nil {
			t.Fatal(err)
		}
		subset = append(subset, s)
	}
	master, err := CombineShares(subset)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := v2.Unlock(ctx, MasterKey(master)); err != nil {
		t.Fatal(err)
	}
	got, _ := mustGet(t, v2, "a")
	if string(got) != "payload" {
		t.Fatal("recovery returned wrong data")
	}
	// Recovered access can set a new passphrase.
	if err := v2.Rekey(ctx, Passphrase([]byte("after recovery")), testKDF); err != nil {
		t.Fatal(err)
	}
	_, err = Create(ctx, filepath.Join(t.TempDir(), "x"), MasterKey(master), CreateOptions{})
	wantErr(t, err, ErrInvalidArgument)
}

func TestObjectSwapDetected(t *testing.T) {
	v, _ := newTestVault(t, Options{})
	mustPut(t, v, "alice", []byte("alice's token"))
	mustPut(t, v, "bob", []byte("bob's token"))
	pa, pb := objectPathFor(t, v, "alice"), objectPathFor(t, v, "bob")
	raw, err := os.ReadFile(pa)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pb, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = v.Get(context.Background(), "bob")
	wantErr(t, err, ErrCorrupt)
	infos, err := v.List(context.Background(), "")
	wantErr(t, err, ErrCorrupt)
	if len(infos) != 1 || infos[0].ID != "alice" {
		t.Fatalf("list returned %+v", infos)
	}
}

func TestTamperedFileOnDisk(t *testing.T) {
	v, _ := newTestVault(t, Options{DisableLazyRewrap: true})
	mustPut(t, v, "obj", []byte("0123456789"))
	path := objectPathFor(t, v, "obj")
	orig, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(orig); i += 7 {
		mod := bytes.Clone(orig)
		mod[i] ^= 0x40
		if err := os.WriteFile(path, mod, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := v.Get(context.Background(), "obj"); err == nil {
			t.Fatalf("tamper at %d not detected", i)
		}
	}
}

func TestConcurrentAccess(t *testing.T) {
	v, _ := newTestVault(t, Options{})
	ctx := context.Background()
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 10 {
				id := fmt.Sprintf("w%d/%d", w, i%3)
				data := []byte(fmt.Sprintf("%d-%d", w, i))
				if _, err := v.Put(ctx, id, data, Metadata{}); err != nil {
					t.Error(err)
					return
				}
				if _, _, err := v.Get(ctx, id); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 3 {
			if _, err := v.Rotate(ctx); err != nil {
				t.Error(err)
			}
		}
	}()
	wg.Wait()
	infos, err := v.List(ctx, "")
	if err != nil || len(infos) != 24 {
		t.Fatalf("list: %d %v", len(infos), err)
	}
}

func TestExportImport(t *testing.T) {
	ctx := context.Background()
	alice, _ := newTestVault(t, Options{})
	bob, _ := newTestVault(t, Options{})
	bobKey, err := GenerateHybridKey()
	if err != nil {
		t.Fatal(err)
	}
	mallory, _ := GenerateHybridKey()

	mustPut(t, alice, "session/42", []byte("transcript"))
	blob, err := alice.Export(ctx, "session/42", bobKey.Public())
	if err != nil {
		t.Fatal(err)
	}
	_, err = bob.Import(ctx, mallory, blob, ImportOptions{})
	wantErr(t, err, ErrWrongRecipient)

	info, err := bob.Import(ctx, bobKey, blob, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "session/42" || info.Kind != KindSession {
		t.Fatalf("import info %+v", info)
	}
	_, err = bob.Import(ctx, bobKey, blob, ImportOptions{})
	wantErr(t, err, ErrExists)
	if _, err := bob.Import(ctx, bobKey, blob, ImportOptions{ID: "shared/alice-42"}); err != nil {
		t.Fatal(err)
	}
	got, _ := mustGet(t, bob, "shared/alice-42")
	if string(got) != "transcript" {
		t.Fatal("bad import")
	}
	// A shared envelope is not accepted as a vault object.
	if err := os.WriteFile(objectPathFor(t, bob, "session/42"), blob, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = bob.Get(ctx, "session/42")
	wantErr(t, err, ErrCorrupt)
}
