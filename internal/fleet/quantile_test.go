package fleet

import (
	"math"
	mrand "math/rand/v2"
	"slices"
	"sort"
	"testing"
)

// rankError is |rank(estimate)/n - q| against the exact sorted data.
func rankError(sorted []float64, est, q float64) float64 {
	lo := sort.SearchFloat64s(sorted, est)
	hi := sort.Search(len(sorted), func(i int) bool { return sorted[i] > est })
	r := (float64(lo) + float64(hi)) / 2 / float64(len(sorted))
	return math.Abs(r - q)
}

func TestTDigestAccuracy(t *testing.T) {
	rng := mrand.New(mrand.NewPCG(1, 2))
	dists := map[string]func() float64{
		"uniform":     func() float64 { return rng.Float64() },
		"lognormal":   func() float64 { return math.Exp(rng.NormFloat64()) },
		"exponential": func() float64 { return rng.ExpFloat64() },
		"bimodal": func() float64 {
			if rng.IntN(10) == 0 {
				return 1000 + rng.NormFloat64()
			}
			return 10 + rng.NormFloat64()
		},
	}
	tol := map[float64]float64{0.5: 0.01, 0.9: 0.006, 0.95: 0.004, 0.99: 0.002, 0.999: 0.0005}
	for name, gen := range dists {
		t.Run(name, func(t *testing.T) {
			d := NewTDigest(0)
			data := make([]float64, 200000)
			for i := range data {
				data[i] = gen()
				d.Add(data[i])
			}
			slices.Sort(data)
			for q, max := range tol {
				if e := rankError(data, d.Quantile(q), q); e > max {
					t.Errorf("q=%v: rank error %.5f > %.5f", q, e, max)
				}
			}
			if d.Count() != float64(len(data)) || d.Min() != data[0] || d.Max() != data[len(data)-1] {
				t.Errorf("count/min/max = %v/%v/%v", d.Count(), d.Min(), d.Max())
			}
			if n := d.Centroids(); n > 2*DefaultCompression {
				t.Errorf("%d centroids exceeds bound", n)
			}
		})
	}
}

func TestTDigestMergeAndEdges(t *testing.T) {
	d := NewTDigest(100)
	if !math.IsNaN(d.Quantile(0.5)) || !math.IsNaN(d.Min()) {
		t.Fatal("empty digest should report NaN")
	}
	d.Add(42)
	if d.Quantile(0.01) != 42 || d.Quantile(0.99) != 42 {
		t.Fatal("single value")
	}
	d.Add(math.NaN())
	d.Add(math.Inf(1))
	if d.Count() != 1 {
		t.Fatalf("NaN/Inf counted: %v", d.Count())
	}

	// Merging shards matches one digest over all data.
	parts := []*TDigest{NewTDigest(0), NewTDigest(0), NewTDigest(0), NewTDigest(0)}
	var all []float64
	for i := range 100000 {
		v := float64(i)
		all = append(all, v)
		parts[i%4].Add(v)
	}
	m := NewTDigest(0)
	for _, p := range parts {
		m.Merge(p)
	}
	for _, q := range []float64{0.5, 0.95, 0.99} {
		if e := rankError(all, m.Quantile(q), q); e > 0.01 {
			t.Errorf("merged q=%v rank error %.4f", q, e)
		}
	}
	m.Reset()
	if m.Count() != 0 {
		t.Fatal("reset")
	}
}
