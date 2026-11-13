package codeintel

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// BM25 parameters.
const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

// Expansion weights relative to an exact term match.
const (
	expandSynonym   = 0.45
	expandPrefix    = 0.7
	expandStem      = 0.6
	maxPrefixExpand = 12
	pathTermWeight  = 0.6
)

type posting struct {
	chunk uint32
	tf    float32
}

// searchIndex is the inverted index of one snapshot.
type searchIndex struct {
	files     []*fileEntry
	chunkFile []uint32
	chunkIdx  []uint32
	chunkLen  []float32
	avgLen    float64
	postings  map[string][]posting
	vocab     []string // sorted, for prefix expansion
	pathTerms map[string][]uint32
}

func buildSearchIndex(files []*fileEntry) *searchIndex {
	si := &searchIndex{files: files, postings: map[string][]posting{}, pathTerms: map[string][]uint32{}}
	total := 0.0
	for fi, f := range files {
		for _, t := range f.PathTerms {
			si.pathTerms[t] = append(si.pathTerms[t], uint32(fi))
		}
		for ci := range f.Chunks {
			c := &f.Chunks[ci]
			id := uint32(len(si.chunkFile))
			si.chunkFile = append(si.chunkFile, uint32(fi))
			si.chunkIdx = append(si.chunkIdx, uint32(ci))
			si.chunkLen = append(si.chunkLen, c.Length)
			total += float64(c.Length)
			for k, t := range c.Terms {
				si.postings[t] = append(si.postings[t], posting{chunk: id, tf: c.Freqs[k]})
			}
		}
	}
	if n := len(si.chunkFile); n > 0 {
		si.avgLen = total / float64(n)
	}
	if si.avgLen <= 0 {
		si.avgLen = 1
	}
	si.vocab = make([]string, 0, len(si.postings))
	for t := range si.postings {
		si.vocab = append(si.vocab, t)
	}
	sort.Strings(si.vocab)
	return si
}

func (si *searchIndex) idf(df int) float64 {
	n := float64(len(si.chunkFile))
	return math.Log(1 + (n-float64(df)+0.5)/(float64(df)+0.5))
}

// SearchOptions refine a search.
type SearchOptions struct {
	// Path restricts results to files matching a glob ("internal/**/*.go")
	// or lying under a directory prefix ("internal/agent").
	Path string
	// Limit is the number of hits (default 10, max 100).
	Limit int
	// MaxPerFile caps hits from one file so one file cannot crowd out the
	// rest (default 3).
	MaxPerFile int
	// SnippetLines is the snippet size (default 6).
	SnippetLines int
}

// SnippetLine is one line of a hit's snippet.
type SnippetLine struct {
	Line  int    `json:"line"`
	Text  string `json:"text"`
	Match bool   `json:"match,omitempty"`
	// Gap is true for a marker line standing for skipped lines.
	Gap bool `json:"gap,omitempty"`
}

// Hit is one ranked search result.
type Hit struct {
	Path      string        `json:"path"`
	StartLine int           `json:"start_line"`
	EndLine   int           `json:"end_line"`
	Score     float64       `json:"score"`
	Symbol    *Symbol       `json:"symbol,omitempty"`
	Matched   []string      `json:"matched,omitempty"`
	Snippet   []SnippetLine `json:"snippet,omitempty"`
}

// queryTerm is one analysed query word with its expansions.
type queryTerm struct {
	word      string
	expansion map[string]float64 // index term -> weight
}

// parsedQuery is an analysed query.
type parsedQuery struct {
	terms  []queryTerm
	idents map[string]bool // lower-cased identifiers as typed
	tests  bool            // the query is about tests
	docs   bool            // the query is about documentation
	// lineTerms are all expansion terms with enough weight to mark a
	// snippet line as matching.
	lineTerms map[string]bool
}

