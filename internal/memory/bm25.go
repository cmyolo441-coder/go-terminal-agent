package memory

import (
	"math"
	"slices"
	"sync"
)

// BM25Config holds the Okapi BM25 parameters. Zero fields take defaults.
type BM25Config struct {
	// K1 controls term-frequency saturation (default 1.2).
	K1 float64
	// B controls document-length normalisation in [0, 1] (default 0.75);
	// a negative value disables length normalisation.
	B float64
}

// BM25 is an in-memory inverted index scored with Okapi BM25 and the
// non-negative Lucene IDF, ln(1 + (N - df + 0.5) / (df + 0.5)). Documents
// are bags of pre-analysed terms keyed by a uint64. It is safe for
// concurrent use: searches share a read lock.
//
// Documents live in dense slots (reused after removal) so that scoring
// accumulates into a flat array instead of a hash map, and posting lists
// are parallel slices that scan sequentially.
type BM25 struct {
	mu       sync.RWMutex
	k1, b    float64
	postings map[string]*postingList
	slots    []bm25Doc
	free     []uint32
	byKey    map[uint64]uint32
	totalLen uint64

	scratch sync.Pool // *bm25Scratch
}

type postingList struct {
	slots []uint32
	tfs   []uint32
}

type bm25Doc struct {
	key    uint64
	live   bool
	length uint32
	terms  []string // distinct terms, for removal
}

type bm25Scratch struct {
	acc     []float64
	touched []uint32
}

// LexicalHit is a BM25 search result.
type LexicalHit struct {
	Key   uint64
	Score float64
}

// NewBM25 returns an empty index.
func NewBM25(cfg BM25Config) *BM25 {
	if cfg.K1 <= 0 {
		cfg.K1 = 1.2
	}
	switch {
	case cfg.B < 0:
		cfg.B = 0
	case cfg.B == 0 || cfg.B > 1:
		cfg.B = 0.75
	}
	ix := &BM25{k1: cfg.K1, b: cfg.B, postings: map[string]*postingList{}, byKey: map[uint64]uint32{}}
	ix.scratch.New = func() any { return &bm25Scratch{} }
	return ix
}

// Len is the number of indexed documents.
func (ix *BM25) Len() int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return len(ix.byKey)
}

// Terms is the number of distinct terms.
func (ix *BM25) Terms() int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return len(ix.postings)
}

// Add indexes terms under key, replacing any previous document.
func (ix *BM25) Add(key uint64, terms []string) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.removeLocked(key)
	tf := make(map[string]uint32, len(terms))
	distinct := make([]string, 0, len(terms))
	for _, t := range terms {
		if tf[t] == 0 {
			distinct = append(distinct, t)
		}
		tf[t]++
	}
	var slot uint32
	if n := len(ix.free); n > 0 {
		slot = ix.free[n-1]
		ix.free = ix.free[:n-1]
	} else {
		slot = uint32(len(ix.slots))
		ix.slots = append(ix.slots, bm25Doc{})
	}
	for _, t := range distinct {
		p := ix.postings[t]
		if p == nil {
			p = &postingList{}
			ix.postings[t] = p
		}
		p.slots = append(p.slots, slot)
		p.tfs = append(p.tfs, tf[t])
	}
	ix.slots[slot] = bm25Doc{key: key, live: true, length: uint32(len(terms)), terms: distinct}
	ix.byKey[key] = slot
	ix.totalLen += uint64(len(terms))
}

// Remove drops key and reports whether it was indexed.
func (ix *BM25) Remove(key uint64) bool {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return ix.removeLocked(key)
}

func (ix *BM25) removeLocked(key uint64) bool {
	slot, ok := ix.byKey[key]
	if !ok {
		return false
	}
	d := &ix.slots[slot]
	for _, t := range d.terms {
		p := ix.postings[t]
		if i := slices.Index(p.slots, slot); i >= 0 {
			last := len(p.slots) - 1
			p.slots[i], p.tfs[i] = p.slots[last], p.tfs[last]
			p.slots, p.tfs = p.slots[:last], p.tfs[:last]
		}
		if len(p.slots) == 0 {
			delete(ix.postings, t)
		}
	}
	ix.totalLen -= uint64(d.length)
	*d = bm25Doc{}
	ix.free = append(ix.free, slot)
	delete(ix.byKey, key)
	return true
}

