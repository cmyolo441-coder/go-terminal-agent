package codeintel

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
)

// Options.Exclude keeps files (for example those a managed policy denies)
// out of the index, the search tools and the repository map.
func TestExcludeKeepsPathsOutOfTheIndex(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"main.go":           "package main\n\nfunc VisibleFunc() {}\n",
		"creds.go":          "package main\n\nfunc SecretCreds() {}\n",
		"private/keys.go":   "package private\n\nfunc PrivateKeys() {}\n",
		"private/sub/x.go":  "package sub\n\nfunc DeepPrivate() {}\n",
		"public/private.go": "package public\n\nfunc NotPrivate() {}\n",
	})
	var dirAsks atomic.Int32
	exclude := func(rel string, dir bool) bool {
		if dir && strings.HasPrefix(rel, "private") {
			dirAsks.Add(1)
		}
		return rel == "creds.go" || rel == "private" || strings.HasPrefix(rel, "private/")
	}
	ix := openIndex(t, root, func(o *Options) { o.Exclude = exclude })
	st := mustRefresh(t, ix)
	if st.Files != 2 || st.Skipped[SkipExcluded] != 2 {
		t.Fatalf("files %d, excluded %d; want 2 and 2 (a file and a directory)", st.Files, st.Skipped[SkipExcluded])
	}
	ctx := context.Background()
	for _, name := range []string{"SecretCreds", "PrivateKeys", "DeepPrivate"} {
		if ms, _ := ix.Symbols(ctx, SymbolQuery{Name: name}); len(ms) != 0 {
			t.Errorf("excluded symbol %s found: %v", name, ms)
		}
		hits, _ := ix.Search(ctx, name, SearchOptions{})
		for _, h := range hits {
			if exclude(h.Path, false) {
				t.Errorf("search for %s returned excluded %s", name, h.Path)
			}
		}
	}
	if ms, _ := ix.Symbols(ctx, SymbolQuery{Name: "NotPrivate"}); len(ms) != 1 {
		t.Error("a visible file was excluded")
	}
	m, err := ix.RepoMap(ctx, RepoMapOptions{Budget: 2000})
	if err != nil || strings.Contains(m, "creds.go") || strings.Contains(m, "keys.go") || !strings.Contains(m, "main.go") {
		t.Fatalf("repo map = %q, %v", m, err)
	}
	// An excluded directory is pruned, not walked.
	if dirAsks.Load() != 1 {
		t.Errorf("asked about private directories %d times, want 1", dirAsks.Load())
	}
	// Invalidate goes through the same rule.
	writeTree(t, root, map[string]string{"private/new.go": "package private\n\nfunc NewSecret() {}\n"})
	ix.Invalidate("private/new.go", "creds.go")
	if ms, _ := ix.Symbols(ctx, SymbolQuery{Name: "NewSecret"}); len(ms) != 0 {
		t.Fatal("excluded file indexed through Invalidate")
	}
}

// A cache written before Exclude applied does not serve excluded files.
func TestExcludeFiltersTheCache(t *testing.T) {
	root, cache := t.TempDir(), t.TempDir()
	writeTree(t, root, map[string]string{"a.go": "package p\n\nfunc Alpha() {}\n", "creds.go": "package p\n\nfunc SecretCreds() {}\n"})
	ix := openIndex(t, root, func(o *Options) { o.CacheDir = cache })
	mustRefresh(t, ix)
	ix.Close()
	ix = openIndex(t, root, func(o *Options) {
		o.CacheDir = cache
		o.Exclude = func(rel string, _ bool) bool { return rel == "creds.go" }
	})
	if st := ix.Stats(); !st.FromCache || st.Files != 1 {
		t.Fatalf("cached stats = %+v, want 1 file from cache", st)
	}
}
