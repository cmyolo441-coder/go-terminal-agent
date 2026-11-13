package autopilot

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
)

func skipWithoutShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("these tests use POSIX shell commands")
	}
}

func TestRunTestsPassAndFail(t *testing.T) {
	skipWithoutShell(t)
	root := t.TempDir()
	ok, err := RunTests(context.Background(), root, TestCommand{Command: "echo all good"}, RunOptions{})
	if err != nil || !ok.Passed() || len(ok.Failures) != 0 || !strings.HasPrefix(ok.Headline(), "PASS echo all good (exit 0") {
		t.Fatalf("pass: %+v %v", ok, err)
	}
	if rep := ok.Report(0); !strings.Contains(rep, "all good") {
		t.Fatalf("pass report should show the output tail: %q", rep)
	}

	bad, err := RunTests(context.Background(), root, TestCommand{Command: "echo something broke >&2; exit 3"}, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if bad.Passed() || bad.ExitCode != 3 || len(bad.Failures) != 1 || bad.Failures[0].Kind != KindError ||
		!strings.Contains(bad.Failures[0].Message, "something broke") {
		t.Fatalf("an unparseable failure must be synthesized from the output: %+v", bad)
	}
	if !strings.Contains(bad.Report(0), "FAIL") {
		t.Fatalf("report: %s", bad.Report(0))
	}
}

func TestRunTestsSetsNonInteractiveEnv(t *testing.T) {
	skipWithoutShell(t)
	run, err := RunTests(context.Background(), t.TempDir(), TestCommand{Command: `echo "ci=$CI color=$NO_COLOR extra=$EXTRA"`}, RunOptions{Env: []string{"EXTRA=yes"}})
	if err != nil || !strings.Contains(run.Output, "ci=true color=1 extra=yes") {
		t.Fatalf("env not set: %q %v", run.Output, err)
	}
}

func TestRunTestsTimeoutKillsProcessGroup(t *testing.T) {
	skipWithoutShell(t)
	start := time.Now()
	// The background sleep holds the output pipe; the whole group must die.
	run, err := RunTests(context.Background(), t.TempDir(), TestCommand{Command: "echo started; sleep 30 & sleep 30"}, RunOptions{Timeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("timeout did not stop the command promptly: %s", took)
	}
	if !run.TimedOut || run.ExitCode != -1 || run.Passed() {
		t.Fatalf("run %+v", run)
	}
	last := run.Failures[len(run.Failures)-1]
	if last.Kind != KindTimeout || !strings.Contains(last.Message, "started") {
		t.Fatalf("timeout failure missing: %+v", run.Failures)
	}
	if !strings.HasPrefix(run.Headline(), "TIMEOUT") {
		t.Fatalf("headline %q", run.Headline())
	}
}

func TestRunTestsCancellation(t *testing.T) {
	skipWithoutShell(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	_, err := RunTests(ctx, t.TempDir(), TestCommand{Command: "sleep 30"}, RunOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestRunTestsNotRunnable(t *testing.T) {
	skipWithoutShell(t)
	_, err := RunTests(context.Background(), t.TempDir(), TestCommand{Command: "gta-no-such-test-runner-xyz ./..."}, RunOptions{})
	if !errors.Is(err, ErrTestCommandNotRunnable) {
		t.Fatalf("want ErrTestCommandNotRunnable, got %v", err)
	}
	if _, err := RunTests(context.Background(), t.TempDir(), TestCommand{Command: "  "}, RunOptions{}); !errors.Is(err, ErrNoTestCommand) {
		t.Fatalf("empty command: %v", err)
	}
}

func TestRunTestsWrapAndDir(t *testing.T) {
	skipWithoutShell(t)
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"sub/marker.txt": "here"})
	var gotShell, gotCmd string
	wrap := func(shell, command string) ([]string, error) {
		gotShell, gotCmd = shell, command
		return []string{"sh", "-c", "cat marker.txt; echo ' wrapped'"}, nil
	}
	run, err := RunTests(context.Background(), root, TestCommand{Command: "go test ./...", Dir: "sub"}, RunOptions{Wrap: wrap})
	if err != nil || !strings.Contains(run.Output, "here wrapped") || gotCmd != "go test ./..." || gotShell == "" {
		t.Fatalf("wrap/dir not applied: %q %v shell=%q cmd=%q", run.Output, err, gotShell, gotCmd)
	}
	refuse := func(string, string) ([]string, error) { return nil, errors.New("blocked by policy") }
	if _, err := RunTests(context.Background(), root, TestCommand{Command: "go test ./..."}, RunOptions{Wrap: refuse}); err == nil || !strings.Contains(err.Error(), "blocked by policy") {
		t.Fatalf("sandbox refusal must surface: %v", err)
	}
}

func TestRunTestsResolvesGoPackagePaths(t *testing.T) {
	skipWithoutShell(t)
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"go.mod":            "module example.com/demo\n\ngo 1.21\n",
		"sub/s_test.go":     "package sub\n",
		"script/fake-go.sh": "printf -- '--- FAIL: TestSub (0.00s)\\n    s_test.go:5: sub fails\\nFAIL\\nFAIL\\texample.com/demo/sub\\t0.002s\\n--- FAIL: TestRoot (0.00s)\\n    r_test.go:2: root fails\\nFAIL\\nFAIL\\texample.com/demo\\t0.002s\\n'; exit 1\n",
	})
	run, err := RunTests(context.Background(), root, TestCommand{Command: "sh script/fake-go.sh", Framework: FrameworkGo}, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Failures) != 2 || run.Failures[0].File != "sub/s_test.go" || run.Failures[1].File != "r_test.go" {
		t.Fatalf("paths not resolved: %+v", run.Failures)
	}
}

func TestCapBufferKeepsHeadAndTail(t *testing.T) {
	b := newCapBuffer(1000)
	b.Write([]byte("HEAD-START\n"))
	for i := 0; i < 5000; i++ {
		b.Write([]byte("middle line of output\n"))
	}
	b.Write([]byte("TAIL-END\n"))
	s := b.String()
	if !strings.HasPrefix(s, "HEAD-START") || !strings.HasSuffix(s, "TAIL-END\n") || !strings.Contains(s, "bytes of output omitted") {
		t.Fatalf("cap buffer lost head or tail: %q…%q", s[:40], s[len(s)-40:])
	}
	if len(s) > 1200 {
		t.Fatalf("cap buffer holds %d bytes, cap 1000", len(s))
	}
	small := newCapBuffer(1000)
	small.Write([]byte("short"))
	if small.String() != "short" {
		t.Fatalf("small output altered: %q", small.String())
	}
}
