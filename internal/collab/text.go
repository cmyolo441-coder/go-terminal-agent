package collab

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"
)

// Limits on a single text operation.
const (
	MaxInsertRunes  = 1 << 20
	MaxDeleteRanges = 1 << 16
)

// Text operation kinds.
const (
	TextInsert = "ins"
	TextDelete = "del"
)

// ErrOutOfRange reports a position outside the visible text.
var ErrOutOfRange = errors.New("collab: position out of range")

// CharID names one character of a Text: a Lamport timestamp and the
// replica that inserted it. Characters inserted together get consecutive
// Lamport values, so a run of n characters is named by its first id and n.
// The zero CharID stands for the start of the document.
type CharID struct {
	Lamport uint64 `json:"l"`
	Replica string `json:"r"`
}

// IsZero reports whether id is the document start.
func (id CharID) IsZero() bool { return id.Lamport == 0 && id.Replica == "" }

// Compare orders ids by Lamport timestamp, then replica.
func (id CharID) Compare(o CharID) int {
	switch {
	case id.Lamport < o.Lamport:
		return -1
	case id.Lamport > o.Lamport:
		return 1
	default:
		return strings.Compare(id.Replica, o.Replica)
	}
}

func (id CharID) String() string { return fmt.Sprintf("%d@%s", id.Lamport, id.Replica) }

// IDRange names Len consecutive characters inserted by Replica starting at
// Lamport.
type IDRange struct {
	Replica string `json:"r"`
	Lamport uint64 `json:"l"`
	Len     uint64 `json:"n"`
}

// TextOp is one replicated edit of a Text. An insertion places Text
// (run-length encoded: its i-th rune gets Lamport+i) after Origin; a
// deletion tombstones the characters in Ranges.
type TextOp struct {
	ID      Dot       `json:"id"`
	Kind    string    `json:"k"`
	Origin  CharID    `json:"o,omitzero"`
	Lamport uint64    `json:"l,omitempty"`
	Text    string    `json:"t,omitempty"`
	Ranges  []IDRange `json:"d,omitempty"`
}

func (op TextOp) opDot() Dot { return op.ID }

// Validate checks the operation's shape without consulting any replica.
func (op TextOp) Validate() error {
	if op.ID.Replica == "" || op.ID.Seq == 0 {
		return fmt.Errorf("%w: text operation without id", ErrInvalidOp)
	}
	switch op.Kind {
	case TextInsert:
		if len(op.Ranges) != 0 {
			return fmt.Errorf("%w: insertion with ranges", ErrInvalidOp)
		}
		if op.Lamport == 0 {
			return fmt.Errorf("%w: insertion without Lamport timestamp", ErrInvalidOp)
		}
		if op.Text == "" || !utf8.ValidString(op.Text) {
			return fmt.Errorf("%w: insertion text must be non-empty UTF-8", ErrInvalidOp)
		}
		n := utf8.RuneCountInString(op.Text)
		if n > MaxInsertRunes {
			return fmt.Errorf("%w: insertion of %d runes exceeds %d", ErrInvalidOp, n, MaxInsertRunes)
		}
		if op.Lamport > ^uint64(0)-uint64(n) {
			return fmt.Errorf("%w: Lamport timestamp overflows", ErrInvalidOp)
		}
		if op.Origin.IsZero() != (op.Origin.Replica == "") || (op.Origin.Lamport == 0) != op.Origin.IsZero() {
			return fmt.Errorf("%w: malformed origin", ErrInvalidOp)
		}
		if op.Origin.Replica == op.ID.Replica && op.Origin.Lamport >= op.Lamport {
			return fmt.Errorf("%w: origin is not older than the insertion", ErrInvalidOp)
		}
	case TextDelete:
		if op.Text != "" || op.Lamport != 0 || !op.Origin.IsZero() {
			return fmt.Errorf("%w: deletion with insertion fields", ErrInvalidOp)
		}
		if len(op.Ranges) == 0 || len(op.Ranges) > MaxDeleteRanges {
			return fmt.Errorf("%w: deletion needs 1..%d ranges", ErrInvalidOp, MaxDeleteRanges)
		}
		for _, rg := range op.Ranges {
			if rg.Replica == "" || rg.Lamport == 0 || rg.Len == 0 || rg.Len > MaxInsertRunes*16 || rg.Lamport > ^uint64(0)-rg.Len {
				return fmt.Errorf("%w: malformed deletion range", ErrInvalidOp)
			}
		}
	default:
		return fmt.Errorf("%w: unknown text operation kind %q", ErrInvalidOp, op.Kind)
	}
	return nil
}

