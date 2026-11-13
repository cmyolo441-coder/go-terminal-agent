package ui

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileIndexMatch(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"cmd/gta/main.go", "internal/ui/model.go", "README.md", "node_modules/x/main.go"} {
		full := filepath.Join(root, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte("x"), 0o644)
	}
	ix := &fileIndex{root: root}
	got := ix.match("main", 5)
	if len(got) != 1 || got[0] != "cmd/gta/main.go" {
		t.Fatalf("match(main) = %v", got)
	}
	if got := ix.match("", 10); len(got) != 3 {
		t.Fatalf("top level = %v", got)
	}
	if got := ix.match("ui/mo", 5); len(got) != 1 || got[0] != "internal/ui/model.go" {
		t.Fatalf("match(ui/mo) = %v", got)
	}
}

func TestAtToken(t *testing.T) {
	cases := map[string]string{"look at @cmd/m": "cmd/m", "@": "", "@a": "a"}
	for in, want := range cases {
		if got, ok := atToken(in); !ok || got != want {
			t.Errorf("atToken(%q) = %q %v", in, got, ok)
		}
	}
	for _, in := range []string{"hi", "@a ", "email a@b", ""} {
		if _, ok := atToken(in); ok {
			t.Errorf("atToken(%q) should not match", in)
		}
	}
}
