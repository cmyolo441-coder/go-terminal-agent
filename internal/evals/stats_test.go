package evals

import (
	"math"
	"testing"
)

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestMeanStdDev(t *testing.T) {
	xs := []float64{2, 4, 4, 4, 5, 5, 7, 9}
	if Mean(xs) != 5 {
		t.Errorf("mean = %v", Mean(xs))
	}
	if !near(StdDev(xs), math.Sqrt(32.0/7), 1e-12) {
		t.Errorf("sample stddev = %v, want %v", StdDev(xs), math.Sqrt(32.0/7))
	}
	if Mean(nil) != 0 || StdDev([]float64{3}) != 0 {
		t.Error("degenerate inputs")
	}
	// Kahan summation: 1e8 + many tiny values.
	big := []float64{1e8}
	for i := 0; i < 1000; i++ {
		big = append(big, 1e-8)
	}
	// One ULP at 1e8 is ~1.5e-8, so allow a few; a naive sum is off by
	// ~5e-6 (each 1e-8 rounds up to a full ULP), which this still catches.
	if !near(Mean(big)*float64(len(big)), 1e8+1e-5, 1e-7) {
		t.Errorf("compensated sum lost precision: %v", Mean(big)*float64(len(big)))
	}
}

func TestPassAtKKnownValues(t *testing.T) {
	cases := []struct {
		n, c, k        int
		passAt, passHt float64
	}{
		{10, 3, 1, 0.3, 0.3},
		{10, 3, 5, 1 - 21.0/252, 0},
		{10, 3, 2, 1 - 21.0/45, 3.0 / 45},
		{10, 3, 3, 1 - 35.0/120, 1.0 / 120},
		{5, 5, 5, 1, 1},
		{5, 0, 3, 0, 0},
		{4, 2, 3, 1, 0}, // n-c < k: at least one pass is guaranteed
		{1, 1, 1, 1, 1},
		{200, 37, 10, 1 - comb(163, 10)/comb(200, 10), comb(37, 10) / comb(200, 10)},
	}
	for _, c := range cases {
		if got := PassAtK(c.n, c.c, c.k); !near(got, c.passAt, 1e-12) {
			t.Errorf("pass@%d(n=%d,c=%d) = %.15f, want %.15f", c.k, c.n, c.c, got, c.passAt)
		}
		if got := PassHatK(c.n, c.c, c.k); !near(got, c.passHt, 1e-12) {
			t.Errorf("pass^%d(n=%d,c=%d) = %.15g, want %.15g", c.k, c.n, c.c, got, c.passHt)
		}
	}
	if !math.IsNaN(PassAtK(3, 1, 4)) || !math.IsNaN(PassHatK(0, 0, 1)) {
		t.Error("out-of-range k should be NaN")
	}
}

func comb(n, k int) float64 {
	return math.Round(math.Exp(lnChoose(n, k)))
}

func TestQuantileType7(t *testing.T) {
	xs := []float64{1, 2, 3, 4}
	// NumPy: np.quantile([1,2,3,4], [0.25, 0.5, 0.9]) -> 1.75, 2.5, 3.7
	for q, want := range map[float64]float64{0.25: 1.75, 0.5: 2.5, 0.9: 3.7, 0: 1, 1: 4} {
		if got := Quantile(xs, q); !near(got, want, 1e-12) {
			t.Errorf("Quantile(%v) = %v, want %v", q, got, want)
		}
	}
}

func TestBootstrapMeanCI(t *testing.T) {
	if ci := BootstrapMeanCI([]float64{0.7, 0.7, 0.7}, 0.95, 1000, 1); ci.Low != 0.7 || ci.High != 0.7 {
		t.Errorf("constant data CI = %+v", ci)
	}
	if ci := BootstrapMeanCI([]float64{0.4}, 0.95, 1000, 1); ci.Low != 0.4 || ci.High != 0.4 {
		t.Errorf("single value CI = %+v", ci)
	}
	// Bernoulli data with p=0.5, n=100: the CI half-width should be close
	// to the normal-theory 1.96*sqrt(.25/100) = 0.098.
	xs := make([]float64, 100)
	for i := range xs {
		if i%2 == 0 {
			xs[i] = 1
		}
	}
	ci := BootstrapMeanCI(xs, 0.95, 4000, 42)
	if ci.Low > 0.5 || ci.High < 0.5 {
		t.Fatalf("CI %+v excludes the mean", ci)
	}
	if hw := (ci.High - ci.Low) / 2; !near(hw, 0.098, 0.012) {
		t.Errorf("half-width %v, want ≈0.098", hw)
	}
	if again := BootstrapMeanCI(xs, 0.95, 4000, 42); again != ci {
		t.Error("bootstrap is not deterministic for a fixed seed")
	}
	if other := BootstrapMeanCI(xs, 0.95, 4000, 43); other == ci {
		t.Log("different seeds happened to agree (unlikely but harmless)")
	}
}

