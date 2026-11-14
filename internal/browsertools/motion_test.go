package browsertools

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"
)

func seeded(seed uint64) *rand.Rand { return rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)) }

func TestPlanPathMonotoneAndEndsOnTarget(t *testing.T) {
	cases := []struct{ from, to Point }{
		{Point{0, 0}, Point{800, 450}},
		{Point{900, 500}, Point{12, 30}},
		{Point{500, 100}, Point{500, 700}},
		{Point{10, 300}, Point{990, 300}},
		{Point{300, 300}, Point{310, 302}},
	}
	for seed := uint64(1); seed <= 40; seed++ {
		rng := seeded(seed)
		for _, c := range cases {
			path := PlanPath(rng, c.from, c.to, 40)
			if len(path) == 0 {
				t.Fatal("empty path")
			}
			last := path[len(path)-1]
			if last.Point != c.to {
				t.Fatalf("path ends at %v, want %v", last.Point, c.to)
			}
			dx, dy := c.to.X-c.from.X, c.to.Y-c.from.Y
			dist := math.Hypot(dx, dy)
			ux, uy := dx/dist, dy/dist
			prevProj, prevAt := 0.0, time.Duration(-1)
			maxDev := 0.0
			for i, p := range path {
				proj := (p.X-c.from.X)*ux + (p.Y-c.from.Y)*uy
				if proj < prevProj-1e-6 {
					t.Fatalf("seed %d %v→%v: progress went back at step %d (%.3f < %.3f)", seed, c.from, c.to, i, proj, prevProj)
				}
				if p.At <= prevAt {
					t.Fatalf("timestamps not increasing at step %d", i)
				}
				prevProj, prevAt = proj, p.At
				dev := math.Abs((p.X-c.from.X)*-uy + (p.Y-c.from.Y)*ux)
				maxDev = math.Max(maxDev, dev)
			}
			if maxDev > 0.25*dist+1 {
				t.Fatalf("path strays %.1fpx from a %.1fpx line", maxDev, dist)
			}
			if want := FittsDuration(dist, 40); last.At != want {
				t.Fatalf("duration %v, want Fitts %v", last.At, want)
			}
		}
	}
}

func TestPlanPathCurves(t *testing.T) {
	curved := 0
	for seed := uint64(1); seed <= 20; seed++ {
		path := PlanPath(seeded(seed), Point{0, 0}, Point{600, 0}, 30)
		for _, p := range path {
			if math.Abs(p.Y) > 5 {
				curved++
				break
			}
		}
	}
	if curved < 18 {
		t.Fatalf("only %d of 20 paths visibly curved", curved)
	}
}

func TestPlanPathDeterministic(t *testing.T) {
	a := PlanPath(seeded(7), Point{1, 2}, Point{640, 380}, 25)
	b := PlanPath(seeded(7), Point{1, 2}, Point{640, 380}, 25)
	c := PlanPath(seeded(8), Point{1, 2}, Point{640, 380}, 25)
	if len(a) != len(b) {
		t.Fatal("same seed gave different lengths")
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("same seed differs at %d: %v vs %v", i, a[i], b[i])
		}
	}
	same := len(a) == len(c)
	for i := 0; same && i < len(a); i++ {
		same = a[i] == c[i]
	}
	if same {
		t.Fatal("different seeds gave identical paths")
	}
}

func TestPlanPathShortHop(t *testing.T) {
	p := PlanPath(seeded(1), Point{10, 10}, Point{11, 10.5}, 10)
	if len(p) != 1 || p[0].Point != (Point{11, 10.5}) || p[0].At != 0 {
		t.Fatalf("short hop = %v", p)
	}
}

func TestPlanPathVelocityBellShaped(t *testing.T) {
	path := PlanPath(seeded(3), Point{0, 0}, Point{900, 0}, 20)
	n := len(path)
	if n < 10 {
		t.Fatalf("too few steps: %d", n)
	}
	step := func(i int) float64 {
		prev := Point{0, 0}
		if i > 0 {
			prev = path[i-1].Point
		}
		return path[i].X - prev.X
	}
	mid := step(n / 2)
	if !(step(0) < mid && step(n-1) < mid) {
		t.Fatalf("velocity not eased: start %.2f mid %.2f end %.2f", step(0), mid, step(n-1))
	}
}

func TestFittsDuration(t *testing.T) {
	if FittsDuration(0, 10) != 0 {
		t.Fatal("zero distance should take no time")
	}
	near, far := FittsDuration(100, 40), FittsDuration(800, 40)
	if !(near < far) {
		t.Fatalf("farther target should take longer: %v vs %v", near, far)
	}
	small, big := FittsDuration(500, 8), FittsDuration(500, 200)
	if !(big < small) {
		t.Fatalf("bigger target should be faster: %v vs %v", big, small)
	}
	if d := FittsDuration(1e9, 1); d != maxMoveTime {
		t.Fatalf("not clamped: %v", d)
	}
	if d := FittsDuration(1, 1000); d < minMoveTime || d > fittsA+time.Millisecond {
		t.Fatalf("easy target should take about the base time: %v", d)
	}
}

func TestPickPointStaysInside(t *testing.T) {
	rng := seeded(11)
	boxes := []Rect{{10, 10, 100, 30}, {0, 0, 5, 5}, {300, 200, 2, 400}, {50, 60, 1000, 1000}}
	for _, b := range boxes {
		inner := Rect{b.X + b.W*0.2 - 1e-9, b.Y + b.H*0.2 - 1e-9, b.W*0.6 + 2e-9, b.H*0.6 + 2e-9}
		spreadX := map[int]bool{}
		for i := 0; i < 2000; i++ {
			p := PickPoint(rng, b, true)
			if !b.Contains(p) {
				t.Fatalf("point %v outside %v", p, b)
			}
			if b.W >= 4 && b.H >= 4 && !inner.Contains(p) {
				t.Fatalf("point %v outside inner box of %v", p, b)
			}
			spreadX[int(p.X)] = true
		}
		if b.W >= 50 && len(spreadX) < 10 {
			t.Fatalf("no jitter in %v", b)
		}
	}
	if p := PickPoint(rng, Rect{10, 10, 100, 40}, false); p != (Point{60, 30}) {
		t.Fatalf("no-jitter point = %v", p)
	}
}

func TestWheelSteps(t *testing.T) {
	for _, total := range []float64{1, 39, 100, 240, 1234, -987, 5000} {
		steps := WheelSteps(seeded(5), total, true)
		sum := 0.0
		for _, s := range steps {
			if math.Signbit(s) != math.Signbit(total) {
				t.Fatalf("step %v has the wrong sign for %v", s, total)
			}
			if math.Abs(s) > 120 {
				t.Fatalf("notch %v too big", s)
			}
			sum += s
		}
		if math.Abs(sum-total) > 1 {
			t.Fatalf("steps for %v sum to %v", total, sum)
		}
	}
	if s := WheelSteps(seeded(1), 777, false); len(s) != 1 || s[0] != 777 {
		t.Fatalf("instant steps = %v", s)
	}
	if s := WheelSteps(seeded(1), 0, true); s != nil {
		t.Fatalf("zero steps = %v", s)
	}
}
