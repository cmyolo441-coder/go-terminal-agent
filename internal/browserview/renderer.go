package browserview

import (
	"image"
	"time"
)

// RenderParams controls how a frame is fitted and encoded.
type RenderParams struct {
	Protocol  Protocol
	TrueColor bool
	Tmux      bool
	// Cols and Rows are the cell area available for the image.
	Cols, Rows int
	Cell       CellSize
	Overlay    Overlay
	// Cursor, when non-nil, overrides Overlay.Cursor (animation).
	Cursor      *image.Point
	Dither      Dither
	SixelColors int
	JPEGQuality int
}

func (p RenderParams) equal(q RenderParams) bool {
	if (p.Cursor == nil) != (q.Cursor == nil) || (p.Cursor != nil && *p.Cursor != *q.Cursor) {
		return false
	}
	return p.Protocol == q.Protocol && p.TrueColor == q.TrueColor && p.Tmux == q.Tmux &&
		p.Cols == q.Cols && p.Rows == q.Rows && p.Cell == q.Cell && p.Dither == q.Dither &&
		p.SixelColors == q.SixelColors && p.JPEGQuality == q.JPEGQuality &&
		p.Overlay.visualEqual(q.Overlay)
}

// Rendered is the terminal representation of one frame.
type Rendered struct {
	Protocol Protocol
	Seq      uint64
	// FrameTime is the frame's capture timestamp.
	FrameTime time.Time
	// FrameW and FrameH are the decoded frame dimensions.
	FrameW, FrameH int
	// Layout places the image within the RenderParams cell area.
	Layout Layout
	// Lines holds Layout.Rows half-block lines of Layout.Cols cells
	// (ProtocolHalfBlock only).
	Lines []string
	// Dirty lists lines that changed relative to the previous render.
	Dirty []int
	// Image is the escape payload that draws the image at the cursor
	// position (image protocols only). It does not position the cursor.
	Image []byte
	// KittyID is the image id used by a Kitty payload.
	KittyID uint32
	// EncodeTime is the wall time spent decoding, resizing and encoding.
	EncodeTime time.Duration
}

// Renderer turns frames into terminal output. It caches the decoded frame
// so overlay or size changes do not re-decode, skips work entirely when
// neither the frame bytes nor the parameters changed, and reuses all large
// buffers. A Renderer is not safe for concurrent use; the Pane runs one on
// a dedicated goroutine.
type Renderer struct {
	resizer Resizer
	scaled  *image.RGBA
	hb      *HalfBlockEncoder
	sixel   *SixelEncoder
	kitty   *KittyEncoder
	iterm   *ITermEncoder
	out     []byte

	decoded     *image.RGBA
	decodedHash uint64
	decodedOK   bool

	lastHash   uint64
	lastParams RenderParams
	lastOK     bool
}

// NewRenderer returns a renderer; kittyBase selects the Kitty image id range.
func NewRenderer(kittyBase uint32) *Renderer {
	return &Renderer{kitty: NewKittyEncoder(kittyBase)}
}

// Invalidate forces the next Render to produce output even if nothing
// changed (e.g. after the terminal was cleared).
func (r *Renderer) Invalidate() {
	r.lastOK = false
	if r.hb != nil {
		r.hb.Reset()
	}
}

// Render encodes f with p. changed is false when the output would be
// identical to the previous call, in which case no work is done.
func (r *Renderer) Render(f Frame, p RenderParams) (out Rendered, changed bool, err error) {
	start := time.Now()
	h := hashBytes(f.Data)
	if r.lastOK && h == r.lastHash && p.equal(r.lastParams) {
		return Rendered{}, false, nil
	}
	if !r.decodedOK || h != r.decodedHash {
		img, err := f.Decode()
		if err != nil {
			return Rendered{}, false, err
		}
		// Keep a private RGBA copy: decoders return fresh images, and
		// converting once lets later resizes take the fast path.
		rgba := r.resizer.ToRGBA(img)
		if rgba == img {
			cp := ensureRGBA(r.decoded, rgba.Rect.Dx(), rgba.Rect.Dy())
			copy(cp.Pix, rgba.Pix)
			rgba = cp
		} else {
			// Hand the conversion buffer to r.decoded and recycle the old
			// decoded buffer for the next conversion.
			r.resizer.src = r.decoded
		}
		r.decoded, r.decodedHash, r.decodedOK = rgba, h, true
	}
	fw, fh := r.decoded.Rect.Dx(), r.decoded.Rect.Dy()
	out = Rendered{Protocol: p.Protocol, Seq: f.Seq, FrameTime: f.Timestamp, FrameW: fw, FrameH: fh}
	if p.Protocol == ProtocolNone || p.Cols <= 0 || p.Rows <= 0 {
		r.commit(h, p)
		return out, true, nil
	}
	half := !p.Protocol.IsImage()
	l := FitCells(fw, fh, p.Cols, p.Rows, p.Cell, half)
	out.Layout = l
	r.scaled = r.resizer.Resize(r.scaled, r.decoded, l.PixW, l.PixH)
	if p.Overlay.visual() || p.Cursor != nil {
		drawOverlay(r.scaled, p.Overlay, p.Cursor, float64(l.PixW)/float64(fw), float64(l.PixH)/float64(fh))
	}
	switch p.Protocol {
	case ProtocolKitty:
		r.kitty.Tmux = p.Tmux
		r.out, out.KittyID = r.kitty.Encode(r.out[:0], r.scaled, l.Cols, l.Rows)
		out.Image = append([]byte(nil), r.out...)
	case ProtocolSixel:
		colors := p.SixelColors
		if colors <= 0 || colors > 256 {
			colors = 256
		}
		if r.sixel == nil {
			r.sixel = NewSixelEncoder(colors, p.Dither)
		}
		r.sixel.Quantizer.MaxColors, r.sixel.Quantizer.Dither = colors, p.Dither
		r.out = r.sixel.Encode(r.out[:0], r.scaled)
		if p.Tmux {
			out.Image = appendTmux(nil, r.out)
		} else {
			out.Image = append([]byte(nil), r.out...)
		}
	case ProtocolITerm2:
		q := p.JPEGQuality
		if q == 0 {
			q = 85
		}
		if r.iterm == nil {
			r.iterm = NewITermEncoder(q)
		}
		r.iterm.JPEGQuality, r.iterm.Tmux = q, p.Tmux
		r.out, err = r.iterm.Encode(r.out[:0], r.scaled, l.Cols, l.Rows)
		if err != nil {
			return Rendered{}, false, err
		}
		out.Image = append([]byte(nil), r.out...)
	default:
		if r.hb == nil {
			r.hb = NewHalfBlockEncoder(p.TrueColor)
		}
		r.hb.TrueColor = p.TrueColor
		out.Lines, out.Dirty = r.hb.Encode(r.scaled)
	}
	r.commit(h, p)
	out.EncodeTime = time.Since(start)
	return out, true, nil
}

func (r *Renderer) commit(h uint64, p RenderParams) {
	r.lastHash, r.lastParams, r.lastOK = h, p.cloneParams(), true
}

func (p RenderParams) cloneParams() RenderParams {
	c := p
	c.Overlay = p.Overlay.Clone()
	if p.Cursor != nil {
		cp := *p.Cursor
		c.Cursor = &cp
	}
	return c
}
