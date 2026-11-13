package llm

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	_ "image/gif"  // register the GIF decoder for DecodeConfig
	_ "image/jpeg" // register the JPEG decoder for DecodeConfig
	_ "image/png"  // register the PNG decoder for DecodeConfig
	"io"
	"os"
	"strings"
)

// Image media types every supported provider accepts.
const (
	MediaPNG  = "image/png"
	MediaJPEG = "image/jpeg"
	MediaGIF  = "image/gif"
	MediaWebP = "image/webp"
)

// MaxImageBytes is the largest image file accepted. The Claude API caps an
// image at 5 MiB of base64 text, which is 3.75 MiB of raw bytes; the same
// cap keeps OpenAI-compatible requests well inside their body limits.
const MaxImageBytes = 5 * 1024 * 1024 / 4 * 3

// MaxImageDimension is the longest edge, in pixels, the providers accept.
const MaxImageDimension = 8000

// maxEncodedImage is MaxImageBytes as base64 text.
var maxEncodedImage = base64.StdEncoding.EncodedLen(MaxImageBytes)

// Image is a picture in a message. Exactly one of Data or Path is the
// source: Data is standard base64 with MediaType set, and Path is a file
// that ResolveImages (and every provider) loads just before sending, so
// stored conversations need not carry the bytes.
type Image struct {
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	Path      string `json:"path,omitempty"`
}

// ImageTooLargeError reports an image over MaxImageBytes.
type ImageTooLargeError struct {
	Name string
	Size int64
}

func (e *ImageTooLargeError) Error() string {
	return fmt.Sprintf("image %s is %s; the limit is %s (resize or compress it first)", e.Name, humanBytes(e.Size), humanBytes(MaxImageBytes))
}

// ErrUnsupportedImage is returned for files that are not PNG, JPEG, GIF or WebP.
var ErrUnsupportedImage = errors.New("unsupported image format: use PNG, JPEG, GIF or WebP")

// SupportedImageType reports whether a provider can take the media type.
func SupportedImageType(mediaType string) bool {
	switch mediaType {
	case MediaPNG, MediaJPEG, MediaGIF, MediaWebP:
		return true
	}
	return false
}

// DetectImageType identifies an image from its leading bytes. The file
// extension is ignored, because a misnamed file would be rejected by the
// API as not matching its declared type.
func DetectImageType(b []byte) (string, error) {
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return MediaPNG, nil
	case bytes.HasPrefix(b, []byte{0xFF, 0xD8, 0xFF}):
		return MediaJPEG, nil
	case bytes.HasPrefix(b, []byte("GIF87a")), bytes.HasPrefix(b, []byte("GIF89a")):
		return MediaGIF, nil
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return MediaWebP, nil
	}
	return "", ErrUnsupportedImage
}

// LoadImage reads an image file, checks its format, size and dimensions,
// and returns it base64-encoded.
func LoadImage(path string) (Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return Image{}, fmt.Errorf("open image: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Image{}, fmt.Errorf("open image: %w", err)
	}
	if st.IsDir() {
		return Image{}, fmt.Errorf("image %s is a directory", path)
	}
	if st.Size() > MaxImageBytes {
		return Image{}, &ImageTooLargeError{Name: path, Size: st.Size()}
	}
	// Read one byte past the cap in case the file grew after Stat.
	b, err := io.ReadAll(io.LimitReader(f, MaxImageBytes+1))
	if err != nil {
		return Image{}, fmt.Errorf("read image %s: %w", path, err)
	}
	img, err := imageFromBytes(path, b)
	if err != nil {
		return Image{}, err
	}
	return img, nil
}

// ImageFromBytes validates raw image bytes and returns them base64-encoded.
func ImageFromBytes(b []byte) (Image, error) { return imageFromBytes("data", b) }

func imageFromBytes(name string, b []byte) (Image, error) {
	if int64(len(b)) > MaxImageBytes {
		return Image{}, &ImageTooLargeError{Name: name, Size: int64(len(b))}
	}
	if len(b) == 0 {
		return Image{}, fmt.Errorf("image %s is empty", name)
	}
	mt, err := DetectImageType(b)
	if err != nil {
		return Image{}, fmt.Errorf("image %s: %w", name, err)
	}
	w, h, err := imageSize(mt, b)
	if err != nil {
		return Image{}, fmt.Errorf("image %s: cannot read dimensions: %w", name, err)
	}
	if w <= 0 || h <= 0 {
		return Image{}, fmt.Errorf("image %s has no pixels", name)
	}
	if w > MaxImageDimension || h > MaxImageDimension {
		return Image{}, fmt.Errorf("image %s is %dx%d pixels; the limit is %d on each side", name, w, h, MaxImageDimension)
	}
	return Image{MediaType: mt, Data: base64.StdEncoding.EncodeToString(b)}, nil
}

