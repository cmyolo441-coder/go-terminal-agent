package browser

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // register decoders for DecodeConfig
	_ "image/png"
	"math"
	"strings"
	"sync"
	"time"
)

// Frame is one screencast image.
type Frame struct {
	// Data is the encoded image.
	Data []byte
	// Format is "jpeg" or "png".
	Format string
	// Width and Height are the image's pixel dimensions.
	Width, Height int
	// Seq numbers frames from 1 in the order the browser produced them;
	// gaps mean frames were dropped for a slow consumer.
	Seq uint64
	// Timestamp is when the browser captured the frame.
	Timestamp time.Time
	// ScrollX and ScrollY are the page's scroll offsets in CSS pixels.
	ScrollX, ScrollY float64
}

// ScreencastOptions configure StartScreencast. Zero values pick defaults.
type ScreencastOptions struct {
	// Format is "jpeg" (default) or "png".
	Format string
	// Quality is the JPEG quality, 1-100; 0 means 80.
	Quality int
	// MaxWidth and MaxHeight bound the frame size; 0 leaves it unbounded
	// (the viewport size).
	MaxWidth, MaxHeight int
	// EveryNthFrame sends one of every n rendered frames; 0 means 1.
	EveryNthFrame int
}

// screencastBuffer is how many undelivered frames are kept for a slow
// consumer before the oldest is dropped.
const screencastBuffer = 2

type screencast struct {
	stopCh     chan struct{}
	stopOnce   sync.Once
	workerDone chan struct{}
}

// frameQueue hands raw frame events from the connection's reader to the
// screencast worker without ever blocking the reader. The browser sends
// a bounded number of unacknowledged frames, so it stays small.
type frameQueue struct {
	mu     sync.Mutex
	items  []json.RawMessage
	signal chan struct{}
}

func (q *frameQueue) push(params json.RawMessage) {
	q.mu.Lock()
	q.items = append(q.items, params)
	q.mu.Unlock()
	select {
	case q.signal <- struct{}{}:
	default:
	}
}

func (q *frameQueue) take() []json.RawMessage {
	q.mu.Lock()
	defer q.mu.Unlock()
	items := q.items
	q.items = nil
	return items
}

// StartScreencast streams frames of the page as it renders. Every frame is
// acknowledged to the browser as soon as it arrives; when the consumer
// falls behind, the oldest undelivered frame is dropped so the browser
// connection never blocks. The stream ends, and the channel is closed,
// when the returned stop function is called (it is idempotent and waits
// for the stream to wind down), ctx ends, or the page closes. Only one
// screencast can run per page.
func (p *Page) StartScreencast(ctx context.Context, opts ScreencastOptions) (<-chan Frame, func(), error) {
	format := strings.ToLower(strings.TrimSpace(opts.Format))
	switch format {
	case "", "jpeg", "jpg":
		format = "jpeg"
	case "png":
	default:
		return nil, nil, fmt.Errorf("browser: unsupported screencast format %q", opts.Format)
	}
	params := map[string]any{"format": format}
	if format == "jpeg" {
		params["quality"] = clampQuality(opts.Quality)
	}
	if opts.MaxWidth > 0 {
		params["maxWidth"] = opts.MaxWidth
	}
	if opts.MaxHeight > 0 {
		params["maxHeight"] = opts.MaxHeight
	}
	params["everyNthFrame"] = max(opts.EveryNthFrame, 1)

	sc := &screencast{stopCh: make(chan struct{}), workerDone: make(chan struct{})}
	p.mu.Lock()
	switch {
	case p.closed:
		err := p.closeErr
		p.mu.Unlock()
		return nil, nil, err
	case p.screencast != nil:
		p.mu.Unlock()
		return nil, nil, errors.New("browser: a screencast is already running on this page")
	}
	p.screencast = sc
	p.mu.Unlock()

	q := &frameQueue{signal: make(chan struct{}, 1)}
	unsub := p.conn.subscribe(p.sessionID, "Page.screencastFrame", func(_ string, params json.RawMessage) { q.push(params) })
	if err := p.call(ctx, "Page.startScreencast", params, nil); err != nil {
		unsub()
		p.mu.Lock()
		p.screencast = nil
		p.mu.Unlock()
		return nil, nil, fmt.Errorf("browser: start screencast: %w", err)
	}

	out := make(chan Frame, screencastBuffer)
	go p.runScreencast(ctx, sc, q, unsub, format, out)
	stop := func() {
		sc.stopOnce.Do(func() { close(sc.stopCh) })
		<-sc.workerDone
	}
	return out, stop, nil
}

