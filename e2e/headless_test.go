package e2e_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/audit"
)

// expectToolResult checks the tool_result for id in r's last message and
// returns its text.
func expectToolResult(r *apiRequest, id string, wantError bool, wantText ...string) string {
	b, ok := r.ToolResult(id)
	if !ok {
		r.errorf("no tool_result for %s in the last message", id)
		return ""
	}
	text := b.ResultText()
	if b.IsError != wantError {
		r.errorf("tool_result %s is_error = %v, want %v (text %q)", id, b.IsError, wantError, text)
	}
	for _, w := range wantText {
		if !strings.Contains(text, w) {
			r.errorf("tool_result %s = %q, want it to contain %q", id, text, w)
		}
	}
	return text
}

func TestHeadlessTextAnswer(t *testing.T) {
	t.Parallel()
	mock := newMock(t, func(r *apiRequest) reply {
		if r.Model != "claude-opus-5-5" {
			r.errorf("model = %q, want the default claude-opus-5-5", r.Model)
		}
		if len(r.Messages) != 1 || !strings.Contains(strings.Join(r.UserTexts(), " "), "What is 2+2?") {
			r.errorf("messages = %+v, want the one user prompt", r.Messages)
		}
		return textReply("2+2 is 4.")
	})
	e := newEnv(t, mock, nil)
	hr, _ := e.headless("What is 2+2?")
	if hr.IsError || hr.Error != "" {
		t.Fatalf("is_error = %v, error = %q", hr.IsError, hr.Error)
	}
	if hr.Result != "2+2 is 4." {
		t.Errorf("result = %q", hr.Result)
	}
	if hr.Usage.InputTokens != mockInputTokens || hr.Usage.OutputTokens != mockOutputTokens {
		t.Errorf("usage = %+v, want %d in / %d out", hr.Usage, mockInputTokens, mockOutputTokens)
	}
	if hr.Turns != 1 || len(hr.ToolCalls) != 0 || hr.Model != "claude-opus-5-5" || hr.SessionID == "" {
		t.Errorf("result = %+v", hr)
	}
	if hr.CostUSD <= 0 {
		t.Errorf("cost_usd = %v, want a priced call", hr.CostUSD)
	}
	if n := len(mock.Requests()); n != 1 {
		t.Errorf("mock got %d requests, want 1", n)
	}
}

func TestHeadlessToolLoop(t *testing.T) {
	requireUnixShell(t)
	t.Parallel()
	const content = "package main\n\nfunc main() { println(\"hi from e2e\") }\n"
	mock := newMock(t,
		func(r *apiRequest) reply {
			return toolReply("I'll create the file.", toolUse("toolu_write", "write_file", map[string]any{"path": "cmd/hello/main.go", "content": content}))
		},
		func(r *apiRequest) reply {
			expectToolResult(r, "toolu_write", false, "created", "main.go")
			return toolReply("", toolUse("toolu_bash", "bash", map[string]any{"command": "echo e2e-$((6*7)) && ls cmd/hello", "description": "check the file"}))
		},
		func(r *apiRequest) reply {
			expectToolResult(r, "toolu_bash", false, "e2e-42", "main.go", "exit code 0")
			// The whole conversation is replayed: prompt, both tool rounds.
			if len(r.Messages) != 5 {
				r.errorf("got %d messages, want 5", len(r.Messages))
			}
			return textReply("Created cmd/hello/main.go and checked it.")
		},
	)
	e := newEnv(t, mock, nil)
	hr, run := e.headless("create a hello program and check it", "--mode", "yolo")
	if hr.IsError || hr.Result != "Created cmd/hello/main.go and checked it." {
		t.Fatalf("result = %+v\nstderr:\n%s", hr, run.stderr)
	}
	if strings.Join(hr.ToolCalls, ",") != "write_file,bash" || hr.Turns != 3 {
		t.Errorf("tool_calls = %v, turns = %d", hr.ToolCalls, hr.Turns)
	}
	if hr.Usage.InputTokens != 3*mockInputTokens || hr.Usage.OutputTokens != 3*mockOutputTokens {
		t.Errorf("usage = %+v, want three calls summed", hr.Usage)
	}
	got, err := os.ReadFile(filepath.Join(e.root, "cmd", "hello", "main.go"))
	if err != nil || string(got) != content {
		t.Fatalf("file = %q, %v; want %q", got, err, content)
	}
	if n := len(mock.Requests()); n != 3 {
		t.Errorf("mock got %d requests, want 3", n)
	}
}