// ImageBlock loads an image file into a user content block.
func ImageBlock(path string) (Block, error) {
	img, err := LoadImage(path)
	if err != nil {
		return Block{}, err
	}
	return Block{Type: BlockImage, Image: &img}, nil
}

// Resolve returns the image ready to send: a Path image is loaded, and a
// Data image is checked for a supported type, a size within limits and
// bytes that match its declared type.
func (img Image) Resolve() (Image, error) {
	if img.Data == "" {
		if strings.TrimSpace(img.Path) == "" {
			return Image{}, errors.New("image has neither data nor a path")
		}
		return LoadImage(img.Path)
	}
	if !SupportedImageType(img.MediaType) {
		if img.MediaType == "" {
			return Image{}, errors.New("image data has no media type")
		}
		return Image{}, fmt.Errorf("image media type %q: %w", img.MediaType, ErrUnsupportedImage)
	}
	if len(img.Data) > maxEncodedImage {
		return Image{}, &ImageTooLargeError{Name: "data", Size: int64(base64.StdEncoding.DecodedLen(len(img.Data)))}
	}
	// Decoding a short prefix is enough to check the format cheaply on
	// every request; LoadImage and ImageFromBytes check the whole file.
	prefix := img.Data
	if len(prefix) > 64 {
		prefix = prefix[:64]
	}
	head, err := base64.StdEncoding.DecodeString(prefix)
	if err != nil {
		return Image{}, fmt.Errorf("image data is not valid base64: %w", err)
	}
	mt, err := DetectImageType(head)
	if err != nil {
		return Image{}, err
	}
	if mt != img.MediaType {
		return Image{}, fmt.Errorf("image data is %s but is labelled %s", mt, img.MediaType)
	}
	return Image{MediaType: img.MediaType, Data: img.Data}, nil
}

// DataURL renders a resolved image as a data: URL.
func (img Image) DataURL() string {
	return "data:" + img.MediaType + ";base64," + img.Data
}

// ResolveImages returns a copy of history in which every image is loaded and
// validated. The input is not modified.
func ResolveImages(history []Message) ([]Message, error) {
	out := make([]Message, len(history))
	for i, m := range history {
		out[i] = m
		copied := false
		for j, b := range m.Blocks {
			var nb Block
			changed := false
			switch {
			case b.Type == BlockImage:
				if b.Image == nil {
					return nil, fmt.Errorf("message %d: image block without an image", i)
				}
				img, err := b.Image.Resolve()
				if err != nil {
					return nil, fmt.Errorf("message %d: %w", i, err)
				}
				nb, changed = b, true
				nb.Image = &img
			case len(b.Images) > 0:
				imgs := make([]Image, len(b.Images))
				for k, im := range b.Images {
					r, err := im.Resolve()
					if err != nil {
						return nil, fmt.Errorf("message %d: tool result image: %w", i, err)
					}
					imgs[k] = r
				}
				nb, changed = b, true
				nb.Images = imgs
			}
			if !changed {
				continue
			}
			if !copied {
				out[i].Blocks = append([]Block(nil), m.Blocks...)
				copied = true
			}
			out[i].Blocks[j] = nb
		}
	}
	return out, nil
}

// imageSize reads an image's pixel dimensions without decoding it.
func imageSize(mediaType string, b []byte) (int, int, error) {
	if mediaType == MediaWebP {
		return webpSize(b)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return 0, 0, err
	}
	return cfg.Width, cfg.Height, nil
}

// webpSize parses the dimensions of the three WebP bitstream kinds: lossy
// (VP8), lossless (VP8L) and extended (VP8X).
func webpSize(b []byte) (int, int, error) {
	if len(b) < 30 {
		return 0, 0, errors.New("truncated WebP header")
	}
	switch string(b[12:16]) {
	case "VP8 ":
		if b[23] != 0x9d || b[24] != 0x01 || b[25] != 0x2a {
			return 0, 0, errors.New("bad VP8 start code")
		}
		w := int(binary.LittleEndian.Uint16(b[26:28]) & 0x3fff)
		h := int(binary.LittleEndian.Uint16(b[28:30]) & 0x3fff)
		return w, h, nil
	case "VP8L":
		if b[20] != 0x2f {
			return 0, 0, errors.New("bad VP8L signature")
		}
		bits := binary.LittleEndian.Uint32(b[21:25])
		return int(bits&0x3fff) + 1, int((bits>>14)&0x3fff) + 1, nil
	case "VP8X":
		w := int(uint32(b[24])|uint32(b[25])<<8|uint32(b[26])<<16) + 1
		h := int(uint32(b[27])|uint32(b[28])<<8|uint32(b[29])<<16) + 1
		return w, h, nil
	}
	return 0, 0, errors.New("unknown WebP chunk")
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}
