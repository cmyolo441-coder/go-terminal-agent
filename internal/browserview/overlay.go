package browserview

import (
	"image"
	"image/color"
	"math"
	"time"
)

var (
	highlightColor = color.RGBA{0xF5, 0xA6, 0x23, 0xFF}
	cursorFill     = color.RGBA{0xFF, 0xFF, 0xFF, 0xFF}
	cursorEdge     = color.RGBA{0x10, 0x10, 0x10, 0xFF}
)

// arrowShape is the classic pointer outline on a 12x21 grid; the tip is at
// the origin.
var arrowShape = [][2]float64{{0, 0}, {0, 17}, {4, 13}, {7, 20}, {9, 19}, {6, 12}, {11.5, 12}}

// drawOverlay paints ov onto img, which shows a frame scaled by (sx, sy).
// cursor overrides ov.Cursor (used for animation); both are frame-space.
func drawOverlay(img *image.RGBA, ov Overlay, cursor *image.Point, sx, sy float64) {
	if ov.Highlight != nil {
		r := ov.Highlight
		x0 := int(math.Floor(float64(r.Min.X) * sx))
		y0 := int(math.Floor(float64(r.Min.Y) * sy))
		x1 := int(math.Ceil(float64(r.Max.X)*sx)) - 1
		y1 := int(math.Ceil(float64(r.Max.Y)*sy)) - 1
		t := max(1, min(img.Rect.Dx(), img.Rect.Dy())/120)
		fillBlend(img, image.Rect(x0, y0, x1+1, y1+1), highlightColor, 40)
		strokeRect(img, x0, y0, x1, y1, t, highlightColor)
	}
	if cursor == nil {
		cursor = ov.Cursor
	}
	if cursor != nil {
		size := math.Max(5, float64(img.Rect.Dy())/36)
		drawArrow(img, float64(cursor.X)*sx, float64(cursor.Y)*sy, size)
	}
}

func fillBlend(img *image.RGBA, r image.Rectangle, c color.RGBA, alpha int) {
	r = r.Intersect(img.Rect)
	for y := r.Min.Y; y < r.Max.Y; y++ {
		p := img.PixOffset(r.Min.X, y)
		for x := r.Min.X; x < r.Max.X; x++ {
			img.Pix[p] = uint8((int(img.Pix[p])*(255-alpha) + int(c.R)*alpha) / 255)
			img.Pix[p+1] = uint8((int(img.Pix[p+1])*(255-alpha) + int(c.G)*alpha) / 255)
			img.Pix[p+2] = uint8((int(img.Pix[p+2])*(255-alpha) + int(c.B)*alpha) / 255)
			p += 4
		}
	}
}

func strokeRect(img *image.RGBA, x0, y0, x1, y1, t int, c color.RGBA) {
	for i := 0; i < t; i++ {
		for x := x0; x <= x1; x++ {
			setPix(img, x, y0+i, c)
			setPix(img, x, y1-i, c)
		}
		for y := y0; y <= y1; y++ {
			setPix(img, x0+i, y, c)
			setPix(img, x1-i, y, c)
		}
	}
}

func setPix(img *image.RGBA, x, y int, c color.RGBA) {
	if !(image.Point{x, y}.In(img.Rect)) {
		return
	}
	p := img.PixOffset(x, y)
	img.Pix[p], img.Pix[p+1], img.Pix[p+2], img.Pix[p+3] = c.R, c.G, c.B, 0xFF
}

// drawArrow rasterises the pointer with its tip at (tx, ty) and the given
// height in pixels: a white body with a dark one-pixel outline.
func drawArrow(img *image.RGBA, tx, ty, height float64) {
	s := height / 21
	poly := make([][2]float64, len(arrowShape))
	for i, p := range arrowShape {
		poly[i] = [2]float64{tx + p[0]*s, ty + p[1]*s}
	}
	bx0, by0 := int(math.Floor(tx))-1, int(math.Floor(ty))-1
	bx1, by1 := int(math.Ceil(tx+12*s))+1, int(math.Ceil(ty+21*s))+1
	inside := func(x, y int) bool {
		return pointInPolygon(float64(x)+0.5, float64(y)+0.5, poly)
	}
	for y := by0; y <= by1; y++ {
		for x := bx0; x <= bx1; x++ {
			if !inside(x, y) {
				continue
			}
			edge := !inside(x-1, y) || !inside(x+1, y) || !inside(x, y-1) || !inside(x, y+1)
			if edge && height >= 8 {
				setPix(img, x, y, cursorEdge)
			} else {
				setPix(img, x, y, cursorFill)
			}
		}
	}
	// Make the hotspot visible even on tiny renders.
	setPix(img, int(tx), int(ty), cursorFill)
}

func pointInPolygon(x, y float64, poly [][2]float64) bool {
	in := false
	for i, j := 0, len(poly)-1; i < len(poly); j, i = i, i+1 {
		xi, yi := poly[i][0], poly[i][1]
		xj, yj := poly[j][0], poly[j][1]
		if (yi > y) != (yj > y) && x < (xj-xi)*(y-yi)/(yj-yi)+xi {
			in = !in
		}
	}
	return in
}

// cursorAnimator eases the displayed cursor towards its target so pointer
// movement reads as motion rather than teleportation.
type cursorAnimator struct {
	from, to image.Point
	start    time.Time
	dur      time.Duration
	has      bool
}

const cursorAnimDuration = 180 * time.Millisecond

// target sets a new destination, starting from wherever the cursor is
// displayed at now.
func (a *cursorAnimator) target(p *image.Point, now time.Time) {
	if p == nil {
		a.has = false
		return
	}
	if !a.has {
		a.from, a.to, a.start, a.dur, a.has = *p, *p, now, 0, true
		return
	}
	if *p == a.to {
		return
	}
	a.from = a.at(now)
	a.to = *p
	a.start = now
	a.dur = cursorAnimDuration
}

// at returns the displayed position at time now.
func (a *cursorAnimator) at(now time.Time) image.Point {
	if a.dur <= 0 {
		return a.to
	}
	t := float64(now.Sub(a.start)) / float64(a.dur)
	if t >= 1 {
		return a.to
	}
	if t < 0 {
		t = 0
	}
	e := 1 - math.Pow(1-t, 3) // ease-out cubic
	return image.Point{
		X: a.from.X + int(math.Round(float64(a.to.X-a.from.X)*e)),
		Y: a.from.Y + int(math.Round(float64(a.to.Y-a.from.Y)*e)),
	}
}

// animating reports whether the cursor is still moving at now.
func (a *cursorAnimator) animating(now time.Time) bool {
	return a.has && a.dur > 0 && now.Sub(a.start) < a.dur
}
