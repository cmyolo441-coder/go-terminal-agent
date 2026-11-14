package evals

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

// gradeEnv builds a workspace with a fixture snapshot, then lets the test
// modify it.
func gradeEnv(t *testing.T, files map[string]string) *GradeContext {
	t.Helper()
	ws := t.TempDir()
	for p, c := range files {
		full := filepath.Join(ws, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	snap, err := snapshotTree(ws)
	if err != nil {
		t.Fatal(err)
	}
	return &GradeContext{Workspace: ws, Fixture: snap, Prompt: "do it", Transcript: &Transcript{}}
}

func grade(t *testing.T, g *GraderSpec, gc *GradeContext) GradeResult {
	t.Helper()
	if err := g.compile(); err != nil {
		t.Fatalf("compile %s: %v", g.Type, err)
	}
	if g.Name == "" {
		g.Name = string(g.Type)
	}
	return g.Grade(context.Background(), gc)
}

func expect(t *testing.T, label string, r GradeResult, pass bool) {
	t.Helper()
	if r.Passed != pass {
		t.Errorf("%s: passed=%v, want %v (detail: %s, error: %s)", label, r.Passed, pass, r.Detail, r.Error)
	}
	if pass && r.Score != 1 && r.Type != GradeLLMJudge {
		t.Errorf("%s: passing score = %v", label, r.Score)
	}
}

func TestFileGraders(t *testing.T) {
	gc := gradeEnv(t, map[string]string{"src/main.go": "package main\nfunc Add(a, b int) int { return a + b }\n", "README.md": "Hello"})
	expect(t, "contains", grade(t, &GraderSpec{Type: GradeFileContains, Path: "src/main.go", Text: "func Add"}, gc), true)
	expect(t, "contains missing text", grade(t, &GraderSpec{Type: GradeFileContains, Path: "src/main.go", Text: "func Sub"}, gc), false)
	expect(t, "contains ignore case", grade(t, &GraderSpec{Type: GradeFileContains, Path: "README.md", Text: "hello", IgnoreCase: true}, gc), true)
	expect(t, "contains negate", grade(t, &GraderSpec{Type: GradeFileContains, Path: "src/main.go", Text: "TODO", Negate: true}, gc), true)
	expect(t, "contains no file", grade(t, &GraderSpec{Type: GradeFileContains, Path: "nope.go", Text: "x"}, gc), false)
	expect(t, "regex", grade(t, &GraderSpec{Type: GradeFileMatchesRegex, Path: "src/main.go", Pattern: `func Add\(a, b int\) int`}, gc), true)
	expect(t, "regex miss", grade(t, &GraderSpec{Type: GradeFileMatchesRegex, Path: "src/main.go", Pattern: `^func`}, gc), false)
	expect(t, "regex multiline flag", grade(t, &GraderSpec{Type: GradeFileMatchesRegex, Path: "src/main.go", Pattern: `(?m)^func`}, gc), true)

	if err := (&GraderSpec{Type: GradeFileContains, Path: "../etc/passwd", Text: "root"}).compile(); err == nil {
		t.Error("path traversal should be rejected at compile time")
	}
	// Even an uncompiled spec cannot read outside the workspace.
	r := (&GraderSpec{Type: GradeFileContains, Name: "raw", Path: "../etc/passwd", Text: "root"}).Grade(context.Background(), gc)
	if r.Passed || r.Error == "" {
		t.Errorf("path traversal must error, got %+v", r)
	}
	if err := (&GraderSpec{Type: GradeFileContains, Path: "/etc/passwd", Text: "x"}).compile(); err == nil {
		t.Error("absolute path should be rejected at compile time")
	}
	// Symlink escaping the workspace.
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("s3cret"), 0o644)
	if err := os.Symlink(outside, filepath.Join(gc.Workspace, "link")); err != nil {
		t.Skip("symlinks unsupported:", err)
	}
	r = grade(t, &GraderSpec{Type: GradeFileContains, Path: "link/secret", Text: "s3cret"}, gc)
	if r.Passed || r.Error == "" {
		t.Errorf("symlink escape should error, got %+v", r)
	}
}

func TestFileUnchanged(t *testing.T) {
	gc := gradeEnv(t, map[string]string{"a.go": "a", "b.go": "b", "docs/x.md": "x"})
	expect(t, "untouched", grade(t, &GraderSpec{Type: GradeFileUnchanged, Path: "a.go"}, gc), true)
	expect(t, "all untouched", grade(t, &GraderSpec{Type: GradeFileUnchanged, Path: "**"}, gc), true)
	os.WriteFile(filepath.Join(gc.Workspace, "a.go"), []byte("changed"), 0o644)
	os.Remove(filepath.Join(gc.Workspace, "docs/x.md"))
	r := grade(t, &GraderSpec{Type: GradeFileUnchanged, Path: "*.go"}, gc)
	expect(t, "glob modified", r, false)
	if !strings.Contains(r.Detail, "a.go (modified)") || strings.Contains(r.Detail, "b.go") {
		t.Errorf("detail = %q", r.Detail)
	}
	r = grade(t, &GraderSpec{Type: GradeFileUnchanged, Path: "docs/x.md"}, gc)
	if r.Passed || !strings.Contains(r.Detail, "deleted") {
		t.Errorf("deleted: %+v", r)
	}
	expect(t, "other glob", grade(t, &GraderSpec{Type: GradeFileUnchanged, Path: "b.go"}, gc), true)
	if r := grade(t, &GraderSpec{Type: GradeFileUnchanged, Path: "nothing/*"}, gc); r.Error == "" {
		t.Error("a glob matching no fixture file is a spec error")
	}
}

func TestCommandSucceeds(t *testing.T) {
	gc := gradeEnv(t, map[string]string{"f": "x"})
	var gotDir, gotCmd string
	gc.Commands = func(ctx context.Context, dir, command string) (CommandResult, error) {
		gotDir, gotCmd = dir, command
		switch command {
		case "go test ./...":
			return CommandResult{ExitCode: 0, Output: "ok  pkg 0.1s\nPASS"}, nil
		case "make lint":
			return CommandResult{ExitCode: 2, Output: "lint error"}, nil
		case "slow":
			<-ctx.Done()
			return CommandResult{}, ctx.Err()
		}
		return CommandResult{}, errors.New("no such shell")
	}
	expect(t, "ok", grade(t, &GraderSpec{Type: GradeCommandSucceeds, Command: "go test ./..."}, gc), true)
	if gotDir != gc.Workspace || gotCmd != "go test ./..." {
		t.Errorf("ran %q in %q", gotCmd, gotDir)
	}
	expect(t, "output pattern", grade(t, &GraderSpec{Type: GradeCommandSucceeds, Command: "go test ./...", Pattern: `(?m)^PASS$`}, gc), true)
	expect(t, "output pattern miss", grade(t, &GraderSpec{Type: GradeCommandSucceeds, Command: "go test ./...", Pattern: `FAIL`}, gc), false)
	r := grade(t, &GraderSpec{Type: GradeCommandSucceeds, Command: "make lint"}, gc)
	expect(t, "exit 2", r, false)
	if !strings.Contains(r.Detail, "exited 2") || !strings.Contains(r.Detail, "lint error") {
		t.Errorf("detail = %q", r.Detail)
	}
	r = grade(t, &GraderSpec{Type: GradeCommandSucceeds, Command: "slow", Timeout: Duration(20e6)}, gc)
	if r.Passed || r.Error != "" || !strings.Contains(r.Detail, "timed out") {
		t.Errorf("timeout: %+v", r)
	}
	if r := grade(t, &GraderSpec{Type: GradeCommandSucceeds, Command: "weird"}, gc); r.Error == "" {
		t.Error("runner failure should be a grader error")
	}
	gc.Commands = nil
	if r := grade(t, &GraderSpec{Type: GradeCommandSucceeds, Command: "x"}, gc); r.Error == "" {
		t.Error("missing runner should be a grader error")
	}
}

func TestShellRunner(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	dir := t.TempDir()
	run := ShellRunner()
	res, err := run(context.Background(), dir, "echo hi; pwd; exit 3")
	if err != nil {
		t.Fatal(err)
	}
	// pwd may print the path as given (macOS: /var/...) or resolved
	// (/private/var/...).
	real, _ := filepath.EvalSymlinks(dir)
	if res.ExitCode != 3 || !strings.Contains(res.Output, "hi") || (!strings.Contains(res.Output, real) && !strings.Contains(res.Output, dir)) {
		t.Errorf("got %+v", res)
	}
	res, err = run(context.Background(), dir, "head -c 2000000 /dev/zero | tr '\\0' a")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Output) > maxCommandOutput+100 || !strings.HasSuffix(res.Output, "[output truncated]") {
		t.Errorf("output not capped: %d bytes", len(res.Output))
	}
}

