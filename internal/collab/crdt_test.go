package collab

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"
)

// network simulates unreliable broadcast: every replica eventually gets
// every operation, in random order, possibly several times.
type network[O any] struct {
	rng    *rand.Rand
	inbox  [][]O
	sender [][]int
}

func newNetwork[O any](rng *rand.Rand, n int) *network[O] {
	return &network[O]{rng: rng, inbox: make([][]O, n), sender: make([][]int, n)}
}

func (n *network[O]) broadcast(from int, op O) {
	for i := range n.inbox {
		if i != from {
			n.inbox[i] = append(n.inbox[i], op)
			if n.rng.IntN(5) == 0 { // duplicate
				n.inbox[i] = append(n.inbox[i], op)
			}
		}
	}
}

// deliver hands replica i a random subset of its inbox in random order,
// or everything when all is set.
func (n *network[O]) deliver(i int, all bool, apply func([]O)) {
	box := n.inbox[i]
	n.rng.Shuffle(len(box), func(a, b int) { box[a], box[b] = box[b], box[a] })
	k := len(box)
	if !all && k > 0 {
		k = n.rng.IntN(k + 1)
	}
	batch := slices.Clone(box[:k])
	n.inbox[i] = slices.Clone(box[k:])
	if all {
		// Redeliver a few as late duplicates too.
		for j := 0; j < len(batch)/4; j++ {
			batch = append(batch, batch[n.rng.IntN(len(batch))])
		}
	}
	for len(batch) > 0 {
		m := 1 + n.rng.IntN(len(batch))
		apply(batch[:m])
		batch = batch[m:]
	}
}

func randomText(rng *rand.Rand) string {
	alphabet := []rune("abcdefghij XYZ\nλ😀")
	n := 1 + rng.IntN(4)
	if rng.IntN(10) == 0 {
		n = 5 + rng.IntN(20)
	}
	var b strings.Builder
	for range n {
		b.WriteRune(alphabet[rng.IntN(len(alphabet))])
	}
	return b.String()
}

func TestTextConvergenceProperty(t *testing.T) {
	seeds := 60
	if testing.Short() {
		seeds = 15
	}
	totalGC := 0
	for seed := range seeds {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(uint64(seed), 0x5eed))
			n := 2 + rng.IntN(4)
			reps := make([]*Text, n)
			for i := range reps {
				reps[i] = NewText(fmt.Sprintf("r%d", i))
			}
			net := newNetwork[TextOp](rng, n)
			apply := func(i int) func([]TextOp) {
				return func(ops []TextOp) {
					if _, err := reps[i].Apply(ops...); err != nil {
						t.Fatalf("replica %d apply: %v", i, err)
					}
				}
			}
			steps := 300 + rng.IntN(300)
			for step := range steps {
				i := rng.IntN(n)
				r := reps[i]
				switch k := rng.IntN(20); {
				case k < 11 || r.Len() == 0:
					op, err := r.Insert(rng.IntN(r.Len()+1), randomText(rng))
					if err != nil {
						t.Fatal(err)
					}
					net.broadcast(i, op)
				case k < 17:
					pos := rng.IntN(r.Len())
					op, err := r.Delete(pos, 1+rng.IntN(min(r.Len()-pos, 6)))
					if err != nil {
						t.Fatal(err)
					}
					net.broadcast(i, op)
				case k < 19:
					net.deliver(i, false, apply(i))
				default:
					// Garbage-collect using the other replicas' true state
					// as their reports.
					reports := map[string]VersionVector{}
					for j, o := range reps {
						if j != i {
							reports[o.Replica()] = o.VersionVector()
						}
					}
					st := r.CollectGarbage(reports)
					totalGC += st.RemovedRunes
				}
				if step%97 == 0 {
					checkTextInvariants(t, r)
				}
			}
			for i := range reps {
				net.deliver(i, true, apply(i))
			}
			want := reps[0].String()
			for i, r := range reps {
				checkTextInvariants(t, r)
				if got := r.String(); got != want {
					t.Fatalf("replica %d diverged:\n got %q\nwant %q", i, got, want)
				}
				if r.VersionVector().Compare(reps[0].VersionVector()) != Equal {
					t.Fatalf("replica %d version vector differs", i)
				}
				if p := r.Stats().Pending; p != 0 {
					t.Fatalf("replica %d still has %d pending operations", i, p)
				}
			}
			// Once everything is delivered every tombstone is collectable.
			reports := map[string]VersionVector{}
			for _, o := range reps {
				reports[o.Replica()] = o.VersionVector()
			}
			for _, r := range reps {
				r.CollectGarbage(reports)
				if r.Stats().Tombstones != 0 {
					t.Fatalf("tombstones left after quiescent GC: %+v", r.Stats())
				}
				if r.String() != want {
					t.Fatal("GC changed visible text")
				}
			}
			// And replicas keep converging after full collection.
			a, b := reps[0], reps[len(reps)-1]
			opA, _ := a.Insert(a.Len()/2, "PQ")
			opB, _ := b.Insert(b.Len()/2, "RS")
			mustApply(t, a, opB)
			mustApply(t, b, opA)
			if a.String() != b.String() {
				t.Fatalf("diverged after GC: %q vs %q", a.String(), b.String())
			}
		})
	}
	if totalGC == 0 {
		t.Fatal("garbage collection never removed anything; the property test is not exercising it")
	}
}

