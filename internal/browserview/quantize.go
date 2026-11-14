package browserview

import (
	"image"
	"image/color"
	"slices"
)

// Dither selects the error-diffusion strategy used when mapping pixels to a
// reduced palette.
type Dither int

const (
	// DitherOrdered applies an 8x8 Bayer threshold matrix. It is stable
	// between frames, which avoids "crawling" noise on live video, and is
	// therefore the default.
	DitherOrdered Dither = iota
	// DitherFloydSteinberg diffuses quantisation error (serpentine scan).
	DitherFloydSteinberg
	// DitherNone maps every pixel to its nearest palette entry.
	DitherNone
)

// Quantizer reduces an RGBA image to at most MaxColors (<=256) colours with
// median-cut palette selection. Buffers are reused across calls; a
// Quantizer is not safe for concurrent use.
type Quantizer struct {
	MaxColors int
	Dither    Dither

	hist    []uint32 // 5-bit-per-channel histogram
	cache   []int16  // 5-bit colour -> palette index (+1; 0 = unknown)
	pix     []uint8
	errs    []int32
	colors  []histColor
	exact   map[uint32]uint8
	palette []color.RGBA
}

type histColor struct {
	r, g, b uint8 // 5-bit components
	n       uint32
}

type box struct {
	lo, hi int // range within Quantizer.colors
	pop    uint64
}

const histBits = 5
const histSize = 1 << (3 * histBits)

func histIndex(r, g, b uint8) int {
	return int(r>>(8-histBits))<<(2*histBits) | int(g>>(8-histBits))<<histBits | int(b>>(8-histBits))
}

// Palette returns the palette produced by the last Quantize call.
func (q *Quantizer) Palette() []color.RGBA { return q.palette }

// Quantize maps img to a paletted image. The returned image's Pix buffer is
// owned by the Quantizer and is only valid until the next call.
func (q *Quantizer) Quantize(img *image.RGBA) *image.Paletted {
	maxColors := q.MaxColors
	if maxColors <= 0 || maxColors > 256 {
		maxColors = 256
	}
	w, h := img.Rect.Dx(), img.Rect.Dy()
	if cap(q.pix) < w*h {
		q.pix = make([]uint8, w*h)
	}
	pix := q.pix[:w*h]
	if q.tryExact(img, maxColors, pix) {
		return q.paletted(pix, w, h)
	}
	q.buildPalette(img, maxColors)
	switch q.Dither {
	case DitherNone:
		q.mapNearest(img, pix)
	case DitherFloydSteinberg:
		q.mapFloydSteinberg(img, pix)
	default:
		q.mapOrdered(img, pix)
	}
	return q.paletted(pix, w, h)
}

func (q *Quantizer) paletted(pix []uint8, w, h int) *image.Paletted {
	pal := make(color.Palette, len(q.palette))
	for i, c := range q.palette {
		pal[i] = c
	}
	return &image.Paletted{Pix: pix, Stride: w, Rect: image.Rect(0, 0, w, h), Palette: pal}
}

// tryExact succeeds when the image has no more than maxColors distinct
// colours, in which case it is represented losslessly.
func (q *Quantizer) tryExact(img *image.RGBA, maxColors int, pix []uint8) bool {
	if q.exact == nil {
		q.exact = make(map[uint32]uint8, 256)
	} else {
		clear(q.exact)
	}
	q.palette = q.palette[:0]
	w, h := img.Rect.Dx(), img.Rect.Dy()
	var lastKey uint32 = 1 << 31
	var lastIdx uint8
	for y := 0; y < h; y++ {
		row := img.Pix[y*img.Stride : y*img.Stride+w*4]
		for x := 0; x < w; x++ {
			key := uint32(row[x*4])<<16 | uint32(row[x*4+1])<<8 | uint32(row[x*4+2])
			if key == lastKey {
				pix[y*w+x] = lastIdx
				continue
			}
			idx, ok := q.exact[key]
			if !ok {
				if len(q.palette) >= maxColors {
					return false
				}
				idx = uint8(len(q.palette))
				q.exact[key] = idx
				q.palette = append(q.palette, color.RGBA{row[x*4], row[x*4+1], row[x*4+2], 0xFF})
			}
			pix[y*w+x] = idx
			lastKey, lastIdx = key, idx
		}
	}
	return true
}

