package codeintel

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

// sampleRepo is a small multi-language project used by the ranking tests.
var sampleRepo = map[string]string{
	"internal/permission/permission.go": `package permission

// Verdict is the outcome of a permission check.
type Verdict int

const (
	Allow Verdict = iota
	Ask
	Deny
)

// Gate holds the permission rules for a session.
type Gate struct {
	mode string
}

// Check decides whether a tool call may run, must be denied, or needs the
// user to approve it at an interactive prompt.
func (g *Gate) Check(kind string) Verdict {
	if g.mode == "yolo" {
		return Allow
	}
	if kind == "read" {
		return Allow
	}
	return Ask
}
`,
	"internal/permission/permission_test.go": `package permission

import "testing"

func TestCheckPrompt(t *testing.T) {
	g := &Gate{}
	if g.Check("write") != Ask {
		t.Fatal("expected a permission prompt")
	}
}
`,
	"internal/config/config.go": `package config

import "os"

// Config is the user configuration.
type Config struct {
	Model string
}

// Load reads the configuration file from disk and applies defaults.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseConfig(data)
}

func parseConfig(data []byte) (*Config, error) {
	return &Config{Model: string(data)}, nil
}
`,
	"internal/agent/agent.go": `package agent

// Agent runs the conversation loop and executes tool calls.
type Agent struct{}

// Run sends the prompt to the model and executes the tools it requests.
func (a *Agent) Run(prompt string) error {
	return nil
}
`,
	"pkg/httpserver/server.go": `package httpserver

// HTTPServer wraps net/http with graceful shutdown.
type HTTPServer struct{ addr string }

// ListenAndServe starts listening for HTTP connections on the address.
func (s *HTTPServer) ListenAndServe() error { return nil }

// Shutdown stops the server gracefully.
func (s *HTTPServer) Shutdown() error { return nil }
`,
	"web/src/auth.ts": `// Authentication helpers for the web client.
export async function login(user: string, password: string): Promise<string> {
  const res = await fetch("/api/login", { method: "POST" });
  return res.text();
}

export function logout(): void {}
`,
	"scripts/deploy.py": `"""Deployment script."""


def deploy_release(version):
    """Upload the release artifacts and restart the service."""
    upload(version)


def upload(version):
    pass
`,
	"README.md": "# Sample\n\nThis project shows how permission prompts and configuration loading work.\n",
	"internal/noise/noise.go": `package noise

// Unrelated computes unrelated values.
func Unrelated(a, b int) int { return a + b }

// Another adds more noise about counters.
func Another() int { return 42 }
`,
}

func sampleIndex(t *testing.T) *Index {
	t.Helper()
	root := t.TempDir()
	writeTree(t, root, sampleRepo)
	ix := openIndex(t, root, nil)
	mustRefresh(t, ix)
	return ix
}

func topHit(t *testing.T, ix *Index, q string, opts SearchOptions) Hit {
	t.Helper()
	hits, err := ix.Search(context.Background(), q, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatalf("no hits for %q", q)
	}
	return hits[0]
}

func TestSearchRanksNaturalLanguageQueries(t *testing.T) {
	ix := sampleIndex(t)
	cases := []struct {
		query, path, symbol string
	}{
		{"where is the permission prompt decided", "internal/permission/permission.go", "Gate.Check"},
		{"load configuration from disk", "internal/config/config.go", "Load"},
		{"parseConfig", "internal/config/config.go", "parseConfig"},
		{"http server listen", "pkg/httpserver/server.go", "HTTPServer.ListenAndServe"},
		{"how does the user log in", "web/src/auth.ts", "login"},
		{"restart service after release upload", "scripts/deploy.py", "deploy_release"},
		{"Gate.Check", "internal/permission/permission.go", "Gate.Check"},
		{"stop server gracefully", "pkg/httpserver/server.go", "HTTPServer.Shutdown"},
		{"sign out the user", "web/src/auth.ts", "logout"},
	}
	for _, c := range cases {
		h := topHit(t, ix, c.query, SearchOptions{})
		got := ""
		if h.Symbol != nil {
			got = h.Symbol.QualifiedName()
		}
		if h.Path != c.path || got != c.symbol {
			t.Errorf("%q: top hit %s %s, want %s %s", c.query, h.Path, got, c.path, c.symbol)
		}
		if len(h.Snippet) == 0 {
			t.Errorf("%q: no snippet", c.query)
		}
	}
}

