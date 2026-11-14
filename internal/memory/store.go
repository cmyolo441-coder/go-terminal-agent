package memory

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Summarizer condenses a cluster of related memories into one statement.
// It is typically backed by a small LLM call. The records are ordered from
// oldest to newest; later ones should win when they conflict.
type Summarizer func(ctx context.Context, cluster []Record) (string, error)

// RecallConfig tunes hybrid retrieval. Zero fields take defaults.
type RecallConfig struct {
	// RRFK is the Reciprocal Rank Fusion constant (default 60).
	RRFK float64
	// VectorWeight and LexicalWeight weight the two rankings in the fusion
	// (default 1 each).
	VectorWeight, LexicalWeight float64
	// Candidates is the number of candidates each retriever contributes
	// (default 64; at least 4*k).
	Candidates int
	// MinSimilarity drops candidates that have no lexical match and whose
	// cosine similarity is below this value (default 0.2), so recall can
	// come back empty instead of padding the prompt with noise. A negative
	// value disables the gate.
	MinSimilarity float64
	// MMRLambda trades relevance against diversity (default 0.75; 1
	// disables diversification).
	MMRLambda float64
	// RecencyWeight in [0, 1] is how much the recency factor can demote a
	// memory (default 0.3); ImportanceWeight likewise for importance
	// (default 0.3). A negative value disables the factor.
	RecencyWeight, ImportanceWeight float64
	// Recency configures the decay curve per kind.
	Recency RecencyWeights
	// ExactThreshold: when at most this many records pass the filter the
	// vector side is an exact scan instead of an HNSW search (default 4096).
	ExactThreshold int
	// EfSearch overrides the HNSW search breadth (default: index setting,
	// raised to the candidate count).
	EfSearch int
}

// Options configure a Store.
type Options struct {
	// Dir is the store directory. Empty keeps everything in memory.
	Dir string
	// Redact is applied to every text before it is embedded, indexed or
	// stored (required). Wire it to the secret redactor.
	Redact func(string) string
	// Embedder produces vectors (default: DefaultHashEmbedder()).
	Embedder Embedder
	// Summarizer enables Consolidate.
	Summarizer Summarizer
	// HNSW and BM25 tune the indexes (HNSW.Dim is taken from the embedder).
	HNSW HNSWConfig
	BM25 BM25Config
	// Recall tunes retrieval.
	Recall RecallConfig
	// DedupeThreshold is the cosine similarity at or above which a new
	// memory is merged into an existing one of the same kind, scope and
	// namespace (default 0.92; negative disables near-duplicate merging;
	// exact duplicates after normalisation are always merged).
	DedupeThreshold float64
	// MaxTextBytes limits one record's text (default 4096).
	MaxTextBytes int
	// MaxRecords and MaxBytes are the size budget (defaults 50000 records
	// and 64 MiB of text). Over budget, expired records go first, then the
	// least recently used, with ImportanceProtection worth of extra
	// recency per unit of importance. Pinned records are never evicted.
	MaxRecords int
	MaxBytes   int64
	// ImportanceProtection is how much recency a fully important record
	// gains in the eviction order (default 30 days).
	ImportanceProtection time.Duration
	// RebuildRatio triggers an HNSW rebuild when tombstones exceed this
	// fraction of the graph (default 0.25).
	RebuildRatio float64
	// CompactBytes snapshots the store when the write-ahead log grows past
	// this size (default 4 MiB).
	CompactBytes int64
	// NoSync skips fsync after log appends (faster, loses the latest
	// changes on power failure; snapshots are always synced).
	NoSync bool
	// RecoverCorrupt salvages what it can from a corrupt snapshot instead
	// of failing Open; the damaged file is kept with a ".corrupt" suffix.
	RecoverCorrupt bool
	// LockTimeout bounds waiting for another process holding the store
	// lock (default 15s) when the caller's context has no deadline.
	LockTimeout time.Duration
	// Clock returns the current time (default time.Now).
	Clock func() time.Time
	// Logger receives warnings about recovery and maintenance (default:
	// discarded).
	Logger *slog.Logger
}

