package permission

import (
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

func TestModes(t *testing.T) {
	read := &tools.Call{Kind: tools.KindRead}
	write := &tools.Call{Kind: tools.KindWrite}
	outside := &tools.Call{Kind: tools.KindWrite, OutsideRoot: true}
	cmd := &tools.Call{Kind: tools.KindExec, Command: "go test ./..."}
	rm := &tools.Call{Kind: tools.KindExec, Command: "rm -rf /"}

	cases := []struct {
		mode Mode
		call *tools.Call
		want Verdict
	}{
		{ModeAsk, read, Allow}, {ModeAsk, write, Ask}, {ModeAsk, cmd, Ask},
		{ModeAutoEdit, write, Allow}, {ModeAutoEdit, outside, Ask}, {ModeAutoEdit, cmd, Ask},
		{ModePlan, read, Allow}, {ModePlan, write, Deny}, {ModePlan, cmd, Deny},
		{ModeYolo, write, Allow}, {ModeYolo, outside, Allow}, {ModeYolo, cmd, Allow}, {ModeYolo, rm, Ask},
	}
	for _, c := range cases {
		g := NewGate(c.mode, nil)
		if got, _ := g.Check(c.call); got != c.want {
			t.Errorf("%s %+v: got %v want %v", c.mode, c.call, got, c.want)
		}
	}
}

func TestAllowListAndSessionGrant(t *testing.T) {
	g := NewGate(ModeAsk, []string{"git status"})
	if v, _ := g.Check(&tools.Call{Kind: tools.KindExec, Command: "git status --short"}); v != Allow {
		t.Fatal("allow-listed prefix should run")
	}
	if v, _ := g.Check(&tools.Call{Kind: tools.KindExec, Command: "git status; rm -rf x"}); v != Ask {
		t.Fatal("chained command must ask")
	}
	c := &tools.Call{Kind: tools.KindExec, Command: "go test ./..."}
	g.Grant(c)
	if v, _ := g.Check(&tools.Call{Kind: tools.KindExec, Command: "go test -run X ./pkg"}); v != Allow {
		t.Fatal("granted prefix should run")
	}
	if v, _ := g.Check(&tools.Call{Kind: tools.KindExec, Command: "go build ./..."}); v != Ask {
		t.Fatal("other subcommand should ask")
	}
	g.Grant(&tools.Call{Kind: tools.KindWrite})
	if v, _ := g.Check(&tools.Call{Kind: tools.KindWrite}); v != Allow {
		t.Fatal("granted writes should run")
	}
}

func TestCommandPrefix(t *testing.T) {
	cases := map[string]string{
		"go test ./...":   "go test",
		"ls -la":          "ls",
		"npm run build":   "npm run",
		"python x.py":     "python",
		"echo a | grep a": "",
		"":                "",
	}
	for in, want := range cases {
		if got := CommandPrefix(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestDestructive(t *testing.T) {
	for _, c := range []string{"rm -rf /", "rm -fr ~", "sudo mkfs.ext4 /dev/sda", "git push origin main --force", "git reset --hard HEAD~3", ":(){ :|:& };:"} {
		if !IsDestructive(c) {
			t.Errorf("expected destructive: %q", c)
		}
	}
	for _, c := range []string{"rm -rf build", "go test ./...", "git push origin feature"} {
		if IsDestructive(c) {
			t.Errorf("not destructive: %q", c)
		}
	}
}

func TestModeCycle(t *testing.T) {
	if ModeAsk.Next() != ModeAutoEdit || ModeYolo.Next() != ModeAsk {
		t.Fatal("bad cycle")
	}
	if _, err := ParseMode("nope"); err == nil {
		t.Fatal("expected error")
	}
}
