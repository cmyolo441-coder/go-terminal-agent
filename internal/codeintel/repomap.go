package codeintel

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Repo map parameters.
const (
	// maxDefiningFiles drops names defined in more files than this from
	// the reference graph: they are too generic to say who uses whom.
	maxDefiningFiles = 12
	pageRankDamping  = 0.85
	pageRankIters    = 40
	pageRankEpsilon  = 1e-9
	defaultMapBudget = 2048
	maxMapBudget     = 32768
	mapSigLimit      = 120
)

// refGraph is the file-level reference graph of a snapshot.
type refGraph struct {
	files []*fileEntry // code files only, same order as s.files subset
	out   []map[int]float64
	// symRefs[f][i] counts the files referencing symbol i of file f.
	symRefs []map[int]int
	inDeg   []int
}

// isMapLanguage reports whether files of lang belong in the repo map.
func isMapLanguage(lang string) bool { return lang != LangMarkdown && lang != LangText }

// mappable reports whether a symbol is worth showing or linking.
func mappable(s *Symbol) bool {
	return s.Kind != KindField && s.Kind != KindSection && len(s.Name) >= 2
}

func buildRefGraph(files []*fileEntry) *refGraph {
	g := &refGraph{}
	for _, f := range files {
		if isMapLanguage(f.Lang) {
			g.files = append(g.files, f)
		}
	}
	n := len(g.files)
	g.out = make([]map[int]float64, n)
	g.symRefs = make([]map[int]int, n)
	g.inDeg = make([]int, n)
	type def = struct{ file, sym int }
	defs := map[string][]def{}
	for fi, f := range g.files {
		for si := range f.Symbols {
			s := &f.Symbols[si]
			if !mappable(s) || len(s.Name) < 3 {
				continue
			}
			defs[s.Name] = append(defs[s.Name], def{fi, si})
		}
	}
	for name, ds := range defs {
		files := map[int]bool{}
		for _, d := range ds {
			files[d.file] = true
		}
		if len(files) > maxDefiningFiles {
			delete(defs, name)
		}
	}
	for fi, f := range g.files {
		for _, ref := range f.Refs {
			ds := defs[ref]
			if len(ds) == 0 {
				continue
			}
			targets := map[int]bool{}
			for _, d := range ds {
				if d.file != fi {
					targets[d.file] = true
				}
			}
			if len(targets) == 0 {
				continue
			}
			w := refWeight(ref, ds, g.files) / float64(len(targets))
			for _, d := range ds {
				if d.file == fi {
					continue
				}
				if g.symRefs[d.file] == nil {
					g.symRefs[d.file] = map[int]int{}
				}
				g.symRefs[d.file][d.sym]++
			}
			for t := range targets {
				if g.out[fi] == nil {
					g.out[fi] = map[int]float64{}
				}
				if g.out[fi][t] == 0 {
					g.inDeg[t]++
				}
				g.out[fi][t] += w
			}
		}
	}
	return g
}

// refWeight rates how much a reference to name says about a dependency:
// specific compound names are strong evidence, short generic names and
// method names shared by many types are weak.
func refWeight(name string, ds []struct{ file, sym int }, files []*fileEntry) float64 {
	w := 1.0
	if len(SplitIdentifier(name)) >= 2 && len(name) >= 8 {
		w *= 10
	}
	if strings.HasPrefix(name, "_") {
		w *= 0.1
	}
	if len(ds) > 5 {
		w *= 0.1
	}
	methods := true
	for _, d := range ds {
		if files[d.file].Symbols[d.sym].Kind != KindMethod {
			methods = false
			break
		}
	}
	if methods {
		w *= 0.3
	}
	return w
}

