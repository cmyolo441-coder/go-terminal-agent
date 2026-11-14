package browser

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"
)

// transport carries whole CDP messages (JSON documents) to and from the
// browser. ReadMessage is called from a single goroutine; WriteMessage and
// Close may be called concurrently with it and with each other.
type transport interface {
	ReadMessage() ([]byte, error)
	WriteMessage([]byte) error
	Close() error
}

// wsTransport sends each CDP message as one WebSocket text message, the
// framing used by --remote-debugging-port.
type wsTransport struct{ c *wsConn }

func (t wsTransport) ReadMessage() ([]byte, error) {
	for {
		op, data, err := t.c.ReadMessage()
		if err != nil {
			return nil, err
		}
		if op == wsOpText || op == wsOpBinary {
			return data, nil
		}
	}
}

func (t wsTransport) WriteMessage(b []byte) error { return t.c.WriteMessage(wsOpText, b) }
func (t wsTransport) Close() error                { return t.c.Close() }

// pipeTransport speaks the --remote-debugging-pipe framing: the browser
// reads NUL-terminated JSON messages on its fd 3 and writes them on fd 4.
type pipeTransport struct {
	r      io.ReadCloser
	br     *bufio.Reader
	w      io.WriteCloser
	maxMsg int

	wmu       sync.Mutex
	closeOnce sync.Once
}

func newPipeTransport(r io.ReadCloser, w io.WriteCloser) *pipeTransport {
	return &pipeTransport{r: r, br: bufio.NewReaderSize(r, 64<<10), w: w, maxMsg: defaultWSMaxMessage}
}

func (t *pipeTransport) ReadMessage() ([]byte, error) {
	var msg []byte
	for {
		chunk, err := t.br.ReadSlice(0)
		if len(msg)+len(chunk) > t.maxMsg {
			return nil, fmt.Errorf("pipe transport: message exceeds %d bytes", t.maxMsg)
		}
		switch {
		case err == nil:
			msg = append(msg, chunk[:len(chunk)-1]...)
			return msg, nil
		case errors.Is(err, bufio.ErrBufferFull):
			msg = append(msg, chunk...)
		default:
			if errors.Is(err, io.EOF) && len(msg)+len(chunk) > 0 {
				err = io.ErrUnexpectedEOF
			}
			return nil, fmt.Errorf("pipe transport: read: %w", err)
		}
	}
}

func (t *pipeTransport) WriteMessage(b []byte) error {
	if bytes.IndexByte(b, 0) >= 0 {
		return errors.New("pipe transport: message contains a NUL byte")
	}
	t.wmu.Lock()
	defer t.wmu.Unlock()
	buf := make([]byte, len(b)+1)
	copy(buf, b)
	if _, err := t.w.Write(buf); err != nil {
		return fmt.Errorf("pipe transport: write: %w", err)
	}
	return nil
}

func (t *pipeTransport) Close() error {
	var err error
	t.closeOnce.Do(func() {
		err = errors.Join(t.w.Close(), t.r.Close())
	})
	return err
}