func TestMcNemarKnownValues(t *testing.T) {
	cases := []struct {
		b, c   int
		p      float64
		method string
	}{
		{10, 2, 0.03857421875, "exact"}, // 2 * (1+12+66)/4096
		{3, 3, 1, "exact"},
		{0, 5, 0.0625, "exact"},
		{0, 0, 1, "exact"},
		// chi2 = (|30-12|-1)^2 / 42 = 6.880952..., p = 0.0087119129623796
		{30, 12, 0.008711912962379598, "chi2-cc"},
	}
	for _, c := range cases {
		r := McNemar(c.b, c.c)
		if r.Method != c.method || !near(r.PValue, c.p, 1e-12) {
			t.Errorf("McNemar(%d,%d) = %s p=%.15g, want %s p=%.15g", c.b, c.c, r.Method, r.PValue, c.method, c.p)
		}
	}
	if r := McNemar(30, 12); !near(r.Statistic, 289.0/42, 1e-12) {
		t.Errorf("chi2 statistic = %v", r.Statistic)
	}
}

func TestWilcoxonKnownValues(t *testing.T) {
	// Darwin's Zea mays data, the scipy.stats.wilcoxon documentation
	// example: statistic 24.0, exact two-sided p = 0.041259765625.
	d := []float64{6, 8, 14, 16, 23, 24, 28, 29, 41, -48, 49, 56, 60, -67, 75}
	r := Wilcoxon(d)
	if r.Method != "exact" || r.Statistic != 24 || r.WPlus != 96 || r.WMinus != 24 || r.N != 15 {
		t.Fatalf("got %+v", r)
	}
	if !near(r.PValue, 0.041259765625, 1e-12) {
		t.Errorf("p = %.15g, want 0.041259765625", r.PValue)
	}
	// Sign symmetry.
	neg := make([]float64, len(d))
	for i, v := range d {
		neg[i] = -v
	}
	if rn := Wilcoxon(neg); !near(rn.PValue, r.PValue, 1e-15) || rn.WPlus != 24 {
		t.Errorf("negated data: %+v", rn)
	}

	// Ties and a zero: exact conditional distribution, verified by full
	// enumeration of the 2^9 sign assignments: W+ = 35.5, p = 0.1484375.
	rt := Wilcoxon([]float64{1, 1, -1, 2, 2, 2, -3, 3, 0, 4})
	if rt.N != 9 || rt.Zeros != 1 || rt.WPlus != 35.5 || rt.WMinus != 9.5 || !near(rt.PValue, 0.1484375, 1e-12) {
		t.Errorf("ties: %+v", rt)
	}

	// Large n uses the tie- and continuity-corrected normal
	// approximation: reference z = 1.6457196604697504,
	// p = 0.09982148935358282 (computed independently).
	var big []float64
	for i := 0; i < 60; i++ {
		v := float64((i*37)%23) - 9.5
		if i%3 == 0 {
			v += 0.25
		}
		big = append(big, v)
	}
	rb := Wilcoxon(big)
	if rb.Method != "normal" || rb.N != 60 || rb.WPlus != 1139 {
		t.Fatalf("large: %+v", rb)
	}
	if !near(rb.Z, 1.6457196604697504, 1e-9) || !near(rb.PValue, 0.09982148935358282, 1e-9) {
		t.Errorf("large: z=%.15g p=%.15g", rb.Z, rb.PValue)
	}

	if r0 := Wilcoxon([]float64{0, 0}); r0.PValue != 1 || r0.N != 0 {
		t.Errorf("all zeros: %+v", r0)
	}
	// Smallest possible exact p for n=5 all positive: 2/32.
	if r5 := Wilcoxon([]float64{1, 2, 3, 4, 5}); !near(r5.PValue, 0.0625, 1e-15) {
		t.Errorf("n=5: %+v", r5)
	}
}
