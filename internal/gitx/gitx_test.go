package gitx

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newRepo creates an isolated repository with one commit containing
// a.txt, b.txt and keep.txt. Global and system git config are ignored so
// the user's setup cannot influence results.
func newRepo(t *testing.T) string {
	t.Helper()
	if !Available() {
		t.Skip("git is not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_DATE", "")
	t.Setenv("GIT_COMMITTER_DATE", "")
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "config", "user.name", "Test User")
	git(t, dir, "config", "user.email", "test@example.com")
	git(t, dir, "config", "commit.gpgsign", "false")
	write(t, dir, "a.txt", "one\ntwo\nthree\n")
	write(t, dir, "b.txt", "bee\n")
	write(t, dir, "keep.txt", strings.Repeat("line of text that is long enough to make rename detection reliable\n", 20))
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "initial")
	return dir
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestIsRepo(t *testing.T) {
	dir := newRepo(t)
	if !IsRepo(dir) {
		t.Fatal("IsRepo = false for a repository")
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !IsRepo(filepath.Join(dir, "sub")) {
		t.Fatal("IsRepo = false for a subdirectory")
	}
	if IsRepo(t.TempDir()) {
		t.Fatal("IsRepo = true for a plain directory")
	}
	if IsRepo(filepath.Join(dir, "missing")) {
		t.Fatal("IsRepo = true for a missing directory")
	}
	if _, err := Info(ctx(t), t.TempDir()); !errors.Is(err, ErrNotRepo) {
		t.Fatalf("Info outside a repo: err = %v, want ErrNotRepo", err)
	}
}

func TestInfo(t *testing.T) {
	dir := newRepo(t)
	st, err := Info(ctx(t), dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Branch != "main" || st.Detached || !st.Clean() || st.Head == "" || len(st.HeadFull) < 40 {
		t.Fatalf("clean status = %+v", st)
	}
	if !strings.HasPrefix(st.HeadFull, st.Head) {
		t.Fatalf("short %q is not a prefix of %q", st.Head, st.HeadFull)
	}
	if !strings.Contains(st.String(), "main@"+st.Head) || !strings.Contains(st.String(), "clean") {
		t.Fatalf("String() = %q", st.String())
	}

	write(t, dir, "a.txt", "changed\n")             // unstaged
	write(t, dir, "b.txt", "staged\n")              // staged, then modified again
	git(t, dir, "add", "b.txt")                     //
	write(t, dir, "b.txt", "staged and modified\n") //
	write(t, dir, "new/x.txt", "x\n")               // untracked, in a new dir
	write(t, dir, "new/y.txt", "y\n")               // counted per file
	git(t, dir, "mv", "keep.txt", "kept.txt")       // staged rename
	write(t, dir, "with space.txt", "s\n")          // untracked with a space

	st, err = Info(ctx(t), dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Staged != 2 || st.Unstaged != 2 || st.Untracked != 3 || st.Conflicted != 0 {
		t.Fatalf("counts staged=%d unstaged=%d untracked=%d conflicted=%d; entries %+v",
			st.Staged, st.Unstaged, st.Untracked, st.Conflicted, st.Entries)
	}
	var rename *StatusEntry
	for i, e := range st.Entries {
		if e.Kind == EntryRenamed {
			rename = &st.Entries[i]
		}
	}
	if rename == nil || rename.Path != "kept.txt" || rename.OrigPath != "keep.txt" || rename.Index != 'R' {
		t.Fatalf("rename entry = %+v", rename)
	}
	if s := st.String(); !strings.Contains(s, "2 staged") || !strings.Contains(s, "3 untracked") {
		t.Fatalf("String() = %q", s)
	}
}

func TestInfoUpstreamAndDetached(t *testing.T) {
	remote := newRepo(t)
	clone := filepath.Join(t.TempDir(), "clone")
	git(t, remote, "clone", "-q", remote, clone)
	git(t, clone, "config", "user.name", "Test User")
	git(t, clone, "config", "user.email", "test@example.com")

	write(t, clone, "c.txt", "c\n")
	git(t, clone, "add", "c.txt")
	git(t, clone, "commit", "-q", "-m", "local")
	write(t, remote, "d.txt", "d\n")
	git(t, remote, "add", "d.txt")
	git(t, remote, "commit", "-q", "-m", "remote 1")
	write(t, remote, "e.txt", "e\n")
	git(t, remote, "add", "e.txt")
	git(t, remote, "commit", "-q", "-m", "remote 2")
	git(t, clone, "fetch", "-q")

	st, err := Info(ctx(t), clone)
	if err != nil {
		t.Fatal(err)
	}
	if st.Upstream != "origin/main" || st.Ahead != 1 || st.Behind != 2 || st.UpstreamGone {
		t.Fatalf("upstream=%q ahead=%d behind=%d gone=%v", st.Upstream, st.Ahead, st.Behind, st.UpstreamGone)
	}
	if s := st.String(); !strings.Contains(s, "↑1") || !strings.Contains(s, "↓2") {
		t.Fatalf("String() = %q", s)
	}

	git(t, clone, "checkout", "-q", "--detach", "HEAD")
	st, err = Info(ctx(t), clone)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Detached || st.Branch != "" || st.Upstream != "" {
		t.Fatalf("detached status = %+v", st)
	}
}

func TestInfoInitialAndConflict(t *testing.T) {
	if !Available() {
		t.Skip("git is not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "trunk")
	write(t, dir, "x.txt", "x\n")
	st, err := Info(ctx(t), dir)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Initial || st.Branch != "trunk" || st.Head != "" || st.Untracked != 1 {
		t.Fatalf("initial status = %+v", st)
	}
	if !strings.Contains(st.String(), "no commits") {
		t.Fatalf("String() = %q", st.String())
	}

	repo := newRepo(t)
	git(t, repo, "checkout", "-q", "-b", "other")
	write(t, repo, "a.txt", "other side\n")
	git(t, repo, "commit", "-q", "-am", "other")
	git(t, repo, "checkout", "-q", "main")
	write(t, repo, "a.txt", "main side\n")
	git(t, repo, "commit", "-q", "-am", "main")
	cmd := exec.Command("git", "-C", repo, "merge", "-q", "other")
	if err := cmd.Run(); err == nil {
		t.Fatal("expected a merge conflict")
	}
	st, err = Info(ctx(t), repo)
	if err != nil {
		t.Fatal(err)
	}
	if st.Conflicted != 1 || st.Entries[0].Kind != EntryUnmerged || st.Entries[0].Path != "a.txt" {
		t.Fatalf("conflict status = %+v", st)
	}
}

func TestParseStatusMalformed(t *testing.T) {
	for _, in := range []string{"1 M", "2 RM N... 100644 100644 100644 a b R100 new", "zzz"} {
		if _, err := parseStatus([]byte(in + "\x00")); err == nil {
			t.Errorf("parseStatus(%q) succeeded", in)
		}
	}
}

func findFile(t *testing.T, r *DiffResult, path string) FileDiff {
	t.Helper()
	for _, f := range r.Files {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("no diff for %s in %+v", path, r.Files)
	return FileDiff{}
}

func TestDiffAllKinds(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "a.txt", "one\nTWO\nthree\nfour\n")
	if err := os.Remove(filepath.Join(dir, "b.txt")); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "mv", "keep.txt", "moved.txt")
	write(t, dir, "added.txt", "staged new\n")
	git(t, dir, "add", "added.txt")
	write(t, dir, "untracked/u.txt", "u1\nu2")
	write(t, dir, "bin.dat", "\x00\x01\x02binary")
	write(t, dir, ".gitignore", "*.log\n")
	write(t, dir, "ignored.log", "nope\n")

	r, err := Diff(ctx(t), dir, DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Base != "HEAD" || r.Root != dir {
		t.Fatalf("base=%q root=%q", r.Base, r.Root)
	}

	a := findFile(t, r, "a.txt")
	if a.Status != StatusModified || a.Added != 2 || a.Deleted != 1 || len(a.Hunks) != 1 {
		t.Fatalf("a.txt = %+v", a)
	}
	h := a.Hunks[0]
	if h.OldStart != 1 || h.OldLines != 3 || h.NewStart != 1 || h.NewLines != 4 {
		t.Fatalf("a.txt hunk = %+v", h)
	}
	if b := findFile(t, r, "b.txt"); b.Status != StatusDeleted || b.Deleted != 1 || b.OldMode != "100644" {
		t.Fatalf("b.txt = %+v", b)
	}
	if m := findFile(t, r, "moved.txt"); m.Status != StatusRenamed || m.OldPath != "keep.txt" || m.Similarity != 100 {
		t.Fatalf("moved.txt = %+v", m)
	}
	if ad := findFile(t, r, "added.txt"); ad.Status != StatusAdded || ad.Untracked || ad.Added != 1 || ad.NewMode != "100644" {
		t.Fatalf("added.txt = %+v", ad)
	}
	u := findFile(t, r, "untracked/u.txt")
	if u.Status != StatusAdded || !u.Untracked || u.Added != 2 || len(u.Hunks) != 1 {
		t.Fatalf("untracked = %+v", u)
	}
	if last := u.Hunks[0].Lines[len(u.Hunks[0].Lines)-1]; last != `\ No newline at end of file` {
		t.Fatalf("untracked last line = %q", last)
	}
	if !strings.Contains(u.Patch, "@@ -0,0 +1,2 @@") || !strings.Contains(u.Patch, "--- /dev/null\n+++ b/untracked/u.txt") {
		t.Fatalf("untracked patch:\n%s", u.Patch)
	}
	if bin := findFile(t, r, "bin.dat"); !bin.Binary || !bin.Untracked || len(bin.Hunks) != 0 {
		t.Fatalf("bin.dat = %+v", bin)
	}
	for _, f := range r.Files {
		if f.Path == "ignored.log" {
			t.Fatal("ignored file included")
		}
	}
	if r.Added != a.Added+1+2+1 /* .gitignore */ || r.Deleted != 2 {
		t.Fatalf("totals +%d -%d", r.Added, r.Deleted)
	}
	stat := r.Stat()
	for _, want := range []string{"R  keep.txt → moved.txt", "D  b.txt", "binary", "insertions(+)"} {
		if !strings.Contains(stat, want) {
			t.Fatalf("Stat() missing %q:\n%s", want, stat)
		}
	}

	// The rendered text is a valid patch: it reverse-applies cleanly to the
	// work tree for the tracked part.
	tracked, err := Diff(ctx(t), dir, DiffOptions{NoUntracked: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range tracked.Files {
		if f.Untracked {
			t.Fatalf("NoUntracked included %s", f.Path)
		}
	}
	patch := filepath.Join(t.TempDir(), "p.diff")
	if err := os.WriteFile(patch, []byte(tracked.Text), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "apply", "--check", "-R", patch)
}

func TestDiffStagedAndPaths(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "a.txt", "one\ntwo\nthree\nstaged\n")
	git(t, dir, "add", "a.txt")
	write(t, dir, "b.txt", "unstaged\n")
	write(t, dir, "sub/u.txt", "u\n")

	staged, err := Diff(ctx(t), dir, DiffOptions{Staged: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(staged.Files) != 1 || staged.Files[0].Path != "a.txt" {
		t.Fatalf("staged files = %+v", staged.Files)
	}

	only, err := Diff(ctx(t), dir, DiffOptions{Paths: []string{"b.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(only.Files) != 1 || only.Files[0].Path != "b.txt" {
		t.Fatalf("path-limited files = %+v", only.Files)
	}

	// Paths are relative to the root argument, even a subdirectory, and
	// results stay relative to the top level.
	sub, err := Diff(ctx(t), filepath.Join(dir, "sub"), DiffOptions{Paths: []string{"u.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(sub.Files) != 1 || sub.Files[0].Path != "sub/u.txt" {
		t.Fatalf("subdir files = %+v", sub.Files)
	}
	// From a subdirectory without paths the whole repository is covered.
	all, err := Diff(ctx(t), filepath.Join(dir, "sub"), DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Files) != 3 {
		t.Fatalf("subdir full diff = %+v", all.Files)
	}
	if _, err := Diff(ctx(t), dir, DiffOptions{Paths: []string{"/elsewhere/file"}}); err == nil {
		t.Fatal("path outside the repo accepted")
	}

	clean := newRepo(t)
	r, err := Diff(ctx(t), clean, DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Empty() || r.Text != "" || r.Stat() != "no changes" {
		t.Fatalf("clean diff = %+v", r)
	}
}

func TestDiffBudget(t *testing.T) {
	dir := newRepo(t)
	var big strings.Builder
	for i := 0; i < 400; i++ {
		big.WriteString("line ")
		big.WriteString(strings.Repeat("x", i%50))
		big.WriteString("\n")
	}
	git(t, dir, "rm", "-q", "a.txt")
	write(t, dir, "big.txt", big.String())
	git(t, dir, "add", "big.txt")
	git(t, dir, "commit", "-q", "-m", "big")
	// Change every 20th line so there are many separate hunks.
	lines := strings.Split(big.String(), "\n")
	for i := 0; i < len(lines)-1; i += 20 {
		lines[i] = "changed " + lines[i]
	}
	write(t, dir, "big.txt", strings.Join(lines, "\n"))

	full, err := Diff(ctx(t), dir, DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := Diff(ctx(t), dir, DiffOptions{MaxBytes: 1500})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Truncated || full.Truncated {
		t.Fatalf("truncated=%v full.truncated=%v", r.Truncated, full.Truncated)
	}
	f := r.Files[0]
	if !f.Omitted || len(f.Hunks) == 0 || len(f.Hunks) >= len(full.Files[0].Hunks) {
		t.Fatalf("kept %d of %d hunks", len(f.Hunks), len(full.Files[0].Hunks))
	}
	if f.Added != full.Files[0].Added || f.Deleted != full.Files[0].Deleted {
		t.Fatal("line counts must cover omitted hunks")
	}
	if !strings.Contains(r.Text, "# diff truncated") || len(r.Text) > 1500+200 {
		t.Fatalf("text (%d bytes) tail: %q", len(r.Text), r.Text[max(0, len(r.Text)-200):])
	}
}

func TestDiffInitialCommit(t *testing.T) {
	if !Available() {
		t.Skip("git is not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	write(t, dir, "tracked.txt", "t\n")
	git(t, dir, "add", "tracked.txt")
	write(t, dir, "loose.txt", "l\n")
	r, err := Diff(ctx(t), dir, DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Base != "empty tree" || len(r.Files) != 2 {
		t.Fatalf("initial diff = %+v", r)
	}
	staged, err := Diff(ctx(t), dir, DiffOptions{Staged: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(staged.Files) != 1 || staged.Files[0].Path != "tracked.txt" || staged.Files[0].Status != StatusAdded {
		t.Fatalf("initial staged diff = %+v", staged.Files)
	}
}

func TestParsePatchEdgeCases(t *testing.T) {
	// A deleted content line that looks like a header, quoted paths, a
	// path with spaces and a mode-only change.
	patch := "diff --git a/x.txt b/x.txt\n" +
		"index 1111111..2222222 100644\n" +
		"--- a/x.txt\n" +
		"+++ b/x.txt\n" +
		"@@ -1,2 +1 @@\n" +
		"--- not a header\n" +
		" keep\n" +
		"diff --git \"a/tab\\there.txt\" \"b/tab\\there.txt\"\n" +
		"new file mode 100644\n" +
		"--- /dev/null\n" +
		"+++ \"b/tab\\there.txt\"\n" +
		"@@ -0,0 +1 @@\n" +
		"+hi\n" +
		"\\ No newline at end of file\n" +
		"diff --git a/with space.txt b/with space.txt\n" +
		"old mode 100644\n" +
		"new mode 100755\n" +
		"diff --git a/caf\303\251 b/caf\303\251\n" +
		"deleted file mode 100644\n" +
		"index 3333333..0000000\n" +
		"Binary files a/caf\303\251 and /dev/null differ\n"
	files, err := parsePatch(patch)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 4 {
		t.Fatalf("got %d files: %+v", len(files), files)
	}
	if f := files[0]; f.Path != "x.txt" || f.Deleted != 1 || f.Added != 0 || len(f.Hunks[0].Lines) != 2 {
		t.Fatalf("file 0 = %+v", f)
	}
	if f := files[1]; f.Path != "tab\there.txt" || f.Status != StatusAdded || len(f.Hunks[0].Lines) != 2 {
		t.Fatalf("file 1 = %+v", f)
	}
	if f := files[2]; f.Path != "with space.txt" || f.OldMode != "100644" || f.NewMode != "100755" || f.Status != StatusModified {
		t.Fatalf("file 2 = %+v", f)
	}
	if f := files[3]; f.Path != "café" || f.Status != StatusDeleted || !f.Binary {
		t.Fatalf("file 3 = %+v", f)
	}
	if _, err := parsePatch("diff --git a/x b/x\n@@ -1,3 +1,3 @@\n a\n"); err == nil {
		t.Fatal("truncated hunk accepted")
	}
	if q := quotePath("a/tab\there\"q"); q != `"a/tab\there\"q"` || unquotePath(q) != "a/tab\there\"q" {
		t.Fatalf("quote round trip: %q", q)
	}
	if u := unquotePath(`"caf\303\251"`); u != "café" {
		t.Fatalf("octal unquote = %q", u)
	}
}

func TestCommit(t *testing.T) {
	dir := newRepo(t)
	if _, err := Commit(ctx(t), dir, CommitOptions{Message: "   "}); err == nil {
		t.Fatal("empty message accepted")
	}
	if _, err := Commit(ctx(t), dir, CommitOptions{Message: "nothing", All: true}); !errors.Is(err, ErrNothingToCommit) {
		t.Fatalf("empty commit: err = %v", err)
	}
	before := git(t, dir, "rev-parse", "HEAD")

	// Paths: only the selected files are committed; other staged work stays.
	write(t, dir, "a.txt", "changed a\n")
	write(t, dir, "b.txt", "changed b\n")
	write(t, dir, "new.txt", "new\n")
	git(t, dir, "add", "b.txt")
	res, err := Commit(ctx(t), dir, CommitOptions{Message: "# Heading kept\n\nUpdate a and add new", Paths: []string{"a.txt", "new.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.SHA == before || res.Branch != "main" || res.Subject != "# Heading kept" || res.FilesChanged != 2 || res.Insertions != 2 || res.Deletions != 3 {
		t.Fatalf("commit result = %+v", res)
	}
	if got := git(t, dir, "rev-parse", "HEAD"); got != res.SHA || !strings.HasPrefix(res.SHA, res.Short) {
		t.Fatalf("HEAD %s, result %s/%s", got, res.SHA, res.Short)
	}
	if files := git(t, dir, "show", "--name-only", "--format=", "HEAD"); files != "a.txt\nnew.txt" {
		t.Fatalf("committed files = %q", files)
	}
	if staged := git(t, dir, "diff", "--cached", "--name-only"); staged != "b.txt" {
		t.Fatalf("still staged = %q", staged)
	}

	// Deletions by path are committed too.
	if err := os.Remove(filepath.Join(dir, "keep.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := Commit(ctx(t), dir, CommitOptions{Message: "remove keep", Paths: []string{"keep.txt"}}); err != nil {
		t.Fatal(err)
	}

	// Nothing selected: commit what is staged.
	res, err = Commit(ctx(t), dir, CommitOptions{Message: "staged b"})
	if err != nil {
		t.Fatal(err)
	}
	if files := git(t, dir, "show", "--name-only", "--format=", "HEAD"); files != "b.txt" {
		t.Fatalf("staged commit files = %q", files)
	}

	// All: new, modified and deleted.
	write(t, dir, "c.txt", "c\n")
	if err := os.Remove(filepath.Join(dir, "new.txt")); err != nil {
		t.Fatal(err)
	}
	res, err = Commit(ctx(t), dir, CommitOptions{Message: "all", All: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.FilesChanged != 2 {
		t.Fatalf("all commit = %+v", res)
	}
	st, err := Info(ctx(t), dir)
	if err != nil || !st.Clean() {
		t.Fatalf("after commit: %v %+v", err, st)
	}
	if _, err := Commit(ctx(t), dir, CommitOptions{Message: "x", Paths: []string{"a.txt"}}); !errors.Is(err, ErrNothingToCommit) {
		t.Fatalf("unchanged path: err = %v", err)
	}
}

func TestCommitInitial(t *testing.T) {
	if !Available() {
		t.Skip("git is not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "config", "user.name", "Test User")
	git(t, dir, "config", "user.email", "test@example.com")
	if _, err := Commit(ctx(t), dir, CommitOptions{Message: "x", All: true}); !errors.Is(err, ErrNothingToCommit) {
		t.Fatalf("empty initial commit: err = %v", err)
	}
	write(t, dir, "first.txt", "1\n")
	res, err := Commit(ctx(t), dir, CommitOptions{Message: "first", Paths: []string{"first.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.FilesChanged != 1 || res.Subject != "first" {
		t.Fatalf("initial commit = %+v", res)
	}
	log, err := Log(ctx(t), dir, 5)
	if err != nil || len(log) != 1 {
		t.Fatalf("log after initial commit: %v %+v", err, log)
	}
}

func TestWorktrees(t *testing.T) {
	dir := newRepo(t)
	for _, bad := range []string{"", "../x", "a/b", ".hidden", "x..y", "x.lock", "end.", strings.Repeat("a", 65), "sp ace"} {
		if _, err := CreateWorktree(ctx(t), dir, bad); err == nil {
			t.Errorf("CreateWorktree(%q) accepted", bad)
		}
	}

	wt, err := CreateWorktree(ctx(t), dir, "feature-1")
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(dir, ".gta", "worktrees", "feature-1")
	if wt.Path != wantPath || wt.Branch != "gta/feature-1" || !wt.Managed || wt.Name != "feature-1" {
		t.Fatalf("created = %+v", wt)
	}
	if _, err := os.Stat(filepath.Join(wt.Path, "a.txt")); err != nil {
		t.Fatalf("worktree content: %v", err)
	}
	if _, err := CreateWorktree(ctx(t), dir, "feature-1"); !errors.Is(err, ErrWorktreeExists) {
		t.Fatalf("duplicate: err = %v", err)
	}
	// The worktree directory does not pollute the main checkout's status.
	st, err := Info(ctx(t), dir)
	if err != nil || !st.Clean() {
		t.Fatalf("main status after create: %v %+v", err, st)
	}

	list, err := ListWorktrees(ctx(t), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || !list[0].Main || list[0].Path != dir || list[0].Branch != "main" || !list[1].Managed {
		t.Fatalf("list = %+v", list)
	}
	// Listing from inside the worktree sees the same set.
	if inner, err := ListWorktrees(ctx(t), wt.Path); err != nil || len(inner) != 2 {
		t.Fatalf("list from worktree: %v %+v", err, inner)
	}

	// Dirty worktree: refused without force.
	write(t, wt.Path, "dirty.txt", "d\n")
	if _, err := RemoveWorktree(ctx(t), dir, "feature-1", false); !errors.Is(err, ErrWorktreeDirty) {
		t.Fatalf("dirty remove: err = %v", err)
	}
	// Commit in the worktree, then remove: unmerged branch is kept.
	if _, err := Commit(ctx(t), wt.Path, CommitOptions{Message: "work", All: true}); err != nil {
		t.Fatal(err)
	}
	rm, err := RemoveWorktree(ctx(t), dir, "feature-1", false)
	if err != nil {
		t.Fatal(err)
	}
	if rm.BranchDeleted || rm.BranchNote == "" {
		t.Fatalf("unmerged branch removed: %+v", rm)
	}
	if _, err := os.Stat(wantPath); !os.IsNotExist(err) {
		t.Fatalf("worktree dir still exists: %v", err)
	}
	if _, err := RemoveWorktree(ctx(t), dir, "feature-1", false); !errors.Is(err, ErrWorktreeNotFound) {
		t.Fatalf("second remove: err = %v", err)
	}
	// The kept branch blocks re-creating the same name.
	if _, err := CreateWorktree(ctx(t), dir, "feature-1"); !errors.Is(err, ErrWorktreeExists) {
		t.Fatalf("create over kept branch: err = %v", err)
	}
	git(t, dir, "branch", "-D", "gta/feature-1")

	// Force removes dirty worktrees and their branches.
	wt2, err := CreateWorktree(ctx(t), dir, "f2")
	if err != nil {
		t.Fatal(err)
	}
	write(t, wt2.Path, "x.txt", "x\n")
	rm, err = RemoveWorktree(ctx(t), dir, "f2", true)
	if err != nil || !rm.BranchDeleted {
		t.Fatalf("force remove: %v %+v", err, rm)
	}

	// A worktree deleted by hand is pruned, and a merged branch is deleted.
	wt3, err := CreateWorktree(ctx(t), dir, "f3")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(wt3.Path); err != nil {
		t.Fatal(err)
	}
	rm, err = RemoveWorktree(ctx(t), dir, "f3", false)
	if err != nil || !rm.BranchDeleted {
		t.Fatalf("remove of deleted dir: %v %+v", err, rm)
	}
	if list, _ := ListWorktrees(ctx(t), dir); len(list) != 1 {
		t.Fatalf("leftover worktrees: %+v", list)
	}
	exclude, err := os.ReadFile(filepath.Join(dir, ".git", "info", "exclude"))
	if err != nil || strings.Count(string(exclude), "/.gta/worktrees/") != 1 {
		t.Fatalf("exclude file: %v %q", err, exclude)
	}
}

func TestWorktreeNeedsCommit(t *testing.T) {
	if !Available() {
		t.Skip("git is not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	if _, err := CreateWorktree(ctx(t), dir, "w"); !errors.Is(err, ErrNoCommits) {
		t.Fatalf("err = %v, want ErrNoCommits", err)
	}
}

func TestLog(t *testing.T) {
	dir := newRepo(t)
	for i, msg := range []string{"second", "third: with | pipes and \"quotes\""} {
		write(t, dir, "a.txt", strings.Repeat("x\n", i+5))
		git(t, dir, "commit", "-q", "-am", msg)
	}
	log, err := Log(ctx(t), dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 2 || log[0].Subject != "third: with | pipes and \"quotes\"" || log[1].Subject != "second" {
		t.Fatalf("log = %+v", log)
	}
	e := log[0]
	if e.Author != "Test User" || e.Email != "test@example.com" || len(e.SHA) < 40 || !strings.HasPrefix(e.SHA, e.Short) {
		t.Fatalf("entry = %+v", e)
	}
	if e.RelDate == "" || time.Since(e.Time) > time.Hour || !strings.Contains(e.String(), e.Short) {
		t.Fatalf("dates: %q %v", e.RelDate, e.Time)
	}
	all, err := Log(ctx(t), dir, 0)
	if err != nil || len(all) != 3 {
		t.Fatalf("default log: %v %d", err, len(all))
	}
}

func TestErrorsAndTimeout(t *testing.T) {
	dir := newRepo(t)
	_, err := run(ctx(t), dir, runOpts{}, "rev-parse", "--verify", "no-such-ref")
	var ge *Error
	if !errors.As(err, &ge) || ge.ExitCode == 0 || !strings.Contains(err.Error(), "git rev-parse") {
		t.Fatalf("err = %v", err)
	}
	c, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Info(c, dir); err == nil {
		t.Fatal("cancelled context did not fail")
	}
	if _, err := Info(context.Background(), filepath.Join(dir, "a.txt")); err == nil {
		t.Fatal("file as root accepted")
	}
}

func TestDiffSpecialPaths(t *testing.T) {
	dir := newRepo(t)
	names := []string{"with space.txt", "tab\tname.txt", "naïve.txt"}
	for _, n := range names {
		write(t, dir, n, "before\n")
	}
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "special")
	for _, n := range names[1:] {
		write(t, dir, n, "after\n")
	}
	git(t, dir, "mv", "with space.txt", "moved space.txt")
	r, err := Diff(ctx(t), dir, DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if m := findFile(t, r, "moved space.txt"); m.Status != StatusRenamed || m.OldPath != "with space.txt" {
		t.Fatalf("renamed with space = %+v", m)
	}
	for _, n := range names[1:] {
		if f := findFile(t, r, n); f.Added != 1 || f.Deleted != 1 {
			t.Fatalf("%q = %+v", n, f)
		}
	}
	only, err := Diff(ctx(t), dir, DiffOptions{Paths: []string{"tab\tname.txt"}})
	if err != nil || len(only.Files) != 1 {
		t.Fatalf("literal path: %v %+v", err, only)
	}
}
