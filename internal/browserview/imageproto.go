package browserview

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"image"
	"image/jpeg"
	"image/png"
	"strconv"
	"strings"
)

// kittyChunk is the maximum base64 payload per graphics escape, as required
// by the Kitty graphics protocol.
const kittyChunk = 4096

// kittyIDSpan is the number of distinct image ids an encoder cycles
// through. Every frame gets a fresh id so that a newly transmitted image is
// never written over the one currently on screen.
const kittyIDSpan = 1 << 20

// KittyEncoder emits Kitty graphics protocol sequences. Each frame is
// transmitted (zlib-compressed RGB, chunked base64) and placed under a fresh
// image id; the host deletes the previously displayed id only after the new
// placement (see KittyDeleteSeq). The terminal therefore always has a
// complete image to show and never flickers, even if intermediate frames
// were dropped by coalescing. It is not safe for concurrent use.
type KittyEncoder struct {
	// Tmux wraps each escape in a tmux DCS passthrough.
	Tmux bool

	base uint32
	gen  uint32
	raw  []byte
	zbuf bytes.Buffer
	zw   *zlib.Writer
	b64  []byte
}

// NewKittyEncoder returns an encoder allocating ids from
// [base, base+kittyIDSpan).
func NewKittyEncoder(base uint32) *KittyEncoder {
	if base == 0 || base > 0xFFFFFFFF-kittyIDSpan {
		base = 1
	}
	return &KittyEncoder{base: base}
}

// Encode appends the sequences that transmit img and place it at the
// current cursor position spanning cols x rows cells, without moving the
// cursor. It returns the image id used.
func (k *KittyEncoder) Encode(dst []byte, img *image.RGBA, cols, rows int) ([]byte, uint32) {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	if cap(k.raw) < w*h*3 {
		k.raw = make([]byte, w*h*3)
	}
	raw := k.raw[:w*h*3]
	for y := 0; y < h; y++ {
		src := img.Pix[y*img.Stride : y*img.Stride+w*4]
		out := raw[y*w*3 : (y+1)*w*3]
		for x := 0; x < w; x++ {
			out[x*3], out[x*3+1], out[x*3+2] = src[x*4], src[x*4+1], src[x*4+2]
		}
	}
	k.zbuf.Reset()
	if k.zw == nil {
		k.zw, _ = zlib.NewWriterLevel(&k.zbuf, zlib.BestSpeed)
	} else {
		k.zw.Reset(&k.zbuf)
	}
	_, _ = k.zw.Write(raw)
	_ = k.zw.Close()
	n := base64.StdEncoding.EncodedLen(k.zbuf.Len())
	if cap(k.b64) < n {
		k.b64 = make([]byte, n)
	}
	payload := k.b64[:n]
	base64.StdEncoding.Encode(payload, k.zbuf.Bytes())

	id := k.base + k.gen%kittyIDSpan
	k.gen++
	var seq []byte
	for off := 0; off < len(payload); off += kittyChunk {
		end := min(off+kittyChunk, len(payload))
		seq = seq[:0]
		seq = append(seq, "\x1b_G"...)
		if off == 0 {
			seq = append(seq, "a=T,q=2,f=24,o=z,C=1,s="...)
			seq = strconv.AppendInt(seq, int64(w), 10)
			seq = append(seq, ",v="...)
			seq = strconv.AppendInt(seq, int64(h), 10)
			seq = append(seq, ",c="...)
			seq = strconv.AppendInt(seq, int64(cols), 10)
			seq = append(seq, ",r="...)
			seq = strconv.AppendInt(seq, int64(rows), 10)
			seq = append(seq, ",i="...)
			seq = strconv.AppendUint(seq, uint64(id), 10)
			seq = append(seq, ",p=1,"...)
		} else {
			seq = append(seq, "q=2,"...)
		}
		if end < len(payload) {
			seq = append(seq, "m=1;"...)
		} else {
			seq = append(seq, "m=0;"...)
		}
		seq = append(seq, payload[off:end]...)
		seq = append(seq, "\x1b\\"...)
		dst = wrapSeq(dst, seq, k.Tmux)
	}
	return dst, id
}

