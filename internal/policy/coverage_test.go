package policy

import (
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

func mustEnforcer(t *testing.T, js string) *Enforcer {
	t.Helper()
	p, err := Parse([]byte(js), "test.json")
	if err != nil {
		t.Fatal(err)
	}
	e, err := NewEnforcer(p)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// The verify tool runs the test command through a shell, so command rules
// apply to it as to bash.
func TestVerifyToolCommandIsChecked(t *testing.T) {
	e := mustEnforcer(t, `{"version":1,"commands":{"deny":["make"]}}`)
	root := t.TempDir()
	deny := tools.NewCall("verify", tools.KindExec, "Verify", "make test", "make test", nil)
	if d := e.CheckTool(RequestFor(deny, []byte(`{}`), root)); d.Allowed {
		t.Fatal("verify ran a denied command")
	}
	ok := tools.NewCall("verify", tools.KindExec, "Verify", "go test ./...", "go test ./...", nil)
	if d := e.CheckTool(RequestFor(ok, []byte(`{}`), root)); !d.Allowed {
		t.Fatalf("verify of an allowed command denied: %s", d)
	}
}

func TestRestrictsReads(t *testing.T) {
	for js, want := range map[string]bool{
		`{"version":1}`: false,
		`{"version":1,"paths":{"deny_write":["x"]}}`:   false,
		`{"version":1,"paths":{"deny":["~/.ssh"]}}`:    true,
		`{"version":1,"paths":{"deny_read":[".env"]}}`: true,
		`{"version":1,"commands":{"deny":["curl"]}}`:   false,
	} {
		if got := mustEnforcer(t, js).RestrictsReads(); got != want {
			t.Errorf("%s: RestrictsReads = %v, want %v", js, got, want)
		}
	}
	var nilE *Enforcer
	if nilE.RestrictsReads() {
		t.Error("nil enforcer restricts reads")
	}
}
