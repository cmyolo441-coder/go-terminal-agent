package codeintel

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
)

// The cache payload is a compact binary encoding of cachePayload. Every
// string is stored once in a table and referred to by index, and the
// variable-length parts of all entries (chunk terms and frequencies,
// symbols, references) are decoded into a few shared arrays, so loading a
// large index costs a handful of allocations instead of one per string.
//
//	strings:  count, count lengths, the concatenated bytes
//	header:   root, key, savedAt, truncate, skipped counts
//	totals:   files, symbols, chunks, chunk terms, refs, path terms
//	entries:  one record per file (see encodeEntry)
//
// All integers are uvarints (signed ones zig-zag encoded) and strings are
// table indexes.

var errCacheFormat = errors.New("malformed cache payload")

type cacheEncoder struct {
	ids  map[string]uint64
	strs []string
	body []byte
}

func (e *cacheEncoder) uint(v uint64) { e.body = binary.AppendUvarint(e.body, v) }
func (e *cacheEncoder) int(v int64)   { e.body = binary.AppendVarint(e.body, v) }

func (e *cacheEncoder) bool(b bool) {
	if b {
		e.uint(1)
	} else {
		e.uint(0)
	}
}

func (e *cacheEncoder) str(s string) {
	id, ok := e.ids[s]
	if !ok {
		id = uint64(len(e.strs))
		e.ids[s] = id
		e.strs = append(e.strs, s)
	}
	e.uint(id)
}

// freq writes a term frequency. Field weights are multiples of 0.5, so a
// frequency is almost always a small multiple of 0.5 and fits a varint;
// anything else is written as raw float bits.
func (e *cacheEncoder) freq(f float32) {
	if h := f * 2; h >= 0 && h < 1<<24 && h == float32(uint32(h)) && !math.Signbit(float64(f)) {
		e.uint(uint64(uint32(h)) << 1)
		return
	}
	e.uint(uint64(math.Float32bits(f))<<1 | 1)
}

// encodeCache encodes a payload.
func encodeCache(cp *cachePayload) []byte {
	e := &cacheEncoder{ids: map[string]uint64{}}
	e.str(cp.Root)
	e.str(cp.Key)
	e.int(cp.SavedAt)
	e.bool(cp.Truncate)
	keys := make([]string, 0, len(cp.Skipped))
	for k := range cp.Skipped {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	e.uint(uint64(len(keys)))
	for _, k := range keys {
		e.str(k)
		e.int(int64(cp.Skipped[k]))
	}
	var nsym, nchunk, nterm, nref, npath int
	for _, f := range cp.Files {
		nsym += len(f.Symbols)
		nchunk += len(f.Chunks)
		for i := range f.Chunks {
			nterm += len(f.Chunks[i].Terms)
		}
		nref += len(f.Refs)
		npath += len(f.PathTerms)
	}
	for _, n := range []int{len(cp.Files), nsym, nchunk, nterm, nref, npath} {
		e.uint(uint64(n))
	}
	for _, f := range cp.Files {
		e.entry(f)
	}
	out := make([]byte, 0, len(e.body)+16*len(e.strs))
	out = binary.AppendUvarint(out, uint64(len(e.strs)))
	for _, s := range e.strs {
		out = binary.AppendUvarint(out, uint64(len(s)))
	}
	for _, s := range e.strs {
		out = append(out, s...)
	}
	return append(out, e.body...)
}

func (e *cacheEncoder) entry(f *fileEntry) {
	e.str(f.Path)
	e.int(f.Size)
	e.int(f.ModTime)
	e.str(f.Lang)
	e.int(int64(f.Lines))
	e.str(f.Skip)
	e.bool(f.Racy)
	e.uint(uint64(len(f.Symbols)))
	for i := range f.Symbols {
		s := &f.Symbols[i]
		e.str(s.Name)
		e.str(string(s.Kind))
		e.str(s.Container)
		e.int(int64(s.Line))
		e.int(int64(s.EndLine))
		e.int(int64(s.DocLine))
		e.str(s.Signature)
		e.str(s.Doc)
		e.bool(s.Exported)
	}
	e.uint(uint64(len(f.Chunks)))
	for i := range f.Chunks {
		c := &f.Chunks[i]
		e.int(int64(c.Start))
		e.int(int64(c.End))
		e.int(int64(c.Symbol))
		e.body = binary.LittleEndian.AppendUint32(e.body, math.Float32bits(c.Length))
		e.uint(uint64(len(c.Terms)))
		for k, t := range c.Terms {
			e.str(t)
			var fr float32
			if k < len(c.Freqs) {
				fr = c.Freqs[k]
			}
			e.freq(fr)
		}
	}
	e.uint(uint64(len(f.Refs)))
	for _, r := range f.Refs {
		e.str(r)
	}
	e.uint(uint64(len(f.PathTerms)))
	for _, t := range f.PathTerms {
		e.str(t)
	}
}

type cacheDecoder struct {
	buf  []byte
	strs []string
	err  error
	// intern canonicalises term strings; interned marks the table entries
	// it has been applied to.
	intern   func(string) string
	interned []bool
}

func (d *cacheDecoder) fail() {
	if d.err == nil {
		d.err = errCacheFormat
	}
}

func (d *cacheDecoder) uint() uint64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Uvarint(d.buf)
	if n <= 0 {
		d.fail()
		return 0
	}
	d.buf = d.buf[n:]
	return v
}

