package browserview

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
	"time"
)

var (
	red   = color.RGBA{255, 0, 0, 255}
	blue  = color.RGBA{0, 0, 255, 255}
	green = color.RGBA{0, 255, 0, 255}
)

func solid(w, h int, c color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, c.A
	}
	return img
}

func gradient(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{uint8(x * 255 / max(w-1, 1)), uint8(y * 255 / max(h-1, 1)), uint8((x + y) * 255 / max(w+h-2, 1)), 255})
		}
	}
	return img
}

func encodePNG(t testing.TB, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func encodeJPEG(t testing.TB, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestSniffAndDecode(t *testing.T) {
	img := gradient(16, 8)
	for _, tc := range []struct {
		data []byte
		want string
	}{{encodePNG(t, img), "png"}, {encodeJPEG(t, img), "jpeg"}, {[]byte("nope"), ""}} {
		if got := SniffFormat(tc.data); got != tc.want {
			t.Fatalf("SniffFormat = %q, want %q", got, tc.want)
		}
		f := Frame{Data: tc.data}
		dec, err := f.Decode()
		if tc.want == "" {
			if err == nil {
				t.Fatal("expected error for unknown format")
			}
			continue
		}
		if err != nil || dec.Bounds().Dx() != 16 || dec.Bounds().Dy() != 8 {
			t.Fatalf("decode %s: %v %v", tc.want, err, dec)
		}
		cfg, err := decodeConfig(f)
		if err != nil || cfg.Width != 16 || cfg.Height != 8 {
			t.Fatalf("decodeConfig: %v %+v", err, cfg)
		}
	}
}

func TestFitCells(t *testing.T) {
	cell := CellSize{W: 8, H: 16}
	// 1280x800 into 80x24 cells (640x384 px): height bound, s = 0.48.
	l := FitCells(1280, 800, 80, 24, cell, false)
	if l.PixW != 614 || l.PixH != 384 || l.Rows != 24 || l.Cols != 77 || l.OffsetCol != 1 {
		t.Fatalf("image layout = %+v", l)
	}
	hb := FitCells(1280, 800, 80, 24, cell, true)
	if hb.PixW != hb.Cols || hb.PixH != 2*hb.Rows || hb.Rows != 24 || hb.Cols != 77 {
		t.Fatalf("half-block layout = %+v", hb)
	}
	// Wide image is width bound and centred vertically.
	w := FitCells(2000, 200, 40, 20, cell, true)
	if w.Cols != 40 || w.Rows != 2 || w.OffsetRow != 9 {
		t.Fatalf("wide layout = %+v", w)
	}
	if (FitCells(0, 10, 10, 10, cell, true) != Layout{}) {
		t.Fatal("zero image should give empty layout")
	}
	// Unknown cell size falls back to the default.
	if FitCells(1280, 800, 80, 24, CellSize{}, false) != l {
		t.Fatal("default cell size not applied")
	}
}

func TestResizeFlatColourPreserved(t *testing.T) {
	var r Resizer
	src := solid(97, 61, color.RGBA{12, 200, 99, 255})
	for _, sz := range [][2]int{{10, 7}, {97, 61}, {300, 200}, {1, 1}} {
		dst := r.Resize(nil, src, sz[0], sz[1])
		for i := 0; i < len(dst.Pix); i += 4 {
			if dst.Pix[i] != 12 || dst.Pix[i+1] != 200 || dst.Pix[i+2] != 99 || dst.Pix[i+3] != 255 {
				t.Fatalf("%v: pixel %d = %v", sz, i/4, dst.Pix[i:i+4])
			}
		}
	}
}

func TestResizeAreaAverage(t *testing.T) {
	var r Resizer
	// 2x2 black/white checkerboard averages to mid grey.
	src := image.NewRGBA(image.Rect(0, 0, 2, 2))
	src.SetRGBA(0, 0, color.RGBA{255, 255, 255, 255})
	src.SetRGBA(1, 1, color.RGBA{255, 255, 255, 255})
	src.SetRGBA(1, 0, color.RGBA{0, 0, 0, 255})
	src.SetRGBA(0, 1, color.RGBA{0, 0, 0, 255})
	dst := r.Resize(nil, src, 1, 1)
	if v := dst.Pix[0]; v < 127 || v > 128 {
		t.Fatalf("average = %d, want ~127.5", v)
	}
	// 3 -> 2 downscale: [0, 90, 180] -> [30, 150] (overlap weights 2/3,1/3).
	row := image.NewRGBA(image.Rect(0, 0, 3, 1))
	for x, v := range []uint8{0, 90, 180} {
		row.SetRGBA(x, 0, color.RGBA{v, v, v, 255})
	}
	d2 := r.Resize(nil, row, 2, 1)
	if d2.Pix[0] != 30 || d2.Pix[4] != 150 {
		t.Fatalf("3->2 = %d,%d want 30,150", d2.Pix[0], d2.Pix[4])
	}
}

func TestResizeUpscaleBilinear(t *testing.T) {
	var r Resizer
	row := image.NewRGBA(image.Rect(0, 0, 2, 1))
	row.SetRGBA(0, 0, color.RGBA{0, 0, 0, 255})
	row.SetRGBA(1, 0, color.RGBA{200, 200, 200, 255})
	d := r.Resize(nil, row, 4, 1)
	got := []uint8{d.Pix[0], d.Pix[4], d.Pix[8], d.Pix[12]}
	want := []uint8{0, 50, 150, 200}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("bilinear = %v, want %v", got, want)
		}
	}
}

