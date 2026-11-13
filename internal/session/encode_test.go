package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

// TestEncodeCacheKnowsAllFields fails when a field is added to the message
// types, so sameMessage and cloneMessage get updated with it.
func TestEncodeCacheKnowsAllFields(t *testing.T) {
	for _, c := range []struct {
		v any
		n int
	}{{llm.Message{}, 4}, {llm.Block{}, 8}, {llm.Image{}, 3}} {
		if got := reflect.TypeOf(c.v).NumField(); got != c.n {
			t.Errorf("%T has %d fields, want %d: update sameMessage and cloneMessage", c.v, got, c.n)
		}
	}
}

func randMessage(r *rand.Rand) llm.Message {
	m := llm.Message{Role: llm.RoleUser}
	if r.Intn(2) == 0 {
		m.Role = llm.RoleAssistant
	}
	switch r.Intn(4) {
	case 0:
		m.Provider, m.Native = "anthropic", json.RawMessage(fmt.Sprintf(`[{"type":"thinking","signature":"s%d"}]`, r.Intn(5)))
	case 1:
		m.Native = json.RawMessage{}
	}
	if r.Intn(8) != 0 {
		m.Blocks = []llm.Block{}
	}
	for n := r.Intn(4); n > 0; n-- {
		b := llm.Block{Type: llm.BlockText, Text: fmt.Sprintf("<text> & %d  ", r.Intn(5))}
		switch r.Intn(4) {
		case 0:
			b.Type, b.ToolUseID, b.ToolName = llm.BlockToolUse, fmt.Sprint("id", r.Intn(3)), "read_file"
			b.Input = json.RawMessage(fmt.Sprintf(`{"path": "a%d.go"}`, r.Intn(3)))
		case 1:
			b.Type, b.IsError = llm.BlockToolResult, r.Intn(2) == 0
			if r.Intn(2) == 0 {
				b.Images = []llm.Image{{MediaType: "image/png", Data: fmt.Sprint("d", r.Intn(3))}}
			}
		case 2:
			b.Type, b.Image = llm.BlockImage, &llm.Image{MediaType: "image/png", Path: fmt.Sprint("p", r.Intn(3))}
		}
		m.Blocks = append(m.Blocks, b)
	}
	return m
}

// mutate changes the conversation the ways callers can: appending turns,
// merging blocks into the last message, replacing or dropping messages
// (compaction), and editing fields in place.
func mutate(r *rand.Rand, msgs []llm.Message) []llm.Message {
	switch r.Intn(8) {
	case 0, 1:
		for n := 1 + r.Intn(3); n > 0; n-- {
			msgs = append(msgs, randMessage(r))
		}
	case 2:
		if len(msgs) > 0 {
			last := &msgs[len(msgs)-1]
			last.Blocks = append(last.Blocks, randMessage(r).Blocks...)
		}
	case 3:
		if len(msgs) > 0 {
			msgs[r.Intn(len(msgs))] = randMessage(r)
		}
	case 4:
		msgs = msgs[:r.Intn(len(msgs)+1)]
	case 5:
		// Edit bytes and fields in place, which a pointer check would miss.
		for i := range msgs {
			for j := range msgs[i].Blocks {
				b := &msgs[i].Blocks[j]
				if len(b.Input) > 3 && r.Intn(3) == 0 {
					b.Input[len(b.Input)-3] = 'x'
				}
				if b.Image != nil && r.Intn(3) == 0 {
					b.Image.Path += "!"
				}
				if len(b.Images) > 0 && r.Intn(3) == 0 {
					b.Images[0].Data += "!"
				}
				if r.Intn(10) == 0 {
					b.IsError = !b.IsError
				}
			}
			if len(msgs[i].Native) > 4 && r.Intn(4) == 0 {
				msgs[i].Native[len(msgs[i].Native)-4] = 'z'
			}
		}
	case 6:
		if len(msgs) > 0 {
			i := r.Intn(len(msgs))
			msgs[i].Blocks, msgs[i].Native = nil, nil
		}
	}
	return msgs
}

func TestSaveMatchesJSONMarshal(t *testing.T) {
	st := &Store{Dir: t.TempDir()}
	r := rand.New(rand.NewSource(1))
	for run := 0; run < 50; run++ {
		sess := New("/w", "m")
		sess.Messages = []llm.Message{llm.UserText("hello <world>")}
		for step := 0; step < 60; step++ {
			if len(sess.Messages) == 0 {
				sess.Messages = []llm.Message{randMessage(r)}
			}
			if err := st.Save(sess); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(st.Dir, sess.ID+".json"))
			if err != nil {
				t.Fatal(err)
			}
			cp := *sess
			cp.enc = nil
			want, err := json.Marshal(&cp)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("run %d step %d: saved file differs from json.Marshal\n got %s\nwant %s", run, step, got, want)
			}
			sess.Messages = mutate(r, sess.Messages)
		}
	}
}

func TestSaveInvalidNative(t *testing.T) {
	st := &Store{Dir: t.TempDir()}
	sess := New("/w", "m")
	sess.Messages = []llm.Message{llm.UserText("a"), {Role: llm.RoleAssistant, Native: json.RawMessage("{bad")}}
	err := st.Save(sess)
	cp := *sess
	cp.enc = nil
	_, want := json.Marshal(&cp)
	if err == nil || want == nil || err.Error() != want.Error() {
		t.Fatalf("got %v, want %v", err, want)
	}
	sess.Messages[1].Native = json.RawMessage(`{"ok":true}`)
	if err := st.Save(sess); err != nil {
		t.Fatal(err)
	}
}