func toolTranscript() *Transcript {
	tr := &Transcript{Messages: []llm.Message{
		llm.UserText("go"),
		{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Type: llm.BlockToolUse, ToolUseID: "1", ToolName: "read_file", Input: json.RawMessage(`{"path":"main.go"}`)},
			{Type: llm.BlockToolUse, ToolUseID: "2", ToolName: "edit_file", Input: json.RawMessage(`{"path":"main.go","edits":[{"old":"a","new":"b"}],"dry_run":false}`)},
		}},
		{Role: llm.RoleUser, Blocks: []llm.Block{
			{Type: llm.BlockToolResult, ToolUseID: "1", Text: "package main"},
			{Type: llm.BlockToolResult, ToolUseID: "2", Text: "edited", IsError: false},
		}},
		{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Type: llm.BlockToolUse, ToolUseID: "3", ToolName: "bash", Input: json.RawMessage(`{"command":"go test ./...","timeout":60}`)},
			{Type: llm.BlockToolUse, ToolUseID: "4", ToolName: "mcp__github__search", Input: json.RawMessage(`{"q":"x"}`)},
		}},
		{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockToolResult, ToolUseID: "3", Text: "FAIL", IsError: true}}},
		{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "All done: ```json\n{\"status\": \"ok\", \"files\": [\"main.go\"]}\n```"}}},
	}}
	tr.Normalize()
	return tr
}