// run is a maximal block of consecutive characters stored as one list
// node: same inserting replica, consecutive Lamport ids, each character
// anchored on the previous one, and one deletion state. Deleted runs keep
// only their length.
type run struct {
	id      CharID
	origin  CharID // origin of the first character
	n       int
	text    []rune // nil once deleted
	seq     uint64 // upper bound of the insertion operations' sequence numbers
	deleted bool
	delBy   string
	delSeq  uint64 // upper bound of the deleting operations' sequence numbers
	prev    *run
	next    *run
}

func (r *run) charID(k int) CharID {
	return CharID{Lamport: r.id.Lamport + uint64(k), Replica: r.id.Replica}
}
func (r *run) last() CharID { return r.charID(r.n - 1) }
func (r *run) end() uint64  { return r.id.Lamport + uint64(r.n) }

// Text is a replicated plain-text sequence: an RGA (Roh et al., 2011)
// whose nodes are run-length encoded. Concurrent insertions at the same
// place are ordered by descending Lamport id, deletions leave tombstones
// that are garbage-collected once causally stable, and replicas that apply
// the same set of operations, in any causal order, hold identical text.
//
// A Text is not safe for concurrent use.
type Text struct {
	replica    string
	log        opLog[TextOp]
	lamport    uint64
	head       run
	index      map[string][]*run // per replica, sorted by id.Lamport
	visible    int
	tombstones int
	nodes      int
}

// NewText returns an empty Text whose local edits are issued as replica.
func NewText(replica string) *Text {
	return &Text{replica: replica, log: newOpLog[TextOp](0), index: map[string][]*run{}}
}

// SetMaxPending bounds the causal buffer.
func (t *Text) SetMaxPending(n int) {
	if n > 0 {
		t.log.maxPending = n
	}
}

// Replica returns the id local edits are issued under.
func (t *Text) Replica() string { return t.replica }

// Len returns the number of visible runes.
func (t *Text) Len() int { return t.visible }

// String returns the visible text.
func (t *Text) String() string {
	var b strings.Builder
	for r := t.head.next; r != nil; r = r.next {
		if !r.deleted {
			for _, c := range r.text {
				b.WriteRune(c)
			}
		}
	}
	return b.String()
}

// VersionVector returns a copy of the applied-operations vector.
func (t *Text) VersionVector() VersionVector { return t.log.vv.Clone() }

// TextStats describes a Text's storage.
type TextStats struct {
	Runes      int `json:"runes"`
	Tombstones int `json:"tombstones"`
	Nodes      int `json:"nodes"`
	LogOps     int `json:"log_ops"`
	Pending    int `json:"pending"`
}

// Stats reports storage counters.
func (t *Text) Stats() TextStats {
	return TextStats{Runes: t.visible, Tombstones: t.tombstones, Nodes: t.nodes, LogOps: len(t.log.applied), Pending: len(t.log.pending)}
}

// Insert inserts s before the rune at visible position pos and returns the
// operation to broadcast.
func (t *Text) Insert(pos int, s string) (TextOp, error) {
	if pos < 0 || pos > t.visible {
		return TextOp{}, fmt.Errorf("%w: insert at %d of %d", ErrOutOfRange, pos, t.visible)
	}
	// Converting through []rune maps each invalid byte to U+FFFD, so the
	// rune count is what every replica will see.
	runes := []rune(s)
	if len(runes) == 0 {
		return TextOp{}, fmt.Errorf("%w: empty insertion", ErrInvalidOp)
	}
	if len(runes) > MaxInsertRunes {
		return TextOp{}, fmt.Errorf("%w: insertion of %d runes exceeds %d", ErrInvalidOp, len(runes), MaxInsertRunes)
	}
	var origin CharID
	if pos > 0 {
		r, off := t.locate(pos - 1)
		origin = r.charID(off)
	}
	op := TextOp{ID: t.log.next(t.replica), Kind: TextInsert, Origin: origin, Lamport: t.lamport + 1, Text: string(runes)}
	if err := t.integrate(op); err != nil {
		return TextOp{}, err
	}
	t.log.commit(op)
	return op, nil
}