// parseQuery analyses a natural-language or identifier query.
func (si *searchIndex) parseQuery(q string) parsedQuery {
	pq := parsedQuery{idents: map[string]bool{}, lineTerms: map[string]bool{}}
	var words []string
	seen := map[string]bool{}
	addWord := func(w string, filler bool) {
		lw := strings.ToLower(w)
		if filler && queryFiller[lw] {
			return
		}
		t, ok := analyzer{keepKeywords: true}.normalize(lw)
		if !ok || seen[t] {
			return
		}
		seen[t] = true
		words = append(words, t)
	}
	collect := func(filler bool) {
		identifiers(q, func(id string) {
			parts := SplitIdentifier(id)
			for _, p := range parts {
				addWord(p, filler)
			}
			if len(parts) > 1 {
				whole := strings.ToLower(strings.Trim(id, "_"))
				if len(whole) <= maxTokenLen && !seen[whole] {
					seen[whole] = true
					words = append(words, whole)
				}
			}
		})
	}
	collect(true)
	if len(words) == 0 {
		// Everything was filler ("where is it done?"): keep it anyway.
		collect(false)
	}
	identifiers(q, func(id string) {
		if len(id) >= 2 {
			pq.idents[strings.ToLower(id)] = true
		}
	})
	if i := strings.IndexAny(q, ".:"); i >= 0 {
		// Qualified names like Gate.Check or Foo::bar.
		for _, f := range strings.Fields(q) {
			if strings.ContainsAny(f, ".:") {
				pq.idents[strings.ToLower(strings.Trim(f, ".:()"))] = true
			}
		}
	}
	// Adjacent words that the code writes as one identifier: "log in" ->
	// login, "set up" -> setup, "file name" -> filename.
	var raw []string
	identifiers(q, func(id string) { raw = append(raw, SplitIdentifier(id)...) })
	joined := map[string]map[string]float64{} // first word's term -> joined terms
	for i := 0; i+1 < len(raw); i++ {
		pair := raw[i] + raw[i+1]
		if len(pair) < 5 {
			continue
		}
		found := map[string]float64{}
		for _, cand := range []string{pair, Stem(pair)} {
			if _, ok := si.postings[cand]; ok {
				found[cand] = 1
			}
		}
		for _, syn := range synonyms[Stem(pair)] {
			if _, ok := si.postings[syn]; ok && found[syn] == 0 {
				found[syn] = 0.8
			}
		}
		if len(found) == 0 {
			continue
		}
		first, ok := analyzer{keepKeywords: true}.normalize(raw[i])
		if !ok || queryFiller[raw[i]] {
			first = pair
		}
		if joined[first] == nil {
			joined[first] = map[string]float64{}
		}
		for t, w := range found {
			joined[first][t] = w
		}
		if !seen[first] {
			seen[first] = true
			words = append(words, first)
		}
	}
	for _, w := range words {
		switch w {
		case "test", "tests", "spec", "assert":
			pq.tests = true
		case "doc", "docs", "readme", "documentation", "guide":
			pq.docs = true
		}
		qt := queryTerm{word: w, expansion: si.expand(w)}
		for t, wt := range joined[w] {
			if wt > qt.expansion[t] {
				qt.expansion[t] = wt
			}
		}
		pq.terms = append(pq.terms, qt)
		for t, wt := range qt.expansion {
			if wt >= expandSynonym {
				pq.lineTerms[t] = true
			}
		}
		pq.lineTerms[w] = true
	}
	return pq
}