func TestTranscriptNormalize(t *testing.T) {
	tr := toolTranscript()
	if len(tr.ToolCalls) != 4 || tr.Turns != 3 {
		t.Fatalf("calls=%d turns=%d", len(tr.ToolCalls), tr.Turns)
	}
	if tr.ToolCalls[0].Output != "package main" || !tr.ToolCalls[2].IsError || tr.ToolCalls[2].Output != "FAIL" {
		t.Errorf("tool results not paired: %+v", tr.ToolCalls)
	}
	if !strings.HasPrefix(tr.Output, "All done") {
		t.Errorf("output = %q", tr.Output)
	}
}

func TestToolGraders(t *testing.T) {
	gc := gradeEnv(t, nil)
	gc.Transcript = toolTranscript()
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }
	cases := []struct {
		name string
		g    GraderSpec
		pass bool
	}{
		{"called", GraderSpec{Type: GradeToolCalled, Tool: "read_file"}, true},
		{"not called", GraderSpec{Type: GradeToolCalled, Tool: "write_file"}, false},
		{"glob", GraderSpec{Type: GradeToolCalled, Tool: "mcp__*"}, true},
		{"equals", GraderSpec{Type: GradeToolCalled, Tool: "read_file", Args: []ArgPredicate{{Path: "path", Value: raw(`"main.go"`)}}}, true},
		{"equals miss", GraderSpec{Type: GradeToolCalled, Tool: "read_file", Args: []ArgPredicate{{Path: "path", Value: raw(`"x.go"`)}}}, false},
		{"nested path", GraderSpec{Type: GradeToolCalled, Tool: "edit_file", Args: []ArgPredicate{{Path: "edits.0.new", Op: "equals", Value: raw(`"b"`)}}}, true},
		{"number equals", GraderSpec{Type: GradeToolCalled, Tool: "bash", Args: []ArgPredicate{{Path: "timeout", Value: raw(`60.0`)}}}, true},
		{"bool equals", GraderSpec{Type: GradeToolCalled, Tool: "edit_file", Args: []ArgPredicate{{Path: "dry_run", Value: raw(`false`)}}}, true},
		{"contains", GraderSpec{Type: GradeToolCalled, Tool: "bash", Args: []ArgPredicate{{Path: "command", Op: "contains", Value: raw(`"go test"`)}}}, true},
		{"matches", GraderSpec{Type: GradeToolCalled, Tool: "bash", Args: []ArgPredicate{{Path: "command", Op: "matches", Value: raw(`"^go (test|vet)"`)}}}, true},
		{"exists", GraderSpec{Type: GradeToolCalled, Tool: "edit_file", Args: []ArgPredicate{{Path: "edits", Op: "exists"}}}, true},
		{"absent", GraderSpec{Type: GradeToolCalled, Tool: "edit_file", Args: []ArgPredicate{{Path: "force", Op: "absent"}}}, true},
		{"not_equals", GraderSpec{Type: GradeToolCalled, Tool: "read_file", Args: []ArgPredicate{{Path: "path", Op: "not_equals", Value: raw(`"main.go"`)}}}, false},
		{"contains on object", GraderSpec{Type: GradeToolCalled, Tool: "edit_file", Args: []ArgPredicate{{Path: "edits", Op: "contains", Value: raw(`"\"old\":\"a\""`)}}}, true},
		{"all predicates", GraderSpec{Type: GradeToolCalled, Tool: "*_file", Args: []ArgPredicate{{Path: "path", Value: raw(`"main.go"`)}, {Path: "edits", Op: "exists"}}}, true},
		{"count max", GraderSpec{Type: GradeToolCalled, Tool: "*_file", MaxCount: ptr(1)}, false},
		{"count range", GraderSpec{Type: GradeToolCalled, Tool: "*_file", MinCount: ptr(2), MaxCount: ptr(2)}, true},
		{"min zero", GraderSpec{Type: GradeToolCalled, Tool: "write_file", MinCount: ptr(0), MaxCount: ptr(0)}, true},
		{"tool_not_called ok", GraderSpec{Type: GradeToolNotCalled, Tool: "write_file"}, true},
		{"tool_not_called fails", GraderSpec{Type: GradeToolNotCalled, Tool: "bash"}, false},
		{"tool_not_called with args", GraderSpec{Type: GradeToolNotCalled, Tool: "bash", Args: []ArgPredicate{{Path: "command", Op: "matches", Value: raw(`"rm -rf"`)}}}, true},
	}
	for _, c := range cases {
		g := c.g
		expect(t, c.name, grade(t, &g, gc), c.pass)
	}
	bad := []GraderSpec{
		{Type: GradeToolCalled},
		{Type: GradeToolCalled, Tool: "[", Args: nil},
		{Type: GradeToolCalled, Tool: "x", Args: []ArgPredicate{{Op: "between"}}},
		{Type: GradeToolCalled, Tool: "x", Args: []ArgPredicate{{Op: "matches", Value: raw(`"("`)}}},
		{Type: GradeToolCalled, Tool: "x", Args: []ArgPredicate{{Op: "equals"}}},
		{Type: GradeToolCalled, Tool: "x", MinCount: ptr(3), MaxCount: ptr(1)},
		{Type: GradeToolNotCalled, Tool: "x", MaxCount: ptr(1)},
	}
	for i, b := range bad {
		if err := b.compile(); err == nil {
			t.Errorf("bad tool grader %d compiled", i)
		}
	}
}