func (d *cacheDecoder) int() int64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Varint(d.buf)
	if n <= 0 {
		d.fail()
		return 0
	}
	d.buf = d.buf[n:]
	return v
}

// small reads an int that must fit in an int32-sized range.
func (d *cacheDecoder) small() int {
	v := d.int()
	if v < math.MinInt32 || v > math.MaxInt32 {
		d.fail()
		return 0
	}
	return int(v)
}

// count reads a length that cannot exceed the bytes left, since every
// counted item takes at least one byte.
func (d *cacheDecoder) count() int {
	v := d.uint()
	if v > uint64(len(d.buf)) {
		d.fail()
		return 0
	}
	return int(v)
}

func (d *cacheDecoder) bool() bool { return d.uint() != 0 }

func (d *cacheDecoder) str() string {
	id := d.uint()
	if id >= uint64(len(d.strs)) {
		d.fail()
		return ""
	}
	return d.strs[id]
}

// term reads a string that is a search term, interning it.
func (d *cacheDecoder) term() string {
	id := d.uint()
	if id >= uint64(len(d.strs)) {
		d.fail()
		return ""
	}
	if d.intern != nil && !d.interned[id] {
		d.interned[id] = true
		d.strs[id] = d.intern(d.strs[id])
	}
	return d.strs[id]
}

func (d *cacheDecoder) freq() float32 {
	v := d.uint()
	if v&1 == 0 {
		if v>>1 >= 1<<24 {
			d.fail()
			return 0
		}
		return float32(uint32(v>>1)) / 2
	}
	if v>>1 > math.MaxUint32 {
		d.fail()
		return 0
	}
	return math.Float32frombits(uint32(v >> 1))
}

func (d *cacheDecoder) float32() float32 {
	if d.err != nil {
		return 0
	}
	if len(d.buf) < 4 {
		d.fail()
		return 0
	}
	v := math.Float32frombits(binary.LittleEndian.Uint32(d.buf))
	d.buf = d.buf[4:]
	return v
}