func mustApply(t *testing.T, r *Text, ops ...TextOp) {
	t.Helper()
	if _, err := r.Apply(ops...); err != nil {
		t.Fatal(err)
	}
}

// checkTextInvariants verifies the list, index and counters agree.
func checkTextInvariants(t *testing.T, x *Text) {
	t.Helper()
	vis, tomb, nodes := 0, 0, 0
	for r := x.head.next; r != nil; r = r.next {
		nodes++
		if r.prev.next != r {
			t.Fatal("broken back link")
		}
		if r.n <= 0 {
			t.Fatal("empty run")
		}
		if r.deleted {
			tomb += r.n
			if r.text != nil {
				t.Fatal("tombstone keeps text")
			}
		} else {
			vis += r.n
			if len(r.text) != r.n {
				t.Fatalf("run length %d but %d runes", r.n, len(r.text))
			}
		}
		if got, off, ok := x.find(r.id); !ok || got != r || off != 0 {
			t.Fatalf("index does not find run %v", r.id)
		}
	}
	indexed := 0
	for _, runs := range x.index {
		indexed += len(runs)
		for i := 1; i < len(runs); i++ {
			if runs[i-1].end() > runs[i].id.Lamport {
				t.Fatal("index runs overlap or are unsorted")
			}
		}
	}
	if vis != x.visible || tomb != x.tombstones || nodes != x.nodes || indexed != nodes {
		t.Fatalf("counters: visible %d/%d tomb %d/%d nodes %d/%d indexed %d", vis, x.visible, tomb, x.tombstones, nodes, x.nodes, indexed)
	}
	if len([]rune(x.String())) != x.Len() {
		t.Fatal("Len disagrees with String")
	}
}

func TestTextMatchesReferenceModel(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 7))
	x := NewText("solo")
	var ref []rune
	for range 3000 {
		if len(ref) == 0 || rng.IntN(3) > 0 {
			pos := rng.IntN(len(ref) + 1)
			s := randomText(rng)
			if _, err := x.Insert(pos, s); err != nil {
				t.Fatal(err)
			}
			ref = slices.Insert(ref, pos, []rune(s)...)
		} else {
			pos := rng.IntN(len(ref))
			n := 1 + rng.IntN(min(len(ref)-pos, 10))
			if _, err := x.Delete(pos, n); err != nil {
				t.Fatal(err)
			}
			ref = slices.Delete(ref, pos, pos+n)
		}
	}
	if x.String() != string(ref) {
		t.Fatal("text differs from reference")
	}
	checkTextInvariants(t, x)
	st := x.CollectGarbage(nil)
	if x.Stats().Tombstones != 0 || x.String() != string(ref) {
		t.Fatalf("single-replica GC: %+v", st)
	}
}

func TestTextRunLengthEncoding(t *testing.T) {
	x := NewText("a")
	for i, c := range "hello world" {
		if _, err := x.Insert(i, string(c)); err != nil {
			t.Fatal(err)
		}
	}
	if x.Stats().Nodes != 1 {
		t.Fatalf("sequential typing produced %d nodes, want 1", x.Stats().Nodes)
	}
	if _, err := x.Delete(5, 1); err != nil {
		t.Fatal(err)
	}
	if x.Stats().Nodes != 3 {
		t.Fatalf("delete in the middle: %d nodes, want 3", x.Stats().Nodes)
	}
	x.CollectGarbage(nil)
	if s := x.Stats(); s.Nodes != 2 || s.Tombstones != 0 {
		t.Fatalf("after GC: %+v", s)
	}
	if x.String() != "helloworld" {
		t.Fatal(x.String())
	}
}