// Store is the long-term memory. It is safe for concurrent use and, with a
// Dir, for use by several processes at once.
type Store struct {
	opts   Options
	emb    Embedder
	dim    int
	embTag uint32 // hash of the embedder ID, stamped on logged vectors
	log    *slog.Logger
	now    func() time.Time

	mu        sync.RWMutex
	byID      map[string]*entry
	byKey     map[uint64]*entry
	exact     map[string]string // dedupe key -> id
	hnsw      *HNSW
	bm25      *BM25
	nextKey   uint64
	textBytes int64
	disk      *diskState // nil for in-memory stores
	closed    bool

	rebuilding atomic.Bool

	touchMu sync.Mutex
	touches map[string]touch

	stats counters
}

type counters struct {
	remembered, merged, recalls, forgotten, expired, evicted, consolidated, rebuilds, compactions, reloads atomic.Int64
}

// entry is one live record. rec is immutable once published; access
// metadata changes through the atomics.
type entry struct {
	rec        Record
	vec        []float32
	key        uint64
	exactKey   string
	lastAccess atomic.Int64 // UnixNano
	accesses   atomic.Uint32
}

func (e *entry) record() Record {
	r := e.rec.Clone()
	r.LastAccess = time.Unix(0, e.lastAccess.Load()).UTC()
	r.AccessCount = e.accesses.Load()
	return r
}

func (e *entry) stored() *storedRecord {
	return &storedRecord{Key: e.key, Rec: e.record(), Vec: e.vec}
}

