package fleet

import (
	"math"
	"slices"
	"sort"
	"sync"
)

// DefaultCompression is the t-digest compression used by metrics. Higher
// values keep more centroids and give more accurate quantiles.
const DefaultCompression = 200

// TDigest is a merging t-digest (Dunning & Ertl): a streaming sketch that
// estimates quantiles of an unbounded stream in bounded memory, with error
// that shrinks towards the tails, where p95/p99 live. It is safe for
// concurrent use.
type TDigest struct {
	mu          sync.Mutex
	compression float64
	centroids   []centroid
	buf         []centroid
	total       float64 // weight in centroids
	min, max    float64
}

type centroid struct {
	Mean  float64 `json:"m"`
	Count float64 `json:"c"`
}

// NewTDigest returns an empty digest; compression <= 0 selects
// DefaultCompression.
func NewTDigest(compression float64) *TDigest {
	if compression <= 0 {
		compression = DefaultCompression
	}
	return &TDigest{
		compression: compression,
		min:         math.Inf(1),
		max:         math.Inf(-1),
	}
}

// Add records one observation. NaN and infinities are ignored.
func (d *TDigest) Add(x float64) { d.AddWeighted(x, 1) }

// AddWeighted records x with weight w > 0.
func (d *TDigest) AddWeighted(x, w float64) {
	if math.IsNaN(x) || math.IsInf(x, 0) || !(w > 0) {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.buf = append(d.buf, centroid{x, w})
	d.min = math.Min(d.min, x)
	d.max = math.Max(d.max, x)
	if len(d.buf) >= d.bufferCap() {
		d.flushLocked()
	}
}

func (d *TDigest) bufferCap() int { return int(5*d.compression) + 16 }

// Count returns the total weight observed.
func (d *TDigest) Count() float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.flushLocked()
	return d.total
}

// Merge folds other into d.
func (d *TDigest) Merge(other *TDigest) {
	if other == nil || other == d {
		return
	}
	other.mu.Lock()
	other.flushLocked()
	cs := slices.Clone(other.centroids)
	mn, mx := other.min, other.max
	other.mu.Unlock()

	d.mu.Lock()
	defer d.mu.Unlock()
	d.buf = append(d.buf, cs...)
	d.min = math.Min(d.min, mn)
	d.max = math.Max(d.max, mx)
	d.flushLocked()
}

// Quantile estimates the q-quantile (0 <= q <= 1). It returns NaN when the
// digest is empty.
func (d *TDigest) Quantile(q float64) float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.flushLocked()
	return d.quantileLocked(q)
}

// Quantiles estimates several quantiles under one lock.
func (d *TDigest) Quantiles(qs ...float64) []float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.flushLocked()
	out := make([]float64, len(qs))
	for i, q := range qs {
		out[i] = d.quantileLocked(q)
	}
	return out
}

// Min and Max return the extremes observed (NaN when empty).
func (d *TDigest) Min() float64 { return d.extreme(true) }

// Max returns the largest observation (NaN when empty).
func (d *TDigest) Max() float64 { return d.extreme(false) }

func (d *TDigest) extreme(min bool) float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.total == 0 && len(d.buf) == 0 {
		return math.NaN()
	}
	if min {
		return d.min
	}
	return d.max
}

// Centroids reports the number of centroids held (for tests and sizing).
func (d *TDigest) Centroids() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.flushLocked()
	return len(d.centroids)
}

// Reset empties the digest.
func (d *TDigest) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.centroids, d.buf, d.total = nil, nil, 0
	d.min, d.max = math.Inf(1), math.Inf(-1)
}

// kScale is the k1 scale function δ/(2π)·asin(2q−1); it makes centroids
// small near q=0 and q=1 and large in the middle.
func (d *TDigest) kScale(q float64) float64 {
	return d.compression / (2 * math.Pi) * math.Asin(2*q-1)
}

func (d *TDigest) kInverse(k float64) float64 {
	x := k * 2 * math.Pi / d.compression
	if x >= math.Pi/2 {
		return 1
	}
	if x <= -math.Pi/2 {
		return 0
	}
	return (math.Sin(x) + 1) / 2
}

func (d *TDigest) flushLocked() {
	if len(d.buf) == 0 {
		return
	}
	all := append(d.centroids, d.buf...)
	d.buf = d.buf[:0]
	sort.Slice(all, func(i, j int) bool { return all[i].Mean < all[j].Mean })
	total := 0.0
	for _, c := range all {
		total += c.Count
	}
	merged := make([]centroid, 0, len(all)/2+1)
	cur := all[0]
	soFar := 0.0
	limit := d.kInverse(d.kScale(0) + 1)
	for _, c := range all[1:] {
		if (soFar+cur.Count+c.Count)/total <= limit {
			// Weighted incremental mean keeps precision for large counts.
			cur.Count += c.Count
			cur.Mean += (c.Mean - cur.Mean) * c.Count / cur.Count
			continue
		}
		soFar += cur.Count
		merged = append(merged, cur)
		limit = d.kInverse(d.kScale(soFar/total) + 1)
		cur = c
	}
	merged = append(merged, cur)
	d.centroids = merged
	d.total = total
}

// quantileLocked interpolates linearly between centroid centres, anchored
// at the observed min and max; single-weight centroids are exact points.
func (d *TDigest) quantileLocked(q float64) float64 {
	n := len(d.centroids)
	if n == 0 {
		return math.NaN()
	}
	switch {
	case q <= 0:
		return d.min
	case q >= 1:
		return d.max
	case n == 1:
		return d.centroids[0].Mean
	}
	index := q * d.total
	cs := d.centroids
	// Left tail: between min and the first centroid's centre.
	if c0 := cs[0]; index < c0.Count/2 {
		if c0.Count == 1 {
			return c0.Mean
		}
		return d.min + (c0.Mean-d.min)*index/(c0.Count/2)
	}
	cum := cs[0].Count / 2
	for i := 0; i < n-1; i++ {
		a, b := cs[i], cs[i+1]
		step := a.Count/2 + b.Count/2
		if index < cum+step {
			// Singletons own a unit-wide neighbourhood exactly.
			left, right := 0.0, 0.0
			if a.Count == 1 {
				left = 0.5
			}
			if b.Count == 1 {
				right = 0.5
			}
			z1 := index - cum - left
			z2 := cum + step - index - right
			if z1 < 0 {
				return a.Mean
			}
			if z2 < 0 {
				return b.Mean
			}
			if z1+z2 == 0 {
				return (a.Mean + b.Mean) / 2
			}
			return (a.Mean*z2 + b.Mean*z1) / (z1 + z2)
		}
		cum += step
	}
	last := cs[n-1]
	if last.Count == 1 {
		return last.Mean
	}
	rem := d.total - index
	return d.max - (d.max-last.Mean)*rem/(last.Count/2)
}