func TestOutputMatches(t *testing.T) {
	gc := gradeEnv(t, nil)
	gc.Transcript = &Transcript{Output: "The answer is 42.\nDone."}
	expect(t, "text", grade(t, &GraderSpec{Type: GradeOutputMatches, Text: "answer is 42"}, gc), true)
	expect(t, "regex", grade(t, &GraderSpec{Type: GradeOutputMatches, Pattern: `\b42\b`}, gc), true)
	expect(t, "both", grade(t, &GraderSpec{Type: GradeOutputMatches, Text: "answer", Pattern: `\b43\b`}, gc), false)
	expect(t, "ignore case", grade(t, &GraderSpec{Type: GradeOutputMatches, Pattern: `^the answer`, IgnoreCase: true}, gc), true)
	expect(t, "negate", grade(t, &GraderSpec{Type: GradeOutputMatches, Text: "error", Negate: true}, gc), true)
	if err := (&GraderSpec{Type: GradeOutputMatches}).compile(); err == nil {
		t.Error("output_matches without text or pattern compiled")
	}
}

func TestJSONSchemaGrader(t *testing.T) {
	gc := gradeEnv(t, map[string]string{"out.json": `{"status":"ok","count":3}`, "bad.json": `{"status":1`})
	gc.Transcript = toolTranscript()
	schema := json.RawMessage(`{"type":"object","required":["status","files"],"properties":{"status":{"enum":["ok","error"]},"files":{"type":"array","items":{"type":"string"},"minItems":1}}}`)
	expect(t, "fenced output", grade(t, &GraderSpec{Type: GradeJSONSchema, Schema: schema}, gc), true)
	gc.Transcript = &Transcript{Output: `Result: {"status": "maybe", "files": []} -- end`}
	r := grade(t, &GraderSpec{Type: GradeJSONSchema, Schema: schema}, gc)
	expect(t, "inline invalid", r, false)
	if !strings.Contains(r.Detail, "2 schema violation(s)") {
		t.Errorf("detail = %q", r.Detail)
	}
	gc.Transcript = &Transcript{Output: "no json here"}
	expect(t, "no json", grade(t, &GraderSpec{Type: GradeJSONSchema, Schema: schema}, gc), false)

	fileSchema := json.RawMessage(`{"type":"object","properties":{"count":{"type":"integer","minimum":1}},"required":["count"]}`)
	expect(t, "file", grade(t, &GraderSpec{Type: GradeJSONSchema, Source: "file", Path: "out.json", Schema: fileSchema}, gc), true)
	expect(t, "file invalid json", grade(t, &GraderSpec{Type: GradeJSONSchema, Source: "file", Path: "bad.json", Schema: fileSchema}, gc), false)
	expect(t, "file missing", grade(t, &GraderSpec{Type: GradeJSONSchema, Source: "file", Path: "none.json", Schema: fileSchema}, gc), false)
	if err := (&GraderSpec{Type: GradeJSONSchema, Schema: json.RawMessage(`{"type":"nope"}`)}).compile(); err == nil {
		t.Error("invalid schema compiled")
	}
	if err := (&GraderSpec{Type: GradeJSONSchema, Source: "stdin", Schema: fileSchema}).compile(); err == nil {
		t.Error("bad source compiled")
	}
}

