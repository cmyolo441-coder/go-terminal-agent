package codeintel

import (
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Benchmarks for the code index on a generated repository. They only run
// with -bench, for example:
//
//	go test ./internal/codeintel -run '^$' -bench . -benchmem

// benchRepoFiles is the size of the generated repository.
const benchRepoFiles = 2000

var benchWords = strings.Fields(`account approval buffer cache client config connection context
decision delta document error event file gate handler history index loader manager message model
parser permission policy provider request response retry router schema search session settings
snapshot stream symbol token tool transport update user validator worker writer reader encoder
decoder budget audit checkpoint plugin sandbox hook telemetry metric trace span render prompt`)

func benchName(r *rand.Rand, parts int, upper bool) string {
	var b strings.Builder
	for i := 0; i < parts; i++ {
		w := benchWords[r.Intn(len(benchWords))]
		if i > 0 || upper {
			w = strings.ToUpper(w[:1]) + w[1:]
		}
		b.WriteString(w)
	}
	return b.String()
}

// benchRepo generates a deterministic mixed Go, TypeScript and Python
// repository with cross-file references, doc comments and tests.
func benchRepo(tb testing.TB, n int) (root string, paths []string) {
	tb.Helper()
	root = tb.TempDir()
	r := rand.New(rand.NewSource(42))
	files := make(map[string]string, n)
	// Names other files refer to, so the reference graph has edges.
	var exported []string
	for i := 0; i < 400; i++ {
		exported = append(exported, benchName(r, 2+r.Intn(2), true))
	}
	for i := 0; i < n; i++ {
		pkg := fmt.Sprintf("pkg%02d", i%40)
		var p, src string
		switch i % 3 {
		case 0:
			p = fmt.Sprintf("internal/%s/%s_%d.go", pkg, strings.ToLower(benchName(r, 1, false)), i)
			if i%10 == 0 {
				p = strings.TrimSuffix(p, ".go") + "_test.go"
			}
			src = benchGo(r, pkg, exported, i)
		case 1:
			p = fmt.Sprintf("web/src/%s/%s%d.ts", pkg, benchName(r, 2, false), i)
			src = benchTS(r, exported, i)
		default:
			p = fmt.Sprintf("py/%s/%s_%d.py", pkg, strings.ToLower(benchName(r, 1, false)), i)
			src = benchPy(r, exported, i)
		}
		files[p] = src
		paths = append(paths, p)
	}
	files["README.md"] = "# Bench repo\n\nA generated repository for benchmarks.\n\n## Usage\n\nRun it.\n"
	files[".gitignore"] = "*.log\nbuild/\n"
	writeTree(tb, root, files)
	return root, paths
}

func benchGo(r *rand.Rand, pkg string, exported []string, i int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "// Package %s is generated for benchmarks.\npackage %s\n\nimport (\n\t\"context\"\n\t\"fmt\"\n)\n\n", pkg, pkg)
	typ := exported[i%len(exported)]
	fmt.Fprintf(&b, "// %s holds the state of one %s.\ntype %s struct {\n", typ, strings.ToLower(benchName(r, 1, false)), typ)
	for f := 0; f < 4+r.Intn(4); f++ {
		fmt.Fprintf(&b, "\t%s %s\n", benchName(r, 2, true), []string{"string", "int", "[]byte", "map[string]int", "*" + exported[r.Intn(len(exported))]}[r.Intn(5)])
	}
	b.WriteString("}\n\n")
	for m := 0; m < 6+r.Intn(6); m++ {
		name := benchName(r, 2+r.Intn(2), m%2 == 0)
		ref := exported[r.Intn(len(exported))]
		fmt.Fprintf(&b, "// %s %s the %s and returns an error when the %s fails.\n", name, benchWords[r.Intn(len(benchWords))], strings.ToLower(ref), benchWords[r.Intn(len(benchWords))])
		fmt.Fprintf(&b, "func (x *%s) %s(ctx context.Context, in *%s) (int, error) {\n", typ, name, ref)
		for l := 0; l < 5+r.Intn(20); l++ {
			v := benchName(r, 2, false)
			switch l % 4 {
			case 0:
				fmt.Fprintf(&b, "\t%s := in.%s(ctx)\n", v, benchName(r, 2, true))
			case 1:
				fmt.Fprintf(&b, "\tif err := %s(ctx, %q); err != nil {\n\t\treturn 0, fmt.Errorf(\"%s: %%w\", err)\n\t}\n", "New"+exported[r.Intn(len(exported))], v, v)
			case 2:
				fmt.Fprintf(&b, "\tfor _, item := range x.%s {\n\t\t_ = item // %s\n\t}\n", benchName(r, 2, true), benchName(r, 3, false))
			default:
				fmt.Fprintf(&b, "\t_ = %d + len(%q)\n", r.Intn(1000), v)
			}
		}
		b.WriteString("\treturn 0, nil\n}\n\n")
	}
	fmt.Fprintf(&b, "// New%s builds a %s.\nfunc New%s(ctx context.Context, name string) error {\n\t_ = ctx\n\t_ = name\n\treturn nil\n}\n", typ, typ, typ)
	return b.String()
}