func TestResizeReusesBuffers(t *testing.T) {
	var r Resizer
	src := gradient(640, 400)
	dst := r.Resize(nil, src, 80, 48)
	allocs := testing.AllocsPerRun(5, func() { dst = r.Resize(dst, src, 80, 48) })
	if allocs > 1 {
		t.Fatalf("Resize allocated %.0f times per run, want <= 1", allocs)
	}
}

func TestQuantizerExact(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 4, 2))
	cols := []color.RGBA{red, blue, green, {1, 2, 3, 255}}
	for y := 0; y < 2; y++ {
		for x := 0; x < 4; x++ {
			img.SetRGBA(x, y, cols[(x+y)%4])
		}
	}
	q := Quantizer{MaxColors: 4}
	p := q.Quantize(img)
	if len(q.Palette()) != 4 {
		t.Fatalf("palette size %d", len(q.Palette()))
	}
	for y := 0; y < 2; y++ {
		for x := 0; x < 4; x++ {
			if got := q.Palette()[p.ColorIndexAt(x, y)]; got != img.RGBAAt(x, y) {
				t.Fatalf("(%d,%d) = %v want %v", x, y, got, img.RGBAAt(x, y))
			}
		}
	}
}

func TestQuantizerMedianCut(t *testing.T) {
	img := gradient(128, 96)
	for _, d := range []Dither{DitherNone, DitherOrdered, DitherFloydSteinberg} {
		q := Quantizer{MaxColors: 64, Dither: d}
		p := q.Quantize(img)
		pal := q.Palette()
		if len(pal) == 0 || len(pal) > 64 {
			t.Fatalf("dither %d: palette size %d", d, len(pal))
		}
		var sum float64
		for y := 0; y < 96; y++ {
			for x := 0; x < 128; x++ {
				i := p.ColorIndexAt(x, y)
				if int(i) >= len(pal) {
					t.Fatalf("index %d out of palette", i)
				}
				a, b := img.RGBAAt(x, y), pal[i]
				dr, dg, db := float64(a.R)-float64(b.R), float64(a.G)-float64(b.G), float64(a.B)-float64(b.B)
				sum += dr*dr + dg*dg + db*db
			}
		}
		rmse := sum / (128 * 96 * 3)
		// 64 colours over a smooth gradient: per-channel RMS error is small.
		if rmse > 20*20 {
			t.Fatalf("dither %d: mean squared error %.1f too high", d, rmse)
		}
	}
	// Floyd–Steinberg must preserve the average colour of a region.
	flat := solid(64, 64, color.RGBA{100, 100, 100, 255})
	flat.SetRGBA(0, 0, color.RGBA{0, 0, 0, 255})
	flat.SetRGBA(1, 0, color.RGBA{255, 255, 255, 255})
	flat.SetRGBA(2, 0, color.RGBA{30, 30, 30, 255}) // 4 colours > 2 => median cut
	q := Quantizer{MaxColors: 2, Dither: DitherFloydSteinberg}
	p := q.Quantize(flat)
	var mean float64
	for _, i := range p.Pix {
		mean += float64(q.Palette()[i].R)
	}
	mean /= float64(len(p.Pix))
	if mean < 90 || mean > 110 {
		t.Fatalf("FS dithered mean = %.1f, want ~100", mean)
	}
}

