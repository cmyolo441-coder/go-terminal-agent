package supplychain

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// copyFixtureTree writes the embedded testdata tree (without the OSV
// dumps) into dir so filesystem-based APIs can be exercised.
func copyFixtureTree(t *testing.T, dir string) {
	t.Helper()
	err := fs.WalkDir(testdataFS, "testdata", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, "testdata"), "/")
		if strings.HasPrefix(rel, "osv") {
			return nil
		}
		// go.mod fixtures are stored as *.golden: a go.mod file would make
		// the directory a separate module and exclude it from embedding.
		target := filepath.Join(dir, filepath.FromSlash(strings.TrimSuffix(rel, ".golden")))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := testdataFS.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}
