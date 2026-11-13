package ui

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

// A 1x1 PNG.
const tinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

func TestBuildPromptAttachesTextAndImages(t *testing.T) {
	dir := t.TempDir()
	png, _ := base64.StdEncoding.DecodeString(tinyPNG)
	if err := os.WriteFile(filepath.Join(dir, "shot.png"), png, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.png"), []byte("not an image"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	env, err := tools.NewEnv(dir)
	if err != nil {
		t.Fatal(err)
	}
	msg, attached, problems := buildPrompt(env, "look at @shot.png and @notes.txt and @broken.png")
	if len(msg.Blocks) != 2 || msg.Blocks[1].Type != llm.BlockImage || msg.Blocks[1].Image.Path != filepath.Join(dir, "shot.png") {
		t.Fatalf("blocks = %+v", msg.Blocks)
	}
	if !strings.Contains(msg.Blocks[0].Text, "<file path=\"notes.txt\">\nhello\n</file>") || strings.Contains(msg.Blocks[0].Text, "not an image") {
		t.Fatalf("text = %q", msg.Blocks[0].Text)
	}
	if strings.Join(attached, ",") != "notes.txt,shot.png (image)" {
		t.Fatalf("attached = %v", attached)
	}
	if len(problems) != 1 || !strings.HasPrefix(problems[0], "broken.png:") {
		t.Fatalf("problems = %v", problems)
	}
}