func (q *Quantizer) buildPalette(img *image.RGBA, maxColors int) {
	if q.hist == nil {
		q.hist = make([]uint32, histSize)
		q.cache = make([]int16, histSize)
	} else {
		clear(q.hist)
		clear(q.cache)
	}
	w, h := img.Rect.Dx(), img.Rect.Dy()
	for y := 0; y < h; y++ {
		row := img.Pix[y*img.Stride : y*img.Stride+w*4]
		for x := 0; x < len(row); x += 4 {
			q.hist[histIndex(row[x], row[x+1], row[x+2])]++
		}
	}
	q.colors = q.colors[:0]
	for i, n := range q.hist {
		if n > 0 {
			q.colors = append(q.colors, histColor{
				r: uint8(i >> (2 * histBits)), g: uint8(i >> histBits & 31), b: uint8(i & 31), n: n,
			})
		}
	}
	boxes := []box{{lo: 0, hi: len(q.colors), pop: uint64(w * h)}}
	for len(boxes) < maxColors {
		// Split the box with the largest population-weighted extent.
		best, bestScore, bestAxis := -1, uint64(0), 0
		for i, b := range boxes {
			if b.hi-b.lo < 2 {
				continue
			}
			axis, extent := q.longestAxis(b)
			score := uint64(extent) * b.pop
			if extent > 0 && score > bestScore {
				best, bestScore, bestAxis = i, score, axis
			}
		}
		if best < 0 {
			break
		}
		a, b := q.split(boxes[best], bestAxis)
		boxes[best] = a
		boxes = append(boxes, b)
	}
	q.palette = q.palette[:0]
	for _, b := range boxes {
		var rs, gs, bs, n uint64
		for _, c := range q.colors[b.lo:b.hi] {
			rs += uint64(c.r) * uint64(c.n)
			gs += uint64(c.g) * uint64(c.n)
			bs += uint64(c.b) * uint64(c.n)
			n += uint64(c.n)
		}
		if n == 0 {
			continue
		}
		// Expand 5-bit averages back to 8 bits (x * 255/31).
		q.palette = append(q.palette, color.RGBA{
			uint8((rs*255 + n*31/2) / (n * 31)),
			uint8((gs*255 + n*31/2) / (n * 31)),
			uint8((bs*255 + n*31/2) / (n * 31)),
			0xFF,
		})
	}
}

func (q *Quantizer) longestAxis(b box) (axis int, extent int) {
	minC := [3]uint8{31, 31, 31}
	maxC := [3]uint8{}
	for _, c := range q.colors[b.lo:b.hi] {
		v := [3]uint8{c.r, c.g, c.b}
		for k := 0; k < 3; k++ {
			minC[k] = min(minC[k], v[k])
			maxC[k] = max(maxC[k], v[k])
		}
	}
	for k := 0; k < 3; k++ {
		if e := int(maxC[k]) - int(minC[k]); e > extent {
			axis, extent = k, e
		}
	}
	return axis, extent
}

func (q *Quantizer) split(b box, axis int) (box, box) {
	cs := q.colors[b.lo:b.hi]
	key := func(c histColor) uint8 {
		switch axis {
		case 0:
			return c.r
		case 1:
			return c.g
		}
		return c.b
	}
	slices.SortFunc(cs, func(x, y histColor) int { return int(key(x)) - int(key(y)) })
	half := b.pop / 2
	var acc uint64
	cut := 1
	for i, c := range cs {
		acc += uint64(c.n)
		if acc >= half {
			cut = i + 1
			break
		}
	}
	if cut >= len(cs) {
		cut = len(cs) - 1
	}
	var popA uint64
	for _, c := range cs[:cut] {
		popA += uint64(c.n)
	}
	return box{lo: b.lo, hi: b.lo + cut, pop: popA}, box{lo: b.lo + cut, hi: b.hi, pop: b.pop - popA}
}

