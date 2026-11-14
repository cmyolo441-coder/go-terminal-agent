package vault

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
)

// MigrationMode selects how Migrate moves objects to the current KEK.
type MigrationMode int

const (
	// MigrateRewrap re-wraps each stale DEK under the current KEK. Object
	// ciphertext is untouched, so this is cheap. Use it for routine rotation.
	MigrateRewrap MigrationMode = iota
	// MigrateReencrypt decrypts each stale object and encrypts it again
	// under a fresh DEK and the current KEK. Use it when DEKs or an old KEK
	// may have been exposed.
	MigrateReencrypt
)

// MigrationOptions configure Migrate.
type MigrationOptions struct {
	Mode MigrationMode
	// Workers is the number of objects processed in parallel; zero means
	// min(4, GOMAXPROCS).
	Workers int
	// Progress, if set, is called after each object, never concurrently.
	Progress func(MigrationProgress)
	// Prune removes KEK versions that no object references once every
	// object has been migrated without failures.
	Prune bool
}

// MigrationProgress counts migration work.
type MigrationProgress struct {
	Total     int    // object files found when the migration started
	Processed int    // objects examined so far
	Migrated  int    // objects moved to the current KEK
	UpToDate  int    // objects that already used the current KEK
	Failed    int    // objects that could not be migrated
	Target    uint32 // the current KEK version when the migration started
	Pruned    []uint32
}

// Done reports whether every object has been examined.
func (p MigrationProgress) Done() bool { return p.Processed == p.Total }

// Migrate moves every object whose DEK is wrapped by an old KEK version to
// the current version. It is resumable: objects already on the current
// version are skipped, so an interrupted migration can simply be run again.
// Per-object failures are counted and returned joined; they do not stop the
// migration. The vault must stay unlocked.
func (v *Vault) Migrate(ctx context.Context, opts MigrationOptions) (MigrationProgress, error) {
	if err := ctx.Err(); err != nil {
		return MigrationProgress{}, err
	}
	if opts.Mode != MigrateRewrap && opts.Mode != MigrateReencrypt {
		return MigrationProgress{}, invalid("unknown migration mode %d", opts.Mode)
	}
	workers := opts.Workers
	if workers <= 0 {
		workers = min(4, runtime.GOMAXPROCS(0))
	}
	ring, release, err := v.acquire()
	if err != nil {
		return MigrationProgress{}, err
	}
	target := ring.current
	release()

	names, err := v.objectNames()
	if err != nil {
		return MigrationProgress{}, err
	}
	prog := MigrationProgress{Total: len(names), Target: target}

	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
	)
	jobs := make(chan string)
	record := func(migrated bool, err error, name string) {
		mu.Lock()
		defer mu.Unlock()
		prog.Processed++
		switch {
		case err != nil:
			prog.Failed++
			errs = append(errs, fmt.Errorf("vault: migrate %s: %w", name, err))
		case migrated:
			prog.Migrated++
		default:
			prog.UpToDate++
		}
		if opts.Progress != nil {
			p := prog
			p.Pruned = slices.Clone(prog.Pruned)
			opts.Progress(p)
		}
	}
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for name := range jobs {
				migrated, err := v.migrateOne(name, opts.Mode)
				record(migrated, err, name)
			}
		}()
	}
	var ctxErr error
feed:
	for _, name := range names {
		if ctxErr = ctx.Err(); ctxErr != nil {
			break
		}
		select {
		case <-ctx.Done():
			ctxErr = ctx.Err()
			break feed
		case jobs <- name:
		}
	}
	close(jobs)
	wg.Wait()

	if ctxErr != nil {
		return prog, ctxErr
	}
	if err := errors.Join(errs...); err != nil {
		return prog, err
	}
	if opts.Prune {
		pruned, err := v.PruneKeys(ctx)
		prog.Pruned = pruned
		if err != nil {
			return prog, err
		}
	}
	return prog, nil
}

