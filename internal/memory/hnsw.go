package memory

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"sync"
)

// HNSWConfig tunes an HNSW index. Zero fields take defaults.
type HNSWConfig struct {
	// Dim is the vector dimension (required).
	Dim int
	// M is the number of links per node on the upper layers; layer 0 keeps
	// up to 2*M (default 16).
	M int
	// EfConstruction is the candidate list size while inserting (default 200).
	EfConstruction int
	// EfSearch is the default candidate list size while searching (default 96).
	EfSearch int
	// Seed makes level generation reproducible; 0 picks a random seed.
	Seed uint64
}

// Neighbor is a search result: Distance is the cosine distance 1-cos.
type Neighbor struct {
	Key      uint64
	Distance float32
}

// HNSW is a Hierarchical Navigable Small World graph (Malkov & Yashunin,
// 2016) over unit vectors with cosine distance. Searches run concurrently
// under a read lock; inserts and deletes take the write lock. Deleted nodes
// become tombstones that still route searches but are never returned;
// Rebuild compacts them away.
type HNSW struct {
	mu       sync.RWMutex
	dim      int
	m        int
	mMax0    int
	efC      int
	efSearch int
	ml       float64
	rng      *rand.Rand

	vecs     []float32 // node i occupies vecs[i*dim : (i+1)*dim]
	nodes    []hnswNode
	byKey    map[uint64]uint32
	entry    int32 // -1 when empty
	maxLevel int
	deleted  int

	visitPool sync.Pool // *visitSet
}

type hnswNode struct {
	key     uint64
	deleted bool
	links   [][]uint32 // links[l] for l in 0..level
}

const maxHNSWLevel = 24

// NewHNSW returns an empty index.
func NewHNSW(cfg HNSWConfig) (*HNSW, error) {
	if cfg.Dim <= 0 || cfg.Dim > 1<<16 {
		return nil, fmt.Errorf("memory: hnsw dim %d out of range", cfg.Dim)
	}
	if cfg.M == 0 {
		cfg.M = 16
	}
	if cfg.M < 2 || cfg.M > 256 {
		return nil, fmt.Errorf("memory: hnsw M %d out of range [2, 256]", cfg.M)
	}
	if cfg.EfConstruction == 0 {
		cfg.EfConstruction = 200
	}
	if cfg.EfSearch == 0 {
		cfg.EfSearch = 96
	}
	if cfg.EfConstruction < cfg.M || cfg.EfSearch < 1 {
		return nil, fmt.Errorf("memory: hnsw efConstruction %d / efSearch %d too small", cfg.EfConstruction, cfg.EfSearch)
	}
	seed := cfg.Seed
	if seed == 0 {
		seed = rand.Uint64()
	}
	h := &HNSW{
		dim: cfg.Dim, m: cfg.M, mMax0: 2 * cfg.M, efC: cfg.EfConstruction, efSearch: cfg.EfSearch,
		ml:    1 / math.Log(float64(cfg.M)),
		rng:   rand.New(rand.NewPCG(seed, seed^0x6a09e667f3bcc909)),
		byKey: map[uint64]uint32{},
		entry: -1,
	}
	h.visitPool.New = func() any { return &visitSet{} }
	return h, nil
}

// Dim is the vector dimension.
func (h *HNSW) Dim() int { return h.dim }

// Len is the number of live (non-deleted) vectors.
func (h *HNSW) Len() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.byKey)
}

// Tombstones is the number of deleted nodes still in the graph.
func (h *HNSW) Tombstones() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.deleted
}

// MaxLevel is the top layer of the graph (-1 when empty).
func (h *HNSW) MaxLevel() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.entry < 0 {
		return -1
	}
	return h.maxLevel
}

// SetEfSearch changes the default search candidate list size.
func (h *HNSW) SetEfSearch(ef int) {
	h.mu.Lock()
	h.efSearch = max(ef, 1)
	h.mu.Unlock()
}

// Contains reports whether key is live.
func (h *HNSW) Contains(key uint64) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.byKey[key]
	return ok
}

