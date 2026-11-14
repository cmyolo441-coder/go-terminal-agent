//go:build unix

package workflow

import (
	"errors"
	"os"
	"syscall"
)

// lockFile takes an exclusive advisory lock on the journal so two
// processes cannot run the same workflow at once. The lock is released
// when the file is closed, including when the process dies.
func lockFile(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return ErrJournalLocked
	}
	return err
}

// syncDir fsyncs a directory so a newly created file's entry is durable.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
