package policy

import (
	"strings"
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

// FuzzParseShell checks that any command line parses without panicking and
// that a curl hidden behind separators is never lost.
func FuzzParseShell(f *testing.F) {
	for _, s := range []string{"ls", "a;b|c&&d", `echo "$(x` + "`y`" + `)"`, "bash -c 'sh -c \"z\"'", "(((", "'unterminated", `\`, "a 2>&1 <in >out", "sudo -u", "timeout", "<(a) >(b)", "#x\ny",
		`$'cu\x72l'`, `$"curl"`, `$'\'`, `$'\u0063url'`, `$'\143url'`, `x$'a'b`} {
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

// FuzzParsePolicy checks that Parse never panics, that a policy it accepts
// always compiles into an enforcer (Validate and the compilers agree), and
// that the enforcer's checks never panic on arbitrary requests.
func FuzzParsePolicy(f *testing.F) {
	for _, s := range []string{
		corpPolicy,
		`{"version":1}`,
		`{"version":1,"tools":{"deny":["mcp__*"]},"commands":{"deny":["re:^rm -rf","git push *"],"allow":["ls","go *"]}}`,
		`{"version":1,"paths":{"deny":["~/.ssh","/etc/**"],"deny_read":["**/.env"],"deny_write":["C:/x","./rel"]}}`,
		`{"version":1,"paths":{"deny":["["]}}`,
		`{"version":1,"commands":{"deny":["re:("]}}`,
		`{"version":1,"secrets":{"redact":true,"rules":[{"id":"corp","pattern":"corp_[a-z]{8}","keywords":["corp_"]}]}}`,
		`{"version":1,"max_permission_mode":"ask","sandbox":{"mode":"read-only","network":false},"disabled_features":["web","mcp"]}`,
		`{"version":1} {}`, `{"version":2}`, `{"version":1,"x":1}`, `[]`, `null`, ``, `{"version":1,"paths":{"deny":["  "]}}`,
		`{"version":1,"paths":{"deny":["~"]}}`, `{"version":1,"budget":{"session_usd":-1}}`,
	} {
		f.Add([]byte(s), "ls; curl x > ~/.ssh/k", "/home/alice/proj/a.go")
	}
	f.Fuzz(func(t *testing.T, data []byte, cmd, path string) {
		p, err := Parse(data, "fuzz.json")
		if err != nil {
			return
		}
		e, err := newEnforcer(home, false, p)
		if err != nil {
			t.Fatalf("Parse accepted a policy NewEnforcer rejects: %v\n%s", err, data)
		}
		ef, err := newEnforcer(home, true, p)
		if err != nil {
			t.Fatalf("case-folding enforcer rejected an accepted policy: %v", err)
		}
		for _, en := range []*Enforcer{e, ef} {
			_ = en.CheckCommand(cmd, "/home/alice/proj")
			_ = en.CheckPath(path, "/home/alice/proj", true)
			_ = en.CheckPath(path, "/home/alice/proj", false)
			_ = en.CheckTool(ToolRequest{Name: cmd, Kind: tools.KindWrite, Path: path, Root: "/home/alice/proj"})
			_ = en.CheckModel(cmd, path)
			_ = en.Describe()
			_, _, _ = en.Secrets()
		}
	})
}

// FuzzCommandPattern checks pattern compilation and matching: a literal
// pattern always matches itself and itself followed by arguments, and a
// pattern ending in " *" matches the bare program.
func FuzzCommandPattern(f *testing.F) {
	for _, s := range []string{"curl", "git push *", "rm -rf *", "re:^ssh ", "re:(", "a?c", "*", "  ", "x  y"} {
		f.Add(s, "curl -s https://x")
	}
	f.Fuzz(func(t *testing.T, pat, cmd string) {
		cp, err := compileCommand(pat)
		if err != nil {
			return
		}
		_ = cp.match(cmd)
		if cp.re != nil || strings.ContainsAny(cp.glob, "*?") {
			return
		}
		if !cp.match(cp.glob) || !cp.match(cp.glob+" arg") {
			t.Fatalf("literal pattern %q does not match itself", pat)
		}
	})
}

// FuzzGlobMatch checks globMatch against the obvious identities.
func FuzzGlobMatch(f *testing.F) {
	for _, s := range []string{"", "a", "mcp__*", "a*b?c", "**", "?"} {
		f.Add(s, "mcp__server__tool")
	}
	f.Fuzz(func(t *testing.T, pat, s string) {
		_ = globMatch(pat, s)
		if !globMatch("*", s) {
			t.Fatalf("* does not match %q", s)
		}
		if !strings.ContainsAny(s, "*?") && !globMatch(s, s) {
			t.Fatalf("%q does not match itself", s)
		}
		if !strings.ContainsAny(pat, "*?") && globMatch(pat, s) != (pat == s) {
			t.Fatalf("literal %q vs %q", pat, s)
		}
	})
}

// FuzzPathPattern checks that path patterns compile or fail cleanly, and
// that a literal absolute pattern protects itself and everything under it.
func FuzzPathPattern(f *testing.F) {
	for _, s := range []string{"~/.ssh", "/etc/**", "**/.env", "C:/Users/x", "./rel", "[", "{a,b}", "/a b/ü", "~", "/"} {
		f.Add(s, "/home/alice/x")
	}
	f.Fuzz(func(t *testing.T, pat, p string) {
		for _, fold := range []bool{false, true} {
			pp, err := compilePath(pat, home, fold)
			if err != nil {
				continue
			}
			_ = pp.match(p)
			lit := pp.glob
			if !strings.HasPrefix(lit, "/") || strings.ContainsAny(lit, "*?[]{}\\") || strings.Contains(lit, "//") || lit == "/" {
				continue
			}
			if !pp.match(lit) || !pp.match(lit+"/child") {
				t.Fatalf("pattern %q (glob %q) does not cover itself", pat, lit)
			}
		}
	})
}