// Vector returns a copy of the stored (unit) vector of key.
func (h *HNSW) Vector(key uint64) ([]float32, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	id, ok := h.byKey[key]
	if !ok {
		return nil, false
	}
	return slices.Clone(h.vec(id)), true
}

func (h *HNSW) vec(id uint32) []float32 {
	off := int(id) * h.dim
	return h.vecs[off : off+h.dim : off+h.dim]
}

func (h *HNSW) dist(q []float32, id uint32) float32 { return cosineDistance(q, h.vec(id)) }

func (h *HNSW) maxConn(level int) int {
	if level == 0 {
		return h.mMax0
	}
	return h.m
}

func (h *HNSW) randomLevel() int {
	u := h.rng.Float64()
	for u == 0 {
		u = h.rng.Float64()
	}
	return min(int(-math.Log(u)*h.ml), maxHNSWLevel)
}

// ErrDimension is returned for vectors of the wrong size or zero length.
var ErrDimension = errors.New("memory: vector has the wrong dimension or zero length")

// Insert adds vec under key, replacing any live vector with the same key.
// The vector is copied and normalised.
func (h *HNSW) Insert(key uint64, vec []float32) error {
	if len(vec) != h.dim {
		return ErrDimension
	}
	q := normalized(vec)
	if q == nil {
		return ErrDimension
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.insertLocked(key, q, h.randomLevel())
	return nil
}

func (h *HNSW) insertLocked(key uint64, q []float32, level int) {
	if old, ok := h.byKey[key]; ok {
		h.nodes[old].deleted = true
		h.deleted++
	}
	id := uint32(len(h.nodes))
	h.vecs = append(h.vecs, q...)
	q = h.vec(id)
	n := hnswNode{key: key, links: make([][]uint32, level+1)}
	for l := range n.links {
		n.links[l] = make([]uint32, 0, h.maxConn(l)+1)
	}
	h.nodes = append(h.nodes, n)
	h.byKey[key] = id
	if h.entry < 0 {
		h.entry, h.maxLevel = int32(id), level
		return
	}

	vs := h.getVisit()
	defer h.visitPool.Put(vs)

	ep := []candidate{{id: uint32(h.entry), d: h.dist(q, uint32(h.entry))}}
	for l := h.maxLevel; l > level; l-- {
		ep[0] = h.greedy(q, ep[0], l)
	}
	for l := min(level, h.maxLevel); l >= 0; l-- {
		vs.reset(len(h.nodes))
		w := h.searchLayer(q, ep, h.efC, l, vs, nil)
		cands := w.sortedAsc()
		neighbours := h.selectNeighbours(cands, h.m, true)
		h.nodes[id].links[l] = append(h.nodes[id].links[l], neighbours...)
		for _, nb := range neighbours {
			h.link(nb, id, l)
		}
		ep = cands
	}
	if level > h.maxLevel {
		h.entry, h.maxLevel = int32(id), level
	}
}

// link adds a back-link from src to dst at level, pruning src's list with
// the selection heuristic when it overflows.
func (h *HNSW) link(src, dst uint32, level int) {
	links := h.nodes[src].links[level]
	if slices.Contains(links, dst) {
		return
	}
	limit := h.maxConn(level)
	if len(links) < limit {
		h.nodes[src].links[level] = append(links, dst)
		return
	}
	sv := h.vec(src)
	cands := make([]candidate, 0, len(links)+1)
	for _, nb := range links {
		cands = append(cands, candidate{id: nb, d: cosineDistance(sv, h.vec(nb))})
	}
	cands = append(cands, candidate{id: dst, d: cosineDistance(sv, h.vec(dst))})
	slices.SortFunc(cands, cmpCandidate)
	sel := h.selectNeighbours(cands, limit, true)
	h.nodes[src].links[level] = append(links[:0], sel...)
}

// selectNeighbours is the heuristic of Algorithm 4 in the HNSW paper: a
// candidate is kept only when it is closer to the base than to every
// neighbour already kept, which spreads links across directions and keeps
// the graph navigable on clustered data. With keepPruned the remaining
// slots are filled with the closest discarded candidates. cands must be
// sorted by ascending distance; tombstones are used only as a last resort.
func (h *HNSW) selectNeighbours(cands []candidate, m int, keepPruned bool) []uint32 {
	out := make([]uint32, 0, m)
	var pruned, dead []uint32
	for _, c := range cands {
		if len(out) >= m {
			break
		}
		if h.nodes[c.id].deleted {
			dead = append(dead, c.id)
			continue
		}
		cv := h.vec(c.id)
		good := true
		for _, r := range out {
			if cosineDistance(cv, h.vec(r)) < c.d {
				good = false
				break
			}
		}
		if good {
			out = append(out, c.id)
		} else if keepPruned {
			pruned = append(pruned, c.id)
		}
	}
	for _, p := range pruned {
		if len(out) >= m {
			break
		}
		out = append(out, p)
	}
	for _, p := range dead {
		if len(out) >= m || len(out) > 0 {
			break
		}
		out = append(out, p)
	}
	return out
}

// greedy walks level l towards q from ep and returns the local minimum.
func (h *HNSW) greedy(q []float32, ep candidate, l int) candidate {
	for changed := true; changed; {
		changed = false
		for _, nb := range h.nodes[ep.id].links[l] {
			if d := h.dist(q, nb); d < ep.d {
				ep, changed = candidate{id: nb, d: d}, true
			}
		}
	}
	return ep
}

// searchLayer is Algorithm 2 of the paper. Every reachable node is
// traversed, but only nodes for which accept returns true (all when nil)
// enter the result set, which therefore holds the ef best accepted nodes.
func (h *HNSW) searchLayer(q []float32, eps []candidate, ef, level int, vs *visitSet, accept func(uint32) bool) *maxHeap {
	cand := &minHeap{}
	res := &maxHeap{}
	for _, e := range eps {
		if vs.visit(e.id) {
			continue
		}
		cand.push(e)
		if accept == nil || accept(e.id) {
			res.push(e)
			if res.len() > ef {
				res.pop()
			}
		}
	}
	for cand.len() > 0 {
		c := cand.pop()
		if res.len() >= ef && c.d > res.top().d {
			break
		}
		for _, nb := range h.nodes[c.id].links[level] {
			if vs.visit(nb) {
				continue
			}
			d := h.dist(q, nb)
			if res.len() < ef || d < res.top().d {
				cand.push(candidate{id: nb, d: d})
				if accept == nil || accept(nb) {
					res.push(candidate{id: nb, d: d})
					if res.len() > ef {
						res.pop()
					}
				}
			}
		}
	}
	return res
}

// Search returns up to k live vectors nearest to q, closest first. ef is
// the candidate list size (0 selects the configured default; it is raised
// to k). accept, when non-nil, filters keys during the search so that k
// accepted results are found when they are reachable.
func (h *HNSW) Search(q []float32, k, ef int, accept func(key uint64) bool) []Neighbor {
	if k <= 0 || len(q) != h.dim {
		return nil
	}
	qn := normalized(q)
	if qn == nil {
		return nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.entry < 0 {
		return nil
	}
	if ef <= 0 {
		ef = h.efSearch
	}
	ef = max(ef, k)
	ep := candidate{id: uint32(h.entry), d: h.dist(qn, uint32(h.entry))}
	for l := h.maxLevel; l > 0; l-- {
		ep = h.greedy(qn, ep, l)
	}
	vs := h.getVisit()
	defer h.visitPool.Put(vs)
	vs.reset(len(h.nodes))
	res := h.searchLayer(qn, []candidate{ep}, ef, 0, vs, func(id uint32) bool {
		n := &h.nodes[id]
		return !n.deleted && (accept == nil || accept(n.key))
	})
	sorted := res.sortedAsc()
	if len(sorted) > k {
		sorted = sorted[:k]
	}
	out := make([]Neighbor, len(sorted))
	for i, c := range sorted {
		out[i] = Neighbor{Key: h.nodes[c.id].key, Distance: c.d}
	}
	return out
}

// Delete tombstones key and reports whether it was live.
func (h *HNSW) Delete(key uint64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	id, ok := h.byKey[key]
	if !ok {
		return false
	}
	delete(h.byKey, key)
	h.nodes[id].deleted = true
	h.deleted++
	return true
}

// NeedsRebuild reports whether tombstones make up more than ratio of the
// graph (and at least minDead nodes).
func (h *HNSW) NeedsRebuild(ratio float64, minDead int) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.deleted >= minDead && float64(h.deleted) > ratio*float64(len(h.nodes))
}

// Rebuild rebuilds the graph from the live vectors, dropping tombstones.
// Searches wait while it runs.
func (h *HNSW) Rebuild() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.deleted == 0 {
		return
	}
	oldNodes, oldVecs, dim := h.nodes, h.vecs, h.dim
	live := len(h.byKey)
	h.nodes = make([]hnswNode, 0, live)
	h.vecs = make([]float32, 0, live*dim)
	h.byKey = make(map[uint64]uint32, live)
	h.entry, h.maxLevel, h.deleted = -1, 0, 0
	for i := range oldNodes {
		n := &oldNodes[i]
		if n.deleted {
			continue
		}
		// Keep each node's level: it was drawn from the right distribution
		// and reusing it keeps the hierarchy shape stable.
		h.insertLocked(n.key, oldVecs[i*dim:(i+1)*dim], len(n.links)-1)
	}
}

