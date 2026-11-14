package fleet

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Lease is a leadership lease. Token is a fencing token: it increases on
// every change of holder (and on re-acquisition after expiry) and never
// decreases, so comparing tokens orders leaders in time.
type Lease struct {
	Name    string    `json:"name"`
	Holder  string    `json:"holder,omitempty"`
	Address string    `json:"address,omitempty"`
	Token   uint64    `json:"token"`
	Expires time.Time `json:"expires,omitzero"`
}

// HeldAt reports whether the lease is held and unexpired at now.
func (l Lease) HeldAt(now time.Time) bool { return l.Holder != "" && now.Before(l.Expires) }

// LeaseStore is shared storage for leader election among coordinators.
// Implementations must make each operation atomic with respect to the
// others on the same name.
type LeaseStore interface {
	// Acquire takes the lease if it is free, expired or already held by
	// holder. A new holder gets a token one above the previous token;
	// the current holder renewing keeps its token. ErrLeaseHeld means
	// another holder has it.
	Acquire(ctx context.Context, name, holder, address string, ttl time.Duration) (Lease, error)
	// Renew extends cur. ErrLeaseLost means cur is no longer the lease.
	Renew(ctx context.Context, cur Lease, ttl time.Duration) (Lease, error)
	// Release gives up cur early, keeping its token as the high-water mark.
	Release(ctx context.Context, cur Lease) error
	// Get returns the current lease (zero Holder when free).
	Get(ctx context.Context, name string) (Lease, error)
	// CheckFence returns the current lease if token is still the live
	// lease's token, and ErrStaleFencingToken otherwise.
	CheckFence(ctx context.Context, name string, token uint64) (Lease, error)
}

// leaseLogic is the transition function shared by both stores.
func acquireLease(cur Lease, name, holder, address string, ttl time.Duration, now time.Time) (Lease, error) {
	if holder == "" || ttl <= 0 {
		return Lease{}, fmt.Errorf("%w: lease needs a holder and a positive ttl", ErrInvalid)
	}
	if cur.HeldAt(now) && cur.Holder != holder {
		return cur, ErrLeaseHeld
	}
	next := Lease{Name: name, Holder: holder, Address: address, Token: cur.Token, Expires: now.Add(ttl)}
	if !cur.HeldAt(now) || cur.Holder != holder {
		next.Token = cur.Token + 1
	}
	return next, nil
}

func renewLease(stored, cur Lease, ttl time.Duration, now time.Time) (Lease, error) {
	if stored.Token != cur.Token || stored.Holder != cur.Holder || !stored.HeldAt(now) {
		return stored, ErrLeaseLost
	}
	stored.Expires = now.Add(ttl)
	return stored, nil
}

func checkFence(stored Lease, token uint64, now time.Time) (Lease, error) {
	if stored.Token != token || !stored.HeldAt(now) {
		return stored, fmt.Errorf("%w: token %d, current %d", ErrStaleFencingToken, token, stored.Token)
	}
	return stored, nil
}

// MemLeaseStore is an in-process LeaseStore, for tests and single-process
// HA experiments.
type MemLeaseStore struct {
	clock  Clock
	mu     sync.Mutex
	leases map[string]Lease
}

// NewMemLeaseStore returns an empty store reading time from clock (nil for
// the real clock).
func NewMemLeaseStore(clock Clock) *MemLeaseStore {
	if clock == nil {
		clock = RealClock()
	}
	return &MemLeaseStore{clock: clock, leases: map[string]Lease{}}
}

// Acquire implements LeaseStore.
func (m *MemLeaseStore) Acquire(_ context.Context, name, holder, address string, ttl time.Duration) (Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.leases[name]
	cur.Name = name
	next, err := acquireLease(cur, name, holder, address, ttl, m.clock.Now())
	if err != nil {
		return next, err
	}
	m.leases[name] = next
	return next, nil
}

// Renew implements LeaseStore.
func (m *MemLeaseStore) Renew(_ context.Context, cur Lease, ttl time.Duration) (Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	next, err := renewLease(m.leases[cur.Name], cur, ttl, m.clock.Now())
	if err != nil {
		return next, err
	}
	m.leases[cur.Name] = next
	return next, nil
}

