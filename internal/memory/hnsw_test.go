package memory

import (
	"errors"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
)

func randomUnitVectors(rng *rand.Rand, n, dim int) [][]float32 {
	out := make([][]float32, n)
	for i := range out {
		v := make([]float32, dim)
		for j := range v {
			v[j] = float32(rng.NormFloat64())
		}
		normalize(v)
		out[i] = v
	}
	return out
}

func bruteForce(vecs [][]float32, live func(int) bool, q []float32, k int) []uint64 {
	type kd struct {
		k uint64
		d float32
	}
	all := make([]kd, 0, len(vecs))
	for i, v := range vecs {
		if live != nil && !live(i) {
			continue
		}
		all = append(all, kd{uint64(i), cosineDistance(q, v)})
	}
	slices.SortFunc(all, func(a, b kd) int {
		switch {
		case a.d < b.d:
			return -1
		case a.d > b.d:
			return 1
		}
		return 0
	})
	out := make([]uint64, 0, k)
	for i := 0; i < k && i < len(all); i++ {
		out = append(out, all[i].k)
	}
	return out
}

func recallAt(got []Neighbor, want []uint64) float64 {
	hit := 0
	for _, g := range got {
		if slices.Contains(want, g.Key) {
			hit++
		}
	}
	return float64(hit) / float64(len(want))
}

func buildHNSW(t testing.TB, vecs [][]float32, cfg HNSWConfig) *HNSW {
	t.Helper()
	h, err := NewHNSW(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range vecs {
		if err := h.Insert(uint64(i), v); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func TestHNSWRecallAt10(t *testing.T) {
	const (
		n       = 10000
		dim     = 32
		queries = 200
		k       = 10
	)
	rng := rand.New(rand.NewPCG(1, 2))
	vecs := randomUnitVectors(rng, n, dim)
	h := buildHNSW(t, vecs, HNSWConfig{Dim: dim, M: 16, EfConstruction: 100, EfSearch: 100, Seed: 7})
	if h.Len() != n {
		t.Fatalf("Len = %d, want %d", h.Len(), n)
	}
	qs := randomUnitVectors(rng, queries, dim)
	var total float64
	for _, q := range qs {
		got := h.Search(q, k, 0, nil)
		if len(got) != k {
			t.Fatalf("got %d results, want %d", len(got), k)
		}
		for i := 1; i < len(got); i++ {
			if got[i].Distance < got[i-1].Distance {
				t.Fatalf("results not sorted: %v", got)
			}
		}
		total += recallAt(got, bruteForce(vecs, nil, q, k))
	}
	recall := total / queries
	t.Logf("recall@10 over %d vectors (dim %d): %.4f, max level %d", n, dim, recall, h.MaxLevel())
	if recall < 0.95 {
		t.Fatalf("recall@10 = %.4f, want >= 0.95", recall)
	}
}

func TestHNSWDeleteAndRebuild(t *testing.T) {
	const n, dim, k = 3000, 24, 10
	rng := rand.New(rand.NewPCG(3, 4))
	vecs := randomUnitVectors(rng, n, dim)
	h := buildHNSW(t, vecs, HNSWConfig{Dim: dim, EfConstruction: 128, EfSearch: 128, Seed: 9})
	dead := map[int]bool{}
	for i := 0; i < n; i += 3 {
		if !h.Delete(uint64(i)) {
			t.Fatalf("Delete(%d) = false", i)
		}
		dead[i] = true
	}
	if h.Delete(0) {
		t.Fatal("double delete reported live")
	}
	if h.Tombstones() != len(dead) || h.Len() != n-len(dead) {
		t.Fatalf("tombstones %d len %d", h.Tombstones(), h.Len())
	}
	if !h.NeedsRebuild(0.2, 10) {
		t.Fatal("NeedsRebuild = false with a third deleted")
	}
	live := func(i int) bool { return !dead[i] }
	check := func(stage string) {
		qs := randomUnitVectors(rng, 50, dim)
		var total float64
		for _, q := range qs {
			got := h.Search(q, k, 0, nil)
			for _, g := range got {
				if dead[int(g.Key)] {
					t.Fatalf("%s: deleted key %d returned", stage, g.Key)
				}
			}
			total += recallAt(got, bruteForce(vecs, live, q, k))
		}
		if r := total / 50; r < 0.93 {
			t.Fatalf("%s: recall %.3f", stage, r)
		}
	}
	check("tombstoned")
	h.Rebuild()
	if h.Tombstones() != 0 || h.Len() != n-len(dead) {
		t.Fatalf("after rebuild: tombstones %d len %d", h.Tombstones(), h.Len())
	}
	check("rebuilt")
}

func TestHNSWFilterAndUpsert(t *testing.T) {
	const n, dim = 2000, 16
	rng := rand.New(rand.NewPCG(5, 6))
	vecs := randomUnitVectors(rng, n, dim)
	h := buildHNSW(t, vecs, HNSWConfig{Dim: dim, Seed: 1})
	q := vecs[42]
	got := h.Search(q, 5, 0, func(key uint64) bool { return key%7 == 0 })
	if len(got) != 5 {
		t.Fatalf("filtered search returned %d", len(got))
	}
	want := bruteForce(vecs, func(i int) bool { return i%7 == 0 }, q, 5)
	if r := recallAt(got, want); r < 0.8 {
		t.Fatalf("filtered recall %.2f: got %v want %v", r, got, want)
	}
	// Upsert moves key 42 far away from its old position.
	far := make([]float32, dim)
	for i := range far {
		far[i] = -q[i]
	}
	if err := h.Insert(42, far); err != nil {
		t.Fatal(err)
	}
	if h.Len() != n || h.Tombstones() != 1 {
		t.Fatalf("upsert: len %d tombstones %d", h.Len(), h.Tombstones())
	}
	top := h.Search(far, 1, 0, nil)
	if len(top) != 1 || top[0].Key != 42 || top[0].Distance > 1e-5 {
		t.Fatalf("upserted vector not found: %v", top)
	}
	if err := h.Insert(1, make([]float32, dim)); !errors.Is(err, ErrDimension) {
		t.Fatalf("zero vector: err = %v", err)
	}
	if err := h.Insert(1, make([]float32, dim+1)); !errors.Is(err, ErrDimension) {
		t.Fatalf("wrong dim: err = %v", err)
	}
}

func TestHNSWBinaryRoundTrip(t *testing.T) {
	const n, dim = 500, 16
	rng := rand.New(rand.NewPCG(7, 8))
	vecs := randomUnitVectors(rng, n, dim)
	h := buildHNSW(t, vecs, HNSWConfig{Dim: dim, Seed: 2})
	h.Delete(10)
	data := h.appendBinary(nil)
	vecOf := func(key uint64) []float32 { v, _ := h.Vector(key); return v }
	h2, _ := NewHNSW(HNSWConfig{Dim: dim, Seed: 3})
	if err := h2.loadBinary(data, vecOf); err != nil {
		t.Fatal(err)
	}
	if h2.Len() != h.Len() || h2.Tombstones() != 1 || h2.MaxLevel() != h.MaxLevel() {
		t.Fatalf("decoded graph differs: len %d/%d", h2.Len(), h.Len())
	}
	for _, q := range randomUnitVectors(rng, 20, dim) {
		if a, b := h.Search(q, 10, 0, nil), h2.Search(q, 10, 0, nil); !slices.Equal(a, b) {
			t.Fatalf("search differs after round trip:\n%v\n%v", a, b)
		}
	}
	// A live key without a vector is corrupt.
	if err := h2.loadBinary(data, func(uint64) []float32 { return nil }); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("missing vectors: err = %v", err)
	}
	// Every truncation must be rejected, never panic.
	for cut := 0; cut < len(data); cut += 1 + len(data)/300 {
		h3, _ := NewHNSW(HNSWConfig{Dim: dim})
		if err := h3.loadBinary(data[:cut], vecOf); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("truncated at %d: err = %v", cut, err)
		}
	}
}

func TestHNSWConcurrentSearchInsert(t *testing.T) {
	const dim = 16
	rng := rand.New(rand.NewPCG(9, 10))
	vecs := randomUnitVectors(rng, 1200, dim)
	h := buildHNSW(t, vecs[:200], HNSWConfig{Dim: dim, EfConstruction: 64, Seed: 4})
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Go(func() {
			for i := 0; i < 200; i++ {
				if res := h.Search(vecs[(i*7+w)%len(vecs)], 5, 32, nil); len(res) == 0 {
					t.Error("empty search result")
					return
				}
			}
		})
	}
	wg.Go(func() {
		for i := 200; i < len(vecs); i++ {
			if err := h.Insert(uint64(i), vecs[i]); err != nil {
				t.Error(err)
				return
			}
			if i%50 == 0 {
				h.Delete(uint64(i - 100))
			}
		}
	})
	wg.Wait()
	if h.Len()+h.Tombstones() != len(vecs) {
		t.Fatalf("len %d + tombstones %d != %d", h.Len(), h.Tombstones(), len(vecs))
	}
}

