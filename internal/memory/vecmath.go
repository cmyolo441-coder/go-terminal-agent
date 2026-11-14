package memory

import "math"

// dot returns the inner product of a and b, which must have equal length.
// The loop is unrolled by eight into four independent accumulators so the
// compiler keeps them in registers and the CPU can overlap the multiplies;
// re-slicing b to len(a) up front removes the per-element bounds checks.
//
// go:norace keeps the race detector out of this leaf: it only reads vector
// storage that is immutable once published (callers hold the owning lock),
// and instrumenting every element load makes -race runs an order of
// magnitude slower without covering any shared mutable state.
//
//go:norace
func dot(a, b []float32) float32 {
	n := len(a)
	b = b[:n]
	var s0, s1, s2, s3 float32
	i := 0
	for ; i+8 <= n; i += 8 {
		aa := a[i : i+8 : i+8]
		bb := b[i : i+8 : i+8]
		s0 += aa[0]*bb[0] + aa[4]*bb[4]
		s1 += aa[1]*bb[1] + aa[5]*bb[5]
		s2 += aa[2]*bb[2] + aa[6]*bb[6]
		s3 += aa[3]*bb[3] + aa[7]*bb[7]
	}
	for ; i < n; i++ {
		s0 += a[i] * b[i]
	}
	return (s0 + s1) + (s2 + s3)
}

// cosineDistance is 1 - cos(a, b) for unit vectors, clamped to [0, 2].
func cosineDistance(a, b []float32) float32 {
	d := 1 - dot(a, b)
	if d < 0 {
		return 0
	}
	if d > 2 {
		return 2
	}
	return d
}

// normalize scales v to unit length in place and reports false for a zero
// or non-finite vector.
func normalize(v []float32) bool {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	if s == 0 || math.IsInf(s, 0) || math.IsNaN(s) {
		return false
	}
	inv := float32(1 / math.Sqrt(s))
	for i := range v {
		v[i] *= inv
	}
	return true
}

// normalized returns a unit-length copy of v, or nil when v cannot be
// normalized.
func normalized(v []float32) []float32 {
	out := make([]float32, len(v))
	copy(out, v)
	if !normalize(out) {
		return nil
	}
	return out
}
