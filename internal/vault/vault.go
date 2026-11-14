package vault

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	vaultFileName   = "vault.json"
	objectsDirName  = "objects"
	vaultFormatName = "gta-vault"
	vaultFormatV1   = 1
	vaultIDSize     = 16
	maxVaultFile    = 1 << 20
	stripeCount     = 64
)

// Options configure an opened vault.
type Options struct {
	// IdleTimeout locks the vault after this long without an operation.
	// Zero disables automatic locking.
	IdleTimeout time.Duration
	// DisableLazyRewrap stops Get from re-wrapping DEKs that use an old KEK.
	DisableLazyRewrap bool
	// MaxObjectSize limits object bodies; zero means DefaultMaxObjectSize.
	MaxObjectSize int64
	// OnBackgroundError receives errors from best-effort work that does not
	// fail the calling operation (lazy re-wrap). It must not call back into
	// the vault synchronously.
	OnBackgroundError func(op, id string, err error)
}

// CreateOptions configure Create.
type CreateOptions struct {
	Options
	KDF KDFOptions
}

// vaultFile is the JSON stored in vault.json.
type vaultFile struct {
	Format  string     `json:"format"`
	Version int        `json:"version"`
	VaultID string     `json:"vault_id"`
	Created time.Time  `json:"created"`
	Root    rootParams `json:"root"`
	Keyring []byte     `json:"keyring"`
}

func (f *vaultFile) id() ([]byte, error) {
	id, err := hex.DecodeString(f.VaultID)
	if err != nil || len(id) != vaultIDSize {
		return nil, corrupt("bad vault id")
	}
	return id, nil
}

// Vault is an encrypted object store rooted at a directory. It is safe for
// concurrent use. A Vault starts locked (Open) or unlocked (Create); most
// operations need it unlocked.
type Vault struct {
	dir  string
	opts Options

	// adminMu serializes operations that rewrite vault.json.
	adminMu sync.Mutex

	// mu guards the fields below. Object operations hold it for reading for
	// their whole duration, so Lock waits for them and never wipes keys in
	// use.
	mu      sync.RWMutex
	file    *vaultFile
	vaultID []byte
	ring    *keyring
	sealer  rootSealer
	gen     uint64
	idle    *time.Timer

	lastUse atomic.Int64
	stripes [stripeCount]sync.Mutex
}

// Create initializes a new vault in dir (created with mode 0700 if needed)
// and returns it unlocked. It fails with ErrExists if dir already holds one.
func Create(ctx context.Context, dir string, src KeySource, opts CreateOptions) (*Vault, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	o, err := normalizeOptions(opts.Options)
	if err != nil {
		return nil, err
	}
	params, err := newRootParams(src, opts.KDF)
	if err != nil {
		return nil, err
	}
	if err := ensurePrivateDir(dir); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(filepath.Join(dir, vaultFileName)); err == nil {
		return nil, fmt.Errorf("%w: %s", ErrExists, dir)
	}
	if err := ensurePrivateDir(filepath.Join(dir, objectsDirName)); err != nil {
		return nil, err
	}
	vaultID := make([]byte, vaultIDSize)
	if err := randomBytes(vaultID); err != nil {
		return nil, err
	}
	sealer, err := newSealer(ctx, src, params, vaultID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	ring, err := newKeyring(vaultID, now)
	if err != nil {
		sealer.wipe()
		return nil, err
	}
	vf := &vaultFile{
		Format:  vaultFormatName,
		Version: vaultFormatV1,
		VaultID: hex.EncodeToString(vaultID),
		Created: now,
		Root:    *params,
	}
	data, err := sealKeyringFile(ctx, vf, vaultID, ring, sealer)
	if err == nil {
		err = createFileExclusive(dir, vaultFileName, data)
	}
	if err != nil {
		ring.wipe()
		sealer.wipe()
		return nil, err
	}
	v := &Vault{dir: dir, opts: o, file: vf, vaultID: vaultID}
	v.mu.Lock()
	v.installLocked(ring, sealer)
	v.mu.Unlock()
	return v, nil
}

// Open opens an existing vault in the locked state.
func Open(dir string, opts Options) (*Vault, error) {
	o, err := normalizeOptions(opts)
	if err != nil {
		return nil, err
	}
	for _, p := range []string{dir, filepath.Join(dir, objectsDirName)} {
		fi, err := os.Stat(p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("%w: no vault at %s", ErrNotFound, dir)
			}
			return nil, fmt.Errorf("vault: stat %s: %w", p, err)
		}
		if !fi.IsDir() {
			return nil, invalid("%s is not a directory", p)
		}
		if err := checkPrivate(p, fi); err != nil {
			return nil, err
		}
	}
	vf, err := readVaultFile(dir)
	if err != nil {
		return nil, err
	}
	id, err := vf.id()
	if err != nil {
		return nil, err
	}
	removeStaleTemps(dir, time.Now())
	removeStaleTemps(filepath.Join(dir, objectsDirName), time.Now())
	return &Vault{dir: dir, opts: o, file: vf, vaultID: id}, nil
}

