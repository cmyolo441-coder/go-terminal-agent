package policy

import (
	"strings"
	"testing"
)

// FuzzParseShell checks that any command line parses without panicking and
// that a curl hidden behind separators is never lost.
func FuzzParseShell(f *testing.F) {
	for _, s := range []string{"ls", "a;b|c&&d", `echo "$(x` + "`y`" + `)"`, "bash -c 'sh -c \"z\"'", "(((", "'unterminated", `\`, "a 2>&1 <in >out", "sudo -u", "timeout", "<(a) >(b)", "#x\ny"} {
		f.Add(s)
	}
	e, err := newEnforcer(home, false, &Policy{Version: 1, Commands: Rules{Deny: []string{"curl"}}, Source: "fuzz"})
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, cmd string) {
		_ = parseShell(cmd)
		if strings.ContainsAny(cmd, "'\"`\\$#(){}<>") {
			return
		}
		if d := e.CheckCommand(cmd+"; curl x", ""); d.Allowed {
			t.Fatalf("curl after %q was not seen", cmd)
		}
	})
}