// Open opens (creating if needed) a store.
func Open(ctx context.Context, opts Options) (*Store, error) {
	if opts.Redact == nil {
		return nil, errors.New("memory: Options.Redact is required so secrets are never stored")
	}
	if opts.Embedder == nil {
		opts.Embedder = DefaultHashEmbedder()
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.DedupeThreshold == 0 {
		opts.DedupeThreshold = 0.92
	}
	if opts.MaxTextBytes <= 0 {
		opts.MaxTextBytes = 4096
	}
	if opts.MaxRecords <= 0 {
		opts.MaxRecords = 50000
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = 64 << 20
	}
	if opts.ImportanceProtection <= 0 {
		opts.ImportanceProtection = 30 * 24 * time.Hour
	}
	if opts.RebuildRatio <= 0 {
		opts.RebuildRatio = 0.25
	}
	if opts.CompactBytes <= 0 {
		opts.CompactBytes = 4 << 20
	}
	if opts.LockTimeout <= 0 {
		opts.LockTimeout = 15 * time.Second
	}
	rc := &opts.Recall
	if rc.RRFK <= 0 {
		rc.RRFK = 60
	}
	if rc.VectorWeight <= 0 {
		rc.VectorWeight = 1
	}
	if rc.LexicalWeight <= 0 {
		rc.LexicalWeight = 1
	}
	if rc.Candidates <= 0 {
		rc.Candidates = 64
	}
	if rc.MinSimilarity == 0 {
		rc.MinSimilarity = 0.2
	}
	if rc.MMRLambda <= 0 || rc.MMRLambda > 1 {
		rc.MMRLambda = 0.75
	}
	if rc.RecencyWeight == 0 {
		rc.RecencyWeight = 0.3
	}
	if rc.ImportanceWeight == 0 {
		rc.ImportanceWeight = 0.3
	}
	rc.RecencyWeight = min(max(rc.RecencyWeight, 0), 1)
	rc.ImportanceWeight = min(max(rc.ImportanceWeight, 0), 1)
	if rc.ExactThreshold <= 0 {
		rc.ExactThreshold = 4096
	}

	s := &Store{
		opts: opts, emb: opts.Embedder, log: opts.Logger, now: opts.Clock,
		touches: map[string]touch{},
	}
	s.dim = s.emb.Dim()
	if s.dim == 0 {
		vecs, err := s.emb.Embed(ctx, []string{"dimension probe"})
		if err != nil {
			return nil, fmt.Errorf("memory: probing embedder: %w", err)
		}
		if len(vecs) != 1 || len(vecs[0]) == 0 {
			return nil, fmt.Errorf("%w: probe returned no vector", ErrEmbedding)
		}
		s.dim = len(vecs[0])
	}
	s.embTag = uint32(hashString(0, s.emb.ID()))
	if err := s.resetIndexes(); err != nil {
		return nil, err
	}
	if opts.Dir == "" {
		return s, nil
	}
	if err := s.openDisk(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// resetIndexes empties the in-memory state.
func (s *Store) resetIndexes() error {
	hc := s.opts.HNSW
	hc.Dim = s.dim
	h, err := NewHNSW(hc)
	if err != nil {
		return err
	}
	s.hnsw = h
	s.bm25 = NewBM25(s.opts.BM25)
	s.byID = map[string]*entry{}
	s.byKey = map[uint64]*entry{}
	s.exact = map[string]string{}
	s.textBytes = 0
	return nil
}

// Embedder returns the store's embedder.
func (s *Store) Embedder() Embedder { return s.emb }

func groupKey(r *Record) string {
	return string(r.Kind) + "\x00" + string(r.Scope) + "\x00" + r.Namespace
}

func lexicalDocTerms(r *Record) []string {
	terms := lexicalTerms(r.Text)
	for _, t := range r.Tags {
		// Tags count twice: they are deliberate labels.
		tt := lexicalTerms(strings.ReplaceAll(t, "-", " "))
		terms = append(terms, tt...)
		terms = append(terms, tt...)
	}
	return terms
}

// applyPut inserts or replaces a record in memory. The caller holds s.mu.
func (s *Store) applyPut(sr *storedRecord) error {
	if len(sr.Vec) != s.dim {
		return fmt.Errorf("%w: record %s has a %d-dimensional vector, store uses %d", ErrDimension, sr.Rec.ID, len(sr.Vec), s.dim)
	}
	old := s.byID[sr.Rec.ID]
	if old != nil {
		s.unindex(old, old.key != sr.Key)
	}
	if other := s.byKey[sr.Key]; other != nil && other != old {
		// Keys are unique per vector; a clash means a foreign writer reused
		// one. Drop the stale holder rather than corrupting the indexes.
		s.unindex(other, true)
		delete(s.byID, other.rec.ID)
	}
	e := &entry{rec: sr.Rec, vec: sr.Vec, key: sr.Key}
	e.rec.LastAccess, e.rec.AccessCount = time.Time{}, 0
	e.lastAccess.Store(sr.Rec.LastAccess.UnixNano())
	e.accesses.Store(sr.Rec.AccessCount)
	if old == nil || old.key != sr.Key {
		if err := s.hnsw.Insert(sr.Key, sr.Vec); err != nil {
			return err
		}
	}
	s.bm25.Add(sr.Key, lexicalDocTerms(&e.rec))
	e.exactKey = groupKey(&e.rec) + "\x00" + normalizedKey(e.rec.Text)
	if _, taken := s.exact[e.exactKey]; !taken {
		s.exact[e.exactKey] = e.rec.ID
	}
	s.byID[e.rec.ID] = e
	s.byKey[e.key] = e
	s.textBytes += int64(len(e.rec.Text))
	if sr.Key >= s.nextKey {
		s.nextKey = sr.Key + 1
	}
	return nil
}

// unindex removes e from the indexes (the vector only when dropVec).
func (s *Store) unindex(e *entry, dropVec bool) {
	if dropVec {
		s.hnsw.Delete(e.key)
	}
	s.bm25.Remove(e.key)
	delete(s.byKey, e.key)
	if s.exact[e.exactKey] == e.rec.ID {
		delete(s.exact, e.exactKey)
	}
	s.textBytes -= int64(len(e.rec.Text))
}

// applyDelete removes a record; the caller holds s.mu.
func (s *Store) applyDelete(id string) bool {
	e := s.byID[id]
	if e == nil {
		return false
	}
	s.unindex(e, true)
	delete(s.byID, id)
	return true
}

func (s *Store) applyTouch(t touch) {
	if e := s.byID[t.ID]; e != nil {
		at := t.At.UnixNano()
		for {
			cur := e.lastAccess.Load()
			if at <= cur || e.lastAccess.CompareAndSwap(cur, at) {
				break
			}
		}
		e.accesses.Add(t.Count)
	}
}

// lockCtx returns ctx bounded by LockTimeout when it has no deadline.
func (s *Store) lockCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, s.opts.LockTimeout)
}

// write runs fn with exclusive access: the inter-process lock (persistent
// stores), s.mu, and the in-memory state caught up with the disk. Changes
// fn logs through the batch are made durable before write returns.
func (s *Store) write(ctx context.Context, fn func(b *walBatch) error) error {
	lctx, cancel := s.lockCtx(ctx)
	defer cancel()
	unlock, err := s.lockFiles(lctx)
	if err != nil {
		return err
	}
	defer unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if err := s.catchUpLocked(ctx); err != nil {
		return err
	}
	b := &walBatch{s: s}
	if t := s.takeTouches(); len(t) > 0 {
		b.frame(walTouch, encodeTouches(t))
	}
	ferr := fn(b)
	if err := b.commit(); err != nil {
		return errors.Join(ferr, err)
	}
	if ferr != nil {
		return ferr
	}
	return s.maybeCompactLocked()
}

// walBatch collects log frames and applies them to memory once logged.
type walBatch struct {
	s       *Store
	buf     []byte
	pending []func() error
}

func (b *walBatch) frame(typ byte, payload []byte) { b.buf = appendFrame(b.buf, typ, payload) }

// put logs and (on commit) applies a record.
func (b *walBatch) put(sr *storedRecord) {
	b.frame(walPut, encodeWALPut(b.s.embTag, sr))
	b.pending = append(b.pending, func() error { return b.s.applyPut(sr) })
}

func (b *walBatch) del(ids ...string) {
	if len(ids) == 0 {
		return
	}
	b.frame(walDelete, encodeIDs(ids))
	b.pending = append(b.pending, func() error {
		for _, id := range ids {
			b.s.applyDelete(id)
		}
		return nil
	})
}

// commit writes the frames ahead of applying them; on a write error
// nothing is applied.
func (b *walBatch) commit() error {
	if len(b.buf) > 0 && b.s.disk != nil {
		if err := b.s.appendLog(b.buf); err != nil {
			return err
		}
	}
	var errs []error
	for _, fn := range b.pending {
		if err := fn(); err != nil {
			errs = append(errs, err)
		}
	}
	b.buf, b.pending = nil, nil
	return errors.Join(errs...)
}

// takeTouches drains pending access updates.
func (s *Store) takeTouches() []touch {
	s.touchMu.Lock()
	defer s.touchMu.Unlock()
	if len(s.touches) == 0 {
		return nil
	}
	out := make([]touch, 0, len(s.touches))
	for _, t := range s.touches {
		out = append(out, t)
	}
	clear(s.touches)
	slices.SortFunc(out, func(a, b touch) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// RememberResult describes what Remember did.
type RememberResult struct {
	// ID of the stored record (the existing one when merged).
	ID string
	// Merged is true when the memory was merged into an existing
	// near-duplicate; Similarity is their cosine similarity.
	Merged     bool
	Similarity float64
	// Replaced is true when a record with the given ID already existed.
	Replaced bool
	// Evicted lists records dropped to stay within the size budget.
	Evicted []string
}

// Remember stores a memory. The text, tags, namespace and source are
// redacted first. A memory that duplicates an existing one of the same
// kind, scope and namespace (identical after normalisation, or with cosine
// similarity >= DedupeThreshold) is merged into it: the newer text wins,
// tags are united, importance and TTL take the maximum and the record is
// marked reinforced.
func (s *Store) Remember(ctx context.Context, rec Record) (RememberResult, error) {
	rec = rec.Clone()
	now := s.now()
	createdGiven := !rec.Created.IsZero()
	if err := normalizeRecord(&rec, s.opts.Redact, s.opts.MaxTextBytes, now); err != nil {
		return RememberResult{}, err
	}
	vec, err := s.vectorFor(ctx, &rec)
	if err != nil {
		return RememberResult{}, err
	}
	var res RememberResult
	err = s.write(ctx, func(b *walBatch) error {
		res = RememberResult{}
		final, key := rec, uint64(0)
		finalVec := vec
		if old := s.byID[rec.ID]; old != nil {
			res.Replaced = true
			if !createdGiven {
				final.Created = old.rec.Created
			}
			final.AccessCount = old.accesses.Load()
			final.Reinforced = old.rec.Reinforced
			if old.rec.Text == rec.Text {
				key, finalVec = old.key, old.vec
			}
		} else if dup, sim := s.findDuplicate(&rec, vec); dup != nil {
			res.Merged, res.Similarity = true, sim
			final = mergeRecords(dup.record(), rec, now)
			if dup.rec.Text == final.Text {
				key, finalVec = dup.key, dup.vec
			}
		}
		if key == 0 {
			key = s.allocKey()
		}
		res.ID = final.ID
		b.put(&storedRecord{Key: key, Rec: final, Vec: finalVec})
		if err := b.commit(); err != nil {
			return err
		}
		res.Evicted = s.enforceBudgetLocked(b, final.ID)
		return nil
	})
	if err != nil {
		return RememberResult{}, err
	}
	if res.Merged {
		s.stats.merged.Add(1)
	} else {
		s.stats.remembered.Add(1)
	}
	s.maybeRebuild()
	return res, nil
}

// vectorFor embeds rec.Text, reusing the stored vector when a record with
// the same ID and text exists.
func (s *Store) vectorFor(ctx context.Context, rec *Record) ([]float32, error) {
	s.mu.RLock()
	if old := s.byID[rec.ID]; old != nil && old.rec.Text == rec.Text {
		v := old.vec
		s.mu.RUnlock()
		return v, nil
	}
	s.mu.RUnlock()
	return s.embedOne(ctx, rec.Text)
}

func (s *Store) embedOne(ctx context.Context, text string) ([]float32, error) {
	vecs, err := s.embedMany(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return vecs[0], nil
}

// embedMany embeds and normalises; every vector must have the store's
// dimension and be non-zero.
func (s *Store) embedMany(ctx context.Context, texts []string) ([][]float32, error) {
	vecs, err := s.emb.Embed(ctx, texts)
	if err != nil {
		if errors.Is(err, ErrEmbedding) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %w", ErrEmbedding, err)
	}
	if len(vecs) != len(texts) {
		return nil, fmt.Errorf("%w: %d vectors for %d texts", ErrEmbedding, len(vecs), len(texts))
	}
	out := make([][]float32, len(vecs))
	for i, v := range vecs {
		if len(v) != s.dim {
			return nil, fmt.Errorf("%w: vector has %d dimensions, store uses %d", ErrEmbedding, len(v), s.dim)
		}
		if out[i] = normalized(v); out[i] == nil {
			// A text without features (e.g. only punctuation) still gets
			// a valid, uninformative direction.
			out[i] = make([]float32, s.dim)
			out[i][int(hashString(1, texts[i])%uint64(s.dim))] = 1
		}
	}
	return out, nil
}

func (s *Store) allocKey() uint64 {
	if s.nextKey == 0 {
		s.nextKey = 1
	}
	k := s.nextKey
	s.nextKey++
	return k
}

// findDuplicate returns the existing record rec duplicates, if any.
func (s *Store) findDuplicate(rec *Record, vec []float32) (*entry, float64) {
	gk := groupKey(rec)
	if id, ok := s.exact[gk+"\x00"+normalizedKey(rec.Text)]; ok {
		if e := s.byID[id]; e != nil {
			return e, float64(dot(vec, e.vec))
		}
	}
	if s.opts.DedupeThreshold < 0 {
		return nil, 0
	}
	now := s.now()
	same := func(e *entry) bool { return groupKey(&e.rec) == gk && !e.rec.Expired(now) }
	var best *entry
	bestSim := float32(-2)
	if len(s.byID) <= s.opts.Recall.ExactThreshold {
		for _, e := range s.byID {
			if same(e) {
				if d := dot(vec, e.vec); d > bestSim || (d == bestSim && best != nil && e.rec.ID < best.rec.ID) {
					best, bestSim = e, d
				}
			}
		}
	} else {
		res := s.hnsw.Search(vec, 1, 0, func(key uint64) bool {
			e := s.byKey[key]
			return e != nil && same(e)
		})
		if len(res) == 1 {
			best, bestSim = s.byKey[res[0].Key], 1-res[0].Distance
		}
	}
	if best != nil && float64(bestSim) >= s.opts.DedupeThreshold {
		return best, float64(bestSim)
	}
	return nil, 0
}

// mergeRecords folds the newer near-duplicate n into the existing old.
func mergeRecords(old, n Record, now time.Time) Record {
	m := old
	m.Text = n.Text
	for _, t := range n.Tags {
		if !slices.Contains(m.Tags, t) && len(m.Tags) < maxTags {
			m.Tags = append(m.Tags, t)
		}
	}
	m.Importance = max(old.Importance, n.Importance)
	if old.TTL == 0 || n.TTL == 0 {
		m.TTL = 0
	} else {
		m.TTL = max(old.TTL, n.TTL)
	}
	if m.Source == "" {
		m.Source = n.Source
	}
	m.Updated, m.LastAccess = now.UTC(), now.UTC()
	m.Reinforced++
	return m
}

// enforceBudgetLocked expires and evicts records until the store is within
// budget, logging the deletions through b. keep is never evicted.
func (s *Store) enforceBudgetLocked(b *walBatch, keep string) []string {
	if len(s.byID) <= s.opts.MaxRecords && s.textBytes <= s.opts.MaxBytes {
		return nil
	}
	now := s.now()
	var expired []string
	for id, e := range s.byID {
		if e.rec.Expired(now) && id != keep {
			expired = append(expired, id)
		}
	}
	slices.Sort(expired)
	b.del(expired...)
	if err := b.commit(); err != nil {
		s.log.Warn("memory: logging expirations failed", "err", err)
		return nil
	}
	s.stats.expired.Add(int64(len(expired)))
	if len(s.byID) <= s.opts.MaxRecords && s.textBytes <= s.opts.MaxBytes {
		return expired
	}
	// Evict down to a low-water mark so the sort is amortised over many
	// subsequent inserts.
	targetN := s.opts.MaxRecords - s.opts.MaxRecords/20
	targetB := s.opts.MaxBytes - s.opts.MaxBytes/20
	type cand struct {
		id    string
		score int64
		size  int64
	}
	protect := float64(s.opts.ImportanceProtection.Nanoseconds())
	var cands []cand
	for id, e := range s.byID {
		if id == keep || e.rec.Pinned() {
			continue
		}
		cands = append(cands, cand{id, e.lastAccess.Load() + int64(e.rec.Importance*protect), int64(len(e.rec.Text))})
	}
	slices.SortFunc(cands, func(a, b cand) int {
		if a.score != b.score {
			if a.score < b.score {
				return -1
			}
			return 1
		}
		return strings.Compare(a.id, b.id)
	})
	n, bytes := len(s.byID), s.textBytes
	var evict []string
	for _, c := range cands {
		if n <= targetN && bytes <= targetB {
			break
		}
		evict = append(evict, c.id)
		n--
		bytes -= c.size
	}
	b.del(evict...)
	if err := b.commit(); err != nil {
		s.log.Warn("memory: logging evictions failed", "err", err)
		return expired
	}
	s.stats.evicted.Add(int64(len(evict)))
	return append(expired, evict...)
}

// Forget deletes a record by ID.
func (s *Store) Forget(id string) error {
	return s.ForgetContext(context.Background(), id)
}

// ForgetContext is Forget with a context bounding the lock wait.
func (s *Store) ForgetContext(ctx context.Context, id string) error {
	err := s.write(ctx, func(b *walBatch) error {
		if s.byID[id] == nil {
			return fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		b.del(id)
		return nil
	})
	if err == nil {
		s.stats.forgotten.Add(1)
		s.maybeRebuild()
	}
	return err
}

// ForgetWhere deletes every record matching f (expired ones included) and
// returns how many were deleted.
func (s *Store) ForgetWhere(ctx context.Context, f Filter) (int, error) {
	f.IncludeExpired = true
	n := 0
	err := s.write(ctx, func(b *walBatch) error {
		cf := f.compile(s.now())
		var ids []string
		for id, e := range s.byID {
			if cf.match(&e.rec) {
				ids = append(ids, id)
			}
		}
		slices.Sort(ids)
		b.del(ids...)
		n = len(ids)
		return nil
	})
	if err != nil {
		return 0, err
	}
	s.stats.forgotten.Add(int64(n))
	s.maybeRebuild()
	return n, nil
}

// Get returns a record by ID.
func (s *Store) Get(id string) (Record, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e := s.byID[id]
	if e == nil {
		return Record{}, false
	}
	return e.record(), true
}

// List returns records matching f, most recently updated first; limit <= 0
// returns all.
func (s *Store) List(f Filter, limit int) []Record {
	s.mu.RLock()
	cf := f.compile(s.now())
	var out []Record
	for _, e := range s.byID {
		if cf.match(&e.rec) {
			out = append(out, e.record())
		}
	}
	s.mu.RUnlock()
	slices.SortFunc(out, func(a, b Record) int {
		if c := b.Updated.Compare(a.Updated); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Len is the number of live records.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.byID)
}

// maybeRebuild rebuilds the HNSW graph once tombstones pass RebuildRatio.
// The new graph is built from a snapshot without blocking readers or
// writers; changes made meanwhile are replayed onto it before the swap.
func (s *Store) maybeRebuild() {
	if !s.hnsw.NeedsRebuild(s.opts.RebuildRatio, 64) || !s.rebuilding.CompareAndSwap(false, true) {
		return
	}
	defer s.rebuilding.Store(false)
	s.rebuildGraph()
}

func (s *Store) rebuildGraph() {
	type kv struct {
		key uint64
		vec []float32
	}
	s.mu.RLock()
	snap := make([]kv, 0, len(s.byKey))
	for k, e := range s.byKey {
		snap = append(snap, kv{k, e.vec})
	}
	cfg := s.opts.HNSW
	cfg.Dim = s.dim
	s.mu.RUnlock()
	slices.SortFunc(snap, func(a, b kv) int {
		switch {
		case a.key < b.key:
			return -1
		case a.key > b.key:
			return 1
		}
		return 0
	})
	g, err := NewHNSW(cfg)
	if err != nil {
		s.log.Warn("memory: hnsw rebuild failed", "err", err)
		return
	}
	for _, x := range snap {
		_ = g.Insert(x.key, x.vec) // vectors are already validated
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	for _, x := range snap {
		if s.byKey[x.key] == nil {
			g.Delete(x.key)
		}
	}
	for k, e := range s.byKey {
		if !g.Contains(k) {
			_ = g.Insert(k, e.vec)
		}
	}
	s.hnsw = g
	s.stats.rebuilds.Add(1)
}

// Stats is a point-in-time summary of the store.
type Stats struct {
	Records        int
	ByKind         map[Kind]int
	ByScope        map[Scope]int
	Pinned         int
	Expired        int // expired but not yet pruned
	TextBytes      int64
	Dim            int
	Embedder       string
	GraphNodes     int
	GraphDeleted   int
	GraphMaxLevel  int
	LexicalTerms   int
	Persistent     bool
	Dir            string
	Generation     uint64
	SegmentBytes   int64
	LogBytes       int64
	PendingTouches int

	Remembered, Merged, Recalls, Forgotten, Expirations, Evictions int64
	Consolidated, Rebuilds, Compactions, Reloads                   int64
}

// Stats returns statistics.
func (s *Store) Stats() Stats {
	s.mu.RLock()
	now := s.now()
	st := Stats{
		Records: len(s.byID), ByKind: map[Kind]int{}, ByScope: map[Scope]int{},
		TextBytes: s.textBytes, Dim: s.dim, Embedder: s.emb.ID(),
		GraphNodes: s.hnsw.Len(), GraphDeleted: s.hnsw.Tombstones(), GraphMaxLevel: s.hnsw.MaxLevel(),
		LexicalTerms: s.bm25.Terms(), Persistent: s.disk != nil, Dir: s.opts.Dir,
	}
	for _, e := range s.byID {
		st.ByKind[e.rec.Kind]++
		st.ByScope[e.rec.Scope]++
		if e.rec.Pinned() {
			st.Pinned++
		}
		if e.rec.Expired(now) {
			st.Expired++
		}
	}
	if s.disk != nil {
		st.Generation = s.disk.gen
		st.SegmentBytes = s.disk.segSize
		st.LogBytes = s.disk.walSize
	}
	s.mu.RUnlock()
	s.touchMu.Lock()
	st.PendingTouches = len(s.touches)
	s.touchMu.Unlock()
	st.Remembered, st.Merged, st.Recalls = s.stats.remembered.Load(), s.stats.merged.Load(), s.stats.recalls.Load()
	st.Forgotten, st.Expirations, st.Evictions = s.stats.forgotten.Load(), s.stats.expired.Load(), s.stats.evicted.Load()
	st.Consolidated, st.Rebuilds = s.stats.consolidated.Load(), s.stats.rebuilds.Load()
	st.Compactions, st.Reloads = s.stats.compactions.Load(), s.stats.reloads.Load()
	return st
}

// MaintenanceReport says what Maintain did.
type MaintenanceReport struct {
	Expired   int
	Evicted   int
	Rebuilt   bool
	Compacted bool
}

// Maintain prunes expired records, enforces the budget, rebuilds the graph
// when tombstones warrant it and snapshots the store when force is set or
// the log is large. Call it at session end or on an idle timer.
func (s *Store) Maintain(ctx context.Context, force bool) (MaintenanceReport, error) {
	var rep MaintenanceReport
	before := s.stats.compactions.Load()
	err := s.write(ctx, func(b *walBatch) error {
		now := s.now()
		var ids []string
		for id, e := range s.byID {
			if e.rec.Expired(now) {
				ids = append(ids, id)
			}
		}
		slices.Sort(ids)
		b.del(ids...)
		if err := b.commit(); err != nil {
			return err
		}
		rep.Expired = len(ids)
		s.stats.expired.Add(int64(len(ids)))
		rep.Evicted = len(s.enforceBudgetLocked(b, ""))
		return nil
	})
	if err != nil {
		return rep, err
	}
	if s.hnsw.NeedsRebuild(s.opts.RebuildRatio, 1) && s.rebuilding.CompareAndSwap(false, true) {
		s.rebuildGraph()
		s.rebuilding.Store(false)
		rep.Rebuilt = true
	}
	if force && s.disk != nil {
		if err := s.Compact(ctx); err != nil {
			return rep, err
		}
	}
	rep.Compacted = s.stats.compactions.Load() > before
	return rep, nil
}

// Flush writes buffered access-time updates to the log.
func (s *Store) Flush(ctx context.Context) error {
	s.touchMu.Lock()
	empty := len(s.touches) == 0
	s.touchMu.Unlock()
	if empty || s.disk == nil {
		return nil
	}
	return s.write(ctx, func(*walBatch) error { return nil })
}

// Close flushes pending updates, snapshots the store when the log holds
// changes and releases its files. The store cannot be used afterwards.
func (s *Store) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), s.opts.LockTimeout)
	defer cancel()
	var errs []error
	if s.disk != nil {
		if err := s.Flush(ctx); err != nil && !errors.Is(err, ErrClosed) {
			errs = append(errs, err)
		}
		s.mu.RLock()
		dirty := !s.closed && s.disk.walSize > walHeaderSize
		s.mu.RUnlock()
		if dirty {
			if err := s.Compact(ctx); err != nil && !errors.Is(err, ErrClosed) {
				errs = append(errs, err)
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.Join(errs...)
	}
	s.closed = true
	if s.disk != nil {
		errs = append(errs, s.disk.close())
	}
	return errors.Join(errs...)
}