// expand maps a query term to index terms with weights: the term itself,
// its synonyms, longer terms it is a prefix of and shorter stems of it.
func (si *searchIndex) expand(w string) map[string]float64 {
	out := map[string]float64{}
	put := func(t string, wt float64) {
		if _, ok := si.postings[t]; !ok {
			return
		}
		if wt > out[t] {
			out[t] = wt
		}
	}
	put(w, 1)
	for _, s := range synonyms[w] {
		put(s, expandSynonym)
	}
	if len(w) >= 4 {
		i := sort.SearchStrings(si.vocab, w)
		type cand struct {
			t  string
			df int
		}
		var cands []cand
		for ; i < len(si.vocab) && strings.HasPrefix(si.vocab[i], w); i++ {
			t := si.vocab[i]
			if t != w && len(t) <= 2*len(w)+4 {
				cands = append(cands, cand{t, len(si.postings[t])})
			}
		}
		sort.Slice(cands, func(a, b int) bool { return cands[a].df > cands[b].df })
		if len(cands) > maxPrefixExpand {
			cands = cands[:maxPrefixExpand]
		}
		for _, c := range cands {
			put(c.t, expandPrefix*float64(len(w))/float64(len(c.t)))
		}
		for n := len(w) - 1; n >= 4 && float64(n) >= 0.6*float64(len(w)); n-- {
			put(w[:n], expandStem*float64(n)/float64(len(w)))
		}
	}
	return out
}

// Search ranks chunks of code for a query, natural language or
// identifiers, and returns the best hits with snippets.
func (ix *Index) Search(ctx context.Context, query string, opts SearchOptions) ([]Hit, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("empty query")
	}
	if opts.Path != "" && !doublestar.ValidatePattern(normalizePathFilter(opts.Path)) {
		return nil, fmt.Errorf("invalid path pattern %q", opts.Path)
	}
	s, err := ix.current(ctx)
	if err != nil {
		return nil, err
	}
	hits := s.search.search(ctx, query, opts)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ix.attachSnippets(hits, s, query, opts)
	return hits, nil
}

func normalizePathFilter(p string) string {
	p = strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(p)), "./")
	return strings.TrimSuffix(p, "/")
}

// pathFilter returns a predicate for the Path option.
func pathFilter(p string) func(string) bool {
	p = normalizePathFilter(p)
	if p == "" || p == "." || p == "**" {
		return nil
	}
	hasMeta := strings.ContainsAny(p, "*?[{")
	return func(rel string) bool {
		if hasMeta {
			if ok, _ := doublestar.Match(p, rel); ok {
				return true
			}
			if !strings.Contains(p, "/") {
				ok, _ := doublestar.Match(p, rel[strings.LastIndexByte(rel, '/')+1:])
				return ok
			}
			return false
		}
		return rel == p || strings.HasPrefix(rel, p+"/")
	}
}

func isTestPath(p string) bool {
	base := p[strings.LastIndexByte(p, '/')+1:]
	lower := strings.ToLower(base)
	if strings.Contains(lower, "_test.") || strings.HasPrefix(lower, "test_") || strings.Contains(lower, ".test.") ||
		strings.Contains(lower, ".spec.") || strings.HasSuffix(strings.TrimSuffix(lower, filepath.Ext(lower)), "test") ||
		strings.HasSuffix(strings.TrimSuffix(lower, filepath.Ext(lower)), "tests") {
		return true
	}
	for _, d := range []string{"test/", "tests/", "__tests__/", "spec/", "testdata/", "fixtures/"} {
		if strings.HasPrefix(p, d) || strings.Contains(p, "/"+d) {
			return true
		}
	}
	return false
}