func TestSixelGolden(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.SetRGBA(0, 0, red)
	img.SetRGBA(0, 1, red)
	img.SetRGBA(1, 0, blue)
	img.SetRGBA(1, 1, blue)
	got := string(NewSixelEncoder(256, DitherNone).Encode(nil, img))
	want := "\x1bP0;1;0q\"1;1;2;2#0;2;100;0;0#1;2;0;0;100#0B$#1?B\x1b\\"
	if got != want {
		t.Fatalf("sixel\n got %q\nwant %q", got, want)
	}
	// RLE and multiple bands: 10x7 single colour.
	got = string(NewSixelEncoder(256, DitherNone).Encode(nil, solid(10, 7, green)))
	want = "\x1bP0;1;0q\"1;1;10;7#0;2;0;100;0#0!10~-#0!10@\x1b\\"
	if got != want {
		t.Fatalf("sixel rle\n got %q\nwant %q", got, want)
	}
}

func TestSixelEncoderManyColours(t *testing.T) {
	enc := NewSixelEncoder(256, DitherOrdered)
	out := string(enc.Encode(nil, gradient(64, 40)))
	if !strings.HasPrefix(out, "\x1bP0;1;0q\"1;1;64;40") || !strings.HasSuffix(out, "\x1b\\") {
		t.Fatalf("bad framing: %q...", out[:40])
	}
	if strings.Count(out, "-") != 6 { // 40 rows = 7 bands
		t.Fatalf("band separators = %d, want 6", strings.Count(out, "-"))
	}
	if n := strings.Count(out, ";2;"); n > 256 {
		t.Fatalf("%d colour registers", n)
	}
}

func TestHalfBlockGolden(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.SetRGBA(0, 0, red)
	img.SetRGBA(1, 0, red)
	img.SetRGBA(0, 1, blue)
	img.SetRGBA(1, 1, red)
	lines, dirty := NewHalfBlockEncoder(true).Encode(img)
	want := "\x1b[38;2;255;0;0m\x1b[48;2;0;0;255m▀\x1b[48;2;255;0;0m \x1b[0m"
	if len(lines) != 1 || lines[0] != want || len(dirty) != 1 {
		t.Fatalf("truecolor\n got %q\nwant %q", lines, want)
	}
	lines, _ = NewHalfBlockEncoder(false).Encode(img)
	want = "\x1b[38;5;196m\x1b[48;5;21m▀\x1b[48;5;196m \x1b[0m"
	if lines[0] != want {
		t.Fatalf("256\n got %q\nwant %q", lines[0], want)
	}
	if w := StringWidth(lines[0]); w != 2 {
		t.Fatalf("width = %d", w)
	}
	// Odd height: last row uses the default background.
	odd := solid(1, 3, green)
	lines, _ = NewHalfBlockEncoder(true).Encode(odd)
	if len(lines) != 2 || lines[1] != "\x1b[38;2;0;255;0m▀\x1b[0m" {
		t.Fatalf("odd height: %q", lines)
	}
}

func TestXTerm256(t *testing.T) {
	for _, tc := range []struct {
		r, g, b uint8
		want    uint8
	}{
		{0, 0, 0, 16}, {255, 255, 255, 231}, {255, 0, 0, 196}, {0, 0, 255, 21},
		{128, 128, 128, 244}, {8, 8, 8, 232}, {95, 135, 175, 67},
	} {
		if got := XTerm256(tc.r, tc.g, tc.b); got != tc.want {
			t.Errorf("XTerm256(%d,%d,%d) = %d, want %d", tc.r, tc.g, tc.b, got, tc.want)
		}
	}
}

