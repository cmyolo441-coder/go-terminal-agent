package memory

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Hit is one recalled memory.
type Hit struct {
	Record Record
	// Score is the final ranking score in [0, 1]: fused relevance times
	// the recency and importance factors.
	Score       float64
	Explanation Explanation
}

// Explanation records how a hit was scored.
type Explanation struct {
	// VectorRank and LexicalRank are 1-based ranks in the two candidate
	// lists (0 = not retrieved by that side).
	VectorRank  int
	LexicalRank int
	// Similarity is the cosine similarity between query and memory.
	Similarity float64
	// BM25 is the lexical score; MatchedTerms the query terms it matched.
	BM25         float64
	MatchedTerms []string
	// RRF is the raw fused score; Relevance is RRF scaled to [0, 1] by the
	// best possible fused score.
	RRF       float64
	Relevance float64
	// Recency is the decay factor in [floor, 1] for Age since last access;
	// RecencyFactor the multiplier it produced after weighting.
	Recency       float64
	RecencyFactor float64
	Age           time.Duration
	// ImportanceFactor is the multiplier produced by the importance.
	ImportanceFactor float64
	// MMRPenalty is the redundancy penalty paid during diversification.
	MMRPenalty float64
	// Exact is true when the vector side was an exact scan, false for HNSW.
	Exact bool
}

// String renders the explanation on one line.
func (e Explanation) String() string {
	var b strings.Builder
	if e.VectorRank > 0 {
		fmt.Fprintf(&b, "vector #%d (cos %.3f)", e.VectorRank, e.Similarity)
	} else {
		fmt.Fprintf(&b, "vector - (cos %.3f)", e.Similarity)
	}
	if e.LexicalRank > 0 {
		fmt.Fprintf(&b, " + lexical #%d (bm25 %.2f", e.LexicalRank, e.BM25)
		if len(e.MatchedTerms) > 0 {
			fmt.Fprintf(&b, "; %s", strings.Join(e.MatchedTerms, ", "))
		}
		b.WriteString(")")
	}
	fmt.Fprintf(&b, " -> rrf %.4f (relevance %.2f); recency x%.2f (age %s); importance x%.2f",
		e.RRF, e.Relevance, e.RecencyFactor, formatAge(e.Age), e.ImportanceFactor)
	if e.MMRPenalty > 0 {
		fmt.Fprintf(&b, "; mmr -%.3f", e.MMRPenalty)
	}
	return b.String()
}

func formatAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

type scoredCand struct {
	e     *entry
	sim   float64
	bm25  float64
	fused Fused
	score float64
	expl  Explanation
}