// ---- persistence -------------------------------------------------------

const hnswFormat = 1

// appendBinary appends the graph to buf. Live nodes are stored without
// their vectors, which the owner persists alongside its records and hands
// back to loadBinary; tombstoned nodes still route searches, so their
// vectors are stored inline.
func (h *HNSW) appendBinary(buf []byte) []byte {
	h.mu.RLock()
	defer h.mu.RUnlock()
	buf = binary.AppendUvarint(buf, hnswFormat)
	buf = binary.AppendUvarint(buf, uint64(h.dim))
	buf = binary.AppendUvarint(buf, uint64(h.m))
	buf = binary.AppendUvarint(buf, uint64(len(h.nodes)))
	buf = binary.AppendUvarint(buf, uint64(h.entry+1))
	buf = binary.AppendUvarint(buf, uint64(h.maxLevel))
	for i := range h.nodes {
		n := &h.nodes[i]
		buf = binary.AppendUvarint(buf, n.key)
		flags := byte(0)
		if n.deleted {
			flags = 1
		}
		buf = append(buf, flags)
		buf = binary.AppendUvarint(buf, uint64(len(n.links)-1))
		for _, ls := range n.links {
			buf = binary.AppendUvarint(buf, uint64(len(ls)))
			for _, l := range ls {
				buf = binary.AppendUvarint(buf, uint64(l))
			}
		}
		if n.deleted {
			buf = appendVector(buf, h.vec(uint32(i)))
		}
	}
	return buf
}

