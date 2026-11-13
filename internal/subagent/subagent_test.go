package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/agent"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permission"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

const testModel = "claude-opus-5-5"

// fake is a provider whose replies come from a handler, so each child's
// conversation can be scripted independently and concurrently.
type fake struct {
	mu       sync.Mutex
	requests []llm.Request
	handle   func(ctx context.Context, req llm.Request) (*llm.Response, error)
}

func (f *fake) Name() string { return "fake" }

func (f *fake) Stream(ctx context.Context, req llm.Request, on func(llm.Event)) (*llm.Response, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	r, err := f.handle(ctx, req)
	if err == nil && r != nil {
		if t := r.Message.Text(); t != "" {
			on(llm.Event{Type: llm.EventTextDelta, Text: t})
		}
	}
	return r, err
}

func (f *fake) reqs() []llm.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]llm.Request(nil), f.requests...)
}

// scripted answers each conversation (keyed by its task prompt) with the
// response for its turn.
func scripted(script map[string][]*llm.Response) *fake {
	return &fake{handle: func(_ context.Context, req llm.Request) (*llm.Response, error) {
		p, turn := promptOf(req), turnOf(req)
		rs, ok := script[p]
		if !ok || turn >= len(rs) {
			return nil, fmt.Errorf("no scripted reply for %q turn %d", p, turn)
		}
		return rs[turn], nil
	}}
}

func promptOf(req llm.Request) string { return req.Messages[0].Blocks[0].Text }
func turnOf(req llm.Request) int      { return len(req.Messages) / 2 }

func toolUse(id, name string, input any) llm.Block {
	raw, _ := json.Marshal(input)
	return llm.Block{Type: llm.BlockToolUse, ToolUseID: id, ToolName: name, Input: raw}
}

func text(s string) llm.Block { return llm.Block{Type: llm.BlockText, Text: s} }

func reply(stop llm.StopReason, blocks ...llm.Block) *llm.Response {
	return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Blocks: blocks}, StopReason: stop, Model: testModel,
		Usage: llm.Usage{InputTokens: 100, OutputTokens: 10}}
}

func done(s string) *llm.Response { return reply(llm.StopEndTurn, text(s)) }

func newEnv(t *testing.T) *tools.Env {
	t.Helper()
	env, err := tools.NewEnv(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func baseConfig(p llm.Provider) Config {
	return Config{
		Provider: p,
		Settings: func() agent.Settings {
			return agent.Settings{Model: testModel, MaxTokens: 4096, Effort: "high", ShowThinking: true}
		},
	}
}

func taskInput(mode string, tasks ...Task) json.RawMessage {
	m := map[string]any{"tasks": tasks}
	if mode != "" {
		m["mode"] = mode
	}
	raw, _ := json.Marshal(m)
	return raw
}

// events records progress callbacks and checks they are never concurrent.
type events struct {
	mu     sync.Mutex
	active atomic.Int32
	list   []string
	race   atomic.Bool
}

func (e *events) progress(i int, desc, ev string) {
	if e.active.Add(1) > 1 {
		e.race.Store(true)
	}
	defer e.active.Add(-1)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.list = append(e.list, fmt.Sprintf("%d|%s|%s", i, desc, ev))
}

func (e *events) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.list...)
}

func TestChildrenRunInParallel(t *testing.T) {
	const n = 3
	var arrived atomic.Int32
	release := make(chan struct{})
	p := &fake{handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		// Barrier: every child must be inside the provider at the same time.
		if arrived.Add(1) == n {
			close(release)
		}
		select {
		case <-release:
		case <-time.After(5 * time.Second):
			return nil, errors.New("barrier timeout: children did not run concurrently")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return done("report for " + promptOf(req)), nil
	}}
	cfg := baseConfig(p)
	cfg.MaxParallel = n
	tool := New(cfg)
	rep, err := tool.Run(context.Background(), newEnv(t), ModeExplore,
		[]Task{{"one", "p1"}, {"two", "p2"}, {"three", "p3"}})
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range rep.Results {
		if r.Err != nil {
			t.Fatalf("task %d: %v", i, r.Err)
		}
		if want := fmt.Sprintf("report for p%d", i+1); r.Report != want {
			t.Fatalf("task %d report %q, want %q (results must stay in task order)", i, r.Report, want)
		}
	}
}

