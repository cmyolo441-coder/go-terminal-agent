package evals

import (
	"hash/fnv"
	"math"
	"math/rand/v2"
	"sort"
)

// Interval is a closed confidence interval.
type Interval struct {
	Low  float64 `json:"low"`
	High float64 `json:"high"`
}

// Mean is the arithmetic mean; 0 for no data.
func Mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	// Kahan summation keeps long runs of small scores accurate.
	var sum, comp float64
	for _, x := range xs {
		y := x - comp
		t := sum + y
		comp = (t - sum) - y
		sum = t
	}
	return sum / float64(len(xs))
}

// StdDev is the sample standard deviation (n-1 denominator); 0 for fewer
// than two values.
func StdDev(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	m := Mean(xs)
	var ss float64
	for _, x := range xs {
		d := x - m
		ss += d * d
	}
	return math.Sqrt(ss / float64(len(xs)-1))
}

// PassAtK is the unbiased estimator of the probability that at least one
// of k samples passes, given c passes in n runs (Chen et al. 2021):
// 1 - C(n-c, k) / C(n, k).
func PassAtK(n, c, k int) float64 {
	if n <= 0 || k <= 0 || c < 0 || c > n || k > n {
		return math.NaN()
	}
	if n-c < k {
		return 1
	}
	// 1 - prod_{i=n-c+1}^{n} (1 - k/i), numerically stable.
	p := 1.0
	for i := n - c + 1; i <= n; i++ {
		p *= 1 - float64(k)/float64(i)
	}
	return 1 - p
}

// PassHatK is the unbiased estimator of the probability that all k samples
// pass (pass^k, Yao et al. 2024): C(c, k) / C(n, k).
func PassHatK(n, c, k int) float64 {
	if n <= 0 || k <= 0 || c < 0 || c > n || k > n {
		return math.NaN()
	}
	if c < k {
		return 0
	}
	p := 1.0
	for i := 0; i < k; i++ {
		p *= float64(c-i) / float64(n-i)
	}
	return p
}

// Quantile returns the q-quantile of sorted data with linear interpolation
// between order statistics (Hyndman–Fan type 7, the R and NumPy default).
func Quantile(sorted []float64, q float64) float64 {
	n := len(sorted)
	if n == 0 {
		return math.NaN()
	}
	if q <= 0 {
		return sorted[0]
	}
	if q >= 1 {
		return sorted[n-1]
	}
	h := float64(n-1) * q
	lo := math.Floor(h)
	i := int(lo)
	if i+1 >= n {
		return sorted[n-1]
	}
	return sorted[i] + (h-lo)*(sorted[i+1]-sorted[i])
}

// DefaultBootstrapSamples is the resample count used when none is given.
const DefaultBootstrapSamples = 4000

// BootstrapMeanCI is the percentile bootstrap confidence interval for the
// mean at the given level (for example 0.95). It is deterministic for a
// given seed. With fewer than two values the interval collapses to the
// mean.
func BootstrapMeanCI(xs []float64, level float64, samples int, seed uint64) Interval {
	if len(xs) == 0 {
		return Interval{}
	}
	if allEqual(xs) {
		return Interval{xs[0], xs[0]}
	}
	if samples <= 0 {
		samples = DefaultBootstrapSamples
	}
	if level <= 0 || level >= 1 {
		level = 0.95
	}
	rng := rand.New(rand.NewPCG(seed, seed^0x9E3779B97F4A7C15))
	means := make([]float64, samples)
	n := len(xs)
	for s := range means {
		var sum float64
		for i := 0; i < n; i++ {
			sum += xs[rng.IntN(n)]
		}
		means[s] = sum / float64(n)
	}
	sort.Float64s(means)
	alpha := 1 - level
	return Interval{Low: Quantile(means, alpha/2), High: Quantile(means, 1-alpha/2)}
}

func allEqual(xs []float64) bool {
	for _, x := range xs[1:] {
		if x != xs[0] {
			return false
		}
	}
	return true
}

// seedFor derives a stable bootstrap seed from a label.
func seedFor(base uint64, label string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(label))
	return base ^ h.Sum64()
}

// McNemarResult is a paired test on pass/fail outcomes.
type McNemarResult struct {
	// B counts pairs that passed in the baseline and failed in the
	// candidate (regressions); C counts the reverse (improvements).
	B         int     `json:"b"`
	C         int     `json:"c"`
	Statistic float64 `json:"statistic"`
	PValue    float64 `json:"p_value"`
	// Method is "exact" (two-sided binomial on the discordant pairs) or
	// "chi2-cc" (chi-square with continuity correction).
	Method string `json:"method"`
}

// mcnemarExactBelow is the discordant-pair count under which the exact
// binomial form is used.
const mcnemarExactBelow = 25

// McNemar tests whether b and c differ more than chance allows.
func McNemar(b, c int) McNemarResult {
	r := McNemarResult{B: b, C: c}
	n := b + c
	if n == 0 {
		r.PValue, r.Method = 1, "exact"
		return r
	}
	if n < mcnemarExactBelow {
		r.Method = "exact"
		r.Statistic = float64(min(b, c))
		r.PValue = math.Min(1, 2*binomCDFHalf(min(b, c), n))
		return r
	}
	r.Method = "chi2-cc"
	d := math.Abs(float64(b-c)) - 1
	if d < 0 {
		d = 0
	}
	r.Statistic = d * d / float64(n)
	r.PValue = chi2SF1(r.Statistic)
	return r
}