func appendVector(buf []byte, v []float32) []byte {
	for _, x := range v {
		buf = binary.LittleEndian.AppendUint32(buf, math.Float32bits(x))
	}
	return buf
}

func decodeVector(raw []byte, dim int) []float32 {
	v := make([]float32, dim)
	for j := range v {
		v[j] = math.Float32frombits(binary.LittleEndian.Uint32(raw[4*j:]))
	}
	return v
}

// loadBinary replaces the graph with one decoded from data. The M of the
// encoded graph must match the index configuration. vecOf supplies the
// vector of every live key; a missing or malformed vector is ErrCorrupt.
func (h *HNSW) loadBinary(data []byte, vecOf func(key uint64) []float32) error {
	r := byteReader{b: data}
	if r.uvarint() != hnswFormat {
		return fmt.Errorf("%w: unknown hnsw format", ErrCorrupt)
	}
	dim, m := int(r.uvarint()), int(r.uvarint())
	count := r.uvarint()
	entry := int64(r.uvarint()) - 1
	maxLevel := int(r.uvarint())
	if r.err != nil {
		return r.err
	}
	if dim != h.dim || m != h.m {
		return fmt.Errorf("%w: hnsw graph has dim %d M %d, index has dim %d M %d", ErrCorrupt, dim, m, h.dim, h.m)
	}
	if count > uint64(len(data)) || entry >= int64(count) || maxLevel > maxHNSWLevel || (count > 0) != (entry >= 0) {
		return fmt.Errorf("%w: hnsw header out of range", ErrCorrupt)
	}
	nodes := make([]hnswNode, count)
	vecs := make([]float32, 0, int(count)*dim)
	byKey := make(map[uint64]uint32, count)
	deleted := 0
	for i := range nodes {
		n := &nodes[i]
		n.key = r.uvarint()
		flags := r.byte()
		level := r.uvarint()
		if r.err != nil || level > maxHNSWLevel || flags > 1 {
			return fmt.Errorf("%w: hnsw node %d", ErrCorrupt, i)
		}
		n.deleted = flags == 1
		n.links = make([][]uint32, level+1)
		for l := range n.links {
			cnt := r.uvarint()
			if cnt > uint64(h.maxConn(l)) {
				return fmt.Errorf("%w: hnsw node %d has %d links at level %d", ErrCorrupt, i, cnt, l)
			}
			ls := make([]uint32, cnt, h.maxConn(l)+1)
			for j := range ls {
				v := r.uvarint()
				if v >= count {
					return fmt.Errorf("%w: hnsw link out of range", ErrCorrupt)
				}
				ls[j] = uint32(v)
			}
			n.links[l] = ls
		}
		var v []float32
		if n.deleted {
			raw := r.bytes(4 * dim)
			if r.err != nil {
				return r.err
			}
			v = decodeVector(raw, dim)
		} else if v = vecOf(n.key); len(v) != dim {
			return fmt.Errorf("%w: no vector for hnsw key %d", ErrCorrupt, n.key)
		}
		vecs = append(vecs, v...)
		if n.deleted {
			deleted++
		} else {
			if _, dup := byKey[n.key]; dup {
				return fmt.Errorf("%w: hnsw key %d appears twice", ErrCorrupt, n.key)
			}
			byKey[n.key] = uint32(i)
		}
	}
	if r.err != nil {
		return r.err
	}
	if len(r.b) != r.off {
		return fmt.Errorf("%w: trailing bytes after hnsw graph", ErrCorrupt)
	}
	// Links may only point to nodes that exist on that level.
	for i := range nodes {
		for l, ls := range nodes[i].links {
			for _, nb := range ls {
				if len(nodes[nb].links) <= l {
					return fmt.Errorf("%w: hnsw link to a node below level %d", ErrCorrupt, l)
				}
			}
		}
	}
	if entry >= 0 && len(nodes[entry].links)-1 != maxLevel {
		return fmt.Errorf("%w: hnsw entry point level mismatch", ErrCorrupt)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nodes, h.vecs, h.byKey = nodes, vecs, byKey
	h.entry, h.maxLevel, h.deleted = int32(entry), maxLevel, deleted
	return nil
}

// ---- search scaffolding -------------------------------------------------

type candidate struct {
	d  float32
	id uint32
}

func cmpCandidate(a, b candidate) int {
	switch {
	case a.d < b.d:
		return -1
	case a.d > b.d:
		return 1
	case a.id < b.id:
		return -1
	case a.id > b.id:
		return 1
	}
	return 0
}

// minHeap pops the closest candidate first.
type minHeap struct{ s []candidate }

func (h *minHeap) len() int { return len(h.s) }

func (h *minHeap) push(c candidate) {
	h.s = append(h.s, c)
	i := len(h.s) - 1
	for i > 0 {
		p := (i - 1) / 2
		if h.s[p].d <= h.s[i].d {
			break
		}
		h.s[p], h.s[i] = h.s[i], h.s[p]
		i = p
	}
}

func (h *minHeap) pop() candidate {
	top := h.s[0]
	last := len(h.s) - 1
	h.s[0] = h.s[last]
	h.s = h.s[:last]
	i := 0
	for {
		l, r, m := 2*i+1, 2*i+2, i
		if l < last && h.s[l].d < h.s[m].d {
			m = l
		}
		if r < last && h.s[r].d < h.s[m].d {
			m = r
		}
		if m == i {
			break
		}
		h.s[m], h.s[i] = h.s[i], h.s[m]
		i = m
	}
	return top
}

// maxHeap keeps the farthest candidate on top, so a bounded result set can
// evict its worst member in O(log n).
type maxHeap struct{ s []candidate }

func (h *maxHeap) len() int { return len(h.s) }

func (h *maxHeap) top() candidate { return h.s[0] }

func (h *maxHeap) push(c candidate) {
	h.s = append(h.s, c)
	i := len(h.s) - 1
	for i > 0 {
		p := (i - 1) / 2
		if h.s[p].d >= h.s[i].d {
			break
		}
		h.s[p], h.s[i] = h.s[i], h.s[p]
		i = p
	}
}

func (h *maxHeap) pop() candidate {
	top := h.s[0]
	last := len(h.s) - 1
	h.s[0] = h.s[last]
	h.s = h.s[:last]
	i := 0
	for {
		l, r, m := 2*i+1, 2*i+2, i
		if l < last && h.s[l].d > h.s[m].d {
			m = l
		}
		if r < last && h.s[r].d > h.s[m].d {
			m = r
		}
		if m == i {
			break
		}
		h.s[m], h.s[i] = h.s[i], h.s[m]
		i = m
	}
	return top
}

// sortedAsc returns the heap contents ordered by ascending distance.
func (h *maxHeap) sortedAsc() []candidate {
	out := slices.Clone(h.s)
	slices.SortFunc(out, cmpCandidate)
	return out
}

// visitSet marks visited nodes with an epoch counter so it never needs
// clearing between searches (except on the rare epoch wrap-around).
type visitSet struct {
	marks []uint16
	epoch uint16
}

func (h *HNSW) getVisit() *visitSet { return h.visitPool.Get().(*visitSet) }

func (v *visitSet) reset(n int) {
	if cap(v.marks) < n {
		v.marks = make([]uint16, n+n/4+16)
		v.epoch = 0
	}
	v.marks = v.marks[:cap(v.marks)]
	v.epoch++
	if v.epoch == 0 {
		clear(v.marks)
		v.epoch = 1
	}
}

// visit marks id and reports whether it had already been visited.
func (v *visitSet) visit(id uint32) bool {
	if v.marks[id] == v.epoch {
		return true
	}
	v.marks[id] = v.epoch
	return false
}

// byteReader decodes the varint/byte primitives used by the binary formats,
// recording the first error instead of panicking on short input.
type byteReader struct {
	b   []byte
	off int
	err error
}

func (r *byteReader) fail() {
	if r.err == nil {
		r.err = fmt.Errorf("%w: truncated or malformed data at offset %d", ErrCorrupt, r.off)
	}
}

func (r *byteReader) uvarint() uint64 {
	if r.err != nil {
		return 0
	}
	v, n := binary.Uvarint(r.b[r.off:])
	if n <= 0 {
		r.fail()
		return 0
	}
	r.off += n
	return v
}

func (r *byteReader) varint() int64 {
	if r.err != nil {
		return 0
	}
	v, n := binary.Varint(r.b[r.off:])
	if n <= 0 {
		r.fail()
		return 0
	}
	r.off += n
	return v
}

func (r *byteReader) byte() byte {
	if r.err != nil || r.off >= len(r.b) {
		r.fail()
		return 0
	}
	c := r.b[r.off]
	r.off++
	return c
}

func (r *byteReader) bytes(n int) []byte {
	if r.err != nil || n < 0 || n > len(r.b)-r.off {
		r.fail()
		return nil
	}
	s := r.b[r.off : r.off+n]
	r.off += n
	return s
}

func (r *byteReader) string() string {
	n := r.uvarint()
	if n > uint64(len(r.b)) {
		r.fail()
		return ""
	}
	return string(r.bytes(int(n)))
}