func (p *Page) runScreencast(ctx context.Context, sc *screencast, q *frameQueue, unsub func(), format string, out chan Frame) {
	defer close(sc.workerDone)
	defer func() {
		unsub()
		if p.err() == nil {
			stopCtx, cancel := context.WithTimeout(context.Background(), gracefulCloseTimeout)
			if err := p.call(stopCtx, "Page.stopScreencast", nil, nil); err != nil {
				p.b.logf("browser: stop screencast: %v", err)
			}
			cancel()
		}
		close(out)
		p.mu.Lock()
		if p.screencast == sc {
			p.screencast = nil
		}
		p.mu.Unlock()
	}()

	var seq uint64
	for {
		select {
		case <-q.signal:
		case <-sc.stopCh:
			return
		case <-ctx.Done():
			return
		case <-p.done:
			return
		case <-p.conn.Done():
			return
		}
		for _, raw := range q.take() {
			f, ackID, err := decodeScreencastFrame(raw, format)
			if ackID >= 0 {
				if aerr := p.conn.send(p.sessionID, "Page.screencastFrameAck", map[string]any{"sessionId": ackID}); aerr != nil {
					return
				}
			}
			if err != nil {
				p.b.logf("browser: screencast frame: %v", err)
				continue
			}
			seq++
			f.Seq = seq
			deliverDropOldest(out, f)
		}
	}
}

// deliverDropOldest sends f, discarding the oldest buffered frame when the
// channel is full. It never blocks; it must be the channel's only sender.
func deliverDropOldest(out chan Frame, f Frame) {
	for {
		select {
		case out <- f:
			return
		default:
		}
		select {
		case <-out:
		default:
		}
	}
}

// decodeScreencastFrame parses a Page.screencastFrame event. ackID is the
// frame's session id to acknowledge, or -1 when the event was unreadable.
func decodeScreencastFrame(raw json.RawMessage, format string) (Frame, int, error) {
	var ev struct {
		Data     string `json:"data"`
		Metadata struct {
			DeviceWidth   float64 `json:"deviceWidth"`
			DeviceHeight  float64 `json:"deviceHeight"`
			ScrollOffsetX float64 `json:"scrollOffsetX"`
			ScrollOffsetY float64 `json:"scrollOffsetY"`
			Timestamp     float64 `json:"timestamp"`
		} `json:"metadata"`
		SessionID int `json:"sessionId"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		return Frame{}, -1, fmt.Errorf("decode event: %w", err)
	}
	data, err := base64.StdEncoding.DecodeString(ev.Data)
	if err != nil {
		return Frame{}, ev.SessionID, fmt.Errorf("decode image data: %w", err)
	}
	f := Frame{
		Data:    data,
		Format:  format,
		ScrollX: ev.Metadata.ScrollOffsetX,
		ScrollY: ev.Metadata.ScrollOffsetY,
	}
	if cfg, name, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		f.Width, f.Height = cfg.Width, cfg.Height
		f.Format = name
	} else {
		f.Width, f.Height = int(ev.Metadata.DeviceWidth), int(ev.Metadata.DeviceHeight)
	}
	if ts := ev.Metadata.Timestamp; ts > 0 {
		sec, frac := math.Modf(ts)
		f.Timestamp = time.Unix(int64(sec), int64(frac*1e9))
	} else {
		f.Timestamp = time.Now()
	}
	return f, ev.SessionID, nil
}
