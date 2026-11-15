package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/mockapi"
)

// When re-executed with GOCODE_FAKE_MCP set, the test binary acts as a tiny
// stdio MCP server so the real client can be exercised end to end.
func TestMain(m *testing.M) {
	if os.Getenv("GOCODE_FAKE_MCP") != "" {
		fakeMCP()
		return
	}
	os.Exit(m.Run())
}

func fakeMCP() {
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var req struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &req) != nil || req.ID == nil {
			continue
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "fake"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "shout", "description": "Upper-cases text",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []string{"text"}}}}}
		case "tools/call":
			var p struct {
				Arguments struct {
					Text string `json:"text"`
				} `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": strings.ToUpper(p.Arguments.Text)}}}
		}
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
		fmt.Println(string(b))
	}
}

func build(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "gocode")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

func runCLI(t *testing.T, bin, dir, home, url string, stdin string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "ANTHROPIC_BASE_URL="+url, "ANTHROPIC_API_KEY=test", "GOCODE_HOME="+home, "NO_COLOR=1")
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return stdout.String(), stderr.String(), code
}

func TestPrintModeEndToEnd(t *testing.T) {
	bin := build(t)
	dir, home := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Always answer in haiku."), 0o644)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("attached-content"), 0o644)
	os.MkdirAll(filepath.Join(dir, ".gocode", "commands"), 0o755)
	os.WriteFile(filepath.Join(dir, ".gocode", "commands", "greet.md"), []byte("---\ndescription: greet\n---\nSay hello to $ARGUMENTS"), 0o644)
	self, _ := os.Executable()
	settings := map[string]any{
		"mcpServers":  map[string]any{"fake": map[string]any{"command": self, "env": map[string]string{"GOCODE_FAKE_MCP": "1"}}},
		"permissions": map[string]any{"allow": []string{"mcp__fake"}},
		"hooks": map[string]any{"PreToolUse": []any{map[string]any{"matcher": "Bash", "hooks": []any{
			map[string]any{"type": "command", "command": "echo 'no shell today' >&2; exit 2"}}}}},
	}
	b, _ := json.Marshal(settings)
	os.WriteFile(filepath.Join(dir, ".gocode", "settings.json"), b, 0o644)

	srv := mockapi.New(
		mockapi.Response{Blocks: []mockapi.Block{
			{Type: "tool_use", ID: "t1", Name: "mcp__fake__shout", Input: `{"text":"quiet"}`},
			{Type: "tool_use", ID: "t2", Name: "Bash", Input: `{"command":"echo hi"}`},
			{Type: "tool_use", ID: "t3", Name: "Write", Input: `{"file_path":"out.txt","content":"x"}`},
		}},
		mockapi.Response{Blocks: []mockapi.Block{{Type: "text", Text: "first answer"}}},
		mockapi.Response{Blocks: []mockapi.Block{{Type: "text", Text: "second answer"}}},
		mockapi.Response{Blocks: []mockapi.Block{{Type: "text", Text: "third answer"}}},
	)
	defer srv.Close()

	// 1. JSON output, piped stdin, @mention, MCP tool, hook and a permission that cannot be asked.
	out, stderr, code := runCLI(t, bin, dir, home, srv.URL, "piped text", "-p", "--output-format", "json", "look at @notes.txt")
	if code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, stderr)
	}
	var res struct {
		Result    string  `json:"result"`
		SessionID string  `json:"session_id"`
		IsError   bool    `json:"is_error"`
		Cost      float64 `json:"total_cost_usd"`
		Requests  int     `json:"num_requests"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("bad json %q: %v", out, err)
	}
	if res.Result != "first answer" || res.IsError || res.Requests != 2 || res.Cost <= 0 || res.SessionID == "" {
		t.Fatalf("result = %+v", res)
	}
	req := srv.Request(0)
	sys := req["system"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(sys, "Always answer in haiku.") {
		t.Error("AGENTS.md is not in the system prompt")
	}
	user, _ := json.Marshal(req["messages"].([]any)[0])
	for _, want := range []string{"look at @notes.txt", "piped text", "attached-content"} {
		if !strings.Contains(string(user), want) {
			t.Errorf("first user message lacks %q", want)
		}
	}
	results := srv.Request(1)["messages"].([]any)[2].(map[string]any)["content"].([]any)
	text := func(i int) (string, bool) {
		r := results[i].(map[string]any)
		isErr, _ := r["is_error"].(bool)
		return r["content"].([]any)[0].(map[string]any)["text"].(string), isErr
	}
	if s, e := text(0); s != "QUIET" || e {
		t.Errorf("MCP result = %q err=%v", s, e)
	}
	if s, e := text(1); !e || !strings.Contains(s, "no shell today") {
		t.Errorf("hook should block Bash: %q err=%v", s, e)
	}
	if s, e := text(2); !e || !strings.Contains(s, "non-interactive") {
		t.Errorf("Write should be declined without a prompt: %q err=%v", s, e)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.txt")); err == nil {
		t.Error("unapproved write happened")
	}

	// 2. --continue restores the conversation.
	out, stderr, code = runCLI(t, bin, dir, home, srv.URL, "", "-p", "-c", "and then?")
	if code != 0 || strings.TrimSpace(out) != "second answer" {
		t.Fatalf("continue: exit %d out %q stderr %q", code, out, stderr)
	}
	if n := len(srv.Request(2)["messages"].([]any)); n != 5 {
		t.Errorf("continued request has %d messages, want 5", n)
	}

	// 3. A custom slash command expands, and stream-json emits events.
	out, _, code = runCLI(t, bin, dir, home, srv.URL, "", "-p", "--output-format", "stream-json", "/greet", "Asha")
	if code != 0 || !strings.Contains(out, `"type":"assistant"`) || !strings.Contains(out, `"type":"result"`) {
		t.Fatalf("stream-json: exit %d out %q", code, out)
	}
	last, _ := json.Marshal(srv.Request(3)["messages"].([]any)[0])
	if !strings.Contains(string(last), "Say hello to Asha") {
		t.Errorf("custom command not expanded: %s", last)
	}
}
