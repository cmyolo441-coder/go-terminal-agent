package dlp

import (
	"bytes"
	"errors"
	"io"
	"math"
)

// ErrClosed is returned by writes after Close.
var ErrClosed = errors.New("dlp: stream is closed")

// StreamOptions configures a streaming Writer or Reader.
type StreamOptions struct {
	// Vault receives tokenized values; nil makes tokenize fall back to
	// redaction.
	Vault *Vault
	// ChunkSize is how many new bytes are scanned per step (default 32 KiB).
	// Memory use is bounded by roughly ChunkSize plus twice the carry-over
	// window (about 1 KiB plus four context windows).
	ChunkSize int
	// OnFinding, if set, is called for every finding with offsets in stream
	// coordinates.
	OnFinding func(Finding)
}

// streamCore holds the sliding buffer shared by Writer and Reader.
//
// buf[:lb] is look-behind: bytes already emitted that remain as context so
// that keywords and token boundaries before the cut are still seen.
// buf[lb:] is pending. Each step scans the whole buffer, emits pending
// bytes up to a cut that lies at least carry bytes before the end, never
// inside a finding and preferably just after whitespace, and keeps the
// tail. A value is at most MaxMatchLen bytes long and its context lies
// within the window, so anything that crosses the cut is complete in a
// later step.
type streamCore struct {
	e     *Engine
	opts  StreamOptions
	chunk int
	carry int
	look  int

	buf  []byte
	lb   int
	base int64 // stream offset of buf[0]

	report Report
	weight float64
	err    error
}

func newStreamCore(e *Engine, opts StreamOptions) *streamCore {
	c := &streamCore{e: e, opts: opts, chunk: opts.ChunkSize}
	if c.chunk <= 0 {
		c.chunk = 32 << 10
	}
	c.carry = MaxMatchLen + 2*e.window + 128
	c.look = MaxMatchLen + 2*e.window
	c.report = Report{Policy: e.name, Documents: 1, Label: Public}
	return c
}

func (c *streamCore) pending() int { return len(c.buf) - c.lb }

func (c *streamCore) ready() bool { return c.pending() >= c.chunk+c.carry }

func isSpaceByte(b byte) bool { return b == ' ' || b == '\n' || b == '\t' || b == '\r' }

// step processes pending bytes and appends the output to out. With final
// set everything pending is processed.
func (c *streamCore) step(final bool, out []byte) ([]byte, error) {
	if c.err != nil {
		return out, c.err
	}
	text := string(c.buf)
	cands := c.e.detect(text, c.lb)
	cut := len(text)
	if !final {
		cut = len(text) - c.carry
		if cut <= c.lb {
			return out, nil
		}
		for k, lo := cut, max(c.lb, cut-128); k > lo; k-- {
			if isSpaceByte(text[k-1]) {
				cut = k
				break
			}
		}
		for moved := true; moved; {
			moved = false
			for _, cd := range cands {
				if cd.start < cut && cd.end > cut {
					cut, moved = cd.start, true
				}
			}
		}
		if cut <= c.lb {
			return out, nil
		}
		k := 0
		for _, cd := range cands {
			if cd.end <= cut {
				cands[k] = cd
				k++
			}
		}
		cands = cands[:k]
	}
	seg, findings, blocks := c.e.apply(text, c.lb, cut, cands, c.opts.Vault, int(c.base))
	r := buildReport(c.e.name, int64(cut-c.lb), findings)
	r.Documents = 0
	for _, f := range findings {
		c.weight += labelWeight(f.Label) * f.Confidence
	}
	c.report.Merge(r)
	c.report.Score = math.Round(1000*(1-math.Exp(-c.weight/10))) / 10
	if c.opts.OnFinding != nil {
		for _, f := range findings {
			c.opts.OnFinding(f)
		}
	}
	tmp := &Result{Report: c.report}
	if err := c.e.decide(tmp, blocks); err != nil {
		c.report.Blocked = 1
		c.err = err
		return out, err
	}
	out = append(out, seg...)
	keep := max(0, cut-c.look)
	n := copy(c.buf, c.buf[keep:])
	c.buf = c.buf[:n]
	c.lb = cut - keep
	c.base += int64(keep)
	return out, nil
}

// Writer applies a policy to everything written to it and forwards the
// result to the destination. Call Close to flush the tail. A Writer is not
// safe for concurrent use.
//
// When the policy blocks, Write returns a *BlockedError and nothing from
// the offending chunk reaches the destination; earlier chunks have already
// been forwarded.
type Writer struct {
	c      *streamCore
	dst    io.Writer
	out    []byte
	closed bool
}

// NewWriter returns a Writer that forwards transformed text to dst.
func (e *Engine) NewWriter(dst io.Writer, opts StreamOptions) *Writer {
	return &Writer{c: newStreamCore(e, opts), dst: dst}
}

