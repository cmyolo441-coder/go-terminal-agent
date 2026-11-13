package session

import (
	"bytes"
	"encoding/json"
	"slices"
	"sync"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

// encodeCache keeps the JSON of each message from the previous save. A
// conversation mostly grows at the end, so a save after a new turn reuses
// the encoding of every earlier message that is still equal to what was
// encoded, and the file comes out byte for byte as json.Marshal would
// write it.
type encodeCache struct {
	mu   sync.Mutex
	msgs []encodedMessage
}

type encodedMessage struct {
	msg  llm.Message // a copy that shares no mutable memory with the caller
	data []byte
}

// messagesNull is how json.Marshal ends a Session whose Messages is nil.
const messagesNull = `"messages":null}`

// marshal returns json.Marshal(sess).
func (c *encodeCache) marshal(sess *Session) ([]byte, error) {
	head := *sess
	head.Messages = nil
	head.enc = nil
	h, err := json.Marshal(head)
	if err != nil {
		return nil, err
	}
	if !bytes.HasSuffix(h, []byte(messagesNull)) || sess.Messages == nil {
		return json.Marshal(sess)
	}
	h = h[:len(h)-len("null}")]

	c.mu.Lock()
	defer c.mu.Unlock()
	size := len(h) + 2
	for i := range sess.Messages {
		m := &sess.Messages[i]
		if i < len(c.msgs) && sameMessage(&c.msgs[i].msg, m) {
			size += len(c.msgs[i].data) + 1
			continue
		}
		data, err := json.Marshal(m)
		if err != nil {
			c.msgs = c.msgs[:min(i, len(c.msgs))]
			return nil, err
		}
		e := encodedMessage{msg: cloneMessage(m), data: data}
		if i < len(c.msgs) {
			c.msgs[i] = e
		} else {
			c.msgs = append(c.msgs, e)
		}
		size += len(data) + 1
	}
	clear(c.msgs[len(sess.Messages):])
	c.msgs = c.msgs[:len(sess.Messages)]

	out := make([]byte, 0, size)
	out = append(out, h...)
	out = append(out, '[')
	for i := range c.msgs {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, c.msgs[i].data...)
	}
	out = append(out, ']', '}')
	return out, nil
}

// sameMessage reports whether two messages encode the same. Strings that
// were not changed share their bytes, so comparing them is cheap.
func sameMessage(a, b *llm.Message) bool {
	if a.Role != b.Role || a.Provider != b.Provider || !sameBytes(a.Native, b.Native) || len(a.Blocks) != len(b.Blocks) || (a.Blocks == nil) != (b.Blocks == nil) {
		return false
	}
	for i := range a.Blocks {
		x, y := &a.Blocks[i], &b.Blocks[i]
		if x.Type != y.Type || x.Text != y.Text || x.ToolUseID != y.ToolUseID || x.ToolName != y.ToolName ||
			x.IsError != y.IsError || !sameBytes(x.Input, y.Input) || !slices.Equal(x.Images, y.Images) ||
			(x.Images == nil) != (y.Images == nil) || (x.Image == nil) != (y.Image == nil) ||
			(x.Image != nil && *x.Image != *y.Image) {
			return false
		}
	}
	return true
}

// sameBytes is bytes.Equal that also tells nil from empty, which encode
// differently for a RawMessage.
func sameBytes(a, b []byte) bool {
	return (a == nil) == (b == nil) && bytes.Equal(a, b)
}

// cloneMessage copies everything in m that the caller could change in
// place; strings are immutable and are shared.
func cloneMessage(m *llm.Message) llm.Message {
	c := *m
	c.Native = slices.Clone(m.Native)
	c.Blocks = slices.Clone(m.Blocks)
	for i := range c.Blocks {
		b := &c.Blocks[i]
		b.Input = slices.Clone(b.Input)
		b.Images = slices.Clone(b.Images)
		if b.Image != nil {
			img := *b.Image
			b.Image = &img
		}
	}
	return c
}
