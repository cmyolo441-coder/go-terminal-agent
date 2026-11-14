package supplychain

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func sampleGraph(t *testing.T) *Graph {
	t.Helper()
	g, err := ParsePackageLock("package-lock.json", fixture(t, "npm3/package-lock.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	cargo, err := ParseCargoLock("Cargo.lock", fixture(t, "rust/Cargo.lock"))
	if err != nil {
		t.Fatal(err)
	}
	g.Merge(cargo)
	return g
}

func TestCycloneDX(t *testing.T) {
	g := sampleGraph(t)
	opts := SBOMOptions{Name: "web", Version: "1.0.0", Timestamp: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC), ToolVersion: "1.2.3"}
	a, err := CycloneDX(g, opts)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := CycloneDX(sampleGraph(t), opts)
	if !bytes.Equal(a, b) {
		t.Fatal("CycloneDX output is not deterministic")
	}
	var bom struct {
		BOMFormat, SpecVersion, SerialNumber string
		Metadata                             struct{ Timestamp string }
		Components                           []struct {
			BOMRef   string `json:"bom-ref"`
			Name     string
			PURL     string
			Scope    string
			Hashes   []struct{ Alg, Content string }
			Licenses []struct {
				License    *struct{ ID, Name string }
				Expression string
			}
		}
		Dependencies []struct {
			Ref       string
			DependsOn []string
		}
	}
	if err := json.Unmarshal(a, &bom); err != nil {
		t.Fatal(err)
	}
	if bom.BOMFormat != "CycloneDX" || bom.SpecVersion != "1.6" || bom.Metadata.Timestamp != "2026-05-01T12:00:00Z" {
		t.Errorf("header: %+v", bom)
	}
	if !regexp.MustCompile(`^urn:uuid:[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(bom.SerialNumber) {
		t.Errorf("serial %s", bom.SerialNumber)
	}
	refs := map[string]bool{}
	for i, c := range bom.Components {
		if i > 0 && bom.Components[i-1].BOMRef >= c.BOMRef {
			t.Error("components not sorted")
		}
		refs[c.BOMRef] = true
		switch c.Name {
		case "jest-lite":
			t.Error("dev package included by default")
		case "express":
			if len(c.Hashes) != 1 || c.Hashes[0].Alg != "SHA-512" || len(c.Hashes[0].Content) != 128 {
				t.Errorf("express hashes %+v", c.Hashes)
			}
			if c.Licenses[0].License == nil || c.Licenses[0].License.ID != "MIT" {
				t.Errorf("express license %+v", c.Licenses)
			}
		case "@scope/pkg":
			if c.Licenses[0].Expression != "MIT OR Apache-2.0" {
				t.Errorf("expression license %+v", c.Licenses)
			}
		}
	}
	for _, d := range bom.Dependencies {
		if d.Ref != "root:web" && !refs[d.Ref] {
			t.Errorf("dependency ref %s has no component", d.Ref)
		}
		for _, x := range d.DependsOn {
			if !refs[x] {
				t.Errorf("dependsOn %s has no component", x)
			}
		}
	}
	withDev, _ := CycloneDX(g, SBOMOptions{Timestamp: opts.Timestamp, IncludeDev: true})
	if !bytes.Contains(withDev, []byte("jest-lite")) {
		t.Error("IncludeDev ignored")
	}
}

func TestSPDX(t *testing.T) {
	g := sampleGraph(t)
	opts := SBOMOptions{Name: "web app", Version: "1.0.0", Timestamp: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)}
	a, err := SPDX(g, opts)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := SPDX(sampleGraph(t), opts)
	if !bytes.Equal(a, b) {
		t.Fatal("SPDX output is not deterministic")
	}
	var doc spdxDoc
	if err := json.Unmarshal(a, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.SPDXVersion != "SPDX-2.3" || doc.DataLicense != "CC0-1.0" || doc.CreationInfo.Created != "2026-05-01T12:00:00Z" ||
		!strings.HasPrefix(doc.DocumentNamespace, "https://spdx.org/spdxdocs/web-app-") {
		t.Errorf("header: %+v", doc)
	}
	idRe := regexp.MustCompile(`^SPDXRef-[A-Za-z0-9.-]+$`)
	ids := map[string]bool{"SPDXRef-DOCUMENT": true}
	for _, p := range doc.Packages {
		if !idRe.MatchString(p.SPDXID) || ids[p.SPDXID] {
			t.Errorf("bad or duplicate SPDXID %q", p.SPDXID)
		}
		ids[p.SPDXID] = true
		if p.Name == "serde" {
			if len(p.Checksums) != 1 || p.Checksums[0].Algorithm != "SHA256" || p.LicenseDeclared != "NOASSERTION" {
				t.Errorf("serde: %+v", p)
			}
			if p.ExternalRefs[0].Locator != "pkg:cargo/serde@1.0.190" {
				t.Errorf("serde purl ref %+v", p.ExternalRefs)
			}
		}
		if p.Name == "express" && (p.LicenseDeclared != "MIT" || p.DownloadLocation == "NOASSERTION") {
			t.Errorf("express: %+v", p)
		}
	}
	describes := false
	for _, r := range doc.Relationships {
		if !ids[r.Element] || !ids[r.Related] {
			t.Errorf("relationship references unknown element: %+v", r)
		}
		if r.Type == "DESCRIBES" {
			describes = true
		}
	}
	if !describes {
		t.Error("missing DESCRIBES relationship")
	}
}

func TestDamerauLevenshtein(t *testing.T) {
	cases := []struct {
		a, b string
		d    int
	}{
		{"", "", 0}, {"abc", "", 3}, {"lodash", "lodash", 0}, {"lodash", "lodahs", 1}, {"lodash", "lodas", 1},
		{"ca", "abc", 2}, // unrestricted DL (OSA gives 3)
		{"kitten", "sitting", 3}, {"requests", "reqeusts", 1}, {"héllo", "hlélo", 1},
	}
	for _, c := range cases {
		if got := DamerauLevenshtein(c.a, c.b); got != c.d {
			t.Errorf("DL(%q,%q)=%d want %d", c.a, c.b, got, c.d)
		}
		if got := DamerauLevenshtein(c.b, c.a); got != c.d {
			t.Errorf("DL symmetric (%q,%q)=%d", c.b, c.a, got)
		}
	}
}

func TestTyposquat(t *testing.T) {
	tc := NewTyposquatChecker(nil)
	flag := map[string][2]string{ // name -> eco, target
		"lodahs":        {"npm", "lodash"},
		"expresss":      {"npm", "express"},
		"@evil/lodash":  {"npm", "lodash"},
		"reqeusts":      {"PyPI", "requests"},
		"requests_":     {"", ""},
		"numpyy":        {"PyPI", "numpy"},
		"djang0":        {"PyPI", "django"},
		"crypt0graphy":  {"PyPI", "cryptography"},
		"rеquests":      {"PyPI", "requests"}, // Cyrillic е
		"beautifulsoup": {"PyPI", "beautifulsoup4"},
	}
	for name, want := range flag {
		eco := Ecosystem(want[0])
		if eco == "" {
			if m := tc.Check(EcosystemPyPI, name); m != nil {
				t.Errorf("%q normalises to a popular name, flagged: %+v", name, m)
			}
			continue
		}
		m := tc.Check(eco, name)
		if m == nil || m.Target != want[1] {
			t.Errorf("Check(%s,%q) = %+v, want target %s", eco, name, m, want[1])
		}
	}
	for _, ok := range []string{"lodash", "react", "@types/lodash", "my-internal-tool", "vue", "ws", "Flask"} {
		eco := EcosystemNPM
		if ok == "Flask" {
			eco = EcosystemPyPI
		}
		if m := tc.Check(eco, ok); m != nil {
			t.Errorf("false positive %q: %+v", ok, m)
		}
	}
}

func TestPolicyEvaluate(t *testing.T) {
	g, err := ParsePackageLock("package-lock.json", fixture(t, "npm3/package-lock.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	findings := []Finding{
		{PackageID: "npm/qs@6.11.0", Name: "qs", Version: "6.11.0", VulnID: "GHSA-1", Aliases: []string{"CVE-1"}, Severity: SeverityHigh, Score: 7.5},
		{PackageID: "npm/ms@2.0.0", Name: "ms", Version: "2.0.0", VulnID: "GHSA-2", Severity: SeverityLow},
		{PackageID: "npm/debug@2.6.9", Name: "debug", Version: "2.6.9", VulnID: "GHSA-3", Severity: SeverityUnknown},
	}
	scripts := map[string]map[string]string{"package.json": {"postinstall": "node x.js"}}
	p := DefaultPolicy()
	p.DenyLicenses = []string{"GPL-3.0", "Apache-2.0"}
	rep := p.Evaluate(g, findings, scripts)
	if rep.Passed {
		t.Fatal("policy should fail on HIGH")
	}
	rules := map[string][]Violation{}
	for _, v := range rep.Violations {
		rules[v.Rule] = append(rules[v.Rule], v)
	}
	if len(rules["vulnerability"]) != 3 || len(rep.Errors()) != 1 {
		t.Errorf("vulnerability violations: %+v", rules["vulnerability"])
	}
	if len(rules["license-denied"]) != 0 {
		// jest-lite (GPL) is dev; @scope/pkg is "MIT OR Apache-2.0" (satisfiable)
		t.Errorf("unexpected license violations: %+v", rules["license-denied"])
	}
	if len(rules["typosquat"]) != 1 || !strings.Contains(rules["typosquat"][0].Message, "lodash") {
		t.Errorf("typosquat: %+v", rules["typosquat"])
	}
	if len(rules["install-script"]) != 2 {
		t.Errorf("install scripts: %+v", rules["install-script"])
	}
	// ignore + dev licenses + unknown severity escalation
	p.IgnoreVulns = []string{"cve-1"}
	p.IncludeDev = true
	p.UnknownSeverityAs = SeverityCritical
	rep = p.Evaluate(g, findings, nil)
	errs := rep.Errors()
	var gotRules []string
	for _, e := range errs {
		gotRules = append(gotRules, e.Rule+":"+e.PackageID)
	}
	want := "license-denied:npm/jest-lite@1.2.0,vulnerability:npm/debug@2.6.9"
	if strings.Join(gotRules, ",") != want {
		t.Errorf("errors = %v, want %s", gotRules, want)
	}
	// clean graph passes
	clean := NewGraph()
	clean.Add(&Package{Ecosystem: EcosystemNPM, Name: "react", Version: "18.2.0", Direct: true, Licenses: []string{"MIT"}})
	if rep := DefaultPolicy().Evaluate(clean, nil, nil); !rep.Passed || len(rep.Violations) != 0 {
		t.Errorf("clean graph: %+v", rep)
	}
}

func TestDSSERoundTripAndTamper(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	art := filepath.Join(dir, "gta")
	content := []byte("binary contents")
	if err := os.WriteFile(art, content, 0o644); err != nil {
		t.Fatal(err)
	}
	subjects, err := SubjectsFromFiles(dir, art)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	if subjects[0].Name != "gta" || subjects[0].Digest["sha256"] != hex.EncodeToString(sum[:]) || len(subjects[0].Digest["sha512"]) != 128 {
		t.Fatalf("subjects: %+v", subjects)
	}
	g := sampleGraph(t)
	start := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	st, err := NewProvenance(ProvenanceInput{Subjects: subjects, BuilderID: "https://example.com/builder@v1",
		ExternalParameters: map[string]any{"target": "./cmd/gta"}, Graph: g, InvocationID: "run-1",
		StartedOn: start, FinishedOn: start.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Predicate.BuildDefinition.ResolvedDependencies) == 0 {
		t.Error("resolved dependencies from graph missing")
	}
	env, err := SignProvenance(st, priv)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(env)
	var decoded Envelope
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	got, err := VerifyProvenance(&decoded, []ed25519.PublicKey{pub}, map[string][]byte{"gta": content})
	if err != nil {
		t.Fatal(err)
	}
	if got.Predicate.RunDetails.Builder.ID != "https://example.com/builder@v1" || got.Subject[0].Name != "gta" {
		t.Errorf("statement: %+v", got)
	}
	// PAE vector from the DSSE specification.
	if string(PAE("http://example.com/HelloWorld", []byte("hello world"))) != "DSSEv1 29 http://example.com/HelloWorld 11 hello world" {
		t.Error("PAE encoding")
	}

	// tampered payload
	payload, _ := decoded.DecodePayload()
	tampered := bytes.Replace(payload, []byte("run-1"), []byte("run-2"), 1)
	bad := decoded
	bad.Payload = base64.StdEncoding.EncodeToString(tampered)
	if _, err := VerifyProvenance(&bad, []ed25519.PublicKey{pub}, nil); err == nil {
		t.Error("tampered payload verified")
	}
	// tampered payload type
	bad = decoded
	bad.PayloadType = "application/json"
	if _, err := VerifyEnvelope(&bad, []ed25519.PublicKey{pub}, 1); err == nil {
		t.Error("payload type is not bound by the signature")
	}
	// wrong key
	otherPub, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := VerifyProvenance(&decoded, []ed25519.PublicKey{otherPub}, nil); err == nil {
		t.Error("verified with an untrusted key")
	}
	// corrupted signature
	bad = decoded
	sig, _ := base64.StdEncoding.DecodeString(decoded.Signatures[0].Sig)
	sig[0] ^= 1
	bad.Signatures = []Signature{{KeyID: decoded.Signatures[0].KeyID, Sig: base64.StdEncoding.EncodeToString(sig)}}
	if _, err := VerifyEnvelope(&bad, []ed25519.PublicKey{pub}, 1); err == nil {
		t.Error("corrupted signature verified")
	}
	// artifact mismatch
	if _, err := VerifyProvenance(&decoded, []ed25519.PublicKey{pub}, map[string][]byte{"gta": []byte("evil")}); err == nil {
		t.Error("modified artifact verified")
	}
	// threshold: two distinct signers
	multi := decoded
	multi.Signatures = append([]Signature(nil), decoded.Signatures...)
	if err := multi.AddSignature(otherPriv); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyEnvelope(&multi, []ed25519.PublicKey{pub, otherPub}, 2); err != nil {
		t.Errorf("2-of-2: %v", err)
	}
	if _, err := VerifyEnvelope(&decoded, []ed25519.PublicKey{pub, otherPub}, 2); err == nil {
		t.Error("threshold not enforced")
	}
	dup := decoded
	dup.Signatures = append(append([]Signature(nil), decoded.Signatures...), decoded.Signatures...)
	if _, err := VerifyEnvelope(&dup, []ed25519.PublicKey{pub, otherPub}, 2); err == nil {
		t.Error("duplicate signatures counted twice")
	}
	if _, err := NewProvenance(ProvenanceInput{BuilderID: "x"}); err == nil {
		t.Error("provenance without subjects accepted")
	}
}

func TestPURL(t *testing.T) {
	cases := []struct {
		eco           Ecosystem
		name, version string
		q             map[string]string
		want          string
	}{
		{EcosystemNPM, "@angular/core", "16.0.0", nil, "pkg:npm/%40angular/core@16.0.0"},
		{EcosystemPyPI, "Django_REST.framework", "3.14.0", nil, "pkg:pypi/django-rest-framework@3.14.0"},
		{EcosystemGo, "github.com/gorilla/mux", "v1.8.0", nil, "pkg:golang/github.com/gorilla/mux@v1.8.0"},
		{EcosystemPackagist, "Symfony/Console", "v6.0.0", nil, "pkg:composer/symfony/console@v6.0.0"},
		{EcosystemRubyGems, "nokogiri", "1.15.4", map[string]string{"platform": "x86_64-linux", "empty": ""}, "pkg:gem/nokogiri@1.15.4?platform=x86_64-linux"},
		{EcosystemPyPI, "foo", "1!2.0+local", nil, "pkg:pypi/foo@1%212.0%2Blocal"},
		{EcosystemCargo, "a b", "1.0.0", map[string]string{"b": "x/y", "a": "1"}, "pkg:cargo/a%20b@1.0.0?a=1&b=x%2Fy"},
	}
	for _, c := range cases {
		p := PURLFor(c.eco, c.name, c.version, c.q)
		if got := p.String(); got != c.want {
			t.Errorf("purl(%s) = %s, want %s", c.name, got, c.want)
		}
		back, err := ParsePURL(c.want)
		if err != nil {
			t.Fatalf("parse %s: %v", c.want, err)
		}
		if back.String() != c.want {
			t.Errorf("round trip %s -> %s", c.want, back.String())
		}
	}
	p, err := ParsePURL("pkg:maven/org.apache/commons@1.0?Classifier=dist#sub/./path/")
	if err != nil || p.Namespace != "org.apache" || p.Qualifiers["classifier"] != "dist" || p.Subpath != "sub/path" {
		t.Errorf("parse: %+v %v", p, err)
	}
	for _, bad := range []string{"npm/foo", "pkg:", "pkg:npm", "pkg:1npm/foo", "pkg:npm/foo@", "pkg:npm/%zz", "pkg:npm/foo?=x", "pkg:npm/foo?a=1&a=2"} {
		if _, err := ParsePURL(bad); err == nil {
			t.Errorf("ParsePURL(%q) accepted", bad)
		}
	}
}