func TestSearchOptionsAndSnippets(t *testing.T) {
	ix := sampleIndex(t)
	ctx := context.Background()
	// Path restriction by directory prefix and by glob.
	for _, p := range []string{"web", "web/", "./web", "web/**/*.ts", "*.ts"} {
		hits, err := ix.Search(ctx, "login permission configuration", SearchOptions{Path: p})
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) == 0 {
			t.Fatalf("path %q: no hits", p)
		}
		for _, h := range hits {
			if !strings.HasPrefix(h.Path, "web/") {
				t.Errorf("path %q: hit outside filter: %s", p, h.Path)
			}
		}
	}
	if _, err := ix.Search(ctx, "x", SearchOptions{Path: "[bad"}); err == nil {
		t.Error("invalid glob accepted")
	}
	if _, err := ix.Search(ctx, "   ", SearchOptions{}); err == nil {
		t.Error("empty query accepted")
	}
	// Limits.
	hits, _ := ix.Search(ctx, "func return", SearchOptions{Limit: 2})
	if len(hits) > 2 {
		t.Errorf("limit ignored: %d", len(hits))
	}
	hits, _ = ix.Search(ctx, "permission", SearchOptions{MaxPerFile: 1, Limit: 50})
	seen := map[string]bool{}
	for _, h := range hits {
		if seen[h.Path] {
			t.Errorf("MaxPerFile ignored for %s", h.Path)
		}
		seen[h.Path] = true
	}
	// Test files rank below the code they test unless tests are asked for.
	h := topHit(t, ix, "check permission prompt", SearchOptions{})
	if strings.Contains(h.Path, "_test") {
		t.Errorf("test file ranked first: %s", h.Path)
	}
	h = topHit(t, ix, "test check prompt", SearchOptions{})
	if !strings.Contains(h.Path, "_test") {
		t.Errorf("test query did not prefer tests: %s", h.Path)
	}
	// Snippet lines carry real line numbers and mark matches.
	h = topHit(t, ix, "ListenAndServe", SearchOptions{})
	found := false
	for _, l := range h.Snippet {
		if strings.Contains(l.Text, "func (s *HTTPServer) ListenAndServe()") {
			found = true
			if !l.Match || l.Line != 7 {
				t.Errorf("snippet line = %+v", l)
			}
		}
	}
	if !found {
		t.Errorf("definition line missing from snippet: %+v", h.Snippet)
	}
	// Nothing matches.
	if hits, err := ix.Search(ctx, "zzqxj", SearchOptions{}); err != nil || len(hits) != 0 {
		t.Errorf("hits = %v err = %v", hits, err)
	}
	// All-filler queries still search.
	if hits, err := ix.Search(ctx, "where is it", SearchOptions{}); err != nil {
		t.Errorf("filler query: %v %v", hits, err)
	}
}