func TestTextConcurrentInsertsDoNotInterleave(t *testing.T) {
	for seed := range 50 {
		rng := rand.New(rand.NewPCG(uint64(seed), 1))
		a, b := NewText("a"), NewText("b")
		base, _ := a.Insert(0, "[]")
		mustApply(t, b, base)
		var opsA, opsB []TextOp
		for i, c := range "alpha" {
			op, _ := a.Insert(1+i, string(c))
			opsA = append(opsA, op)
		}
		for i, c := range "omega" {
			op, _ := b.Insert(1+i, string(c))
			opsB = append(opsB, op)
		}
		rng.Shuffle(len(opsA), func(i, j int) { opsA[i], opsA[j] = opsA[j], opsA[i] })
		rng.Shuffle(len(opsB), func(i, j int) { opsB[i], opsB[j] = opsB[j], opsB[i] })
		mustApply(t, a, opsB...)
		mustApply(t, b, opsA...)
		if a.String() != b.String() {
			t.Fatalf("diverged: %q vs %q", a.String(), b.String())
		}
		if s := a.String(); s != "[alphaomega]" && s != "[omegaalpha]" {
			t.Fatalf("interleaved: %q", s)
		}
	}
}

func TestTextRejectsBadOps(t *testing.T) {
	x := NewText("x")
	op, _ := x.Insert(0, "abc")
	y := NewText("y")
	mustApply(t, y, op)
	// The same character ids under a different operation id.
	dup := op
	dup.ID.Seq = 2
	if _, err := y.Apply(dup); !errors.Is(err, ErrInvalidOp) {
		t.Fatalf("duplicate ids: %v", err)
	}
	for _, bad := range []TextOp{
		{ID: Dot{"q", 1}, Kind: "ins", Lamport: 1},
		{ID: Dot{"q", 1}, Kind: "ins", Lamport: 1, Text: "\xff"},
		{ID: Dot{"q", 1}, Kind: "del"},
		{ID: Dot{"q", 1}, Kind: "del", Ranges: []IDRange{{Replica: "x", Lamport: 1}}},
		{ID: Dot{"q", 1}, Kind: "zap"},
		{ID: Dot{"q", 0}, Kind: "ins", Lamport: 1, Text: "a"},
		{ID: Dot{"q", 1}, Kind: "ins", Lamport: 1, Text: "a", Origin: CharID{Lamport: 1}},
		{ID: Dot{"q", 1}, Kind: "ins", Lamport: ^uint64(0), Text: "ab"},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if _, err := x.Insert(9, "z"); !errors.Is(err, ErrOutOfRange) {
		t.Fatal(err)
	}
	if _, err := x.Delete(2, 5); !errors.Is(err, ErrOutOfRange) {
		t.Fatal(err)
	}
}

func TestTextPendingBufferBounded(t *testing.T) {
	src := NewText("s")
	var ops []TextOp
	for i := range 10 {
		op, _ := src.Insert(i, "x")
		ops = append(ops, op)
	}
	dst := NewText("d")
	dst.SetMaxPending(3)
	if _, err := dst.Apply(ops[5:]...); !errors.Is(err, ErrTooManyPending) {
		t.Fatalf("want ErrTooManyPending, got %v", err)
	}
	if n := dst.DropPending("s"); n != 3 {
		t.Fatalf("dropped %d", n)
	}
	mustApply(t, dst, ops...)
	if dst.String() != src.String() {
		t.Fatal("did not recover")
	}
}

func TestDeltaSyncMinimality(t *testing.T) {
	rng := rand.New(rand.NewPCG(42, 42))
	for round := range 40 {
		a, b := NewText("a"), NewText("b")
		// Shared history, then divergent histories.
		for range 1 + rng.IntN(20) {
			op, _ := a.Insert(rng.IntN(a.Len()+1), randomText(rng))
			mustApply(t, b, op)
		}
		for range rng.IntN(30) {
			if a.Len() > 0 && rng.IntN(3) == 0 {
				_, _ = a.Delete(rng.IntN(a.Len()), 1)
			} else {
				_, _ = a.Insert(rng.IntN(a.Len()+1), randomText(rng))
			}
		}
		for range rng.IntN(30) {
			_, _ = b.Insert(rng.IntN(b.Len()+1), randomText(rng))
		}
		vb := b.VersionVector()
		delta, err := a.Delta(vb)
		if err != nil {
			t.Fatal(err)
		}
		want := a.VersionVector().Missing(vb)
		if uint64(len(delta)) != want {
			t.Fatalf("round %d: delta has %d ops, peer misses %d", round, len(delta), want)
		}
		for _, op := range delta {
			if vb.Contains(op.ID) {
				t.Fatalf("delta resends %s", op.ID)
			}
		}
		// Causal order: applying the delta never needs buffering.
		applied, err := b.Apply(delta...)
		if err != nil || len(applied) != len(delta) || b.Stats().Pending != 0 {
			t.Fatalf("delta not causally ordered: applied %d of %d, err %v", len(applied), len(delta), err)
		}
		back, _ := b.Delta(a.VersionVector())
		mustApply(t, a, back...)
		if a.String() != b.String() {
			t.Fatal("diverged after delta sync")
		}
		if d, _ := a.Delta(b.VersionVector()); len(d) != 0 {
			t.Fatalf("delta to an up-to-date peer has %d ops", len(d))
		}
	}
}

func TestDeltaAfterCompactionNeedsSnapshot(t *testing.T) {
	a, b := NewText("a"), NewText("b")
	op1, _ := a.Insert(0, "hello")
	mustApply(t, b, op1)
	op2, _ := a.Delete(0, 2)
	mustApply(t, b, op2)
	stale := VersionVector{}
	st := a.CollectGarbage(map[string]VersionVector{"b": b.VersionVector()})
	if st.CompactedOps != 2 || st.RemovedRunes != 2 {
		t.Fatalf("gc: %+v", st)
	}
	if _, err := a.Delta(stale); !errors.Is(err, ErrSnapshotRequired) {
		t.Fatalf("want ErrSnapshotRequired, got %v", err)
	}
	// A new replica starts from a snapshot and keeps converging.
	raw, _ := json.Marshal(a.Snapshot(true))
	var snap TextSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	c, err := RestoreText("c", snap)
	if err != nil {
		t.Fatal(err)
	}
	if c.String() != "llo" {
		t.Fatalf("restored %q", c.String())
	}
	opc, _ := c.Insert(3, "!")
	opa, _ := a.Insert(0, "¡")
	mustApply(t, a, opc)
	mustApply(t, c, opa)
	mustApply(t, b, opa, opc)
	if a.String() != c.String() || b.String() != c.String() || c.String() != "¡llo!" {
		t.Fatalf("%q %q %q", a.String(), b.String(), c.String())
	}
}

func TestGCWaitsForUnstableAndUnreceived(t *testing.T) {
	a, b := NewText("a"), NewText("b")
	op, _ := a.Insert(0, "abc")
	mustApply(t, b, op)
	del, _ := a.Delete(1, 1)
	// b has not seen the deletion: it is not stable.
	if st := a.CollectGarbage(map[string]VersionVector{"b": b.VersionVector()}); st.RemovedRunes != 0 {
		t.Fatalf("collected an unstable tombstone: %+v", st)
	}
	// b reports operations a has not received: unsafe to collect.
	bop, _ := b.Insert(2, "Z") // anchored on the tombstone-to-be
	mustApply(t, b, del)
	if st := a.CollectGarbage(map[string]VersionVector{"b": b.VersionVector()}); !st.SkippedUnsafe {
		t.Fatalf("collected while missing b's operations: %+v", st)
	}
	mustApply(t, a, bop)
	st := a.CollectGarbage(map[string]VersionVector{"b": b.VersionVector()})
	if st.SkippedUnsafe || a.Stats().Tombstones != 0 {
		t.Fatalf("expected collection: %+v %+v", st, a.Stats())
	}
	if a.String() != b.String() {
		t.Fatalf("%q vs %q", a.String(), b.String())
	}
}

func TestORSetConvergenceProperty(t *testing.T) {
	for seed := range 40 {
		rng := rand.New(rand.NewPCG(uint64(seed), 9))
		n := 2 + rng.IntN(4)
		reps := make([]*ORSet, n)
		for i := range reps {
			reps[i] = NewORSet(fmt.Sprintf("r%d", i))
		}
		net := newNetwork[SetOp](rng, n)
		elems := []string{"a", "b", "c", "d", "e"}
		for range 400 {
			i := rng.IntN(n)
			switch rng.IntN(3) {
			case 0:
				op, err := reps[i].Add(elems[rng.IntN(len(elems))])
				if err != nil {
					t.Fatal(err)
				}
				net.broadcast(i, op)
			case 1:
				if op, ok := reps[i].Remove(elems[rng.IntN(len(elems))]); ok {
					net.broadcast(i, op)
				}
			default:
				net.deliver(i, false, func(ops []SetOp) {
					if _, err := reps[i].Apply(ops...); err != nil {
						t.Fatal(err)
					}
				})
			}
		}
		for i := range reps {
			net.deliver(i, true, func(ops []SetOp) {
				if _, err := reps[i].Apply(ops...); err != nil {
					t.Fatal(err)
				}
			})
		}
		for i := range reps {
			if !slices.Equal(reps[i].Elements(), reps[0].Elements()) {
				t.Fatalf("seed %d: replica %d has %v, want %v", seed, i, reps[i].Elements(), reps[0].Elements())
			}
		}
	}
}

func TestORSetAddWins(t *testing.T) {
	a, b := NewORSet("a"), NewORSet("b")
	add, _ := a.Add("x")
	if _, err := b.Apply(add); err != nil {
		t.Fatal(err)
	}
	rem, _ := b.Remove("x")
	readd, _ := a.Add("x") // concurrent with the removal
	if _, err := a.Apply(rem); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Apply(readd); err != nil {
		t.Fatal(err)
	}
	if !a.Contains("x") || !b.Contains("x") {
		t.Fatal("concurrent add must win")
	}
	// A removal that arrives before the addition it observed waits.
	c := NewORSet("c")
	if out, err := c.Apply(rem); err != nil || len(out) != 0 {
		t.Fatalf("removal applied before its addition: %v %v", out, err)
	}
	if _, err := c.Apply(add, readd); err != nil {
		t.Fatal(err)
	}
	if !c.Contains("x") {
		t.Fatal("c lost x")
	}
	snap, _ := json.Marshal(c.Snapshot(true))
	var s ORSetSnapshot
	_ = json.Unmarshal(snap, &s)
	d, err := RestoreORSet("d", s)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !slices.Equal(d.Elements(), c.Elements()) {
		t.Fatalf("restore: %v want %v", d.Elements(), c.Elements())
	}
}

type fakeClock struct{ t time.Time }

func (f *fakeClock) now() time.Time { return f.t }

func TestLWWMapConvergenceProperty(t *testing.T) {
	for seed := range 40 {
		rng := rand.New(rand.NewPCG(uint64(seed), 3))
		n := 2 + rng.IntN(4)
		phys := make([]*fakeClock, n)
		reps := make([]*LWWMap, n)
		base := time.Unix(1_800_000_000, 0)
		for i := range reps {
			// Skewed physical clocks, within the drift bound.
			phys[i] = &fakeClock{t: base.Add(time.Duration(rng.IntN(5000)) * time.Millisecond)}
			reps[i] = NewLWWMap(fmt.Sprintf("r%d", i), NewClock(fmt.Sprintf("r%d", i), WithPhysicalClock(phys[i].now)))
		}
		net := newNetwork[MapOp](rng, n)
		keys := []string{"model", "mode", "title"}
		for step := range 400 {
			i := rng.IntN(n)
			if rng.IntN(4) == 0 {
				phys[i].t = phys[i].t.Add(time.Duration(rng.IntN(3)-1) * time.Millisecond) // may go backwards
			}
			switch rng.IntN(4) {
			case 0, 1:
				op, err := reps[i].Set(keys[rng.IntN(len(keys))], map[string]any{"v": step, "by": i})
				if err != nil {
					t.Fatal(err)
				}
				net.broadcast(i, op)
			case 2:
				op, _ := reps[i].Delete(keys[rng.IntN(len(keys))])
				net.broadcast(i, op)
			default:
				net.deliver(i, false, func(ops []MapOp) {
					if _, err := reps[i].Apply(ops...); err != nil {
						t.Fatal(err)
					}
				})
			}
		}
		for i := range reps {
			net.deliver(i, true, func(ops []MapOp) {
				if _, err := reps[i].Apply(ops...); err != nil {
					t.Fatal(err)
				}
			})
		}
		want, _ := json.Marshal(reps[0].Entries())
		for i := range reps {
			got, _ := json.Marshal(reps[i].Entries())
			if string(got) != string(want) {
				t.Fatalf("seed %d replica %d: %s want %s", seed, i, got, want)
			}
		}
	}
}

func TestLWWMapLaterWriteWins(t *testing.T) {
	pa := &fakeClock{t: time.Unix(100, 0)}
	pb := &fakeClock{t: time.Unix(50, 0)} // b's clock is far behind
	a := NewLWWMap("a", NewClock("a", WithPhysicalClock(pa.now)))
	b := NewLWWMap("b", NewClock("b", WithPhysicalClock(pb.now)))
	w1, _ := a.Set("k", "first")
	if _, err := b.Apply(w1); err != nil {
		t.Fatal(err)
	}
	// b saw a's write, so its next write is causally later and must win
	// despite b's slow physical clock.
	w2, _ := b.Set("k", "second")
	if _, err := a.Apply(w2); err != nil {
		t.Fatal(err)
	}
	for _, m := range []*LWWMap{a, b} {
		v, ok := m.Get("k")
		if !ok || string(v) != `"second"` {
			t.Fatalf("got %s", v)
		}
	}
	del, _ := a.Delete("k")
	if _, err := b.Apply(del); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.Get("k"); ok || len(b.Keys()) != 0 {
		t.Fatal("delete lost")
	}
	if _, err := b.Apply(w1); err != nil { // stale duplicate
		t.Fatal(err)
	}
	if _, ok := b.Get("k"); ok {
		t.Fatal("stale write resurrected the key")
	}
	snap, _ := json.Marshal(a.Snapshot(true))
	var s LWWMapSnapshot
	_ = json.Unmarshal(snap, &s)
	c, err := RestoreLWWMap("c", nil, s)
	if err != nil {
		t.Fatal(err)
	}
	if c.Clock().Last().Compare(del.TS) < 0 {
		t.Fatal("restored clock is behind the snapshot")
	}
}

func TestHLCMonotonic(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	phys := &fakeClock{t: time.Unix(1000, 0)}
	c := NewClock("n", WithPhysicalClock(phys.now), WithMaxDrift(time.Second))
	prev := c.Now()
	for i := range 20000 {
		// Physical time jitters, stalls and jumps backwards.
		phys.t = phys.t.Add(time.Duration(rng.IntN(2001)-1000) * time.Microsecond)
		var ts Timestamp
		if i%3 == 0 {
			remote := Timestamp{Wall: phys.t.UnixNano() + int64(rng.IntN(1_500_000_000)) - 500_000_000, Logical: uint32(rng.IntN(10)), Node: "m"}
			got, err := c.Update(remote)
			if err != nil {
				if !errors.Is(err, ErrClockDrift) {
					t.Fatal(err)
				}
				continue
			}
			if got.Compare(remote) <= 0 {
				t.Fatalf("update %v not after remote %v", got, remote)
			}
			ts = got
		} else {
			ts = c.Now()
		}
		if ts.Compare(prev) <= 0 {
			t.Fatalf("step %d: %v not after %v", i, ts, prev)
		}
		if ts.Wall < phys.t.UnixNano() {
			t.Fatalf("wall %d behind physical %d", ts.Wall, phys.t.UnixNano())
		}
		prev = ts
	}
	// Drift beyond the bound is refused without moving the clock.
	before := c.Last()
	if _, err := c.Update(Timestamp{Wall: phys.t.Add(time.Hour).UnixNano(), Node: "evil"}); !errors.Is(err, ErrClockDrift) {
		t.Fatalf("want drift error, got %v", err)
	}
	if c.Last() != before {
		t.Fatal("refused update moved the clock")
	}
	// Logical overflow carries into the wall component.
	c2 := NewClock("o", WithPhysicalClock(func() time.Time { return time.Unix(0, 5) }))
	c2.wall, c2.logical = 5, ^uint32(0)
	if ts := c2.Now(); ts.Wall != 6 || ts.Logical != 0 {
		t.Fatalf("overflow: %v", ts)
	}
}

func TestVersionVector(t *testing.T) {
	a := VersionVector{"x": 2, "y": 1}
	b := VersionVector{"x": 1, "y": 3}
	if a.Compare(b) != Concurrent || a.Compare(a.Clone()) != Equal {
		t.Fatal("compare")
	}
	c := a.Clone()
	c.Merge(b)
	if c.Compare(a) != After || a.Compare(c) != Before {
		t.Fatal("merge")
	}
	if m := Meet(a, b); m.String() != "{x:1 y:1}" {
		t.Fatal(m)
	}
	if a.Missing(b) != 1 || b.Missing(a) != 2 || !a.Contains(Dot{"x", 2}) || a.Contains(Dot{"x", 3}) || a.Contains(Dot{"z", 0}) {
		t.Fatal("contains/missing")
	}
}
