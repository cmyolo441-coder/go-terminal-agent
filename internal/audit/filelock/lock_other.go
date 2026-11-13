//go:build !(darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris || windows)

package filelock

import (
	"os"
	"sync"
)

// On platforms without flock or LockFileEx (aix, js, wasip1, plan9) the lock is held
// per path inside this process only; such platforms run one gta at a time.
var (
	mu    sync.Mutex
	held  = map[string]bool{}
	paths = map[*os.File]string{}
)

func tryLock(f *os.File) (bool, error) {
	mu.Lock()
	defer mu.Unlock()
	if held[f.Name()] {
		return false, nil
	}
	held[f.Name()] = true
	paths[f] = f.Name()
	return true, nil
}

func unlock(f *os.File) error {
	mu.Lock()
	defer mu.Unlock()
	delete(held, paths[f])
	delete(paths, f)
	return nil
}