// KittyDeleteSeq returns the sequence deleting image id and freeing its
// data, wrapped for tmux when tmux is true.
func KittyDeleteSeq(id uint32, tmux bool) []byte {
	return wrapSeq(nil, kittyDelete(id), tmux)
}

func kittyDelete(id uint32) []byte {
	b := []byte("\x1b_Ga=d,d=I,q=2,i=")
	b = strconv.AppendUint(b, uint64(id), 10)
	return append(b, "\x1b\\"...)
}

func wrapSeq(dst, seq []byte, tmux bool) []byte {
	if !tmux {
		return append(dst, seq...)
	}
	return appendTmux(dst, seq)
}

// appendTmux wraps seq in a tmux passthrough DCS, doubling every ESC.
func appendTmux(dst, seq []byte) []byte {
	dst = append(dst, "\x1bPtmux;"...)
	for _, c := range seq {
		if c == 0x1b {
			dst = append(dst, 0x1b)
		}
		dst = append(dst, c)
	}
	return append(dst, "\x1b\\"...)
}

// ITermEncoder emits iTerm2 inline image sequences (OSC 1337), also
// understood by WezTerm, mintty and Konsole. It is not safe for concurrent
// use.
type ITermEncoder struct {
	// Tmux wraps the sequence in a tmux DCS passthrough.
	Tmux bool
	// JPEGQuality controls the transport encoding; 0 uses PNG (lossless).
	JPEGQuality int

	buf bytes.Buffer
	b64 []byte
	png png.Encoder
}

// NewITermEncoder returns an encoder transporting frames as JPEG at the
// given quality, or PNG when quality is 0.
func NewITermEncoder(jpegQuality int) *ITermEncoder {
	return &ITermEncoder{JPEGQuality: jpegQuality, png: png.Encoder{CompressionLevel: png.BestSpeed}}
}

// Encode appends an OSC 1337 inline image sized to cols x rows cells. The
// image is drawn at the cursor position.
func (e *ITermEncoder) Encode(dst []byte, img *image.RGBA, cols, rows int) ([]byte, error) {
	e.buf.Reset()
	var err error
	if e.JPEGQuality > 0 {
		err = jpeg.Encode(&e.buf, img, &jpeg.Options{Quality: e.JPEGQuality})
	} else {
		err = e.png.Encode(&e.buf, img)
	}
	if err != nil {
		return dst, err
	}
	n := base64.StdEncoding.EncodedLen(e.buf.Len())
	if cap(e.b64) < n {
		e.b64 = make([]byte, n)
	}
	payload := e.b64[:n]
	base64.StdEncoding.Encode(payload, e.buf.Bytes())

	var seq []byte
	seq = append(seq, "\x1b]1337;File=inline=1;size="...)
	seq = strconv.AppendInt(seq, int64(e.buf.Len()), 10)
	seq = append(seq, ";width="...)
	seq = strconv.AppendInt(seq, int64(cols), 10)
	seq = append(seq, ";height="...)
	seq = strconv.AppendInt(seq, int64(rows), 10)
	seq = append(seq, ";preserveAspectRatio=0;doNotMoveCursor=1:"...)
	seq = append(seq, payload...)
	seq = append(seq, '\a')
	if e.Tmux {
		return appendTmux(dst, seq), nil
	}
	return append(dst, seq...), nil
}

// blankArea returns sequences that erase a rows x cols block of cells at the
// 1-based screen position (row, col), used to remove sixel/iTerm images.
func blankArea(row, col, rows, cols int) string {
	if rows <= 0 || cols <= 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(cursorSave)
	sb.WriteString("\x1b[0m")
	for i := 0; i < rows; i++ {
		writeCUP(&sb, row+i, col)
		sb.WriteString("\x1b[")
		sb.WriteString(strconv.Itoa(cols))
		sb.WriteByte('X')
	}
	sb.WriteString(cursorRestore)
	return sb.String()
}