func TestSnippetFollowsEditsOnDisk(t *testing.T) {
	ix := sampleIndex(t)
	// Shrinking a file below an indexed range must not panic.
	abs := ix.Root() + "/internal/config/config.go"
	if err := os.WriteFile(abs, []byte("package config\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.Search(context.Background(), "parseConfig", SearchOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestSymbolsQuery(t *testing.T) {
	ix := sampleIndex(t)
	ctx := context.Background()
	names := func(ms []SymbolMatch) []string {
		var out []string
		for _, m := range ms {
			out = append(out, m.QualifiedName())
		}
		return out
	}
	ms, err := ix.Symbols(ctx, SymbolQuery{Name: "Check"})
	if err != nil || len(ms) == 0 || ms[0].QualifiedName() != "Gate.Check" || ms[0].Path != "internal/permission/permission.go" {
		t.Fatalf("Check: %v %v", names(ms), err)
	}
	ms, _ = ix.Symbols(ctx, SymbolQuery{Name: "HTTPServer.Shut"})
	if len(ms) != 1 || ms[0].QualifiedName() != "HTTPServer.Shutdown" {
		t.Errorf("qualified: %v", names(ms))
	}
	ms, _ = ix.Symbols(ctx, SymbolQuery{Name: "LAS"})
	if len(ms) == 0 || ms[0].Name != "ListenAndServe" {
		t.Errorf("abbreviation: %v", names(ms))
	}
	ms, _ = ix.Symbols(ctx, SymbolQuery{Name: "config", Kind: KindFunction})
	for _, m := range ms {
		if m.Kind != KindFunction {
			t.Errorf("kind filter leaked %v", m.Kind)
		}
	}
	if len(ms) == 0 || ms[0].Name != "parseConfig" {
		t.Errorf("kind filtered: %v", names(ms))
	}
	ms, _ = ix.Symbols(ctx, SymbolQuery{Path: "web/src/auth.ts"})
	if strings.Join(names(ms), ",") != "login,logout" && strings.Join(names(ms), ",") != "logout,login" {
		t.Errorf("by path: %v", names(ms))
	}
	ms, _ = ix.Symbols(ctx, SymbolQuery{Name: "parse", ExportedOnly: true})
	for _, m := range ms {
		if !m.Exported {
			t.Errorf("unexported %s returned", m.Name)
		}
	}
	ms, _ = ix.Symbols(ctx, SymbolQuery{Name: "a", Limit: 2})
	if len(ms) > 2 {
		t.Errorf("limit ignored")
	}
	if _, err := ix.Symbols(ctx, SymbolQuery{}); err == nil {
		t.Error("empty query accepted")
	}
	fs, err := ix.FileSymbols(ctx, "internal/permission/permission.go")
	if err != nil || len(fs) < 4 {
		t.Errorf("FileSymbols: %v %v", fs, err)
	}
	if _, err := ix.FileSymbols(ctx, "/etc/passwd"); err == nil {
		t.Error("outside path accepted")
	}
}

func TestNameScore(t *testing.T) {
	order := []string{"Load", "load", "LoadConfig", "configLoader", "xloadx", "LxOxAxD"}
	prev := 1000.0
	for _, n := range order {
		s := nameScore(n, "Load")
		if n == "configLoader" {
			s = nameScore(n, "load")
		}
		if s <= 0 || s > prev {
			t.Errorf("nameScore(%q) = %v not below %v", n, s, prev)
		}
		prev = s
	}
	if nameScore("Other", "Load") != 0 {
		t.Error("unrelated name matched")
	}
	if !camelAbbrev("NewRegistry", "NewReg") || !camelAbbrev("HTTPServer", "hs") || camelAbbrev("Server", "hs") {
		t.Error("camelAbbrev")
	}
}

func TestRepoMap(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"core/store.go": "package core\n\n// DocumentStore persists documents.\ntype DocumentStore struct{}\n\n// SaveDocument writes one document.\nfunc (s *DocumentStore) SaveDocument() error { return nil }\n\nfunc NewDocumentStore() *DocumentStore { return nil }\n",
		"leaf/leaf.go":  "package leaf\n\nfunc LonelyHelper() {}\n",
	}
	for i := 0; i < 8; i++ {
		files["users/u"+string(rune('a'+i))+".go"] = "package users\n\nfunc UseIt() { s := core.NewDocumentStore(); s.SaveDocument(); var _ core.DocumentStore }\n"
	}
	writeTree(t, root, files)
	ix := openIndex(t, root, nil)
	mustRefresh(t, ix)
	ctx := context.Background()
	m, err := ix.RepoMap(ctx, RepoMapOptions{Budget: 400})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(m, "core/store.go\n") {
		t.Errorf("central file not first:\n%s", m)
	}
	if !strings.Contains(m, "type DocumentStore struct") || !strings.Contains(m, "func (s *DocumentStore) SaveDocument() error") {
		t.Errorf("key symbols missing:\n%s", m)
	}
	// Focus pulls a leaf to the top.
	m, _ = ix.RepoMap(ctx, RepoMapOptions{Budget: 400, Focus: []string{"leaf"}})
	if !strings.HasPrefix(m, "leaf/leaf.go\n") {
		t.Errorf("focus ignored:\n%s", m)
	}
	// The budget is respected and the rest is summarised.
	for _, budget := range []int{20, 60, 150} {
		m, _ = ix.RepoMap(ctx, RepoMapOptions{Budget: budget})
		if got := estimateTokens(m); got > budget {
			t.Errorf("budget %d exceeded: %d tokens\n%s", budget, got, m)
		}
		if budget < 100 && !strings.Contains(m, "more files not shown") {
			t.Errorf("budget %d: no trailer\n%s", budget, m)
		}
	}
	ranked, err := ix.RankedFiles(ctx, 1)
	if err != nil || len(ranked) != 1 || ranked[0] != "core/store.go" {
		t.Errorf("RankedFiles = %v %v", ranked, err)
	}
	empty := openIndex(t, t.TempDir(), nil)
	if m, err := empty.RepoMap(ctx, RepoMapOptions{}); err != nil || !strings.Contains(m, "no source files") {
		t.Errorf("empty map = %q %v", m, err)
	}
}

func TestCacheRoundTripAndCorruption(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, sampleRepo)
	cacheDir := t.TempDir()
	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, format) }
	ix := openIndex(t, root, func(o *Options) { o.CacheDir = cacheDir; o.Logf = logf })
	want := mustRefresh(t, ix)
	path := ix.CachePath()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cache not written: %v", err)
	}

	warm := openIndex(t, root, func(o *Options) { o.CacheDir = cacheDir; o.Logf = logf })
	st := warm.Stats()
	if !st.FromCache || st.Files != want.Files || st.Symbols != want.Symbols {
		t.Fatalf("warm stats %+v, want %+v", st, want)
	}
	if h := topHit(t, warm, "permission prompt decided", SearchOptions{}); h.Symbol == nil || h.Symbol.Name != "Check" {
		t.Errorf("warm search = %+v", h)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	corruptions := map[string][]byte{
		"flipped byte": func() []byte { d := append([]byte(nil), data...); d[len(d)-10] ^= 0xff; return d }(),
		"truncated":    data[:len(data)/2],
		"short header": data[:10],
		"bad magic":    append([]byte("NOTMAGIC"), data[8:]...),
		"huge length": func() []byte {
			d := append([]byte(nil), data...)
			binary.BigEndian.PutUint64(d[12:20], 1<<40)
			return d
		}(),
		"trailing data": append(append([]byte(nil), data...), 'x'),
		"empty":         {},
	}
	for name, bad := range corruptions {
		logs = nil
		if err := os.WriteFile(path, bad, 0o644); err != nil {
			t.Fatal(err)
		}
		cold := openIndex(t, root, func(o *Options) { o.CacheDir = cacheDir; o.Logf = logf })
		if cold.Stats().FromCache {
			t.Errorf("%s: corrupted cache was loaded", name)
		}
		if len(logs) == 0 {
			t.Errorf("%s: corruption not reported", name)
		}
		st := mustRefresh(t, cold)
		if st.Files != want.Files || st.Parsed != want.Files {
			t.Errorf("%s: rebuild stats %+v", name, st)
		}
	}
	// A different version or setting is a silent miss.
	os.WriteFile(path, data, 0o644)
	logs = nil
	other := openIndex(t, root, func(o *Options) { o.CacheDir = cacheDir; o.Logf = logf; o.IncludeGenerated = true })
	if other.Stats().FromCache || len(logs) != 0 {
		t.Errorf("settings change: fromCache=%v logs=%v", other.Stats().FromCache, logs)
	}
	ver := append([]byte(nil), data...)
	binary.BigEndian.PutUint32(ver[8:12], cacheVersion+1)
	os.WriteFile(path, ver, 0o644)
	logs = nil
	if v := openIndex(t, root, func(o *Options) { o.CacheDir = cacheDir; o.Logf = logf }); v.Stats().FromCache || len(logs) != 0 {
		t.Error("future cache version should be a silent miss")
	}
}

