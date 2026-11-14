package evals

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

const suiteJSON = `{
  "name": "smoke",
  "description": "end-to-end harness check",
  "defaults": {"repetitions": 3, "limits": {"max_turns": 5, "timeout": "30s"}},
  "cases": [
    {
      "id": "write-hello",
      "tags": ["files"],
      "fixture": {"dir": "fixtures/basic", "files": {"notes/todo.txt": "keep me"}},
      "prompt": "write hello.txt: Hello, world",
      "pass_threshold": 0.95,
      "graders": [
        {"type": "file_contains", "path": "hello.txt", "text": "Hello", "weight": 2, "required": true},
        {"type": "file_matches_regex", "path": "hello.txt", "pattern": "^Hello, \\w+$"},
        {"type": "file_unchanged", "path": "**"},
        {"type": "tool_called", "tool": "write_file", "args": [{"path": "path", "value": "hello.txt"}], "max_count": 1},
        {"type": "tool_not_called", "tool": "bash"},
        {"type": "command_succeeds", "command": "check hello.txt"},
        {"type": "output_matches", "pattern": "(?i)done"},
        {"type": "llm_judge", "rubric": "Did it write the greeting?", "weight": 0.5}
      ]
    },
    {
      "id": "answer-json",
      "prompt": "answer as JSON",
      "repetitions": 2,
      "graders": [
        {"type": "json_schema", "schema": {"type": "object", "required": ["answer"], "properties": {"answer": {"const": 42}}}}
      ]
    },
    {
      "id": "always-fails",
      "prompt": "answer as JSON",
      "pass_threshold": 0.5,
      "graders": [
        {"type": "output_matches", "text": "never printed"},
        {"type": "output_matches", "text": "answer", "weight": 0}
      ]
    }
  ]
}`

