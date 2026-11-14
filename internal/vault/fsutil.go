package vault

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	dirPerm      fs.FileMode = 0o700
	filePerm     fs.FileMode = 0o600
	tempPrefix               = ".tmp-"
	staleTempAge             = time.Hour
)

// permissionsEnforced reports whether Unix permission bits are meaningful.
func permissionsEnforced() bool { return runtime.GOOS != "windows" && runtime.GOOS != "plan9" }

// checkPrivate rejects a file or directory that group or others can access.
func checkPrivate(path string, fi fs.FileInfo) error {
	if !permissionsEnforced() {
		return nil
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: %s has mode %#o, want no group/other access", ErrInsecurePermissions, path, fi.Mode().Perm())
	}
	return nil
}

// ensurePrivateDir creates dir with mode 0700 if missing and sets the mode to
// exactly 0700, so a pre-existing directory is tightened rather than trusted.
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("vault: create %s: %w", dir, err)
	}
	if permissionsEnforced() {
		if err := os.Chmod(dir, dirPerm); err != nil {
			return fmt.Errorf("vault: chmod %s: %w", dir, err)
		}
	}
	return nil
}

// syncDir fsyncs a directory so a rename or unlink inside it is durable.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("vault: open dir for sync: %w", err)
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("vault: sync dir %s: %w", dir, err)
	}
	return nil
}

// writeTemp writes data to a fresh 0600 temporary file in dir, fsyncs and
// closes it, and returns its path.
func writeTemp(dir string, data []byte) (string, error) {
	f, err := os.CreateTemp(dir, tempPrefix+"*")
	if err != nil {
		return "", fmt.Errorf("vault: create temp file: %w", err)
	}
	tmp := f.Name()
	fail := func(err error) (string, error) {
		_ = f.Close()
		_ = os.Remove(tmp)
		return "", err
	}
	if permissionsEnforced() {
		if err := f.Chmod(filePerm); err != nil {
			return fail(fmt.Errorf("vault: chmod temp file: %w", err))
		}
	}
	if _, err := f.Write(data); err != nil {
		return fail(fmt.Errorf("vault: write temp file: %w", err))
	}
	if err := f.Sync(); err != nil {
		return fail(fmt.Errorf("vault: sync temp file: %w", err))
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("vault: close temp file: %w", err)
	}
	return tmp, nil
}

// writeFileAtomic replaces dir/name with data so readers see either the old
// or the new content, never a mix, and the change survives a crash.
func writeFileAtomic(dir, name string, data []byte) error {
	tmp, err := writeTemp(dir, data)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("vault: rename into place: %w", err)
	}
	return syncDir(dir)
}

// createFileExclusive writes dir/name atomically and fails with ErrExists if
// it already exists. It publishes the file with a hard link, which fails
// atomically when the target exists; on file systems without hard links it
// falls back to check-then-rename.
func createFileExclusive(dir, name string, data []byte) error {
	tmp, err := writeTemp(dir, data)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	target := filepath.Join(dir, name)
	lerr := os.Link(tmp, target)
	switch {
	case lerr == nil:
	case errors.Is(lerr, fs.ErrExist):
		return fmt.Errorf("%w: %s", ErrExists, target)
	default:
		if _, err := os.Lstat(target); err == nil {
			return fmt.Errorf("%w: %s", ErrExists, target)
		}
		if err := os.Rename(tmp, target); err != nil {
			return fmt.Errorf("vault: rename into place: %w", err)
		}
	}
	return syncDir(dir)
}

// removeStaleTemps deletes temporary files left behind by crashed writers.
// Only files older than staleTempAge are removed so concurrent writers in
// other processes are not disturbed.
func removeStaleTemps(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), tempPrefix) || !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < staleTempAge {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}