func TestHalfBlockDirtyRows(t *testing.T) {
	enc := NewHalfBlockEncoder(true)
	img := gradient(20, 10)
	first, dirty := enc.Encode(img)
	if len(dirty) != 5 {
		t.Fatalf("first encode dirty = %v", dirty)
	}
	same, dirty := enc.Encode(img)
	if len(dirty) != 0 {
		t.Fatalf("unchanged frame dirty = %v", dirty)
	}
	img.SetRGBA(3, 7, color.RGBA{1, 2, 3, 255}) // pixel row 7 -> cell row 3
	next, dirty := enc.Encode(img)
	if len(dirty) != 1 || dirty[0] != 3 {
		t.Fatalf("dirty = %v, want [3]", dirty)
	}
	if first[0] != same[0] || next[3] == first[3] {
		t.Fatal("line reuse broken")
	}
	diff := DiffLines(first, next, 5, 10, 20)
	if !strings.HasPrefix(diff, "\x1b7\x1b[8;10H") || !strings.HasSuffix(diff, next[3]+"\x1b8") {
		t.Fatalf("diff = %q", diff)
	}
	if strings.Count(diff, "H") != 1 {
		t.Fatalf("diff wrote more than one line: %q", diff)
	}
	if DiffLines(next, next, 1, 1, 20) != "" {
		t.Fatal("identical frames must produce no output")
	}
	shrink := DiffLines(next, next[:4], 1, 1, 20)
	if !strings.Contains(shrink, "\x1b[5;1H\x1b[0m\x1b[20X") {
		t.Fatalf("shrink did not erase: %q", shrink)
	}
	// Returned lines are copies the caller may keep.
	first[0] = "mutated"
	if again, _ := enc.Encode(img); again[0] == "mutated" {
		t.Fatal("encoder shares slice with caller")
	}
}

// parseKitty splits a stream into APC payloads.
func parseKitty(t *testing.T, s string) (ctrl []string, data []string) {
	t.Helper()
	for len(s) > 0 {
		if !strings.HasPrefix(s, "\x1b_G") {
			t.Fatalf("unexpected bytes %q", s[:min(20, len(s))])
		}
		end := strings.Index(s, "\x1b\\")
		body := s[3:end]
		c, d, _ := strings.Cut(body, ";")
		ctrl, data = append(ctrl, c), append(data, d)
		s = s[end+2:]
	}
	return
}

func TestKittyGolden(t *testing.T) {
	k := NewKittyEncoder(100)
	img := image.NewRGBA(image.Rect(0, 0, 2, 1))
	img.SetRGBA(0, 0, red)
	img.SetRGBA(1, 0, blue)
	out, id := k.Encode(nil, img, 3, 2)
	if id != 100 {
		t.Fatalf("id = %d", id)
	}
	ctrl, data := parseKitty(t, string(out))
	if len(ctrl) != 1 || ctrl[0] != "a=T,q=2,f=24,o=z,C=1,s=2,v=1,c=3,r=2,i=100,p=1,m=0" {
		t.Fatalf("ctrl = %q", ctrl)
	}
	z, err := base64.StdEncoding.DecodeString(data[0])
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zlib.NewReader(bytes.NewReader(z))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(zr)
	if !bytes.Equal(raw, []byte{255, 0, 0, 0, 0, 255}) {
		t.Fatalf("raw = %v", raw)
	}
	_, id2 := k.Encode(nil, img, 3, 2)
	if id2 != 101 {
		t.Fatalf("second id = %d, want fresh id 101", id2)
	}
	if got := string(KittyDeleteSeq(100, false)); got != "\x1b_Ga=d,d=I,q=2,i=100\x1b\\" {
		t.Fatalf("delete = %q", got)
	}
	if got := string(KittyDeleteSeq(7, true)); got != "\x1bPtmux;\x1b\x1b_Ga=d,d=I,q=2,i=7\x1b\x1b\\\x1b\\" {
		t.Fatalf("tmux delete = %q", got)
	}
}