// pageRank computes personalised PageRank over the graph.
func (g *refGraph) pageRank(personal []float64) []float64 {
	n := len(g.files)
	if n == 0 {
		return nil
	}
	p := personal
	if p == nil {
		p = make([]float64, n)
		for i := range p {
			p[i] = 1 / float64(n)
		}
	}
	outSum := make([]float64, n)
	for i, m := range g.out {
		for _, w := range m {
			outSum[i] += w
		}
	}
	rank := append([]float64(nil), p...)
	next := make([]float64, n)
	for iter := 0; iter < pageRankIters; iter++ {
		dangling := 0.0
		for i := range next {
			next[i] = 0
			if outSum[i] == 0 {
				dangling += rank[i]
			}
		}
		for i, m := range g.out {
			if outSum[i] == 0 {
				continue
			}
			for j, w := range m {
				next[j] += pageRankDamping * rank[i] * w / outSum[i]
			}
		}
		delta := 0.0
		for i := range next {
			next[i] += (1-pageRankDamping)*p[i] + pageRankDamping*dangling*p[i]
			delta += math.Abs(next[i] - rank[i])
		}
		rank, next = next, rank
		if delta < pageRankEpsilon {
			break
		}
	}
	return rank
}

func (s *snapshot) refGraph() *refGraph {
	s.graphOnce.Do(func() { s.graph = buildRefGraph(s.files) })
	return s.graph
}

// RepoMapOptions configure RepoMap.
type RepoMapOptions struct {
	// Budget is the approximate size of the map in tokens (default 2048).
	Budget int
	// Focus lists files, directories or globs of current interest; they
	// and the files they reference rank higher.
	Focus []string
	// MaxSymbolsPerFile caps the symbols listed per file (default 12).
	MaxSymbolsPerFile int
}

// estimateTokens approximates the token count of text (about four bytes
// per token for code).
func estimateTokens(s string) int { return (len(s) + 3) / 4 }

