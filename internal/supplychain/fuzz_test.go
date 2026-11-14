package supplychain

import (
	"testing"
)

// The parser fuzz targets assert that malformed input never panics and
// that successful parses satisfy basic invariants.

func checkGraph(t *testing.T, g *Graph) {
	t.Helper()
	for _, p := range g.Packages() {
		if p.PURL == "" {
			t.Fatalf("package %s without purl", p.ID())
		}
		if _, err := ParsePURL(p.PURL); err != nil {
			t.Fatalf("package %s has unparsable purl %q: %v", p.ID(), p.PURL, err)
		}
		for _, d := range g.DependenciesOf(p.ID()) {
			if g.Package(d) == nil {
				t.Fatalf("dangling edge %s -> %s", p.ID(), d)
			}
		}
	}
}

func FuzzParseGoMod(f *testing.F) {
	f.Add(fixture(f, "go/go.mod.golden"), fixture(f, "go/go.sum"))
	f.Add([]byte("module x\nrequire (\n\ta.b/c v1.0.0 // indirect\n)\n"), []byte("a.b/c v1.0.0 h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n"))
	f.Fuzz(func(t *testing.T, mod, sum []byte) {
		m, err := ParseGoMod("go.mod", mod)
		if err != nil {
			return
		}
		s, _ := ParseGoSum("go.sum", sum)
		checkGraph(t, GoGraph("go.mod", m, s))
	})
}

func FuzzParseRequirements(f *testing.F) {
	f.Add(fixture(f, "python/requirements.txt"))
	f.Add([]byte("a[b]==1.0; python_version<'3' --hash=sha256:00\n-r x\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		rf, err := ParseRequirements("r.txt", data)
		if err != nil {
			return
		}
		checkGraph(t, RequirementsGraph("r.txt", rf))
	})
}

func FuzzParsePNPMLock(f *testing.F) {
	f.Add(fixture(f, "pnpm9/pnpm-lock.yaml"))
	f.Add(fixture(f, "pnpm6/pnpm-lock.yaml"))
	f.Add([]byte("lockfileVersion: 5.4\nspecifiers:\n  a: ^1\ndependencies:\n  a: 1.0.0\npackages:\n  /a/1.0.0:\n    resolution: {integrity: sha1-AAAA}\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		g, err := ParsePNPMLock("pnpm-lock.yaml", data)
		if err != nil {
			return
		}
		checkGraph(t, g)
	})
}

func FuzzParseYAMLSubset(f *testing.F) {
	f.Add([]byte("a: 1\nb:\n  - x\n  - {c: d, e: [1, 2]}\nf: |\n  text\n"))
	f.Add([]byte("'q': \"\\u0041\"\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = parseYAMLSubset("x.yaml", data)
	})
}

func FuzzParseTOML(f *testing.F) {
	f.Add(fixture(f, "rust/Cargo.lock"))
	f.Add(fixture(f, "python/poetry.lock"))
	f.Add([]byte("a.b = 1\n[t]\nx = [1, 2.5, \"s\", 'l', {y = true}]\n[[arr]]\nz = 1979-05-27T07:32:00Z\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if _, err := parseTOML("x.toml", data); err != nil {
			return
		}
		if g, err := ParseCargoLock("Cargo.lock", data); err == nil {
			checkGraph(t, g)
		}
		if g, err := ParsePoetryLock("poetry.lock", data, nil); err == nil {
			checkGraph(t, g)
		}
	})
}

func FuzzParseNPMLocks(f *testing.F) {
	f.Add(fixture(f, "npm3/package-lock.json"), fixture(f, "yarn/yarn.lock"))
	f.Add(fixture(f, "npm1/package-lock.json"), []byte("a@^1:\n  version \"1.0.0\"\n"))
	f.Fuzz(func(t *testing.T, lock, yarn []byte) {
		if g, err := ParsePackageLock("package-lock.json", lock, nil); err == nil {
			checkGraph(t, g)
		}
		if g, err := ParseYarnLock("yarn.lock", yarn, nil); err == nil {
			checkGraph(t, g)
		}
	})
}

func FuzzParseOtherLocks(f *testing.F) {
	f.Add(fixture(f, "ruby/Gemfile.lock"), fixture(f, "php/composer.lock"), fixture(f, "python/Pipfile.lock"))
	f.Fuzz(func(t *testing.T, gem, composer, pipfile []byte) {
		if g, err := ParseGemfileLock("Gemfile.lock", gem); err == nil {
			checkGraph(t, g)
		}
		if g, err := ParseComposerLock("composer.lock", composer, nil); err == nil {
			checkGraph(t, g)
		}
		if g, err := ParsePipfileLock("Pipfile.lock", pipfile, nil); err == nil {
			checkGraph(t, g)
		}
	})
}

func FuzzVersions(f *testing.F) {
	for _, s := range []string{"1.2.3-rc.1+b", "v0.0.0-20191109021931-daa7c04131f5", "1!2.0a1.post3.dev4+l.5", "1.0.0.pre", "^1.2 || ~0.3", "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H"} {
		f.Add(s, s)
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		if v, err := ParseSemVer(a); err == nil {
			w, err := ParseSemVer(v.String())
			if err != nil || w.Compare(v) != 0 {
				t.Fatalf("semver round trip %q -> %q: %v", a, v.String(), err)
			}
		}
		if v, err := ParsePEP440(a); err == nil {
			w, err := ParsePEP440(v.String())
			if err != nil || w.Compare(v) != 0 {
				t.Fatalf("pep440 round trip %q -> %q: %v", a, v.String(), err)
			}
		}
		for _, eco := range Ecosystems() {
			c1, e1 := ComparatorFor(eco).Compare(a, b)
			c2, e2 := ComparatorFor(eco).Compare(b, a)
			if e1 == nil && e2 == nil && c1 != -c2 {
				t.Fatalf("%s comparator not antisymmetric for %q %q: %d %d", eco, a, b, c1, c2)
			}
		}
		_, _ = MatchNPMRange(a, b)
		_, _ = MatchCargoReq(a, b)
		_, _ = ParsePEP440Specifiers(a)
		_, _ = ParseMarker(a)
		_, _ = ParsePseudoVersion(a)
		if c, err := ParseCVSS3(a); err == nil {
			if s := c.BaseScore(); s < 0 || s > 10 {
				t.Fatalf("score %v out of range", s)
			}
		}
	})
}

func FuzzPURL(f *testing.F) {
	f.Add("pkg:npm/%40angular/core@16.0.0?a=b#x/y")
	f.Add("pkg:golang/github.com/a/b@v1.0.0%2Bincompatible")
	f.Fuzz(func(t *testing.T, s string) {
		p, err := ParsePURL(s)
		if err != nil {
			return
		}
		out := p.String()
		q, err := ParsePURL(out)
		if err != nil {
			t.Fatalf("re-parse %q (from %q): %v", out, s, err)
		}
		if q.String() != out {
			t.Fatalf("purl not canonical: %q -> %q -> %q", s, out, q.String())
		}
	})
}
