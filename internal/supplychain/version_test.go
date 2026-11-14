package supplychain

import (
	"testing"
	"time"
)

func sign(x int) int {
	switch {
	case x < 0:
		return -1
	case x > 0:
		return 1
	}
	return 0
}

func TestSemVerPrecedence(t *testing.T) {
	// The ordering example from semver.org section 11, plus extra cases.
	ordered := []string{
		"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2",
		"1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.1-0", "1.0.1", "1.1.0", "2.0.0-0.3.7", "2.0.0-x.7.z.92", "2.0.0",
		"10.0.0",
	}
	for i := 0; i < len(ordered); i++ {
		for j := 0; j < len(ordered); j++ {
			c, err := compareSemVerStrings(ordered[i], ordered[j])
			if err != nil {
				t.Fatal(err)
			}
			if sign(c) != sign(i-j) {
				t.Errorf("compare(%s,%s)=%d want %d", ordered[i], ordered[j], c, sign(i-j))
			}
		}
	}
	if c, _ := compareSemVerStrings("1.0.0+build.1", "1.0.0+build.2"); c != 0 {
		t.Error("build metadata must be ignored")
	}
	for _, bad := range []string{"1.0", "01.0.0", "1.0.0-01", "1.0.0-", "1.0.0+", "1.0.0-a..b", "v", "1.0.0-a_b", "99999999999999999999.0.0"} {
		if _, err := ParseSemVer(bad); err == nil {
			t.Errorf("ParseSemVer(%q) accepted", bad)
		}
	}
}

func TestGoVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v0.0.0-20191109021931-daa7c04131f5", "v0.0.1", -1},
		{"v1.2.4-0.20191109021931-daa7c04131f5", "v1.2.3", 1},
		{"v1.2.4-0.20191109021931-daa7c04131f5", "v1.2.4", -1},
		{"v1.2.3-pre.0.20191109021931-daa7c04131f5", "v1.2.3-pre", 1},
		{"v1.2.3-pre.0.20191109021931-daa7c04131f5", "v1.2.3", -1},
		{"v2.0.0+incompatible", "v2.0.0", 0},
		{"v1.10.0", "v1.9.9", 1},
		{"1.2.3", "v1.2.3", 0}, // OSV writes Go versions without "v"
	}
	for _, c := range cases {
		got, err := compareGo(c.a, c.b)
		if err != nil {
			t.Fatalf("%s vs %s: %v", c.a, c.b, err)
		}
		if got != c.want {
			t.Errorf("compareGo(%s,%s)=%d want %d", c.a, c.b, got, c.want)
		}
	}
	pv, err := ParsePseudoVersion("v1.2.4-0.20191109021931-daa7c04131f5")
	if err != nil || pv.Base != "v1.2.3" || pv.Revision != "daa7c04131f5" || !pv.Time.Equal(time.Date(2019, 11, 9, 2, 19, 31, 0, time.UTC)) {
		t.Errorf("pseudo: %+v %v", pv, err)
	}
	if pv, err := ParsePseudoVersion("v1.2.3-rc.1.0.20191109021931-daa7c04131f5"); err != nil || pv.Base != "v1.2.3-rc.1" {
		t.Errorf("pre-release pseudo: %+v %v", pv, err)
	}
	if pv, err := ParsePseudoVersion("v0.0.0-20191109021931-daa7c04131f5"); err != nil || pv.Base != "" {
		t.Errorf("base-less pseudo: %+v %v", pv, err)
	}
	for _, s := range []string{"v1.2.3", "v1.2.3-rc.1", "v1.2.3-0.2019-abc", "v1.0.0-20191109021931-daa7c04131f5", "v1.2.4-0.20191309021931-daa7c04131f5"} {
		if IsPseudoVersion(s) {
			t.Errorf("%s is not a pseudo-version", s)
		}
	}
	if _, err := ParseGoVersion("v1.0.0+incompatible"); err == nil {
		t.Error("+incompatible on v1 accepted")
	}
}

