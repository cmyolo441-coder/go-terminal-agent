package mcp

import (
	"bufio"
	"errors"
	"strings"
	"sync"
)

// ringBuffer keeps the last cap bytes written to it. It captures a server's
// stderr so failures can be explained without unbounded memory growth.
type ringBuffer struct {
	mu        sync.Mutex
	buf       []byte
	cap       int
	truncated bool
}

func newRingBuffer(capacity int) *ringBuffer {
	return &ringBuffer{cap: capacity, buf: make([]byte, 0, capacity)}
}

// Write implements io.Writer and never fails.
func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(p)
	if n >= r.cap {
		r.buf = append(r.buf[:0], p[n-r.cap:]...)
		r.truncated = true
		return n, nil
	}
	if over := len(r.buf) + n - r.cap; over > 0 {
		r.buf = append(r.buf[:0], r.buf[over:]...)
		r.truncated = true
	}
	r.buf = append(r.buf, p...)
	return n, nil
}

// String returns the retained text. When older output was dropped, the
// partial first line is removed and the text starts with an ellipsis.
func (r *ringBuffer) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := string(r.buf)
	if r.truncated {
		if i := strings.IndexByte(s, '\n'); i >= 0 && i < len(s)-1 {
			s = s[i+1:]
		}
		s = "…" + s
	}
	return s
}

// Tail returns at most the last n lines, trimmed.
func (r *ringBuffer) Tail(n int) string {
	s := strings.TrimSpace(r.String())
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = append([]string{"…"}, lines[len(lines)-n:]...)
	}
	return strings.Join(lines, "\n")
}

// errLineTooLong reports a line that exceeded the reader's limit; the rest of
// that line has been discarded.
var errLineTooLong = errors.New("line too long")

// readLine reads one '\n'-terminated line of at most max bytes, without the
// terminator and any trailing '\r'. Longer lines are consumed and discarded,
// returning errLineTooLong so the caller can skip them and keep reading.
func readLine(r *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	tooLong := false
	for {
		chunk, err := r.ReadSlice('\n')
		if !tooLong {
			if len(line)+len(chunk) > max+2 {
				tooLong = true
				line = nil
			} else {
				line = append(line, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if tooLong {
			if err != nil {
				return nil, err
			}
			return nil, errLineTooLong
		}
		if err != nil && len(line) == 0 {
			return nil, err
		}
		line = trimEOL(line)
		if len(line) > max {
			return nil, errLineTooLong
		}
		return line, nil
	}
}

func trimEOL(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
	}
	if n := len(b); n > 0 && b[n-1] == '\r' {
		b = b[:n-1]
	}
	return b
}
