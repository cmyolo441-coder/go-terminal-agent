package evals

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// writeFileAtomic writes a file through a temporary sibling and a rename,
// so readers never observe a partially written file.
func writeFileAtomic(dst string, write func(io.Writer) error) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(dst)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	cleanup := func() { _ = os.Remove(name) }
	if err := write(tmp); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(name, dst); err != nil {
		cleanup()
		return err
	}
	return nil
}

// cleanRelPath validates a slash-separated path relative to a workspace
// and returns it in clean form. Absolute paths and paths that escape the
// root are rejected.
func cleanRelPath(p string) (string, error) {
	if p == "" {
		return "", errors.New("empty path")
	}
	s := filepath.ToSlash(p)
	if strings.HasPrefix(s, "/") || filepath.IsAbs(p) || filepath.VolumeName(p) != "" {
		return "", fmt.Errorf("path %q must be relative", p)
	}
	c := path.Clean(s)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("path %q escapes the workspace", p)
	}
	return c, nil
}

// resolveInside joins a relative path to root and makes sure the result,
// after resolving symlinks in the existing part of the path, stays inside
// root.
func resolveInside(root, rel string) (string, error) {
	c, err := cleanRelPath(rel)
	if err != nil {
		return "", err
	}
	full := filepath.Join(root, filepath.FromSlash(c))
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	// Resolve the deepest existing ancestor.
	probe := full
	var tail []string
	for {
		if _, err := os.Lstat(probe); err == nil {
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		tail = append([]string{filepath.Base(probe)}, tail...)
		probe = parent
	}
	realProbe, err := filepath.EvalSymlinks(probe)
	if err != nil {
		return "", err
	}
	resolved := filepath.Join(append([]string{realProbe}, tail...)...)
	relToRoot, err := filepath.Rel(realRoot, resolved)
	if err != nil || relToRoot == ".." || strings.HasPrefix(relToRoot, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q resolves outside the workspace", rel)
	}
	return full, nil
}

// copyTree copies a directory tree. Regular files keep their permission
// bits; symlinks are recreated only when they are relative and stay inside
// the tree; other file types are rejected.
func copyTree(src, dst string) error {
	src = filepath.Clean(src)
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch mode := info.Mode(); {
		case mode.IsDir():
			return os.MkdirAll(target, mode.Perm()|0o700)
		case mode.IsRegular():
			return copyFile(p, target, mode.Perm())
		case mode&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if filepath.IsAbs(link) {
				return fmt.Errorf("fixture symlink %s is absolute", rel)
			}
			resolved := filepath.Clean(filepath.Join(filepath.Dir(rel), link))
			if resolved == ".." || strings.HasPrefix(resolved, ".."+string(filepath.Separator)) {
				return fmt.Errorf("fixture symlink %s points outside the fixture", rel)
			}
			return os.Symlink(link, target)
		default:
			return fmt.Errorf("fixture entry %s has unsupported type %s", rel, mode.Type())
		}
	})
}

func copyFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm|0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// snapshotTree hashes every regular file under root, keyed by slash path.
func snapshotTree(root string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		sum, err := hashFile(p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = sum
		return nil
	})
	return out, err
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
