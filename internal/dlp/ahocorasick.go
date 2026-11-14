package dlp

import (
	"errors"
	"fmt"
)

// AhoCorasick matches a set of byte patterns against a text in one pass,
// in time linear in the text plus the number of matches. It reports every
// occurrence of every pattern, including overlapping ones.
//
// The bytes of the alphabet are first reduced to equivalence classes (bytes
// no pattern uses share one class), then the automaton is compiled to a
// dense DFA table when that fits in the memory budget; very large pattern
// sets fall back to a goto/failure automaton. An AhoCorasick is immutable
// and safe for concurrent use.
type AhoCorasick struct {
	fold     bool
	classes  [256]uint16
	nclasses int

	// dfa[s*nclasses+c] is the next state's row offset (next*nclasses),
	// bit-inverted (negative) when that state emits; nil in fallback mode.
	dfa []int32

	// Trie edges for fallback mode, sorted by class.
	edges [][]acEdge
	fail  []int32

	// out[s] lists patterns ending exactly at state s; dict[s] is the
	// nearest state on the failure chain with a non-empty out, or -1.
	out  [][]int32
	dict []int32
	// emits[s] is true when s or its failure chain has outputs.
	emits []bool

	patLen []int
}

type acEdge struct {
	class uint16
	next  int32
}

// ACOptions configures an automaton.
type ACOptions struct {
	// CaseInsensitive folds ASCII letters. Non-ASCII bytes always match
	// exactly.
	CaseInsensitive bool
	// MaxDFAEntries bounds the dense transition table (states × classes).
	// Zero means 1<<22 (16 MiB).
	MaxDFAEntries int
}

// ACMatch is one occurrence: text[Start:End] matches pattern Pattern.
type ACMatch struct {
	Pattern    int
	Start, End int
}

// ErrEmptyPattern is returned for an empty pattern, which would match at
// every position.
var ErrEmptyPattern = errors.New("dlp: empty Aho-Corasick pattern")

func foldByte(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 32
	}
	return c
}

// NewAhoCorasick compiles the patterns. Duplicate patterns are allowed;
// each is reported under its own index.
func NewAhoCorasick(patterns []string, opts ACOptions) (*AhoCorasick, error) {
	a := &AhoCorasick{fold: opts.CaseInsensitive, patLen: make([]int, len(patterns))}
	// Alphabet reduction.
	var used [256]bool
	for i, p := range patterns {
		if p == "" {
			return nil, fmt.Errorf("%w (index %d)", ErrEmptyPattern, i)
		}
		for j := 0; j < len(p); j++ {
			c := p[j]
			if a.fold {
				c = foldByte(c)
			}
			used[c] = true
		}
		a.patLen[i] = len(p)
	}
	n := uint16(1)
	for c := 0; c < 256; c++ {
		if used[c] {
			a.classes[c] = n
			n++
		}
	}
	if a.fold {
		for c := 'A'; c <= 'Z'; c++ {
			a.classes[c] = a.classes[c+32]
		}
	}
	a.nclasses = int(n)

	// Trie.
	a.edges = [][]acEdge{nil}
	a.out = [][]int32{nil}
	for i, p := range patterns {
		s := int32(0)
		for j := 0; j < len(p); j++ {
			cl := a.classes[p[j]]
			next := a.child(s, cl)
			if next < 0 {
				next = int32(len(a.edges))
				a.edges = append(a.edges, nil)
				a.out = append(a.out, nil)
				a.insertEdge(s, cl, next)
			}
			s = next
		}
		a.out[s] = append(a.out[s], int32(i))
	}

	// Failure and dictionary links, breadth first.
	ns := len(a.edges)
	a.fail = make([]int32, ns)
	a.dict = make([]int32, ns)
	a.emits = make([]bool, ns)
	a.dict[0] = -1
	queue := make([]int32, 0, ns)
	for _, e := range a.edges[0] {
		a.fail[e.next] = 0
		queue = append(queue, e.next)
	}
	for qi := 0; qi < len(queue); qi++ {
		s := queue[qi]
		f := a.fail[s]
		if len(a.out[f]) > 0 {
			a.dict[s] = f
		} else {
			a.dict[s] = a.dict[f]
		}
		a.emits[s] = len(a.out[s]) > 0 || a.dict[s] >= 0
		for _, e := range a.edges[s] {
			t := a.fail[s]
			for {
				if nx := a.child(t, e.class); nx >= 0 {
					a.fail[e.next] = nx
					break
				}
				if t == 0 {
					a.fail[e.next] = 0
					break
				}
				t = a.fail[t]
			}
			queue = append(queue, e.next)
		}
	}

	limit := opts.MaxDFAEntries
	if limit <= 0 {
		limit = 1 << 22
	}
	if ns*a.nclasses <= limit {
		a.buildDFA(queue)
	}
	return a, nil
}

