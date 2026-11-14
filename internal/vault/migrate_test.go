package vault

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sync"
	"testing"
)

func keyVersionOnDisk(t *testing.T, v *Vault, id string) (uint32, [dekIDSize]byte) {
	t.Helper()
	e, err := readObjectPrefix(objectPathFor(t, v, id))
	if err != nil {
		t.Fatal(err)
	}
	return e.hdr.kekVersion, e.hdr.dekID
}

func TestRotationMixedVersions(t *testing.T) {
	ctx := context.Background()
	v, dir := newTestVault(t, Options{DisableLazyRewrap: true})
	mustPut(t, v, "a", []byte("A"))
	if ver, err := v.Rotate(ctx); err != nil || ver != 2 {
		t.Fatalf("rotate: %d %v", ver, err)
	}
	mustPut(t, v, "b", []byte("B"))
	if ver, err := v.Rotate(ctx); err != nil || ver != 3 {
		t.Fatalf("rotate: %d %v", ver, err)
	}
	mustPut(t, v, "c", []byte("C"))

	want := map[string]uint32{"a": 1, "b": 2, "c": 3}
	for id, kv := range want {
		got, info := mustGet(t, v, id)
		if string(got) != string(rune(id[0]-32)) || info.KeyVersion != kv {
			t.Fatalf("%s: %q key %d", id, got, info.KeyVersion)
		}
		if d, _ := keyVersionOnDisk(t, v, id); d != kv {
			t.Fatalf("%s on disk at %d with lazy rewrap disabled", id, d)
		}
	}

	// Mixed versions survive a lock/reopen cycle.
	v.Lock()
	v2, err := Open(dir, Options{DisableLazyRewrap: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := v2.Unlock(ctx, Passphrase(testPass())); err != nil {
		t.Fatal(err)
	}
	st, err := v2.KeyStatus()
	if err != nil || st.Current != 3 || len(st.Versions) != 3 || !st.Versions[2].Current {
		t.Fatalf("status %+v %v", st, err)
	}
	for id := range want {
		mustGet(t, v2, id)
	}

	// Pruning before migration must keep every referenced version.
	removed, err := v2.PruneKeys(ctx)
	if err != nil || len(removed) != 0 {
		t.Fatalf("premature prune removed %v (%v)", removed, err)
	}
}

func TestLazyRewrap(t *testing.T) {
	ctx := context.Background()
	v, _ := newTestVault(t, Options{})
	mustPut(t, v, "a", []byte("data"))
	_, dek1 := keyVersionOnDisk(t, v, "a")
	if _, err := v.Rotate(ctx); err != nil {
		t.Fatal(err)
	}
	_, info := mustGet(t, v, "a")
	if info.KeyVersion != 2 {
		t.Fatalf("info key version %d", info.KeyVersion)
	}
	kv, dek2 := keyVersionOnDisk(t, v, "a")
	if kv != 2 || dek1 != dek2 {
		t.Fatalf("after lazy rewrap: kek %d, same DEK %v", kv, dek1 == dek2)
	}
	got, _ := mustGet(t, v, "a")
	if string(got) != "data" {
		t.Fatal("bad data after rewrap")
	}
}

func TestMigrateRewrapAndPrune(t *testing.T) {
	ctx := context.Background()
	v, _ := newTestVault(t, Options{DisableLazyRewrap: true})
	const n = 25
	for i := range n {
		mustPut(t, v, fmt.Sprintf("obj%02d", i), []byte(fmt.Sprintf("payload %d", i)))
		if i%10 == 9 {
			if _, err := v.Rotate(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := v.Rotate(ctx); err != nil {
		t.Fatal(err)
	}
	var (
		mu    sync.Mutex
		snaps []MigrationProgress
	)
	prog, err := v.Migrate(ctx, MigrationOptions{
		Mode:    MigrateRewrap,
		Workers: 3,
		Prune:   true,
		Progress: func(p MigrationProgress) {
			mu.Lock()
			snaps = append(snaps, p)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if prog.Total != n || prog.Migrated != n || prog.Failed != 0 || !prog.Done() || prog.Target != 4 {
		t.Fatalf("progress %+v", prog)
	}
	if !slices.Equal(prog.Pruned, []uint32{1, 2, 3}) {
		t.Fatalf("pruned %v", prog.Pruned)
	}
	if len(snaps) != n || snaps[n-1].Processed != n {
		t.Fatalf("%d progress callbacks", len(snaps))
	}
	for i := 1; i < len(snaps); i++ {
		if snaps[i].Processed != snaps[i-1].Processed+1 {
			t.Fatal("progress not monotonic")
		}
	}
	st, _ := v.KeyStatus()
	if len(st.Versions) != 1 || st.Current != 4 {
		t.Fatalf("status after prune %+v", st)
	}
	for i := range n {
		id := fmt.Sprintf("obj%02d", i)
		got, info := mustGet(t, v, id)
		if string(got) != fmt.Sprintf("payload %d", i) || info.KeyVersion != 4 {
			t.Fatalf("%s: %q %d", id, got, info.KeyVersion)
		}
	}
	// Second run is a no-op.
	prog, err = v.Migrate(ctx, MigrationOptions{})
	if err != nil || prog.Migrated != 0 || prog.UpToDate != n {
		t.Fatalf("rerun %+v %v", prog, err)
	}
}

func TestMigrateReencryptChangesDEK(t *testing.T) {
	ctx := context.Background()
	v, _ := newTestVault(t, Options{DisableLazyRewrap: true})
	info := mustPut(t, v, "a", []byte("data"))
	_, dek1 := keyVersionOnDisk(t, v, "a")
	if _, err := v.Rotate(ctx); err != nil {
		t.Fatal(err)
	}
	m := v.StartMigration(ctx, MigrationOptions{Mode: MigrateReencrypt, Prune: true})
	prog, err := m.Wait()
	if err != nil || prog.Migrated != 1 || len(prog.Pruned) != 1 {
		t.Fatalf("%+v %v", prog, err)
	}
	kv, dek2 := keyVersionOnDisk(t, v, "a")
	if kv != 2 || dek1 == dek2 {
		t.Fatalf("reencrypt: kek %d, DEK changed %v", kv, dek1 != dek2)
	}
	got, gi := mustGet(t, v, "a")
	if string(got) != "data" || gi.Version != info.Version || !gi.Created.Equal(info.Created) {
		t.Fatalf("after reencrypt %q %+v", got, gi)
	}
}

func TestMigrateReportsCorruptAndDoesNotPrune(t *testing.T) {
	ctx := context.Background()
	v, _ := newTestVault(t, Options{DisableLazyRewrap: true})
	mustPut(t, v, "good", []byte("g"))
	mustPut(t, v, "bad", []byte("b"))
	if _, err := v.Rotate(ctx); err != nil {
		t.Fatal(err)
	}
	p := objectPathFor(t, v, "bad")
	raw, _ := os.ReadFile(p)
	raw[len(raw)-1] ^= 1
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	prog, err := v.Migrate(ctx, MigrationOptions{Prune: true})
	wantErr(t, err, ErrCorrupt)
	if prog.Failed != 1 || prog.Migrated != 1 || len(prog.Pruned) != 0 {
		t.Fatalf("%+v", prog)
	}
	st, _ := v.KeyStatus()
	if len(st.Versions) != 2 {
		t.Fatal("old version pruned despite failure")
	}
}

func TestMigrateCancel(t *testing.T) {
	v, _ := newTestVault(t, Options{DisableLazyRewrap: true})
	for i := range 5 {
		mustPut(t, v, fmt.Sprintf("o%d", i), []byte("x"))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := v.Migrate(ctx, MigrationOptions{})
	wantErr(t, err, context.Canceled)
}
