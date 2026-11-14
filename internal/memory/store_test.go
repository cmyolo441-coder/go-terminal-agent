package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// testClock is a settable clock.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *testClock { return &testClock{t: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

var secretRE = regexp.MustCompile(`sk-[A-Za-z0-9]{16,}`)

func testRedact(s string) string { return secretRE.ReplaceAllString(s, "[REDACTED]") }

func openTest(t testing.TB, dir string, mod func(*Options)) (*Store, *testClock) {
	t.Helper()
	clk := newClock()
	o := Options{Dir: dir, Redact: testRedact, Clock: clk.Now, NoSync: true}
	if mod != nil {
		mod(&o)
	}
	s, err := Open(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return s, clk
}

func mustRemember(t testing.TB, s *Store, r Record) RememberResult {
	t.Helper()
	res, err := s.Remember(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func ids(hits []Hit) []string {
	var out []string
	for _, h := range hits {
		out = append(out, h.Record.ID)
	}
	return out
}

var corpus = []Record{
	{ID: "pref-tests", Kind: KindPreference, Text: "The user prefers table-driven tests with t.Run subtests in Go."},
	{ID: "pref-tabs", Kind: KindPreference, Text: "Always indent YAML files with two spaces, never tabs."},
	{ID: "dec-pgx", Kind: KindDecision, Scope: ScopeProject, Namespace: "/repo/api", Text: "We decided to use pgx instead of database/sql for Postgres access.", Tags: []string{"database"}},
	{ID: "fact-port", Kind: KindFact, Scope: ScopeProject, Namespace: "/repo/api", Text: "The API server listens on port 8443 behind the envoy proxy."},
	{ID: "fact-ci", Kind: KindFact, Scope: ScopeProject, Namespace: "/repo/web", Text: "CI runs on GitHub Actions; the race detector job takes about twelve minutes."},
	{ID: "ep-flaky", Kind: KindEpisode, Scope: ScopeSession, Namespace: "s1", Text: "TestWatcher flaked twice because of a timing assumption in the file watcher."},
	{ID: "fact-deploy", Kind: KindFact, Text: "Deployments to staging go through ArgoCD; production needs a manual approval."},
}

func seed(t testing.TB, s *Store) {
	for _, r := range corpus {
		mustRemember(t, s, r)
	}
}

func TestRememberRecall(t *testing.T) {
	s, clk := openTest(t, "", nil)
	defer s.Close()
	seed(t, s)
	if s.Len() != len(corpus) {
		t.Fatalf("Len = %d", s.Len())
	}
	ctx := context.Background()
	hits, err := s.Recall(ctx, "how should I write unit tests in go?", 3, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].Record.ID != "pref-tests" {
		t.Fatalf("hits = %v", ids(hits))
	}
	e := hits[0].Explanation
	if e.VectorRank != 1 || e.LexicalRank != 1 || !slices.Contains(e.MatchedTerms, "test") || !e.Exact {
		t.Fatalf("explanation %+v", e)
	}
	if !strings.Contains(e.String(), "vector #1") || hits[0].Score <= 0 || hits[0].Score > 1 {
		t.Fatalf("explanation string %q score %v", e.String(), hits[0].Score)
	}
	// Recall touched the record.
	clk.Advance(time.Minute)
	if r, _ := s.Get("pref-tests"); r.AccessCount != 1 {
		t.Fatalf("access count %d", r.AccessCount)
	}

	// Lexical-only identifiers still match.
	hits, _ = s.Recall(ctx, "pgx", 2, Filter{})
	if len(hits) == 0 || hits[0].Record.ID != "dec-pgx" {
		t.Fatalf("pgx hits = %v", ids(hits))
	}
	// Filters: other project hidden, user scope kept.
	hits, _ = s.Recall(ctx, "database postgres access port", 10, Filter{Project: "/repo/web"})
	for _, h := range hits {
		if h.Record.Namespace == "/repo/api" {
			t.Fatalf("filter leaked %s", h.Record.ID)
		}
	}
	hits, _ = s.Recall(ctx, "deploy staging", 5, Filter{Kinds: []Kind{KindDecision}})
	for _, h := range hits {
		if h.Record.Kind != KindDecision {
			t.Fatalf("kind filter leaked %s", h.Record.ID)
		}
	}
	hits, _ = s.Recall(ctx, "postgres", 5, Filter{Tags: []string{"Database"}})
	if len(hits) != 1 || hits[0].Record.ID != "dec-pgx" {
		t.Fatalf("tag filter = %v", ids(hits))
	}
	// Unrelated queries come back empty rather than padded with noise.
	hits, _ = s.Recall(ctx, "banana smoothie recipe", 5, Filter{})
	if len(hits) != 0 {
		t.Fatalf("noise hits = %v", ids(hits))
	}
	if _, err := s.Remember(ctx, Record{Text: "  "}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty text: %v", err)
	}
	if _, err := s.Remember(ctx, Record{Text: "x", Scope: ScopeProject}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing namespace: %v", err)
	}
}

func TestRecallRanksRecencyAndImportance(t *testing.T) {
	s, clk := openTest(t, "", func(o *Options) { o.DedupeThreshold = -1; o.Recall.MMRLambda = 1 })
	defer s.Close()
	mustRemember(t, s, Record{ID: "old", Kind: KindEpisode, Text: "The release build failed because of a missing codesign certificate.", Importance: 0.3})
	clk.Advance(120 * 24 * time.Hour)
	mustRemember(t, s, Record{ID: "new", Kind: KindEpisode, Text: "The release build failed because a codesign certificate was missing.", Importance: 0.3})
	hits, _ := s.Recall(context.Background(), "release build codesign certificate failure", 2, Filter{})
	if len(hits) != 2 || hits[0].Record.ID != "new" || hits[1].Explanation.Recency >= hits[0].Explanation.Recency {
		t.Fatalf("recency ordering: %v %+v", ids(hits), hits)
	}
}

func TestRecallMMRDiversifies(t *testing.T) {
	s, _ := openTest(t, "", func(o *Options) { o.DedupeThreshold = -1 })
	defer s.Close()
	mustRemember(t, s, Record{ID: "a", Text: "Run go test with the race detector before pushing."})
	mustRemember(t, s, Record{ID: "b", Text: "Run go test with the race detector before every push."})
	mustRemember(t, s, Record{ID: "c", Text: "Run golangci-lint before pushing changes."})
	hits, _ := s.Recall(context.Background(), "what to run before pushing", 2, Filter{})
	got := ids(hits)
	if len(got) != 2 || !slices.Contains(got, "c") {
		t.Fatalf("MMR should surface the distinct memory: %v", got)
	}
}

func TestDedupe(t *testing.T) {
	s, clk := openTest(t, "", nil)
	defer s.Close()
	r1 := mustRemember(t, s, Record{Kind: KindPreference, Text: "The user prefers table-driven tests in Go.", Tags: []string{"testing"}, TTL: time.Hour})
	clk.Advance(time.Hour / 2)
	// Exact after normalisation (case, punctuation, stopwords).
	r2 := mustRemember(t, s, Record{Kind: KindPreference, Text: "the user PREFERS table driven tests, in go", Tags: []string{"go"}, Importance: 0.9})
	if !r2.Merged || r2.ID != r1.ID {
		t.Fatalf("exact duplicate not merged: %+v", r2)
	}
	// Near duplicate.
	r3 := mustRemember(t, s, Record{Kind: KindPreference, Text: "The user prefers table-driven tests in Go code."})
	if !r3.Merged || r3.ID != r1.ID || r3.Similarity < 0.92 {
		t.Fatalf("near duplicate not merged: %+v", r3)
	}
	// Same text, different kind or scope: kept apart.
	if r := mustRemember(t, s, Record{Kind: KindFact, Text: "The user prefers table-driven tests in Go code."}); r.Merged {
		t.Fatal("merged across kinds")
	}
	if r := mustRemember(t, s, Record{Kind: KindPreference, Scope: ScopeProject, Namespace: "p", Text: "The user prefers table-driven tests in Go code."}); r.Merged {
		t.Fatal("merged across scopes")
	}
	// A different preference is not a duplicate.
	if r := mustRemember(t, s, Record{Kind: KindPreference, Text: "The user prefers testify assertions over plain if checks."}); r.Merged {
		t.Fatal("distinct memory merged")
	}
	rec, _ := s.Get(r1.ID)
	if rec.Reinforced != 2 || rec.Importance != 0.9 || rec.TTL != 0 || !slices.Equal(rec.Tags, []string{"testing", "go"}) ||
		rec.Text != "The user prefers table-driven tests in Go code." {
		t.Fatalf("merged record %+v", rec)
	}
	if s.Len() != 4 {
		t.Fatalf("Len = %d", s.Len())
	}
	st := s.Stats()
	if st.Merged != 2 || st.Remembered != 4 {
		t.Fatalf("stats %+v", st)
	}
}

func TestUpsertByID(t *testing.T) {
	s, clk := openTest(t, "", nil)
	defer s.Close()
	mustRemember(t, s, Record{ID: "x", Text: "Service A uses port 9000."})
	created, _ := s.Get("x")
	clk.Advance(time.Hour)
	r := mustRemember(t, s, Record{ID: "x", Text: "Service A moved to port 9100."})
	if !r.Replaced || s.Len() != 1 {
		t.Fatalf("%+v len %d", r, s.Len())
	}
	got, _ := s.Get("x")
	if !got.Created.Equal(created.Created) || !got.Updated.After(created.Updated) {
		t.Fatalf("timestamps %+v", got)
	}
	hits, _ := s.Recall(context.Background(), "9100", 1, Filter{})
	if len(hits) != 1 || hits[0].Record.ID != "x" {
		t.Fatal("updated text not indexed")
	}
	if hits, _ := s.Recall(context.Background(), "9000", 1, Filter{}); len(hits) != 0 && hits[0].Explanation.LexicalRank != 0 {
		t.Fatal("old text still indexed lexically")
	}
}

func TestSecretsNeverStored(t *testing.T) {
	dir := t.TempDir()
	const secret = "sk-ABCDEFGHIJKLMNOPQRSTUV123"
	var seen []string
	var mu sync.Mutex
	spy := &spyEmbedder{inner: DefaultHashEmbedder(), seen: func(s string) { mu.Lock(); seen = append(seen, s); mu.Unlock() }}
	s, _ := openTest(t, dir, func(o *Options) { o.Embedder = spy })
	mustRemember(t, s, Record{Text: "The deploy key is " + secret, Tags: []string{secret}, Source: secret})
	hits, _ := s.Recall(context.Background(), "deploy key "+secret, 1, Filter{})
	if len(hits) != 1 || strings.Contains(hits[0].Record.Text, secret) || !strings.Contains(hits[0].Record.Text, "[REDACTED]") {
		t.Fatalf("hits %+v", hits)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, x := range seen {
		if strings.Contains(x, secret) {
			t.Fatal("secret sent to the embedder")
		}
	}
	files, _ := os.ReadDir(dir)
	for _, f := range files {
		data, _ := os.ReadFile(filepath.Join(dir, f.Name()))
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("secret on disk in %s", f.Name())
		}
	}
	if _, err := Open(context.Background(), Options{}); err == nil {
		t.Fatal("Open without Redact succeeded")
	}
}

type spyEmbedder struct {
	inner Embedder
	seen  func(string)
	calls int
	mu    sync.Mutex
}

func (e *spyEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	for _, t := range texts {
		if e.seen != nil {
			e.seen(t)
		}
	}
	return e.inner.Embed(ctx, texts)
}
func (e *spyEmbedder) Dim() int   { return e.inner.Dim() }
func (e *spyEmbedder) ID() string { return e.inner.ID() }

func TestForgetAndTTLAndBudget(t *testing.T) {
	s, clk := openTest(t, "", func(o *Options) { o.MaxRecords = 20 })
	defer s.Close()
	ctx := context.Background()
	mustRemember(t, s, Record{ID: "temp", Kind: KindEpisode, Text: "Temporary note about the ongoing bisect of commit range.", TTL: time.Hour})
	mustRemember(t, s, Record{ID: "pin", Text: "Pinned: never push directly to main.", Importance: 1})
	if err := s.Forget("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Forget missing: %v", err)
	}
	clk.Advance(2 * time.Hour)
	if hits, _ := s.Recall(ctx, "bisect commit range", 3, Filter{}); len(hits) != 0 {
		t.Fatalf("expired record recalled: %v", ids(hits))
	}
	rep, err := s.Maintain(ctx, false)
	if err != nil || rep.Expired != 1 {
		t.Fatalf("maintain %+v %v", rep, err)
	}
	for i := 0; i < 40; i++ {
		clk.Advance(time.Minute)
		mustRemember(t, s, Record{ID: fmt.Sprintf("n%02d", i), Text: fmt.Sprintf("Distinct note %d about subsystem %c%c%c.", i, 'a'+i%26, 'k'+i%7, 'p'+i%5)})
	}
	if n := s.Len(); n > 20 {
		t.Fatalf("budget exceeded: %d", n)
	}
	if _, ok := s.Get("pin"); !ok {
		t.Fatal("pinned record evicted")
	}
	if _, ok := s.Get("n39"); !ok {
		t.Fatal("newest record evicted")
	}
	if _, ok := s.Get("n00"); ok {
		t.Fatal("oldest record kept")
	}
	if err := s.Forget("n39"); err != nil {
		t.Fatal(err)
	}
	n, err := s.ForgetWhere(ctx, Filter{Kinds: []Kind{KindFact}, MinImportance: 0.99})
	if err != nil || n != 1 {
		t.Fatalf("ForgetWhere = %d, %v", n, err)
	}
	if st := s.Stats(); st.Evictions == 0 || st.Expirations != 1 || st.Forgotten != 2 {
		t.Fatalf("stats %+v", st)
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s, clk := openTest(t, dir, nil)
	seed(t, s)
	if _, err := s.Recall(ctx, "table driven tests", 1, Filter{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Forget("fact-ci"); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Hour)
	before := s.List(Filter{IncludeExpired: true}, 0)
	// Abandon without Close: everything must come back from the log.
	s.disk.close()
	s.closed = true

	s2, _ := openTest(t, dir, nil)
	after := s2.List(Filter{IncludeExpired: true}, 0)
	if !recordsEqual(before, after) {
		t.Fatalf("log replay mismatch:\n%+v\n%+v", before, after)
	}
	if r, _ := s2.Get("pref-tests"); r.AccessCount != 0 {
		// touches are buffered until the next write; this one was never flushed
		t.Logf("access count after crash: %d", r.AccessCount)
	}
	mustRemember(t, s2, Record{ID: "late", Text: "Added after reopening."})
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	// After Close the state lives in a compacted snapshot with an empty log.
	man, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil || !bytes.Contains(man, []byte(`"records": 7`)) {
		t.Fatalf("manifest %s %v", man, err)
	}
	s3, _ := openTest(t, dir, nil)
	defer s3.Close()
	if s3.Len() != 7 || s3.Stats().LogBytes != walHeaderSize {
		t.Fatalf("len %d stats %+v", s3.Len(), s3.Stats())
	}
	hits, _ := s3.Recall(ctx, "pgx postgres", 1, Filter{})
	if len(hits) != 1 || hits[0].Record.ID != "dec-pgx" || hits[0].Explanation.Exact != true {
		t.Fatalf("recall after reload: %v", ids(hits))
	}
	if g := s3.hnsw.Len(); g != 7 {
		t.Fatalf("graph not restored: %d", g)
	}
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 4 { // LOCK, MANIFEST.json, seg, wal
		t.Fatalf("stale files: %v", names)
	}
}

func recordsEqual(a, b []Record) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.ID != y.ID || x.Text != y.Text || x.Kind != y.Kind || x.Scope != y.Scope || x.Namespace != y.Namespace ||
			!x.Created.Equal(y.Created) || !x.Updated.Equal(y.Updated) || x.Importance != y.Importance ||
			!slices.Equal(x.Tags, y.Tags) || x.Reinforced != y.Reinforced {
			return false
		}
	}
	return true
}

func TestCorruptionDetection(t *testing.T) {
	dir := t.TempDir()
	s, _ := openTest(t, dir, nil)
	seed(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	segs, _ := filepath.Glob(filepath.Join(dir, "seg-*.bin"))
	if len(segs) != 1 {
		t.Fatalf("segments %v", segs)
	}
	data, _ := os.ReadFile(segs[0])
	// Flip one byte inside the third record's text.
	i := bytes.Index(data, []byte("Postgres"))
	data[i] ^= 0x20
	if err := os.WriteFile(segs[0], data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(context.Background(), Options{Dir: dir, Redact: testRedact})
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt snapshot opened: %v", err)
	}
	// Recovery salvages every intact record and quarantines the file.
	s2, _ := openTest(t, dir, func(o *Options) { o.RecoverCorrupt = true })
	defer s2.Close()
	if s2.Len() != len(corpus)-1 {
		t.Fatalf("salvaged %d records", s2.Len())
	}
	if _, ok := s2.Get("dec-pgx"); ok {
		t.Fatal("damaged record salvaged")
	}
	q, _ := filepath.Glob(filepath.Join(dir, "*.corrupt-*"))
	if len(q) != 1 {
		t.Fatalf("quarantine %v", q)
	}
	if hits, _ := s2.Recall(context.Background(), "API server port", 1, Filter{}); len(hits) != 1 || hits[0].Record.ID != "fact-port" {
		t.Fatalf("recall after salvage %v", ids(hits))
	}
}

func TestTornLogTail(t *testing.T) {
	dir := t.TempDir()
	s, _ := openTest(t, dir, nil)
	mustRemember(t, s, Record{ID: "a", Text: "First durable memory."})
	mustRemember(t, s, Record{ID: "b", Text: "Second memory whose frame will be torn."})
	walPath := s.disk.wal.Name()
	size := s.disk.walSize
	s.disk.close()
	s.closed = true
	if err := os.Truncate(walPath, size-7); err != nil {
		t.Fatal(err)
	}
	s2, _ := openTest(t, dir, nil)
	defer s2.Close()
	if _, ok := s2.Get("a"); !ok {
		t.Fatal("intact record lost")
	}
	if _, ok := s2.Get("b"); ok {
		t.Fatal("torn record applied")
	}
	fi, _ := os.Stat(walPath)
	if fi.Size() != s2.disk.walSize || fi.Size() >= size-7 {
		t.Fatalf("torn tail not truncated: %d", fi.Size())
	}
	mustRemember(t, s2, Record{ID: "c", Text: "Written after recovery."})
}

func TestStaleGenerationCleanup(t *testing.T) {
	dir := t.TempDir()
	s, _ := openTest(t, dir, nil)
	mustRemember(t, s, Record{ID: "a", Text: "Memory before a crash mid-snapshot."})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// A crash after writing the next segment but before the manifest
	// rename leaves orphans that must be ignored and removed.
	for _, n := range []string{segName(99), walName(99), manifestName + ".tmp"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("garbage"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s2, _ := openTest(t, dir, nil)
	defer s2.Close()
	if _, ok := s2.Get("a"); !ok {
		t.Fatal("record lost")
	}
	for _, n := range []string{segName(99), walName(99), manifestName + ".tmp"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			t.Fatalf("orphan %s kept", n)
		}
	}
}

func TestTwoProcessesShareDir(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	a, _ := openTest(t, dir, nil)
	b, _ := openTest(t, dir, nil)
	mustRemember(t, a, Record{ID: "from-a", Text: "Process A learned that the cache lives in ~/.cache/gta."})
	if hits, _ := b.Recall(ctx, "where does the cache live", 1, Filter{}); len(hits) != 1 || hits[0].Record.ID != "from-a" {
		t.Fatalf("b does not see a's write: %v", ids(hits))
	}
	mustRemember(t, b, Record{ID: "from-b", Text: "Process B learned the linter config is .golangci.yml."})
	if err := b.Compact(ctx); err != nil { // new generation under a's feet
		t.Fatal(err)
	}
	mustRemember(t, a, Record{ID: "from-a2", Text: "Process A wrote after B compacted."})
	if _, ok := a.Get("from-b"); !ok {
		t.Fatal("a missed b's write across a generation change")
	}
	if a.Stats().Reloads == 0 {
		t.Fatal("a did not reload")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	c, _ := openTest(t, dir, nil)
	defer c.Close()
	if c.Len() != 3 {
		t.Fatalf("final len %d", c.Len())
	}
}

func TestEmbedderChangeReembeds(t *testing.T) {
	dir := t.TempDir()
	s, _ := openTest(t, dir, nil)
	seed(t, s)
	s.Close()
	small, _ := NewHashEmbedder(HashEmbedderConfig{Dim: 256})
	spy := &spyEmbedder{inner: small}
	s2, _ := openTest(t, dir, func(o *Options) { o.Embedder = spy })
	defer s2.Close()
	if s2.Len() != len(corpus) || s2.Stats().Dim != 256 || spy.calls == 0 {
		t.Fatalf("len %d dim %d calls %d", s2.Len(), s2.Stats().Dim, spy.calls)
	}
	if hits, _ := s2.Recall(context.Background(), "argocd staging deployment", 1, Filter{}); len(hits) != 1 || hits[0].Record.ID != "fact-deploy" {
		t.Fatalf("recall after re-embed %v", ids(hits))
	}
}

func TestConsolidate(t *testing.T) {
	s, _ := openTest(t, "", func(o *Options) {
		o.DedupeThreshold = -1
		o.Summarizer = func(_ context.Context, c []Record) (string, error) {
			var parts []string
			for _, r := range c {
				parts = append(parts, r.Text)
			}
			return "Summary (" + fmt.Sprint(len(c)) + "): " + strings.Join(parts, " / ") + " token sk-ZZZZZZZZZZZZZZZZZZZZ", nil
		}
	})
	defer s.Close()
	ctx := context.Background()
	for i, txt := range []string{
		"The flaky TestWatcher failure is caused by fsnotify event timing on CI.",
		"TestWatcher failed again on CI due to fsnotify event timing.",
		"Again the flaky TestWatcher failed on CI because of fsnotify event timing.",
	} {
		mustRemember(t, s, Record{ID: fmt.Sprint("w", i), Kind: KindEpisode, Text: txt, Tags: []string{fmt.Sprint("t", i)}})
	}
	mustRemember(t, s, Record{ID: "other", Kind: KindEpisode, Text: "Upgraded the protobuf toolchain to v30."})
	rep, err := s.Consolidate(ctx, ConsolidateOptions{Threshold: 0.6})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Clusters != 1 || len(rep.Created) != 1 || rep.Merged != 3 {
		t.Fatalf("report %+v", rep)
	}
	if s.Len() != 2 {
		t.Fatalf("len %d", s.Len())
	}
	sum, _ := s.Get(rep.Created[0])
	if !strings.HasPrefix(sum.Text, "Summary (3)") || strings.Contains(sum.Text, "sk-ZZZ") || sum.Source != "consolidation" ||
		len(sum.MergedFrom) != 3 || len(sum.Tags) != 3 || sum.Kind != KindEpisode {
		t.Fatalf("summary %+v", sum)
	}
	s2, _ := openTest(t, "", nil)
	defer s2.Close()
	if _, err := s2.Consolidate(ctx, ConsolidateOptions{}); !errors.Is(err, ErrNoSummarizer) {
		t.Fatal(err)
	}
}

func TestHNSWPathAndRebuild(t *testing.T) {
	s, _ := openTest(t, "", func(o *Options) {
		o.Recall.ExactThreshold = 10
		o.DedupeThreshold = -1
		o.HNSW = HNSWConfig{Seed: 1, EfConstruction: 64}
	})
	defer s.Close()
	for i := 0; i < 300; i++ {
		mustRemember(t, s, Record{ID: fmt.Sprint("r", i), Text: fmt.Sprintf("Component %d named widget%d handles queue shard %d.", i, i, i*7)})
	}
	hits, _ := s.Recall(context.Background(), "widget123", 3, Filter{})
	if len(hits) == 0 || hits[0].Record.ID != "r123" || hits[0].Explanation.Exact {
		t.Fatalf("hnsw path hits %v", ids(hits))
	}
	for i := 0; i < 200; i++ {
		if err := s.Forget(fmt.Sprint("r", i)); err != nil {
			t.Fatal(err)
		}
	}
	st := s.Stats()
	if st.Rebuilds == 0 || st.GraphDeleted > st.GraphNodes {
		t.Fatalf("no rebuild: %+v", st)
	}
	hits, _ = s.Recall(context.Background(), "widget250", 1, Filter{})
	if len(hits) != 1 || hits[0].Record.ID != "r250" {
		t.Fatalf("after rebuild %v", ids(hits))
	}
}

func TestConcurrentRememberRecall(t *testing.T) {
	dir := t.TempDir()
	s, _ := openTest(t, dir, func(o *Options) { o.CompactBytes = 64 << 10; o.Recall.ExactThreshold = 50 })
	ctx := context.Background()
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Go(func() {
			for i := 0; i < 60; i++ {
				_, err := s.Remember(ctx, Record{
					Kind: Kinds[i%4], Text: fmt.Sprintf("Worker %d observation %d about module m%d and feature f%d.", w, i, i%9, w*100+i),
				})
				if err != nil {
					t.Error(err)
					return
				}
				if i%10 == 0 {
					if _, err := s.Maintain(ctx, i%20 == 0); err != nil {
						t.Error(err)
						return
					}
				}
			}
		})
		wg.Go(func() {
			for i := 0; i < 60; i++ {
				if _, err := s.Recall(ctx, fmt.Sprintf("module m%d feature", i%9), 5, Filter{}); err != nil {
					t.Error(err)
					return
				}
				_ = s.Stats()
			}
		})
	}
	wg.Wait()
	n := s.Len()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, _ := openTest(t, dir, nil)
	defer s2.Close()
	if s2.Len() != n || n < 200 {
		t.Fatalf("records after reopen %d, before %d", s2.Len(), n)
	}
}

func BenchmarkHashEmbed(b *testing.B) {
	e := DefaultHashEmbedder()
	text := corpus[0].Text + " " + corpus[3].Text
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		e.EmbedOne(text)
	}
}

func benchStore(b *testing.B, n int) *Store {
	s, _ := openTest(b, "", func(o *Options) { o.DedupeThreshold = -1; o.HNSW.EfConstruction = 100 })
	for i := 0; i < n; i++ {
		mustRemember(b, s, Record{Text: fmt.Sprintf("Note %d: service s%d depends on queue q%d and table t%d for %s.", i, i%97, i%31, i%53, corpus[i%len(corpus)].Text)})
	}
	return s
}

func BenchmarkRemember(b *testing.B) {
	s := benchStore(b, 1000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mustRemember(b, s, Record{Text: fmt.Sprintf("Bench note %d about service s%d.", i, i%97)})
	}
}

func BenchmarkRecall(b *testing.B) {
	for _, n := range []int{1000, 10000} {
		b.Run(fmt.Sprint("records", n), func(b *testing.B) {
			s := benchStore(b, n)
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := s.Recall(ctx, fmt.Sprintf("which queue does service s%d use", i%97), 8, Filter{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkBM25Search(b *testing.B) {
	ix := NewBM25(BM25Config{})
	for i := 0; i < 10000; i++ {
		ix.Add(uint64(i), lexicalTerms(fmt.Sprintf("service s%d queue q%d table t%d %s", i%97, i%31, i%53, corpus[i%len(corpus)].Text)))
	}
	q := lexicalTerms("table driven tests queue q7")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ix.Search(q, 64, nil)
	}
}
