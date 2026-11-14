package workflow

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestShellExecutor(t *testing.T) {
	var mu sync.Mutex
	var got []ShellCommand
	runner := func(_ context.Context, c ShellCommand) (ShellResult, error) {
		mu.Lock()
		got = append(got, c)
		mu.Unlock()
		if strings.Contains(c.Command, "false") {
			return ShellResult{Stderr: "it broke", ExitCode: 2}, nil
		}
		return ShellResult{Stdout: "hello " + c.Env["WHO"]}, nil
	}
	reg := NewBuiltinRegistry(BuiltinOptions{Shell: runner})
	spec := mustSpec(t, `{"inputs": {"who": {"default": "world"}}, "steps": [
		{"id": "hi", "type": "shell", "inputs": {"command": "echo hi", "dir": "/tmp", "env": {"WHO": "${inputs.who}"}, "stdin": "in"}},
		{"id": "soft", "type": "shell", "inputs": {"command": "false", "allow_failure": true}},
		{"id": "hard", "type": "shell", "depends_on": ["hi", "soft"], "inputs": {"command": "false"}}]}`)
	res, err := newEngine(t, reg, nil).Run(context.Background(), spec, nil)
	if !errors.Is(err, ErrRunFailed) || !strings.Contains(err.Error(), "command exited with status 2: it broke") {
		t.Fatalf("err = %v", err)
	}
	hi, _ := res.Step("hi")
	if hi.Outputs["stdout"] != "hello world" || hi.Outputs["exit_code"] != 0.0 {
		t.Fatalf("hi outputs = %v", hi.Outputs)
	}
	if soft, _ := res.Step("soft"); soft.Status != StepSucceeded || soft.Outputs["exit_code"] != 2.0 {
		t.Fatalf("soft = %+v", soft)
	}
	var c ShellCommand
	for _, g := range got {
		if g.Command == "echo hi" {
			c = g
		}
	}
	if c.Dir != "/tmp" || c.Stdin != "in" || c.IdempotencyKey == "" {
		t.Fatalf("command = %+v", c)
	}
}

func TestExecShellRunner(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	res, err := ExecShellRunner(context.Background(), ShellCommand{Command: `read x; echo "$x-$V"; echo err >&2; exit 3`, Env: map[string]string{"V": "v"}, Stdin: "in\n"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "in-v\n" || res.Stderr != "err\n" || res.ExitCode != 3 {
		t.Fatalf("res = %+v", res)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := ExecShellRunner(ctx, ShellCommand{Command: "sleep 10"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled command: %v", err)
	}
}

func TestAgentExecutor(t *testing.T) {
	var req AgentRequest
	reg := NewBuiltinRegistry(BuiltinOptions{Agent: func(_ context.Context, r AgentRequest) (AgentResult, error) {
		req = r
		return AgentResult{Report: "done: " + r.Prompt, Outputs: map[string]any{"files": []string{"a.go"}}}, nil
	}})
	spec := mustSpec(t, `{"steps": [
		{"id": "plan", "type": "agent", "inputs": {"prompt": "Plan the change", "mode": "explore", "context": {"k": 1}}},
		{"id": "impl", "type": "noop", "depends_on": ["plan"], "inputs": {"r": "${steps.plan.outputs.report}", "f": "${steps.plan.outputs.files[0]}"}}]}`)
	res, err := newEngine(t, reg, nil).Run(context.Background(), spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.Mode != "explore" || req.Description != "plan" || !reflect.DeepEqual(req.Context, map[string]any{"k": 1.0}) || req.StepID != "plan" {
		t.Fatalf("request = %+v", req)
	}
	impl, _ := res.Step("impl")
	if impl.Outputs["r"] != "done: Plan the change" || impl.Outputs["f"] != "a.go" {
		t.Fatalf("impl outputs = %v", impl.Outputs)
	}
}

func TestIdempotentWrapper(t *testing.T) {
	calls := 0
	ex := Idempotent(ExecutorFunc(func(context.Context, StepContext) (Outputs, error) {
		calls++
		return Outputs{"n": calls}, nil
	}), NewMemoryIdempotencyStore())
	for range 3 {
		out, err := ex.Execute(context.Background(), StepContext{IdempotencyKey: "k"})
		if err != nil || out["n"] != 1.0 {
			t.Fatalf("out = %v, err = %v", out, err)
		}
	}
	if calls != 1 {
		t.Fatalf("executed %d times", calls)
	}
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	if err := r.Register("Bad Name", NoopExecutor{}); err == nil {
		t.Fatal("accepted invalid name")
	}
	if err := r.Register("x", nil); err == nil {
		t.Fatal("accepted nil executor")
	}
	r.MustRegister("x", NoopExecutor{})
	if err := r.Register("x", NoopExecutor{}); err == nil {
		t.Fatal("accepted duplicate")
	}
	if _, err := NewEngine(Options{}); err == nil {
		t.Fatal("engine without registry")
	}
	if got := NewBuiltinRegistry(BuiltinOptions{}).Names(); !reflect.DeepEqual(got, []string{"fail", "noop", "sleep"}) {
		t.Fatalf("names = %q", got)
	}
}

func TestBackoffFullJitter(t *testing.T) {
	p := compileRetry(&RetrySpec{InitialBackoff: Duration(time.Second), MaxBackoff: Duration(5 * time.Second)}, func(string, string) {})
	for attempt, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 4: 5 * time.Second, 60: 5 * time.Second} {
		if got := p.backoff(attempt, func() float64 { return 1 }); got != want {
			t.Errorf("attempt %d ceiling = %v, want %v", attempt, got, want)
		}
		if got := p.backoff(attempt, func() float64 { return 0.5 }); got != want/2 {
			t.Errorf("attempt %d half = %v, want %v", attempt, got, want/2)
		}
		if got := p.backoff(attempt, func() float64 { return 0 }); got != 0 {
			t.Errorf("attempt %d floor = %v", attempt, got)
		}
	}
	if p.maxAttempts != DefaultMaxAttempts || !p.shouldRetry(1, ClassTransient) || p.shouldRetry(1, ClassError) || p.shouldRetry(3, ClassTransient) {
		t.Fatalf("policy = %+v", p)
	}
}

func TestFakeClock(t *testing.T) {
	c := NewFakeClock(time.Unix(0, 0))
	a, b := c.NewTimer(2*time.Second), c.NewTimer(time.Second)
	stopped := c.NewTimer(time.Second)
	if !stopped.Stop() || stopped.Stop() {
		t.Fatal("Stop semantics")
	}
	if got := c.Pending(); !reflect.DeepEqual(got, []time.Duration{time.Second, 2 * time.Second}) {
		t.Fatalf("pending = %v", got)
	}
	c.Advance(time.Second)
	select {
	case <-b.C():
	default:
		t.Fatal("1s timer did not fire")
	}
	select {
	case <-a.C():
		t.Fatal("2s timer fired early")
	default:
	}
	c.Advance(time.Second)
	if got := <-a.C(); !got.Equal(time.Unix(2, 0)) {
		t.Fatalf("fired at %v", got)
	}
	select {
	case <-c.NewTimer(0).C():
	default:
		t.Fatal("zero timer did not fire immediately")
	}
}
