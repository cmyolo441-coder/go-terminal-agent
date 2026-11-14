package collab

import (
	"errors"
	"fmt"
	"maps"
	"slices"
)

// DefaultMaxPending bounds the operations a replica buffers while waiting
// for their causal dependencies.
const DefaultMaxPending = 8192

// Errors shared by the replicated types.
var (
	// ErrInvalidOp reports a malformed or inconsistent operation.
	ErrInvalidOp = errors.New("collab: invalid operation")
	// ErrTooManyPending reports that the causal buffer is full.
	ErrTooManyPending = errors.New("collab: too many operations awaiting dependencies")
	// ErrSnapshotRequired reports that the operations a peer is missing
	// were compacted away; the peer must restore from a snapshot.
	ErrSnapshotRequired = errors.New("collab: peer is behind the compaction frontier; a snapshot is required")
)

// causalOp is implemented by every replicated operation.
type causalOp interface{ opDot() Dot }

// opLog is the delivery engine shared by the replicated types. It keeps
// the version vector of applied operations, the applied operations
// themselves in application order (which is a causal order) for delta
// sync, and a buffer of operations that arrived before their dependencies.
//
// Operations from one replica are applied strictly in sequence order; a
// type-specific ready predicate adds cross-replica dependencies (an
// insertion's anchor, a removal's observed tags). Duplicates are dropped
// by sequence number, so delivery may be in any order, any number of
// times.
type opLog[O causalOp] struct {
	vv         VersionVector
	base       VersionVector // operations covered by base were compacted away
	applied    []O
	pending    map[Dot]O
	maxPending int
}

func newOpLog[O causalOp](maxPending int) opLog[O] {
	if maxPending <= 0 {
		maxPending = DefaultMaxPending
	}
	return opLog[O]{vv: VersionVector{}, base: VersionVector{}, pending: map[Dot]O{}, maxPending: maxPending}
}

// next returns the dot of replica's next local operation.
func (l *opLog[O]) next(replica string) Dot { return Dot{Replica: replica, Seq: l.vv[replica] + 1} }

// commit records op as applied.
func (l *opLog[O]) commit(op O) {
	d := op.opDot()
	l.vv[d.Replica] = d.Seq
	l.applied = append(l.applied, op)
}

// ingest buffers ops, then applies every operation whose dependencies are
// met, returning the ones applied in the order they were applied. An
// apply error drops the offending operation and stops; operations applied
// before it stay applied and are returned.
func (l *opLog[O]) ingest(ops []O, ready func(O) bool, apply func(O) error) ([]O, error) {
	var applied []O
	for _, op := range ops {
		d := op.opDot()
		if d.Replica == "" || d.Seq == 0 {
			return applied, fmt.Errorf("%w: missing operation id", ErrInvalidOp)
		}
		if l.vv.Contains(d) {
			continue
		}
		if _, dup := l.pending[d]; dup {
			continue
		}
		if len(l.pending) >= l.maxPending {
			// Make room by applying whatever is already deliverable.
			out, err := l.drain(ready, apply)
			applied = append(applied, out...)
			if err != nil {
				return applied, err
			}
			if len(l.pending) >= l.maxPending {
				return applied, fmt.Errorf("%w (%d buffered)", ErrTooManyPending, len(l.pending))
			}
		}
		l.pending[d] = op
	}
	out, err := l.drain(ready, apply)
	return append(applied, out...), err
}

func (l *opLog[O]) drain(ready func(O) bool, apply func(O) error) ([]O, error) {
	var out []O
	for progress := true; progress && len(l.pending) > 0; {
		progress = false
		for _, r := range l.pendingReplicas() {
			for {
				d := Dot{Replica: r, Seq: l.vv[r] + 1}
				op, ok := l.pending[d]
				if !ok || !ready(op) {
					break
				}
				delete(l.pending, d)
				if err := apply(op); err != nil {
					return out, fmt.Errorf("apply %s: %w", d, err)
				}
				l.commit(op)
				out = append(out, op)
				progress = true
			}
		}
	}
	return out, nil
}

func (l *opLog[O]) pendingReplicas() []string {
	set := make(map[string]struct{}, 4)
	for d := range l.pending {
		set[d.Replica] = struct{}{}
	}
	return slices.Sorted(maps.Keys(set))
}

// dropPending discards buffered operations from replica.
func (l *opLog[O]) dropPending(replica string) int {
	n := 0
	for d := range l.pending {
		if d.Replica == replica {
			delete(l.pending, d)
			n++
		}
	}
	return n
}

// delta returns, in causal order, exactly the applied operations remote
// has not seen.
func (l *opLog[O]) delta(remote VersionVector) ([]O, error) {
	for r, b := range l.base {
		if remote[r] < b {
			return nil, fmt.Errorf("%w (replica %s: have from %d, peer at %d)", ErrSnapshotRequired, r, b+1, remote[r])
		}
	}
	var out []O
	for _, op := range l.applied {
		if !remote.Contains(op.opDot()) {
			out = append(out, op)
		}
	}
	return out, nil
}

// compact drops applied operations covered by frontier (clamped to what
// this replica has applied). Peers behind the new base need a snapshot.
func (l *opLog[O]) compact(frontier VersionVector) int {
	f := Meet(frontier, l.vv)
	if len(f) == 0 {
		return 0
	}
	kept := l.applied[:0]
	dropped := 0
	for _, op := range l.applied {
		if f.Contains(op.opDot()) {
			dropped++
			continue
		}
		kept = append(kept, op)
	}
	clear(l.applied[len(kept):])
	l.applied = kept
	l.base.Merge(f)
	return dropped
}

// restore validates and installs a log taken from a snapshot.
func (l *opLog[O]) restore(vv, base VersionVector, ops []O) error {
	l.vv, l.base = vv.Clone(), base.Clone()
	if !l.vv.Dominates(l.base) {
		return fmt.Errorf("%w: snapshot base %v is ahead of its version vector %v", ErrInvalidOp, base, vv)
	}
	seen := make(map[Dot]struct{}, len(ops))
	for _, op := range ops {
		d := op.opDot()
		if !l.vv.Contains(d) || l.base.Contains(d) {
			return fmt.Errorf("%w: snapshot log operation %s outside (base, vv]", ErrInvalidOp, d)
		}
		if _, dup := seen[d]; dup {
			return fmt.Errorf("%w: snapshot log repeats %s", ErrInvalidOp, d)
		}
		seen[d] = struct{}{}
	}
	if uint64(len(ops)) != l.vv.Missing(l.base) {
		return fmt.Errorf("%w: snapshot log has %d operations, want %d", ErrInvalidOp, len(ops), l.vv.Missing(l.base))
	}
	l.applied = slices.Clone(ops)
	return nil
}

// pendingFrom counts buffered operations from replica.
func (l *opLog[O]) pendingFrom(replica string) int {
	n := 0
	for d := range l.pending {
		if d.Replica == replica {
			n++
		}
	}
	return n
}
