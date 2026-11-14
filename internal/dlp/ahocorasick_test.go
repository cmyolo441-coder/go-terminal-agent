package dlp

import (
	"errors"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

func naiveFind(patterns []string, text string, fold bool) []ACMatch {
	var out []ACMatch
	t := text
	if fold {
		t = asciiLowerTest(text)
	}
	for pi, p := range patterns {
		if fold {
			p = asciiLowerTest(p)
		}
		for i := 0; i+len(p) <= len(t); i++ {
			if t[i:i+len(p)] == p {
				out = append(out, ACMatch{Pattern: pi, Start: i, End: i + len(p)})
			}
		}
	}
	return out
}

func asciiLowerTest(s string) string {
	b := []byte(s)
	for i, c := range b {
		b[i] = foldByte(c)
	}
	return string(b)
}

func sortMatches(m []ACMatch) {
	sort.Slice(m, func(i, j int) bool {
		if m[i].Start != m[j].Start {
			return m[i].Start < m[j].Start
		}
		if m[i].End != m[j].End {
			return m[i].End < m[j].End
		}
		return m[i].Pattern < m[j].Pattern
	})
}

func equalMatches(a, b []ACMatch) bool {
	if len(a) != len(b) {
		return false
	}
	sortMatches(a)
	sortMatches(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func checkAC(t testing.TB, patterns []string, text string, fold bool) {
	t.Helper()
	for _, limit := range []int{0, 1} { // dense DFA and goto/failure fallback
		ac, err := NewAhoCorasick(patterns, ACOptions{CaseInsensitive: fold, MaxDFAEntries: limit})
		if err != nil {
			t.Fatal(err)
		}
		if (limit == 1) == ac.Dense() && len(patterns) > 0 {
			t.Fatalf("limit %d: Dense() = %v", limit, ac.Dense())
		}
		got := ac.FindAll(text)
		want := naiveFind(patterns, text, fold)
		if !equalMatches(got, want) {
			t.Fatalf("patterns %q text %q fold %v dense %v:\n got %v\nwant %v", patterns, text, fold, ac.Dense(), got, want)
		}
		if ac.Contains(text) != (len(want) > 0) {
			t.Fatalf("Contains mismatch")
		}
	}
}

func TestAhoCorasickBasic(t *testing.T) {
	checkAC(t, []string{"he", "she", "his", "hers"}, "ushers and his shed", false)
	checkAC(t, []string{"a", "aa", "aaa"}, "aaaaa", false)
	checkAC(t, []string{"SSN", "ssn number"}, "My Ssn Number and SSN", true)
	checkAC(t, []string{"abc", "abc"}, "xxabcxx", false) // duplicates
	checkAC(t, []string{"ü", "naïve"}, "Naïve über ü", true)
	checkAC(t, nil, "anything", false)
	if _, err := NewAhoCorasick([]string{"ok", ""}, ACOptions{}); !errors.Is(err, ErrEmptyPattern) {
		t.Fatalf("err = %v", err)
	}
}

func TestAhoCorasickOrderAndStop(t *testing.T) {
	ac, _ := NewAhoCorasick([]string{"b", "ab", "cab"}, ACOptions{})
	var got []ACMatch
	ac.Iterate("xcab", func(m ACMatch) bool { got = append(got, m); return true })
	// Same end: longest first.
	if len(got) != 3 || got[0].Pattern != 2 || got[1].Pattern != 1 || got[2].Pattern != 0 {
		t.Fatalf("order %v", got)
	}
	n := 0
	ac.Iterate("abababab", func(ACMatch) bool { n++; return n < 3 })
	if n != 3 {
		t.Fatalf("stop: %d", n)
	}
}

func TestAhoCorasickRandom(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	alpha := "abAB c"
	gen := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = alpha[rng.Intn(len(alpha))]
		}
		return string(b)
	}
	for iter := 0; iter < 500; iter++ {
		pats := make([]string, 1+rng.Intn(12))
		for i := range pats {
			pats[i] = gen(1 + rng.Intn(5))
		}
		checkAC(t, pats, gen(rng.Intn(200)), iter%2 == 0)
	}
}

func TestAhoCorasickLarge(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	pats := make([]string, 20000)
	for i := range pats {
		b := make([]byte, 4+rng.Intn(8))
		for j := range b {
			b[j] = byte('a' + rng.Intn(26))
		}
		pats[i] = string(b)
	}
	ac, err := NewAhoCorasick(pats, ACOptions{CaseInsensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Repeat("lorem ipsum "+pats[123]+" dolor ", 50)
	found := false
	for _, m := range ac.FindAll(text) {
		if m.Pattern == 123 {
			found = true
		}
	}
	if !found {
		t.Fatal("pattern 123 not found")
	}
}

// FuzzAhoCorasick checks the automaton against a naive matcher. The first
// input holds newline-separated patterns.
func FuzzAhoCorasick(f *testing.F) {
	f.Add("he\nshe\nhis\nhers", "ushers", true)
	f.Add("a\naa\naaa", "aaaaaaa", false)
	f.Add("SSN\nssn number\nDoB", "my ssn NUMBER and dob", true)
	f.Add("\x00\xff\n\xff", "\xff\x00\xff\xff", false)
	f.Fuzz(func(t *testing.T, pats, text string, fold bool) {
		var ps []string
		for _, p := range strings.Split(pats, "\n") {
			if p != "" && len(ps) < 64 && len(p) <= 32 {
				ps = append(ps, p)
			}
		}
		if len(text) > 4096 {
			text = text[:4096]
		}
		checkAC(t, ps, text, fold)
	})
}
