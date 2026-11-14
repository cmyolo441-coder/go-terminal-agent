package supplychain

import (
	"archive/zip"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func loadOfflineDB(t *testing.T) *OSVDatabase {
	t.Helper()
	db := NewOSVDatabase()
	entries, err := fs.ReadDir(testdataFS, "testdata/osv")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := db.LoadJSON(fixture(t, "osv/"+e.Name())); err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
	}
	return db
}

func TestOSVRangeEvaluation(t *testing.T) {
	a := OSVAffected{Package: OSVPackage{Ecosystem: "npm", Name: "qs"}, Ranges: []OSVRange{{Type: "SEMVER", Events: []OSVEvent{
		{Introduced: "6.11.0"}, {Fixed: "6.11.1"}, {Introduced: "6.10.0"}, {Fixed: "6.10.3"}, // unsorted on purpose
	}}}}
	cases := map[string]bool{"6.9.9": false, "6.10.0": true, "6.10.2": true, "6.10.3": false, "6.11.0": true,
		"6.11.1": false, "6.11.1-rc.1": true, "7.0.0": false}
	for v, want := range cases {
		got, err := a.Affects(EcosystemNPM, v)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("qs %s affected=%v want %v", v, got, want)
		}
	}
	la := OSVAffected{Ranges: []OSVRange{{Type: "ECOSYSTEM", Events: []OSVEvent{{Introduced: "0"}, {LastAffected: "2.0.0"}}}}}
	for v, want := range map[string]bool{"0.0.1": true, "2.0.0": true, "2.0.0.post1": false, "2.0.1": false, "2.0.0rc1": true} {
		if got, _ := la.Affects(EcosystemPyPI, v); got != want {
			t.Errorf("last_affected pypi %s = %v want %v", v, got, want)
		}
	}
	lim := OSVAffected{Ranges: []OSVRange{{Type: "SEMVER", Events: []OSVEvent{{Introduced: "1.0.0"}, {Limit: "1.5.0"}}}}}
	for v, want := range map[string]bool{"1.2.0": true, "1.5.0": false, "2.0.0": false} {
		if got, _ := lim.Affects(EcosystemCargo, v); got != want {
			t.Errorf("limit %s = %v want %v", v, got, want)
		}
	}
	explicit := OSVAffected{Versions: []string{"1.0"}}
	if got, _ := explicit.Affects(EcosystemPyPI, "1.0.0"); !got {
		t.Error("explicit versions compare with ecosystem semantics")
	}
	goPseudo := OSVAffected{Ranges: []OSVRange{{Type: "SEMVER", Events: []OSVEvent{{Introduced: "0"}, {Fixed: "0.0.0-20200101000000-abcdefabcdef"}}}}}
	if got, _ := goPseudo.Affects(EcosystemGo, "v0.0.0-20191109021931-daa7c04131f5"); !got {
		t.Error("older pseudo-version should be affected")
	}
	php := OSVAffected{Ranges: []OSVRange{{Type: "ECOSYSTEM", Events: []OSVEvent{{Introduced: "5.0.0"}, {Fixed: "5.4.21"}}}}}
	if got, _ := php.Affects(EcosystemPackagist, "v5.4.20"); !got {
		t.Error("composer v-prefixed version")
	}
}

