package checkpoint

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func open(t *testing.T, dir string, opts Options) *Store {
	t.Helper()
	s, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func put(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// editFile records path in s and then writes content, the way the tools do.
func editFile(t *testing.T, s *Store, path, content string) {
	t.Helper()
	if w, err := s.Record(path); err != nil || w != "" {
		t.Fatalf("Record(%s) = %q, %v", path, w, err)
	}
	put(t, path, content, 0o644)
}

func TestUndoAcrossCheckpoints(t *testing.T) {
	work := t.TempDir()
	s := open(t, t.TempDir(), Options{})
	a := filepath.Join(work, "a.txt")
	b := filepath.Join(work, "b.txt")
	put(t, a, "a0\n", 0o644)
	put(t, b, "b0\n", 0o644)

	s.Begin("turn one")
	editFile(t, s, a, "a1\n")
	editFile(t, s, a, "a1 again\n") // second record in the same checkpoint is a no-op
	editFile(t, s, b, "b1\n")

	s.Begin("turn without edits") // leaves no checkpoint behind

	s.Begin("turn two")
	editFile(t, s, a, "a2\n")

	list := s.List()
	if len(list) != 2 || list[0].Label != "turn one" || list[1].Label != "turn two" {
		t.Fatalf("list = %+v", list)
	}
	if len(list[0].Files) != 2 || list[0].Files[0] != a || list[1].ID <= list[0].ID {
		t.Fatalf("list files = %+v", list)
	}

	plan, err := s.PreviewUndo()
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 1 || plan.Changes[0].Action != ActionRestore ||
		!strings.Contains(plan.Changes[0].Diff, "-a2\n+a1 again\n") {
		t.Fatalf("plan = %+v", plan)
	}
	if read(t, a) != "a2\n" {
		t.Fatal("preview changed the file")
	}

	res, err := s.Undo()
	if err != nil {
		t.Fatal(err)
	}
	if read(t, a) != "a1 again\n" || len(res.Restored) != 1 || res.Checkpoints[0] != list[1].ID {
		t.Fatalf("after first undo: %q %+v", read(t, a), res)
	}
	if !strings.Contains(res.String(), "restored 1 file") {
		t.Fatalf("String() = %q", res.String())
	}
	res, err = s.Undo()
	if err != nil {
		t.Fatal(err)
	}
	if read(t, a) != "a0\n" || read(t, b) != "b0\n" || len(res.Restored) != 2 {
		t.Fatalf("after second undo: %q %q %+v", read(t, a), read(t, b), res)
	}
	if _, err := s.Undo(); !errors.Is(err, ErrNothingToUndo) {
		t.Fatalf("third undo: %v", err)
	}
	if _, err := s.PreviewUndo(); !errors.Is(err, ErrNothingToUndo) {
		t.Fatalf("preview with nothing: %v", err)
	}
}

func TestRestoreRewindsLaterCheckpoints(t *testing.T) {
	work := t.TempDir()
	s := open(t, t.TempDir(), Options{})
	f := filepath.Join(work, "f.txt")
	g := filepath.Join(work, "g.txt")
	put(t, f, "v0\n", 0o644)

	var ids []int
	for i := 1; i <= 4; i++ {
		s.Begin(fmt.Sprintf("turn %d", i))
		editFile(t, s, f, fmt.Sprintf("v%d\n", i))
		if i == 3 {
			editFile(t, s, g, "created in turn 3\n")
		}
		ids = append(ids, s.List()[i-1].ID)
	}

	plan, err := s.Preview(ids[1])
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(plan.Checkpoints) != fmt.Sprint([]int{ids[3], ids[2], ids[1]}) || len(plan.Changes) != 2 {
		t.Fatalf("plan = %+v", plan)
	}
	if plan.Changes[1].Action != ActionDelete || !strings.Contains(plan.Changes[1].Diff, "-created in turn 3") {
		t.Fatalf("delete change = %+v", plan.Changes[1])
	}
	if out := plan.String(); !strings.Contains(out, "delete") || !strings.Contains(out, "+v1") {
		t.Fatalf("plan text:\n%s", out)
	}

	res, err := s.Restore(ids[1])
	if err != nil {
		t.Fatal(err)
	}
	if read(t, f) != "v1\n" || exists(g) || len(res.Deleted) != 1 || len(res.Checkpoints) != 3 {
		t.Fatalf("after restore: %q exists(g)=%v %+v", read(t, f), exists(g), res)
	}
	if list := s.List(); len(list) != 1 || list[0].ID != ids[0] {
		t.Fatalf("remaining = %+v", list)
	}
	if _, err := s.Restore(ids[2]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("restore removed id: %v", err)
	}
	if _, err := s.Preview(999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("preview unknown id: %v", err)
	}

	// New edits after a restore get fresh ids, never reused ones.
	s.Begin("after restore")
	editFile(t, s, f, "v5\n")
	if list := s.List(); list[1].ID <= ids[3] {
		t.Fatalf("id reused: %+v", list)
	}
}

func TestCreatedFilesAndDirectories(t *testing.T) {
	work := t.TempDir()
	s := open(t, t.TempDir(), Options{})
	nested := filepath.Join(work, "new", "deep", "file.go")
	keep := filepath.Join(work, "existing", "file.go")
	put(t, filepath.Join(work, "existing", "other.txt"), "x", 0o644)

	s.Begin("create")
	editFile(t, s, nested, "package x\n")
	editFile(t, s, keep, "package y\n")
	if list := s.List(); list[0].Created != 2 {
		t.Fatalf("created = %d", list[0].Created)
	}
	if _, err := s.Undo(); err != nil {
		t.Fatal(err)
	}
	if exists(nested) || exists(filepath.Join(work, "new")) {
		t.Fatal("created file or its new directories survived undo")
	}
	if exists(keep) || !exists(filepath.Join(work, "existing", "other.txt")) {
		t.Fatal("pre-existing directory content was touched")
	}

	// A created file that was already removed again is simply unchanged.
	s.Begin("create and remove")
	editFile(t, s, nested, "x\n")
	if err := os.RemoveAll(filepath.Join(work, "new")); err != nil {
		t.Fatal(err)
	}
	res, err := s.Undo()
	if err != nil || len(res.Unchanged) != 1 {
		t.Fatalf("undo of vanished file: %v %+v", err, res)
	}

	// A file deleted after being edited is recreated.
	gone := filepath.Join(work, "gone.txt")
	put(t, gone, "original\n", 0o600)
	s.Begin("edit then delete")
	editFile(t, s, gone, "edited\n")
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	plan, err := s.PreviewUndo()
	if err != nil || plan.Changes[0].Action != ActionRecreate {
		t.Fatalf("plan: %v %+v", err, plan)
	}
	if _, err := s.Undo(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(gone)
	if err != nil || read(t, gone) != "original\n" || st.Mode().Perm() != 0o600 {
		t.Fatalf("recreated: %v %v", err, st)
	}
}

func TestModesAndSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes and symlinks differ on Windows")
	}
	work := t.TempDir()
	s := open(t, t.TempDir(), Options{})
	script := filepath.Join(work, "run.sh")
	put(t, script, "#!/bin/sh\necho hi\n", 0o755)
	target := filepath.Join(work, "target.txt")
	put(t, target, "target\n", 0o644)
	link := filepath.Join(work, "link.txt")
	if err := os.Symlink("target.txt", link); err != nil {
		t.Fatal(err)
	}

	s.Begin("modes")
	editFile(t, s, script, "#!/bin/sh\necho changed\n") // put chmods to 0644
	if _, err := s.Record(link); err != nil {
		t.Fatal(err)
	}
	// Tools write with temp file + rename, which replaces the link.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	put(t, link, "now a regular file\n", 0o644)

	if _, err := s.Undo(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(script)
	if err != nil || st.Mode().Perm() != 0o755 || read(t, script) != "#!/bin/sh\necho hi\n" {
		t.Fatalf("script restored with mode %v: %v", st.Mode(), err)
	}
	if got, err := os.Readlink(link); err != nil || got != "target.txt" {
		t.Fatalf("link = %q, %v", got, err)
	}
	if read(t, target) != "target\n" {
		t.Fatal("link target was modified")
	}

	// A mode-only change is restored too.
	s.Begin("chmod")
	if _, err := s.Record(script); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(script, 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := s.PreviewUndo()
	if err != nil || plan.Changes[0].Action != ActionRestore || !strings.Contains(plan.Changes[0].Reason, "mode") {
		t.Fatalf("mode plan: %v %+v", err, plan)
	}
	if _, err := s.Undo(); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(script); st.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v", st.Mode())
	}
}

func TestSkippedFiles(t *testing.T) {
	work := t.TempDir()
	s := open(t, t.TempDir(), Options{MaxFileSize: 1024})
	big := filepath.Join(work, "big.txt")
	bin := filepath.Join(work, "image.png")
	small := filepath.Join(work, "small.txt")
	put(t, big, strings.Repeat("x", 2048), 0o644)
	put(t, bin, "\x89PNG\x00\x00binary", 0o644)
	put(t, small, "small\n", 0o644)

	s.Begin("mixed")
	w, err := s.Record(big)
	if err != nil || !strings.Contains(w, "larger than 1.0 KB") || !strings.Contains(w, big) {
		t.Fatalf("big: %q %v", w, err)
	}
	w, err = s.Record(bin)
	if err != nil || !strings.Contains(w, "binary") {
		t.Fatalf("binary: %q %v", w, err)
	}
	editFile(t, s, small, "changed\n")
	put(t, big, "rewritten", 0o644)
	put(t, bin, "rewritten", 0o644)

	sum := s.List()[0]
	if len(sum.Files) != 3 || len(sum.Skipped) != 2 {
		t.Fatalf("summary = %+v", sum)
	}
	plan, err := s.PreviewUndo()
	if err != nil {
		t.Fatal(err)
	}
	skips := 0
	for _, c := range plan.Changes {
		if c.Action == ActionSkip {
			skips++
		}
	}
	if skips != 2 {
		t.Fatalf("plan = %+v", plan)
	}
	res, err := s.Undo()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 2 || len(res.Restored) != 1 || read(t, small) != "small\n" || read(t, big) != "rewritten" {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(res.String(), "2 skipped") {
		t.Fatalf("String() = %q", res.String())
	}

	// The default limit is 5 MB.
	d := open(t, t.TempDir(), Options{})
	huge := filepath.Join(work, "huge.txt")
	put(t, huge, strings.Repeat("y", DefaultMaxFileSize+1), 0o644)
	if w, err := d.Record(huge); err != nil || !strings.Contains(w, "larger than 5.0 MB") {
		t.Fatalf("default limit: %q %v", w, err)
	}
}

func TestPersistenceAcrossStores(t *testing.T) {
	work := t.TempDir()
	dir := filepath.Join(t.TempDir(), "checkpoints", "session-1")
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s := open(t, dir, Options{Now: func() time.Time { return now }})
	f := filepath.Join(work, "f.txt")
	put(t, f, "before\n", 0o644)
	s.Begin("persisted turn")
	editFile(t, s, f, "after\n")
	created := filepath.Join(work, "created.txt")
	editFile(t, s, created, "new\n")

	// A new process: a fresh Store over the same directory.
	s2 := open(t, dir, Options{})
	list := s2.List()
	if len(list) != 1 || list[0].Label != "persisted turn" || !list[0].Time.Equal(now) || len(list[0].Files) != 2 {
		t.Fatalf("reloaded list = %+v", list)
	}
	cp, err := s2.Get(list[0].ID)
	if err != nil || len(cp.Files) != 2 {
		t.Fatalf("Get = %+v, %v", cp, err)
	}
	if _, err := s2.Undo(); err != nil {
		t.Fatal(err)
	}
	if read(t, f) != "before\n" || exists(created) {
		t.Fatal("undo after reopen did not restore")
	}
	// Restored content is garbage-collected once nothing refers to it.
	var blobs int
	_ = filepath.WalkDir(filepath.Join(dir, blobDirName), func(_ string, d os.DirEntry, _ error) error {
		if d != nil && !d.IsDir() {
			blobs++
		}
		return nil
	})
	if blobs != 0 {
		t.Fatalf("%d blobs left after undo", blobs)
	}

	// Ids keep increasing across reopen.
	s2.Begin("next")
	editFile(t, s2, f, "again\n")
	s3 := open(t, dir, Options{})
	if got := s3.List(); len(got) != 1 || got[0].ID <= list[0].ID {
		t.Fatalf("ids after reopen = %+v", got)
	}
}

func TestCorruptionIsDetected(t *testing.T) {
	work := t.TempDir()
	dir := t.TempDir()
	s := open(t, dir, Options{})
	f := filepath.Join(work, "f.txt")
	put(t, f, "before\n", 0o644)
	s.Begin("x")
	editFile(t, s, f, "after\n")

	// Tampered content is refused rather than written back.
	cp, _ := s.Get(s.List()[0].ID)
	if err := os.WriteFile(s.blobPath(cp.Files[0].Blob), []byte("evil\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Undo(); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("undo with corrupt blob: %v", err)
	}
	if read(t, f) != "after\n" || s.Len() != 1 {
		t.Fatal("failed restore must leave the file and the checkpoint alone")
	}

	if err := os.WriteFile(filepath.Join(dir, indexName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, Options{}); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("corrupt index: %v", err)
	}
}

func TestRetentionAndClear(t *testing.T) {
	work := t.TempDir()
	s := open(t, t.TempDir(), Options{MaxCheckpoints: 3})
	f := filepath.Join(work, "f.txt")
	put(t, f, "0\n", 0o644)
	for i := 1; i <= 5; i++ {
		s.Begin(fmt.Sprintf("turn %d", i))
		editFile(t, s, f, fmt.Sprintf("%d\n", i))
	}
	list := s.List()
	if len(list) != 3 || list[0].Label != "turn 3" {
		t.Fatalf("retained = %+v", list)
	}
	if _, err := s.Restore(list[0].ID); err != nil {
		t.Fatal(err)
	}
	if read(t, f) != "2\n" {
		t.Fatalf("oldest retained state = %q", read(t, f))
	}
	s.Begin("again")
	editFile(t, s, f, "x\n")
	if err := s.Clear(); err != nil {
		t.Fatal(err)
	}
	if s.Len() != 0 {
		t.Fatal("Clear left checkpoints")
	}
	if _, err := s.Undo(); !errors.Is(err, ErrNothingToUndo) {
		t.Fatalf("undo after clear: %v", err)
	}
}

func TestRecordValidation(t *testing.T) {
	s := open(t, t.TempDir(), Options{})
	if _, err := s.Record("relative/path"); err == nil {
		t.Fatal("relative path accepted")
	}
	if _, err := s.Record(t.TempDir()); err == nil {
		t.Fatal("directory accepted")
	}
	if s.Len() != 0 {
		t.Fatal("failed records created a checkpoint")
	}
	// Record without Begin starts a checkpoint automatically.
	f := filepath.Join(t.TempDir(), "auto.txt")
	editFile(t, s, f, "x")
	if list := s.List(); len(list) != 1 || list[0].Label == "" {
		t.Fatalf("auto checkpoint = %+v", list)
	}
	// After undo, more edits in the same turn open a new checkpoint.
	s.Begin("turn")
	editFile(t, s, f, "y")
	if _, err := s.Undo(); err != nil {
		t.Fatal(err)
	}
	editFile(t, s, f, "z")
	if list := s.List(); len(list) != 2 || list[1].Label != "turn" {
		t.Fatalf("after undo mid-turn = %+v", list)
	}

	if _, err := SessionDir("/base", "../escape"); err == nil {
		t.Fatal("bad session id accepted")
	}
	if d, err := SessionDir("/base", "abc-123"); err != nil || d != filepath.Join("/base", "checkpoints", "abc-123") {
		t.Fatalf("SessionDir = %q, %v", d, err)
	}
	if _, err := Open("", Options{}); err == nil {
		t.Fatal("empty dir accepted")
	}
}

func TestConcurrentRecord(t *testing.T) {
	work := t.TempDir()
	s := open(t, t.TempDir(), Options{})
	const n = 40
	paths := make([]string, n)
	for i := range paths {
		paths[i] = filepath.Join(work, fmt.Sprintf("f%02d.txt", i))
		put(t, paths[i], fmt.Sprintf("orig %d\n", i), 0o644)
	}
	s.Begin("parallel")
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for range 3 { // duplicate records must be harmless
				if _, err := s.Record(paths[i]); err != nil {
					t.Error(err)
					return
				}
			}
			_ = os.WriteFile(paths[i], []byte("changed\n"), 0o644)
			_ = s.List()
		}(i)
	}
	wg.Wait()
	if list := s.List(); len(list) != 1 || len(list[0].Files) != n {
		t.Fatalf("list = %d checkpoints", len(list))
	}
	res, err := s.Undo()
	if err != nil || len(res.Restored) != n {
		t.Fatalf("undo: %v %+v", err, res)
	}
	for i, p := range paths {
		if read(t, p) != fmt.Sprintf("orig %d\n", i) {
			t.Fatalf("%s not restored", p)
		}
	}
}

func TestUnifiedDiff(t *testing.T) {
	cases := []struct {
		a, b string
		want string
	}{
		{"a\nb\nc\n", "a\nB\nc\n", "--- x\n+++ x\n@@ -1,3 +1,3 @@\n a\n-b\n+B\n c\n"},
		{"", "new\n", "--- x\n+++ x\n@@ -0,0 +1 @@\n+new\n"},
		{"old\n", "", "--- x\n+++ x\n@@ -1 +0,0 @@\n-old\n"},
		{"a\n", "a", "--- x\n+++ x\n@@ -1 +1 @@\n-a\n+a\n\\ No newline at end of file\n"},
		{"same\n", "same\n", ""},
	}
	for _, c := range cases {
		if got := unifiedDiff("x", "x", c.a, c.b); got != c.want {
			t.Errorf("diff(%q, %q) =\n%s\nwant\n%s", c.a, c.b, got, c.want)
		}
	}
	// Distant changes produce separate hunks with correct line numbers.
	var a, b strings.Builder
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&a, "%d\n", i)
		if i == 2 || i == 25 {
			fmt.Fprintf(&b, "changed %d\n", i)
		} else {
			fmt.Fprintf(&b, "%d\n", i)
		}
	}
	d := unifiedDiff("x", "x", a.String(), b.String())
	if !strings.Contains(d, "@@ -1,5 +1,5 @@") || !strings.Contains(d, "@@ -22,7 +22,7 @@") {
		t.Fatalf("multi-hunk diff:\n%s", d)
	}
}
