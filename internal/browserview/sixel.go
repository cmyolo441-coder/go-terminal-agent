package browserview

import (
	"image"
	"strconv"
)

// SixelEncoder converts RGBA images to DEC Sixel graphics using its own
// median-cut quantizer, optional dithering and run-length encoding.
// Buffers are reused between calls; it is not safe for concurrent use.
type SixelEncoder struct {
	Quantizer Quantizer

	masks []uint8
	used  []bool
	order []uint8
	minX  [256]int
	maxX  [256]int
}

// NewSixelEncoder returns an encoder producing up to maxColors registers.
func NewSixelEncoder(maxColors int, d Dither) *SixelEncoder {
	return &SixelEncoder{Quantizer: Quantizer{MaxColors: maxColors, Dither: d}}
}

// Encode appends a complete sixel DCS sequence for img to dst.
func (e *SixelEncoder) Encode(dst []byte, img *image.RGBA) []byte {
	pal := e.Quantizer.Quantize(img)
	colors := e.Quantizer.Palette()
	w, h := pal.Rect.Dx(), pal.Rect.Dy()

	// P2=1: pixels we do not paint keep the background; aspect 1:1.
	dst = append(dst, "\x1bP0;1;0q\"1;1;"...)
	dst = strconv.AppendInt(dst, int64(w), 10)
	dst = append(dst, ';')
	dst = strconv.AppendInt(dst, int64(h), 10)
	for i, c := range colors {
		dst = append(dst, '#')
		dst = strconv.AppendInt(dst, int64(i), 10)
		dst = append(dst, ";2;"...)
		dst = strconv.AppendInt(dst, int64((int(c.R)*100+127)/255), 10)
		dst = append(dst, ';')
		dst = strconv.AppendInt(dst, int64((int(c.G)*100+127)/255), 10)
		dst = append(dst, ';')
		dst = strconv.AppendInt(dst, int64((int(c.B)*100+127)/255), 10)
	}

	n := len(colors)
	if cap(e.masks) < n*w {
		e.masks = make([]uint8, n*w)
	}
	masks := e.masks[:n*w]
	clear(masks)
	if cap(e.used) < n {
		e.used = make([]bool, n)
	}
	used := e.used[:n]
	clear(used)

	for y0 := 0; y0 < h; y0 += 6 {
		e.order = e.order[:0]
		for r := 0; r < 6 && y0+r < h; r++ {
			row := pal.Pix[(y0+r)*pal.Stride : (y0+r)*pal.Stride+w]
			bit := uint8(1) << r
			for x, c := range row {
				if !used[c] {
					used[c] = true
					e.order = append(e.order, c)
					e.minX[c], e.maxX[c] = x, x
				} else {
					if x < e.minX[c] {
						e.minX[c] = x
					}
					if x > e.maxX[c] {
						e.maxX[c] = x
					}
				}
				masks[int(c)*w+x] |= bit
			}
		}
		for i, c := range e.order {
			if i > 0 {
				dst = append(dst, '$') // carriage return within the band
			}
			dst = append(dst, '#')
			dst = strconv.AppendInt(dst, int64(c), 10)
			m := masks[int(c)*w : int(c)*w+e.maxX[c]+1]
			lo := e.minX[c]
			if lo > 0 {
				dst = appendSixelRun(dst, '?', lo)
			}
			run, prev := 0, byte(0)
			for x := lo; x < len(m); x++ {
				ch := 63 + m[x]
				if run > 0 && ch == prev {
					run++
					continue
				}
				if run > 0 {
					dst = appendSixelRun(dst, prev, run)
				}
				prev, run = ch, 1
			}
			if run > 0 {
				dst = appendSixelRun(dst, prev, run)
			}
			clear(masks[int(c)*w+lo : int(c)*w+e.maxX[c]+1])
			used[c] = false
		}
		if y0+6 < h {
			dst = append(dst, '-') // next band
		}
	}
	return append(dst, "\x1b\\"...)
}

func appendSixelRun(dst []byte, ch byte, n int) []byte {
	if n > 3 {
		dst = append(dst, '!')
		dst = strconv.AppendInt(dst, int64(n), 10)
		return append(dst, ch)
	}
	for ; n > 0; n-- {
		dst = append(dst, ch)
	}
	return dst
}