func (a *AhoCorasick) child(s int32, cl uint16) int32 {
	es := a.edges[s]
	lo, hi := 0, len(es)
	for lo < hi {
		m := (lo + hi) / 2
		if es[m].class < cl {
			lo = m + 1
		} else {
			hi = m
		}
	}
	if lo < len(es) && es[lo].class == cl {
		return es[lo].next
	}
	return -1
}

func (a *AhoCorasick) insertEdge(s int32, cl uint16, next int32) {
	es := a.edges[s]
	i := len(es)
	for i > 0 && es[i-1].class > cl {
		i--
	}
	es = append(es, acEdge{})
	copy(es[i+1:], es[i:])
	es[i] = acEdge{cl, next}
	a.edges[s] = es
}

// buildDFA fills the dense table in BFS order so a state's failure target
// is complete before the state itself.
func (a *AhoCorasick) buildDFA(bfs []int32) {
	nc := a.nclasses
	a.dfa = make([]int32, len(a.edges)*nc)
	for _, e := range a.edges[0] {
		a.dfa[int(e.class)] = e.next
	}
	for _, s := range bfs {
		row := a.dfa[int(s)*nc : int(s)*nc+nc]
		copy(row, a.dfa[int(a.fail[s])*nc:int(a.fail[s])*nc+nc])
		for _, e := range a.edges[s] {
			row[e.class] = e.next
		}
	}
	// Encode entries as row offsets with the emit flag in the sign.
	for i, nx := range a.dfa {
		v := nx * int32(nc)
		if a.emits[nx] {
			v = ^v
		}
		a.dfa[i] = v
	}
	a.edges = nil
}

// Len returns the number of patterns.
func (a *AhoCorasick) Len() int { return len(a.patLen) }

// States returns the number of automaton states.
func (a *AhoCorasick) States() int { return len(a.fail) }

// Dense reports whether the automaton runs from a dense DFA table.
func (a *AhoCorasick) Dense() bool { return a.dfa != nil }

// Iterate calls fn for every match in order of end offset (and, for equal
// ends, longest pattern first). Returning false stops the scan.
func (a *AhoCorasick) Iterate(text string, fn func(ACMatch) bool) {
	if len(a.patLen) == 0 {
		return
	}
	if a.dfa != nil {
		dfa, nc, cls := a.dfa, a.nclasses, &a.classes
		off := int32(0)
		for i := 0; i < len(text); i++ {
			x := dfa[int(off)+int(cls[text[i]])]
			if x >= 0 {
				off = x
				continue
			}
			off = ^x
			if !a.report(off/int32(nc), i+1, fn) {
				return
			}
		}
		return
	}
	s := int32(0)
	for i := 0; i < len(text); i++ {
		cl := a.classes[text[i]]
		for {
			if nx := a.child(s, cl); nx >= 0 {
				s = nx
				break
			}
			if s == 0 {
				break
			}
			s = a.fail[s]
		}
		if a.emits[s] && !a.report(s, i+1, fn) {
			return
		}
	}
}

func (a *AhoCorasick) report(s int32, end int, fn func(ACMatch) bool) bool {
	for t := s; t >= 0; t = a.dict[t] {
		for _, p := range a.out[t] {
			if !fn(ACMatch{Pattern: int(p), Start: end - a.patLen[p], End: end}) {
				return false
			}
		}
	}
	return true
}

// FindAll returns every match.
func (a *AhoCorasick) FindAll(text string) []ACMatch {
	var out []ACMatch
	a.Iterate(text, func(m ACMatch) bool {
		out = append(out, m)
		return true
	})
	return out
}

// Contains reports whether any pattern occurs in text.
func (a *AhoCorasick) Contains(text string) bool {
	found := false
	a.Iterate(text, func(ACMatch) bool {
		found = true
		return false
	})
	return found
}
