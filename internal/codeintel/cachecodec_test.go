package codeintel

import (
	"bytes"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func codecPayload(t testing.TB) *cachePayload {
	t.Helper()
	cp := &cachePayload{Root: "/w", Key: "k", SavedAt: 12345, Truncate: true, Skipped: map[string]int{"big": 2, "binary": 7}}
	for _, dir := range []string{".", "../llm/openai", "../agent"} {
		paths, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, p := range paths {
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			cp.Files = append(cp.Files, analyzeFile(strings.TrimPrefix(filepath.ToSlash(p), "../"), data))
		}
	}
	cp.Files = append(cp.Files, analyzeFile("doc/notes.md", []byte("# Title\n\nSome words about parsing config files.\n")),
		analyzeFile("web/app.ts", []byte("export function handleRequest(req: Request) {\n  return fetchUser(req.id)\n}\n")),
		analyzeFile("tools/run.py", []byte("def main():\n    return parse_args()\n")))
	return cp
}

func TestCacheCodecRoundTrip(t *testing.T) {
	cp := codecPayload(t)
	enc := encodeCache(cp)
	got, err := decodeCache(enc, func(s string) string { return s })
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, cp) {
		if again := encodeCache(got); !bytes.Equal(again, enc) {
			t.Fatal("decoded payload re-encodes differently")
		}
		for i := range cp.Files {
			if !reflect.DeepEqual(got.Files[i], cp.Files[i]) {
				t.Fatalf("file %s differs after a round trip\n got %+v\nwant %+v", cp.Files[i].Path, got.Files[i], cp.Files[i])
			}
		}
		t.Fatalf("header differs: got %+v", got)
	}
}

// TestCacheCodecRejectsDamage checks that a truncated or corrupted cache
// is refused or decoded without panicking, never trusted blindly.
func TestCacheCodecRejectsDamage(t *testing.T) {
	enc := encodeCache(codecPayload(t))
	id := func(s string) string { return s }
	step := max(1, len(enc)/400)
	for n := 0; n < len(enc); n += step {
		if _, err := decodeCache(enc[:n], id); err == nil {
			t.Fatalf("truncated to %d of %d bytes: no error", n, len(enc))
		}
	}
	if _, err := decodeCache(append(bytes.Clone(enc), 0), id); err == nil {
		t.Fatal("trailing byte: no error")
	}
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 400; i++ {
		b := bytes.Clone(enc)
		for k := 1 + r.Intn(4); k > 0; k-- {
			b[r.Intn(len(b))] = byte(r.Intn(256))
		}
		_, _ = decodeCache(b, id) // must not panic
	}
}

func FuzzCacheCodec(f *testing.F) {
	cp := &cachePayload{Root: "/w", Key: "k", Skipped: map[string]int{"x": 1}}
	cp.Files = append(cp.Files, analyzeFile("a.go", []byte("package a\n\nfunc Parse() int { return helper() }\n")))
	f.Add(encodeCache(cp))
	f.Fuzz(func(t *testing.T, b []byte) {
		got, err := decodeCache(b, func(s string) string { return s })
		if err != nil {
			return
		}
		if again := encodeCache(got); !bytes.Equal(again, b) {
			// Equal content may encode differently (map order), but it
			// must decode to the same thing.
			got2, err := decodeCache(again, func(s string) string { return s })
			if err != nil || !reflect.DeepEqual(got, got2) {
				t.Fatalf("re-encoding changed the payload: %v", err)
			}
		}
	})
}