func TestOfflineAuditAndReachability(t *testing.T) {
	db := loadOfflineDB(t)
	if db.Len() != 6 {
		t.Fatalf("loaded %d advisories", db.Len())
	}
	g := NewGraph()
	mod, err := ParseGoMod("go.mod", fixture(t, "go/go.mod.golden"))
	if err != nil {
		t.Fatal(err)
	}
	g.Merge(GoGraph("go.mod", mod, nil))
	npm, err := ParsePackageLock("package-lock.json", fixture(t, "npm3/package-lock.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	g.Merge(npm)
	py := RequirementsGraph("r.txt", &RequirementsFile{Requirements: []Requirement{{Name: "Django", Specifier: "==4.2.1"}}})
	g.Merge(py)
	cargo, err := ParseCargoLock("Cargo.lock", fixture(t, "rust/Cargo.lock"))
	if err != nil {
		t.Fatal(err)
	}
	g.Merge(cargo)

	dir := t.TempDir()
	copyFixtureTree(t, dir)
	ix, err := IndexGoImports(filepath.Join(dir, "go"))
	if err != nil {
		t.Fatal(err)
	}
	if !ix.Selectors["gopkg.in/yaml.v3"]["Unmarshal"] {
		t.Fatalf("aliased import selectors not indexed: %v", ix.Selectors)
	}
	findings, err := Audit(context.Background(), g, db, AuditOptions{GoImports: ix})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Finding{}
	for _, f := range findings {
		got[f.VulnID+" "+f.PackageID] = f
	}
	mux, ok := got["GO-2023-0002 Go/github.com/gorilla/mux@v1.8.0"]
	if !ok || mux.Severity != SeverityCritical || mux.Score != 9.8 || mux.Reachability != ReachImported {
		t.Errorf("mux finding: %+v (all: %v)", mux, keys(got))
	}
	qs, ok := got["GHSA-test-qs npm/qs@6.11.0"]
	if !ok || qs.Severity != SeverityHigh || len(qs.Fixed) != 2 || len(qs.Path) == 0 {
		t.Errorf("qs finding: %+v", qs)
	}
	if _, ok := got["GHSA-test-qs npm/qs@6.5.0"]; ok {
		t.Error("qs 6.5.0 is not affected")
	}
	if _, ok := got["PYSEC-test-django PyPI/django@4.2.1"]; !ok {
		t.Error("django finding missing")
	}
	if f, ok := got["RUSTSEC-test-time crates.io/time@0.1.45"]; !ok || f.Severity != SeverityHigh {
		t.Errorf("time finding: %+v", f)
	}
	if _, ok := got["GO-2022-0001 Go/gopkg.in/yaml.v3@v3.0.1"]; ok {
		t.Error("yaml v3.0.1 is fixed")
	}
	for k := range got {
		if strings.HasPrefix(k, "WITHDRAWN") {
			t.Error("withdrawn advisory matched")
		}
	}
	if findings[0].Severity != SeverityCritical {
		t.Error("findings must be sorted by severity")
	}
	// symbol-level reachability
	yaml := &OSVAffected{Package: OSVPackage{Ecosystem: "Go", Name: "gopkg.in/yaml.v3"},
		EcosystemSpecific: json.RawMessage(`{"imports":[{"path":"gopkg.in/yaml.v3","symbols":["Unmarshal"]}]}`)}
	if r := ix.Reachability(yaml); r != ReachSymbolUsed {
		t.Errorf("yaml reachability = %s", r)
	}
	other := &OSVAffected{Package: OSVPackage{Ecosystem: "Go", Name: "golang.org/x/text"}}
	if r := ix.Reachability(other); r != ReachNotImported {
		t.Errorf("x/text reachability = %s", r)
	}
}

func keys(m map[string]Finding) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestOfflineZipDump(t *testing.T) {
	dir := t.TempDir()
	zp := filepath.Join(dir, "all.zip")
	f, err := os.Create(zp)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, name := range []string{"GHSA-test-qs.json", "GO-2023-0002.json"} {
		w, _ := zw.Create(name)
		w.Write(fixture(t, "osv/"+name))
	}
	zw.Close()
	f.Close()
	db := NewOSVDatabase()
	if err := db.LoadPath(dir); err != nil {
		t.Fatal(err)
	}
	if db.Len() != 2 {
		t.Fatalf("zip load: %d", db.Len())
	}
	res, err := db.Query(context.Background(), []PackageQuery{{EcosystemNPM, "qs", "6.10.1"}, {EcosystemNPM, "qs", "6.10.3"}})
	if err != nil || len(res[0]) != 1 || len(res[1]) != 0 {
		t.Fatalf("query: %v %v", res, err)
	}
}

type fakeOSV struct {
	batchCalls, vulnCalls atomic.Int32
	failFirst             atomic.Bool
}

func (f *fakeOSV) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/querybatch", func(w http.ResponseWriter, r *http.Request) {
		f.batchCalls.Add(1)
		if f.failFirst.CompareAndSwap(true, false) {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content type %q", r.Header.Get("Content-Type"))
		}
		var req osvBatchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		type vuln struct {
			ID string `json:"id"`
		}
		type result struct {
			Vulns         []vuln `json:"vulns,omitempty"`
			NextPageToken string `json:"next_page_token,omitempty"`
		}
		var out struct {
			Results []result `json:"results"`
		}
		for _, q := range req.Queries {
			var res result
			switch {
			case q.Package.Name == "qs" && q.Version == "6.11.0" && q.PageToken == "":
				res = result{Vulns: []vuln{{"GHSA-test-qs"}}, NextPageToken: "page2"}
			case q.Package.Name == "qs" && q.PageToken == "page2":
				res = result{Vulns: []vuln{{"GHSA-extra"}}}
			case q.Package.Name == "symfony/http-foundation" && q.Version != "5.4.20":
				t.Errorf("packagist version not normalised: %q", q.Version)
			}
			out.Results = append(out.Results, res)
		}
		json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("GET /v1/vulns/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.vulnCalls.Add(1)
		id := r.PathValue("id")
		switch id {
		case "GHSA-test-qs":
			w.Write(fixture(t, "osv/GHSA-test-qs.json"))
		case "GHSA-extra":
			json.NewEncoder(w).Encode(OSVVulnerability{ID: id, Summary: "extra", Affected: []OSVAffected{{Package: OSVPackage{Ecosystem: "npm", Name: "qs"}, Versions: []string{"6.11.0"}}}})
		default:
			http.NotFound(w, r)
		}
	})
	return mux
}

