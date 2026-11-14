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
// concurrent use.
type BM25 struct {
	mu       sync.RWMutex
	k1, b    float64
	postings map[string]map[uint64]uint32 // term -> doc -> tf
	docs     map[uint64]bm25Doc
	totalLen uint64
}

type bm25Doc struct {
	length uint32
	terms  []string // distinct terms, for removal
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
	return &BM25{k1: cfg.K1, b: cfg.B, postings: map[string]map[uint64]uint32{}, docs: map[uint64]bm25Doc{}}
}

// Len is the number of indexed documents.
func (ix *BM25) Len() int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return len(ix.docs)
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
	for _, t := range terms {
		tf[t]++
	}
	distinct := make([]string, 0, len(tf))
	for t, n := range tf {
		p := ix.postings[t]
		if p == nil {
			p = map[uint64]uint32{}
			ix.postings[t] = p
		}
		p[key] = n
		distinct = append(distinct, t)
	}
	ix.docs[key] = bm25Doc{length: uint32(len(terms)), terms: distinct}
	ix.totalLen += uint64(len(terms))
}

// Remove drops key and reports whether it was indexed.
func (ix *BM25) Remove(key uint64) bool {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return ix.removeLocked(key)
}

func (ix *BM25) removeLocked(key uint64) bool {
	d, ok := ix.docs[key]
	if !ok {
		return false
	}
	for _, t := range d.terms {
		p := ix.postings[t]
		delete(p, key)
		if len(p) == 0 {
			delete(ix.postings, t)
		}
	}
	ix.totalLen -= uint64(d.length)
	delete(ix.docs, key)
	return true
}

// IDF returns the inverse document frequency of term.
func (ix *BM25) IDF(term string) float64 {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.idfLocked(len(ix.postings[term]))
}

func (ix *BM25) idfLocked(df int) float64 {
	n := float64(len(ix.docs))
	return math.Log(1 + (n-float64(df)+0.5)/(float64(df)+0.5))
}

// Search scores every document containing at least one query term and
// returns the best n (all when n <= 0) by descending score. Repeated query
// terms count once. accept, when non-nil, filters documents.
func (ix *BM25) Search(query []string, n int, accept func(key uint64) bool) []LexicalHit {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	if len(ix.docs) == 0 || len(query) == 0 {
		return nil
	}
	avg := float64(ix.totalLen) / float64(len(ix.docs))
	if avg == 0 {
		avg = 1
	}
	scores := map[uint64]float64{}
	seen := make(map[string]bool, len(query))
	for _, t := range query {
		if seen[t] {
			continue
		}
		seen[t] = true
		p := ix.postings[t]
		if len(p) == 0 {
			continue
		}
		idf := ix.idfLocked(len(p))
		for key, tf := range p {
			if accept != nil && !accept(key) {
				continue
			}
			f := float64(tf)
			dl := float64(ix.docs[key].length)
			scores[key] += idf * f * (ix.k1 + 1) / (f + ix.k1*(1-ix.b+ix.b*dl/avg))
		}
	}
	hits := make([]LexicalHit, 0, len(scores))
	for k, s := range scores {
		hits = append(hits, LexicalHit{Key: k, Score: s})
	}
	slices.SortFunc(hits, func(a, b LexicalHit) int {
		switch {
		case a.Score > b.Score:
			return -1
		case a.Score < b.Score:
			return 1
		case a.Key < b.Key:
			return -1
		case a.Key > b.Key:
			return 1
		}
		return 0
	})
	if n > 0 && len(hits) > n {
		hits = hits[:n]
	}
	return hits
}

// Matched returns the query terms present in document key, in query order
// without repeats.
func (ix *BM25) Matched(key uint64, query []string) []string {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	var out []string
	for _, t := range query {
		if _, ok := ix.postings[t][key]; ok && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}
