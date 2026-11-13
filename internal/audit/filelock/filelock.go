// Package filelock provides an exclusive advisory lock on a file that works
// across processes, so several gta instances can safely share a ledger or
// an audit log. Locks are released when the holder closes them or exits,
// so a crashed process never leaves a stale lock behind.
package filelock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Lock is a held lock. Unlock releases it.
type Lock struct {
	f    *os.File
	path string
	once sync.Once
	err  error
}

// ErrTimeout is returned when the lock could not be taken in time.
var ErrTimeout = errors.New("filelock: timed out waiting for lock")

// Acquire takes an exclusive lock on path, creating the file (mode 0600)
// and its directory (0700) if needed. It waits until the lock is free or ctx
// is done.
func Acquire(ctx context.Context, path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	delay := time.Millisecond
	for {
		ok, err := tryLock(f)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("filelock: %s: %w", path, err)
		}
		if ok {
			return &Lock{f: f, path: path}, nil
		}
		select {
		case <-ctx.Done():
			f.Close()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, fmt.Errorf("%w: %s", ErrTimeout, path)
			}
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		if delay < 50*time.Millisecond {
			delay *= 2
		}
	}
}

// AcquireTimeout is Acquire with a timeout.
func AcquireTimeout(path string, d time.Duration) (*Lock, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return Acquire(ctx, path)
}

// Path is the locked file.
func (l *Lock) Path() string { return l.path }

// Unlock releases the lock. It is safe to call more than once.
func (l *Lock) Unlock() error {
	l.once.Do(func() {
		uerr := unlock(l.f)
		cerr := l.f.Close()
		l.err = errors.Join(uerr, cerr)
	})
	return l.err
}