func writeSuite(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	fix := filepath.Join(dir, "fixtures", "basic")
	if err := os.MkdirAll(filepath.Join(fix, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(fix, "src", "main.go"), []byte("package main\n"), 0o644)
	os.WriteFile(filepath.Join(fix, "README.md"), []byte("# fixture\n"), 0o644)
	path := filepath.Join(dir, "suite.json")
	if err := os.WriteFile(path, []byte(suiteJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func testRunner(p ProviderFactory) *Runner {
	return &Runner{
		Agent:    fakeAgent,
		Provider: p,
		Commands: func(ctx context.Context, dir, command string) (CommandResult, error) {
			if _, err := os.Stat(filepath.Join(dir, strings.TrimPrefix(command, "check "))); err != nil {
				return CommandResult{ExitCode: 1, Output: err.Error()}, nil
			}
			return CommandResult{}, nil
		},
		Judge: func(ctx context.Context, req JudgeRequest) (string, error) {
			return "Looks right. Score: 9/10", nil
		},
		Concurrency: 4,
		TempDir:     "",
		Seed:        7,
	}
}

func TestSuiteRunEndToEnd(t *testing.T) {
	suite, err := LoadSuite(writeSuite(t))
	if err != nil {
		t.Fatal(err)
	}
	model := codingModel()
	r := testRunner(StaticProvider(model))
	r.TempDir = t.TempDir()
	var progress atomic.Int64
	r.OnRun = func(RunResult) { progress.Add(1) }
	res, err := r.Run(context.Background(), suite)
	if err != nil {
		t.Fatal(err)
	}
	if progress.Load() != 8 || res.Summary.Runs != 8 {
		t.Fatalf("runs = %d (progress %d), want 3+2+3", res.Summary.Runs, progress.Load())
	}
	byID := indexCases(res)
	w := byID["write-hello"]
	if w.Passes != 3 || !near(w.MeanScore, 8.45/8.5, 1e-12) || w.PassAt(1) != 1 || w.PassHat(3) != 1 {
		for _, run := range w.Runs {
			t.Logf("%s", run.FailureReason())
			for _, g := range run.Grades {
				t.Logf("  %s %v %s %s", g.Name, g.Passed, g.Detail, g.Error)
			}
		}
		t.Fatalf("write-hello: %+v", w)
	}
	run := w.Runs[0]
	if run.Turns != 2 || run.ToolCalls != 1 || run.Usage.InputTokens != 220 || run.CostUSD <= 0 || run.Model != "claude-sonnet-5" {
		t.Errorf("metering: %+v", run)
	}
	if len(run.Grades) != 8 || run.Workspace != "" {
		t.Errorf("grades=%d workspace=%q", len(run.Grades), run.Workspace)
	}
	if a := byID["answer-json"]; a.Passes != 2 || a.N != 2 {
		t.Errorf("answer-json: %+v", a)
	}
	f := byID["always-fails"]
	if f.Passes != 0 || f.MeanScore != 0 || !strings.Contains(f.Runs[0].FailureReason(), "never printed") {
		t.Errorf("always-fails: %+v / %s", f, f.Runs[0].FailureReason())
	}
	s := res.Summary
	if s.PassedRuns != 5 || s.CasesAllPassed != 2 || s.CasesAnyPassed != 2 || s.AgentError != 0 {
		t.Errorf("summary: %+v", s)
	}
	if !near(s.MeanScore, (8.45/8.5+1)/3, 1e-12) {
		t.Errorf("macro mean = %v", s.MeanScore)
	}
	// Workspaces are cleaned up.
	left, _ := os.ReadDir(r.TempDir)
	if len(left) != 0 {
		t.Errorf("%d workspace(s) left behind", len(left))
	}
	// Order of cases follows the suite.
	if res.Cases[0].ID != "write-hello" || res.Cases[2].ID != "always-fails" {
		t.Errorf("case order: %s, %s", res.Cases[0].ID, res.Cases[2].ID)
	}
	// The fixture directory itself was never modified.
	if _, err := os.Stat(filepath.Join(filepath.Dir(suite.Cases[0].fixDir), "basic", "hello.txt")); !os.IsNotExist(err) {
		t.Error("agent wrote into the source fixture")
	}
}

func TestRunWithCassetteIsDeterministic(t *testing.T) {
	suite, err := LoadSuite(writeSuite(t))
	if err != nil {
		t.Fatal(err)
	}
	cpath := filepath.Join(t.TempDir(), "smoke.jsonl")
	live := codingModel()
	rec, err := OpenRecorder(cpath, ModeRecord, live, RecorderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r := testRunner(func(rc RunContext) (llm.Provider, error) { return rc.Session(rec), nil })
	recorded, err := r.Run(context.Background(), suite)
	if err != nil {
		t.Fatal(err)
	}
	rec.Close()
	liveCalls := live.calls.Load()

	rep, err := OpenRecorder(cpath, ModeReplay, nil, RecorderOptions{StrictReplay: false})
	if err != nil {
		t.Fatal(err)
	}
	defer rep.Close()
	r2 := testRunner(func(rc RunContext) (llm.Provider, error) { return rc.Session(rep), nil })
	replayed, err := r2.Run(context.Background(), suite)
	if err != nil {
		t.Fatal(err)
	}
	if live.calls.Load() != liveCalls {
		t.Fatal("replay reached the live provider")
	}
	for i := range recorded.Cases {
		a, b := recorded.Cases[i], replayed.Cases[i]
		if a.Passes != b.Passes || a.MeanScore != b.MeanScore {
			t.Errorf("%s: recorded %d/%v, replayed %d/%v", a.ID, a.Passes, a.MeanScore, b.Passes, b.MeanScore)
		}
		for j := range b.Runs {
			if b.Runs[j].Error != "" {
				t.Errorf("%s #%d replay error: %s", b.ID, j, b.Runs[j].Error)
			}
		}
	}
	// The workspace path is in every system prompt yet never in the cassette.
	data, _ := os.ReadFile(cpath)
	if strings.Contains(string(data), "gta-eval-") || !strings.Contains(string(data), WorkspacePlaceholder) {
		t.Error("workspace paths were not replaced by the placeholder")
	}
}

func TestLimitsEnforced(t *testing.T) {
	looping := &scriptProvider{name: "loop", reply: func(req llm.Request) ([]llm.Event, *llm.Response, error) {
		evs, resp := toolReply("t", "write_file", map[string]any{"path": "x.txt", "content": "x"})
		resp.Usage = llm.Usage{InputTokens: 1000, OutputTokens: 1000}
		return evs, resp, nil
	}}
	mk := func(l Limits) *Suite {
		return &Suite{Name: "limits", Cases: []*Case{{
			ID: "c", Prompt: "loop", Limits: l,
			Graders: []*GraderSpec{{Type: GradeFileContains, Path: "x.txt", Text: "x"}},
		}}}
	}
	cases := map[string]Limits{
		"max_turns":    {MaxTurns: 3},
		"max_tokens":   {MaxTokens: 5000},
		"max_cost_usd": {MaxCostUSD: 0.01},
	}
	for want, l := range cases {
		r := testRunner(StaticProvider(looping))
		res, err := r.Run(context.Background(), mk(l))
		if err != nil {
			t.Fatal(err)
		}
		run := res.Cases[0].Runs[0]
		if run.Passed || len(run.Violations) == 0 || run.Violations[0] != want || run.Error != "" {
			t.Errorf("%s: %+v", want, run)
		}
		if run.Score != 1 {
			t.Errorf("%s: graders should still run, score %v", want, run.Score)
		}
	}

	slow := func(ctx context.Context, ws, prompt string, p llm.Provider) (Transcript, error) {
		<-ctx.Done()
		return Transcript{}, ctx.Err()
	}
	r := testRunner(StaticProvider(looping))
	r.Agent = slow
	res, err := r.Run(context.Background(), mk(Limits{Timeout: Duration(30 * time.Millisecond)}))
	if err != nil {
		t.Fatal(err)
	}
	if run := res.Cases[0].Runs[0]; run.Passed || len(run.Violations) != 1 || run.Violations[0] != "timeout" || run.Error != "" {
		t.Errorf("timeout: %+v", run)
	}
}

func TestAgentFailuresAreIsolated(t *testing.T) {
	var n atomic.Int64
	agent := func(ctx context.Context, ws, prompt string, p llm.Provider) (Transcript, error) {
		switch n.Add(1) % 3 {
		case 0:
			panic("boom")
		case 1:
			return Transcript{}, errors.New("model unavailable")
		}
		os.WriteFile(filepath.Join(ws, "ok"), []byte("ok"), 0o644)
		return Transcript{Output: "fine"}, nil
	}
	s := &Suite{Name: "iso", Cases: []*Case{{ID: "c", Prompt: "p", Repetitions: 9,
		Graders: []*GraderSpec{{Type: GradeFileContains, Path: "ok", Text: "ok"}}}}}
	r := testRunner(StaticProvider(codingModel()))
	r.Agent = agent
	r.KeepFailed = true
	r.TempDir = t.TempDir()
	res, err := r.Run(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	c := res.Cases[0]
	if c.Passes != 3 || res.Summary.AgentError != 6 {
		t.Fatalf("passes=%d errors=%d", c.Passes, res.Summary.AgentError)
	}
	var panics int
	for _, run := range c.Runs {
		if strings.Contains(run.Error, "agent panicked: boom") {
			panics++
		}
		if !run.Passed && run.Workspace == "" {
			t.Error("failed workspace should be kept")
		}
		if run.Passed && run.Workspace != "" {
			t.Error("passing workspace should be removed")
		}
	}
	if panics != 3 {
		t.Errorf("panics = %d", panics)
	}
	if c.PassRate != 1.0/3 || !near(c.PassAt(3), 1-PassHatK(9, 6, 3), 1e-12) {
		t.Errorf("pass rate %v pass@3 %v", c.PassRate, c.PassAt(3))
	}
}

func TestRunConcurrencyAndCancel(t *testing.T) {
	var cur, peak atomic.Int64
	var mu sync.Mutex
	agent := func(ctx context.Context, ws, prompt string, p llm.Provider) (Transcript, error) {
		v := cur.Add(1)
		mu.Lock()
		if v > peak.Load() {
			peak.Store(v)
		}
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		cur.Add(-1)
		return Transcript{Output: "x"}, nil
	}
	s := &Suite{Name: "c", Cases: []*Case{{ID: "c", Prompt: "p", Repetitions: 20,
		Graders: []*GraderSpec{{Type: GradeOutputMatches, Text: "x"}}}}}
	r := testRunner(StaticProvider(codingModel()))
	r.Agent = agent
	r.Concurrency = 3
	if _, err := r.Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if peak.Load() > 3 || peak.Load() < 2 {
		t.Errorf("peak concurrency %d, want 2..3", peak.Load())
	}

	ctx, cancel := context.WithCancel(context.Background())
	var started atomic.Int64
	r.Agent = func(ctx context.Context, ws, prompt string, p llm.Provider) (Transcript, error) {
		if started.Add(1) == 2 {
			cancel()
		}
		return Transcript{Output: "x"}, nil
	}
	r.Concurrency = 1
	res, err := r.Run(ctx, s)
	if !errors.Is(err, context.Canceled) || res == nil || res.Summary.Runs >= 20 {
		t.Errorf("cancel: err=%v runs=%v", err, res)
	}
}

func TestSuiteValidation(t *testing.T) {
	bad := map[string]string{
		"unknown field":   `{"name":"s","cases":[{"id":"a","prompt":"p","graders":[{"type":"output_matches","text":"x","txet":"y"}]}]}`,
		"no cases":        `{"name":"s","cases":[]}`,
		"no name":         `{"cases":[{"id":"a","prompt":"p","graders":[{"type":"output_matches","text":"x"}]}]}`,
		"dup id":          `{"name":"s","cases":[{"id":"a","prompt":"p","graders":[{"type":"output_matches","text":"x"}]},{"id":"a","prompt":"p","graders":[{"type":"output_matches","text":"x"}]}]}`,
		"bad id":          `{"name":"s","cases":[{"id":"a b","prompt":"p","graders":[{"type":"output_matches","text":"x"}]}]}`,
		"no prompt":       `{"name":"s","cases":[{"id":"a","graders":[{"type":"output_matches","text":"x"}]}]}`,
		"no graders":      `{"name":"s","cases":[{"id":"a","prompt":"p"}]}`,
		"zero weights":    `{"name":"s","cases":[{"id":"a","prompt":"p","graders":[{"type":"output_matches","text":"x","weight":0}]}]}`,
		"bad threshold":   `{"name":"s","cases":[{"id":"a","prompt":"p","pass_threshold":2,"graders":[{"type":"output_matches","text":"x"}]}]}`,
		"bad duration":    `{"name":"s","defaults":{"limits":{"timeout":"soon"}},"cases":[{"id":"a","prompt":"p","graders":[{"type":"output_matches","text":"x"}]}]}`,
		"neg limit":       `{"name":"s","cases":[{"id":"a","prompt":"p","limits":{"max_turns":-1},"graders":[{"type":"output_matches","text":"x"}]}]}`,
		"missing fixture": `{"name":"s","cases":[{"id":"a","prompt":"p","fixture":{"dir":"nope"},"graders":[{"type":"output_matches","text":"x"}]}]}`,
		"escaping file":   `{"name":"s","cases":[{"id":"a","prompt":"p","fixture":{"files":{"../x":"y"}},"graders":[{"type":"output_matches","text":"x"}]}]}`,
		"dup grader name": `{"name":"s","cases":[{"id":"a","prompt":"p","graders":[{"type":"output_matches","text":"x","name":"g"},{"type":"output_matches","text":"y","name":"g"}]}]}`,
		"trailing":        `{"name":"s","cases":[]} {}`,
		"bad repetitions": `{"name":"s","cases":[{"id":"a","prompt":"p","repetitions":-2,"graders":[{"type":"output_matches","text":"x"}]}]}`,
	}
	for name, js := range bad {
		if _, err := ParseSuite([]byte(js), t.TempDir()); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	s, err := ParseSuite([]byte(`{"name":"s","defaults":{"repetitions":4,"pass_threshold":0.5,"limits":{"max_turns":7,"timeout":90}},
		"cases":[{"id":"a","prompt":"p","limits":{"max_turns":2},"graders":[{"type":"output_matches","text":"x"}]}]}`), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := s.Cases[0]
	if c.EffectiveRepetitions() != 4 || c.EffectiveThreshold() != 0.5 || c.EffectiveLimits().MaxTurns != 2 || c.EffectiveLimits().Timeout.Std() != 90*time.Second {
		t.Errorf("defaults: reps=%d thr=%v limits=%+v", c.EffectiveRepetitions(), c.EffectiveThreshold(), c.EffectiveLimits())
	}
	if c.Graders[0].Name != "output_matches#1" {
		t.Errorf("default grader name = %q", c.Graders[0].Name)
	}
	if err := s.Validate(); err != nil {
		t.Errorf("Validate is not idempotent: %v", err)
	}
	if _, err := (&Runner{}).Run(context.Background(), s); err == nil {
		t.Error("runner without agent should fail")
	}
}

func TestFixtureSymlinks(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("a"), 0o644)
	if err := os.Symlink("a.txt", filepath.Join(src, "link")); err != nil {
		t.Skip(err)
	}
	dst := t.TempDir()
	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(filepath.Join(dst, "link")); err != nil || target != "a.txt" {
		t.Errorf("link = %q %v", target, err)
	}
	os.Symlink("../../etc/passwd", filepath.Join(src, "evil"))
	if err := copyTree(src, t.TempDir()); err == nil {
		t.Error("escaping symlink copied")
	}
}
