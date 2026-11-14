// Package browserview renders a live, low-latency view of the agent's
// browser next to the terminal CLI and serves an optional local web viewer.
//
// The package is split into four layers:
//
//   - Frame sources (Source) deliver encoded screenshots (JPEG/PNG), usually
//     from a CDP screencast. Input lets a human take control from the web
//     viewer.
//   - The smoothness engine (Latest, Pacer, Renderer) coalesces frames so a
//     slow consumer only ever sees the newest one, paces encoding to a target
//     FPS, skips unchanged frames by hash and performs all decoding, scaling
//     and terminal encoding off the UI goroutine with pooled buffers.
//   - Terminal encoders: Kitty graphics, iTerm2 inline images, Sixel (own
//     median-cut quantizer + dithering) and a Unicode half-block fallback with
//     dirty-row diffing and 256-colour downgrade.
//   - Pane, a bubbletea component, and Serve, a loopback-only HTTP server with
//     a hand-rolled RFC 6455 WebSocket and an MJPEG fallback.
package browserview

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"time"
)

// Frame is one encoded screenshot of the browser viewport.
type Frame struct {
	// Data holds the encoded image bytes. Consumers treat it as immutable.
	Data []byte
	// Format is "jpeg" or "png". An empty Format is sniffed from Data.
	Format string
	// Width and Height are the image dimensions in CSS/device pixels as
	// reported by the producer. They may be zero when unknown.
	Width, Height int
	// Seq increases monotonically per source.
	Seq uint64
	// Timestamp is the capture time.
	Timestamp time.Time
}

// Source produces frames. Subscribe returns a channel that is closed when
// ctx is cancelled or the source ends. Implementations should never block
// on a slow subscriber: dropping intermediate frames is expected and
// consumers in this package only ever look at the newest frame.
type Source interface {
	Subscribe(ctx context.Context) (<-chan Frame, error)
}

// Input is implemented by browsers that accept synthetic input. It is used
// for interactive takeover from the web viewer. Coordinates are in frame
// pixel space (the same space as Frame.Width/Height).
type Input interface {
	MouseMove(ctx context.Context, x, y float64) error
	MouseDown(ctx context.Context, x, y float64, button string, clickCount int) error
	MouseUp(ctx context.Context, x, y float64, button string, clickCount int) error
	Wheel(ctx context.Context, x, y, dx, dy float64) error
	KeyDown(ctx context.Context, key string) error
	KeyUp(ctx context.Context, key string) error
	InsertText(ctx context.Context, text string) error
}

// Overlay describes decorations drawn on top of frames: an animated cursor,
// a highlight around the element being acted on and an action label.
// Coordinates are in frame pixel space.
type Overlay struct {
	Cursor    *image.Point
	Highlight *image.Rectangle
	Label     string
	URL       string
	Status    string
}

// Clone returns a deep copy so callers may keep mutating their own value.
func (o Overlay) Clone() Overlay {
	c := o
	if o.Cursor != nil {
		p := *o.Cursor
		c.Cursor = &p
	}
	if o.Highlight != nil {
		r := *o.Highlight
		c.Highlight = &r
	}
	return c
}

// Equal reports whether two overlays describe the same decoration.
func (o Overlay) Equal(p Overlay) bool {
	if o.Label != p.Label || o.URL != p.URL || o.Status != p.Status {
		return false
	}
	if (o.Cursor == nil) != (p.Cursor == nil) || (o.Cursor != nil && *o.Cursor != *p.Cursor) {
		return false
	}
	if (o.Highlight == nil) != (p.Highlight == nil) || (o.Highlight != nil && *o.Highlight != *p.Highlight) {
		return false
	}
	return true
}

// visualEqual compares only the parts drawn onto the image.
func (o Overlay) visualEqual(p Overlay) bool {
	return Overlay{Cursor: o.Cursor, Highlight: o.Highlight}.Equal(Overlay{Cursor: p.Cursor, Highlight: p.Highlight})
}

// visual reports whether the overlay draws anything on the image itself.
func (o Overlay) visual() bool { return o.Cursor != nil || o.Highlight != nil }

// ErrUnknownFormat is returned when frame data is neither JPEG nor PNG.
var ErrUnknownFormat = errors.New("browserview: unknown image format")

var pngMagic = []byte("\x89PNG\r\n\x1a\n")

// SniffFormat returns "jpeg", "png" or "" for the given encoded bytes.
func SniffFormat(data []byte) string {
	switch {
	case len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return "jpeg"
	case bytes.HasPrefix(data, pngMagic):
		return "png"
	}
	return ""
}

// format returns the effective, normalised format of f.
func (f Frame) format() string {
	switch f.Format {
	case "jpeg", "jpg", "image/jpeg":
		return "jpeg"
	case "png", "image/png":
		return "png"
	}
	return SniffFormat(f.Data)
}

// MIME returns the MIME type for the frame's format.
func (f Frame) MIME() string {
	if f.format() == "png" {
		return "image/png"
	}
	return "image/jpeg"
}

// Decode decodes the frame into an image.
func (f Frame) Decode() (image.Image, error) {
	r := bytes.NewReader(f.Data)
	switch f.format() {
	case "jpeg":
		img, err := jpeg.Decode(r)
		if err != nil {
			return nil, fmt.Errorf("browserview: decode jpeg frame %d: %w", f.Seq, err)
		}
		return img, nil
	case "png":
		img, err := png.Decode(r)
		if err != nil {
			return nil, fmt.Errorf("browserview: decode png frame %d: %w", f.Seq, err)
		}
		return img, nil
	}
	return nil, ErrUnknownFormat
}

// decodeConfig returns the frame's dimensions without decoding pixels.
func decodeConfig(f Frame) (image.Config, error) {
	r := bytes.NewReader(f.Data)
	switch f.format() {
	case "jpeg":
		return jpeg.DecodeConfig(r)
	case "png":
		return png.DecodeConfig(r)
	}
	return image.Config{}, ErrUnknownFormat
}