// Delete removes n visible runes starting at pos and returns the operation
// to broadcast.
func (t *Text) Delete(pos, n int) (TextOp, error) {
	if pos < 0 || n <= 0 || pos+n > t.visible {
		return TextOp{}, fmt.Errorf("%w: delete [%d,%d) of %d", ErrOutOfRange, pos, pos+n, t.visible)
	}
	var ranges []IDRange
	r, off := t.locate(pos)
	for left := n; left > 0; r, off = r.next, 0 {
		if r.deleted {
			continue
		}
		take := min(r.n-off, left)
		id := r.charID(off)
		if k := len(ranges) - 1; k >= 0 && ranges[k].Replica == id.Replica && ranges[k].Lamport+ranges[k].Len == id.Lamport {
			ranges[k].Len += uint64(take)
		} else {
			ranges = append(ranges, IDRange{Replica: id.Replica, Lamport: id.Lamport, Len: uint64(take)})
		}
		left -= take
	}
	op := TextOp{ID: t.log.next(t.replica), Kind: TextDelete, Ranges: ranges}
	if err := t.applyDelete(op); err != nil {
		return TextOp{}, err
	}
	t.log.commit(op)
	return op, nil
}

// Apply integrates remote operations, buffering those whose dependencies
// have not arrived. It returns the operations applied by this call (which
// may include previously buffered ones) in application order. Duplicates
// are ignored.
func (t *Text) Apply(ops ...TextOp) ([]TextOp, error) {
	for _, op := range ops {
		if err := op.Validate(); err != nil {
			return nil, err
		}
	}
	return t.log.ingest(ops, t.ready, t.apply)
}

// Delta returns the operations a replica at remote is missing, in causal
// order, or ErrSnapshotRequired when they were compacted away.
func (t *Text) Delta(remote VersionVector) ([]TextOp, error) { return t.log.delta(remote) }

// DropPending discards buffered operations from replica.
func (t *Text) DropPending(replica string) int { return t.log.dropPending(replica) }

func (t *Text) apply(op TextOp) error {
	if op.Kind == TextInsert {
		return t.integrate(op)
	}
	return t.applyDelete(op)
}

func (t *Text) ready(op TextOp) bool {
	if op.Kind == TextInsert {
		if op.Origin.IsZero() {
			return true
		}
		_, _, ok := t.find(op.Origin)
		return ok
	}
	for _, rg := range op.Ranges {
		for l, end := rg.Lamport, rg.Lamport+rg.Len; l < end; {
			r, _, ok := t.find(CharID{Lamport: l, Replica: rg.Replica})
			if !ok {
				return false
			}
			l = r.end()
		}
	}
	return true
}

// find returns the run holding id and id's offset in it.
func (t *Text) find(id CharID) (*run, int, bool) {
	runs := t.index[id.Replica]
	i := sortSearchRuns(runs, id.Lamport) - 1
	if i < 0 {
		return nil, 0, false
	}
	r := runs[i]
	if id.Lamport >= r.end() {
		return nil, 0, false
	}
	return r, int(id.Lamport - r.id.Lamport), true
}

// sortSearchRuns returns the index of the first run starting after lamport.
func sortSearchRuns(runs []*run, lamport uint64) int {
	lo, hi := 0, len(runs)
	for lo < hi {
		m := int(uint(lo+hi) >> 1)
		if runs[m].id.Lamport <= lamport {
			lo = m + 1
		} else {
			hi = m
		}
	}
	return lo
}

func (t *Text) indexAdd(r *run) {
	runs := t.index[r.id.Replica]
	i := sortSearchRuns(runs, r.id.Lamport)
	t.index[r.id.Replica] = slices.Insert(runs, i, r)
}

func (t *Text) indexRemove(r *run) {
	runs := t.index[r.id.Replica]
	i := sortSearchRuns(runs, r.id.Lamport) - 1
	if i >= 0 && runs[i] == r {
		runs = slices.Delete(runs, i, i+1)
		if len(runs) == 0 {
			delete(t.index, r.id.Replica)
		} else {
			t.index[r.id.Replica] = runs
		}
	}
}

