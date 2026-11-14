package collab

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Dot identifies one operation: the replica that issued it and that
// replica's dense, 1-based operation counter. A replica's operations are
// numbered 1, 2, 3, ... with no gaps, which is what lets a version vector
// summarise "everything seen" in one integer per replica.
type Dot struct {
	Replica string `json:"r"`
	Seq     uint64 `json:"s"`
}

// IsZero reports whether d is the zero Dot.
func (d Dot) IsZero() bool { return d.Replica == "" && d.Seq == 0 }

// String formats d as replica:seq.
func (d Dot) String() string { return fmt.Sprintf("%s:%d", d.Replica, d.Seq) }

// Ordering is the causal relation between two version vectors.
type Ordering int

// Causal orderings returned by VersionVector.Compare.
const (
	Equal Ordering = iota
	Before
	After
	Concurrent
)

func (o Ordering) String() string {
	switch o {
	case Equal:
		return "equal"
	case Before:
		return "before"
	case After:
		return "after"
	default:
		return "concurrent"
	}
}

// VersionVector maps a replica to the highest contiguous operation seen
// from it. A missing entry means zero. The zero value (nil) is a valid,
// empty, read-only vector; use Clone or make before writing.
type VersionVector map[string]uint64

// Get returns the counter for replica r.
func (v VersionVector) Get(r string) uint64 { return v[r] }

// Contains reports whether the operation d is covered by v.
func (v VersionVector) Contains(d Dot) bool { return d.Seq != 0 && v[d.Replica] >= d.Seq }

// Clone returns an independent copy of v without zero entries.
func (v VersionVector) Clone() VersionVector {
	out := make(VersionVector, len(v))
	for r, n := range v {
		if n != 0 {
			out[r] = n
		}
	}
	return out
}

// Merge raises v to the pointwise maximum of v and o. v must be non-nil.
func (v VersionVector) Merge(o VersionVector) {
	for r, n := range o {
		if n > v[r] {
			v[r] = n
		}
	}
}

// Dominates reports whether v >= o pointwise.
func (v VersionVector) Dominates(o VersionVector) bool {
	for r, n := range o {
		if v[r] < n {
			return false
		}
	}
	return true
}

// Compare returns the causal relation of v to o.
func (v VersionVector) Compare(o VersionVector) Ordering {
	ge, le := v.Dominates(o), o.Dominates(v)
	switch {
	case ge && le:
		return Equal
	case le:
		return Before
	case ge:
		return After
	default:
		return Concurrent
	}
}

// Missing counts the operations v has that o does not.
func (v VersionVector) Missing(o VersionVector) uint64 {
	var n uint64
	for r, c := range v {
		if have := o[r]; c > have {
			n += c - have
		}
	}
	return n
}

// Total is the number of operations v covers.
func (v VersionVector) Total() uint64 {
	var n uint64
	for _, c := range v {
		n += c
	}
	return n
}

// String formats v deterministically, sorted by replica.
func (v VersionVector) String() string {
	keys := slices.Sorted(maps.Keys(v))
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%s:%d", k, v[k])
	}
	b.WriteByte('}')
	return b.String()
}

// Meet returns the pointwise minimum of the given vectors, treating a
// missing entry as zero. With no arguments it returns an empty vector. The
// meet of every replica's vector is the causal-stability frontier: every
// operation it covers has been seen by all of them.
func Meet(vs ...VersionVector) VersionVector {
	out := VersionVector{}
	if len(vs) == 0 {
		return out
	}
	for r, n := range vs[0] {
		m := n
		for _, o := range vs[1:] {
			m = min(m, o[r])
		}
		if m > 0 {
			out[r] = m
		}
	}
	return out
}
