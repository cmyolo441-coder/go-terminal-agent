package collab

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Client talks to one room as one participant.
type Client struct {
	// BaseURL is the server root, e.g. http://127.0.0.1:7878.
	BaseURL string
	RoomID  string
	Token   string
	// HTTP defaults to a client without an overall timeout (streams are
	// long-lived).
	HTTP *http.Client

	seq    atomic.Uint64
	lastID atomic.Uint64
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) url(suffix string) string {
	return strings.TrimRight(c.BaseURL, "/") + "/v1/collab/rooms/" + url.PathEscape(c.RoomID) + suffix
}

// decodeError turns a non-2xx response into an *Error.
func decodeError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e apiErrorBody
	out := &Error{Status: resp.StatusCode, Code: "http_" + strconv.Itoa(resp.StatusCode), Message: strings.TrimSpace(string(body))}
	if json.Unmarshal(body, &e) == nil && e.Error.Code != "" {
		out.Code, out.Message = e.Error.Code, e.Error.Message
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if s, err := strconv.Atoi(ra); err == nil {
			out.RetryAfter = time.Duration(s) * time.Second
		}
	}
	return out
}

func (c *Client) do(ctx context.Context, method, url, token string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return decodeError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// CreateRoom creates a room with the hub's admin token.
func CreateRoom(ctx context.Context, hc *http.Client, baseURL, adminToken string, cfg RoomConfig) (CreateRoomResponse, error) {
	c := &Client{BaseURL: baseURL, HTTP: hc}
	var out CreateRoomResponse
	err := c.do(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/v1/collab/rooms", adminToken, cfg, &out)
	return out, err
}

// Send posts one upstream message, numbering it so a retry of the same
// call is recognised as a duplicate.
func (c *Client) Send(ctx context.Context, typ string, data any) (Reply, error) {
	var raw json.RawMessage
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return Reply{}, err
		}
		raw = b
	}
	msg := ClientMessage{Type: typ, Seq: c.seq.Add(1), Data: raw}
	var reply Reply
	err := c.do(ctx, http.MethodPost, c.url("/messages"), c.Token, msg, &reply)
	return reply, err
}

// Invite invites a participant (the client must be an owner).
func (c *Client) Invite(ctx context.Context, name string, role Role) (Credentials, error) {
	var out Credentials
	err := c.do(ctx, http.MethodPost, c.url("/participants"), c.Token, InviteRequest{Name: name, Role: role}, &out)
	return out, err
}

// Info fetches the room view.
func (c *Client) Info(ctx context.Context) (RoomView, error) {
	var out RoomView
	err := c.do(ctx, http.MethodGet, c.url(""), c.Token, nil, &out)
	return out, err
}

// LastEventID returns the id of the last sequenced event received.
func (c *Client) LastEventID() uint64 { return c.lastID.Load() }

// Stream is one open event stream.
type Stream struct {
	body   io.ReadCloser
	rd     *bufio.Reader
	client *Client
	// Retry is the reconnection delay the server last asked for.
	Retry time.Duration
}

// Open connects an event stream. With resume set it sends lastID as
// Last-Event-ID.
func (c *Client) Open(ctx context.Context, lastID uint64, resume bool) (*Stream, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url("/events"), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "text/event-stream")
	if resume {
		req.Header.Set("Last-Event-ID", strconv.FormatUint(lastID, 10))
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, decodeError(resp)
	}
	return &Stream{body: resp.Body, rd: bufio.NewReaderSize(resp.Body, 64<<10), client: c}, nil
}

// Close ends the stream.
func (s *Stream) Close() error { return s.body.Close() }

// readLine reads one SSE line without its terminator, bounded in length.
func (s *Stream) readLine() (string, error) {
	var buf []byte
	for {
		chunk, err := s.rd.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > 2*MaxMessageBytes+4096 {
			return "", errors.New("collab: SSE line too long")
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			if err == io.EOF && len(buf) > 0 {
				return "", io.ErrUnexpectedEOF
			}
			return "", err
		}
		line := strings.TrimSuffix(string(buf), "\n")
		return strings.TrimSuffix(line, "\r"), nil
	}
}

// Next returns the next event; io.EOF means the server ended the stream.
func (s *Stream) Next() (Event, error) {
	var (
		typ  string
		data strings.Builder
		id   string
		has  bool
	)
	for {
		line, err := s.readLine()
		if err != nil {
			return Event{}, err
		}
		if line == "" {
			if !has {
				continue
			}
			var ev Event
			if err := json.Unmarshal([]byte(data.String()), &ev); err != nil {
				return Event{}, fmt.Errorf("collab: bad event data: %w", err)
			}
			if ev.Type == "" {
				ev.Type = typ
			}
			if id != "" {
				if n, err := strconv.ParseUint(id, 10, 64); err == nil {
					s.client.lastID.Store(n)
				}
			}
			return ev, nil
		}
		if line[0] == ':' {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			typ, has = value, true
		case "data":
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(value)
			has = true
		case "id":
			id = value
		case "retry":
			if ms, err := strconv.Atoi(value); err == nil {
				s.Retry = time.Duration(ms) * time.Millisecond
			}
		}
	}
}

// ErrStop, returned by a Run handler, ends Run without error.
var ErrStop = errors.New("collab: stop")

// Run keeps an event stream open, reconnecting with Last-Event-ID after
// network errors and slow-consumer disconnects, and calls handle for every
// event (including each connection's welcome). It returns when ctx ends,
// handle returns an error, or the server refuses the participant or
// closes the room.
func (c *Client) Run(ctx context.Context, handle func(Event) error) error {
	backoff := 250 * time.Millisecond
	connected := false
	for {
		s, err := c.Open(ctx, c.lastID.Load(), connected)
		if err != nil {
			var ae *Error
			if errors.As(err, &ae) && ae.Status != http.StatusTooManyRequests && ae.Status < 500 {
				return err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
		} else {
			connected = true
			backoff = 250 * time.Millisecond
			stop, herr, _ := c.consume(s, handle)
			s.Close()
			if stop || errors.Is(herr, ErrStop) {
				return nil
			}
			if herr != nil {
				return herr
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if s.Retry > 0 {
				backoff = s.Retry
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 10*time.Second)
	}
}

// consume reads events until the stream ends. stop reports a terminal
// disconnect, herr a handler error, and rerr the (retryable) read error.
func (c *Client) consume(s *Stream, handle func(Event) error) (stop bool, herr, rerr error) {
	for {
		ev, err := s.Next()
		if err != nil {
			return false, nil, err
		}
		if err := handle(ev); err != nil {
			return false, err, nil
		}
		if ev.Type == EvDisconnect {
			var d DisconnectEvent
			_ = json.Unmarshal(ev.Data, &d)
			switch d.Reason {
			case ReasonKicked, ReasonRoomClosed, ReasonLeft:
				return true, nil, nil
			}
			return false, nil, io.EOF
		}
	}
}