func benchTS(r *rand.Rand, exported []string, i int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "import { %s } from './%s';\n\n", exported[r.Intn(len(exported))], strings.ToLower(benchName(r, 1, false)))
	cls := exported[(i*7)%len(exported)] + "View"
	fmt.Fprintf(&b, "/** %s renders a %s. */\nexport class %s {\n  private %s: number = 0;\n\n", cls, benchName(r, 1, false), cls, benchName(r, 1, false))
	for m := 0; m < 5+r.Intn(6); m++ {
		fmt.Fprintf(&b, "  /** %s the %s. */\n  async %s(input: %s): Promise<void> {\n", benchWords[r.Intn(len(benchWords))], benchName(r, 1, false), benchName(r, 2, false), exported[r.Intn(len(exported))])
		for l := 0; l < 4+r.Intn(14); l++ {
			fmt.Fprintf(&b, "    const %s = await input.%s(%d, '%s');\n", benchName(r, 2, false), benchName(r, 2, false), r.Intn(100), benchName(r, 1, false))
		}
		b.WriteString("  }\n\n")
	}
	b.WriteString("}\n\n")
	fmt.Fprintf(&b, "export function %s(x: number): number {\n  return x * %d;\n}\n\nexport const %s = 42;\n", benchName(r, 3, false), r.Intn(10), strings.ToUpper(benchName(r, 1, false)))
	return b.String()
}

func benchPy(r *rand.Rand, exported []string, i int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\"\"\"Generated module %d.\"\"\"\n\nimport os\nfrom .%s import %s\n\nMAX_%s = %d\n\n", i, strings.ToLower(benchName(r, 1, false)), exported[r.Intn(len(exported))], strings.ToUpper(benchName(r, 1, false)), r.Intn(100))
	cls := exported[(i*13)%len(exported)] + "Service"
	fmt.Fprintf(&b, "class %s:\n    \"\"\"%s manages %s objects.\"\"\"\n\n    def __init__(self, %s):\n        self.%s = %s\n\n", cls, cls, benchName(r, 1, false), strings.ToLower(benchName(r, 1, false)), strings.ToLower(benchName(r, 1, false)), strings.ToLower(benchName(r, 1, false)))
	for m := 0; m < 5+r.Intn(6); m++ {
		name := strings.ToLower(benchName(r, 1, false)) + "_" + strings.ToLower(benchName(r, 1, false))
		fmt.Fprintf(&b, "    def %s(self, value):\n        \"\"\"Return the %s for value.\"\"\"\n", name, benchName(r, 1, false))
		for l := 0; l < 3+r.Intn(12); l++ {
			fmt.Fprintf(&b, "        %s = %s(value, %d)\n", strings.ToLower(benchName(r, 1, false)), exported[r.Intn(len(exported))], r.Intn(50))
		}
		b.WriteString("        return value\n\n")
	}
	fmt.Fprintf(&b, "\ndef %s_helper(path):\n    return os.path.join(path, '%s')\n", strings.ToLower(benchName(r, 1, false)), benchName(r, 1, false))
	return b.String()
}