func normalizeOptions(o Options) (Options, error) {
	if o.MaxObjectSize == 0 {
		o.MaxObjectSize = DefaultMaxObjectSize
	}
	if o.MaxObjectSize < 0 || o.MaxObjectSize > MaxObjectSizeLimit {
		return o, invalid("MaxObjectSize %d outside [1, %d]", o.MaxObjectSize, MaxObjectSizeLimit)
	}
	if o.IdleTimeout < 0 {
		return o, invalid("negative IdleTimeout")
	}
	return o, nil
}

// readVaultFile reads and strictly validates vault.json.
func readVaultFile(dir string) (*vaultFile, error) {
	path := filepath.Join(dir, vaultFileName)
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: no vault at %s", ErrNotFound, dir)
		}
		return nil, fmt.Errorf("vault: open %s: %w", path, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("vault: stat %s: %w", path, err)
	}
	if err := checkPrivate(path, fi); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxVaultFile+1))
	if err != nil {
		return nil, fmt.Errorf("vault: read %s: %w", path, err)
	}
	return decodeVaultFile(data)
}

func decodeVaultFile(data []byte) (*vaultFile, error) {
	if len(data) > maxVaultFile {
		return nil, corrupt("vault.json too large")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var vf vaultFile
	if err := dec.Decode(&vf); err != nil {
		return nil, corrupt("vault.json: %v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, corrupt("vault.json: trailing data")
	}
	if vf.Format != vaultFormatName || vf.Version != vaultFormatV1 {
		return nil, corrupt("vault.json: unsupported format %q version %d", vf.Format, vf.Version)
	}
	if _, err := vf.id(); err != nil {
		return nil, err
	}
	if err := vf.Root.validate(); err != nil {
		return nil, err
	}
	if len(vf.Keyring) == 0 {
		return nil, corrupt("vault.json: missing keyring")
	}
	return &vf, nil
}

// sealKeyringFile seals ring into vf (a copy is not made; vf.Keyring is
// replaced) and returns the encoded vault.json.
func sealKeyringFile(ctx context.Context, vf *vaultFile, vaultID []byte, ring *keyring, s rootSealer) ([]byte, error) {
	pt := ring.marshal()
	defer wipe(pt)
	sealed, err := s.seal(ctx, pt, keyringAAD(vaultID, &vf.Root))
	if err != nil {
		return nil, err
	}
	vf.Keyring = sealed
	return json.MarshalIndent(vf, "", "  ")
}

func openKeyring(ctx context.Context, vf *vaultFile, vaultID []byte, s rootSealer) (*keyring, error) {
	pt, err := s.open(ctx, vf.Keyring, keyringAAD(vaultID, &vf.Root))
	if err != nil {
		return nil, err
	}
	defer wipe(pt)
	return decodeKeyring(pt, vaultID)
}

// Dir returns the vault directory.
func (v *Vault) Dir() string { return v.dir }

// Locked reports whether the vault is locked.
func (v *Vault) Locked() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.ring == nil
}

// Unlock derives the root key from src, authenticates the keyring and keeps
// the KEKs in memory until Lock, Close or the idle timeout. It re-reads
// vault.json, so it picks up changes made by another process. Unlocking an
// unlocked vault replaces its keys after verifying src.
func (v *Vault) Unlock(ctx context.Context, src KeySource) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if src == nil {
		return invalid("nil key source")
	}
	vf, err := readVaultFile(v.dir)
	if err != nil {
		return err
	}
	vaultID, err := vf.id()
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(vaultID, v.vaultID) != 1 {
		return corrupt("vault id changed underneath the open vault")
	}
	// Key derivation (PBKDF2, KMS round trip) runs without holding mu.
	sealer, err := newSealer(ctx, src, &vf.Root, vaultID)
	if err != nil {
		return err
	}
	ring, err := openKeyring(ctx, vf, vaultID, sealer)
	if err != nil {
		sealer.wipe()
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.lockLocked()
	v.file = vf
	v.installLocked(ring, sealer)
	return nil
}

func (v *Vault) installLocked(ring *keyring, sealer rootSealer) {
	v.ring, v.sealer = ring, sealer
	v.gen++
	v.touch()
	v.armIdleLocked()
}

// Lock wipes all key material held in memory. Operations in flight finish
// first. Lock is idempotent.
func (v *Vault) Lock() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.lockLocked()
}

