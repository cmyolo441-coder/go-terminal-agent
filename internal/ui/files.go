package ui

import (
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// fileIndex is a cached list of workspace paths for @file completion.
type fileIndex struct {
	root  string
	paths []string // slash-separated, directories end in "/"
	built time.Time
}

const (
	fileIndexTTL   = 5 * time.Second
	fileIndexLimit = 20000
)

var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, ".venv": true, "venv": true,
	"__pycache__": true, "dist": true, "build": true, "target": true, ".next": true, ".cache": true,
}

func (ix *fileIndex) list() []string {
	if ix.paths != nil && time.Since(ix.built) < fileIndexTTL {
		return ix.paths
	}
	var out []string
	_ = filepath.WalkDir(ix.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == ix.root {
			return nil
		}
		if len(out) >= fileIndexLimit {
			return filepath.SkipAll
		}
		rel, _ := filepath.Rel(ix.root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			out = append(out, rel+"/")
			return nil
		}
		out = append(out, rel)
		return nil
	})
	ix.paths, ix.built = out, time.Now()
	return out
}

// match returns up to max paths for query, best first: paths whose base
// name starts with the query, then paths that start with it, then any
// path containing it; shorter paths first within each group.
func (ix *fileIndex) match(query string, max int) []string {
	q := strings.ToLower(query)
	type hit struct {
		p    string
		rank int
	}
	var hits []hit
	for _, p := range ix.list() {
		lp := strings.ToLower(p)
		base := strings.ToLower(filepath.Base(strings.TrimSuffix(p, "/")))
		switch {
		case q == "":
			if !strings.Contains(strings.TrimSuffix(p, "/"), "/") {
				hits = append(hits, hit{p, 0})
			}
		case strings.HasPrefix(base, q):
			hits = append(hits, hit{p, 0})
		case strings.HasPrefix(lp, q):
			hits = append(hits, hit{p, 1})
		case strings.Contains(lp, q):
			hits = append(hits, hit{p, 2})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].rank != hits[j].rank {
			return hits[i].rank < hits[j].rank
		}
		if len(hits[i].p) != len(hits[j].p) {
			return len(hits[i].p) < len(hits[j].p)
		}
		return hits[i].p < hits[j].p
	})
	out := make([]string, 0, max)
	for _, h := range hits {
		if len(out) == max {
			break
		}
		out = append(out, h.p)
	}
	return out
}

// atToken returns the @mention being typed at the end of v, without the @.
func atToken(v string) (string, bool) {
	if v == "" || strings.HasSuffix(v, " ") || strings.HasSuffix(v, "\n") {
		return "", false
	}
	i := strings.LastIndexAny(v, " \n\t")
	tok := v[i+1:]
	if !strings.HasPrefix(tok, "@") {
		return "", false
	}
	return tok[1:], true
}