func TestKittyChunking(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for i := range img.Pix {
		img.Pix[i] = uint8(rng.IntN(256)) // incompressible
	}
	out, _ := NewKittyEncoder(1).Encode(nil, img, 8, 4)
	ctrl, data := parseKitty(t, string(out))
	if len(ctrl) < 3 {
		t.Fatalf("expected several chunks, got %d", len(ctrl))
	}
	var all strings.Builder
	for i := range ctrl {
		if len(data[i]) > kittyChunk {
			t.Fatalf("chunk %d has %d bytes", i, len(data[i]))
		}
		last := i == len(ctrl)-1
		if i > 0 && ctrl[i] != "q=2,m="+map[bool]string{true: "0", false: "1"}[last] {
			t.Fatalf("chunk %d ctrl %q", i, ctrl[i])
		}
		if i == 0 && !strings.HasSuffix(ctrl[0], ",m=1") {
			t.Fatalf("first chunk ctrl %q", ctrl[0])
		}
		all.WriteString(data[i])
	}
	z, _ := base64.StdEncoding.DecodeString(all.String())
	zr, err := zlib.NewReader(bytes.NewReader(z))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(zr)
	if len(raw) != 64*64*3 || raw[3] != img.Pix[4] {
		t.Fatalf("payload mismatch (%d bytes)", len(raw))
	}
}

func TestITermGolden(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 1))
	img.SetRGBA(0, 0, red)
	img.SetRGBA(1, 0, green)
	out, err := NewITermEncoder(0).Encode(nil, img, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	head, b64, ok := strings.Cut(s, ":")
	if !ok || !strings.HasSuffix(b64, "\a") {
		t.Fatalf("bad framing %q", s)
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(b64, "\a"))
	if err != nil {
		t.Fatal(err)
	}
	wantHead := "\x1b]1337;File=inline=1;size=" + strconv.Itoa(len(data)) + ";width=2;height=1;preserveAspectRatio=0;doNotMoveCursor=1"
	if head != wantHead {
		t.Fatalf("head = %q, want %q", head, wantHead)
	}
	dec, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if r, g, _, _ := dec.At(1, 0).RGBA(); r != 0 || g != 0xFFFF {
		t.Fatal("pixel mismatch")
	}
	tm, _ := (&ITermEncoder{Tmux: true}).Encode(nil, img, 2, 1)
	if !strings.HasPrefix(string(tm), "\x1bPtmux;\x1b\x1b]1337;") {
		t.Fatalf("tmux wrap: %q", tm[:20])
	}
}

func TestDetectEnv(t *testing.T) {
	cases := []struct {
		env  map[string]string
		want Protocol
		tc   bool
	}{
		{map[string]string{"TERM": "xterm-kitty"}, ProtocolKitty, true},
		{map[string]string{"KITTY_WINDOW_ID": "1", "TERM": "xterm-256color"}, ProtocolKitty, true},
		{map[string]string{"TERM_PROGRAM": "ghostty"}, ProtocolKitty, true},
		{map[string]string{"TERM_PROGRAM": "WezTerm", "WEZTERM_PANE": "0"}, ProtocolITerm2, true},
		{map[string]string{"TERM_PROGRAM": "iTerm.app"}, ProtocolITerm2, true},
		{map[string]string{"TERM": "foot"}, ProtocolSixel, false},
		{map[string]string{"TERM": "mlterm", "COLORTERM": "truecolor"}, ProtocolSixel, true},
		{map[string]string{"TERM": "xterm-256color"}, ProtocolHalfBlock, false},
		{map[string]string{"TERM": "xterm-256color", "COLORTERM": "24bit"}, ProtocolHalfBlock, true},
		{map[string]string{"TERM": "xterm-kitty", "TMUX": "/tmp/x"}, ProtocolHalfBlock, true},
		{map[string]string{"TERM": "xterm-kitty", "GTA_GRAPHICS": "sixel"}, ProtocolSixel, true},
		{map[string]string{"TERM": "xterm-kitty", "GTA_GRAPHICS": "off"}, ProtocolNone, true},
		{map[string]string{"TERM": "foot", "GTA_GRAPHICS": "bogus"}, ProtocolSixel, false},
		{map[string]string{"TERM_PROGRAM": "Apple_Terminal", "COLORTERM": "truecolor"}, ProtocolHalfBlock, false},
	}
	for i, c := range cases {
		got := DetectEnv(func(k string) string { return c.env[k] })
		if got.Protocol != c.want || got.TrueColor != c.tc {
			t.Errorf("case %d %v: got %v truecolor=%v (%s), want %v truecolor=%v", i, c.env, got.Protocol, got.TrueColor, got.Reason, c.want, c.tc)
		}
	}
	if p, ok := ParseProtocol("ITERM2"); !ok || p != ProtocolITerm2 {
		t.Fatal("ParseProtocol case-insensitive")
	}
	if ProtocolKitty.String() != "kitty" || !ProtocolSixel.IsImage() || ProtocolHalfBlock.IsImage() {
		t.Fatal("protocol helpers")
	}
}