// Recall returns up to k memories relevant to query that match f, best
// first. The vector (HNSW or exact) and BM25 rankings are fused with RRF,
// weighted by recency and importance and diversified with MMR. Returned
// records have their access time and count updated.
func (s *Store) Recall(ctx context.Context, query string, k int, f Filter) ([]Hit, error) {
	if k <= 0 {
		return nil, nil
	}
	query = cleanText(s.opts.Redact(query))
	if query == "" {
		return nil, nil
	}
	qvec, err := s.embedOne(ctx, query)
	if err != nil {
		return nil, err
	}
	terms := lexicalTerms(query)
	s.maybeCatchUp(ctx)

	rc := &s.opts.Recall
	now := s.now()
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return nil, ErrClosed
	}
	cf := f.compile(now)
	n := max(rc.Candidates, 4*k)

	// Vector side: exact scan for small eligible sets, HNSW otherwise.
	var eligible []*entry
	count := 0
	for _, e := range s.byID {
		if cf.match(&e.rec) {
			count++
			if count <= rc.ExactThreshold {
				eligible = append(eligible, e)
			}
		}
	}
	if count == 0 {
		s.mu.RUnlock()
		return nil, nil
	}
	exact := count <= rc.ExactThreshold
	accept := func(key uint64) bool {
		e := s.byKey[key]
		return e != nil && cf.match(&e.rec)
	}
	var vecKeys []uint64
	if exact {
		type ks struct {
			key uint64
			sim float32
		}
		all := make([]ks, len(eligible))
		for i, e := range eligible {
			all[i] = ks{e.key, dot(qvec, e.vec)}
		}
		slices.SortFunc(all, func(a, b ks) int {
			switch {
			case a.sim > b.sim:
				return -1
			case a.sim < b.sim:
				return 1
			case a.key < b.key:
				return -1
			case a.key > b.key:
				return 1
			}
			return 0
		})
		for i := 0; i < len(all) && i < n; i++ {
			vecKeys = append(vecKeys, all[i].key)
		}
	} else {
		ef := max(rc.EfSearch, n)
		for _, nb := range s.hnsw.Search(qvec, n, ef, accept) {
			vecKeys = append(vecKeys, nb.Key)
		}
	}
	lex := s.bm25.Search(terms, n, accept)
	lexKeys := make([]uint64, len(lex))
	lexScore := make(map[uint64]float64, len(lex))
	for i, h := range lex {
		lexKeys[i] = h.Key
		lexScore[h.Key] = h.Score
	}
	fused := FuseRRF(rc.RRFK,
		RankedList{Keys: vecKeys, Weight: rc.VectorWeight},
		RankedList{Keys: lexKeys, Weight: rc.LexicalWeight})
	maxRRF := (rc.VectorWeight + rc.LexicalWeight) / (rc.RRFK + 1)

	cands := make([]scoredCand, 0, len(fused))
	for _, fu := range fused {
		e := s.byKey[fu.Key]
		if e == nil {
			continue
		}
		c := scoredCand{e: e, sim: float64(dot(qvec, e.vec)), bm25: lexScore[fu.Key], fused: fu}
		if fu.Ranks[1] == 0 && rc.MinSimilarity >= 0 && c.sim < rc.MinSimilarity {
			continue // no lexical evidence and semantically distant
		}
		age := now.Sub(time.Unix(0, e.lastAccess.Load()))
		rec := rc.Recency.factor(e.rec.Kind, age)
		recF := 1 - rc.RecencyWeight*(1-rec)
		impF := 1 - rc.ImportanceWeight*(1-e.rec.Importance)
		rel := fu.Score / maxRRF
		c.score = rel * recF * impF
		c.expl = Explanation{
			VectorRank: fu.Ranks[0], LexicalRank: fu.Ranks[1], Similarity: c.sim, BM25: c.bm25,
			RRF: fu.Score, Relevance: rel, Recency: rec, RecencyFactor: recF, Age: max(age, 0),
			ImportanceFactor: impF, Exact: exact,
		}
		cands = append(cands, c)
	}
	slices.SortStableFunc(cands, func(a, b scoredCand) int {
		switch {
		case a.score > b.score:
			return -1
		case a.score < b.score:
			return 1
		}
		return strings.Compare(a.e.rec.ID, b.e.rec.ID)
	})
	if pool := 3 * k; len(cands) > pool {
		cands = cands[:pool]
	}
	var hits []Hit
	var touched []*entry
	if len(cands) > 0 {
		top := cands[0].score
		rel := make([]float64, len(cands))
		for i, c := range cands {
			if top > 0 {
				rel[i] = c.score / top
			}
		}
		order, pen := MMR(rel, k, rc.MMRLambda, func(i, j int) float64 {
			return float64(dot(cands[i].e.vec, cands[j].e.vec))
		})
		hits = make([]Hit, 0, len(order))
		for i, idx := range order {
			c := cands[idx]
			c.expl.MMRPenalty = pen[i]
			c.expl.MatchedTerms = s.bm25.Matched(c.e.key, terms)
			hits = append(hits, Hit{Record: c.e.record(), Score: c.score, Explanation: c.expl})
			touched = append(touched, c.e)
		}
	}
	// Touch under the read lock: the entries stay valid, and the atomics
	// make concurrent touches safe.
	s.touchMu.Lock()
	for _, e := range touched {
		at := now.UnixNano()
		for {
			cur := e.lastAccess.Load()
			if at <= cur || e.lastAccess.CompareAndSwap(cur, at) {
				break
			}
		}
		e.accesses.Add(1)
		t := s.touches[e.rec.ID]
		t.ID, t.At, t.Count = e.rec.ID, now.UTC(), t.Count+1
		s.touches[e.rec.ID] = t
	}
	s.touchMu.Unlock()
	s.mu.RUnlock()
	s.stats.recalls.Add(1)
	return hits, nil
}

