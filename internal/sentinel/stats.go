package sentinel

import "math"

// EWM is an exponentially weighted moving mean and variance (West's
// incremental EWMVar). The zero value with Alpha set is ready to use.
type EWM struct {
	Alpha float64
	Mean  float64
	Var   float64
	N     int
}

// Update folds x into the estimate.
func (e *EWM) Update(x float64) {
	if e.N == 0 {
		e.Mean, e.Var, e.N = x, 0, 1
		return
	}
	diff := x - e.Mean
	incr := e.Alpha * diff
	e.Mean += incr
	e.Var = (1 - e.Alpha) * (e.Var + diff*incr)
	e.N++
}

// Std is the standard deviation.
func (e *EWM) Std() float64 { return math.Sqrt(math.Max(e.Var, 0)) }

// Z is the z-score of x against the estimate, with the deviation floored
// at minStd so a perfectly regular baseline does not make every blip
// infinitely anomalous.
func (e *EWM) Z(x, minStd float64) float64 {
	return (x - e.Mean) / math.Max(e.Std(), math.Max(minStd, 1e-12))
}

// Holt is double exponential smoothing (level plus linear trend) with an
// EWM of the one-step-ahead forecast residuals, so anomalies are measured
// against where the series was heading rather than where it has been.
type Holt struct {
	Alpha, Beta  float64
	Level, Trend float64
	Res          EWM
	N            int
}

// Forecast is the one-step-ahead prediction.
func (h *Holt) Forecast() float64 { return h.Level + h.Trend }

// Update folds x into the model.
func (h *Holt) Update(x float64) {
	switch h.N {
	case 0:
		h.Level = x
	case 1:
		h.Trend = x - h.Level
		h.Level = x
	default:
		h.Res.Update(x - h.Forecast())
		prev := h.Level
		h.Level = h.Alpha*x + (1-h.Alpha)*(h.Level+h.Trend)
		h.Trend = h.Beta*(h.Level-prev) + (1-h.Beta)*h.Trend
	}
	h.N++
}

// Z is the z-score of x against the forecast.
func (h *Holt) Z(x, minStd float64) float64 {
	return (x - h.Forecast()) / math.Max(h.Res.Std(), math.Max(minStd, 1e-12))
}

// decay is an exponentially decayed counter: each unit added loses weight
// with time constant tau, so value ≈ events in the last tau.
type decay struct {
	v  float64
	at int64
}

func (d *decay) value(now int64, tau float64) float64 {
	if d.at == 0 || now <= d.at {
		return d.v
	}
	return d.v * math.Exp(-float64(now-d.at)/tau)
}

func (d *decay) add(now int64, w, tau float64) float64 {
	d.v = d.value(now, tau) + w
	if now > d.at {
		d.at = now
	}
	return d.v
}

// sampleCap bounds sampleRing; it must be a power of two.
const sampleCap = 64

// sampleRing is a fixed-size window of (time, value) samples with an
// ordinary least squares fit.
type sampleRing struct {
	t    [sampleCap]int64
	v    [sampleCap]float64
	n    int
	head int // next slot
	size int // window, ≤ sampleCap
}

func (r *sampleRing) push(t int64, v float64) {
	r.t[r.head] = t
	r.v[r.head] = v
	r.head = (r.head + 1) % r.size
	if r.n < r.size {
		r.n++
	}
}

// fit regresses value on time (seconds) over the window. It returns the
// slope per second, the coefficient of determination, the span covered and
// false when there are fewer than two distinct times.
func (r *sampleRing) fit() (slope, r2 float64, span int64, ok bool) {
	if r.n < 2 {
		return 0, 0, 0, false
	}
	first := (r.head - r.n + r.size) % r.size
	t0 := r.t[first]
	var sx, sy, sxx, sxy, syy float64
	last := t0
	for i := 0; i < r.n; i++ {
		j := (first + i) % r.size
		x := float64(r.t[j]-t0) / 1e9
		y := r.v[j]
		sx += x
		sy += y
		sxx += x * x
		sxy += x * y
		syy += y * y
		last = r.t[j]
	}
	n := float64(r.n)
	dx := n*sxx - sx*sx
	if dx <= 1e-12 {
		return 0, 0, last - t0, false
	}
	slope = (n*sxy - sx*sy) / dx
	dy := n*syy - sy*sy
	if dy > 1e-12 {
		r2 = (n*sxy - sx*sy) * (n*sxy - sx*sy) / (dx * dy)
	} else {
		r2 = 1
	}
	return slope, r2, last - t0, true
}

// shannon returns the Shannon entropy of b in bits per byte.
func shannon(b []byte) float64 {
	if len(b) == 0 {
		return 0
	}
	var hist [256]uint32
	for _, c := range b {
		hist[c]++
	}
	n := float64(len(b))
	h := 0.0
	for _, c := range hist {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}