// Close locks the vault. The Vault may be unlocked again afterwards.
func (v *Vault) Close() error {
	v.Lock()
	return nil
}

func (v *Vault) lockLocked() {
	if v.idle != nil {
		v.idle.Stop()
		v.idle = nil
	}
	if v.ring != nil {
		v.ring.wipe()
		v.ring = nil
	}
	if v.sealer != nil {
		v.sealer.wipe()
		v.sealer = nil
	}
	v.gen++
}

func (v *Vault) touch() { v.lastUse.Store(time.Now().UnixNano()) }

func (v *Vault) armIdleLocked() {
	if v.opts.IdleTimeout <= 0 {
		return
	}
	gen := v.gen
	v.idle = time.AfterFunc(v.opts.IdleTimeout, func() { v.idleFire(gen) })
}

func (v *Vault) idleFire(gen uint64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.gen != gen || v.ring == nil {
		return
	}
	idle := time.Since(time.Unix(0, v.lastUse.Load()))
	if idle >= v.opts.IdleTimeout {
		v.lockLocked()
		return
	}
	v.idle = time.AfterFunc(v.opts.IdleTimeout-idle, func() { v.idleFire(gen) })
}

// acquire returns the keyring for an object operation. release must be
// called when the operation no longer needs keys.
func (v *Vault) acquire() (ring *keyring, release func(), err error) {
	v.mu.RLock()
	if v.ring == nil {
		v.mu.RUnlock()
		return nil, nil, ErrLocked
	}
	v.touch()
	return v.ring, func() {
		v.touch()
		v.mu.RUnlock()
	}, nil
}

// persistLocked seals ring with sealer under root params p and writes
// vault.json. On success it returns the new file record.
func (v *Vault) persistLocked(ctx context.Context, ring *keyring, sealer rootSealer, p rootParams) (*vaultFile, error) {
	vf := *v.file
	vf.Root = p
	ring.generation++
	data, err := sealKeyringFile(ctx, &vf, v.vaultID, ring, sealer)
	if err != nil {
		ring.generation--
		return nil, err
	}
	if err := writeFileAtomic(v.dir, vaultFileName, data); err != nil {
		ring.generation--
		return nil, err
	}
	return &vf, nil
}