func TestTokenizer(t *testing.T) {
	cases := map[string][]string{
		"HTTPServerError": {"http", "server", "error"},
		"parse_config2":   {"parse", "config2"},
		"getURLForUser":   {"get", "url", "for", "user"},
		"already":         {"already"},
		"kebab-case-name": {"kebab", "case", "name"},
		"ÜberCoolName":    {"über", "cool", "name"},
		"__dunder__":      {"dunder"},
		"$scope.value":    {"scope", "value"},
		"Vec3D":           {"vec3", "d"},
		"IOError":         {"io", "error"},
		"":                nil,
	}
	for in, want := range cases {
		if got := SplitIdentifier(in); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("SplitIdentifier(%q) = %v, want %v", in, got, want)
		}
	}
	stems := [][2]string{
		{"decided", "decision"}, {"permission", "permit"}, {"parsing", "parser"}, {"parse", "parsed"},
		{"running", "run"}, {"files", "file"}, {"entries", "entry"}, {"checks", "checking"},
		{"selection", "select"}, {"connection", "connect"}, {"handler", "handle"},
	}
	for _, p := range stems {
		if Stem(p[0]) != Stem(p[1]) {
			t.Errorf("Stem(%q)=%q != Stem(%q)=%q", p[0], Stem(p[0]), p[1], Stem(p[1]))
		}
	}
	for _, w := range []string{"add", "use", "status", "class", "user", "utf8"} {
		if s := Stem(w); s != Stem(s) && len(s) < 3 {
			t.Errorf("Stem(%q) = %q", w, s)
		}
	}
	if Stem("add") != "add" || Stem("status") != "status" || Stem("user") != "user" {
		t.Errorf("over-stemming: %q %q %q", Stem("add"), Stem("status"), Stem("user"))
	}
	var terms []string
	analyzer{}.terms("func (g *Gate) CheckPermission() error { return nil }", func(s string) { terms = append(terms, s) })
	joined := strings.Join(terms, " ")
	for _, want := range []string{"gate", "check", "permit", "checkpermission", "error"} {
		if !strings.Contains(" "+joined+" ", " "+want+" ") {
			t.Errorf("terms %q missing %q", joined, want)
		}
	}
	for _, unwanted := range []string{"func", "return", "nil"} {
		if strings.Contains(" "+joined+" ", " "+unwanted+" ") {
			t.Errorf("keyword %q not dropped: %q", unwanted, joined)
		}
	}
	if !contains(synonyms[Stem("prompt")], Stem("approve")) || !contains(synonyms[Stem("cfg")], Stem("config")) {
		t.Error("synonyms missing")
	}
}