// Write buffers p and processes complete chunks. It reports len(p) bytes
// consumed unless an error occurs.
func (w *Writer) Write(p []byte) (int, error) {
	if w.closed {
		return 0, ErrClosed
	}
	if w.c.err != nil {
		return 0, w.c.err
	}
	written := 0
	for len(p) > 0 {
		room := max(1, w.c.chunk+w.c.carry-w.c.pending())
		take := min(len(p), room)
		w.c.buf = append(w.c.buf, p[:take]...)
		p = p[take:]
		written += take
		if w.c.ready() {
			if err := w.flushStep(false); err != nil {
				return written, err
			}
		}
	}
	return written, nil
}

func (w *Writer) flushStep(final bool) error {
	out, err := w.c.step(final, w.out[:0])
	w.out = out[:0]
	if len(out) > 0 {
		if _, werr := w.dst.Write(out); werr != nil {
			w.c.err = werr
			return werr
		}
	}
	return err
}

// Close processes everything still buffered. It does not close the
// destination.
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if w.c.err != nil {
		return w.c.err
	}
	return w.flushStep(true)
}

// Report returns the stream's cumulative report.
func (w *Writer) Report() Report { return w.c.report.clone() }

// Reader applies a policy to everything read from a source. A Reader is not
// safe for concurrent use.
type Reader struct {
	c    *streamCore
	src  io.Reader
	rbuf []byte
	out  []byte
	pos  int
	eof  bool
	done bool
}

// NewReader returns a Reader that yields src's content with the policy
// applied.
func (e *Engine) NewReader(src io.Reader, opts StreamOptions) *Reader {
	c := newStreamCore(e, opts)
	return &Reader{c: c, src: src, rbuf: make([]byte, min(c.chunk, 64<<10))}
}

// Read implements io.Reader. A blocked stream returns a *BlockedError.
func (r *Reader) Read(p []byte) (int, error) {
	for r.pos == len(r.out) {
		if r.c.err != nil {
			return 0, r.c.err
		}
		if r.done {
			return 0, io.EOF
		}
		r.out, r.pos = r.out[:0], 0
		if !r.eof {
			want := min(len(r.rbuf), max(1, r.c.chunk+r.c.carry-r.c.pending()))
			n, err := r.src.Read(r.rbuf[:want])
			r.c.buf = append(r.c.buf, r.rbuf[:n]...)
			if err == io.EOF {
				r.eof = true
			} else if err != nil {
				return 0, err
			}
		}
		var err error
		switch {
		case r.eof:
			r.out, err = r.c.step(true, r.out)
			r.done = true
		case r.c.ready():
			r.out, err = r.c.step(false, r.out)
		}
		if err != nil {
			return 0, err
		}
	}
	n := copy(p, r.out[r.pos:])
	r.pos += n
	return n, nil
}

// Report returns the stream's cumulative report.
func (r *Reader) Report() Report { return r.c.report.clone() }

// DetokenizeWriter restores Vault tokens in a stream (typically model
// output) before forwarding it. A token split across writes is held back
// until it is complete, so memory stays bounded by the longest token.
type DetokenizeWriter struct {
	dst      io.Writer
	v        *Vault
	pend     []byte
	restored int
	closed   bool
}

// NewDetokenizeWriter returns a DetokenizeWriter forwarding to dst.
func NewDetokenizeWriter(dst io.Writer, v *Vault) *DetokenizeWriter {
	return &DetokenizeWriter{dst: dst, v: v}
}

// holdBack returns how many trailing bytes of b could be the start of a
// token and must wait for more input.
func holdBack(b []byte) int {
	i := bytes.LastIndex(b, []byte("[["))
	if i >= 0 && len(b)-i < maxTokenLen && !bytes.Contains(b[i:], []byte(tokenSuffix)) {
		return len(b) - i
	}
	if len(b) > 0 && b[len(b)-1] == '[' {
		return 1
	}
	return 0
}

// Write implements io.Writer.
func (d *DetokenizeWriter) Write(p []byte) (int, error) {
	if d.closed {
		return 0, ErrClosed
	}
	d.pend = append(d.pend, p...)
	hold := holdBack(d.pend)
	if err := d.emit(len(d.pend) - hold); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (d *DetokenizeWriter) emit(n int) error {
	if n <= 0 {
		return nil
	}
	s, k := d.v.Detokenize(string(d.pend[:n]))
	d.restored += k
	m := copy(d.pend, d.pend[n:])
	d.pend = d.pend[:m]
	_, err := io.WriteString(d.dst, s)
	return err
}

// Close flushes held bytes. It does not close the destination.
func (d *DetokenizeWriter) Close() error {
	if d.closed {
		return nil
	}
	d.closed = true
	return d.emit(len(d.pend))
}

// Restored returns how many tokens have been replaced so far.
func (d *DetokenizeWriter) Restored() int { return d.restored }
