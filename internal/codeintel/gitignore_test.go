package codeintel

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestIgnorerPatterns(t *testing.T) {
	cases := []struct {
		patterns string
		path     string
		dir      bool
		want     bool
	}{
		{"*.log", "a.log", false, true},
		{"*.log", "deep/dir/a.log", false, true},
		{"*.log", "a.log.txt", false, false},
		{"build/", "build", true, true},
		{"build/", "build", false, false},
		{"build/", "src/build", true, true},
		{"/build", "build", false, true},
		{"/build", "src/build", false, false},
		{"doc/frotz", "doc/frotz", true, true},
		{"doc/frotz", "a/doc/frotz", true, false},
		{"frotz/", "a/frotz", true, true},
		{"**/foo", "foo", false, true},
		{"**/foo", "a/b/foo", false, true},
		{"**/foo/bar", "x/foo/bar", false, true},
		{"abc/**", "abc/x", false, true},
		{"abc/**", "abc/x/y", false, true},
		{"abc/**", "abc", true, false},
		{"a/**/b", "a/b", false, true},
		{"a/**/b", "a/x/y/b", false, true},
		{"a/**/b", "a/xb", false, false},
		{"foo**bar", "fooxbar", false, true},
		{"foo**bar", "foo/bar", false, false},
		{"?.txt", "a.txt", false, true},
		{"?.txt", "ab.txt", false, false},
		{"[abc].go", "b.go", false, true},
		{"[!abc].go", "b.go", false, false},
		{"[!abc].go", "d.go", false, true},
		{"[a-c]x", "bx", false, true},
		{"[]x]", "]", false, true},
		{"*.log\n!keep.log", "keep.log", false, false},
		{"*.log\n!keep.log", "other.log", false, true},
		{"!keep.log\n*.log", "keep.log", false, true},
		{`\#hash`, "#hash", false, true},
		{"#comment", "#comment", false, false},
		{`\!bang`, "!bang", false, true},
		{"trail   ", "trail", false, true},
		{`space\ `, "space ", false, true},
		{"", "anything", false, false},
		{"/", "x", false, false},
		{"a.(b)+", "a.(b)+", false, true},
		{"a.(b)+", "aa(b)", false, false},
		{"*", "x/y/z", false, true},
		{"/*.c", "cat-file.c", false, true},
		{"/*.c", "mozilla-sha1/sha1.c", false, false},
		{"[[:digit:]]*.txt", "1abc.txt", false, true},
		{"[[:digit:]]*.txt", "abc.txt", false, false},
	}
	for _, c := range cases {
		ig := NewIgnorer().Extend("", []byte(c.patterns))
		if got := ig.Match(c.path, c.dir); got != c.want {
			t.Errorf("patterns %q path %q dir=%v: got %v want %v", c.patterns, c.path, c.dir, got, c.want)
		}
	}
}

func TestIgnorerNestedBaseAndParents(t *testing.T) {
	ig := NewIgnorer().Extend("", []byte("*.tmp\n")).Extend("sub", []byte("/local\n!important.tmp\ngen/\n"))
	checks := map[string]bool{
		"a.tmp":               true,
		"sub/important.tmp":   false,
		"other/important.tmp": true,
		"sub/local":           true,
		"local":               false,
		"sub/x/local":         false,
	}
	for p, want := range checks {
		if got := ig.Match(p, false); got != want {
			t.Errorf("Match(%q) = %v, want %v", p, got, want)
		}
	}
	// A file below an excluded directory cannot be re-included.
	if !ig.Ignored("sub/gen/important.tmp", false) {
		t.Error("file under excluded dir must stay ignored")
	}
	if ig.Ignored("sub/important.tmp", false) {
		t.Error("negated file should not be ignored")
	}
	// Extend never mutates the parent.
	parent := NewIgnorer().Extend("", []byte("a\n"))
	child := parent.Extend("", []byte("b\n"))
	if parent.Len() != 1 || child.Len() != 2 || parent.Match("b", false) {
		t.Error("Extend mutated its receiver")
	}
	var nilIg *Ignorer
	if nilIg.Match("x", false) || nilIg.Len() != 0 {
		t.Error("nil Ignorer should match nothing")
	}
}

func TestParseExcludesFile(t *testing.T) {
	cfg := "[user]\n\tname = x\n[core]\n\texcludesFile = \"~/.gitignore_global\"\n[]\n"
	if got := parseExcludesFile(cfg); got != "~/.gitignore_global" {
		t.Errorf("got %q", got)
	}
	if got := parseExcludesFile("[core]\n editor = vim\n"); got != "" {
		t.Errorf("got %q", got)
	}
}

// TestIgnorerMatchesGit cross-checks the matcher against git itself on a
// tree with nested ignore files, negations and directory patterns.
func TestIgnorerMatchesGit(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	files := map[string]string{
		".gitignore":            "*.log\n!keep.log\nbuild/\n/top.txt\ndocs/**/*.pdf\n**/cache\nnested/*.tmp\n[Tt]humbs.db\n",
		"a.log":                 "",
		"keep.log":              "",
		"src/b.log":             "",
		"src/keep.log":          "",
		"top.txt":               "",
		"src/top.txt":           "",
		"build/out.o":           "",
		"src/build/x.c":         "",
		"docs/a/b/c.pdf":        "",
		"docs/c.pdf":            "",
		"docs/readme.md":        "",
		"x/cache/y":             "",
		"nested/a.tmp":          "",
		"nested/deeper/b.tmp":   "",
		"Thumbs.db":             "",
		"src/thumbs.db":         "",
		"sub/.gitignore":        "*.gen\n!special.gen\n/only-here\n",
		"sub/a.gen":             "",
		"sub/special.gen":       "",
		"sub/only-here":         "",
		"sub/deep/only-here":    "",
		"sub/deep/x.gen":        "",
		"keep/.gitignore":       "*\n!*.go\n!*/\n",
		"keep/main.go":          "",
		"keep/data.json":        "",
		"keep/inner/util.go":    "",
		"keep/inner/notes.txt":  "",
		"spaces/with space.txt": "",
		"regular.go":            "",
	}
	for p, c := range files {
		abs := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) []byte {
		cmd := exec.Command(gitBin, args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "HOME="+root)
		out, err := cmd.Output()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
				return out // check-ignore exits 1 when nothing matched
			}
			t.Fatalf("git %v: %v", args, err)
		}
		return out
	}
	run("init", "-q")
	// Files git would track (not ignored), as the reference.
	out := run("ls-files", "--others", "--exclude-standard")
	var want []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l != "" && !strings.HasSuffix(l, ".gitignore") {
			want = append(want, strings.Trim(l, `"`))
		}
	}
	sort.Strings(want)
	if len(want) < 10 {
		t.Fatalf("git listed too few files to be a meaningful reference: %v", want)
	}

	ix, err := Open(Options{Root: root, SkipGlobalGitignore: true, Workers: 3})
	if err != nil {
		t.Fatal(err)
	}
	wr, err := ix.walk(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range wr.files {
		if !strings.HasSuffix(f.rel, ".gitignore") {
			got = append(got, f.rel)
		}
	}
	sort.Strings(got)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("walker disagrees with git\n got: %v\nwant: %v", got, want)
	}
}
