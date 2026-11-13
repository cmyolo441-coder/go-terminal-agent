package codeintel

import (
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// tokenizerCorpus is source text from this repository plus random strings
// mixing ASCII, punctuation, digits and tricky Unicode (Kelvin sign,
// dotted I, NEL, no-break space, invalid UTF-8).
func tokenizerCorpus(t testing.TB) []string {
	t.Helper()
	var corpus []string
	root := filepath.Join("..", "..")
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == ".claude") {
			return filepath.SkipDir
		}
		if !d.IsDir() && (strings.HasSuffix(p, ".go") || strings.HasSuffix(p, ".md")) && len(corpus) < 400 {
			if data, err := os.ReadFile(p); err == nil {
				corpus = append(corpus, string(data))
			}
		}
		return nil
	})
	alphabet := []string{"a", "b", "e", "s", "i", "o", "n", "g", "d", "r", "t", "y", "l", "z", "A", "B", "S", "T", "Z", "_", "-", ".", "$", "#", ":",
		"0", "7", " ", "\t", "\n", "\v", "(", ")", "é", "É", "ß", "İ", "K", "Σ", "σ", "\u0085", " ", "\xff", "日", "١",
		"ing", "tion", "ission", "ed", "er", "ies", "sses", "ches", "ment", "ation", "HTTP", "Server", "XMLHttp"}
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 20000; i++ {
		var b strings.Builder
		for n := r.Intn(14); n >= 0; n-- {
			b.WriteString(alphabet[r.Intn(len(alphabet))])
		}
		corpus = append(corpus, b.String())
	}
	corpus = append(corpus, "", "_", "__init__", "HTTPServerError", "parse_config2", strings.Repeat("abc", 50), strings.Repeat("Ab", 40))
	return corpus
}

func TestTokenizerMatchesReference(t *testing.T) {
	for _, text := range tokenizerCorpus(t) {
		checkTokenizer(t, text)
	}
}

func FuzzTokenizerMatchesReference(f *testing.F) {
	for _, s := range []string{"HTTPServerError", "parse_config2 running", "İstanbul_KÉLVINK", "a\u0085b C", "\xffxYz"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) { checkTokenizer(t, s) })
}

func checkTokenizer(t *testing.T, text string) {
	t.Helper()
	var got, want []string
	identifiers(text, func(w string) { got = append(got, w) })
	refIdentifiers(text, func(w string) { want = append(want, w) })
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("identifiers(%q) = %q, want %q", text, got, want)
	}
	for _, w := range append(want, text) {
		if g, r := SplitIdentifier(w), refSplitIdentifier(w); !reflect.DeepEqual(g, r) {
			t.Fatalf("SplitIdentifier(%q) = %q, want %q", w, g, r)
		}
		if g, r := identifierParts(w), len(refSplitIdentifier(w)); g != r {
			t.Fatalf("identifierParts(%q) = %d, want %d", w, g, r)
		}
		lw := strings.ToLower(w)
		if g, r := Stem(lw), refStem(lw); g != r {
			t.Fatalf("Stem(%q) = %q, want %q", lw, g, r)
		}
	}
	for _, a := range []analyzer{{}, {keepKeywords: true}} {
		got, want = nil, nil
		a.terms(text, func(s string) { got = append(got, s) })
		refTerms(a, text, func(s string) { want = append(want, s) })
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("terms(%q, %+v) = %q, want %q", text, a, got, want)
		}
		got = got[:0]
		var tb termBuf
		a.scan(text, &tb, func(s string) { got = append(got, strings.Clone(s)) })
		if !reflect.DeepEqual(got, want) && !(len(got) == 0 && len(want) == 0) {
			t.Fatalf("scan(%q, %+v) = %q, want %q", text, a, got, want)
		}
	}
}
