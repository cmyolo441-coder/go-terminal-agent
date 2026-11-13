package codeintel

import (
	"reflect"
	"regexp"
	"regexp/syntax"
	"strings"
	"testing"
)

func TestRequiredLiterals(t *testing.T) {
	cases := map[string][]string{
		`^\s*def\s+(?P<name>\w+)`:           {"def"},
		`^\s*(?:async\s+)?def\s+\w+\s*[\(]`: {"def"},
		`(?P<kind>const|let|var)\s+x`:       {"const", "let", "var"},
		`^\s*(?:export\s+)?function`:        {"function"},
		`a*b?`:                              nil,
		`(?i)class`:                         nil,
		`x|\w+`:                             nil,
		`(?:ab){2,}`:                        {"ab"},
	}
	for src, want := range cases {
		re, err := syntax.Parse(src, syntax.Perl)
		if err != nil {
			t.Fatal(err)
		}
		got := requiredLiterals(re)
		// Alternations may be factored by the parser ("let|var" stays,
		// "const|let" may not); compare as sets of what must be found.
		if want == nil {
			if got != nil {
				t.Errorf("%s: got %q, want nil", src, got)
			}
			continue
		}
		for _, w := range want {
			found := false
			for _, g := range got {
				if strings.Contains(w, g) {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: got %q, which does not cover %q", src, got, w)
			}
		}
	}
}

// TestLazyRegexpMatchesRegexp checks that the literal prefilter never
// changes a match of any extractor pattern.
func TestLazyRegexpMatchesRegexp(t *testing.T) {
	var lines []string
	corpus := tokenizerCorpus(t)
	for _, text := range corpus {
		if len(lines) >= 12000 {
			break
		}
		lines = append(lines, strings.Split(text, "\n")...) // source files come first
	}
	lines = append(lines, corpus[len(corpus)-3000:]...) // and random strings
	lines = append(lines, "def f(", "class A:", "export const x = () =>", "  pub fn main() {", "#define X 1",
		"impl<T> Foo for Bar<T> {", "public static void main(String[] a) {", "module Foo::Bar", "X = 1")
	check := func(l *lazyRegexp) {
		if l == nil {
			return
		}
		plain := regexp.MustCompile(l.src)
		for _, line := range lines {
			if got, want := l.FindStringSubmatchIndex(line), plain.FindStringSubmatchIndex(line); !reflect.DeepEqual(got, want) {
				t.Fatalf("%s on %q: got %v, want %v", l.src, line, got, want)
			}
			if got, want := l.MatchString(line), plain.MatchString(line); got != want {
				t.Fatalf("%s on %q: MatchString %v, want %v", l.src, line, got, want)
			}
		}
	}
	for _, lp := range langPatterns {
		for i := range lp.patterns {
			check(lp.patterns[i].re)
			check(lp.patterns[i].skipIf)
		}
	}
}