func TestNPMRanges(t *testing.T) {
	cases := []struct {
		rng, v string
		want   bool
	}{
		{"^1.2.3", "1.9.9", true},
		{"^1.2.3", "2.0.0", false},
		{"^1.2.3", "2.0.0-alpha", false},
		{"^1.2.3", "1.2.4-beta", false}, // pre-release of a different tuple
		{"^1.2.3-beta.2", "1.2.3-beta.4", true},
		{"^1.2.3-beta.2", "1.2.4-beta.2", false},
		{"^0.2.3", "0.2.9", true},
		{"^0.2.3", "0.3.0", false},
		{"^0.0.3", "0.0.4", false},
		{"^0.0", "0.0.9", true},
		{"^0.0", "0.1.0", false},
		{"~1.2.3", "1.2.9", true},
		{"~1.2.3", "1.3.0", false},
		{"~1", "1.9.0", true},
		{"1.x", "1.4.0", true},
		{"1.2.x", "1.3.0", false},
		{"*", "3.0.0", true},
		{"*", "3.0.0-rc.1", false},
		{"", "0.0.1", true},
		{">=1.2.7 <1.3.0", "1.2.8", true},
		{">=1.2.7 <1.3.0", "1.3.0", false},
		{"1.2.7 || >=1.2.9 <2.0.0", "1.2.8", false},
		{"1.2.7 || >=1.2.9 <2.0.0", "1.4.6", true},
		{"1.2.3 - 2.3.4", "2.3.4", true},
		{"1.2.3 - 2.3", "2.3.9", true},
		{"1.2.3 - 2.3", "2.4.0", false},
		{"<=1.2", "1.2.9", true},
		{"<=1.2", "1.3.0", false},
		{">1.2", "1.2.9", false},
		{">1.2", "1.3.0", true},
		{">= 1.0.0", "1.0.0", true},
		{"=1.0.0", "1.0.0", true},
		{"v1.0.0", "1.0.0", true},
	}
	for _, c := range cases {
		got, err := MatchNPMRange(c.rng, c.v)
		if err != nil {
			t.Fatalf("%q %q: %v", c.rng, c.v, err)
		}
		if got != c.want {
			t.Errorf("npm %q matches %s = %v, want %v", c.rng, c.v, got, c.want)
		}
	}
	for _, bad := range []string{"^1.2.3.4", "1.x.3", "~>x", ">=a"} {
		if _, err := ParseNPMRange(bad); err == nil {
			t.Errorf("npm range %q accepted", bad)
		}
	}
}

func TestCargoReqs(t *testing.T) {
	cases := []struct {
		req, v string
		want   bool
	}{
		{"1.2.3", "1.4.0", true}, // bare = caret
		{"1.2.3", "2.0.0", false},
		{"0.2.3", "0.2.9", true},
		{"0.2.3", "0.3.0", false},
		{"0.0.3", "0.0.4", false},
		{"0", "0.9.9", true},
		{"0", "1.0.0", false},
		{"=1.2.3", "1.2.4", false},
		{"=1.2", "1.2.9", true},
		{"~1.2", "1.2.9", true},
		{"~1.2", "1.3.0", false},
		{"1.2.*", "1.2.5", true},
		{"1.2.*", "1.3.0", false},
		{"1.*", "1.9.0", true},
		{"*", "42.0.0", true},
		{">=1.2, <1.5", "1.4.9", true},
		{">=1.2, <1.5", "1.5.0", false},
		{">=1.0.0", "2.0.0-alpha", false},
		{">=1.0.0-alpha", "1.0.0-beta", true},
		{">=1.0.0-alpha", "1.0.1-beta", false},
		{"<1.0.0", "1.0.0-rc.1", false},
		{"<=1.2", "1.2.7", true},
		{">1", "1.9.0", false},
		{">1", "2.0.0", true},
	}
	for _, c := range cases {
		got, err := MatchCargoReq(c.req, c.v)
		if err != nil {
			t.Fatalf("%q %q: %v", c.req, c.v, err)
		}
		if got != c.want {
			t.Errorf("cargo %q matches %s = %v, want %v", c.req, c.v, got, c.want)
		}
	}
	for _, bad := range []string{"", "~>1.0", ">=1.0,", "1.2.3.4", "1.*.3"} {
		if _, err := ParseCargoReq(bad); err == nil {
			t.Errorf("cargo req %q accepted", bad)
		}
	}
}

