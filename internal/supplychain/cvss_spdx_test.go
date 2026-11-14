package supplychain

import (
	"strings"
	"testing"
)

func TestCVSS31BaseScores(t *testing.T) {
	cases := []struct {
		vector string
		score  float64
		sev    Severity
	}{
		{"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H", 9.8, SeverityCritical},
		{"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:C/C:H/I:H/A:H", 10.0, SeverityCritical},
		{"CVSS:3.1/AV:N/AC:L/PR:L/UI:N/S:C/C:H/I:H/A:H", 9.9, SeverityCritical},
		{"CVSS:3.1/AV:N/AC:L/PR:L/UI:N/S:U/C:H/I:H/A:H", 8.8, SeverityHigh},
		{"CVSS:3.1/AV:L/AC:L/PR:L/UI:N/S:U/C:H/I:H/A:H", 7.8, SeverityHigh},
		{"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:N/A:H", 7.5, SeverityHigh},
		{"CVSS:3.1/AV:N/AC:L/PR:H/UI:N/S:U/C:H/I:H/A:H", 7.2, SeverityHigh},
		{"CVSS:3.1/AV:N/AC:L/PR:L/UI:N/S:U/C:H/I:N/A:N", 6.5, SeverityMedium},
		{"CVSS:3.1/AV:N/AC:L/PR:N/UI:R/S:C/C:L/I:L/A:N", 6.1, SeverityMedium},
		{"CVSS:3.1/AV:N/AC:H/PR:N/UI:N/S:U/C:H/I:N/A:N", 5.9, SeverityMedium},
		{"CVSS:3.1/AV:N/AC:L/PR:L/UI:R/S:C/C:L/I:L/A:N", 5.4, SeverityMedium},
		{"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:L/I:N/A:N", 5.3, SeverityMedium},
		{"CVSS:3.1/AV:N/AC:L/PR:L/UI:N/S:U/C:N/I:L/A:N", 4.3, SeverityMedium},
		{"CVSS:3.1/AV:P/AC:H/PR:H/UI:R/S:U/C:L/I:N/A:N", 1.6, SeverityLow},
		{"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:N/A:N", 0.0, SeverityNone},
		// metric order is free and temporal/environmental metrics are accepted
		{"CVSS:3.1/S:U/AV:N/AC:L/PR:N/UI:N/C:H/I:H/A:H/E:P/RL:O/RC:C/CR:H", 9.8, SeverityCritical},
		{"CVSS:3.0/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H", 9.8, SeverityCritical},
	}
	for _, c := range cases {
		cv, err := ParseCVSS3(c.vector)
		if err != nil {
			t.Fatalf("%s: %v", c.vector, err)
		}
		if got := cv.BaseScore(); got != c.score {
			t.Errorf("%s = %.1f, want %.1f", c.vector, got, c.score)
		}
		if cv.Severity() != c.sev {
			t.Errorf("%s severity = %s, want %s", c.vector, cv.Severity(), c.sev)
		}
	}
	bad := []string{
		"AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H",
		"CVSS:2.0/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H",
		"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H",
		"CVSS:3.1/AV:X/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H",
		"CVSS:3.1/AV:N/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H",
		"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H/ZZ:1",
		"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H/",
	}
	for _, b := range bad {
		if _, err := ParseCVSS3(b); err == nil {
			t.Errorf("accepted %q", b)
		}
	}
	if roundUp31(4.02) != 4.1 || roundUp31(4.0) != 4.0 || roundUp31(4.000001) != 4.0 {
		t.Error("Roundup per Appendix A")
	}
}