// decodeCache decodes a payload. intern, when not nil, canonicalises the
// strings used as chunk and path terms.
func decodeCache(payload []byte, intern func(string) string) (*cachePayload, error) {
	d := &cacheDecoder{buf: payload, intern: intern}
	n := d.count()
	lens := make([]int, n)
	total := 0
	for i := range lens {
		l := d.count()
		lens[i] = l
		total += l
		if total > len(payload) {
			d.fail()
			break
		}
	}
	if d.err != nil || total > len(d.buf) {
		return nil, errCacheFormat
	}
	// One allocation holds every string.
	all := string(d.buf[:total])
	d.buf = d.buf[total:]
	d.strs = make([]string, n)
	off := 0
	for i, l := range lens {
		d.strs[i] = all[off : off+l]
		off += l
	}
	if intern != nil {
		d.interned = make([]bool, n)
	}

	cp := &cachePayload{}
	cp.Root = d.str()
	cp.Key = d.str()
	cp.SavedAt = d.int()
	cp.Truncate = d.bool()
	if ns := d.count(); ns > 0 {
		cp.Skipped = make(map[string]int, ns)
		for i := 0; i < ns && d.err == nil; i++ {
			k := d.str()
			cp.Skipped[k] = d.small()
		}
	}
	nfile, nsym, nchunk, nterm, nref, npath := d.count(), d.count(), d.count(), d.count(), d.count(), d.count()
	if d.err != nil {
		return nil, d.err
	}
	entries := make([]fileEntry, nfile)
	syms := make([]Symbol, nsym)
	chunks := make([]chunk, nchunk)
	terms := make([]string, nterm)
	freqs := make([]float32, nterm)
	refs := make([]string, nref)
	paths := make([]string, npath)
	// take carves the next n items off a shared array, with the capacity
	// clipped so an append never writes into a neighbour.
	takeErr := false
	take := func(have, n int, used *int) (int, int) {
		if n < 0 || n > have-*used {
			takeErr = true
			return 0, 0
		}
		lo := *used
		*used += n
		return lo, *used
	}
	var usedSym, usedChunk, usedTerm, usedRef, usedPath int
	cp.Files = make([]*fileEntry, nfile)
	for i := range entries {
		e := &entries[i]
		e.Path = d.str()
		e.Size = d.int()
		e.ModTime = d.int()
		e.Lang = d.str()
		e.Lines = d.small()
		e.Skip = d.str()
		e.Racy = d.bool()
		if lo, hi := take(nsym, d.count(), &usedSym); hi > lo {
			e.Symbols = syms[lo:hi:hi]
			for k := range e.Symbols {
				s := &e.Symbols[k]
				s.Name = d.str()
				s.Kind = SymbolKind(d.str())
				s.Container = d.str()
				s.Line = d.small()
				s.EndLine = d.small()
				s.DocLine = d.small()
				s.Signature = d.str()
				s.Doc = d.str()
				s.Exported = d.bool()
			}
		}
		if lo, hi := take(nchunk, d.count(), &usedChunk); hi > lo {
			e.Chunks = chunks[lo:hi:hi]
			for k := range e.Chunks {
				c := &e.Chunks[k]
				c.Start = d.small()
				c.End = d.small()
				c.Symbol = d.small()
				c.Length = d.float32()
				tlo, thi := take(nterm, d.count(), &usedTerm)
				c.Terms = terms[tlo:thi:thi]
				c.Freqs = freqs[tlo:thi:thi]
				for t := range c.Terms {
					c.Terms[t] = d.term()
					c.Freqs[t] = d.freq()
				}
			}
		}
		if lo, hi := take(nref, d.count(), &usedRef); hi > lo {
			e.Refs = refs[lo:hi:hi]
			for k := range e.Refs {
				e.Refs[k] = d.str()
			}
		}
		if lo, hi := take(npath, d.count(), &usedPath); hi > lo {
			e.PathTerms = paths[lo:hi:hi]
			for k := range e.PathTerms {
				e.PathTerms[k] = d.term()
			}
		}
		if d.err != nil || takeErr {
			return nil, errCacheFormat
		}
		cp.Files[i] = e
	}
	if len(d.buf) != 0 {
		return nil, fmt.Errorf("%w: %d trailing bytes", errCacheFormat, len(d.buf))
	}
	return cp, nil
}