// nearest returns the palette index closest to (r,g,b), memoised at 5-bit
// precision.
func (q *Quantizer) nearest(r, g, b uint8) uint8 {
	hi := histIndex(r, g, b)
	if v := q.cache[hi]; v != 0 {
		return uint8(v - 1)
	}
	// Search with the bucket centre so the memoised answer is unbiased.
	cr := int32(r&^7) + 4
	cg := int32(g&^7) + 4
	cb := int32(b&^7) + 4
	best, bestD := 0, int32(1<<30)
	for i, c := range q.palette {
		dr, dg, db := cr-int32(c.R), cg-int32(c.G), cb-int32(c.B)
		// Weighted Euclidean distance approximating perceived luminance.
		d := 2*dr*dr + 4*dg*dg + 3*db*db
		if d < bestD {
			best, bestD = i, d
		}
	}
	q.cache[hi] = int16(best + 1)
	return uint8(best)
}

func (q *Quantizer) mapNearest(img *image.RGBA, pix []uint8) {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	for y := 0; y < h; y++ {
		row := img.Pix[y*img.Stride:]
		for x := 0; x < w; x++ {
			pix[y*w+x] = q.nearest(row[x*4], row[x*4+1], row[x*4+2])
		}
	}
}

// bayer8 is the classic 8x8 ordered-dither threshold matrix (0..63).
var bayer8 = [64]int32{
	0, 32, 8, 40, 2, 34, 10, 42,
	48, 16, 56, 24, 50, 18, 58, 26,
	12, 44, 4, 36, 14, 46, 6, 38,
	60, 28, 52, 20, 62, 30, 54, 22,
	3, 35, 11, 43, 1, 33, 9, 41,
	51, 19, 59, 27, 49, 17, 57, 25,
	15, 47, 7, 39, 13, 45, 5, 37,
	63, 31, 55, 23, 61, 29, 53, 21,
}

func (q *Quantizer) mapOrdered(img *image.RGBA, pix []uint8) {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	// Spread roughly matches the average distance between palette entries.
	const spread = 24
	for y := 0; y < h; y++ {
		row := img.Pix[y*img.Stride:]
		for x := 0; x < w; x++ {
			t := (bayer8[(y&7)*8+(x&7)]*spread)/64 - spread/2
			pix[y*w+x] = q.nearest(
				clamp8(int32(row[x*4])+t),
				clamp8(int32(row[x*4+1])+t),
				clamp8(int32(row[x*4+2])+t),
			)
		}
	}
}

func (q *Quantizer) mapFloydSteinberg(img *image.RGBA, pix []uint8) {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	// Two error rows (current, next), 3 channels, with one pixel of padding
	// on each side to avoid bounds checks at the edges.
	stride := (w + 2) * 3
	if cap(q.errs) < 2*stride {
		q.errs = make([]int32, 2*stride)
	}
	errs := q.errs[:2*stride]
	clear(errs)
	cur, next := errs[:stride], errs[stride:]
	for y := 0; y < h; y++ {
		row := img.Pix[y*img.Stride:]
		ltr := y&1 == 0
		x, end, dir := 0, w, 1
		if !ltr {
			x, end, dir = w-1, -1, -1
		}
		for ; x != end; x += dir {
			e := (x + 1) * 3
			// Errors are stored scaled by 16.
			r := clamp8(int32(row[x*4]) + cur[e]/16)
			g := clamp8(int32(row[x*4+1]) + cur[e+1]/16)
			b := clamp8(int32(row[x*4+2]) + cur[e+2]/16)
			idx := q.nearest(r, g, b)
			pix[y*w+x] = idx
			c := q.palette[idx]
			er := int32(r) - int32(c.R)
			eg := int32(g) - int32(c.G)
			eb := int32(b) - int32(c.B)
			fwd := e + dir*3
			bwd := e - dir*3
			cur[fwd] += er * 7
			cur[fwd+1] += eg * 7
			cur[fwd+2] += eb * 7
			next[bwd] += er * 3
			next[bwd+1] += eg * 3
			next[bwd+2] += eb * 3
			next[e] += er * 5
			next[e+1] += eg * 5
			next[e+2] += eb * 5
			next[fwd] += er
			next[fwd+1] += eg
			next[fwd+2] += eb
		}
		cur, next = next, cur
		clear(next)
	}
}
