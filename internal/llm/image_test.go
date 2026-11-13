package llm

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func jpegBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 3, 2)), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gifBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := gif.Encode(&buf, image.NewPaletted(image.Rect(0, 0, 4, 5), color.Palette{color.Black, color.White}), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// webpHeader builds the first 30 bytes of a WebP file of the given kind.
func webpHeader(kind string, w, h int) []byte {
	b := make([]byte, 30)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], 22)
	copy(b[8:], "WEBP")
	copy(b[12:], kind)
	switch kind {
	case "VP8 ":
		b[23], b[24], b[25] = 0x9d, 0x01, 0x2a
		binary.LittleEndian.PutUint16(b[26:], uint16(w))
		binary.LittleEndian.PutUint16(b[28:], uint16(h))
	case "VP8L":
		b[20] = 0x2f
		binary.LittleEndian.PutUint32(b[21:], uint32(w-1)|uint32(h-1)<<14)
	case "VP8X":
		w, h = w-1, h-1
		b[24], b[25], b[26] = byte(w), byte(w>>8), byte(w>>16)
		b[27], b[28], b[29] = byte(h), byte(h>>8), byte(h>>16)
	}
	return b
}

func TestImageFormats(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"png", pngBytes(t, 2, 2), MediaPNG},
		{"jpeg", jpegBytes(t), MediaJPEG},
		{"gif", gifBytes(t), MediaGIF},
		{"webp-lossy", webpHeader("VP8 ", 640, 480), MediaWebP},
		{"webp-lossless", webpHeader("VP8L", 16, 9), MediaWebP},
		{"webp-extended", webpHeader("VP8X", 7000, 20), MediaWebP},
	}
	for _, c := range cases {
		img, err := ImageFromBytes(c.data)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if img.MediaType != c.want {
			t.Fatalf("%s: media type %q", c.name, img.MediaType)
		}
		raw, _ := base64.StdEncoding.DecodeString(img.Data)
		if !bytes.Equal(raw, c.data) {
			t.Fatalf("%s: data does not round-trip", c.name)
		}
	}
}

func TestImageRejections(t *testing.T) {
	if _, err := ImageFromBytes([]byte("%PDF-1.7 not an image")); !errors.Is(err, ErrUnsupportedImage) {
		t.Fatalf("pdf: %v", err)
	}
	if _, err := ImageFromBytes(nil); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty: %v", err)
	}
	if _, err := ImageFromBytes(webpHeader("VP8X", 9000, 10)); err == nil || !strings.Contains(err.Error(), "9000x10") {
		t.Fatalf("oversize dimensions: %v", err)
	}
	if _, err := ImageFromBytes(append([]byte("\x89PNG\r\n\x1a\n"), 1, 2, 3)); err == nil || !strings.Contains(err.Error(), "dimensions") {
		t.Fatalf("corrupt png: %v", err)
	}
	big := make([]byte, MaxImageBytes+1)
	copy(big, pngBytes(t, 1, 1))
	var tooLarge *ImageTooLargeError
	if _, err := ImageFromBytes(big); !errors.As(err, &tooLarge) || !strings.Contains(err.Error(), "3.8 MiB") {
		t.Fatalf("oversize bytes: %v", err)
	}
}

func TestLoadImage(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "shot.jpg") // a PNG with the wrong extension
	if err := os.WriteFile(good, pngBytes(t, 3, 3), 0o600); err != nil {
		t.Fatal(err)
	}
	img, err := LoadImage(good)
	if err != nil || img.MediaType != MediaPNG {
		t.Fatalf("load: %+v %v", img, err)
	}
	b, err := ImageBlock(good)
	if err != nil || b.Type != BlockImage || b.Image == nil || b.Image.Data != img.Data {
		t.Fatalf("block: %+v %v", b, err)
	}

	huge := filepath.Join(dir, "huge.png")
	f, _ := os.Create(huge)
	f.Write(pngBytes(t, 1, 1))
	f.Truncate(MaxImageBytes + 10)
	f.Close()
	var tooLarge *ImageTooLargeError
	if _, err := LoadImage(huge); !errors.As(err, &tooLarge) || tooLarge.Name != huge {
		t.Fatalf("huge: %v", err)
	}
	if _, err := LoadImage(dir); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("dir: %v", err)
	}
	if _, err := LoadImage(filepath.Join(dir, "missing.png")); err == nil {
		t.Fatal("missing file must fail")
	}
}

func TestResolve(t *testing.T) {
	data := base64.StdEncoding.EncodeToString(pngBytes(t, 2, 2))
	if _, err := (Image{MediaType: MediaPNG, Data: data}).Resolve(); err != nil {
		t.Fatal(err)
	}
	if _, err := (Image{MediaType: MediaJPEG, Data: data}).Resolve(); err == nil || !strings.Contains(err.Error(), "labelled image/jpeg") {
		t.Fatalf("mislabelled: %v", err)
	}
	if _, err := (Image{MediaType: "image/bmp", Data: data}).Resolve(); !errors.Is(err, ErrUnsupportedImage) {
		t.Fatalf("bmp: %v", err)
	}
	if _, err := (Image{Data: data}).Resolve(); err == nil {
		t.Fatal("missing media type must fail")
	}
	if _, err := (Image{MediaType: MediaPNG, Data: "!!!not base64!!!"}).Resolve(); err == nil {
		t.Fatal("bad base64 must fail")
	}
	if _, err := (Image{MediaType: MediaPNG, Data: strings.Repeat("A", maxEncodedImage+4)}).Resolve(); err == nil {
		t.Fatal("oversize data must fail")
	}
	if _, err := (Image{}).Resolve(); err == nil {
		t.Fatal("empty image must fail")
	}
	if got := (Image{MediaType: MediaPNG, Data: "QUJD"}).DataURL(); got != "data:image/png;base64,QUJD" {
		t.Fatal(got)
	}
}

func TestResolveImagesCopies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.gif")
	if err := os.WriteFile(path, gifBytes(t), 0o600); err != nil {
		t.Fatal(err)
	}
	hist := []Message{
		UserText("hello"),
		{Role: RoleUser, Blocks: []Block{
			{Type: BlockText, Text: "look"},
			{Type: BlockImage, Image: &Image{Path: path}},
			{Type: BlockToolResult, ToolUseID: "t1", Text: "shot", Images: []Image{{Path: path}}},
		}},
	}
	out, err := ResolveImages(hist)
	if err != nil {
		t.Fatal(err)
	}
	if hist[1].Blocks[1].Image.Data != "" || hist[1].Blocks[2].Images[0].Data != "" {
		t.Fatal("input history was modified")
	}
	if out[1].Blocks[1].Image.MediaType != MediaGIF || out[1].Blocks[2].Images[0].Data == "" || out[1].Blocks[0].Text != "look" {
		t.Fatalf("resolved: %+v", out[1])
	}
	if &out[0].Blocks[0] != &hist[0].Blocks[0] {
		t.Fatal("messages without images should share their blocks")
	}
	if _, err := ResolveImages([]Message{{Role: RoleUser, Blocks: []Block{{Type: BlockImage}}}}); err == nil {
		t.Fatal("image block without image must fail")
	}
	if _, err := ResolveImages([]Message{{Role: RoleUser, Blocks: []Block{{Type: BlockImage, Image: &Image{Path: path + ".missing"}}}}}); err == nil {
		t.Fatal("missing path must fail")
	}
}