// search scores chunks and returns hits without snippets.
func (si *searchIndex) search(ctx context.Context, query string, opts SearchOptions) []Hit {
	if opts.Limit <= 0 {
		opts.Limit = 10
	}
	if opts.Limit > 100 {
		opts.Limit = 100
	}
	if opts.MaxPerFile <= 0 {
		opts.MaxPerFile = 3
	}
	pq := si.parseQuery(query)
	if len(pq.terms) == 0 || len(si.chunkFile) == 0 {
		return nil
	}
	allow := pathFilter(opts.Path)
	var fileOK []bool
	if allow != nil {
		fileOK = make([]bool, len(si.files))
		for i, f := range si.files {
			fileOK[i] = allow(f.Path)
		}
	}
	nq := len(pq.terms)
	if nq > 64 {
		nq = 64
	}
	scores := map[uint32]float64{}
	masks := map[uint32]uint64{}
	for qi := 0; qi < nq; qi++ {
		if ctx.Err() != nil {
			return nil
		}
		best := map[uint32]float64{}
		for t, w := range pq.terms[qi].expansion {
			ps := si.postings[t]
			idf := si.idf(len(ps))
			for _, p := range ps {
				if fileOK != nil && !fileOK[si.chunkFile[p.chunk]] {
					continue
				}
				tf := float64(p.tf)
				norm := tf * (bm25K1 + 1) / (tf + bm25K1*(1-bm25B+bm25B*float64(si.chunkLen[p.chunk])/si.avgLen))
				if v := w * idf * norm; v > best[p.chunk] {
					best[p.chunk] = v
				}
			}
		}
		for c, v := range best {
			scores[c] += v
			masks[c] |= 1 << uint(qi)
		}
	}
	// Path matches: a query word naming the file's path counts toward
	// coverage and adds a field score.
	pathMask := map[uint32]uint64{}
	pathScore := map[uint32]float64{}
	nf := float64(len(si.files))
	for qi := 0; qi < nq; qi++ {
		for t, w := range pq.terms[qi].expansion {
			if w < expandPrefix*0.8 {
				continue
			}
			fs := si.pathTerms[t]
			idf := math.Log(1 + (nf-float64(len(fs))+0.5)/(float64(len(fs))+0.5))
			for _, fi := range fs {
				if pathMask[fi]&(1<<uint(qi)) == 0 {
					pathMask[fi] |= 1 << uint(qi)
					pathScore[fi] += pathTermWeight * w * idf
				}
			}
		}
	}
	type scored struct {
		chunk uint32
		score float64
		mask  uint64
	}
	results := make([]scored, 0, len(scores))
	for c, sc := range scores {
		fi := si.chunkFile[c]
		f := si.files[fi]
		ch := &f.Chunks[si.chunkIdx[c]]
		mask := masks[c] | pathMask[fi]
		sc += pathScore[fi]
		cov := float64(popcount(mask)) / float64(nq)
		sc *= 0.3 + 0.7*cov*cov
		if sym := f.symbol(ch.Symbol); sym != nil {
			sc *= si.symbolBoost(sym, pq, nq)
		}
		if !pq.tests && isTestPath(f.Path) {
			sc *= 0.7
		}
		if !pq.docs && (f.Lang == LangMarkdown || f.Lang == LangText) {
			sc *= 0.8
		}
		results = append(results, scored{c, sc, mask})
	}
	sort.Slice(results, func(a, b int) bool {
		if results[a].score != results[b].score {
			return results[a].score > results[b].score
		}
		fa, fb := si.files[si.chunkFile[results[a].chunk]], si.files[si.chunkFile[results[b].chunk]]
		if fa.Path != fb.Path {
			return fa.Path < fb.Path
		}
		return results[a].chunk < results[b].chunk
	})
	perFile := map[uint32]int{}
	var hits []Hit
	for _, r := range results {
		if len(hits) >= opts.Limit {
			break
		}
		fi := si.chunkFile[r.chunk]
		if perFile[fi] >= opts.MaxPerFile {
			continue
		}
		perFile[fi]++
		f := si.files[fi]
		ch := f.Chunks[si.chunkIdx[r.chunk]]
		h := Hit{Path: f.Path, StartLine: ch.Start, EndLine: ch.End, Score: math.Round(r.score*1000) / 1000}
		if sym := f.symbol(ch.Symbol); sym != nil {
			cp := *sym
			h.Symbol = &cp
		}
		for qi := 0; qi < nq; qi++ {
			if r.mask&(1<<uint(qi)) != 0 {
				h.Matched = append(h.Matched, pq.terms[qi].word)
			}
		}
		hits = append(hits, h)
	}
	return hits
}

func popcount(x uint64) int {
	n := 0
	for x != 0 {
		x &= x - 1
		n++
	}
	return n
}

