package browserview

import (
	"hash/maphash"
	"image"
	"slices"
	"strconv"
	"strings"
)

// HalfBlockEncoder renders images with the Unicode upper half block "▀":
// each character cell shows two vertical pixels, the upper one as the
// foreground colour and the lower one as the background colour.
//
// The encoder keeps the previous frame's lines and a hash per cell row, so
// rows whose pixels did not change are not re-encoded and are reported as
// clean, enabling dirty-row terminal updates (see DiffLines). It is not safe
// for concurrent use.
type HalfBlockEncoder struct {
	// TrueColor selects 24-bit SGR colours; otherwise colours are
	// downgraded to the xterm 256-colour palette.
	TrueColor bool

	lines    []string
	hashes   []uint64
	width    int
	trueMode bool
	sb       strings.Builder
	h        maphash.Hash
}

// NewHalfBlockEncoder returns an encoder for the given colour depth.
func NewHalfBlockEncoder(trueColor bool) *HalfBlockEncoder {
	e := &HalfBlockEncoder{TrueColor: trueColor}
	e.h.SetSeed(hashSeed)
	return e
}

// Reset forgets the previous frame so the next Encode reports every row as
// dirty.
func (e *HalfBlockEncoder) Reset() {
	e.lines, e.hashes, e.width = e.lines[:0], e.hashes[:0], 0
}

// Encode converts img (whose height should be even) to ceil(h/2) lines, each
// exactly img width cells wide and terminated with an SGR reset. dirty lists
// the indexes of lines that differ from the previous call. The returned
// slice is a fresh copy that the caller may retain.
func (e *HalfBlockEncoder) Encode(img *image.RGBA) (lines []string, dirty []int) {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	rows := (h + 1) / 2
	if w != e.width || e.trueMode != e.TrueColor || len(e.lines) != rows {
		e.lines = make([]string, rows)
		e.hashes = make([]uint64, rows)
		e.width, e.trueMode = w, e.TrueColor
		for i := range e.lines {
			e.lines[i] = "\x00" // sentinel: never equal to a rendered line
		}
	}
	for r := 0; r < rows; r++ {
		top := img.Pix[(2*r)*img.Stride : (2*r)*img.Stride+w*4]
		var bot []byte
		if 2*r+1 < h {
			bot = img.Pix[(2*r+1)*img.Stride : (2*r+1)*img.Stride+w*4]
		}
		e.h.Reset()
		_, _ = e.h.Write(top)
		_, _ = e.h.Write(bot)
		sum := e.h.Sum64()
		if sum == e.hashes[r] && e.lines[r] != "\x00" {
			continue
		}
		e.hashes[r] = sum
		e.lines[r] = e.encodeRow(top, bot)
		dirty = append(dirty, r)
	}
	return slices.Clone(e.lines), dirty
}

func (e *HalfBlockEncoder) encodeRow(top, bot []byte) string {
	sb := &e.sb
	sb.Reset()
	sb.Grow(len(top) * 6)
	var buf [48]byte
	haveFG, haveBG := false, false
	var fg, bg uint32
	for x := 0; x < len(top); x += 4 {
		t := e.key(top[x], top[x+1], top[x+2])
		if bot == nil {
			if !haveFG || fg != t {
				sb.Write(e.sgr(buf[:0], 38, t))
				fg, haveFG = t, true
			}
			if haveBG {
				sb.WriteString("\x1b[49m")
				haveBG = false
			}
			sb.WriteString("▀")
			continue
		}
		b := e.key(bot[x], bot[x+1], bot[x+2])
		if t == b {
			// A space shows only the background; leave the fg untouched.
			if !haveBG || bg != b {
				sb.Write(e.sgr(buf[:0], 48, b))
				bg, haveBG = b, true
			}
			sb.WriteByte(' ')
			continue
		}
		if !haveFG || fg != t {
			sb.Write(e.sgr(buf[:0], 38, t))
			fg, haveFG = t, true
		}
		if !haveBG || bg != b {
			sb.Write(e.sgr(buf[:0], 48, b))
			bg, haveBG = b, true
		}
		sb.WriteString("▀")
	}
	sb.WriteString("\x1b[0m")
	return sb.String()
}