func TestAutoEditRunsAllowedCommands(t *testing.T) {
	t.Parallel()
	// auto-edit applies edits without asking; `go version` is in the
	// default allow_commands, so it runs too, while a command that is not
	// allowed is declined because nobody can approve it.
	mock := newMock(t,
		func(r *apiRequest) reply {
			return toolReply("", toolUse("toolu_w", "write_file", map[string]any{"path": "notes.txt", "content": "auto-edit\n"}))
		},
		func(r *apiRequest) reply {
			expectToolResult(r, "toolu_w", false, "created notes.txt")
			return toolReply("", toolUse("toolu_go", "bash", map[string]any{"command": "go version"}))
		},
		func(r *apiRequest) reply {
			expectToolResult(r, "toolu_go", false, "go version go1.")
			return toolReply("", toolUse("toolu_mkdir", "bash", map[string]any{"command": "mkdir should-not-exist"}))
		},
	)
	e := newEnv(t, mock, nil)
	hr, run := e.headless("write notes and check go", "--mode", "auto-edit")
	if hr.IsError {
		t.Fatalf("result = %+v", hr)
	}
	if b, err := os.ReadFile(filepath.Join(e.root, "notes.txt")); err != nil || string(b) != "auto-edit\n" {
		t.Errorf("notes.txt = %q, %v", b, err)
	}
	if e.exists("should-not-exist") {
		t.Error("a command that needs approval ran in headless auto-edit mode")
	}
	if !strings.Contains(run.stderr, "declined") {
		t.Errorf("stderr does not mention the declined command:\n%s", run.stderr)
	}
	// The declined call ends the turn: no fourth request.
	if n := len(mock.Requests()); n != 3 {
		t.Errorf("mock got %d requests, want 3", n)
	}
}

func TestPlanModeRefusesWrites(t *testing.T) {
	t.Parallel()
	mock := newMock(t,
		func(r *apiRequest) reply {
			return toolReply("", toolUse("toolu_plan", "write_file", map[string]any{"path": "plan.txt", "content": "nope"}))
		},
		func(r *apiRequest) reply {
			expectToolResult(r, "toolu_plan", true, "plan mode is read-only")
			return textReply("Plan: create plan.txt once allowed.")
		},
	)
	e := newEnv(t, mock, nil)
	hr, _ := e.headless("make plan.txt", "--mode", "plan")
	if hr.IsError || hr.Result != "Plan: create plan.txt once allowed." {
		t.Fatalf("result = %+v", hr)
	}
	if e.exists("plan.txt") {
		t.Fatal("plan mode created a file")
	}
}

func TestAskModeHeadlessDeclinesWrites(t *testing.T) {
	t.Parallel()
	mock := newMock(t, func(r *apiRequest) reply {
		return toolReply("Writing it now.", toolUse("toolu_ask", "write_file", map[string]any{"path": "ask.txt", "content": "nope"}))
	})
	e := newEnv(t, mock, nil)
	hr, run := e.headless("make ask.txt") // ask is the default mode
	if hr.IsError {
		t.Fatalf("result = %+v", hr)
	}
	if e.exists("ask.txt") {
		t.Fatal("ask mode wrote a file without approval")
	}
	if !strings.Contains(run.stderr, "declined") || !strings.Contains(run.stderr, "ask.txt") {
		t.Errorf("stderr does not explain the decline:\n%s", run.stderr)
	}
	if n := len(mock.Requests()); n != 1 {
		t.Errorf("mock got %d requests, want 1 (a declined action ends the turn)", n)
	}
	// The decline is recorded for the model, so a resumed session stays
	// valid: the next request must carry a tool_result for toolu_ask.
	mock.addSteps(func(r *apiRequest) reply {
		if len(r.Messages) < 3 {
			r.errorf("resumed conversation has %d messages, want the declined round too", len(r.Messages))
			return textReply("?")
		}
		for _, b := range r.Messages[2].Blocks {
			if b.Type == "tool_result" && b.ToolUseID == "toolu_ask" && b.IsError && strings.Contains(b.ResultText(), "declined") {
				return textReply("Understood.")
			}
		}
		r.errorf("the declined call has no error tool_result: %+v", r.Messages[2])
		return textReply("?")
	})
	hr2, _ := e.headless("ok, skip it", "-c")
	if hr2.Result != "Understood." || hr2.SessionID != hr.SessionID {
		t.Errorf("resumed result = %+v (first session %s)", hr2, hr.SessionID)
	}
}