func TestPEP440Ordering(t *testing.T) {
	// Ordering from PEP 440 "Summary of permitted suffixes and relative ordering".
	ordered := []string{
		"1.0.dev456", "1.0a1", "1.0a2.dev456", "1.0a12.dev456", "1.0a12", "1.0b1.dev456", "1.0b2",
		"1.0b2.post345.dev456", "1.0b2.post345", "1.0rc1.dev456", "1.0rc1", "1.0", "1.0+abc.5", "1.0+abc.7",
		"1.0+5", "1.0.post456.dev34", "1.0.post456", "1.0.15", "1.1.dev1",
	}
	for i := range ordered {
		for j := range ordered {
			c, err := comparePEP440Strings(ordered[i], ordered[j])
			if err != nil {
				t.Fatal(err)
			}
			if sign(c) != sign(i-j) {
				t.Errorf("pep440 compare(%s,%s)=%d want %d", ordered[i], ordered[j], c, sign(i-j))
			}
		}
	}
	norm := map[string]string{
		"1.0-ALPHA.1": "1.0a1", "1.0c3": "1.0rc3", "1.0-preview2": "1.0rc2", "1.0-1": "1.0.post1",
		"1.0.rev": "1.0.post0", "v1.0": "1.0", "1!2.0_dev": "1!2.0.dev0", "1.0+Ubuntu-1": "1.0+ubuntu.1",
	}
	for in, want := range norm {
		v, err := ParsePEP440(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if v.String() != want {
			t.Errorf("normalize(%s) = %s, want %s", in, v, want)
		}
	}
	if c, _ := comparePEP440Strings("1.0", "1.0.0"); c != 0 {
		t.Error("trailing zeros must compare equal")
	}
	if c, _ := comparePEP440Strings("1!0.1", "2.0"); c != 1 {
		t.Error("epoch dominates")
	}
}

func TestPEP440Specifiers(t *testing.T) {
	cases := []struct {
		spec, v  string
		allowPre bool
		want     bool
	}{
		{"~=2.2", "2.3", false, true},
		{"~=2.2", "3.0", false, false},
		{"~=1.4.5", "1.4.9", false, true},
		{"~=1.4.5", "1.5.0", false, false},
		{"==1.1.*", "1.1.post1", false, true},
		{"==1.1.*", "1.10", false, false},
		{"!=1.1.*", "1.2", false, true},
		{"==1.0", "1.0+local", false, true},
		{"==1.0+local", "1.0", false, false},
		{"<2.0", "2.0rc1", true, false}, // <V excludes pre-releases of V
		{"<2.0", "1.9", false, true},
		{">1.7", "1.7.post2", false, false}, // >V excludes post-releases of V
		{">1.7.post2", "1.7.post3", false, true},
		{">1.7", "1.7+local", false, false},
		{">=1.0", "2.0b1", false, false}, // pre-releases excluded by default
		{">=1.0", "2.0b1", true, true},
		{">=1.0b1", "1.0b2", false, true}, // explicitly mentions a pre-release
		{">=1.0,!=1.5,<2", "1.5", false, false},
		{"===1.0.0", "1.0.0", false, true},
		{"<=2.0", "2.0+local", false, true},
	}
	for _, c := range cases {
		set, err := ParsePEP440Specifiers(c.spec)
		if err != nil {
			t.Fatalf("%s: %v", c.spec, err)
		}
		v, err := ParsePEP440(c.v)
		if err != nil {
			t.Fatal(err)
		}
		if got := set.Contains(v, c.allowPre); got != c.want {
			t.Errorf("%q contains %s (pre=%v) = %v, want %v", c.spec, c.v, c.allowPre, got, c.want)
		}
	}
	for _, bad := range []string{"~=1", ">=1.*", "1.0", "<1.0+local", "==1.0a1.*"} {
		if _, err := ParsePEP440Specifiers(bad); err == nil {
			t.Errorf("specifier %q accepted", bad)
		}
	}
}

func TestGemAndComposerOrdering(t *testing.T) {
	gems := []string{"1.0.a", "1.0.a.2", "1.0.b1", "1.0.0.pre", "1.0.rc1", "1", "1.0.1", "1.1", "2.0.0.alpha", "2"}
	// "1.0.0.pre" vs "1.0.a"/"1.0.b1": canonical prerelease segments sort
	// lexically after the release part: a < b < pre < rc.
	for i := range gems {
		for j := range gems {
			c, err := CompareGemVersions(gems[i], gems[j])
			if err != nil {
				t.Fatal(err)
			}
			if sign(c) != sign(i-j) {
				t.Errorf("gem compare(%s,%s)=%d want %d", gems[i], gems[j], c, sign(i-j))
			}
		}
	}
	if c, _ := CompareGemVersions("1.0", "1.0.0"); c != 0 {
		t.Error("gem trailing zeros")
	}
	if c, _ := CompareGemVersions("1.0.0-rc1", "1.0.0.pre.rc1"); c != 0 {
		t.Error("gem '-' maps to .pre.")
	}
	php := []string{"1.0.0-dev", "1.0.0-alpha1", "1.0.0-beta", "1.0.0-RC1", "1.0.0", "1.0.0-pl1", "v1.0.1", "1.10.0"}
	for i := range php {
		for j := range php {
			if c := CompareComposerVersions(php[i], php[j]); sign(c) != sign(i-j) {
				t.Errorf("composer compare(%s,%s)=%d want %d", php[i], php[j], c, sign(i-j))
			}
		}
	}
	if CompareComposerVersions("1.0", "1.0.0") != -1 {
		t.Error("PHP: 1.0 < 1.0.0")
	}
}
