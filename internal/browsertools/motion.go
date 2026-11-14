package browsertools

import (
	"math"
	"math/rand/v2"
	"time"
)

// Rect is an axis-aligned box in viewport CSS pixels.
type Rect struct{ X, Y, W, H float64 }

// rectOf converts a script [x, y, w, h] array.
func rectOf(a []float64) Rect {
	if len(a) < 4 {
		return Rect{}
	}
	return Rect{a[0], a[1], a[2], a[3]}
}

// Center returns the middle of r.
func (r Rect) Center() Point { return Point{r.X + r.W/2, r.Y + r.H/2} }

// Area returns w*h, or zero for an empty rect.
func (r Rect) Area() float64 {
	if r.W <= 0 || r.H <= 0 {
		return 0
	}
	return r.W * r.H
}

// Contains reports whether p lies inside r (edges included).
func (r Rect) Contains(p Point) bool {
	return p.X >= r.X && p.X <= r.X+r.W && p.Y >= r.Y && p.Y <= r.Y+r.H
}

// PathPoint is one pointer position along a motion, At after it starts.
type PathPoint struct {
	Point
	At time.Duration
}

// Motion tuning. Fitts' law: T = a + b·log2(D/W + 1).
const (
	fittsA       = 90 * time.Millisecond
	fittsB       = 120 * time.Millisecond
	minMoveTime  = 80 * time.Millisecond
	maxMoveTime  = 1200 * time.Millisecond
	frameTime    = 16 * time.Millisecond
	maxPathSteps = 90
	maxCurve     = 120.0
)

// FittsDuration returns how long a person takes to point at a target of
// the given width at the given distance, by Fitts' law, clamped to a
// sensible range.
func FittsDuration(dist, width float64) time.Duration {
	if dist <= 0 {
		return 0
	}
	width = math.Max(width, 4)
	d := fittsA + time.Duration(float64(fittsB)*math.Log2(dist/width+1))
	return min(max(d, minMoveTime), maxMoveTime)
}

// minimumJerk is the minimum-jerk position profile 10τ³ − 15τ⁴ + 6τ⁵:
// a smooth start and stop with a bell-shaped velocity in between, the
// shape of natural point-to-point arm movement. It is monotone on [0, 1].
func minimumJerk(t float64) float64 {
	t = math.Min(math.Max(t, 0), 1)
	return t * t * t * (10 - 15*t + 6*t*t)
}

// PlanPath plans a pointer motion from one point to another along a cubic
// Bézier curve with slight, random curvature. Its control points sit at
// one and two thirds of the way, displaced to one side, so progress along
// the straight line is monotone; timing follows a minimum-jerk profile over
// a Fitts' law duration for a target of targetWidth. The last point is
// exactly to. The same rng state yields the same path.
func PlanPath(rng *rand.Rand, from, to Point, targetWidth float64) []PathPoint {
	dx, dy := to.X-from.X, to.Y-from.Y
	dist := math.Hypot(dx, dy)
	if dist < 2 {
		return []PathPoint{{Point: to, At: 0}}
	}
	dur := FittsDuration(dist, targetWidth)
	steps := int(dur / frameTime)
	steps = min(max(steps, 3), maxPathSteps)

	ux, uy := dx/dist, dy/dist // along the line
	px, py := -uy, ux          // perpendicular
	curve := math.Min(dist*(0.04+0.14*rng.Float64()), maxCurve)
	if rng.IntN(2) == 0 {
		curve = -curve
	}
	c1 := curve * (0.7 + 0.5*rng.Float64())
	c2 := curve * (0.7 + 0.5*rng.Float64())
	p1 := Point{from.X + ux*dist/3 + px*c1, from.Y + uy*dist/3 + py*c1}
	p2 := Point{from.X + ux*2*dist/3 + px*c2, from.Y + uy*2*dist/3 + py*c2}

	out := make([]PathPoint, 0, steps)
	for i := 1; i <= steps; i++ {
		tau := float64(i) / float64(steps)
		t := minimumJerk(tau)
		pt := bezier(from, p1, p2, to, t)
		if i < steps {
			// Slight sideways tremor; it never moves the point along the
			// line, so progress stays monotone.
			tr := (rng.Float64() - 0.5) * 0.8
			pt.X += px * tr
			pt.Y += py * tr
		} else {
			pt = to
		}
		out = append(out, PathPoint{Point: pt, At: time.Duration(float64(dur) * tau)})
	}
	return out
}

func bezier(p0, p1, p2, p3 Point, t float64) Point {
	u := 1 - t
	a, b, c, d := u*u*u, 3*u*u*t, 3*u*t*t, t*t*t
	return Point{
		a*p0.X + b*p1.X + c*p2.X + d*p3.X,
		a*p0.Y + b*p1.Y + c*p2.Y + d*p3.Y,
	}
}

// PickPoint picks where in a visible box to put the pointer. With jitter
// it scatters around the center, normally distributed, and always stays
// within the inner 60% of the box so it never lands on an edge or outside.
// Without jitter it returns the exact center.
func PickPoint(rng *rand.Rand, box Rect, jitter bool) Point {
	c := box.Center()
	if !jitter || box.W < 4 || box.H < 4 {
		return c
	}
	x := c.X + rng.NormFloat64()*box.W/8
	y := c.Y + rng.NormFloat64()*box.H/8
	padX, padY := box.W*0.2, box.H*0.2
	x = math.Min(math.Max(x, box.X+padX), box.X+box.W-padX)
	y = math.Min(math.Max(y, box.Y+padY), box.Y+box.H-padY)
	return Point{x, y}
}

// WheelSteps splits a scroll distance into wheel notches. Natural scrolling
// uses notches of roughly 100px that shrink near the end, the way a mouse
// wheel or trackpad flick does; otherwise one step covers it all.
func WheelSteps(rng *rand.Rand, total float64, natural bool) []float64 {
	if total == 0 {
		return nil
	}
	if !natural {
		return []float64{total}
	}
	sign := 1.0
	if total < 0 {
		sign, total = -1, -total
	}
	var out []float64
	left := total
	for left > 0.5 && len(out) < 400 {
		step := 80 + 40*rng.Float64()
		if left < 240 {
			step = math.Max(left/2, 40)
		}
		step = math.Min(step, left)
		out = append(out, sign*math.Round(step))
		left -= math.Round(step)
	}
	if left > 0.5 || left < -0.5 {
		out = append(out, sign*left)
	}
	return out
}
