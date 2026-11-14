package evals

import (
	"encoding/json"
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

func convo(toolID string, input string, native string) llm.Request {
	return llm.Request{
		Model:  "claude-sonnet-5",
		System: "sys",
		Messages: []llm.Message{
			llm.UserText("edit the file"),
			{Role: llm.RoleAssistant, Provider: "anthropic", Native: json.RawMessage(native), Blocks: []llm.Block{
				{Type: llm.BlockText, Text: "ok"},
				{Type: llm.BlockToolUse, ToolUseID: toolID, ToolName: "edit_file", Input: json.RawMessage(input)},
			}},
			{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockToolResult, ToolUseID: toolID, Text: "done"}}},
		},
		Tools: []llm.ToolSpec{
			{Name: "edit_file", Description: "edit", Properties: map[string]any{"path": map[string]any{"type": "string"}, "n": map[string]any{"type": "integer"}}, Required: []string{"path", "n"}},
			{Name: "bash", Description: "run", Properties: map[string]any{"command": map[string]any{"type": "string"}}},
		},
		MaxTokens: 1000,
	}
}

func mustHash(t *testing.T, req llm.Request, opts CanonicalOptions) string {
	t.Helper()
	h, err := RequestHash(req, opts)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestCanonicalHashIgnoresVolatileData(t *testing.T) {
	a := convo("toolu_01AAA", `{"path":"a.go","n":1}`, `{"sig":"one"}`)
	b := convo("toolu_09ZZZ", "{\n  \"n\": 1.0,\n  \"path\": \"a.go\"\n}", `{"sig":"two"}`)
	// Tool registration order must not matter.
	b.Tools[0], b.Tools[1] = b.Tools[1], b.Tools[0]
	b.Tools[1].Required = []string{"n", "path"}
	ha, hb := mustHash(t, a, CanonicalOptions{}), mustHash(t, b, CanonicalOptions{})
	if ha != hb {
		d := structuralDiff(canonicalRequest(a, CanonicalOptions{}, nil), canonicalRequest(b, CanonicalOptions{}, nil))
		t.Fatalf("hashes differ for equivalent requests: %v", d)
	}
	// Stable across repeated computation (map iteration order).
	for i := 0; i < 50; i++ {
		if mustHash(t, a, CanonicalOptions{}) != ha {
			t.Fatal("hash is not deterministic")
		}
	}
	// Opt-ins make the volatile data significant again.
	if mustHash(t, a, CanonicalOptions{KeepToolIDs: true}) == mustHash(t, b, CanonicalOptions{KeepToolIDs: true}) {
		t.Error("KeepToolIDs should distinguish tool ids")
	}
	if mustHash(t, a, CanonicalOptions{IncludeNative: true}) == mustHash(t, b, CanonicalOptions{IncludeNative: true}) {
		t.Error("IncludeNative should distinguish native blobs")
	}
	c := convo("toolu_01AAA", `{"path":"a.go","n":1}`, `{}`)
	c.Tools[0], c.Tools[1] = c.Tools[1], c.Tools[0]
	if mustHash(t, a, CanonicalOptions{KeepToolOrder: true}) == mustHash(t, c, CanonicalOptions{KeepToolOrder: true}) {
		t.Error("KeepToolOrder should distinguish tool order")
	}
}

func TestCanonicalHashDetectsRealChanges(t *testing.T) {
	base := convo("t", `{"path":"a.go","n":1}`, "")
	h := mustHash(t, base, CanonicalOptions{})
	mutations := map[string]func(*llm.Request){
		"model":      func(r *llm.Request) { r.Model = "claude-opus-5" },
		"system":     func(r *llm.Request) { r.System = "other" },
		"max_tokens": func(r *llm.Request) { r.MaxTokens = 1 },
		"effort":     func(r *llm.Request) { r.Effort = "high" },
		"thinking":   func(r *llm.Request) { r.ShowThinking = true },
		"input":      func(r *llm.Request) { r.Messages[1].Blocks[1].Input = json.RawMessage(`{"path":"b.go","n":1}`) },
		"number":     func(r *llm.Request) { r.Messages[1].Blocks[1].Input = json.RawMessage(`{"path":"a.go","n":2}`) },
		"result":     func(r *llm.Request) { r.Messages[2].Blocks[0].Text = "failed" },
		"is_error":   func(r *llm.Request) { r.Messages[2].Blocks[0].IsError = true },
		"tool desc":  func(r *llm.Request) { r.Tools[1].Description = "run a command" },
		"image": func(r *llm.Request) {
			r.Messages[0].Blocks = append(r.Messages[0].Blocks, llm.Block{Type: llm.BlockImage, Image: &llm.Image{MediaType: llm.MediaPNG, Data: "AAAA"}})
		},
		"tool id order": func(r *llm.Request) {
			// Referencing a different (earlier) tool id changes the pairing.
			r.Messages[2].Blocks[0].ToolUseID = "other"
		},
	}
	for name, mut := range mutations {
		r := convo("t", `{"path":"a.go","n":1}`, "")
		mut(&r)
		if mustHash(t, r, CanonicalOptions{}) == h {
			t.Errorf("%s: change not reflected in hash", name)
		}
	}
}

func TestCanonicalIgnoreFields(t *testing.T) {
	a := convo("t", `{"path":"a.go","n":1}`, "")
	b := convo("t", `{"path":"a.go","n":1}`, "")
	b.MaxTokens = 9
	b.System = "different"
	b.Messages[2].Blocks[0].Text = "other result"
	opts := CanonicalOptions{IgnoreFields: []string{"max_tokens", "system", "messages.*.blocks.*.text"}}
	if mustHash(t, a, opts) != mustHash(t, b, opts) {
		t.Fatal("ignored fields still affect the hash")
	}
	if mustHash(t, a, CanonicalOptions{}) == mustHash(t, b, CanonicalOptions{}) {
		t.Fatal("fields should matter without the ignore list")
	}
	// Ignoring must not mutate the stored tree.
	tree := canonicalRequest(a, CanonicalOptions{}, nil)
	if _, err := hashTree(tree, []string{"system", "messages"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := tree["system"]; !ok {
		t.Fatal("hashTree mutated its input")
	}
}

// TestCanonicalHashGolden pins the hash of a fixed request so accidental
// changes to canonicalisation (which would invalidate every cassette) are
// caught. Update the constant only together with a cassette version bump.
func TestCanonicalHashGolden(t *testing.T) {
	const golden = "f6929733346c5a205524150287536102db6b2fdf8bf1db1ecbf94e4f687dae1f"
	got := mustHash(t, convo("toolu_x", `{"path":"a.go","n":1}`, `{"sig":"s"}`), CanonicalOptions{})
	if got != golden {
		t.Fatalf("canonical hash changed: got %s", got)
	}
}

func TestNormalizeNumber(t *testing.T) {
	cases := map[string]string{"1": "1", "1.0": "1", "1e0": "1", "-0": "0", "0.5": "0.5", "5e-1": "0.5", "123456789012345678": "123456789012345678", "1e300": "1e+300"}
	for in, want := range cases {
		if got := string(normalizeNumber(json.Number(in))); got != want {
			t.Errorf("normalizeNumber(%s) = %s, want %s", in, got, want)
		}
	}
}