// locate returns the run and offset of the visible rune at pos.
func (t *Text) locate(pos int) (*run, int) {
	for r := t.head.next; r != nil; r = r.next {
		if r.deleted {
			continue
		}
		if pos < r.n {
			return r, pos
		}
		pos -= r.n
	}
	panic("collab: locate past end of text")
}

// split cuts r before offset k (0 < k < r.n) and returns the right part.
func (t *Text) split(r *run, k int) *run {
	nr := &run{
		id: r.charID(k), origin: r.charID(k - 1), n: r.n - k,
		seq: r.seq, deleted: r.deleted, delBy: r.delBy, delSeq: r.delSeq,
	}
	if !r.deleted {
		nr.text = slices.Clone(r.text[k:])
		r.text = r.text[:k:k]
	}
	r.n = k
	t.link(r, nr)
	t.indexAdd(nr)
	t.nodes++
	return nr
}

func (t *Text) link(after, nr *run) {
	nr.prev, nr.next = after, after.next
	if after.next != nil {
		after.next.prev = nr
	}
	after.next = nr
}

func (t *Text) unlink(r *run) {
	r.prev.next = r.next
	if r.next != nil {
		r.next.prev = r.prev
	}
	r.prev, r.next = nil, nil
}

func (t *Text) integrate(op TextOp) error {
	runes := []rune(op.Text)
	n := len(runes)
	id := CharID{Lamport: op.Lamport, Replica: op.ID.Replica}
	if t.overlaps(id, n) {
		return fmt.Errorf("%w: character ids %v+%d already exist", ErrInvalidOp, id, n)
	}
	left := &t.head
	scan := true
	if !op.Origin.IsZero() {
		o, off, ok := t.find(op.Origin)
		if !ok {
			return fmt.Errorf("%w: origin %v is unknown", ErrInvalidOp, op.Origin)
		}
		left = o
		if off < o.n-1 {
			if o.charID(off+1).Compare(id) < 0 {
				// The next character is older: the new run goes right here.
				t.split(o, off+1)
				scan = false
			}
			// Otherwise the rest of the run is newer and is skipped as a
			// whole by the scan below.
		}
	}
	if scan {
		// RGA: skip every following element newer than the new one. A
		// run's first character is its oldest, so whole runs are skipped.
		for left.next != nil && left.next.id.Compare(id) > 0 {
			left = left.next
		}
	}
	if left != &t.head && !left.deleted && left.id.Replica == id.Replica && left.end() == id.Lamport && left.last() == op.Origin {
		left.text = append(left.text, runes...)
		left.n += n
		left.seq = max(left.seq, op.ID.Seq)
	} else {
		nr := &run{id: id, origin: op.Origin, n: n, text: runes, seq: op.ID.Seq}
		t.link(left, nr)
		t.indexAdd(nr)
		t.nodes++
	}
	t.visible += n
	t.lamport = max(t.lamport, op.Lamport+uint64(n)-1)
	return nil
}

// overlaps reports whether any of the n ids starting at id exist.
func (t *Text) overlaps(id CharID, n int) bool {
	runs := t.index[id.Replica]
	i := sortSearchRuns(runs, id.Lamport)
	if i > 0 && runs[i-1].end() > id.Lamport {
		return true
	}
	return i < len(runs) && runs[i].id.Lamport < id.Lamport+uint64(n)
}

func (t *Text) applyDelete(op TextOp) error {
	for _, rg := range op.Ranges {
		for l, end := rg.Lamport, rg.Lamport+rg.Len; l < end; {
			r, off, ok := t.find(CharID{Lamport: l, Replica: rg.Replica})
			if !ok {
				return fmt.Errorf("%w: deletion of unknown character %d@%s", ErrInvalidOp, l, rg.Replica)
			}
			if r.deleted {
				l += uint64(min(uint64(r.n-off), end-l))
				continue
			}
			if off > 0 {
				r = t.split(r, off)
			}
			if take := end - l; take < uint64(r.n) {
				t.split(r, int(take))
			}
			r.deleted, r.text = true, nil
			r.delBy, r.delSeq = op.ID.Replica, op.ID.Seq
			t.visible -= r.n
			t.tombstones += r.n
			l += uint64(r.n)
		}
	}
	return nil
}