// objectNames lists object file names.
func (v *Vault) objectNames() ([]string, error) {
	entries, err := os.ReadDir(v.objectsDir())
	if err != nil {
		return nil, fmt.Errorf("vault: list objects: %w", err)
	}
	var names []string
	for _, e := range entries {
		if _, ok := parseObjectName(e.Name()); ok {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// migrateOne moves one object file to the current KEK. It reports whether the
// file was rewritten.
func (v *Vault) migrateOne(name string, mode MigrationMode) (bool, error) {
	tag, _ := parseObjectName(name)
	ring, release, err := v.acquire()
	if err != nil {
		return false, err
	}
	defer release()
	mu := v.stripe(tag)
	mu.Lock()
	defer mu.Unlock()

	path := filepath.Join(v.objectsDir(), name)
	blob, err := readObjectFile(path, v.opts.MaxObjectSize)
	if errors.Is(err, ErrNotFound) {
		return false, nil // deleted while migrating
	}
	if err != nil {
		return false, err
	}
	e, err := parseEnvelope(blob, v.opts.MaxObjectSize)
	if err != nil {
		return false, err
	}
	if e.hdr.wrapKind == wrapKindKeyring && e.hdr.kekVersion == ring.current {
		return false, nil
	}
	var ne *envelope
	switch mode {
	case MigrateRewrap:
		// Authenticate the whole object before re-wrapping so corruption
		// is reported rather than carried forward.
		_, body, err := decryptObject(ring, tag, e, true)
		if err != nil {
			return false, err
		}
		wipe(body)
		if ne, err = rewrapEnvelope(ring, e); err != nil {
			return false, err
		}
	case MigrateReencrypt:
		m, body, err := decryptObject(ring, tag, e, true)
		if err != nil {
			return false, err
		}
		ne, err = sealObject(ring, tag, e.hdr.objVersion, m, body)
		wipe(body)
		if err != nil {
			return false, err
		}
	}
	if err := writeFileAtomic(v.objectsDir(), name, ne.marshal()); err != nil {
		return false, err
	}
	return true, nil
}

// PruneKeys removes KEK versions that are neither current nor referenced by
// any object, and returns the removed versions. It refuses to prune while any
// object header is unreadable, because it could not prove that object does
// not need an old version. Objects restored from backups made before a prune
// cannot be decrypted afterwards.
func (v *Vault) PruneKeys(ctx context.Context) ([]uint32, error) {
	v.adminMu.Lock()
	defer v.adminMu.Unlock()
	if v.Locked() {
		return nil, ErrLocked
	}
	names, err := v.objectNames()
	if err != nil {
		return nil, err
	}
	referenced := make(map[uint32]bool)
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		e, err := readObjectPrefix(filepath.Join(v.objectsDir(), name))
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("vault: prune: cannot read %s: %w", name, err)
		}
		referenced[e.hdr.kekVersion] = true
	}

	// Rotate is excluded by adminMu, and every write since the scan used the
	// current version, so the scan result is still valid.
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.ring == nil {
		return nil, ErrLocked
	}
	next := v.ring.clone()
	var removed []uint32
	kept := next.keks[:0]
	for _, k := range next.keks {
		if k.version == next.current || referenced[k.version] {
			kept = append(kept, k)
			continue
		}
		removed = append(removed, k.version)
		k.wipe()
	}
	next.keks = kept
	if len(removed) == 0 {
		next.wipe()
		return nil, nil
	}
	vf, err := v.persistLocked(ctx, next, v.sealer, v.file.Root)
	if err != nil {
		next.wipe()
		return nil, err
	}
	v.ring.wipe()
	v.ring, v.file = next, vf
	return removed, nil
}

// Migration is a migration running in the background.
type Migration struct {
	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	progress MigrationProgress
	err      error
}

// StartMigration runs Migrate in a new goroutine. Progress can be polled with
// Progress; Wait returns the final result.
func (v *Vault) StartMigration(ctx context.Context, opts MigrationOptions) *Migration {
	ctx, cancel := context.WithCancel(ctx)
	m := &Migration{cancel: cancel, done: make(chan struct{})}
	user := opts.Progress
	opts.Progress = func(p MigrationProgress) {
		m.mu.Lock()
		m.progress = p
		m.mu.Unlock()
		if user != nil {
			user(p)
		}
	}
	go func() {
		defer close(m.done)
		defer cancel()
		p, err := v.Migrate(ctx, opts)
		m.mu.Lock()
		m.progress, m.err = p, err
		m.mu.Unlock()
	}()
	return m
}

// Progress returns the latest progress snapshot.
func (m *Migration) Progress() MigrationProgress {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.progress
	p.Pruned = slices.Clone(p.Pruned)
	return p
}

// Cancel stops the migration after the objects in flight.
func (m *Migration) Cancel() { m.cancel() }

// Done is closed when the migration finishes.
func (m *Migration) Done() <-chan struct{} { return m.done }

// Wait blocks until the migration finishes and returns its result.
func (m *Migration) Wait() (MigrationProgress, error) {
	<-m.done
	return m.Progress(), m.errValue()
}

func (m *Migration) errValue() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.err
}