// Rotate creates a new KEK version and makes it current. New writes use it at
// once; existing objects keep working with their old version until Get
// re-wraps them lazily or Migrate moves them. It returns the new version.
func (v *Vault) Rotate(ctx context.Context) (uint32, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	v.adminMu.Lock()
	defer v.adminMu.Unlock()
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.ring == nil {
		return 0, ErrLocked
	}
	next := v.ring.clone()
	k, err := next.addVersion(time.Now())
	if err != nil {
		next.wipe()
		return 0, err
	}
	vf, err := v.persistLocked(ctx, next, v.sealer, v.file.Root)
	if err != nil {
		next.wipe()
		return 0, err
	}
	v.ring.wipe()
	v.ring, v.file = next, vf
	v.touch()
	return k.version, nil
}

// Rekey re-seals the keyring under a new root secret (new passphrase with a
// fresh salt, a key file, or a KMS key). KEKs and objects are unchanged. The
// vault must be unlocked.
func (v *Vault) Rekey(ctx context.Context, src KeySource, kdf KDFOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	v.adminMu.Lock()
	defer v.adminMu.Unlock()
	if v.Locked() {
		return ErrLocked
	}
	params, err := newRootParams(src, kdf)
	if err != nil {
		return err
	}
	sealer, err := newSealer(ctx, src, params, v.vaultID)
	if err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.ring == nil {
		sealer.wipe()
		return ErrLocked
	}
	vf, err := v.persistLocked(ctx, v.ring, sealer, *params)
	if err != nil {
		sealer.wipe()
		return err
	}
	v.sealer.wipe()
	v.sealer, v.file = sealer, vf
	v.touch()
	return nil
}

// MasterShares verifies that src unlocks this vault and splits its master key
// into n Shamir shares with the given threshold. Combine threshold shares with
// CombineShares and unlock with MasterKey to recover a vault whose passphrase
// or key file is lost. KMS-rooted vaults have no local master key and return
// ErrKeySourceMismatch. The vault need not be unlocked. Rekey invalidates
// existing shares.
func (v *Vault) MasterShares(ctx context.Context, src KeySource, threshold, n int) ([]Share, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	vf, err := readVaultFile(v.dir)
	if err != nil {
		return nil, err
	}
	if vf.Root.Kind == rootKindKMS {
		return nil, fmt.Errorf("%w: KMS-rooted vaults are recovered through the KMS", ErrKeySourceMismatch)
	}
	vaultID, err := vf.id()
	if err != nil {
		return nil, err
	}
	master, err := masterFromSource(ctx, src, &vf.Root)
	if err != nil {
		return nil, err
	}
	defer wipe(master)
	sealer, err := newLocalSealer(master, vaultID)
	if err != nil {
		return nil, err
	}
	ring, err := openKeyring(ctx, vf, vaultID, sealer)
	sealer.wipe()
	if err != nil {
		return nil, err
	}
	ring.wipe()
	return SplitSecret(master, threshold, n)
}

// KeyVersion describes one KEK version.
type KeyVersion struct {
	Version uint32
	Created time.Time
	Current bool
}

// KeyStatus summarizes the keyring and root configuration.
type KeyStatus struct {
	Current       uint32
	Versions      []KeyVersion
	Generation    uint64
	RootKind      string
	KDF           string
	KDFIterations int
	KMSKeyID      string
}

// KeyStatus reports the keyring state. The vault must be unlocked.
func (v *Vault) KeyStatus() (KeyStatus, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.ring == nil {
		return KeyStatus{}, ErrLocked
	}
	st := KeyStatus{
		Current:       v.ring.current,
		Generation:    v.ring.generation,
		RootKind:      v.file.Root.Kind,
		KDF:           v.file.Root.KDF,
		KDFIterations: v.file.Root.Iterations,
		KMSKeyID:      v.file.Root.KMSKeyID,
	}
	for _, k := range v.ring.keks {
		st.Versions = append(st.Versions, KeyVersion{
			Version: k.version,
			Created: time.Unix(k.created, 0).UTC(),
			Current: k.version == v.ring.current,
		})
	}
	sort.Slice(st.Versions, func(i, j int) bool { return st.Versions[i].Version < st.Versions[j].Version })
	return st, nil
}