// GCStats reports what CollectGarbage reclaimed.
type GCStats struct {
	Frontier       VersionVector `json:"frontier"`
	RemovedRunes   int           `json:"removed_runes"`
	MergedNodes    int           `json:"merged_nodes"`
	CompactedOps   int           `json:"compacted_ops"`
	SkippedUnsafe  bool          `json:"skipped_unsafe,omitempty"`
	ProtectedRunes int           `json:"protected_runes,omitempty"`
}

// CollectGarbage removes tombstones that no current or future operation
// can refer to, merges adjacent runs, and compacts the operation log.
//
// reports must hold the latest version vector acknowledged by every
// replica that can still issue operations (this replica's own entry is
// implied). Their meet is the causal-stability frontier. Nothing is
// collected unless this replica already holds each reporter's own
// operations up to its report, which guarantees that every operation it
// has yet to receive was issued causally after the frontier. A tombstone
// is then removed when its insertion and deletion are both stable and no
// unstable character is anchored on it; such a character's position still
// depends on the tombstone's id. Replicas may collect at different times
// and still converge.
func (t *Text) CollectGarbage(reports map[string]VersionVector) GCStats {
	st := GCStats{}
	for r, rep := range reports {
		if t.log.vv[r] < rep[r] {
			st.SkippedUnsafe = true
			st.Frontier = VersionVector{}
			return st
		}
	}
	vs := []VersionVector{t.log.vv}
	for _, k := range slices.Sorted(maps.Keys(reports)) {
		vs = append(vs, reports[k])
	}
	f := Meet(vs...)
	st.Frontier = f

	protected := map[string][]uint64{}
	for r := t.head.next; r != nil; r = r.next {
		if r.seq > f[r.id.Replica] && !r.origin.IsZero() {
			protected[r.origin.Replica] = append(protected[r.origin.Replica], r.origin.Lamport)
		}
	}
	for _, ls := range protected {
		slices.Sort(ls)
	}
	for r := t.head.next; r != nil; {
		next := r.next
		if r.deleted && r.seq <= f[r.id.Replica] && r.delSeq <= f[r.delBy] {
			removed, kept := t.collectRun(r, protected[r.id.Replica])
			st.RemovedRunes += removed
			st.ProtectedRunes += kept
		}
		r = next
	}
	st.MergedNodes = t.mergeRuns()
	st.CompactedOps = t.log.compact(f)
	return st
}

// collectRun removes run r except the characters in protected (sorted
// Lamport ids of r's replica), which stay as single-character tombstones.
func (t *Text) collectRun(r *run, protected []uint64) (removed, kept int) {
	i, _ := slices.BinarySearch(protected, r.id.Lamport)
	cur := r
	for cur != nil {
		for i < len(protected) && protected[i] < cur.id.Lamport {
			i++
		}
		if i >= len(protected) || protected[i] >= cur.end() {
			removed += cur.n
			t.dropRun(cur)
			return removed, kept
		}
		p := protected[i]
		if p > cur.id.Lamport {
			right := t.split(cur, int(p-cur.id.Lamport))
			removed += cur.n
			t.dropRun(cur)
			cur = right
		}
		kept++
		if cur.n == 1 {
			return removed, kept
		}
		cur = t.split(cur, 1)
		for i < len(protected) && protected[i] < cur.id.Lamport {
			i++
		}
	}
	return removed, kept
}

func (t *Text) dropRun(r *run) {
	t.tombstones -= r.n
	t.unlink(r)
	t.indexRemove(r)
	t.nodes--
}

// mergeRuns joins neighbouring runs that form one run.
func (t *Text) mergeRuns() int {
	merged := 0
	for a := t.head.next; a != nil && a.next != nil; {
		b := a.next
		if a.id.Replica == b.id.Replica && a.end() == b.id.Lamport && b.origin == a.last() &&
			a.deleted == b.deleted && a.delBy == b.delBy {
			if !a.deleted {
				a.text = append(a.text, b.text...)
			}
			a.n += b.n
			a.seq = max(a.seq, b.seq)
			a.delSeq = max(a.delSeq, b.delSeq)
			t.unlink(b)
			t.indexRemove(b)
			t.nodes--
			merged++
			continue
		}
		a = b
	}
	return merged
}

