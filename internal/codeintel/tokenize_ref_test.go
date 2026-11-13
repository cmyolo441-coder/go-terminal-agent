package codeintel

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Reference copies of the original, allocation-heavy tokenizer. The tests
// check that the optimised tokenizer produces exactly the same output.

// SplitIdentifier splits an identifier into its lower-case words:
// "HTTPServerError" -> [http server error], "parse_config2" -> [parse config2].
// Digits stay attached to the letters before them.
func refSplitIdentifier(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	rs := []rune(s)
	for i, r := range rs {
		switch {
		case r == '_' || r == '-' || r == '.' || r == '$' || r == '#' || r == ':' || unicode.IsSpace(r):
			flush()
			continue
		case unicode.IsUpper(r):
			if len(cur) > 0 {
				prev := rs[i-1]
				nextLower := i+1 < len(rs) && unicode.IsLower(rs[i+1])
				if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
					flush()
				}
			}
		case unicode.IsLetter(r) || unicode.IsDigit(r):
		default:
			flush()
			continue
		}
		cur = append(cur, r)
	}
	flush()
	return out
}

// identifiers calls fn for every identifier-like word in text.
func refIdentifiers(text string, fn func(word string)) {
	start := -1
	for i := 0; i < len(text); {
		r, size := rune(text[i]), 1
		if r >= utf8.RuneSelf {
			r, size = utf8.DecodeRuneInString(text[i:])
		}
		isWord := r == '_' || unicode.IsLetter(r) || (start >= 0 && unicode.IsDigit(r))
		if isWord {
			if start < 0 {
				start = i
			}
		} else if start >= 0 {
			fn(text[start:i])
			start = -1
		}
		i += size
	}
	if start >= 0 {
		fn(text[start:])
	}
}

// terms calls emit for every term of text with its multiplicity of one.
// Compound identifiers also emit their whole lower-cased form so exact
// identifier queries score highest.
func refTerms(a analyzer, text string, emit func(term string)) {
	refIdentifiers(text, func(w string) {
		if len(w) > maxTokenLen*2 {
			return
		}
		parts := refSplitIdentifier(w)
		for _, p := range parts {
			if t, ok := refNormalize(a, p); ok {
				emit(t)
			}
		}
		if len(parts) > 1 {
			whole := strings.ToLower(strings.Trim(w, "_"))
			if len(whole) <= maxTokenLen && !isStopword(whole) {
				emit(whole)
			}
		}
	})
}

// normalize filters and stems one lower-case word.
func refNormalize(a analyzer, w string) (string, bool) {
	if len(w) < minTokenLen || len(w) > maxTokenLen || refAllDigits(w) {
		return "", false
	}
	if isStopword(w) {
		return "", false
	}
	if !a.keepKeywords && codeKeywords[w] {
		return "", false
	}
	return refStem(w), true
}

func refAllDigits(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// Stem reduces an English-ish lower-case word to a stem with a small set of
// suffix rules tuned for identifiers. It is deterministic and idempotent
// enough that query and index terms meet.
func refStem(w string) string {
	if len(w) <= 3 || !refIsASCIILower(w) {
		return w
	}
	type rule struct {
		suffix, repl string
		minLen       int // minimum length of w for the rule to apply
	}
	rules := [...]rule{
		{"ission", "it", 8},
		{"ization", "ize", 0},
		{"isation", "ize", 0},
		{"ational", "ate", 0},
		{"ation", "ate", 0},
		{"sion", "de", 8},
		{"tion", "t", 0},
		{"ments", "", 0},
		{"ment", "", 0},
		{"ness", "", 0},
		{"ings", "", 0},
		{"ing", "", 0},
		{"ies", "y", 0},
		{"ied", "y", 0},
		{"edly", "", 0},
		{"ed", "", 0},
		{"ers", "", 0},
		{"er", "", 0},
		{"ly", "", 0},
	}
	s := w
	applied := false
	for _, r := range rules {
		if len(w) < r.minLen || !strings.HasSuffix(w, r.suffix) {
			continue
		}
		cand := w[:len(w)-len(r.suffix)] + r.repl
		if len(cand) < 3 {
			continue
		}
		s = cand
		applied = true
		break
	}
	if !applied {
		switch {
		case strings.HasSuffix(w, "sses"), strings.HasSuffix(w, "xes"), strings.HasSuffix(w, "zes"),
			strings.HasSuffix(w, "ches"), strings.HasSuffix(w, "shes"):
			s = w[:len(w)-2]
		case strings.HasSuffix(w, "ss"), strings.HasSuffix(w, "us"), strings.HasSuffix(w, "is"):
		case strings.HasSuffix(w, "s") && len(w) > 3:
			s = w[:len(w)-1]
		}
	}
	// Undouble a final consonant left by -ing/-ed/-er: "running" -> "run".
	if applied && len(s) >= 4 {
		a, b := s[len(s)-1], s[len(s)-2]
		if a == b && !refIsVowel(a) && a != 'l' && a != 's' && a != 'z' {
			s = s[:len(s)-1]
		}
	}
	if len(s) >= 5 && strings.HasSuffix(s, "e") {
		s = s[:len(s)-1]
	}
	return s
}

func refIsVowel(c byte) bool { return strings.IndexByte("aeiouy", c) >= 0 }

func refIsASCIILower(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 'a' || c > 'z' {
			return false
		}
	}
	return true
}