func benchOpen(b *testing.B, root, cacheDir string) *Index {
	b.Helper()
	ix, err := Open(Options{Root: root, CacheDir: cacheDir, SkipGlobalGitignore: true, RefreshInterval: time.Hour})
	if err != nil {
		b.Fatal(err)
	}
	return ix
}

// BenchmarkIndexBuild is a cold, full build: walk, read, analyse and build
// the search index for every file.
func BenchmarkIndexBuild(b *testing.B) {
	root, _ := benchRepo(b, benchRepoFiles)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ix := benchOpen(b, root, "")
		if _, err := ix.Refresh(context.Background()); err != nil {
			b.Fatal(err)
		}
		ix.Close()
	}
}

// BenchmarkIndexBuildWithCache is a cold build that also writes the cache.
func BenchmarkIndexBuildWithCache(b *testing.B) {
	root, _ := benchRepo(b, benchRepoFiles)
	cache := b.TempDir()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		ix := benchOpen(b, root, filepath.Join(cache, fmt.Sprint(i))) // empty: nothing to load
		b.StartTimer()
		if _, err := ix.Refresh(context.Background()); err != nil {
			b.Fatal(err)
		}
		ix.Close()
	}
}

// BenchmarkIndexOpenFromCache is the startup path: open an index whose
// cache is on disk.
func BenchmarkIndexOpenFromCache(b *testing.B) {
	root, _ := benchRepo(b, benchRepoFiles)
	cache := b.TempDir()
	ix := benchOpen(b, root, cache)
	if _, err := ix.Refresh(context.Background()); err != nil {
		b.Fatal(err)
	}
	ix.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ix := benchOpen(b, root, cache)
		if !ix.snap.Load().stats.FromCache {
			b.Fatal("cache not loaded")
		}
		ix.Close()
	}
}

// BenchmarkIndexRefreshNoChange is the periodic re-scan of an unchanged
// tree.
func BenchmarkIndexRefreshNoChange(b *testing.B) {
	root, _ := benchRepo(b, benchRepoFiles)
	ix := benchOpen(b, root, "")
	defer ix.Close()
	if _, err := ix.Refresh(context.Background()); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		st, err := ix.Refresh(context.Background())
		if err != nil {
			b.Fatal(err)
		}
		if st.Parsed != 0 {
			b.Fatalf("parsed %d files on an unchanged tree", st.Parsed)
		}
	}
}

// BenchmarkIndexRefreshIncremental re-scans after 20 files changed.
func BenchmarkIndexRefreshIncremental(b *testing.B) {
	root, paths := benchRepo(b, benchRepoFiles)
	ix := benchOpen(b, root, "")
	defer ix.Close()
	if _, err := ix.Refresh(context.Background()); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		for k := 0; k < 20; k++ {
			p := paths[(i*20+k)%len(paths)]
			touchPast(b, filepath.Join(root, filepath.FromSlash(p)), fmt.Sprintf("// edit %d\nfunc Edited%d() {}\n", i, k), time.Duration(i+10)*time.Minute)
		}
		b.StartTimer()
		st, err := ix.Refresh(context.Background())
		if err != nil {
			b.Fatal(err)
		}
		if st.Parsed != 20 {
			b.Fatalf("parsed %d files, want 20", st.Parsed)
		}
	}
}

