package codeintel

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/bmatcuk/doublestar/v4"
)

// SymbolQuery selects definitions by name.
type SymbolQuery struct {
	// Name is matched against symbol names: exact, prefix, substring and
	// camel-case abbreviation ("NewReg" finds NewRegistry). A qualified
	// name such as "Gate.Check" or "Foo::bar" also matches the container.
	Name string
	// Kind restricts results to one kind.
	Kind SymbolKind
	// Path restricts results to a glob or directory prefix.
	Path string
	// Limit caps results (default 50, max 500).
	Limit int
	// ExportedOnly drops unexported or private symbols.
	ExportedOnly bool
}

// SymbolMatch is a found definition.
type SymbolMatch struct {
	Symbol
	Path  string  `json:"path"`
	Score float64 `json:"score"`
}

// Symbols finds definitions matching q, best matches first.
func (ix *Index) Symbols(ctx context.Context, q SymbolQuery) ([]SymbolMatch, error) {
	name := strings.TrimSpace(q.Name)
	if name == "" && q.Kind == "" && q.Path == "" {
		return nil, fmt.Errorf("give a name, a kind or a path")
	}
	if q.Path != "" && !doublestar.ValidatePattern(normalizePathFilter(q.Path)) {
		return nil, fmt.Errorf("invalid path pattern %q", q.Path)
	}
	if q.Limit <= 0 {
		q.Limit = 50
	}
	if q.Limit > 500 {
		q.Limit = 500
	}
	s, err := ix.current(ctx)
	if err != nil {
		return nil, err
	}
	container, base := splitQualified(name)
	allow := pathFilter(q.Path)
	var out []SymbolMatch
	for _, f := range s.files {
		if allow != nil && !allow(f.Path) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		test := isTestPath(f.Path)
		for _, sym := range f.Symbols {
			if q.Kind != "" && sym.Kind != q.Kind {
				continue
			}
			if q.ExportedOnly && !sym.Exported {
				continue
			}
			score := 1.0
			if base != "" {
				score = nameScore(sym.Name, base)
				if score == 0 {
					continue
				}
				if container != "" {
					cs := nameScore(sym.Container, container)
					if cs == 0 {
						continue
					}
					score += cs / 10
				}
			}
			if sym.Exported {
				score += 3
			}
			if sym.Kind.isCallable() || sym.Kind.isTypeLike() {
				score += 2
			}
			if sym.Kind == KindField || sym.Kind == KindSection {
				score -= 3
			}
			if test {
				score -= 5
			}
			out = append(out, SymbolMatch{Symbol: sym, Path: f.Path, Score: score})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Line < out[j].Line
	})
	if len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

// splitQualified splits "A.b", "A::b" or "A#b" into container and name.
func splitQualified(q string) (string, string) {
	for _, sep := range []string{"::", "#", "."} {
		if i := strings.LastIndex(q, sep); i > 0 && i+len(sep) < len(q) {
			return q[:i], q[i+len(sep):]
		}
	}
	return "", q
}

// nameScore rates how well a symbol name matches a query; 0 is no match.
func nameScore(name, q string) float64 {
	if name == "" || q == "" {
		return 0
	}
	ln, lq := strings.ToLower(name), strings.ToLower(q)
	ratio := float64(len(q)) / float64(len(name))
	switch {
	case name == q:
		return 100
	case ln == lq:
		return 90
	case strings.HasPrefix(ln, lq):
		return 70 + 10*ratio
	case wordPrefix(name, lq):
		return 60 + 10*ratio
	case strings.Contains(ln, lq):
		return 50 + 10*ratio
	case len(lq) >= 2 && camelAbbrev(name, q):
		return 35 + 10*ratio
	case len(lq) >= 3 && subsequence(ln, lq):
		return 15 + 10*ratio
	}
	return 0
}

// wordPrefix reports whether a sub-word of name starts with q.
func wordPrefix(name, lq string) bool {
	for _, w := range SplitIdentifier(name) {
		if strings.HasPrefix(w, lq) {
			return true
		}
	}
	return false
}

// camelAbbrev matches queries made of word prefixes: "NewReg" or "nr" for
// NewRegistry, "hs" for HTTPServer.
func camelAbbrev(name, q string) bool {
	words := SplitIdentifier(name)
	parts := SplitIdentifier(q)
	if len(parts) == 1 && len(parts[0]) <= len(words) && strings.IndexFunc(q, unicode.IsUpper) < 0 {
		// All lower-case: treat each letter as a word initial.
		parts = strings.Split(parts[0], "")
	}
	wi := 0
	for _, p := range parts {
		found := false
		for wi < len(words) {
			w := words[wi]
			wi++
			if strings.HasPrefix(w, p) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func subsequence(s, sub string) bool {
	i := 0
	for j := 0; j < len(s) && i < len(sub); j++ {
		if s[j] == sub[i] {
			i++
		}
	}
	return i == len(sub)
}