// IDF returns the inverse document frequency of term.
func (ix *BM25) IDF(term string) float64 {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	df := 0
	if p := ix.postings[term]; p != nil {
		df = len(p.slots)
	}
	return ix.idfLocked(df)
}

func (ix *BM25) idfLocked(df int) float64 {
	n := float64(len(ix.byKey))
	return math.Log(1 + (n-float64(df)+0.5)/(float64(df)+0.5))
}

// Search scores every document containing at least one query term and
// returns the best n (all when n <= 0) by descending score, ties by key.
// Repeated query terms count once. accept, when non-nil, filters
// documents; it is called once per matching document.
func (ix *BM25) Search(query []string, n int, accept func(key uint64) bool) []LexicalHit {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	if len(ix.byKey) == 0 || len(query) == 0 {
		return nil
	}
	avg := float64(ix.totalLen) / float64(len(ix.byKey))
	if avg == 0 {
		avg = 1
	}
	sc := ix.scratch.Get().(*bm25Scratch)
	defer ix.scratch.Put(sc)
	if len(sc.acc) < len(ix.slots) {
		sc.acc = make([]float64, len(ix.slots)+len(ix.slots)/4)
	}
	acc, touched := sc.acc, sc.touched[:0]
	k1, b := ix.k1, ix.b
	norm := k1 * (1 - b)
	lenScale := k1 * b / avg
	for qi, t := range query {
		if slices.Contains(query[:qi], t) {
			continue
		}
		p := ix.postings[t]
		if p == nil {
			continue
		}
		idf := ix.idfLocked(len(p.slots)) * (k1 + 1)
		for i, slot := range p.slots {
			f := float64(p.tfs[i])
			if acc[slot] == 0 {
				touched = append(touched, slot)
			}
			acc[slot] += idf * f / (f + norm + lenScale*float64(ix.slots[slot].length))
		}
	}
	limit := len(touched)
	if n > 0 {
		limit = min(n, limit)
	}
	h := lexHeap{s: make([]LexicalHit, 0, limit)}
	for _, slot := range touched {
		score := acc[slot]
		acc[slot] = 0
		key := ix.slots[slot].key
		if score <= 0 || (accept != nil && !accept(key)) {
			continue
		}
		hit := LexicalHit{Key: key, Score: score}
		if n <= 0 || h.len() < n {
			h.push(hit)
		} else if lexBetter(hit, h.s[0]) {
			h.s[0] = hit
			h.down(0)
		}
	}
	sc.touched = touched[:0]
	out := h.s
	slices.SortFunc(out, func(a, b LexicalHit) int {
		if lexBetter(a, b) {
			return -1
		}
		if lexBetter(b, a) {
			return 1
		}
		return 0
	})
	return out
}

// lexBetter orders hits by descending score, then ascending key.
func lexBetter(a, b LexicalHit) bool {
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	return a.Key < b.Key
}

// lexHeap is a bounded heap with the worst hit on top.
type lexHeap struct{ s []LexicalHit }

func (h *lexHeap) len() int { return len(h.s) }

func (h *lexHeap) push(x LexicalHit) {
	h.s = append(h.s, x)
	i := len(h.s) - 1
	for i > 0 {
		p := (i - 1) / 2
		if !lexBetter(h.s[p], h.s[i]) {
			break
		}
		h.s[p], h.s[i] = h.s[i], h.s[p]
		i = p
	}
}

func (h *lexHeap) down(i int) {
	n := len(h.s)
	for {
		l, r, w := 2*i+1, 2*i+2, i
		if l < n && lexBetter(h.s[w], h.s[l]) {
			w = l
		}
		if r < n && lexBetter(h.s[w], h.s[r]) {
			w = r
		}
		if w == i {
			return
		}
		h.s[w], h.s[i] = h.s[i], h.s[w]
		i = w
	}
}

// Matched returns the query terms present in document key, in query order
// without repeats.
func (ix *BM25) Matched(key uint64, query []string) []string {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	slot, ok := ix.byKey[key]
	if !ok {
		return nil
	}
	var out []string
	for _, t := range query {
		if slices.Contains(out, t) {
			continue
		}
		if slices.Contains(ix.slots[slot].terms, t) {
			out = append(out, t)
		}
	}
	return out
}