// scriptedTTY answers the probe with a canned reply.
type scriptedTTY struct {
	reply   string
	written bytes.Buffer
	r       io.Reader
	block   chan struct{}
}

func (s *scriptedTTY) Write(p []byte) (int, error) { return s.written.Write(p) }
func (s *scriptedTTY) Read(p []byte) (int, error) {
	if s.r == nil {
		s.r = strings.NewReader(s.reply)
	}
	n, err := s.r.Read(p)
	if err == io.EOF && s.block != nil {
		<-s.block
	}
	return n, err
}

func TestProbe(t *testing.T) {
	tty := &scriptedTTY{reply: "\x1b_Gi=4294967295;OK\x1b\\\x1bP>|WezTerm 2024\x1b\\\x1b[6;18;9t\x1b[?62;4;22c"}
	r, err := Probe(tty, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Kitty || !r.Sixel || r.Version != "WezTerm 2024" || r.Cell != (CellSize{W: 9, H: 18}) || len(r.DA1) != 3 {
		t.Fatalf("probe = %+v", r)
	}
	if r.Protocol() != ProtocolKitty {
		t.Fatalf("protocol %v", r.Protocol())
	}
	if !strings.HasSuffix(tty.written.String(), "\x1b[c") {
		t.Fatal("DA1 must be the final query")
	}
	// No DA1 answer: times out with partial results.
	blk := make(chan struct{})
	defer close(blk)
	tty2 := &scriptedTTY{reply: "\x1b[?62;", block: blk}
	start := time.Now()
	_, err = Probe(tty2, 50*time.Millisecond)
	if err != ErrProbeTimeout || time.Since(start) > time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
	plain, _ := parseProbe([]byte("\x1b[?1;2c"))
	if plain.Protocol() != ProtocolAuto || plain.Sixel {
		t.Fatalf("plain terminal: %+v", plain)
	}
}

func TestOverlayDrawing(t *testing.T) {
	img := solid(200, 100, color.RGBA{0, 0, 0, 255})
	ov := Overlay{Cursor: &image.Point{100, 50}, Highlight: &image.Rectangle{Min: image.Pt(20, 20), Max: image.Pt(60, 40)}}
	drawOverlay(img, ov, nil, 1, 1)
	if c := img.RGBAAt(20, 20); c != highlightColor {
		t.Fatalf("highlight corner = %v", c)
	}
	if c := img.RGBAAt(40, 30); c.R == 0 || c == highlightColor {
		t.Fatalf("highlight fill not blended: %v", c)
	}
	if c := img.RGBAAt(101, 54); c != cursorFill && c != cursorEdge {
		t.Fatalf("cursor body missing: %v", c)
	}
	if c := img.RGBAAt(98, 48); c != (color.RGBA{0, 0, 0, 255}) {
		t.Fatalf("cursor drawn above tip: %v", c)
	}
	// Scaling: frame-space coordinates map into the scaled image.
	small := solid(20, 10, color.RGBA{0, 0, 0, 255})
	drawOverlay(small, ov, nil, 0.1, 0.1)
	if c := small.RGBAAt(10, 5); c != cursorFill {
		t.Fatalf("scaled cursor hotspot = %v", c)
	}
}

func TestCursorAnimator(t *testing.T) {
	var a cursorAnimator
	t0 := time.Unix(0, 0)
	a.target(&image.Point{0, 0}, t0)
	if a.animating(t0) {
		t.Fatal("first target must snap")
	}
	a.target(&image.Point{100, 0}, t0)
	mid := a.at(t0.Add(cursorAnimDuration / 2))
	if mid.X <= 50 || mid.X >= 100 {
		t.Fatalf("ease-out midpoint = %v", mid)
	}
	if !a.animating(t0.Add(cursorAnimDuration/2)) || a.animating(t0.Add(cursorAnimDuration)) {
		t.Fatal("animating window wrong")
	}
	if a.at(t0.Add(time.Second)) != (image.Point{100, 0}) {
		t.Fatal("animation must end at target")
	}
}

func TestRendererSkipsUnchanged(t *testing.T) {
	r := NewRenderer(1)
	f := Frame{Data: encodePNG(t, gradient(64, 40)), Seq: 1}
	p := RenderParams{Protocol: ProtocolHalfBlock, TrueColor: true, Cols: 32, Rows: 10, Cell: DefaultCellSize}
	out, changed, err := r.Render(f, p)
	if err != nil || !changed || len(out.Lines) != out.Layout.Rows || out.FrameW != 64 {
		t.Fatalf("first render: %v %v %+v", err, changed, out.Layout)
	}
	if _, changed, _ = r.Render(f, p); changed {
		t.Fatal("identical frame and params must be skipped")
	}
	p.Overlay.Label = "click"
	if _, changed, _ = r.Render(f, p); changed {
		t.Fatal("text-only overlay change must not re-encode")
	}
	p.Overlay.Cursor = &image.Point{10, 10}
	if _, changed, _ = r.Render(f, p); !changed {
		t.Fatal("cursor change must re-render")
	}
	r.Invalidate()
	if _, changed, _ = r.Render(f, p); !changed {
		t.Fatal("Invalidate must force a render")
	}
	for _, proto := range []Protocol{ProtocolKitty, ProtocolSixel, ProtocolITerm2} {
		p.Protocol = proto
		out, changed, err := r.Render(f, p)
		if err != nil || !changed || len(out.Image) == 0 || out.Lines != nil {
			t.Fatalf("%v: %v %v", proto, err, changed)
		}
	}
	if _, _, err := r.Render(Frame{Data: []byte("garbage")}, p); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestANSIHelpers(t *testing.T) {
	s := "\x1b[31mred\x1b[0m 世界\x1b]8;;http://x\x1b\\link\x1b]8;;\x1b\\"
	if w := StringWidth(s); w != 3+1+4+4 {
		t.Fatalf("width = %d", w)
	}
	tr := TruncateANSI(s, 5)
	if StringWidth(tr) != 4 || !strings.HasSuffix(tr, "\x1b[0m") { // wide rune does not fit
		t.Fatalf("truncate = %q (%d)", tr, StringWidth(tr))
	}
	if PadANSI("ab", 4) != "ab  " || StringWidth(PadANSI(s, 6)) != 6 {
		t.Fatal("pad")
	}
	out := SideBySide("\x1b[1mleft\x1b[0m\nl2\nl3", 6, "R1\nR2")
	lines := strings.Split(out, "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %q", lines)
	}
	if lines[0] != "\x1b[1mleft\x1b[0m  \x1b[0mR1" || lines[1] != "l2    R2" || lines[2] != "l3    " {
		t.Fatalf("side by side = %q", lines)
	}
	out = SideBySide("a", 3, "x\ny")
	if out != "a  x\n   y" {
		t.Fatalf("short left: %q", out)
	}
}