func TestOSVClientQueryBatch(t *testing.T) {
	fake := &fakeOSV{}
	fake.failFirst.Store(true)
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cache := &DiskCache{Dir: t.TempDir(), TTL: time.Hour, Now: func() time.Time { return now }}
	c := &OSVClient{BaseURL: srv.URL, HTTPClient: srv.Client(), Cache: cache, BatchSize: 2}
	qs := []PackageQuery{
		{EcosystemNPM, "qs", "6.11.0"}, {EcosystemNPM, "ms", "2.0.0"},
		{EcosystemPackagist, "symfony/http-foundation", "v5.4.20"},
	}
	res, err := c.Query(context.Background(), qs)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 3 || len(res[0]) != 2 || len(res[1]) != 0 || len(res[2]) != 0 {
		t.Fatalf("results: %v", res)
	}
	if res[0][0].ID != "GHSA-extra" || res[0][1].ID != "GHSA-test-qs" {
		t.Errorf("ids: %s %s", res[0][0].ID, res[0][1].ID)
	}
	batches, vulns := fake.batchCalls.Load(), fake.vulnCalls.Load()
	if batches != 3 { // 1 failed + retry, then composer query batched with the qs page-2 query
		t.Errorf("batch calls = %d", batches)
	}
	// second run is served from cache
	if _, err := c.Query(context.Background(), qs); err != nil {
		t.Fatal(err)
	}
	if fake.batchCalls.Load() != batches || fake.vulnCalls.Load() != vulns {
		t.Error("cache not used")
	}
	// expire the cache
	now = now.Add(2 * time.Hour)
	if _, err := c.Query(context.Background(), qs); err != nil {
		t.Fatal(err)
	}
	if fake.batchCalls.Load() == batches {
		t.Error("expired cache entries were used")
	}
	// Audit through the online source
	g, err := ParsePackageLock("package-lock.json", fixture(t, "npm3/package-lock.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := Audit(context.Background(), g, c, AuditOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 2 || findings[0].Severity != SeverityHigh {
		t.Errorf("findings: %+v", findings)
	}
}

func TestOSVClientErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/querybatch" {
			io.WriteString(w, `{"results": []}`)
			return
		}
		http.Error(w, "bad", http.StatusBadRequest)
	}))
	defer srv.Close()
	c := &OSVClient{BaseURL: srv.URL, HTTPClient: srv.Client(), MaxRetries: 1}
	if _, err := c.QueryIDs(context.Background(), []PackageQuery{{EcosystemNPM, "a", "1.0.0"}}); err == nil {
		t.Error("result count mismatch not detected")
	}
	if _, err := c.GetVuln(context.Background(), "X"); err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("status error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.GetVuln(ctx, "X"); err == nil {
		t.Error("cancelled context ignored")
	}
}

func TestVulnSeverityFallbacks(t *testing.T) {
	v := &OSVVulnerability{DatabaseSpecific: json.RawMessage(`{"severity":"MODERATE"}`)}
	if s, _, _ := VulnSeverity(v, nil); s != SeverityMedium {
		t.Errorf("db severity = %s", s)
	}
	v = &OSVVulnerability{Severity: []OSVSeverity{{Type: "CVSS_V3", Score: "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:L/I:N/A:N"}}}
	a := &OSVAffected{Severity: []OSVSeverity{{Type: "CVSS_V3", Score: "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H"}}}
	if s, sc, _ := VulnSeverity(v, a); s != SeverityCritical || sc != 9.8 {
		t.Errorf("max severity = %s %.1f", s, sc)
	}
	if s, _, _ := VulnSeverity(&OSVVulnerability{}, nil); s != SeverityUnknown {
		t.Error("unknown")
	}
}