func TestExtractJSON(t *testing.T) {
	cases := map[string]string{
		`{"a":1}`:                                  `{"a":1}`,
		"text\n```json\n[1, 2]\n```\nmore":         `[1, 2]`,
		"```\n{\"x\": true}\n```":                  `{"x": true}`,
		`prefix {"s": "brace } in string"} suffix`: `{"s": "brace } in string"}`,
		`[not json] then {"ok": 1}`:                `{"ok": 1}`,
	}
	for in, want := range cases {
		got, ok := ExtractJSON(in)
		if !ok || string(got) != want {
			t.Errorf("ExtractJSON(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "nothing", "{unclosed", "}{"} {
		if _, ok := ExtractJSON(in); ok {
			t.Errorf("ExtractJSON(%q) found JSON", in)
		}
	}
}

func TestLLMJudgeGrader(t *testing.T) {
	gc := gradeEnv(t, nil)
	gc.Transcript = &Transcript{Output: "I refactored the parser."}
	var seen JudgeRequest
	reply := "The refactor is clean.\n\n**Score:** 8/10"
	gc.Judge = func(ctx context.Context, req JudgeRequest) (string, error) {
		seen = req
		return reply, nil
	}
	r := grade(t, &GraderSpec{Type: GradeLLMJudge, Rubric: "Is the refactor clean?"}, gc)
	expect(t, "judge 8/10", r, true)
	if math.Abs(r.Score-0.8) > 1e-12 {
		t.Errorf("score = %v", r.Score)
	}
	if seen.Rubric != "Is the refactor clean?" || seen.Output != "I refactored the parser." || seen.Prompt != "do it" || seen.Scale != 10 {
		t.Errorf("judge request = %+v", seen)
	}
	reply = "Score: 3/5"
	r = grade(t, &GraderSpec{Type: GradeLLMJudge, Rubric: "x", Threshold: ptr(0.7)}, gc)
	expect(t, "judge below threshold", r, false)
	if math.Abs(r.Score-0.6) > 1e-12 {
		t.Errorf("partial score = %v", r.Score)
	}
	reply = "I cannot decide."
	if r := grade(t, &GraderSpec{Type: GradeLLMJudge, Rubric: "x"}, gc); r.Error == "" {
		t.Error("unparseable judge reply should be a grader error")
	}
	gc.Judge = func(context.Context, JudgeRequest) (string, error) { return "", errors.New("rate limited") }
	if r := grade(t, &GraderSpec{Type: GradeLLMJudge, Rubric: "x"}, gc); !strings.Contains(r.Error, "rate limited") {
		t.Errorf("judge error = %+v", r)
	}
	gc.Judge = nil
	if r := grade(t, &GraderSpec{Type: GradeLLMJudge, Rubric: "x"}, gc); r.Error == "" {
		t.Error("missing judge should be a grader error")
	}
}

func TestProviderJudge(t *testing.T) {
	var got llm.Request
	p := &scriptProvider{name: "judge", reply: func(req llm.Request) ([]llm.Event, *llm.Response, error) {
		got = req
		evs, resp := textReply("Fine work.\nScore: 9/10")
		return evs, resp, nil
	}}
	j := ProviderJudge(p, "claude-haiku-5-5", 0)
	reply, err := j(context.Background(), JudgeRequest{Rubric: "R", Prompt: "P", Output: "O", Scale: 10, Transcript: toolTranscript()})
	if err != nil {
		t.Fatal(err)
	}
	if s, err := ParseJudgeScore(reply, 10); err != nil || s != 0.9 {
		t.Errorf("score %v %v", s, err)
	}
	text := got.Messages[0].Text()
	for _, want := range []string{"## Rubric\nR", "## Task given to the agent\nP", "## Agent's final answer\nO", "- read_file", "0 to 10"} {
		if !strings.Contains(text, want) {
			t.Errorf("judge prompt misses %q:\n%s", want, text)
		}
	}
	if got.Model != "claude-haiku-5-5" || got.MaxTokens != 1024 || got.System == "" {
		t.Errorf("request = %+v", got)
	}
}

func TestParseJudgeScore(t *testing.T) {
	cases := []struct {
		in    string
		scale float64
		want  float64
	}{
		{"Score: 8/10", 10, 0.8},
		{"**Score:** 7.5 out of 10", 10, 0.75},
		{"**Final Score**: 4/5", 10, 0.8},
		{"Rating = 85%", 10, 0.85},
		{"score - 6", 10, 0.6},
		{"The score is 9", 10, 0.9},
		{"Overall score: 3 (scale 0-5)", 5, 0.6},
		{"Initial score: 2/10. After review, final score: 9/10.", 10, 0.9},
		{`{"score": 7, "reason": "ok"}`, 10, 0.7},
		{"```json\n{\"score\": 0.65}\n```", 1, 0.65},
		{`{"Score": 4, "max": 5}`, 10, 0.8},
		{`{"score": "8/10"}`, 10, 0.8},
		{"I'd give this a 6/10 overall.", 10, 0.6},
		{"7 out of 10", 10, 0.7},
		{"Verdict: PASS", 10, 1},
		{"verdict: fail", 10, 0},
		{"8", 10, 0.8},
		{"  **0.9**  ", 1, 0.9},
		{"75%", 10, 0.75},
		{"Grade: 10/10", 10, 1},
		{"SCORE:0", 10, 0},
	}
	for _, c := range cases {
		got, err := ParseJudgeScore(c.in, c.scale)
		if err != nil || math.Abs(got-c.want) > 1e-12 {
			t.Errorf("ParseJudgeScore(%q, %v) = %v, %v; want %v", c.in, c.scale, got, err, c.want)
		}
	}
	for _, in := range []string{"", "no idea", "Score: 15", "Score: 11/10", "Score: -1/10"} {
		if v, err := ParseJudgeScore(in, 10); err == nil {
			t.Errorf("ParseJudgeScore(%q) = %v, want error", in, v)
		}
	}
	if _, err := ParseJudgeScore("nothing", 10); !errors.Is(err, ErrNoScore) {
		t.Errorf("want ErrNoScore, got %v", err)
	}
}

func TestGraderCompileErrors(t *testing.T) {
	bad := []GraderSpec{
		{},
		{Type: "bogus"},
		{Type: GradeFileContains, Path: "a"},
		{Type: GradeFileContains, Text: "a"},
		{Type: GradeFileContains, Path: "../x", Text: "a"},
		{Type: GradeFileMatchesRegex, Path: "a", Pattern: "("},
		{Type: GradeFileMatchesRegex, Path: "a"},
		{Type: GradeFileUnchanged},
		{Type: GradeFileUnchanged, Path: "a/[b"},
		{Type: GradeCommandSucceeds},
		{Type: GradeJSONSchema},
		{Type: GradeLLMJudge},
		{Type: GradeLLMJudge, Rubric: "x", Threshold: ptr(1.5)},
		{Type: GradeLLMJudge, Rubric: "x", Scale: -1},
		{Type: GradeOutputMatches, Text: "x", Weight: ptr(-1.0)},
	}
	for i, g := range bad {
		if err := g.compile(); err == nil {
			t.Errorf("bad grader %d (%s) compiled", i, g.Type)
		}
	}
}
