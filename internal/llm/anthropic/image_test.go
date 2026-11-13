package anthropic

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

func testPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestImageBlocksConvert(t *testing.T) {
	raw := testPNG(t)
	data := base64.StdEncoding.EncodeToString(raw)
	path := filepath.Join(t.TempDir(), "screen.png")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	hist := []llm.Message{
		{Role: llm.RoleUser, Blocks: []llm.Block{
			{Type: llm.BlockText, Text: "what is this?"},
			{Type: llm.BlockImage, Image: &llm.Image{MediaType: llm.MediaPNG, Data: data}},
		}},
		{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockToolUse, ToolUseID: "t1", ToolName: "screenshot", Input: json.RawMessage(`{}`)}}},
		{Role: llm.RoleUser, Blocks: []llm.Block{
			{Type: llm.BlockToolResult, ToolUseID: "t1", Text: "captured", Images: []llm.Image{{Path: path}}},
		}},
	}
	params, err := ToParams(hist)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	var msgs []struct {
		Role    string `json:"role"`
		Content []struct {
			Type    string                                  `json:"type"`
			Text    string                                  `json:"text"`
			Source  *struct{ Type, MediaType, Data string } `json:"source"`
			Content []struct {
				Type   string `json:"type"`
				Text   string `json:"text"`
				Source *struct {
					Type      string `json:"type"`
					MediaType string `json:"media_type"`
					Data      string `json:"data"`
				} `json:"source"`
			} `json:"content"`
		} `json:"content"`
	}
	if err := json.Unmarshal(wire, &msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 || len(msgs[0].Content) != 2 || msgs[0].Content[1].Type != "image" {
		t.Fatalf("user image: %s", wire)
	}
	if !strings.Contains(string(wire), `"source":{"data":"`+data+`","media_type":"image/png","type":"base64"}`) {
		t.Fatalf("image source not base64 png: %s", wire)
	}
	tr := msgs[2].Content[0]
	if tr.Type != "tool_result" || len(tr.Content) != 2 || tr.Content[0].Text != "captured" || tr.Content[1].Type != "image" {
		t.Fatalf("tool result: %s", wire)
	}
	if tr.Content[1].Source == nil || tr.Content[1].Source.Data != data || tr.Content[1].Source.MediaType != "image/png" {
		t.Fatalf("tool result image source: %s", wire)
	}
}

func TestImageErrorsSurface(t *testing.T) {
	bad := []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockImage, Image: &llm.Image{Path: "/nonexistent/x.png"}}}}}
	if _, err := ToParams(bad); err == nil || !strings.Contains(err.Error(), "message 0") {
		t.Fatalf("missing file: %v", err)
	}
	if _, err := ToParams([]llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockImage}}}}); err == nil {
		t.Fatal("nil image must fail")
	}
	trBad := []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockToolResult, ToolUseID: "t9", Images: []llm.Image{{MediaType: "image/tiff", Data: "AAAA"}}}}}}
	if _, err := ToParams(trBad); err == nil || !strings.Contains(err.Error(), "t9") {
		t.Fatalf("bad tool image: %v", err)
	}
}