// Release implements LeaseStore.
func (m *MemLeaseStore) Release(_ context.Context, cur Lease) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	stored := m.leases[cur.Name]
	if stored.Token != cur.Token || stored.Holder != cur.Holder {
		return ErrLeaseLost
	}
	m.leases[cur.Name] = Lease{Name: cur.Name, Token: stored.Token}
	return nil
}

// Get implements LeaseStore.
func (m *MemLeaseStore) Get(_ context.Context, name string) (Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.leases[name]
	l.Name = name
	return l, nil
}

// CheckFence implements LeaseStore.
func (m *MemLeaseStore) CheckFence(_ context.Context, name string, token uint64) (Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return checkFence(m.leases[name], token, m.clock.Now())
}

// FileLeaseStore keeps leases as files in a directory shared by all
// coordinators (a local disk for several processes on one machine, or a
// network file system with close-to-open consistency and atomic
// create-exclusive and rename).
//
// Each operation holds <name>.lock, created with O_EXCL, while it reads
// and rewrites <name>.lease atomically. A lock file older than
// StaleLockAge is presumed left by a crashed process and broken. Tokens
// live in the lease file and are only ever incremented, so they stay
// monotonic across restarts and releases.
type FileLeaseStore struct {
	dir          string
	clock        Clock
	staleLockAge time.Duration
	lockWait     time.Duration
}

// NewFileLeaseStore creates dir if needed. clock nil means real time.
func NewFileLeaseStore(dir string, clock Clock) (*FileLeaseStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("fleet: lease dir: %w", err)
	}
	if clock == nil {
		clock = RealClock()
	}
	return &FileLeaseStore{dir: dir, clock: clock, staleLockAge: 10 * time.Second, lockWait: 5 * time.Second}, nil
}

func (s *FileLeaseStore) paths(name string) (lease, lock string, err error) {
	if !validID(name) {
		return "", "", fmt.Errorf("%w: lease name %q", ErrInvalid, name)
	}
	return filepath.Join(s.dir, name+".lease"), filepath.Join(s.dir, name+".lock"), nil
}

// withLock runs fn while holding the name's lock file.
func (s *FileLeaseStore) withLock(ctx context.Context, name string, fn func(leasePath string) error) error {
	leasePath, lockPath, err := s.paths(name)
	if err != nil {
		return err
	}
	var nb [12]byte
	_, _ = rand.Read(nb[:])
	nonce := hex.EncodeToString(nb[:])
	deadline := time.Now().Add(s.lockWait)
	for {
		f, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, werr := f.WriteString(nonce)
			cerr := f.Close()
			if werr != nil || cerr != nil {
				os.Remove(lockPath)
				return fmt.Errorf("fleet: lease lock: %w", errors.Join(werr, cerr))
			}
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("fleet: lease lock: %w", err)
		}
		// Break a lock abandoned by a crashed process. Lock ages use the
		// file system's clock, not the injectable one.
		if st, serr := os.Stat(lockPath); serr == nil && time.Since(st.ModTime()) > s.staleLockAge {
			os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("fleet: lease lock %s: timed out", name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Millisecond):
		}
	}
	defer func() {
		// Only remove the lock if it is still ours.
		if b, err := os.ReadFile(lockPath); err == nil && string(b) == nonce {
			os.Remove(lockPath)
		}
	}()
	return fn(leasePath)
}

func (s *FileLeaseStore) read(path, name string) (Lease, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Lease{Name: name}, nil
	}
	if err != nil {
		return Lease{}, fmt.Errorf("fleet: lease read: %w", err)
	}
	var l Lease
	if err := decodeLine(trimNL(b), &l); err != nil {
		return Lease{}, fmt.Errorf("fleet: lease %s: %w", name, err)
	}
	l.Name = name
	return l, nil
}

func (s *FileLeaseStore) write(path string, l Lease) error {
	line, err := encodeLine(&l)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(path, line, 0o600); err != nil {
		return fmt.Errorf("fleet: lease write: %w", err)
	}
	return nil
}

func trimNL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

// Acquire implements LeaseStore.
func (s *FileLeaseStore) Acquire(ctx context.Context, name, holder, address string, ttl time.Duration) (Lease, error) {
	var out Lease
	err := s.withLock(ctx, name, func(p string) error {
		cur, err := s.read(p, name)
		if err != nil {
			return err
		}
		next, err := acquireLease(cur, name, holder, address, ttl, s.clock.Now())
		out = next
		if err != nil {
			return err
		}
		return s.write(p, next)
	})
	return out, err
}

