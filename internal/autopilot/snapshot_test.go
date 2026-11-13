package autopilot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSnapshotChanges(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"keep.txt":            "same\n",
		"edit.go":             "package a\n\nfunc A() int { return 1 }\n",
		"gone.txt":            "bye\n",
		"emptied.txt":         "had content\n",
		"node_modules/x.js":   "ignored\n",
		".git/HEAD":           "ref\n",
		"bin.dat":             "a\x00b",
		"nested/dir/deep.txt": "deep\n",
	})
	snap, err := TakeSnapshot(context.Background(), root, SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	writeFiles(t, root, map[string]string{
		"edit.go":           "package a\n\nfunc A() int { return 2 }\n",
		"new.txt":           "fresh\nlines\n",
		"emptied.txt":       "",
		"node_modules/x.js": "changed but ignored\n",
		"bin.dat":           "a\x00c",
		"empty-new.txt":     "",
	})
	if err := os.Remove(filepath.Join(root, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	cs, err := snap.Changes(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Change{}
	for _, c := range cs.Files {
		got[c.Path] = c
	}
	want := map[string]ChangeKind{
		"edit.go": ChangeModified, "new.txt": ChangeAdded, "emptied.txt": ChangeModified,
		"gone.txt": ChangeDeleted, "bin.dat": ChangeModified, "empty-new.txt": ChangeAdded,
	}
	if len(got) != len(want) {
		t.Fatalf("changes %+v, want %v", cs.Paths(), want)
	}
	for p, k := range want {
		if got[p].Kind != k {
			t.Errorf("%s: kind %q, want %q", p, got[p].Kind, k)
		}
	}
	if c := got["edit.go"]; c.Added != 1 || c.Deleted != 1 || !strings.Contains(c.Patch, "+func A() int { return 2 }") {
		t.Errorf("edit.go patch: %+v", c)
	}
	if c := got["new.txt"]; c.Added != 2 || c.Deleted != 0 {
		t.Errorf("new.txt counts: %+v", c)
	}
	if c := got["gone.txt"]; c.Deleted != 1 {
		t.Errorf("gone.txt counts: %+v", c)
	}
	if !got["bin.dat"].Binary {
		t.Error("binary change not flagged")
	}
	stat := cs.Stat()
	if !strings.Contains(stat, "M  edit.go  +1 -1") || !strings.Contains(stat, "6 files changed") || !strings.Contains(stat, "(binary)") {
		t.Errorf("stat:\n%s", stat)
	}
	if !strings.Contains(cs.Text, "--- edit.go") || strings.Contains(cs.Text, "node_modules") {
		t.Errorf("diff text:\n%s", cs.Text)
	}
}

func TestSnapshotBudgetsAndLimits(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"a.txt": "a\n", "big.txt": strings.Repeat("x\n", 100)})
	snap, err := TakeSnapshot(context.Background(), root, SnapshotOptions{MaxFileBytes: 50})
	if err != nil {
		t.Fatal(err)
	}
	writeFiles(t, root, map[string]string{"a.txt": strings.Repeat("changed line\n", 500), "big.txt": "small now\n"})
	cs, err := snap.Changes(context.Background(), 300)
	if err != nil {
		t.Fatal(err)
	}
	if !cs.Truncated || len(cs.Text) > 400 {
		t.Errorf("diff budget not applied: truncated=%v len=%d", cs.Truncated, len(cs.Text))
	}
	var big Change
	for _, c := range cs.Files {
		if c.Path == "big.txt" {
			big = c
		}
	}
	if big.Kind != ChangeModified || !strings.Contains(big.Note, "not captured") {
		t.Errorf("large file change: %+v", big)
	}

	many := t.TempDir()
	for i := range 5 {
		writeFiles(t, many, map[string]string{filepath.Join("d", string(rune('a'+i))+".txt"): "x"})
	}
	snap, err = TakeSnapshot(context.Background(), many, SnapshotOptions{MaxFiles: 3})
	if err != nil || !snap.Incomplete {
		t.Fatalf("file limit not reported: %v", err)
	}
	cs, err = snap.Changes(context.Background(), 0)
	if err != nil || !cs.Incomplete {
		t.Fatalf("incomplete snapshot must yield an incomplete change set: %+v %v", cs, err)
	}
	for _, c := range cs.Files {
		if c.Kind == ChangeDeleted {
			t.Errorf("an incomplete walk must not report deletions: %+v", c)
		}
	}
}

func TestSnapshotCancellation(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"a": "1", "b": "2"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := TakeSnapshot(ctx, root, SnapshotOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if _, err := TakeSnapshot(context.Background(), filepath.Join(root, "missing"), SnapshotOptions{}); err == nil {
		t.Fatal("a missing root must fail")
	}
}