func TestMaxParallelIsRespected(t *testing.T) {
	var active, peak atomic.Int32
	p := &fake{handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
		return done("ok"), nil
	}}
	cfg := baseConfig(p)
	cfg.MaxParallel = 2
	var tasks []Task
	for i := range 6 {
		tasks = append(tasks, Task{fmt.Sprintf("t%d", i), fmt.Sprintf("p%d", i)})
	}
	rep, err := New(cfg).Run(context.Background(), newEnv(t), ModeExplore, tasks)
	if err != nil || rep.Failed() != 0 {
		t.Fatalf("run: %v, failed %d", err, rep.Failed())
	}
	if got := peak.Load(); got > 2 {
		t.Fatalf("peak concurrency %d exceeds MaxParallel 2", got)
	}
}

func TestExploreChildrenAreReadOnly(t *testing.T) {
	env := newEnv(t)
	if err := os.WriteFile(filepath.Join(env.Root, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := scripted(map[string][]*llm.Response{
		"look": {
			reply(llm.StopToolUse,
				toolUse("t1", "read_file", map[string]any{"path": "a.txt"}),
				toolUse("t2", "write_file", map[string]any{"path": "b.txt", "content": "x"}),
				toolUse("t3", "bash", map[string]any{"command": "touch c.txt"})),
			done("a.txt:1 says hello"),
		},
	})
	cfg := baseConfig(p)
	approvals := 0
	cfg.Approve = func(context.Context, *tools.Call, string) permission.Decision {
		approvals++
		return permission.DecisionAllowOnce
	}
	tool := New(cfg)
	call, err := tool.Prepare(env, taskInput("", Task{"inspect", "look"}))
	if err != nil {
		t.Fatal(err)
	}
	if call.Kind != tools.KindRead {
		t.Fatalf("explore call kind %v, want read", call.Kind)
	}
	if v, _ := permission.NewGate(permission.ModePlan, nil).Check(call); v != permission.Allow {
		t.Fatal("explore launch must be allowed even in plan mode")
	}
	out, err := call.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.txt:1 says hello") {
		t.Fatalf("report missing: %s", out)
	}
	for _, f := range []string{"b.txt", "c.txt"} {
		if _, err := os.Stat(filepath.Join(env.Root, f)); err == nil {
			t.Fatalf("explore child created %s", f)
		}
	}
	if approvals != 0 {
		t.Fatal("explore children must never ask for approval")
	}
	reqs := p.reqs()
	var names []string
	for _, s := range reqs[0].Tools {
		names = append(names, s.Name)
	}
	if !slices.Equal(names, readOnlyTools) {
		t.Fatalf("explore tools %v, want %v", names, readOnlyTools)
	}
	results := reqs[1].Messages[2].Blocks
	if results[0].IsError || !strings.Contains(results[0].Text, "hello") {
		t.Fatalf("read should succeed: %+v", results[0])
	}
	if !results[1].IsError || !results[2].IsError {
		t.Fatalf("write and bash must fail: %+v", results[1:])
	}
}

// writeAsRead is a misbehaving tool that claims a read-only name but is a
// write; it must never reach an explore child, and the plan gate is the
// second line of defence.
type writeAsRead struct{ tools.WriteFile }

func (writeAsRead) Spec() llm.ToolSpec { s := (tools.WriteFile{}).Spec(); s.Name = "grep"; return s }

func TestExploreGateIsPlanMode(t *testing.T) {
	env := newEnv(t)
	tool := New(baseConfig(scripted(nil)))
	parent := tools.NewRegistry(tools.ReadFile{}, writeAsRead{})
	tool.cfg.Tools = parent
	b := &batch{tool: tool, env: env, mode: ModeExplore, settings: tool.settings(), total: 1}
	child, err := b.newChild(0)
	if err != nil {
		t.Fatal(err)
	}
	if child.Gate.Mode() != permission.ModePlan {
		t.Fatalf("explore gate mode %s, want plan", child.Gate.Mode())
	}
	g, _ := child.Tools.Get("grep")
	if g.Kind() != tools.KindRead {
		t.Fatal("a non-read tool leaked into the explore registry")
	}
	w, err := (tools.WriteFile{}).Prepare(child.Env, json.RawMessage(`{"path":"x","content":"y"}`))
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := child.Gate.Check(w); v != permission.Deny {
		t.Fatal("explore gate must deny writes")
	}
	if child.Env == env {
		t.Fatal("child must get its own environment")
	}
}

func TestGeneralModeDelegatesApprovals(t *testing.T) {
	env := newEnv(t)
	p := scripted(map[string][]*llm.Response{
		"make it": {
			reply(llm.StopToolUse, toolUse("t1", "write_file", map[string]any{"path": "out.txt", "content": "made\n"})),
			done("Created out.txt:1."),
		},
	})
	cfg := baseConfig(p)
	var mu sync.Mutex
	var reasons []string
	cfg.Approve = func(_ context.Context, c *tools.Call, reason string) permission.Decision {
		mu.Lock()
		defer mu.Unlock()
		reasons = append(reasons, c.Summary+" | "+reason)
		return permission.DecisionAllowOnce
	}
	tool := New(cfg)
	// The parent's registry includes the task tool itself, as it will in the
	// real wiring; children must not inherit it.
	tool.cfg.Tools = tools.NewRegistry(tools.ReadFile{}, tools.WriteFile{}, tools.Bash{}, tool)

	call, err := tool.Prepare(env, taskInput("general", Task{"writer", "make it"}))
	if err != nil {
		t.Fatal(err)
	}
	if call.Kind != tools.KindExec || call.Command != "task general" {
		t.Fatalf("general call kind %v command %q", call.Kind, call.Command)
	}
	if v, _ := permission.NewGate(permission.ModeAsk, nil).Check(call); v != permission.Ask {
		t.Fatal("general launch must need approval in ask mode")
	}
	if v, _ := permission.NewGate(permission.ModePlan, nil).Check(call); v != permission.Deny {
		t.Fatal("general launch must be refused in plan mode")
	}
	if !strings.Contains(call.Preview, "writer") || !strings.Contains(call.Preview, "make it") {
		t.Fatalf("preview should list tasks: %q", call.Preview)
	}
	out, err := call.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(env.Root, "out.txt")); string(got) != "made\n" {
		t.Fatalf("file not written: %q", got)
	}
	if len(reasons) != 1 || !strings.Contains(reasons[0], "out.txt |") || !strings.Contains(reasons[0], "sub-agent 1: writer") {
		t.Fatalf("approval not delegated with sub-agent label: %v", reasons)
	}
	if !strings.Contains(out, "Created out.txt:1.") {
		t.Fatalf("output: %s", out)
	}
	for _, s := range p.reqs()[0].Tools {
		if s.Name == Name {
			t.Fatal("children must not get the task tool")
		}
	}
}

