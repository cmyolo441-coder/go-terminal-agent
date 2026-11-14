package supplychain

import (
	"embed"
	"errors"
	"slices"
	"strings"
	"testing"
)

//go:embed testdata
var testdataFS embed.FS

func fixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := testdataFS.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return b
}

func mustPkg(t *testing.T, g *Graph, eco Ecosystem, name, version string) *Package {
	t.Helper()
	p := g.Package(PackageID(eco, name, version))
	if p == nil {
		var ids []string
		for _, q := range g.Packages() {
			ids = append(ids, q.ID())
		}
		t.Fatalf("package %s %s@%s missing; have %v", eco, name, version, ids)
	}
	return p
}

func hasEdge(g *Graph, from, to string) bool {
	return slices.Contains(g.DependenciesOf(from), to)
}

func TestParseGoModAndSum(t *testing.T) {
	mod, err := ParseGoMod("go.mod", fixture(t, "go/go.mod.golden"))
	if err != nil {
		t.Fatal(err)
	}
	if mod.Module != "example.com/app" || mod.Go != "1.22" || mod.Toolchain != "go1.22.3" {
		t.Fatalf("header: %+v", mod)
	}
	if len(mod.Require) != 7 || len(mod.Replace) != 2 || len(mod.Exclude) != 1 || len(mod.Retract) != 1 {
		t.Fatalf("directives: req=%d rep=%d exc=%d ret=%d", len(mod.Require), len(mod.Replace), len(mod.Exclude), len(mod.Retract))
	}
	sum, err := ParseGoSum("go.sum", fixture(t, "go/go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	g := GoGraph("go.mod", mod, sum)
	mux := mustPkg(t, g, EcosystemGo, "github.com/gorilla/mux", "v1.8.0")
	if !mux.Direct || len(mux.Hashes) != 1 || mux.Hashes[0].Alg != HashSHA256 || len(mux.Hashes[0].Hex) != 64 {
		t.Errorf("mux: %+v", mux)
	}
	if mux.PURL != "pkg:golang/github.com/gorilla/mux@v1.8.0" {
		t.Errorf("purl = %s", mux.PURL)
	}
	if p := mustPkg(t, g, EcosystemGo, "golang.org/x/text", "v0.3.7"); p.Direct {
		t.Error("x/text should be indirect")
	}
	if p := mustPkg(t, g, EcosystemGo, "example.com/local", "v1.0.0"); !p.Local || p.Resolved != "../local" {
		t.Errorf("local replace: %+v", p)
	}
	mustPkg(t, g, EcosystemGo, "github.com/pkg/errors", "v0.9.2") // versioned replace applied
	inc := mustPkg(t, g, EcosystemGo, "github.com/old/thing", "v2.0.0+incompatible")
	if inc.PURL != "pkg:golang/github.com/old/thing@v2.0.0%2Bincompatible" {
		t.Errorf("incompatible purl = %s", inc.PURL)
	}
}

func TestParseGoModErrors(t *testing.T) {
	cases := map[string]string{
		"no module":       "go 1.21\n",
		"unknown":         "module a.b/c\nfrobnicate x\n",
		"bad version":     "module a.b/c\nrequire x.y/z 1.2.3\n",
		"major mismatch":  "module a.b/c\nrequire x.y/z/v2 v3.0.0\n",
		"needs suffix":    "module a.b/c\nrequire x.y/z v2.0.0\n",
		"unterminated":    "module a.b/c\nrequire (\nx.y/z v1.0.0\n",
		"local with ver":  "module a.b/c\nreplace x.y/z => ./z v1.0.0\n",
		"remote no ver":   "module a.b/c\nreplace x.y/z => x.y/w\n",
		"bad path":        "module a..b/../c\n",
		"leading zero":    "module a.b/c\nrequire x.y/z v1.02.0\n",
		"unterminated qs": "module \"a.b/c\n",
	}
	for name, src := range cases {
		if _, err := ParseGoMod("go.mod", []byte(src)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if _, err := ParseGoSum("go.sum", []byte("x.y/z v1.0.0 h1:short\n")); err == nil {
		t.Error("bad h1 accepted")
	}
	if _, err := ParseGoSum("go.sum", []byte("x.y/z v1.0.0\n")); err == nil {
		t.Error("two-field line accepted")
	}
}

func TestParsePackageLockV3(t *testing.T) {
	g, err := ParsePackageLock("package-lock.json", fixture(t, "npm3/package-lock.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	express := mustPkg(t, g, EcosystemNPM, "express", "4.18.2")
	if !express.Direct || express.Dev || len(express.Hashes) != 1 || express.Hashes[0].Alg != HashSHA512 {
		t.Errorf("express: %+v", express)
	}
	// nested node_modules resolution: express -> its own qs 6.11.0, not hoisted 6.5.0
	if !hasEdge(g, express.ID(), PackageID(EcosystemNPM, "qs", "6.11.0")) {
		t.Error("express should depend on nested qs 6.11.0")
	}
	if hasEdge(g, express.ID(), PackageID(EcosystemNPM, "qs", "6.5.0")) {
		t.Error("express must not depend on hoisted qs 6.5.0")
	}
	if !hasEdge(g, PackageID(EcosystemNPM, "debug", "2.6.9"), PackageID(EcosystemNPM, "ms", "2.0.0")) {
		t.Error("debug -> ms missing")
	}
	scoped := mustPkg(t, g, EcosystemNPM, "@scope/pkg", "1.0.0")
	if !scoped.InstallScript || scoped.PURL != "pkg:npm/%40scope/pkg@1.0.0" {
		t.Errorf("scoped: %+v", scoped)
	}
	jest := mustPkg(t, g, EcosystemNPM, "jest-lite", "1.2.0")
	if !jest.Direct || !jest.Dev {
		t.Errorf("jest-lite: %+v", jest)
	}
	lib := mustPkg(t, g, EcosystemNPM, "lib-a", "0.1.0")
	if !lib.Local || !lib.Direct {
		t.Errorf("workspace lib-a: %+v", lib)
	}
	if !hasEdge(g, lib.ID(), PackageID(EcosystemNPM, "lodahs", "1.0.0")) {
		t.Error("workspace dependency edge missing")
	}
	if p := mustPkg(t, g, EcosystemNPM, "ms", "2.0.0"); p.Direct {
		t.Error("ms should be transitive")
	}
	path := g.PathTo(PackageID(EcosystemNPM, "ms", "2.0.0"))
	if len(path) != 2 && len(path) != 3 {
		t.Errorf("PathTo(ms) = %v", path)
	}
}

func TestParsePackageLockV1(t *testing.T) {
	man, err := ParsePackageJSON("package.json", fixture(t, "npm1/package.json"))
	if err != nil {
		t.Fatal(err)
	}
	if s := man.InstallScripts(); s["postinstall"] != "node setup.js" || len(s) != 1 {
		t.Errorf("install scripts: %v", s)
	}
	g, err := ParsePackageLock("package-lock.json", fixture(t, "npm1/package-lock.json"), man)
	if err != nil {
		t.Fatal(err)
	}
	chalk := mustPkg(t, g, EcosystemNPM, "chalk", "2.4.2")
	if !chalk.Direct || !hasEdge(g, chalk.ID(), PackageID(EcosystemNPM, "ansi-styles", "3.2.1")) {
		t.Errorf("chalk should be direct and use nested ansi-styles 3.2.1")
	}
	if p := mustPkg(t, g, EcosystemNPM, "ansi-styles", "4.0.0"); !p.Direct || !p.Dev {
		t.Errorf("ansi-styles 4: %+v", p)
	}
	// without manifest: top-level unrequired packages become direct
	g2, err := ParsePackageLock("package-lock.json", fixture(t, "npm1/package-lock.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !mustPkg(t, g2, EcosystemNPM, "left-pad", "1.3.0").Direct {
		t.Error("heuristic direct detection failed")
	}
	if _, err := ParsePackageLock("x", []byte(`{"lockfileVersion": 7, "packages": {}}`), nil); err == nil {
		t.Error("unsupported version accepted")
	}
	if _, err := ParsePackageLock("x", []byte(`{"lockfileVersion": 3, "packages": {}} trailing`), nil); err == nil {
		t.Error("trailing data accepted")
	}
}

func TestParseYarnLock(t *testing.T) {
	man, err := ParsePackageJSON("package.json", fixture(t, "yarn/package.json"))
	if err != nil {
		t.Fatal(err)
	}
	g, err := ParseYarnLock("yarn.lock", fixture(t, "yarn/yarn.lock"), man)
	if err != nil {
		t.Fatal(err)
	}
	cf := mustPkg(t, g, EcosystemNPM, "@babel/code-frame", "7.12.13")
	hl := mustPkg(t, g, EcosystemNPM, "@babel/highlight", "7.13.10")
	if !cf.Direct || hl.Direct || !hasEdge(g, cf.ID(), hl.ID()) {
		t.Error("code-frame -> highlight edge / direct flags wrong")
	}
	if len(hl.Hashes) != 1 || hl.Hashes[0].Alg != HashSHA1 {
		t.Errorf("sha1 from resolved fragment: %+v", hl.Hashes)
	}
	if !mustPkg(t, g, EcosystemNPM, "lodash", "4.17.21").Direct {
		t.Error("aliased lodash should be direct under its real name")
	}
	if _, err := ParseYarnLock("yarn.lock", []byte("__metadata:\n  version: 6\n"), nil); !errors.Is(err, ErrUnsupportedFormat) {
		t.Errorf("berry: %v", err)
	}
	if _, err := ParseYarnLock("yarn.lock", []byte("foo@1:\n      version \"1\"\n"), nil); err == nil {
		t.Error("bad indentation accepted")
	}
}

func TestParsePNPMv9(t *testing.T) {
	g, err := ParsePNPMLock("pnpm-lock.yaml", fixture(t, "pnpm9/pnpm-lock.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	rd := mustPkg(t, g, EcosystemNPM, "react-dom", "18.2.0")
	r := mustPkg(t, g, EcosystemNPM, "react", "18.2.0")
	if !rd.Direct || !r.Direct || rd.Dev || !hasEdge(g, rd.ID(), r.ID()) {
		t.Error("react-dom flags/edges wrong")
	}
	ts := mustPkg(t, g, EcosystemNPM, "typescript", "5.3.3")
	tn := mustPkg(t, g, EcosystemNPM, "@types/node", "20.1.0")
	if !ts.Direct || !ts.Dev || !tn.Dev || tn.Direct {
		t.Errorf("dev propagation: ts=%+v tn=%+v", ts, tn)
	}
	if le := mustPkg(t, g, EcosystemNPM, "loose-envify", "1.4.0"); le.Dev || len(le.Hashes) != 1 {
		t.Errorf("loose-envify: %+v", le)
	}
}

func TestParsePNPMv6(t *testing.T) {
	g, err := ParsePNPMLock("pnpm-lock.yaml", fixture(t, "pnpm6/pnpm-lock.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	ax := mustPkg(t, g, EcosystemNPM, "axios", "1.6.2")
	if !ax.Direct || !hasEdge(g, ax.ID(), PackageID(EcosystemNPM, "follow-redirects", "1.15.3")) {
		t.Error("axios edges")
	}
	if vt := mustPkg(t, g, EcosystemNPM, "vitest", "1.0.4"); !vt.Dev || !vt.InstallScript {
		t.Errorf("vitest: %+v", vt)
	}
	if _, err := ParsePNPMLock("x", []byte("lockfileVersion: '4.0'\n")); err == nil {
		t.Error("v4 accepted")
	}
	if _, err := ParsePNPMLock("x", []byte("a: &anchor 1\n")); err == nil {
		t.Error("anchor accepted")
	}
}

func TestParseRequirements(t *testing.T) {
	rf, err := ParseRequirements("requirements.txt", fixture(t, "python/requirements.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rf.Includes, []string{"requirements-base.txt"}) || len(rf.IndexURLs) != 1 {
		t.Errorf("options: %+v", rf)
	}
	byName := map[string]Requirement{}
	for _, r := range rf.Requirements {
		byName[NormalizePyPIName(r.Name)] = r
	}
	dj := byName["django"]
	if v, ok := dj.PinnedVersion(); !ok || v != "4.2.1" || len(dj.Hashes) != 1 {
		t.Errorf("django: %+v", dj)
	}
	rq := byName["requests"]
	if !slices.Equal(rq.Extras, []string{"security", "socks"}) || rq.Specifier != ">=2.25,<3" || !strings.Contains(rq.Marker, "sys_platform") {
		t.Errorf("requests: %+v", rq)
	}
	if v, ok := byName["flask-cors"].PinnedVersion(); !ok || v != "3.0.10" {
		t.Errorf("flask-cors === pin: %+v", byName["flask-cors"])
	}
	if byName["urllib3"].Marker != `python_version < "3.12"` {
		t.Errorf("urllib3 marker: %q", byName["urllib3"].Marker)
	}
	if byName["pkg"].URL == "" || !byName["proj"].Editable {
		t.Error("url/editable requirements")
	}
	g := RequirementsGraph("requirements.txt", rf)
	if p := mustPkg(t, g, EcosystemPyPI, "django", "4.2.1"); p.PURL != "pkg:pypi/django@4.2.1" {
		t.Errorf("purl %s", p.PURL)
	}
	if p := mustPkg(t, g, EcosystemPyPI, "requests", ""); !p.Unpinned {
		t.Error("requests should be unpinned")
	}
	bad := []string{"--frobnicate x\n", "foo >= \n", "foo==1.0 --hash=sha256:zz\n", "foo; bogus_var == '1'\n", "!!!\n", "foo==1.0 --hash=md4:00\n"}
	for _, b := range bad {
		if _, err := ParseRequirements("r.txt", []byte(b)); err == nil {
			t.Errorf("accepted %q", b)
		}
	}
}

func TestMarkerEvaluate(t *testing.T) {
	m, err := ParseMarker(`python_version >= "3.8" and (sys_platform == "linux" or sys_platform == "darwin") and extra != "test"`)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Evaluate(map[string]string{"python_version": "3.11", "sys_platform": "linux"}) {
		t.Error("expected true")
	}
	if m.Evaluate(map[string]string{"python_version": "3.7", "sys_platform": "linux"}) {
		t.Error("3.7 should fail")
	}
	if m.Evaluate(map[string]string{"python_version": "3.11", "sys_platform": "win32"}) {
		t.Error("win32 should fail")
	}
	m2, _ := ParseMarker(`"linux" in sys_platform`)
	if !m2.Evaluate(map[string]string{"sys_platform": "linux2"}) {
		t.Error("in operator")
	}
}

func TestParsePoetryLock(t *testing.T) {
	g, err := ParsePoetryLock("poetry.lock", fixture(t, "python/poetry.lock"), fixture(t, "python/pyproject.toml"))
	if err != nil {
		t.Fatal(err)
	}
	req := mustPkg(t, g, EcosystemPyPI, "requests", "2.31.0")
	if !req.Direct || req.Dev || len(req.Hashes) != 1 {
		t.Errorf("requests: %+v", req)
	}
	if !hasEdge(g, req.ID(), PackageID(EcosystemPyPI, "urllib3", "2.0.7")) || !hasEdge(g, req.ID(), PackageID(EcosystemPyPI, "certifi", "2023.7.22")) {
		t.Error("requests edges")
	}
	if c := mustPkg(t, g, EcosystemPyPI, "certifi", "2023.7.22"); c.Direct || len(c.Hashes) != 2 {
		t.Errorf("certifi: %+v", c)
	}
	if p := mustPkg(t, g, EcosystemPyPI, "pytest", "7.4.3"); !p.Direct || !p.Dev {
		t.Errorf("pytest: %+v", p)
	}
}

func TestParsePipfileLock(t *testing.T) {
	g, err := ParsePipfileLock("Pipfile.lock", fixture(t, "python/Pipfile.lock"), fixture(t, "python/Pipfile"))
	if err != nil {
		t.Fatal(err)
	}
	if p := mustPkg(t, g, EcosystemPyPI, "flask", "2.3.2"); !p.Direct || len(p.Hashes) != 1 {
		t.Errorf("flask: %+v", p)
	}
	if p := mustPkg(t, g, EcosystemPyPI, "idna", "3.4"); p.Direct || p.Marker == "" {
		t.Errorf("idna: %+v", p)
	}
	if p := mustPkg(t, g, EcosystemPyPI, "black", "23.1.0"); !p.Dev || !p.Direct {
		t.Errorf("black: %+v", p)
	}
}

func TestParseCargoLock(t *testing.T) {
	g, err := ParseCargoLock("Cargo.lock", fixture(t, "rust/Cargo.lock"))
	if err != nil {
		t.Fatal(err)
	}
	app := mustPkg(t, g, EcosystemCargo, "app", "0.1.0")
	serde := mustPkg(t, g, EcosystemCargo, "serde", "1.0.190")
	t3 := mustPkg(t, g, EcosystemCargo, "time", "0.3.30")
	t1 := mustPkg(t, g, EcosystemCargo, "time", "0.1.45")
	if !app.Local || !serde.Direct || !t3.Direct || t1.Direct {
		t.Error("direct/local flags")
	}
	if !hasEdge(g, t3.ID(), t1.ID()) || !hasEdge(g, serde.ID(), PackageID(EcosystemCargo, "serde_derive", "1.0.190")) {
		t.Error("cargo edges")
	}
	if serde.PURL != "pkg:cargo/serde@1.0.190" || len(serde.Hashes) != 1 {
		t.Errorf("serde: %+v", serde)
	}
	if _, err := ParseCargoLock("x", []byte("version = 3\n[[package]]\nname = \"a\"\nversion = \"1.0.0\"\ndependencies = [\"missing\"]\n")); err == nil {
		t.Error("dangling dependency accepted")
	}
}

func TestParseGemfileLock(t *testing.T) {
	g, err := ParseGemfileLock("Gemfile.lock", fixture(t, "ruby/Gemfile.lock"))
	if err != nil {
		t.Fatal(err)
	}
	nk := mustPkg(t, g, EcosystemRubyGems, "nokogiri", "1.15.4")
	if !nk.Direct || nk.Qualifiers["platform"] != "x86_64-linux" || nk.PURL != "pkg:gem/nokogiri@1.15.4?platform=x86_64-linux" {
		t.Errorf("nokogiri: %+v", nk)
	}
	if !hasEdge(g, nk.ID(), PackageID(EcosystemRubyGems, "racc", "1.7.1")) {
		t.Error("nokogiri -> racc")
	}
	as := mustPkg(t, g, EcosystemRubyGems, "activesupport", "7.1.0.alpha")
	if !as.Direct || as.Resolved == "" {
		t.Errorf("git gem: %+v", as)
	}
	if rack := mustPkg(t, g, EcosystemRubyGems, "rack", "2.2.8"); len(rack.Hashes) != 1 {
		t.Error("rack checksum")
	}
	if _, err := ParseGemfileLock("x", []byte("BOGUS\n")); err == nil {
		t.Error("unknown section accepted")
	}
}

func TestParseComposerLock(t *testing.T) {
	g, err := ParseComposerLock("composer.lock", fixture(t, "php/composer.lock"), fixture(t, "php/composer.json"))
	if err != nil {
		t.Fatal(err)
	}
	hf := mustPkg(t, g, EcosystemPackagist, "symfony/http-foundation", "v5.4.20")
	if !hf.Direct || len(hf.Hashes) != 1 || hf.Licenses[0] != "MIT" || hf.PURL != "pkg:composer/symfony/http-foundation@v5.4.20" {
		t.Errorf("http-foundation: %+v", hf)
	}
	pm := mustPkg(t, g, EcosystemPackagist, "symfony/polyfill-mbstring", "v1.27.0")
	if pm.Direct || !hasEdge(g, hf.ID(), pm.ID()) {
		t.Error("polyfill should be transitive")
	}
	if pu := mustPkg(t, g, EcosystemPackagist, "phpunit/phpunit", "9.6.13"); !pu.Dev || !pu.Direct {
		t.Errorf("phpunit: %+v", pu)
	}
}

func TestScanWorkspace(t *testing.T) {
	dir := t.TempDir()
	copyFixtureTree(t, dir)
	res, err := ScanWorkspace(dir, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("scan errors: %v", res.Errors)
	}
	for _, want := range []string{"go/go.mod", "npm3/package-lock.json", "python/poetry.lock", "python/requirements-base.txt", "rust/Cargo.lock", "ruby/Gemfile.lock", "php/composer.lock", "yarn/yarn.lock", "pnpm9/pnpm-lock.yaml"} {
		if !slices.Contains(res.Manifests, want) {
			t.Errorf("manifest %s not scanned (have %v)", want, res.Manifests)
		}
	}
	if _, ok := res.InstallScripts["npm1/package.json"]; !ok {
		t.Error("workspace install script not reported")
	}
	mustPkg(t, res.Graph, EcosystemPyPI, "six", "1.16.0")
	if res.Graph.Len() < 40 {
		t.Errorf("graph has only %d packages", res.Graph.Len())
	}
}