func TestTools(t *testing.T) {
	ix := sampleIndex(t)
	env, err := tools.NewEnv(ix.Root())
	if err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry(Tools(ix)...)
	for _, name := range []string{"code_search", "symbols", "repo_map"} {
		tl, ok := reg.Get(name)
		if !ok {
			t.Fatalf("tool %s missing", name)
		}
		if tl.Kind() != tools.KindRead {
			t.Errorf("%s kind = %v", name, tl.Kind())
		}
	}
	run := func(name, input string) (string, error) {
		t.Helper()
		tl, _ := reg.Get(name)
		call, err := tl.Prepare(env, json.RawMessage(input))
		if err != nil {
			return "", err
		}
		if call.Kind != tools.KindRead || call.Summary == "" {
			t.Errorf("%s call = %+v", name, call)
		}
		return call.Run(context.Background())
	}
	out, err := run("code_search", `{"query":"where is the permission prompt decided","limit":3}`)
	if err != nil || !strings.Contains(out, "internal/permission/permission.go:") || !strings.Contains(out, "method Gate.Check") {
		t.Errorf("code_search: %v\n%s", err, out)
	}
	out, err = run("code_search", `{"query":"login","path":"`+ix.Root()+`/web"}`)
	if err != nil || !strings.Contains(out, "web/src/auth.ts") || strings.Contains(out, "permission.go") {
		t.Errorf("code_search abs path: %v\n%s", err, out)
	}
	out, _ = run("code_search", `{"query":"zzqxj"}`)
	if !strings.Contains(out, "No results") {
		t.Errorf("no-result output: %s", out)
	}
	out, err = run("symbols", `{"name":"Gate.Check"}`)
	if err != nil || !strings.Contains(out, "method Gate.Check  internal/permission/permission.go:") || !strings.Contains(out, "func (g *Gate) Check(kind string) Verdict") {
		t.Errorf("symbols: %v\n%s", err, out)
	}
	out, _ = run("symbols", `{"path":"internal/config/config.go"}`)
	if !strings.Contains(out, "struct Config") || !strings.Contains(out, "function Load") {
		t.Errorf("symbols by path: %s", out)
	}
	out, err = run("repo_map", `{"budget":300}`)
	if err != nil || !strings.Contains(out, ".go\n") {
		t.Errorf("repo_map: %v\n%s", err, out)
	}
	bad := map[string]string{
		"code_search": `{"query":"x","unknown":1}`,
		"symbols":     `{"kind":"function"}`,
		"repo_map":    `{"budget":-1}`,
	}
	for name, input := range bad {
		if _, err := run(name, input); err == nil {
			t.Errorf("%s accepted %s", name, input)
		}
	}
	for _, input := range []string{`{}`, `{"query":""}`, `{"query":"x"} {"query":"y"}`, `{"query":"x","limit":-1}`, `not json`} {
		if _, err := run("code_search", input); err == nil {
			t.Errorf("code_search accepted %s", input)
		}
	}
	if _, err := run("symbols", `{"name":"x","kind":"nonsense"}`); err == nil {
		t.Error("bad kind accepted")
	}
	if _, err := (SearchTool{}).Prepare(env, json.RawMessage(`{"query":"x"}`)); err == nil {
		t.Error("nil index accepted")
	}
	long := limitOutput(strings.Repeat("é", 5000), 1000)
	if len(long) > 1100 || !strings.Contains(long, "characters omitted") {
		t.Errorf("limitOutput produced %d bytes", len(long))
	}
	for _, r := range long {
		if r == 0xFFFD {
			t.Fatal("limitOutput split a rune")
		}
	}
}