// Renew implements LeaseStore.
func (s *FileLeaseStore) Renew(ctx context.Context, cur Lease, ttl time.Duration) (Lease, error) {
	var out Lease
	err := s.withLock(ctx, cur.Name, func(p string) error {
		stored, err := s.read(p, cur.Name)
		if err != nil {
			return err
		}
		next, err := renewLease(stored, cur, ttl, s.clock.Now())
		out = next
		if err != nil {
			return err
		}
		return s.write(p, next)
	})
	return out, err
}

// Release implements LeaseStore.
func (s *FileLeaseStore) Release(ctx context.Context, cur Lease) error {
	return s.withLock(ctx, cur.Name, func(p string) error {
		stored, err := s.read(p, cur.Name)
		if err != nil {
			return err
		}
		if stored.Token != cur.Token || stored.Holder != cur.Holder {
			return ErrLeaseLost
		}
		return s.write(p, Lease{Name: cur.Name, Token: stored.Token})
	})
}

// Get implements LeaseStore. It reads without the lock; the lease file is
// always replaced atomically.
func (s *FileLeaseStore) Get(_ context.Context, name string) (Lease, error) {
	p, _, err := s.paths(name)
	if err != nil {
		return Lease{}, err
	}
	return s.read(p, name)
}

// CheckFence implements LeaseStore.
func (s *FileLeaseStore) CheckFence(_ context.Context, name string, token uint64) (Lease, error) {
	p, _, err := s.paths(name)
	if err != nil {
		return Lease{}, err
	}
	l, err := s.read(p, name)
	if err != nil {
		return l, err
	}
	return checkFence(l, token, s.clock.Now())
}

// Fencer guards a leader's writes.
type Fencer interface {
	// Token is the leader's fencing token.
	Token() uint64
	// Check returns ErrStaleFencingToken once the leader is deposed.
	Check(ctx context.Context) error
}

// StoreFencer checks a token against a LeaseStore. A successful check is
// trusted for at most MaxCache and never past the lease expiry minus
// Margin, which bounds store round trips without letting a paused leader
// write after a successor can have been elected (assuming clock drift
// below Margin).
type StoreFencer struct {
	Store    LeaseStore
	Name     string
	Tok      uint64
	Clock    Clock
	Margin   time.Duration
	MaxCache time.Duration

	mu         sync.Mutex
	validUntil time.Time
}

// NewStoreFencer returns a fencer for the given lease.
func NewStoreFencer(store LeaseStore, l Lease, clock Clock) *StoreFencer {
	if clock == nil {
		clock = RealClock()
	}
	return &StoreFencer{Store: store, Name: l.Name, Tok: l.Token, Clock: clock, Margin: 500 * time.Millisecond, MaxCache: time.Second}
}

// Token implements Fencer.
func (f *StoreFencer) Token() uint64 { return f.Tok }

// Check implements Fencer.
func (f *StoreFencer) Check(ctx context.Context) error {
	now := f.Clock.Now()
	f.mu.Lock()
	ok := now.Before(f.validUntil)
	f.mu.Unlock()
	if ok {
		return nil
	}
	l, err := f.Store.CheckFence(ctx, f.Name, f.Tok)
	if err != nil {
		f.mu.Lock()
		f.validUntil = time.Time{}
		f.mu.Unlock()
		return err
	}
	until := l.Expires.Add(-f.Margin)
	if f.MaxCache >= 0 && now.Add(f.MaxCache).Before(until) {
		until = now.Add(f.MaxCache)
	}
	f.mu.Lock()
	f.validUntil = until
	f.mu.Unlock()
	return nil
}

// ElectorConfig configures an Elector.
type ElectorConfig struct {
	Store LeaseStore
	// Name is the lease name all competing coordinators share.
	Name string
	// Holder identifies this coordinator; Address is how clients reach
	// it (advertised as the leader hint).
	Holder  string
	Address string
	// TTL is the lease duration (default 10s). RenewEvery defaults to
	// TTL/3 and RetryEvery (for followers) to TTL/2.
	TTL        time.Duration
	RenewEvery time.Duration
	RetryEvery time.Duration
	Clock      Clock
	Logger     *slog.Logger
}