func BenchmarkHNSWInsert(b *testing.B) {
	for _, dim := range []int{128, 1024} {
		b.Run(dimName(dim), func(b *testing.B) {
			rng := rand.New(rand.NewPCG(1, 1))
			vecs := randomUnitVectors(rng, 4096, dim)
			h, _ := NewHNSW(HNSWConfig{Dim: dim, Seed: 1})
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = h.Insert(uint64(i), vecs[i%len(vecs)])
			}
		})
	}
}

func BenchmarkHNSWSearch(b *testing.B) {
	for _, dim := range []int{128, 1024} {
		b.Run(dimName(dim), func(b *testing.B) {
			rng := rand.New(rand.NewPCG(2, 2))
			vecs := randomUnitVectors(rng, 10000, dim)
			h := buildHNSW(b, vecs, HNSWConfig{Dim: dim, EfConstruction: 100, Seed: 1})
			qs := randomUnitVectors(rng, 256, dim)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				h.Search(qs[i%len(qs)], 10, 64, nil)
			}
		})
	}
}

func BenchmarkBruteForce10k(b *testing.B) {
	rng := rand.New(rand.NewPCG(3, 3))
	vecs := randomUnitVectors(rng, 10000, 1024)
	q := randomUnitVectors(rng, 1, 1024)[0]
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bruteForce(vecs, nil, q, 10)
	}
}

func BenchmarkDot1024(b *testing.B) {
	rng := rand.New(rand.NewPCG(4, 4))
	v := randomUnitVectors(rng, 2, 1024)
	b.SetBytes(2 * 4 * 1024)
	var s float32
	for i := 0; i < b.N; i++ {
		s += dot(v[0], v[1])
	}
	_ = s
}

func dimName(d int) string {
	return "dim" + string(rune('0'+d/1000%10)) + string(rune('0'+d/100%10)) + string(rune('0'+d/10%10)) + string(rune('0'+d%10))
}