// key returns a packed colour: 24-bit RGB or a 256-colour index.
func (e *HalfBlockEncoder) key(r, g, b uint8) uint32 {
	if e.trueMode {
		return uint32(r)<<16 | uint32(g)<<8 | uint32(b)
	}
	return uint32(XTerm256(r, g, b))
}

func (e *HalfBlockEncoder) sgr(dst []byte, base int, c uint32) []byte {
	dst = append(dst, "\x1b["...)
	dst = strconv.AppendInt(dst, int64(base), 10)
	if e.trueMode {
		dst = append(dst, ";2;"...)
		dst = strconv.AppendInt(dst, int64(c>>16), 10)
		dst = append(dst, ';')
		dst = strconv.AppendInt(dst, int64(c>>8&0xFF), 10)
		dst = append(dst, ';')
		dst = strconv.AppendInt(dst, int64(c&0xFF), 10)
	} else {
		dst = append(dst, ";5;"...)
		dst = strconv.AppendInt(dst, int64(c), 10)
	}
	return append(dst, 'm')
}

var cubeLevels = [6]int{0, 95, 135, 175, 215, 255}

func cubeIndex(v uint8) int {
	switch {
	case v < 48:
		return 0
	case v < 115:
		return 1
	}
	return (int(v) - 35) / 40
}

// XTerm256 maps an RGB colour to the nearest entry of the xterm 256-colour
// palette, choosing between the 6x6x6 cube and the 24-step grey ramp.
func XTerm256(r, g, b uint8) uint8 {
	ri, gi, bi := cubeIndex(r), cubeIndex(g), cubeIndex(b)
	cr, cg, cb := cubeLevels[ri], cubeLevels[gi], cubeLevels[bi]
	avg := (int(r) + int(g) + int(b)) / 3
	gi2 := 23
	if avg < 238 {
		gi2 = (avg - 3) / 10
		if gi2 < 0 {
			gi2 = 0
		}
	}
	gv := 8 + 10*gi2
	dist := func(x, y, z int) int {
		dr, dg, db := int(r)-x, int(g)-y, int(b)-z
		return dr*dr + dg*dg + db*db
	}
	if dist(gv, gv, gv) < dist(cr, cg, cb) {
		return uint8(232 + gi2)
	}
	return uint8(16 + 36*ri + 6*gi + bi)
}

// DiffLines returns the escape sequence that updates a terminal from prev to
// cur, writing only changed lines. Lines are placed at the 1-based screen
// position (row+i, col). Lines present in prev but not in cur are erased
// for width cells. The cursor is saved and restored around the update.
func DiffLines(prev, cur []string, row, col, width int) string {
	var sb strings.Builder
	for i, l := range cur {
		if i < len(prev) && prev[i] == l {
			continue
		}
		writeCUP(&sb, row+i, col)
		sb.WriteString(l)
	}
	for i := len(cur); i < len(prev); i++ {
		writeCUP(&sb, row+i, col)
		sb.WriteString("\x1b[0m\x1b[")
		sb.WriteString(strconv.Itoa(width))
		sb.WriteByte('X')
	}
	if sb.Len() == 0 {
		return ""
	}
	return cursorSave + sb.String() + cursorRestore
}

const (
	cursorSave    = "\x1b7"
	cursorRestore = "\x1b8"
)

func writeCUP(sb *strings.Builder, row, col int) {
	sb.WriteString("\x1b[")
	sb.WriteString(strconv.Itoa(max(row, 1)))
	sb.WriteByte(';')
	sb.WriteString(strconv.Itoa(max(col, 1)))
	sb.WriteByte('H')
}