// symbolBoost rewards chunks whose own symbol name matches the query.
func (si *searchIndex) symbolBoost(sym *Symbol, pq parsedQuery, nq int) float64 {
	boost := 1.0
	lname := strings.ToLower(sym.Name)
	if pq.idents[lname] || pq.idents[strings.ToLower(sym.QualifiedName())] {
		boost *= 2.5
	}
	var nameTerms []string
	analyzer{keepKeywords: true}.terms(sym.Name, func(t string) { nameTerms = append(nameTerms, t) })
	if len(nameTerms) == 0 {
		return boost
	}
	hitName, hitQuery := 0, 0
	qhit := make([]bool, nq)
	for _, nt := range nameTerms {
		matched := false
		for qi := 0; qi < nq; qi++ {
			if w, ok := pq.terms[qi].expansion[nt]; ok && w >= expandSynonym {
				matched = true
				qhit[qi] = true
			}
		}
		if matched {
			hitName++
		}
	}
	for _, h := range qhit {
		if h {
			hitQuery++
		}
	}
	frac := float64(hitName) / float64(len(nameTerms))
	qcov := float64(hitQuery) / float64(nq)
	boost *= 1 + 1.2*frac*qcov
	if sym.Kind.isCallable() || sym.Kind.isTypeLike() {
		boost *= 1.1
	}
	return boost
}

// attachSnippets reads the hit files and picks the most relevant lines.
func (ix *Index) attachSnippets(hits []Hit, s *snapshot, query string, opts SearchOptions) {
	if len(hits) == 0 {
		return
	}
	n := opts.SnippetLines
	if n <= 0 {
		n = 6
	}
	if n > 40 {
		n = 40
	}
	pq := s.search.parseQuery(query)
	cache := map[string][]string{}
	for i := range hits {
		h := &hits[i]
		lines, ok := cache[h.Path]
		if !ok {
			data, err := readCapped(filepath.Join(ix.root, filepath.FromSlash(h.Path)), ix.opts.MaxFileSize)
			if err == nil {
				lines = strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
			}
			cache[h.Path] = lines
		}
		h.Snippet = pickSnippet(lines, h, pq.lineTerms, n)
	}
}

// pickSnippet chooses the window of n lines within a hit with the most
// matching lines, prefixed with the definition line when it is not inside.
func pickSnippet(lines []string, h *Hit, terms map[string]bool, n int) []SnippetLine {
	if len(lines) == 0 {
		return nil
	}
	start, end := h.StartLine, h.EndLine
	if end > len(lines) {
		end = len(lines)
	}
	if start > end {
		return nil
	}
	match := make([]bool, end-start+1)
	for l := start; l <= end; l++ {
		hit := false
		analyzer{keepKeywords: true}.terms(lines[l-1], func(t string) {
			if terms[t] {
				hit = true
			}
		})
		match[l-start] = hit
	}
	size := n
	if size > len(match) {
		size = len(match)
	}
	bestAt, bestScore, cur := 0, -1, 0
	for i := 0; i < len(match); i++ {
		if match[i] {
			cur++
		}
		if i >= size && match[i-size] {
			cur--
		}
		if i >= size-1 && cur > bestScore {
			bestScore = cur
			bestAt = i - size + 1
		}
	}
	from := start + bestAt
	var out []SnippetLine
	if h.Symbol != nil && h.Symbol.Line >= start && h.Symbol.Line < from && h.Symbol.Line <= len(lines) {
		// Keep the window at n lines including the definition line.
		out = append(out, SnippetLine{Line: h.Symbol.Line, Text: snippetText(lines[h.Symbol.Line-1]), Match: match[h.Symbol.Line-start]})
		if h.Symbol.Line+1 < from {
			out = append(out, SnippetLine{Gap: true})
		}
		if size > 1 {
			from++
			size--
		}
	}
	for l := from; l < from+size && l <= end; l++ {
		out = append(out, SnippetLine{Line: l, Text: snippetText(lines[l-1]), Match: match[l-start]})
	}
	return out
}

func snippetText(s string) string {
	s = strings.ReplaceAll(strings.TrimRight(s, " \t\r"), "\t", "    ")
	return clip(s, 220)
}
