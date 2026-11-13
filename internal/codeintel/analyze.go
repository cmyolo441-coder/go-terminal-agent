package codeintel

import (
	"bytes"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

// Chunking parameters. Symbols become their own chunks so a hit points at
// a definition; long bodies and unowned regions are split into windows.
const (
	maxChunkLines   = 80
	unownedChunk    = 40
	maxRefsPerFile  = 4000
	maxSymbolsFile  = 5000
	binarySniffSize = 8000
)

// Term weights for the fields of a chunk (BM25F-style).
const (
	weightCode      = 1.0
	weightDoc       = 1.0 // added on top of the code weight of doc lines
	weightName      = 6.0
	weightNameCont  = 1.5 // symbol name on continuation chunks of long bodies
	weightContainer = 2.0
	weightInnerName = 2.5 // names of members defined inside the chunk
)

// fileEntry is everything the index keeps about one file. It is immutable
// once built and is shared between snapshots.
type fileEntry struct {
	Path    string // slash-separated, relative to the root
	Size    int64
	ModTime int64 // UnixNano
	Lang    string
	Lines   int
	// Skip is non-empty when the file is known but not indexed (binary or
	// generated content); keeping it avoids re-reading unchanged files.
	Skip string
	// Racy marks a file modified so close to the scan that a later change
	// could keep the same size and mtime; it is re-read on the next refresh.
	Racy      bool
	Symbols   []Symbol
	Chunks    []chunk
	Refs      []string // distinct identifiers used in the file
	PathTerms []string
}

// chunk is a searchable region of a file.
type chunk struct {
	Start, End int // 1-based, inclusive
	Symbol     int // index into Symbols, -1 for none
	Terms      []string
	Freqs      []float32
	Length     float32
}

func (e *fileEntry) symbol(i int) *Symbol {
	if i < 0 || i >= len(e.Symbols) {
		return nil
	}
	return &e.Symbols[i]
}

// interner deduplicates term strings across files to save memory.
type interner struct {
	mu sync.Mutex
	m  map[string]string
}

func newInterner() *interner { return &interner{m: map[string]string{}} }

func (in *interner) internAll(ss []string) {
	in.mu.Lock()
	defer in.mu.Unlock()
	for i, s := range ss {
		if v, ok := in.m[s]; ok {
			ss[i] = v
		} else {
			in.m[s] = s
		}
	}
}

// intern returns the canonical copy of s.
func (in *interner) intern(s string) string {
	in.mu.Lock()
	defer in.mu.Unlock()
	if v, ok := in.m[s]; ok {
		return v
	}
	in.m[s] = s
	return s
}

func (in *interner) internEntry(e *fileEntry) {
	for i := range e.Chunks {
		in.internAll(e.Chunks[i].Terms)
	}
	in.internAll(e.PathTerms)
}

// sniffContent classifies raw file content; an empty result means the file
// should be indexed.
func sniffContent(data []byte, includeGenerated bool) string {
	head := data
	if len(head) > binarySniffSize {
		head = head[:binarySniffSize]
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return SkipBinary
	}
	if !utf8.Valid(head) {
		// Tolerate legacy encodings and a rune cut at the boundary, but
		// not data that is mostly invalid.
		bad, total := 0, len(head)
		for len(head) > 0 {
			r, size := utf8.DecodeRune(head)
			if r == utf8.RuneError && size == 1 {
				bad++
			}
			head = head[size:]
		}
		if bad*10 > total {
			return SkipBinary
		}
	}
	if !includeGenerated && isGeneratedContent(data) {
		return SkipGenerated
	}
	return ""
}

// analyzeFile builds the index entry for one text file. A panic in an
// extractor is contained: the file is still searchable as plain text.
func analyzeFile(rel string, data []byte) (e *fileEntry) {
	lang := DetectLanguage(rel)
	src := string(data)
	e = &fileEntry{Path: rel, Lang: lang}
	var syms []Symbol
	func() {
		defer func() {
			if r := recover(); r != nil {
				syms = nil
			}
		}()
		syms = ExtractSymbols(lang, src)
	}()
	if len(syms) > maxSymbolsFile {
		syms = syms[:maxSymbolsFile]
	}
	e.Symbols = syms
	lines := strings.Split(src, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	e.Lines = len(lines)
	e.Chunks = buildChunks(lines, syms)
	e.Refs = collectRefs(src)
	e.PathTerms = pathTerms(rel)
	return e
}

// pathTerms are the terms of a file path without its extension.
func pathTerms(rel string) []string {
	p := strings.TrimSuffix(rel, path.Ext(rel))
	seen := map[string]bool{}
	var out []string
	analyzer{keepKeywords: true}.terms(p, func(t string) {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	})
	sort.Strings(out)
	return out
}

// collectRefs lists the distinct identifiers of at least three characters
// used in src, for the reference graph. The result shares one string
// allocation and does not keep src alive.
func collectRefs(src string) []string {
	seen := map[string]bool{}
	var out []string
	var tb termBuf
	size := 0
	identifiers(src, func(w string) {
		if len(w) < 3 || len(w) > 80 || seen[w] || len(out) >= maxRefsPerFile {
			return
		}
		// Lower-casing never shrinks a rune by more than 4 bytes to 1.
		if len(w) <= 4*maxKeywordLen && codeKeywords[lowerInto(&tb, w, "", "")] {
			return
		}
		seen[w] = true
		out = append(out, w)
		size += len(w)
	})
	sort.Strings(out)
	var b strings.Builder
	b.Grow(size)
	for _, w := range out {
		b.WriteString(w)
	}
	all := b.String()
	off := 0
	for i, w := range out {
		out[i] = all[off : off+len(w)]
		off += len(w)
	}
	return out
}

// owningSymbols reports, per symbol, whether it claims its lines as its
// own chunk. Fields and one-line members inside a larger definition stay
// part of it.
func owningSymbols(syms []Symbol, lines int) []bool {
	// covered[l] counts multi-line symbols spanning line l (1-based).
	diff := make([]int, lines+2)
	for _, s := range syms {
		if s.EndLine-s.Line >= 2 && s.Kind != KindField {
			lo, hi := clampLine(s.Line, lines), clampLine(s.EndLine, lines)
			diff[lo]++
			diff[hi+1]--
		}
	}
	covered := make([]int, lines+2)
	run := 0
	for l := 1; l <= lines; l++ {
		run += diff[l]
		covered[l] = run
	}
	out := make([]bool, len(syms))
	for i, s := range syms {
		switch {
		case s.Kind == KindField:
		case s.EndLine-s.Line >= 2:
			out[i] = true
		default:
			out[i] = covered[clampLine(s.Line, lines)] == 0
		}
	}
	return out
}

func clampLine(l, lines int) int {
	if l < 1 {
		return 1
	}
	if l > lines {
		return lines
	}
	return l
}

// buildChunks partitions lines into chunks owned by the innermost symbol
// covering them, and computes each chunk's weighted terms.
func buildChunks(lines []string, syms []Symbol) []chunk {
	n := len(lines)
	if n == 0 {
		return nil
	}
	owner := make([]int, n)
	for i := range owner {
		owner[i] = -1
	}
	owning := owningSymbols(syms, n)
	order := make([]int, 0, len(syms))
	for i := range syms {
		if owning[i] {
			order = append(order, i)
		}
	}
	// Outer symbols first so inner ones overwrite them.
	sort.SliceStable(order, func(a, b int) bool {
		sa, sb := syms[order[a]], syms[order[b]]
		return sa.EndLine-sa.DocLine > sb.EndLine-sb.DocLine
	})
	for _, i := range order {
		s := syms[i]
		for l := s.DocLine; l <= s.EndLine && l <= n; l++ {
			if l >= 1 {
				owner[l-1] = i
			}
		}
	}
	var chunks []chunk
	start := 0
	for start < n {
		end := start
		for end+1 < n && owner[end+1] == owner[start] {
			end++
		}
		size := maxChunkLines
		if owner[start] < 0 {
			size = unownedChunk
		}
		for s := start; s <= end; s += size {
			e := s + size - 1
			if e > end {
				e = end
			}
			// Avoid a tiny trailing window: fold it into the previous one.
			if end-e > 0 && end-e < size/4 {
				e = end
			}
			if !allBlank(lines[s : e+1]) {
				chunks = append(chunks, chunk{Start: s + 1, End: e + 1, Symbol: owner[start]})
			}
			if e == end {
				break
			}
		}
		start = end + 1
	}
	// Members that do not own lines lend their names to the chunk that
	// holds their definition.
	inner := make([][]int, len(chunks))
	for i, s := range syms {
		if owning[i] {
			continue
		}
		ci := sort.Search(len(chunks), func(k int) bool { return chunks[k].End >= s.Line })
		if ci < len(chunks) && chunks[ci].Start <= s.Line && chunks[ci].Symbol != i {
			inner[ci] = append(inner[ci], i)
		}
	}
	// Terms are counted per chunk in a dense array indexed by a per-file
	// term id, so each distinct term of the file is allocated once.
	var (
		dict    = termDict{ids: make(map[string]int32, 256)}
		acc     []float32
		stamp   []int32
		touched []int32
		tb      termBuf
		cur     int32
	)
	add := func(text string, w float32, a analyzer) {
		a.scan(text, &tb, func(t string) {
			id := dict.id(t)
			if int(id) >= len(acc) {
				acc = append(acc, 0)
				stamp = append(stamp, 0)
			}
			if stamp[id] != cur {
				stamp[id] = cur
				acc[id] = 0
				touched = append(touched, id)
			}
			acc[id] += w
		})
	}
	for ci := range chunks {
		c := &chunks[ci]
		cur = int32(ci + 1)
		touched = touched[:0]
		for l := c.Start; l <= c.End; l++ {
			add(lines[l-1], weightCode, analyzer{})
		}
		if c.Symbol >= 0 {
			s := syms[c.Symbol]
			nameW := float32(weightName)
			if c.Start > s.DocLine && c.Start > s.Line {
				nameW = weightNameCont
			}
			add(s.Name, nameW, analyzer{keepKeywords: true})
			if s.Container != "" {
				add(s.Container, weightContainer, analyzer{keepKeywords: true})
			}
			if s.Doc != "" && c.Start <= s.Line {
				add(s.Doc, weightDoc, analyzer{})
			}
		}
		for _, i := range inner[ci] {
			add(syms[i].Name, weightInnerName, analyzer{keepKeywords: true})
		}
		slices.SortFunc(touched, func(a, b int32) int { return strings.Compare(dict.terms[a], dict.terms[b]) })
		c.Terms = make([]string, len(touched))
		c.Freqs = make([]float32, len(touched))
		var total float32
		for i, id := range touched {
			c.Terms[i] = dict.terms[id]
			c.Freqs[i] = acc[id]
			total += acc[id]
		}
		c.Length = total
	}
	return chunks
}

// termDict assigns ids to the distinct terms of one file.
type termDict struct {
	ids   map[string]int32
	terms []string
}

// id returns the id of t, copying t when it is new: t may alias scratch
// memory.
func (d *termDict) id(t string) int32 {
	if id, ok := d.ids[t]; ok {
		return id
	}
	s := strings.Clone(t)
	id := int32(len(d.terms))
	d.terms = append(d.terms, s)
	d.ids[s] = id
	return id
}

func allBlank(lines []string) bool {
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			return false
		}
	}
	return true
}

// validate checks an entry loaded from the cache for internal consistency.
func (e *fileEntry) validate() error {
	if e == nil {
		return fmt.Errorf("nil entry")
	}
	if e.Path == "" || strings.HasPrefix(e.Path, "/") || strings.Contains(e.Path, "\\") || path.Clean(e.Path) != e.Path ||
		e.Path == ".." || strings.HasPrefix(e.Path, "../") {
		return fmt.Errorf("bad path %q", e.Path)
	}
	if e.Lines < 0 || e.Size < 0 {
		return fmt.Errorf("%s: bad size", e.Path)
	}
	for _, c := range e.Chunks {
		if c.Start < 1 || c.End < c.Start || len(c.Terms) != len(c.Freqs) || c.Symbol >= len(e.Symbols) || c.Symbol < -1 {
			return fmt.Errorf("%s: bad chunk", e.Path)
		}
	}
	for _, s := range e.Symbols {
		if s.Line < 1 || s.EndLine < s.Line || s.Name == "" {
			return fmt.Errorf("%s: bad symbol", e.Path)
		}
	}
	return nil
}
