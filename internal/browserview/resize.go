package browserview

import (
	"image"
	"image/draw"
	"math"
	"sync"
)

// CellSize is the size in pixels of one terminal character cell.
type CellSize struct{ W, H int }

// DefaultCellSize is used when the terminal does not report pixel sizes.
var DefaultCellSize = CellSize{W: 8, H: 16}

func (c CellSize) valid() bool { return c.W > 0 && c.H > 0 && c.W < 1000 && c.H < 1000 }

func (c CellSize) orDefault() CellSize {
	if c.valid() {
		return c
	}
	return DefaultCellSize
}

// Layout describes how an image is fitted into a cell grid.
type Layout struct {
	// Cols and Rows are the cells covered by the image.
	Cols, Rows int
	// OffsetCol and OffsetRow centre the image within the available area.
	OffsetCol, OffsetRow int
	// PixW and PixH are the dimensions the image must be resized to. For
	// the half-block renderer PixW == Cols and PixH == 2*Rows.
	PixW, PixH int
}

// FitCells fits an imgW x imgH image into a cols x rows cell area while
// preserving the physical aspect ratio given the cell pixel size. With
// halfBlock each cell carries 1x2 pixels; otherwise each cell carries
// cell.W x cell.H pixels.
func FitCells(imgW, imgH, cols, rows int, cell CellSize, halfBlock bool) Layout {
	if imgW <= 0 || imgH <= 0 || cols <= 0 || rows <= 0 {
		return Layout{}
	}
	cell = cell.orDefault()
	availW := float64(cols * cell.W)
	availH := float64(rows * cell.H)
	s := math.Min(availW/float64(imgW), availH/float64(imgH))
	physW := float64(imgW) * s
	physH := float64(imgH) * s
	var l Layout
	if halfBlock {
		l.Cols = clampInt(int(math.Round(physW/float64(cell.W))), 1, cols)
		l.Rows = clampInt(int(math.Round(physH/float64(cell.H))), 1, rows)
		l.PixW, l.PixH = l.Cols, l.Rows*2
	} else {
		l.PixW = clampInt(int(math.Round(physW)), 1, cols*cell.W)
		l.PixH = clampInt(int(math.Round(physH)), 1, rows*cell.H)
		l.Cols = clampInt((l.PixW+cell.W-1)/cell.W, 1, cols)
		l.Rows = clampInt((l.PixH+cell.H-1)/cell.H, 1, rows)
	}
	l.OffsetCol = (cols - l.Cols) / 2
	l.OffsetRow = (rows - l.Rows) / 2
	return l
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

const weightBits = 14
const weightOne = 1 << weightBits

// contribs holds per-destination-pixel filter taps for one axis.
type contribs struct {
	srcN, dstN int
	start      []int32 // first source index per destination pixel
	idx        []int32 // offsets into w; len dstN+1
	w          []int32 // fixed-point weights summing to weightOne
}

func makeContribs(srcN, dstN int) *contribs {
	c := &contribs{srcN: srcN, dstN: dstN, start: make([]int32, dstN), idx: make([]int32, dstN+1)}
	scale := float64(srcN) / float64(dstN)
	fw := make([]float64, 0, int(math.Ceil(scale))+2)
	for i := 0; i < dstN; i++ {
		fw = fw[:0]
		var first int
		if scale >= 1 {
			// Area averaging: weight = overlap of the source pixel with the
			// destination pixel's footprint.
			a := float64(i) * scale
			b := a + scale
			first = int(math.Floor(a))
			last := int(math.Ceil(b)) - 1
			if last >= srcN {
				last = srcN - 1
			}
			for j := first; j <= last; j++ {
				ov := math.Min(b, float64(j+1)) - math.Max(a, float64(j))
				if ov < 0 {
					ov = 0
				}
				fw = append(fw, ov/scale)
			}
		} else {
			// Bilinear interpolation between the two nearest samples.
			center := (float64(i)+0.5)*scale - 0.5
			j0 := int(math.Floor(center))
			f := center - float64(j0)
			if j0 < 0 {
				j0, f = 0, 0
			}
			if j0 >= srcN-1 {
				j0, f = srcN-1, 0
			}
			first = j0
			fw = append(fw, 1-f)
			if f > 0 {
				fw = append(fw, f)
			}
		}
		// Quantise to fixed point and make the weights sum exactly to one so
		// flat colours survive resizing unchanged.
		sum, maxK := int32(0), 0
		base := len(c.w)
		for k, v := range fw {
			q := int32(math.Round(v * weightOne))
			c.w = append(c.w, q)
			sum += q
			if q > c.w[base+maxK] {
				maxK = k
			}
		}
		c.w[base+maxK] += weightOne - sum
		c.start[i] = int32(first)
		c.idx[i+1] = int32(len(c.w))
	}
	return c
}

// Resizer scales images with area averaging (downscale) or bilinear
// interpolation (upscale). It caches filter taps and scratch buffers so
// repeated resizes of same-sized frames allocate nothing. A Resizer is not
// safe for concurrent use.
type Resizer struct {
	h, v *contribs
	tmp  []uint8
	src  *image.RGBA
}

// ToRGBA converts img into an *image.RGBA whose bounds start at (0,0),
// reusing the Resizer's buffer. If img already is such an RGBA it is
// returned unchanged.
func (r *Resizer) ToRGBA(img image.Image) *image.RGBA {
	if rgba, ok := img.(*image.RGBA); ok && rgba.Rect.Min == (image.Point{}) {
		return rgba
	}
	b := img.Bounds()
	r.src = ensureRGBA(r.src, b.Dx(), b.Dy())
	draw.Draw(r.src, r.src.Rect, img, b.Min, draw.Src)
	return r.src
}

func ensureRGBA(img *image.RGBA, w, h int) *image.RGBA {
	if img != nil && img.Rect.Dx() == w && img.Rect.Dy() == h {
		return img
	}
	if img != nil && cap(img.Pix) >= w*h*4 {
		return &image.RGBA{Pix: img.Pix[:w*h*4], Stride: w * 4, Rect: image.Rect(0, 0, w, h)}
	}
	return image.NewRGBA(image.Rect(0, 0, w, h))
}

// Resize scales src to dstW x dstH. When dst is non-nil and large enough its
// storage is reused. The returned image is fully opaque.
func (r *Resizer) Resize(dst *image.RGBA, src image.Image, dstW, dstH int) *image.RGBA {
	s := r.ToRGBA(src)
	dst = ensureRGBA(dst, dstW, dstH)
	sw, sh := s.Rect.Dx(), s.Rect.Dy()
	if sw == 0 || sh == 0 || dstW == 0 || dstH == 0 {
		return dst
	}
	if sw == dstW && sh == dstH {
		for y := 0; y < sh; y++ {
			copy(dst.Pix[y*dst.Stride:y*dst.Stride+dstW*4], s.Pix[y*s.Stride:y*s.Stride+sw*4])
		}
		return dst
	}
	if r.h == nil || r.h.srcN != sw || r.h.dstN != dstW {
		r.h = makeContribs(sw, dstW)
	}
	if r.v == nil || r.v.srcN != sh || r.v.dstN != dstH {
		r.v = makeContribs(sh, dstH)
	}
	// Horizontal pass: sh rows of dstW pixels, 3 bytes each.
	need := sh * dstW * 3
	if cap(r.tmp) < need {
		r.tmp = make([]uint8, need)
	}
	tmp := r.tmp[:need]
	hc := r.h
	for y := 0; y < sh; y++ {
		row := s.Pix[y*s.Stride : y*s.Stride+sw*4]
		out := tmp[y*dstW*3 : (y+1)*dstW*3]
		for x := 0; x < dstW; x++ {
			ws := hc.w[hc.idx[x]:hc.idx[x+1]]
			p := int(hc.start[x]) * 4
			var rr, gg, bb int32
			for _, w := range ws {
				rr += int32(row[p]) * w
				gg += int32(row[p+1]) * w
				bb += int32(row[p+2]) * w
				p += 4
			}
			o := x * 3
			out[o] = clamp8((rr + weightOne/2) >> weightBits)
			out[o+1] = clamp8((gg + weightOne/2) >> weightBits)
			out[o+2] = clamp8((bb + weightOne/2) >> weightBits)
		}
	}
	// Vertical pass with a row accumulator for cache-friendly access.
	acc := accPool.Get().(*[]int32)
	if cap(*acc) < dstW*3 {
		*acc = make([]int32, dstW*3)
	}
	a := (*acc)[:dstW*3]
	vc := r.v
	for y := 0; y < dstH; y++ {
		clear(a)
		ws := vc.w[vc.idx[y]:vc.idx[y+1]]
		sy := int(vc.start[y])
		for k, w := range ws {
			row := tmp[(sy+k)*dstW*3 : (sy+k+1)*dstW*3]
			for i, v := range row {
				a[i] += int32(v) * w
			}
		}
		out := dst.Pix[y*dst.Stride : y*dst.Stride+dstW*4]
		for x := 0; x < dstW; x++ {
			out[x*4] = clamp8((a[x*3] + weightOne/2) >> weightBits)
			out[x*4+1] = clamp8((a[x*3+1] + weightOne/2) >> weightBits)
			out[x*4+2] = clamp8((a[x*3+2] + weightOne/2) >> weightBits)
			out[x*4+3] = 0xFF
		}
	}
	accPool.Put(acc)
	return dst
}

var accPool = sync.Pool{New: func() any { s := make([]int32, 0, 4096); return &s }}

func clamp8(v int32) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}
