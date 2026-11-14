package collab

import (
	"fmt"
	"maps"
	"slices"
	"unicode/utf8"
)

// MaxElementBytes bounds an OR-Set element and an LWW-Map key.
const MaxElementBytes = 4096

// OR-Set operation kinds.
const (
	SetAdd    = "add"
	SetRemove = "rem"
)

// SetOp is one replicated OR-Set operation. An addition is tagged with its
// own id; a removal lists the addition tags it observed.
type SetOp struct {
	ID   Dot    `json:"id"`
	Kind string `json:"k"`
	Elem string `json:"e"`
	Tags []Dot  `json:"tags,omitempty"`
}

func (op SetOp) opDot() Dot { return op.ID }

// Validate checks the operation's shape.
func (op SetOp) Validate() error {
	if op.ID.Replica == "" || op.ID.Seq == 0 {
		return fmt.Errorf("%w: set operation without id", ErrInvalidOp)
	}
	if op.Elem == "" || len(op.Elem) > MaxElementBytes || !utf8.ValidString(op.Elem) {
		return fmt.Errorf("%w: set element must be 1..%d bytes of UTF-8", ErrInvalidOp, MaxElementBytes)
	}
	switch op.Kind {
	case SetAdd:
		if len(op.Tags) != 0 {
			return fmt.Errorf("%w: addition with tags", ErrInvalidOp)
		}
	case SetRemove:
		if len(op.Tags) == 0 || len(op.Tags) > DefaultMaxPending {
			return fmt.Errorf("%w: removal needs observed tags", ErrInvalidOp)
		}
		for _, d := range op.Tags {
			if d.Replica == "" || d.Seq == 0 {
				return fmt.Errorf("%w: malformed tag", ErrInvalidOp)
			}
			if d.Replica == op.ID.Replica && d.Seq >= op.ID.Seq {
				return fmt.Errorf("%w: removal observes a later addition", ErrInvalidOp)
			}
		}
	default:
		return fmt.Errorf("%w: unknown set operation kind %q", ErrInvalidOp, op.Kind)
	}
	return nil
}

// ORSet is an observed-remove set of strings with add-wins semantics: a
// removal only cancels the additions its replica had seen, so an addition
// concurrent with a removal survives. It is not safe for concurrent use.
type ORSet struct {
	replica string
	log     opLog[SetOp]
	elems   map[string]map[Dot]struct{}
}

// NewORSet returns an empty set whose local operations are issued as
// replica.
func NewORSet(replica string) *ORSet {
	return &ORSet{replica: replica, log: newOpLog[SetOp](0), elems: map[string]map[Dot]struct{}{}}
}

// Add adds elem and returns the operation to broadcast.
func (s *ORSet) Add(elem string) (SetOp, error) {
	op := SetOp{ID: s.log.next(s.replica), Kind: SetAdd, Elem: elem}
	if err := op.Validate(); err != nil {
		return SetOp{}, err
	}
	s.apply(op)
	s.log.commit(op)
	return op, nil
}

// Remove removes elem, returning false when it is absent.
func (s *ORSet) Remove(elem string) (SetOp, bool) {
	tags := s.elems[elem]
	if len(tags) == 0 {
		return SetOp{}, false
	}
	op := SetOp{ID: s.log.next(s.replica), Kind: SetRemove, Elem: elem, Tags: sortedDots(tags)}
	s.apply(op)
	s.log.commit(op)
	return op, true
}

// Contains reports membership.
func (s *ORSet) Contains(elem string) bool { return len(s.elems[elem]) > 0 }

// Len returns the number of elements.
func (s *ORSet) Len() int { return len(s.elems) }

// Elements returns the members, sorted.
func (s *ORSet) Elements() []string { return slices.Sorted(maps.Keys(s.elems)) }

// VersionVector returns a copy of the applied-operations vector.
func (s *ORSet) VersionVector() VersionVector { return s.log.vv.Clone() }

// Apply integrates remote operations; see Text.Apply.
func (s *ORSet) Apply(ops ...SetOp) ([]SetOp, error) {
	for _, op := range ops {
		if err := op.Validate(); err != nil {
			return nil, err
		}
	}
	return s.log.ingest(ops, s.ready, func(op SetOp) error { s.apply(op); return nil })
}

// Delta returns the operations a replica at remote is missing.
func (s *ORSet) Delta(remote VersionVector) ([]SetOp, error) { return s.log.delta(remote) }

// Compact drops logged operations covered by frontier.
func (s *ORSet) Compact(frontier VersionVector) int { return s.log.compact(frontier) }

// DropPending discards buffered operations from replica.
func (s *ORSet) DropPending(replica string) int { return s.log.dropPending(replica) }

func (s *ORSet) ready(op SetOp) bool {
	for _, d := range op.Tags {
		if !s.log.vv.Contains(d) {
			return false
		}
	}
	return true
}

func (s *ORSet) apply(op SetOp) {
	switch op.Kind {
	case SetAdd:
		tags := s.elems[op.Elem]
		if tags == nil {
			tags = map[Dot]struct{}{}
			s.elems[op.Elem] = tags
		}
		tags[op.ID] = struct{}{}
	case SetRemove:
		tags := s.elems[op.Elem]
		for _, d := range op.Tags {
			delete(tags, d)
		}
		if len(tags) == 0 {
			delete(s.elems, op.Elem)
		}
	}
}

func sortedDots(set map[Dot]struct{}) []Dot {
	out := slices.Collect(maps.Keys(set))
	slices.SortFunc(out, func(a, b Dot) int {
		if a.Replica != b.Replica {
			if a.Replica < b.Replica {
				return -1
			}
			return 1
		}
		switch {
		case a.Seq < b.Seq:
			return -1
		case a.Seq > b.Seq:
			return 1
		}
		return 0
	})
	return out
}

// ORSetSnapshot is the full state of an ORSet.
type ORSetSnapshot struct {
	VV    VersionVector    `json:"vv"`
	Base  VersionVector    `json:"base"`
	Elems map[string][]Dot `json:"elems"`
	Log   []SetOp          `json:"log,omitempty"`
}

// Snapshot captures the state; withLog includes the operation log.
func (s *ORSet) Snapshot(withLog bool) ORSetSnapshot {
	snap := ORSetSnapshot{VV: s.log.vv.Clone(), Elems: make(map[string][]Dot, len(s.elems))}
	if withLog {
		snap.Base, snap.Log = s.log.base.Clone(), slices.Clone(s.log.applied)
	} else {
		snap.Base = s.log.vv.Clone()
	}
	for e, tags := range s.elems {
		snap.Elems[e] = sortedDots(tags)
	}
	return snap
}

// RestoreORSet builds a replica from a snapshot.
func RestoreORSet(replica string, snap ORSetSnapshot) (*ORSet, error) {
	s := NewORSet(replica)
	base := snap.Base
	if base == nil {
		base = VersionVector{}
	}
	if err := s.log.restore(snap.VV, base, snap.Log); err != nil {
		return nil, err
	}
	for _, op := range snap.Log {
		if err := op.Validate(); err != nil {
			return nil, err
		}
	}
	for e, tags := range snap.Elems {
		if e == "" || len(tags) == 0 {
			return nil, fmt.Errorf("%w: snapshot element %q is malformed", ErrInvalidOp, e)
		}
		set := make(map[Dot]struct{}, len(tags))
		for _, d := range tags {
			if !snap.VV.Contains(d) {
				return nil, fmt.Errorf("%w: snapshot tag %s is not covered by its version vector", ErrInvalidOp, d)
			}
			set[d] = struct{}{}
		}
		s.elems[e] = set
	}
	return s, nil
}
