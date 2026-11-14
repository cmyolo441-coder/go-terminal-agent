package browserview

import (
	"image"
	"image/color"
	"math"
	"testing"
)

// screenshot1280 returns a synthetic 1280x800 frame with UI-like content:
// flat panels, a gradient hero and some high-frequency "text".
func screenshot1280() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 1280, 800))
	for y := 0; y < 800; y++ {
		for x := 0; x < 1280; x++ {
			var c color.RGBA
			switch {
			case y < 64:
				c = color.RGBA{36, 41, 47, 255}
			case y < 360:
				c = color.RGBA{uint8(x * 255 / 1279), uint8(100 + y/4), uint8(200 - x/8), 255}
			default:
				c = color.RGBA{250, 250, 250, 255}
				if (x/7+y/11)%3 == 0 && x%7 < 5 && y%11 < 8 {
					c = color.RGBA{20, 20, 20, 255}
				}
			}
			c.R = uint8(math.Min(255, float64(c.R)+float64((x*y)%5)))
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

func BenchmarkResize1280x800To160x96(b *testing.B) {
	src := screenshot1280()
	var r Resizer
	var dst *image.RGBA
	b.ReportAllocs()
	for b.Loop() {
		dst = r.Resize(dst, src, 160, 96)
	}
}

func BenchmarkResize1280x800To640x400(b *testing.B) {
	src := screenshot1280()
	var r Resizer
	var dst *image.RGBA
	b.ReportAllocs()
	for b.Loop() {
		dst = r.Resize(dst, src, 640, 400)
	}
}

func BenchmarkHalfBlock1280x800(b *testing.B) {
	src := screenshot1280()
	var r Resizer
	scaled := r.Resize(nil, src, 160, 96)
	b.ReportAllocs()
	for b.Loop() {
		enc := NewHalfBlockEncoder(true) // fresh: no dirty-row reuse
		enc.Encode(scaled)
	}
}

func BenchmarkHalfBlockUnchanged(b *testing.B) {
	src := screenshot1280()
	var r Resizer
	scaled := r.Resize(nil, src, 160, 96)
	enc := NewHalfBlockEncoder(true)
	enc.Encode(scaled)
	b.ReportAllocs()
	for b.Loop() {
		enc.Encode(scaled)
	}
}

func BenchmarkSixelEncode1280x800(b *testing.B) {
	src := screenshot1280()
	enc := NewSixelEncoder(256, DitherOrdered)
	var out []byte
	b.ReportAllocs()
	for b.Loop() {
		out = enc.Encode(out[:0], src)
	}
	b.ReportMetric(float64(len(out)), "bytes/frame")
}

func BenchmarkSixelEncode1280x800FloydSteinberg(b *testing.B) {
	src := screenshot1280()
	enc := NewSixelEncoder(256, DitherFloydSteinberg)
	var out []byte
	b.ReportAllocs()
	for b.Loop() {
		out = enc.Encode(out[:0], src)
	}
}

func BenchmarkRenderPipelineHalfBlock(b *testing.B) {
	data := encodePNG(b, screenshot1280())
	r := NewRenderer(1)
	p := RenderParams{Protocol: ProtocolHalfBlock, TrueColor: true, Cols: 120, Rows: 40, Cell: DefaultCellSize}
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		i++
		p.Overlay.Cursor = &image.Point{i % 1280, 400} // force a re-render, decode cached
		if _, _, err := r.Render(Frame{Data: data}, p); err != nil {
			b.Fatal(err)
		}
	}
}
