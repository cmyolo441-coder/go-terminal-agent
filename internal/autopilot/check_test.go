package autopilot

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/permission"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

var errDenied = errors.New("command is denied by policy")

func denyAll(string) error { return errDenied }

func TestRunTestsCheckRefusesCommand(t *testing.T) {
	root := t.TempDir()
	var seen string
	_, err := RunTests(context.Background(), root, TestCommand{Command: "echo ran > marker"}, RunOptions{
		Check: func(c string) error { seen = c; return errDenied },
	})
	if !errors.Is(err, errDenied) || seen != "echo ran > marker" {
		t.Fatalf("err %v, checked %q", err, seen)
	}
	if _, err := os.Stat(filepath.Join(root, "marker")); err == nil {
		t.Fatal("a refused test command ran")
	}
}

func TestVerifyToolHonoursCheckCommand(t *testing.T) {
	root := t.TempDir()
	env, err := tools.NewEnv(root)
	if err != nil {
		t.Fatal(err)
	}
	env.CheckCommand = denyAll
	vt := NewVerifyTool(VerifyToolConfig{Command: &TestCommand{Command: "echo ran > marker"}})
	call, err := vt.Prepare(env, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := call.Run(context.Background()); !errors.Is(err, errDenied) {
		t.Fatalf("verify ran despite the check: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "marker")); err == nil {
		t.Fatal("a refused test command ran")
	}
}

// /fixtests runs the test command directly, so it must be vetted too.
func TestFixTestsHonoursCheckCommand(t *testing.T) {
	root := t.TempDir()
	a := newMainAgent(t, newFake(), root, permission.ModeAutoEdit)
	a.Env.CheckCommand = denyAll
	_, err := FixTests(context.Background(), FixConfig{Agent: a, Sink: &uiSink{}, Command: &TestCommand{Command: "touch marker"}})
	if err == nil || !strings.Contains(err.Error(), "denied by policy") {
		t.Fatalf("FixTests err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "marker")); err == nil {
		t.Fatal("a refused test command ran")
	}
}