// Elector campaigns for a lease and keeps it renewed.
type Elector struct {
	cfg ElectorConfig

	mu      sync.Mutex
	current Lease
	leading bool
}

// NewElector validates cfg and fills defaults.
func NewElector(cfg ElectorConfig) (*Elector, error) {
	if cfg.Store == nil || cfg.Name == "" || cfg.Holder == "" {
		return nil, fmt.Errorf("%w: elector needs a store, a name and a holder", ErrInvalid)
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 10 * time.Second
	}
	if cfg.RenewEvery <= 0 {
		cfg.RenewEvery = cfg.TTL / 3
	}
	if cfg.RetryEvery <= 0 {
		cfg.RetryEvery = cfg.TTL / 2
	}
	if cfg.Clock == nil {
		cfg.Clock = RealClock()
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	return &Elector{cfg: cfg}, nil
}

// Leading reports whether this elector currently holds the lease, and the
// lease.
func (e *Elector) Leading() (Lease, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.current, e.leading
}

// LeaderHint returns the advertised address of the current leader, read
// from the store.
func (e *Elector) LeaderHint(ctx context.Context) string {
	l, err := e.cfg.Store.Get(ctx, e.cfg.Name)
	if err != nil || !l.HeldAt(e.cfg.Clock.Now()) {
		return ""
	}
	return l.Address
}

// Run campaigns until ctx ends. onElected runs (synchronously) each time
// leadership is won, with a context canceled when it is lost; if it
// returns an error the lease is released and the campaign continues.
// onDemoted runs after each loss. On exit the lease is released.
func (e *Elector) Run(ctx context.Context, onElected func(ctx context.Context, l Lease) error, onDemoted func(l Lease)) error {
	lg := e.cfg.Logger
	for {
		l, err := e.cfg.Store.Acquire(ctx, e.cfg.Name, e.cfg.Holder, e.cfg.Address, e.cfg.TTL)
		if err != nil {
			if !errors.Is(err, ErrLeaseHeld) && ctx.Err() == nil {
				lg.Warn("fleet: lease acquire failed", "err", err)
			}
			if err := sleepCtx(ctx, e.cfg.Clock, e.cfg.RetryEvery); err != nil {
				return nil
			}
			continue
		}
		lg.Info("fleet: elected leader", "holder", e.cfg.Holder, "token", l.Token)
		e.set(l, true)
		lctx, cancel := context.WithCancel(ctx)
		if err := onElected(lctx, l); err != nil {
			lg.Error("fleet: promotion failed; stepping down", "err", err)
			cancel()
			e.set(l, false)
			_ = e.cfg.Store.Release(context.WithoutCancel(ctx), l)
			if onDemoted != nil {
				onDemoted(l)
			}
			if err := sleepCtx(ctx, e.cfg.Clock, e.cfg.RetryEvery); err != nil {
				return nil
			}
			continue
		}
		l = e.hold(ctx, l)
		cancel()
		e.set(l, false)
		if onDemoted != nil {
			onDemoted(l)
		}
		if ctx.Err() != nil {
			rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			_ = e.cfg.Store.Release(rctx, l)
			rcancel()
			return nil
		}
	}
}

// hold renews l until renewal fails for long enough that the lease may
// have lapsed, or ctx ends.
func (e *Elector) hold(ctx context.Context, l Lease) Lease {
	for {
		if err := sleepCtx(ctx, e.cfg.Clock, e.cfg.RenewEvery); err != nil {
			return l
		}
		nl, err := e.cfg.Store.Renew(ctx, l, e.cfg.TTL)
		if err == nil {
			l = nl
			e.set(l, true)
			continue
		}
		if errors.Is(err, ErrLeaseLost) {
			e.cfg.Logger.Warn("fleet: leadership lost", "token", l.Token)
			return l
		}
		// Transient store error: keep trying while the lease may still be
		// ours, but step down before it can have expired.
		if !e.cfg.Clock.Now().Add(e.cfg.RenewEvery).Before(l.Expires) {
			e.cfg.Logger.Warn("fleet: cannot renew lease before expiry; stepping down", "err", err)
			return l
		}
	}
}

func (e *Elector) set(l Lease, leading bool) {
	e.mu.Lock()
	e.current, e.leading = l, leading
	e.mu.Unlock()
}