// BenchmarkIndexInvalidateOne is the after-edit path: one file is
// invalidated and the next query re-reads only it.
func BenchmarkIndexInvalidateOne(b *testing.B) {
	root, paths := benchRepo(b, benchRepoFiles)
	ix := benchOpen(b, root, "")
	defer ix.Close()
	if _, err := ix.Refresh(context.Background()); err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ix.Invalidate(paths[i%len(paths)])
		if err := ix.EnsureFresh(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkIndexEditThenSearch is the write-then-search loop of an agent
// turn: one file changes, the index catches up and a search follows, so
// work deferred by the refresh is counted here.
func BenchmarkIndexEditThenSearch(b *testing.B) {
	root, paths := benchRepo(b, benchRepoFiles)
	ix := benchOpen(b, root, "")
	defer ix.Close()
	ctx := context.Background()
	if _, err := ix.Refresh(ctx); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ix.Invalidate(paths[i%len(paths)])
		if err := ix.EnsureFresh(ctx); err != nil {
			b.Fatal(err)
		}
		if _, err := ix.Search(ctx, benchQueries[i%len(benchQueries)], SearchOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}

var benchQueries = []string{
	"where is the permission decision made",
	"SessionManager",
	"retry backoff for the provider request",
	"parse config file",
	"stream delta handler",
	"checkpointWriter",
}

// BenchmarkSearch runs ranked searches, snippets included.
func BenchmarkSearch(b *testing.B) {
	root, _ := benchRepo(b, benchRepoFiles)
	ix := benchOpen(b, root, "")
	defer ix.Close()
	ctx := context.Background()
	if _, err := ix.Refresh(ctx); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ix.Search(ctx, benchQueries[i%len(benchQueries)], SearchOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSearchPathFilter runs searches restricted by a glob.
func BenchmarkSearchPathFilter(b *testing.B) {
	root, _ := benchRepo(b, benchRepoFiles)
	ix := benchOpen(b, root, "")
	defer ix.Close()
	ctx := context.Background()
	if _, err := ix.Refresh(ctx); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ix.Search(ctx, benchQueries[i%len(benchQueries)], SearchOptions{Path: "internal/**/*.go"}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRepoMap renders the startup repository map, reference graph
// included (each iteration gets a new snapshot, as after a refresh).
func BenchmarkRepoMap(b *testing.B) {
	root, _ := benchRepo(b, benchRepoFiles)
	ix := benchOpen(b, root, "")
	defer ix.Close()
	ctx := context.Background()
	if _, err := ix.Refresh(ctx); err != nil {
		b.Fatal(err)
	}
	s := ix.snap.Load()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		fresh := &snapshot{all: s.all, files: s.files, stats: s.stats, walkSkipped: s.walkSkipped, derived: &derived{files: s.files}}
		ix.snap.Store(fresh)
		b.StartTimer()
		m, err := ix.RepoMap(ctx, RepoMapOptions{Budget: 1500})
		if err != nil || m == "" {
			b.Fatal(err)
		}
	}
}

// BenchmarkRepoMapWarm renders the map with the reference graph built.
func BenchmarkRepoMapWarm(b *testing.B) {
	root, _ := benchRepo(b, benchRepoFiles)
	ix := benchOpen(b, root, "")
	defer ix.Close()
	ctx := context.Background()
	if _, err := ix.RepoMap(ctx, RepoMapOptions{Budget: 1500}); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ix.RepoMap(ctx, RepoMapOptions{Budget: 1500, Focus: []string{"internal/pkg01"}}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkAnalyzeFile is the per-file analysis of one Go file.
func BenchmarkAnalyzeFile(b *testing.B) {
	r := rand.New(rand.NewSource(1))
	var exported []string
	for i := 0; i < 50; i++ {
		exported = append(exported, benchName(r, 2, true))
	}
	src := []byte(benchGo(r, "pkg", exported, 3))
	b.SetBytes(int64(len(src)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		analyzeFile("internal/pkg/file.go", src)
	}
}