// RunState is one run in a TextSnapshot.
type RunState struct {
	ID      CharID `json:"id"`
	Origin  CharID `json:"o,omitzero"`
	N       int    `json:"n"`
	Text    string `json:"t,omitempty"`
	Seq     uint64 `json:"s"`
	Deleted bool   `json:"x,omitempty"`
	DelBy   string `json:"xr,omitempty"`
	DelSeq  uint64 `json:"xs,omitempty"`
}

// TextSnapshot is the full state of a Text, from which a new replica can
// start. Without a log, the snapshot cannot serve deltas to older peers.
type TextSnapshot struct {
	VV      VersionVector `json:"vv"`
	Base    VersionVector `json:"base,omitempty"`
	Lamport uint64        `json:"lamport"`
	Runs    []RunState    `json:"runs"`
	Log     []TextOp      `json:"log,omitempty"`
}

// Snapshot captures the state; withLog includes the operation log.
func (t *Text) Snapshot(withLog bool) TextSnapshot {
	s := TextSnapshot{VV: t.log.vv.Clone(), Lamport: t.lamport, Runs: make([]RunState, 0, t.nodes)}
	if withLog {
		s.Base = t.log.base.Clone()
		s.Log = slices.Clone(t.log.applied)
	} else {
		s.Base = t.log.vv.Clone()
	}
	for r := t.head.next; r != nil; r = r.next {
		s.Runs = append(s.Runs, RunState{
			ID: r.id, Origin: r.origin, N: r.n, Text: string(r.text), Seq: r.seq,
			Deleted: r.deleted, DelBy: r.delBy, DelSeq: r.delSeq,
		})
	}
	return s
}

// RestoreText builds a replica named replica from a snapshot, validating
// it fully.
func RestoreText(replica string, s TextSnapshot) (*Text, error) {
	t := NewText(replica)
	base := s.Base
	if base == nil {
		base = s.VV
	}
	if err := t.log.restore(s.VV, base, s.Log); err != nil {
		return nil, err
	}
	for _, op := range s.Log {
		if err := op.Validate(); err != nil {
			return nil, err
		}
	}
	t.lamport = s.Lamport
	prev := &t.head
	for _, rs := range s.Runs {
		if rs.N <= 0 || rs.ID.Lamport == 0 || rs.ID.Replica == "" || rs.ID.Lamport > ^uint64(0)-uint64(rs.N) {
			return nil, fmt.Errorf("%w: snapshot run %v is malformed", ErrInvalidOp, rs.ID)
		}
		if rs.ID.Lamport+uint64(rs.N)-1 > s.Lamport {
			return nil, fmt.Errorf("%w: snapshot run %v is newer than the snapshot clock", ErrInvalidOp, rs.ID)
		}
		if t.overlaps(rs.ID, rs.N) {
			return nil, fmt.Errorf("%w: snapshot runs overlap at %v", ErrInvalidOp, rs.ID)
		}
		if rs.Seq == 0 || rs.Seq > s.VV[rs.ID.Replica] || (rs.Deleted && (rs.DelBy == "" || rs.DelSeq == 0 || rs.DelSeq > s.VV[rs.DelBy])) {
			return nil, fmt.Errorf("%w: snapshot run %v has inconsistent versions", ErrInvalidOp, rs.ID)
		}
		r := &run{id: rs.ID, origin: rs.Origin, n: rs.N, seq: rs.Seq, deleted: rs.Deleted, delBy: rs.DelBy, delSeq: rs.DelSeq}
		if rs.Deleted {
			if rs.Text != "" {
				return nil, fmt.Errorf("%w: snapshot tombstone %v carries text", ErrInvalidOp, rs.ID)
			}
			t.tombstones += rs.N
		} else {
			if !utf8.ValidString(rs.Text) || utf8.RuneCountInString(rs.Text) != rs.N {
				return nil, fmt.Errorf("%w: snapshot run %v text does not match its length", ErrInvalidOp, rs.ID)
			}
			r.text = []rune(rs.Text)
			t.visible += rs.N
		}
		t.link(prev, r)
		t.indexAdd(r)
		t.nodes++
		prev = r
	}
	return t, nil
}
