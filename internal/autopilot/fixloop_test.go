package autopilot

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permission"
)

// brokenModule writes a Go module whose Add subtracts, so TestAdd fails
// and TestZero passes.
func brokenModule(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"go.mod":  "module example.com/fixme\n\ngo 1.21\n",
		"calc.go": "package calc\n\n// Add returns a + b.\nfunc Add(a, b int) int {\n\treturn a - b\n}\n",
		"calc_test.go": "package calc\n\nimport \"testing\"\n\n" +
			"func TestAdd(t *testing.T) {\n\tif got := Add(2, 3); got != 5 {\n\t\tt.Errorf(\"Add(2, 3) = %d, want 5\", got)\n\t}\n}\n\n" +
			"func TestZero(t *testing.T) {\n\tif Add(0, 0) != 0 {\n\t\tt.Fatal(\"zero\")\n\t}\n}\n",
	})
	return root
}

// fixSteps is the scripted agent turn that repairs brokenModule.
func fixSteps() []step {
	return []step{
		call(use("read_file", map[string]any{"path": "calc.go"})),
		call(use("edit_file", map[string]any{"path": "calc.go", "old_string": "return a - b", "new_string": "return a + b"})),
		say("Root cause: Add subtracted. Fixed it to add."),
	}
}

func TestFixTestsEndToEnd(t *testing.T) {
	root := brokenModule(t)
	p := newFake().on(roleMain, fixSteps()...)
	a := newMainAgent(t, p, root, permission.ModeAutoEdit)
	ui := &uiSink{}
	var events []string
	rep, err := FixTests(context.Background(), FixConfig{
		Agent: a,
		Sink:  ui,
		Callbacks: FixCallbacks{
			TestStart: func(it int, cmd TestCommand) { events = append(events, "test:"+cmd.Command) },
			TestDone:  func(it int, r *TestRun) { events = append(events, "done:"+strings.Fields(r.Headline())[0]) },
			FixStart:  func(it int, fs []Failure) { events = append(events, "fix:"+fs[0].Test) },
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Outcome != FixGreen || rep.Iterations != 1 || len(rep.Runs) != 2 {
		t.Fatalf("report:\n%s", rep.Summary())
	}
	if rep.Command.Command != "go test ./..." || rep.Command.Framework != FrameworkGo {
		t.Fatalf("detected command %+v", rep.Command)
	}
	if len(rep.Initial) != 1 || rep.Initial[0].Test != "TestAdd" || rep.Initial[0].File != "calc_test.go" || rep.Initial[0].Line != 7 {
		t.Fatalf("initial failures %+v", rep.Initial)
	}
	if len(rep.Fixed) != 1 || rep.Fixed[0].Test != "TestAdd" || len(rep.Remaining) != 0 || len(rep.Introduced) != 0 {
		t.Fatalf("fixed %+v remaining %+v", rep.Fixed, rep.Remaining)
	}
	want := []string{"test:go test ./...", "done:FAIL", "fix:TestAdd", "test:go test ./...", "done:PASS"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("events %v, want %v", events, want)
	}
	got, _ := os.ReadFile(filepath.Join(root, "calc.go"))
	if !strings.Contains(string(got), "return a + b") {
		t.Fatalf("fix not applied:\n%s", got)
	}
	first := lastUserText(p.reqs(roleMain)[0])
	for _, s := range []string{"The test suite is failing", "TestAdd", "calc_test.go:7", "Add(2, 3) = -1, want 5", "Do not delete, skip or weaken tests"} {
		if !strings.Contains(first, s) {
			t.Errorf("fix prompt lacks %q:\n%s", s, first)
		}
	}
	if rep.ModelCalls != 3 || rep.Usage.InputTokens != 300 || ui.usage != 3 {
		t.Errorf("usage not tracked: calls %d usage %+v ui %d", rep.ModelCalls, rep.Usage, ui.usage)
	}
	if s := rep.Summary(); !strings.Contains(s, "Tests pass after 1 fix iteration") || !strings.Contains(s, "✓ TestAdd") {
		t.Errorf("summary:\n%s", s)
	}
}

func TestFixTestsAlreadyGreen(t *testing.T) {
	root := brokenModule(t)
	if err := os.WriteFile(filepath.Join(root, "calc.go"), []byte("package calc\n\nfunc Add(a, b int) int { return a + b }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := newFake()
	rep, err := FixTests(context.Background(), FixConfig{Agent: newMainAgent(t, p, root, permission.ModeAutoEdit), Sink: &uiSink{}})
	if err != nil || rep.Outcome != FixGreen || rep.Iterations != 0 || len(p.reqs(roleMain)) != 0 {
		t.Fatalf("%v %+v", err, rep)
	}
	if !strings.HasPrefix(rep.Summary(), "Tests already pass") {
		t.Errorf("summary: %s", rep.Summary())
	}
}

func TestFixTestsStopsWithoutProgress(t *testing.T) {
	root := brokenModule(t)
	// The agent never changes anything: the failing set stays the same.
	p := newFake().on(roleMain, say("I looked but changed nothing."), say("Still nothing."), say("unreachable"))
	rep, err := FixTests(context.Background(), FixConfig{Agent: newMainAgent(t, p, root, permission.ModeAutoEdit), Sink: &uiSink{}, MaxIterations: 10})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Outcome != FixNoProgress || rep.Iterations != 2 || len(rep.Runs) != 3 || p.remaining(roleMain) != 1 {
		t.Fatalf("want no-progress after 2 iterations:\n%s", rep.Summary())
	}
	if len(rep.Remaining) != 1 || rep.Remaining[0].Test != "TestAdd" || len(rep.Fixed) != 0 {
		t.Fatalf("remaining %+v fixed %+v", rep.Remaining, rep.Fixed)
	}
	second := lastUserText(p.reqs(roleMain)[1])
	if !strings.Contains(second, "0 fixed, 0 newly failing") {
		t.Errorf("follow-up prompt should report progress:\n%s", second)
	}
}

// churnScript fails with a different test on every run, so the loop never
// stalls and must stop at the iteration cap.
const churnScript = `n=$(cat count 2>/dev/null || echo 0); n=$((n+1)); echo $n > count
printf -- '--- FAIL: TestRun%s (0.00s)\n    x_test.go:1: failure number %s\nFAIL\nFAIL\texample.com/x\t0.01s\n' $n $n
exit 1
`

func TestFixTestsStopsAtMaxIterations(t *testing.T) {
	skipWithoutShell(t)
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"churn.sh": churnScript})
	p := newFake().on(roleMain, say("try 1"), say("try 2"), say("try 3"))
	rep, err := FixTests(context.Background(), FixConfig{
		Agent:         newMainAgent(t, p, root, permission.ModeAutoEdit),
		Sink:          &uiSink{},
		Command:       &TestCommand{Command: "sh churn.sh"},
		MaxIterations: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Command.Framework != FrameworkAuto {
		t.Errorf("framework %q", rep.Command.Framework)
	}
	if rep.Outcome != FixMaxIterations || rep.Iterations != 3 || len(rep.Runs) != 4 {
		t.Fatalf("want max-iterations:\n%s", rep.Summary())
	}
	if len(rep.Fixed) != 3 || len(rep.Remaining) != 1 || rep.Remaining[0].Test != "TestRun4" || len(rep.Introduced) != 1 {
		t.Fatalf("fixed %+v remaining %+v introduced %+v", rep.Fixed, rep.Remaining, rep.Introduced)
	}
	if second := lastUserText(p.reqs(roleMain)[1]); !strings.Contains(second, "1 fixed, 1 newly failing") || !strings.Contains(second, "TestRun2") {
		t.Errorf("follow-up prompt:\n%s", second)
	}
	if s := rep.Summary(); !strings.Contains(s, "newly failing: 1") || !strings.Contains(s, "✗ TestRun4") {
		t.Errorf("summary:\n%s", s)
	}
}

func TestFixTestsCancellation(t *testing.T) {
	root := brokenModule(t)
	ctx, cancel := context.WithCancel(context.Background())
	p := newFake().on(roleMain, func(ctx2 context.Context, _ llm.Request) (*llm.Response, error) {
		cancel()
		return nil, ctx2.Err()
	})
	rep, err := FixTests(ctx, FixConfig{Agent: newMainAgent(t, p, root, permission.ModeAutoEdit), Sink: &uiSink{}})
	if !errors.Is(err, context.Canceled) || rep == nil || rep.Outcome != FixCancelled || rep.Iterations != 1 {
		t.Fatalf("err %v report %+v", err, rep)
	}
}

func TestFixTestsStopsWhenUserDeclines(t *testing.T) {
	root := brokenModule(t)
	p := newFake().on(roleMain,
		call(use("read_file", map[string]any{"path": "calc.go"})),
		call(use("edit_file", map[string]any{"path": "calc.go", "old_string": "return a - b", "new_string": "return a + b"})),
		say("unreachable: the turn ends on decline"),
	)
	ui := &uiSink{decision: permission.DecisionDeny}
	rep, err := FixTests(context.Background(), FixConfig{Agent: newMainAgent(t, p, root, permission.ModeAsk), Sink: ui})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Outcome != FixStopped || !strings.Contains(rep.Declined, "calc.go") || len(rep.Runs) != 1 || len(ui.asked) != 1 {
		t.Fatalf("report %+v asked %v", rep, ui.asked)
	}
}

func TestFixTestsErrors(t *testing.T) {
	skipWithoutShell(t)
	empty := t.TempDir()
	a := newMainAgent(t, newFake(), empty, permission.ModeAutoEdit)
	if _, err := FixTests(context.Background(), FixConfig{Agent: a, Sink: &uiSink{}}); !errors.Is(err, ErrNoTestCommand) {
		t.Fatalf("want ErrNoTestCommand, got %v", err)
	}
	rep, err := FixTests(context.Background(), FixConfig{Agent: a, Sink: &uiSink{}, Command: &TestCommand{Command: "gta-no-such-runner-xyz"}})
	if !errors.Is(err, ErrTestCommandNotRunnable) || rep.Outcome != FixFailed {
		t.Fatalf("want ErrTestCommandNotRunnable, got %v %+v", err, rep)
	}
	if _, err := FixTests(context.Background(), FixConfig{Sink: &uiSink{}}); err == nil {
		t.Fatal("missing agent must fail")
	}
	if _, err := FixTests(context.Background(), FixConfig{Agent: a}); err == nil {
		t.Fatal("missing sink must fail")
	}
	if _, err := FixTests(context.Background(), FixConfig{Agent: a, Sink: &uiSink{}, MaxIterations: -1}); err == nil {
		t.Fatal("negative limits must fail")
	}
	// A provider error ends the loop with an error, not a hang.
	root := brokenModule(t)
	p := newFake().on(roleMain, func(context.Context, llm.Request) (*llm.Response, error) { return nil, errors.New("overloaded") })
	rep, err = FixTests(context.Background(), FixConfig{Agent: newMainAgent(t, p, root, permission.ModeAutoEdit), Sink: &uiSink{}})
	if err == nil || !strings.Contains(err.Error(), "overloaded") || rep.Outcome != FixFailed {
		t.Fatalf("provider error: %v %+v", err, rep)
	}
}
