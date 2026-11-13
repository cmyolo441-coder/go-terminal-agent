package openai

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

func testJPEG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, 4, 4)), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestImagesBecomeDataURLParts(t *testing.T) {
	raw := testJPEG(t)
	data := base64.StdEncoding.EncodeToString(raw)
	path := filepath.Join(t.TempDir(), "photo.jpeg")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	url := "data:image/jpeg;base64," + data
	hist := []llm.Message{
		{Role: llm.RoleUser, Blocks: []llm.Block{
			{Type: llm.BlockText, Text: "describe"},
			{Type: llm.BlockImage, Image: &llm.Image{Path: path}},
		}},
		{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Type: llm.BlockToolUse, ToolUseID: "c1", ToolName: "screenshot"},
			{Type: llm.BlockToolUse, ToolUseID: "c2", ToolName: "ls"},
		}},
		{Role: llm.RoleUser, Blocks: []llm.Block{
			{Type: llm.BlockToolResult, ToolUseID: "c1", Text: "done", Images: []llm.Image{{MediaType: llm.MediaJPEG, Data: data}, {Path: path}}},
			{Type: llm.BlockToolResult, ToolUseID: "c2", Text: "a.go"},
		}},
	}
	msgs, err := toMessages("", hist)
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := json.Marshal(msgs)
	got := string(wire)
	want := `[` +
		`{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"` + url + `"}}]},` +
		`{"role":"assistant","content":null,"tool_calls":[` +
		`{"id":"c1","type":"function","function":{"name":"screenshot","arguments":"{}"}},` +
		`{"id":"c2","type":"function","function":{"name":"ls","arguments":"{}"}}]},` +
		`{"role":"tool","content":"done\n\n(2 images are attached in the next message.)","tool_call_id":"c1"},` +
		`{"role":"tool","content":"a.go","tool_call_id":"c2"},` +
		`{"role":"user","content":[{"type":"text","text":"Images returned by tool call (c1):"},` +
		`{"type":"image_url","image_url":{"url":"` + url + `"}},{"type":"image_url","image_url":{"url":"` + url + `"}}]}` +
		`]`
	if got != want {
		t.Fatalf("wire mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestImageConversionErrors(t *testing.T) {
	cases := [][]llm.Block{
		{{Type: llm.BlockImage}},
		{{Type: llm.BlockImage, Image: &llm.Image{Path: "/nonexistent.png"}}},
		{{Type: llm.BlockToolResult, ToolUseID: "x", Images: []llm.Image{{MediaType: "image/png", Data: "Zm9v"}}}},
	}
	for i, blocks := range cases {
		if _, err := toMessages("", []llm.Message{{Role: llm.RoleUser, Blocks: blocks}}); err == nil {
			t.Fatalf("case %d: expected an error", i)
		}
	}
	// Text-only messages keep the plain string form.
	msgs, err := toMessages("", []llm.Message{llm.UserText("hi")})
	if err != nil {
		t.Fatal(err)
	}
	if wire, _ := json.Marshal(msgs); string(wire) != `[{"role":"user","content":"hi"}]` {
		t.Fatalf("text-only: %s", wire)
	}
	// Images also survive the full request body.
	p, err := New(Options{Name: "vllm", BaseURL: "http://localhost:8000/v1"})
	if err != nil {
		t.Fatal(err)
	}
	img, _ := llm.ImageFromBytes(testJPEG(t))
	body, err := p.buildBody(llm.Request{Model: "m", Messages: []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockImage, Image: &img}}}}})
	if err != nil || !strings.Contains(string(body), `"image_url":{"url":"data:image/jpeg;base64,`) {
		t.Fatalf("body: %s %v", body, err)
	}
}