// binomCDFHalf is P(X <= k) for X ~ Binomial(n, 1/2), computed in log
// space so large n does not overflow.
func binomCDFHalf(k, n int) float64 {
	if k < 0 {
		return 0
	}
	if k >= n {
		return 1
	}
	lnHalfN := float64(n) * math.Log(0.5)
	var sum float64
	for i := 0; i <= k; i++ {
		sum += math.Exp(lnChoose(n, i) + lnHalfN)
	}
	return math.Min(1, sum)
}

func lnChoose(n, k int) float64 {
	a, _ := math.Lgamma(float64(n + 1))
	b, _ := math.Lgamma(float64(k + 1))
	c, _ := math.Lgamma(float64(n - k + 1))
	return a - b - c
}

// chi2SF1 is the survival function of the chi-square distribution with one
// degree of freedom.
func chi2SF1(x float64) float64 {
	if x <= 0 {
		return 1
	}
	return math.Erfc(math.Sqrt(x / 2))
}

// normalSF2 is the two-sided normal tail probability for |z|.
func normalSF2(z float64) float64 {
	return math.Erfc(math.Abs(z) / math.Sqrt2)
}

// WilcoxonResult is a signed-rank test on paired differences.
type WilcoxonResult struct {
	// N is the number of non-zero differences used.
	N      int     `json:"n"`
	Zeros  int     `json:"zeros"`
	WPlus  float64 `json:"w_plus"`
	WMinus float64 `json:"w_minus"`
	// Statistic is min(W+, W-).
	Statistic float64 `json:"statistic"`
	// Z is the normal-approximation statistic (0 for the exact method).
	Z      float64 `json:"z"`
	PValue float64 `json:"p_value"`
	// Method is "exact" (enumerated null distribution, exact also with
	// ties) or "normal" (tie- and continuity-corrected approximation).
	Method string `json:"method"`
}

// wilcoxonExactMax is the largest N handled with the exact distribution.
const wilcoxonExactMax = 50

// Wilcoxon runs a two-sided Wilcoxon signed-rank test on paired
// differences (candidate minus baseline). Zero differences are dropped
// (Wilcoxon's method) and tied magnitudes get average ranks.
func Wilcoxon(diffs []float64) WilcoxonResult {
	type item struct {
		abs float64
		neg bool
	}
	var items []item
	r := WilcoxonResult{}
	for _, d := range diffs {
		if math.IsNaN(d) {
			continue
		}
		if math.Abs(d) < 1e-12 {
			r.Zeros++
			continue
		}
		items = append(items, item{abs: math.Abs(d), neg: d < 0})
	}
	n := len(items)
	r.N = n
	if n == 0 {
		r.PValue, r.Method = 1, "exact"
		return r
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].abs < items[j].abs })
	// Doubled average ranks are integers, which the exact DP needs.
	ranks2 := make([]int, n)
	var tieTerm float64
	for i := 0; i < n; {
		j := i
		for j+1 < n && math.Abs(items[j+1].abs-items[i].abs) <= 1e-12*math.Max(1, items[i].abs) {
			j++
		}
		// Ranks i+1..j+1 averaged, doubled: (i+1 + j+1).
		for k := i; k <= j; k++ {
			ranks2[k] = i + j + 2
		}
		t := float64(j - i + 1)
		tieTerm += t*t*t - t
		i = j + 1
	}
	wplus2 := 0
	for i, it := range items {
		if !it.neg {
			wplus2 += ranks2[i]
		}
	}
	total2 := n * (n + 1)
	r.WPlus = float64(wplus2) / 2
	r.WMinus = float64(total2-wplus2) / 2
	r.Statistic = math.Min(r.WPlus, r.WMinus)

	if n <= wilcoxonExactMax {
		r.Method = "exact"
		// dist[s] = P(doubled W+ = s) under H0 (each sign equally likely).
		dist := make([]float64, total2+1)
		dist[0] = 1
		maxS := 0
		for _, rk := range ranks2 {
			for s := maxS; s >= 0; s-- {
				if dist[s] == 0 {
					continue
				}
				dist[s+rk] += dist[s] * 0.5
				dist[s] *= 0.5
			}
			maxS += rk
		}
		var lower, upper float64
		for s, p := range dist {
			if s <= wplus2 {
				lower += p
			}
			if s >= wplus2 {
				upper += p
			}
		}
		r.PValue = math.Min(1, 2*math.Min(lower, upper))
		return r
	}

	r.Method = "normal"
	nf := float64(n)
	mean := nf * (nf + 1) / 4
	variance := nf*(nf+1)*(2*nf+1)/24 - tieTerm/48
	if variance <= 0 {
		r.PValue = 1
		return r
	}
	d := r.WPlus - mean
	// Continuity correction toward the mean.
	switch {
	case d > 0:
		d = math.Max(0, d-0.5)
	case d < 0:
		d = math.Min(0, d+0.5)
	}
	r.Z = d / math.Sqrt(variance)
	r.PValue = math.Min(1, normalSF2(r.Z))
	return r
}