// ConsolidateOptions tunes Consolidate. Zero fields take defaults.
type ConsolidateOptions struct {
	// Filter selects the records to consider (default: facts and episodes).
	Filter Filter
	// Threshold is the cosine similarity that links two records into one
	// cluster (default 0.75).
	Threshold float64
	// MinCluster is the smallest cluster worth summarising (default 3);
	// MaxCluster caps how many records one summary covers, the most recent
	// being kept (default 24); MaxClusters caps summaries per call
	// (default 16).
	MinCluster, MaxCluster, MaxClusters int
}

// ConsolidateReport says what Consolidate did.
type ConsolidateReport struct {
	Clusters int      // clusters found
	Created  []string // IDs of the summary records
	Merged   int      // source records replaced by summaries
	Skipped  int      // clusters skipped (changed concurrently or failed)
	Errors   []error  // summarizer or embedding failures
}

// Consolidate finds clusters of closely related memories (same kind,
// scope and namespace) and replaces each with one summary produced by the
// configured Summarizer. A cluster whose members change while it is being
// summarised is skipped, so concurrent edits are never lost.
func (s *Store) Consolidate(ctx context.Context, o ConsolidateOptions) (ConsolidateReport, error) {
	var rep ConsolidateReport
	if s.opts.Summarizer == nil {
		return rep, ErrNoSummarizer
	}
	if o.Threshold <= 0 {
		o.Threshold = 0.75
	}
	if o.MinCluster < 2 {
		o.MinCluster = 3
	}
	if o.MaxCluster < o.MinCluster {
		o.MaxCluster = max(24, o.MinCluster)
	}
	if o.MaxClusters <= 0 {
		o.MaxClusters = 16
	}
	if len(o.Filter.Kinds) == 0 {
		o.Filter.Kinds = []Kind{KindFact, KindEpisode}
	}
	type member struct {
		rec Record
		vec []float32
		key uint64
	}
	s.maybeCatchUp(ctx)
	s.mu.RLock()
	cf := o.Filter.compile(s.now())
	groups := map[string][]member{}
	for _, e := range s.byID {
		if cf.match(&e.rec) {
			gk := groupKey(&e.rec)
			groups[gk] = append(groups[gk], member{e.record(), e.vec, e.key})
		}
	}
	// Union-find over similarity edges: exact pairs for small groups, the
	// HNSW neighbourhood for large ones.
	var clusters [][]member
	for _, ms := range groups {
		if len(ms) < o.MinCluster {
			continue
		}
		slices.SortFunc(ms, func(a, b member) int { return strings.Compare(a.rec.ID, b.rec.ID) })
		parent := make([]int, len(ms))
		for i := range parent {
			parent[i] = i
		}
		var find func(int) int
		find = func(i int) int {
			for parent[i] != i {
				parent[i] = parent[parent[i]]
				i = parent[i]
			}
			return i
		}
		union := func(a, b int) {
			if ra, rb := find(a), find(b); ra != rb {
				parent[ra] = rb
			}
		}
		if len(ms) <= 1024 {
			for i := range ms {
				for j := i + 1; j < len(ms); j++ {
					if float64(dot(ms[i].vec, ms[j].vec)) >= o.Threshold {
						union(i, j)
					}
				}
			}
		} else {
			pos := make(map[uint64]int, len(ms))
			for i, m := range ms {
				pos[m.key] = i
			}
			for i, m := range ms {
				for _, nb := range s.hnsw.Search(m.vec, 12, 64, func(k uint64) bool { _, ok := pos[k]; return ok }) {
					if 1-float64(nb.Distance) >= o.Threshold {
						union(i, pos[nb.Key])
					}
				}
			}
		}
		byRoot := map[int][]member{}
		for i := range ms {
			r := find(i)
			byRoot[r] = append(byRoot[r], ms[i])
		}
		for _, c := range byRoot {
			if len(c) >= o.MinCluster {
				clusters = append(clusters, c)
			}
		}
	}
	s.mu.RUnlock()
	rep.Clusters = len(clusters)
	slices.SortFunc(clusters, func(a, b []member) int {
		if len(a) != len(b) {
			return len(b) - len(a)
		}
		return strings.Compare(a[0].rec.ID, b[0].rec.ID)
	})
	if len(clusters) > o.MaxClusters {
		clusters = clusters[:o.MaxClusters]
	}
	for _, c := range clusters {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		slices.SortFunc(c, func(a, b member) int {
			if d := a.rec.Created.Compare(b.rec.Created); d != 0 {
				return d
			}
			return strings.Compare(a.rec.ID, b.rec.ID)
		})
		if len(c) > o.MaxCluster {
			c = c[len(c)-o.MaxCluster:]
		}
		recs := make([]Record, len(c))
		for i, m := range c {
			recs[i] = m.rec.Clone()
		}
		text, err := s.opts.Summarizer(ctx, recs)
		if err != nil {
			rep.Skipped++
			rep.Errors = append(rep.Errors, fmt.Errorf("summarizing %d memories: %w", len(c), err))
			continue
		}
		now := s.now()
		sum := Record{
			Kind: c[0].rec.Kind, Scope: c[0].rec.Scope, Namespace: c[0].rec.Namespace,
			Text: text, Source: "consolidation", Created: c[0].rec.Created,
		}
		var tags []string
		for _, m := range c {
			sum.Importance = max(sum.Importance, m.rec.Importance)
			sum.AccessCount += m.rec.AccessCount
			sum.Reinforced += m.rec.Reinforced + 1
			sum.MergedFrom = append(sum.MergedFrom, m.rec.ID)
			sum.LastAccess = later(sum.LastAccess, m.rec.LastAccess)
			for _, t := range m.rec.Tags {
				if !slices.Contains(tags, t) && len(tags) < maxTags {
					tags = append(tags, t)
				}
			}
			if m.rec.TTL == 0 {
				sum.TTL = -1 // marker: some member never expires
			} else if sum.TTL >= 0 {
				sum.TTL = max(sum.TTL, m.rec.TTL)
			}
		}
		sum.TTL = max(sum.TTL, 0)
		sum.Tags = tags
		sum.Importance = min(1, sum.Importance+0.05)
		if err := normalizeRecord(&sum, s.opts.Redact, s.opts.MaxTextBytes, now); err != nil {
			rep.Skipped++
			rep.Errors = append(rep.Errors, fmt.Errorf("summary rejected: %w", err))
			continue
		}
		vec, err := s.embedOne(ctx, sum.Text)
		if err != nil {
			rep.Skipped++
			rep.Errors = append(rep.Errors, err)
			continue
		}
		applied := false
		err = s.write(ctx, func(b *walBatch) error {
			ids := make([]string, len(c))
			for i, m := range c {
				e := s.byID[m.rec.ID]
				if e == nil || !e.rec.Updated.Equal(m.rec.Updated) || e.rec.Text != m.rec.Text {
					return nil // changed meanwhile: leave this cluster alone
				}
				ids[i] = m.rec.ID
			}
			b.put(&storedRecord{Key: s.allocKey(), Rec: sum, Vec: vec})
			b.del(ids...)
			applied = true
			return nil
		})
		if err != nil {
			return rep, err
		}
		if !applied {
			rep.Skipped++
			continue
		}
		rep.Created = append(rep.Created, sum.ID)
		rep.Merged += len(c)
		s.stats.consolidated.Add(int64(len(c)))
	}
	s.maybeRebuild()
	return rep, nil
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