func TestSPDXExpressions(t *testing.T) {
	cases := map[string]string{
		"MIT":                                  "MIT",
		"mit":                                  "MIT",
		"MIT OR Apache-2.0":                    "MIT OR Apache-2.0",
		"(MIT OR Apache-2.0) AND BSD-3-Clause": "(MIT OR Apache-2.0) AND BSD-3-Clause",
		"MIT OR Apache-2.0 AND BSD-3-Clause":   "MIT OR Apache-2.0 AND BSD-3-Clause",
		"GPL-2.0-or-later WITH Classpath-exception-2.0":    "GPL-2.0-or-later WITH Classpath-exception-2.0",
		"gpl-2.0+ with classpath-exception-2.0":            "GPL-2.0+ WITH Classpath-exception-2.0",
		"LicenseRef-Proprietary":                           "LicenseRef-Proprietary",
		"DocumentRef-spdx-tool-1.2:LicenseRef-MIT-Style-2": "DocumentRef-spdx-tool-1.2:LicenseRef-MIT-Style-2",
		"((MIT))":                                "MIT",
		"Apache-2.0 AND (MIT OR (ISC AND Zlib))": "Apache-2.0 AND (MIT OR ISC AND Zlib)",
	}
	for in, want := range cases {
		e, err := ParseLicenseExpression(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if e.String() != want {
			t.Errorf("%q -> %q, want %q", in, e.String(), want)
		}
	}
	// precedence: AND binds tighter than OR
	e, _ := ParseLicenseExpression("MIT OR Apache-2.0 AND BSD-3-Clause")
	if or, ok := e.(*LicenseOr); !ok {
		t.Error("top node should be OR")
	} else if _, ok := or.Right.(*LicenseAnd); !ok {
		t.Error("right of OR should be AND")
	}
	bad := []string{"", "MIT AND", "AND MIT", "(MIT", "MIT)", "MIT WITH", "(MIT OR ISC) WITH LLVM-exception",
		"MIT+ +", "LicenseRef-", "DocumentRef-x:MIT", "MIT $", "MIT OR OR ISC", "LicenseRef-x+",
		strings.Repeat("(", 100) + "MIT" + strings.Repeat(")", 100)}
	for _, b := range bad {
		if _, err := ParseLicenseExpression(b); err == nil {
			t.Errorf("accepted %q", b)
		}
	}
	e, _ = ParseLicenseExpression("Foo-1.0 AND MIT WITH Bar-exception")
	if u := UnknownLicenseIDs(e); strings.Join(u, ",") != "Bar-exception,Foo-1.0" {
		t.Errorf("unknown ids = %v", u)
	}
}

func TestLicenseDenyList(t *testing.T) {
	deny := []string{"GPL-3.0", "AGPL-*"}
	cases := map[string]bool{
		"MIT":                                true,
		"GPL-3.0-only":                       false,
		"GPL-3.0-or-later":                   false,
		"GPL-3.0+":                           false,
		"GPL-2.0-only":                       true,
		"MIT OR GPL-3.0-only":                true,
		"MIT AND GPL-3.0-only":               false,
		"AGPL-3.0-or-later OR ISC":           true,
		"(AGPL-3.0-only OR GPL-3.0) AND MIT": false,
		"LGPL-3.0-only":                      true,
	}
	for in, want := range cases {
		e, err := ParseLicenseExpression(in)
		if err != nil {
			t.Fatal(err)
		}
		ok, bad := LicenseSatisfiable(e, deny)
		if ok != want {
			t.Errorf("%q satisfiable=%v want %v (bad=%v)", in, ok, want, bad)
		}
		if !ok && len(bad) == 0 {
			t.Errorf("%q: no offending ids reported", in)
		}
	}
}

func FuzzParseLicenseExpression(f *testing.F) {
	for _, s := range []string{"MIT", "MIT OR Apache-2.0", "(MIT AND ISC) OR GPL-2.0+ WITH Classpath-exception-2.0",
		"DocumentRef-a:LicenseRef-b", "((a))", "a and b or c", "MIT WITH", ")("} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		e, err := ParseLicenseExpression(s)
		if err != nil {
			return
		}
		out := e.String()
		e2, err := ParseLicenseExpression(out)
		if err != nil {
			t.Fatalf("re-parse of %q (from %q) failed: %v", out, s, err)
		}
		if e2.String() != out {
			t.Fatalf("not idempotent: %q -> %q -> %q", s, out, e2.String())
		}
		_, _ = LicenseSatisfiable(e, []string{"GPL-*"})
	})
}
