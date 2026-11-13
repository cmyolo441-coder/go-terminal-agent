package openai

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// A server that streams one endless line or event must not make the client
// buffer it all.
func TestReadSSEBoundsEventSize(t *testing.T) {
	for name, r := range map[string]io.Reader{
		"line":  io.MultiReader(strings.NewReader("data: "), &repeatReader{b: 'x', n: maxEventBytes + 10}),
		"event": &linesReader{line: []byte("data: " + strings.Repeat("y", 1<<16) + "\n"), n: maxEventBytes/(1<<16) + 2},
	} {
		err := readSSE(r, func() {}, func([]byte) (bool, error) { return false, nil })
		if !errors.Is(err, errEventTooLarge) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// Valid JSON that is not an object is not usable tool input.
func TestToolInputMustBeObject(t *testing.T) {
	for _, in := range []string{`[1,2]`, `"s"`, `null`, `3`, `{"a":`} {
		if got := string(toolInput(in)); got != "{}" {
			t.Errorf("toolInput(%q) = %q", in, got)
		}
	}
	if got := string(toolInput(` {"a": [1, 2]} `)); got != `{"a":[1,2]}` {
		t.Errorf("object = %q", got)
	}
}

// A non-data field on the last line, cut by a read error, must not hide
// that error.
func TestReadSSEReportsReadErrorAfterOtherField(t *testing.T) {
	boom := errors.New("boom")
	r := io.MultiReader(strings.NewReader("id: 1"), errReader{boom})
	err := readSSE(r, func() {}, func([]byte) (bool, error) { return false, nil })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

type repeatReader struct {
	b byte
	n int
}

func (r *repeatReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, io.EOF
	}
	k := min(len(p), r.n)
	copy(p, bytes.Repeat([]byte{r.b}, k))
	r.n -= k
	return k, nil
}

type linesReader struct {
	line []byte
	n    int
	off  int
}

func (r *linesReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, io.EOF
	}
	k := copy(p, r.line[r.off:])
	r.off += k
	if r.off == len(r.line) {
		r.off = 0
		r.n--
	}
	return k, nil
}