func TestPolicyDeniesCommandAndAuditVerifies(t *testing.T) {
	requireUnixShell(t)
	t.Parallel()
	const policyMsg = "Blocked by the e2e policy; ask #dev-tools."
	mock := newMock(t,
		func(r *apiRequest) reply {
			return toolReply("", toolUse("toolu_touch", "bash", map[string]any{"command": "touch policy-ran.txt"}))
		},
		func(r *apiRequest) reply {
			expectToolResult(r, "toolu_touch", true, policyMsg)
			return textReply("The policy blocked touch.")
		},
	)
	e := newEnv(t, mock, map[string]any{"audit_log": true})
	policyFile := filepath.Join(e.cfgDir, "..", "policy.json")
	e.writeFile(policyFile, `{"version": 1, "message": "`+policyMsg+`", "commands": {"deny": ["touch"]}}`)
	e.extra = append(e.extra, "GTA_POLICY_FILE="+policyFile)

	// yolo would run anything; the policy must still win.
	hr, _ := e.headless("touch a file", "--mode", "yolo")
	if hr.IsError || hr.Result != "The policy blocked touch." {
		t.Fatalf("result = %+v", hr)
	}
	if e.exists("policy-ran.txt") {
		t.Fatal("a command denied by policy ran")
	}

	dir := filepath.Join(e.cfgDir, "audit")
	rep, err := audit.Verify(dir, audit.VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() || rep.Records == 0 {
		t.Fatalf("audit verify: %+v", rep)
	}
	var denied, models, ended int
	err = audit.Read(dir, func(r audit.Record) error {
		switch {
		case r.Event == audit.EventPolicyDecision && r.Tool == "bash" && r.Decision == audit.DecisionDeny:
			denied++
		case r.Event == audit.EventModelCall:
			models++
		case r.Event == audit.EventSessionEnd:
			ended++
		case r.Event == audit.EventToolCall:
			t.Errorf("a tool call was audited as run: %+v", r)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if denied != 1 || models != 2 || ended != 1 {
		t.Errorf("audit records: %d policy denials, %d model calls, %d session ends; want 1, 2, 1", denied, models, ended)
	}
}

// fakeToken builds a GitHub-token-shaped string at run time, so no
// secret-looking literal sits in the source.
func fakeToken(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	h := hex.EncodeToString(sum[:])
	// Mix case so it looks like a real token to the entropy checks.
	var b strings.Builder
	for i, c := range h[:36] {
		if i%3 == 0 && c >= 'a' {
			c -= 'a' - 'A'
		}
		b.WriteRune(c)
	}
	return "ghp_" + b.String()
}

func TestSecretsNeverReachTheModel(t *testing.T) {
	t.Parallel()
	inFile := fakeToken("gta-e2e-redaction-file")
	inMemory := fakeToken("gta-e2e-redaction-memory")
	inPrompt := fakeToken("gta-e2e-redaction-prompt")
	mock := newMock(t,
		func(r *apiRequest) reply {
			if !strings.Contains(strings.Join(r.UserTexts(), " "), "is my token valid?") {
				r.errorf("prompt text missing: %v", r.UserTexts())
			}
			return toolReply("", toolUse("toolu_read", "read_file", map[string]any{"path": "deploy.env"}))
		},
		func(r *apiRequest) reply {
			text := expectToolResult(r, "toolu_read", false, "GITHUB_TOKEN=", "REDACTED", "REGION=eu-west-1")
			if strings.Contains(text, "ghp_") {
				r.errorf("tool result still holds the token: %q", text)
			}
			return textReply("The file sets a GitHub token and the region.")
		},
	)
	e := newEnv(t, mock, nil)
	fileContent := "REGION=eu-west-1\nGITHUB_TOKEN=" + inFile + "\n"
	e.writeFile("deploy.env", fileContent)
	// Memory files go into the system prompt, which must be redacted too.
	e.writeFile("AGENTS.md", "Deploy with GITHUB_TOKEN="+inMemory+" from CI.\n")
	hr, run := e.headless(inPrompt + " is my token valid? Also, what does deploy.env configure?")
	if hr.IsError {
		t.Fatalf("result = %+v", hr)
	}
	if len(mock.Requests()) != 2 {
		t.Fatalf("mock got %d requests, want 2", len(mock.Requests()))
	}
	for _, r := range mock.Requests() {
		body := string(r.Raw)
		for name, s := range map[string]string{"file": inFile, "memory file": inMemory, "prompt": inPrompt} {
			if strings.Contains(body, s[4:]) {
				t.Errorf("request %d sent the secret from the %s to the model", r.N, name)
			}
		}
		if !strings.Contains(body, "Deploy with GITHUB_TOKEN=") {
			t.Errorf("request %d lost the memory file around the secret", r.N)
		}
	}
	if !strings.Contains(run.stderr, "Redacted") {
		t.Errorf("stderr does not report the redaction:\n%s", run.stderr)
	}
	// Local files keep the real value.
	if b, _ := os.ReadFile(filepath.Join(e.root, "deploy.env")); string(b) != fileContent {
		t.Errorf("deploy.env changed: %q", b)
	}
}

func TestFallbackOnOverloaded(t *testing.T) {
	t.Parallel()
	mock := newMock(t)
	mock.setHandler(func(r *apiRequest) reply {
		switch r.Model {
		case "claude-opus-5-5":
			return errorReply(529, "overloaded_error", "Overloaded")
		case "claude-sonnet-5-5":
			return textReply("Answered by the fallback.")
		}
		r.errorf("unexpected model %q", r.Model)
		return errorReply(400, "invalid_request_error", "unknown model")
	})
	e := newEnv(t, mock, map[string]any{
		"fallback_models": []any{map[string]any{"provider": "anthropic", "model": "claude-sonnet-5-5"}},
		"routing":         map[string]any{"max_retries": -1},
	})
	hr, run := e.headless("hello")
	if hr.IsError || hr.Result != "Answered by the fallback." {
		t.Fatalf("result = %+v\nstderr:\n%s", hr, run.stderr)
	}
	var models []string
	for _, r := range mock.Requests() {
		models = append(models, r.Model)
	}
	if strings.Join(models, ",") != "claude-opus-5-5,claude-sonnet-5-5" {
		t.Errorf("models requested = %v, want the primary once, then the fallback", models)
	}
	if !strings.Contains(run.stderr, "falling back to anthropic claude-sonnet-5-5") {
		t.Errorf("stderr does not announce the fallback:\n%s", run.stderr)
	}
}

func TestContinueSession(t *testing.T) {
	t.Parallel()
	mock := newMock(t,
		func(r *apiRequest) reply {
			return textReply("Noted: the code word is kiwi.")
		},
		func(r *apiRequest) reply {
			if len(r.Messages) != 3 {
				r.errorf("got %d messages, want the previous exchange plus the new prompt", len(r.Messages))
				return textReply("?")
			}
			first, prev, now := r.Messages[0], r.Messages[1], r.Messages[2]
			if first.Role != "user" || !strings.Contains(first.Blocks[0].Text, "code word is kiwi") {
				r.errorf("first message = %+v", first)
			}
			if prev.Role != "assistant" || prev.Blocks[len(prev.Blocks)-1].Text != "Noted: the code word is kiwi." {
				r.errorf("previous answer = %+v", prev)
			}
			if !strings.Contains(now.Blocks[0].Text, "What was the code word?") {
				r.errorf("new prompt = %+v", now)
			}
			return textReply("kiwi")
		},
	)
	e := newEnv(t, mock, nil)
	first, _ := e.headless("Remember: the code word is kiwi.")
	second, _ := e.headless("What was the code word?", "-c")
	if second.Result != "kiwi" || second.IsError {
		t.Fatalf("second result = %+v", second)
	}
	if first.SessionID == "" || second.SessionID != first.SessionID {
		t.Errorf("session ids %q then %q, want the same session", first.SessionID, second.SessionID)
	}
	// --resume by id prefix reaches the same session too.
	mock.addSteps(func(r *apiRequest) reply {
		if len(r.Messages) != 5 {
			r.errorf("resumed session has %d messages, want 5", len(r.Messages))
		}
		return textReply("still kiwi")
	})
	third, _ := e.headless("and now?", "--resume", first.SessionID[:len(first.SessionID)-2])
	if third.Result != "still kiwi" || third.SessionID != first.SessionID {
		t.Errorf("third result = %+v", third)
	}
}

func TestInvalidStartup(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		config     string // user config.json; "" keeps the base config
		policy     string // GTA_POLICY_FILE contents; "" for none
		policyMode os.FileMode
		want       []string
	}{
		{name: "bad permission mode", config: `{"permission_mode": "sometimes"}`, want: []string{`permission_mode "sometimes"`}},
		{name: "unknown config field", config: `{"modle": "claude-opus-5-5"}`, want: []string{"config.json", `unknown field "modle"`}},
		{name: "malformed config", config: `{"model": `, want: []string{"config.json"}},
		{name: "bad max_tokens", config: `{"max_tokens": 5}`, want: []string{"max_tokens must be between"}},
		{name: "policy version", policy: `{"version": 2}`, want: []string{"managed policy", "version must be 1"}},
		{name: "policy unknown field", policy: `{"version": 1, "comands": {"deny": ["curl"]}}`, want: []string{"managed policy", `unknown field "comands"`}},
		{name: "policy bad mode", policy: `{"version": 1, "max_permission_mode": "godmode"}`, want: []string{"managed policy", "max_permission_mode"}},
		{name: "policy forbids model", policy: `{"version": 1, "models": {"allow": ["claude-haiku-*"]}}`, want: []string{"managed policy", "claude-opus-5-5"}},
		{name: "world-writable policy", policy: `{"version": 1}`, policyMode: 0o666, want: []string{"world-writable"}},
		{name: "missing workspace", want: []string{"is not a directory"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.policyMode&0o002 != 0 && os.PathSeparator == '\\' {
				t.Skip("file modes are not enforced on Windows")
			}
			mock := newMock(t) // any request is a failure
			e := newEnv(t, mock, nil)
			if tc.config != "" {
				e.writeFile(filepath.Join(e.cfgDir, "config.json"), tc.config)
			}
			if tc.policy != "" {
				pf := filepath.Join(e.cfgDir, "policy.json")
				e.writeFile(pf, tc.policy)
				if tc.policyMode != 0 {
					if err := os.Chmod(pf, tc.policyMode); err != nil {
						t.Fatal(err)
					}
				}
				e.extra = append(e.extra, "GTA_POLICY_FILE="+pf)
			}
			args := []string{"-p", "hello", "--json"}
			if tc.name == "missing workspace" {
				args = append([]string{"--cwd", filepath.Join(e.root, "nope")}, args...)
			}
			res := e.run(args...)
			if res.code == 0 {
				t.Fatalf("gta exited 0\nstdout:\n%s\nstderr:\n%s", res.stdout, res.stderr)
			}
			if !strings.HasPrefix(res.stderr, "gta: ") {
				t.Errorf("stderr does not start with \"gta: \":\n%s", res.stderr)
			}
			for _, w := range tc.want {
				if !strings.Contains(res.stderr, w) {
					t.Errorf("stderr lacks %q:\n%s", w, res.stderr)
				}
			}
			if strings.TrimSpace(res.stdout) != "" {
				t.Errorf("stdout should be empty, got:\n%s", res.stdout)
			}
			if n := len(mock.Requests()); n != 0 {
				t.Errorf("gta called the model %d time(s) despite the bad setup", n)
			}
		})
	}
}

func TestHeadlessFlagOrder(t *testing.T) {
	t.Parallel()
	// The README documents flags both before and after the prompt.
	cases := []struct {
		name   string
		args   []string
		json   bool
		prompt string
	}{
		{"flags first", []string{"--json", "--mode", "plan", "-p", "say hi"}, true, "say hi"},
		{"flags after the prompt", []string{"-p", "say hi", "--mode", "plan", "--json"}, true, "say hi"},
		{"value with equals", []string{"-p", "say hi", "--mode=plan", "--json"}, true, "say hi"},
		{"double dash keeps the rest as prompt", []string{"-p", "say", "--", "--json", "hi"}, false, "say --json hi"},
		{"unknown dash words stay prompt text", []string{"-p", "explain", "-v", "and", "--frobnicate", "--json"}, true, "explain -v and --frobnicate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mock := newMock(t, func(r *apiRequest) reply {
				if got := strings.Join(r.UserTexts(), " "); got != tc.prompt {
					r.errorf("prompt = %q, want %q", got, tc.prompt)
				}
				return textReply("hi!")
			})
			e := newEnv(t, mock, nil)
			res := e.run(tc.args...)
			if res.code != 0 {
				t.Fatalf("exit %d\nstderr:\n%s", res.code, res.stderr)
			}
			var hr headlessResult
			isJSON := json.Unmarshal([]byte(res.stdout), &hr) == nil
			if isJSON != tc.json {
				t.Fatalf("JSON output = %v, want %v:\n%s", isJSON, tc.json, res.stdout)
			}
			if tc.json && hr.Result != "hi!" {
				t.Errorf("result = %+v", hr)
			}
			if !tc.json && strings.TrimSpace(res.stdout) != "hi!" {
				t.Errorf("stdout = %q", res.stdout)
			}
		})
	}
}