// RepoMap renders a compact outline of the most important files and their
// key definitions that fits within the token budget. Files are ranked by
// PageRank over the cross-file reference graph, so central code comes
// first.
func (ix *Index) RepoMap(ctx context.Context, opts RepoMapOptions) (string, error) {
	if opts.Budget <= 0 {
		opts.Budget = defaultMapBudget
	}
	if opts.Budget > maxMapBudget {
		opts.Budget = maxMapBudget
	}
	if opts.MaxSymbolsPerFile <= 0 {
		opts.MaxSymbolsPerFile = 12
	}
	s, err := ix.current(ctx)
	if err != nil {
		return "", err
	}
	g := s.refGraph()
	if len(g.files) == 0 {
		return "(no source files indexed)\n", nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	focused := make([]bool, len(g.files))
	var personal []float64
	if len(opts.Focus) > 0 {
		var preds []func(string) bool
		for _, f := range opts.Focus {
			if rel, ok := ix.relPath(f); ok {
				f = rel
			}
			if p := pathFilter(f); p != nil {
				preds = append(preds, p)
			}
		}
		nf := 0
		for i, f := range g.files {
			for _, p := range preds {
				if p(f.Path) {
					focused[i] = true
					nf++
					break
				}
			}
		}
		if nf > 0 {
			personal = make([]float64, len(g.files))
			for i := range personal {
				personal[i] = 0.2 / float64(len(g.files))
				if focused[i] {
					personal[i] += 0.8 / float64(nf)
				}
			}
		}
	}
	rank := g.pageRank(personal)
	type ranked struct {
		i     int
		score float64
	}
	order := make([]ranked, len(g.files))
	for i, f := range g.files {
		sc := rank[i] * (1 + 0.1*math.Log1p(float64(g.inDeg[i])))
		if isTestPath(f.Path) {
			sc *= 0.3
		}
		if len(f.Symbols) == 0 {
			sc *= 0.5
		}
		if focused[i] {
			sc += 10
		}
		order[i] = ranked{i, sc}
	}
	sort.Slice(order, func(a, b int) bool {
		if order[a].score != order[b].score {
			return order[a].score > order[b].score
		}
		return g.files[order[a].i].Path < g.files[order[b].i].Path
	})

	var b strings.Builder
	used := 0
	shown := 0
	trailerReserve := estimateTokens("… 100000 more files not shown\n")
	for k, r := range order {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		remaining := opts.Budget - used - trailerReserve
		if remaining <= 0 {
			break
		}
		f := g.files[r.i]
		var block string
		for _, max := range []int{opts.MaxSymbolsPerFile, 4, 0} {
			block = renderFileBlock(f, g.symRefs[r.i], max)
			if estimateTokens(block) <= remaining {
				break
			}
			block = ""
		}
		if block == "" {
			// Even the header does not fit; smaller files further down
			// might, but order matters more than packing.
			if k > 0 {
				break
			}
			continue
		}
		b.WriteString(block)
		used += estimateTokens(block)
		shown++
	}
	if rest := len(g.files) - shown; rest > 0 {
		fmt.Fprintf(&b, "… %d more files not shown\n", rest)
	}
	return b.String(), nil
}

// renderFileBlock renders one file with up to max key symbols.
func renderFileBlock(f *fileEntry, refs map[int]int, max int) string {
	var b strings.Builder
	b.WriteString(f.Path)
	b.WriteString("\n")
	if max <= 0 {
		return b.String()
	}
	type cand struct {
		i     int
		score float64
	}
	var cands []cand
	for i := range f.Symbols {
		s := &f.Symbols[i]
		if !mappable(s) {
			continue
		}
		sc := float64(refs[i]) * 2
		if s.Exported {
			sc += 1
		}
		if s.Kind.isTypeLike() {
			sc += 0.75
		}
		if s.Kind.isCallable() {
			sc += 0.5
		}
		if s.Kind == KindVar {
			sc -= 0.5
		}
		cands = append(cands, cand{i, sc})
	}
	sort.SliceStable(cands, func(a, c int) bool { return cands[a].score > cands[c].score })
	more := 0
	if len(cands) > max {
		more = len(cands) - max
		cands = cands[:max]
	}
	// A listed member needs its container for context.
	chosen := map[int]bool{}
	for _, c := range cands {
		chosen[c.i] = true
	}
	for _, c := range cands {
		s := &f.Symbols[c.i]
		if s.Container == "" {
			continue
		}
		for j := range f.Symbols {
			o := &f.Symbols[j]
			if !chosen[j] && o.Name == s.Container && (o.Kind.isTypeLike() || o.Kind == KindModule) && o.Line <= s.Line && o.EndLine >= s.EndLine {
				chosen[j] = true
				cands = append(cands, cand{j, 0})
				break
			}
		}
	}
	sort.Slice(cands, func(a, c int) bool { return cands[a].i < cands[c].i })
	for _, c := range cands {
		s := &f.Symbols[c.i]
		indent := "  "
		if s.Container != "" && !strings.HasPrefix(s.Signature, "func ") {
			indent = "    "
		}
		sig := s.Signature
		if sig == "" {
			sig = string(s.Kind) + " " + s.Name
		}
		b.WriteString(indent)
		b.WriteString(clip(sig, mapSigLimit))
		b.WriteString("\n")
	}
	if more > 0 {
		fmt.Fprintf(&b, "  … %d more\n", more)
	}
	return b.String()
}

// RankedFiles returns the indexed source files ordered by importance, for
// callers that want to build their own overview.
func (ix *Index) RankedFiles(ctx context.Context, limit int) ([]string, error) {
	s, err := ix.current(ctx)
	if err != nil {
		return nil, err
	}
	g := s.refGraph()
	rank := g.pageRank(nil)
	idx := make([]int, len(g.files))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		if rank[idx[a]] != rank[idx[b]] {
			return rank[idx[a]] > rank[idx[b]]
		}
		return g.files[idx[a]].Path < g.files[idx[b]].Path
	})
	if limit > 0 && len(idx) > limit {
		idx = idx[:limit]
	}
	out := make([]string, len(idx))
	for i, k := range idx {
		out[i] = g.files[k].Path
	}
	return out, nil
}