func TestGeneralModeWithoutApproverDeclines(t *testing.T) {
	env := newEnv(t)
	p := scripted(map[string][]*llm.Response{
		"make it": {
			reply(llm.StopToolUse, text("Writing."), toolUse("t1", "write_file", map[string]any{"path": "out.txt", "content": "x"})),
		},
		"other": {done("fine")},
	})
	rep, err := New(baseConfig(p)).Run(context.Background(), env, ModeGeneral, []Task{{"writer", "make it"}, {"other", "other"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(env.Root, "out.txt")); err == nil {
		t.Fatal("declined write must not run")
	}
	r := rep.Results[0]
	if r.Err == nil || !strings.Contains(r.Err.Error(), "declined") || r.Report != "Writing." {
		t.Fatalf("want declined failure with partial report, got %v %q", r.Err, r.Report)
	}
	if rep.Results[1].Err != nil {
		t.Fatalf("sibling must be unaffected: %v", rep.Results[1].Err)
	}
}

func TestParentModeAppliesToGeneralChildren(t *testing.T) {
	env := newEnv(t)
	p := scripted(map[string][]*llm.Response{
		"make it": {
			reply(llm.StopToolUse, toolUse("t1", "write_file", map[string]any{"path": "y.txt", "content": "y"})),
			done("done"),
		},
	})
	cfg := baseConfig(p)
	cfg.ParentMode = func() permission.Mode { return permission.ModeYolo }
	cfg.Approve = func(context.Context, *tools.Call, string) permission.Decision {
		t.Error("yolo parent must not be asked")
		return permission.DecisionDeny
	}
	rep, err := New(cfg).Run(context.Background(), env, ModeGeneral, []Task{{"w", "make it"}})
	if err != nil || rep.Failed() != 0 {
		t.Fatalf("%v %+v", err, rep.Results)
	}
	if _, err := os.Stat(filepath.Join(env.Root, "y.txt")); err != nil {
		t.Fatal("write should have run")
	}
}

func TestFailuresAreIsolated(t *testing.T) {
	p := &fake{handle: func(_ context.Context, req llm.Request) (*llm.Response, error) {
		switch promptOf(req) {
		case "boom":
			return nil, errors.New("provider exploded")
		case "panic":
			panic("bad provider")
		}
		return done("all good"), nil
	}}
	tool := New(baseConfig(p))
	env := newEnv(t)
	call, err := tool.Prepare(env, taskInput("", Task{"bad", "boom"}, Task{"crash", "panic"}, Task{"good", "fine"}))
	if err != nil {
		t.Fatal(err)
	}
	out, err := call.Run(context.Background())
	if err != nil {
		t.Fatalf("one success means no tool error: %v", err)
	}
	for _, want := range []string{"1 succeeded, 2 failed", "## bad\n\n**Failed:** provider exploded", "## crash\n\n**Failed:** sub-agent crashed: bad provider", "## good\n\nall good"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}

	call, _ = tool.Prepare(env, taskInput("", Task{"bad", "boom"}))
	out, err = call.Run(context.Background())
	if err == nil || !strings.Contains(out, "provider exploded") {
		t.Fatalf("all-failed batch should be a tool error with output: %v %q", err, out)
	}
}

func TestCancellationStopsChildren(t *testing.T) {
	p := &fake{handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	ev := &events{}
	started := make(chan struct{}, 8)
	cfg := baseConfig(p)
	cfg.MaxParallel = 2
	cfg.Progress = func(i int, d, e string) {
		ev.progress(i, d, e)
		if e == EventStarted {
			started <- struct{}{}
		}
	}
	tool := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	type outcome struct {
		rep *Report
		err error
	}
	res := make(chan outcome, 1)
	go func() {
		rep, err := tool.Run(ctx, newEnv(t), ModeExplore, []Task{{"a", "1"}, {"b", "2"}, {"c", "3"}})
		res <- outcome{rep, err}
	}()
	for range 2 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("children did not start")
		}
	}
	cancel()
	select {
	case o := <-res:
		if !errors.Is(o.err, context.Canceled) {
			t.Fatalf("Run error %v, want context.Canceled", o.err)
		}
		for _, r := range o.rep.Results {
			if !errors.Is(r.Err, context.Canceled) {
				t.Fatalf("task %d error %v, want cancelled", r.Index, r.Err)
			}
		}
		if out := o.rep.Format(0); strings.Count(out, "**Cancelled**") != 3 {
			t.Fatalf("output should mark all cancelled:\n%s", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not stop the children")
	}
	terminal := 0
	for _, e := range ev.all() {
		if Terminal(strings.SplitN(e, "|", 3)[2]) {
			terminal++
		}
	}
	if terminal != 3 || ev.race.Load() {
		t.Fatalf("want 3 terminal events and serialized callbacks, got %d (race %v): %v", terminal, ev.race.Load(), ev.all())
	}
}

func TestValidation(t *testing.T) {
	tool := New(baseConfig(scripted(nil)))
	env := newEnv(t)
	nine := make([]Task, 9)
	for i := range nine {
		nine[i] = Task{fmt.Sprintf("d%d", i), "p"}
	}
	cases := map[string]string{
		"empty":            ``,
		"no tasks":         `{"mode":"explore"}`,
		"null tasks":       `{"tasks":null}`,
		"zero tasks":       `{"tasks":[]}`,
		"too many":         string(taskInput("", nine...)),
		"unknown field":    `{"tasks":[{"description":"a","prompt":"b"}],"extra":1}`,
		"unknown task key": `{"tasks":[{"description":"a","prompt":"b","model":"x"}]}`,
		"empty prompt":     `{"tasks":[{"description":"a","prompt":"   "}]}`,
		"no description":   `{"tasks":[{"prompt":"b"}]}`,
		"long description": string(taskInput("", Task{strings.Repeat("x", 101), "p"})),
		"duplicate":        `{"tasks":[{"description":"Same","prompt":"b"},{"description":"same","prompt":"c"}]}`,
		"bad mode":         `{"tasks":[{"description":"a","prompt":"b"}],"mode":"admin"}`,
		"trailing":         `{"tasks":[{"description":"a","prompt":"b"}]} {}`,
		"wrong type":       `{"tasks":"a"}`,
		"huge prompt":      string(taskInput("", Task{"a", strings.Repeat("x", maxPromptBytes+1)})),
	}
	for name, raw := range cases {
		if _, err := tool.Prepare(env, json.RawMessage(raw)); err == nil {
			t.Errorf("%s: expected a validation error", name)
		} else if !strings.HasPrefix(err.Error(), "invalid tool input") {
			t.Errorf("%s: unexpected error %v", name, err)
		}
	}
	if _, err := tool.Prepare(env, taskInput("", Task{"  spaced \n label ", "p"})); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
	if _, err := New(Config{}).Prepare(env, taskInput("", Task{"a", "b"})); err == nil {
		t.Fatal("missing provider must be reported")
	}
	if _, err := New(Config{Provider: scripted(nil)}).Prepare(env, taskInput("", Task{"a", "b"})); err == nil {
		t.Fatal("missing model must be reported")
	}
}

func TestOutputAndUsageAggregation(t *testing.T) {
	env := newEnv(t)
	if err := os.WriteFile(filepath.Join(env.Root, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cached := reply(llm.StopEndTurn, text("B report"))
	cached.Usage.CacheReadTokens = 1000
	p := scripted(map[string][]*llm.Response{
		"pa": {reply(llm.StopToolUse, toolUse("t1", "read_file", map[string]any{"path": "a.txt"})), done("A report: a.txt:1")},
		"pb": {cached},
	})
	ev := &events{}
	var umu sync.Mutex
	var billed llm.Usage
	var billedCalls int
	cfg := baseConfig(p)
	cfg.Progress = ev.progress
	cfg.OnUsage = func(model string, u llm.Usage) {
		umu.Lock()
		defer umu.Unlock()
		if model != testModel {
			t.Errorf("usage model %q", model)
		}
		billed.Add(u)
		billedCalls++
	}
	tool := New(cfg)
	rep, err := tool.Run(context.Background(), env, ModeExplore, []Task{{"Alpha", "pa"}, {"Beta", "pb"}})
	if err != nil {
		t.Fatal(err)
	}
	want := llm.Usage{InputTokens: 300, OutputTokens: 30, CacheReadTokens: 1000}
	if rep.Usage != want || rep.ModelCalls != 3 || rep.ToolCalls != 1 {
		t.Fatalf("aggregate %+v calls %d tools %d", rep.Usage, rep.ModelCalls, rep.ToolCalls)
	}
	if rep.Results[0].Usage.InputTokens != 200 || rep.Results[1].Usage.CacheReadTokens != 1000 {
		t.Fatalf("per-task usage wrong: %+v", rep.Results)
	}
	if billed != want || billedCalls != 3 {
		t.Fatalf("OnUsage got %+v over %d calls", billed, billedCalls)
	}
	if total, calls := tool.Usage(); total != want || calls != 3 {
		t.Fatalf("Tool.Usage %+v %d", total, calls)
	}
	out := rep.Format(0)
	ia, ib := strings.Index(out, "## Alpha\n\nA report: a.txt:1"), strings.Index(out, "## Beta\n\nB report")
	if ia < 0 || ib < ia {
		t.Fatalf("sections missing or out of order:\n%s", out)
	}
	for _, s := range []string{"2 sub-agents finished", "2 succeeded, 0 failed",
		"1,300 input tokens (1,000 cache read, 0 cache write), 30 output tokens, 3 model calls, 1 tool call."} {
		if !strings.Contains(out, s) {
			t.Fatalf("output missing %q:\n%s", s, out)
		}
	}
	got := ev.all()
	for _, s := range []string{"0|Alpha|Queued", "0|Alpha|Started", "0|Alpha|Read a.txt", "1|Beta|Queued"} {
		if !slices.Contains(got, s) {
			t.Fatalf("progress missing %q: %v", s, got)
		}
	}
	if last := got[len(got)-1]; !Terminal(strings.SplitN(last, "|", 3)[2]) {
		t.Fatalf("last event should be terminal: %q", last)
	}
	if ev.race.Load() {
		t.Fatal("progress callbacks overlapped")
	}
}

func TestSettingsAndSystemPrompt(t *testing.T) {
	p := scripted(map[string][]*llm.Response{"q": {done("r")}})
	cfg := baseConfig(p)
	cfg.Model = "claude-haiku-cheap"
	cfg.Instructions = "Use tabs."
	cfg.MaxTurns = 7
	if _, err := New(cfg).Run(context.Background(), newEnv(t), ModeExplore, []Task{{"d", "q"}}); err != nil {
		t.Fatal(err)
	}
	req := p.reqs()[0]
	if req.Model != "claude-haiku-cheap" || req.MaxTokens != 4096 || req.Effort != "high" || req.ShowThinking {
		t.Fatalf("child settings not derived from parent: %+v", req)
	}
	for _, s := range []string{"sub-agent of GTA", "never ask", "read-only", "path:line", "at most 7 model turns", "Use tabs.", "sub-agent 1 of 1"} {
		if !strings.Contains(req.System, s) {
			t.Fatalf("system prompt missing %q:\n%s", s, req.System)
		}
	}
}

func TestTurnLimit(t *testing.T) {
	looping := &fake{handle: func(_ context.Context, req llm.Request) (*llm.Response, error) {
		blocks := []llm.Block{toolUse(fmt.Sprintf("t%d", turnOf(req)), "list_dir", map[string]any{})}
		if promptOf(req) == "talky" {
			blocks = append([]llm.Block{text(fmt.Sprintf("progress %d", turnOf(req)))}, blocks...)
		}
		return reply(llm.StopToolUse, blocks...), nil
	}}
	cfg := baseConfig(looping)
	cfg.MaxTurns = 2
	rep, err := New(cfg).Run(context.Background(), newEnv(t), ModeExplore, []Task{{"talky", "talky"}, {"silent", "silent"}})
	if err != nil {
		t.Fatal(err)
	}
	talky, silent := rep.Results[0], rep.Results[1]
	if talky.Err != nil || talky.Report != "progress 1" || len(talky.Notes) != 1 || !strings.Contains(talky.Notes[0], "2-turn limit") {
		t.Fatalf("talky: %+v", talky)
	}
	if silent.Err == nil || !strings.Contains(silent.Err.Error(), "turn limit") || silent.ModelCalls != 2 {
		t.Fatalf("silent: %+v", silent)
	}
}

func TestTruncateReport(t *testing.T) {
	var b strings.Builder
	for i := range 2000 {
		fmt.Fprintf(&b, "line %d: héllo wörld ✓\n", i)
	}
	s := b.String()
	out := truncateReport(s, 4000)
	if !utf8.ValidString(out) {
		t.Fatal("truncation split a UTF-8 sequence")
	}
	if len(out) > 4200 || !strings.HasPrefix(out, "line 0:") || !strings.Contains(out, "line 1999:") || !strings.Contains(out, "characters of this report omitted") {
		t.Fatalf("bad truncation (%d bytes): %q…", len(out), out[:200])
	}
	if truncateReport("short", 4000) != "short" {
		t.Fatal("short reports must be untouched")
	}
	rep := &Report{Mode: ModeExplore, Model: testModel, Results: []Result{{Description: "big", Report: s}}}
	if f := rep.Format(1000); len(f) > 2000 {
		t.Fatalf("Format did not apply the limit: %d bytes", len(f))
	}
}

func TestSpecAndHelpers(t *testing.T) {
	tool := New(Config{})
	s := tool.Spec()
	if s.Name != Name || !slices.Equal(s.Required, []string{"tasks"}) || s.Properties["tasks"] == nil || s.Properties["mode"] == nil {
		t.Fatalf("spec: %+v", s)
	}
	if tool.Kind() != tools.KindExec {
		t.Fatal("tool kind should be the most a call can do")
	}
	if fmtInt(1234567) != "1,234,567" || fmtInt(-1000) != "-1,000" || fmtInt(12) != "12" {
		t.Fatal("fmtInt")
	}
	if !Terminal("Done (1 tool call, 2s)") || !Terminal("Failed: x") || !Terminal(EventCancelled) || Terminal("Read a.go") || Terminal(EventStarted) {
		t.Fatal("Terminal")
	}
	if got := summary(ModeExplore, []Task{{"one", "p"}}); got != "Explore: one" {
		t.Fatalf("summary %q", got)
	}
	if got := summary(ModeGeneral, []Task{{"a", "p"}, {"b", "q"}}); got != "Delegate with 2 sub-agents" {
		t.Fatalf("summary %q", got)
	}
}

// A general child runs shell commands, so it must keep the parent's sandbox
// wrapper and policy hooks.
func TestChildKeepsSandboxAndPolicyHooks(t *testing.T) {
	env := newEnv(t)
	wrapped := false
	env.Wrap = func(shell, command string) ([]string, error) { wrapped = true; return nil, errors.New("sandboxed") }
	env.Hidden = func(string) bool { return true }
	env.CheckCommand = func(string) error { return nil }
	tool := New(baseConfig(scripted(nil)))
	b := &batch{tool: tool, env: env, mode: ModeGeneral, settings: tool.settings(), total: 1, gate: permission.NewGate(permission.ModeYolo, nil)}
	child, err := b.newChild(0)
	if err != nil {
		t.Fatal(err)
	}
	if child.Env.Hidden == nil || child.Env.CheckCommand == nil || child.Env.Wrap == nil {
		t.Fatal("child environment lost the policy hooks or the sandbox")
	}
	call, err := (tools.Bash{}).Prepare(child.Env, json.RawMessage(`{"command":"echo hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := call.Run(context.Background()); err == nil || !wrapped {
		t.Fatalf("child bash bypassed the sandbox: err %v", err)
	}
}
