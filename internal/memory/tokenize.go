package memory

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// token is one word of a text: Raw is the lower-cased surface form, Term the
// stemmed form used for matching. Stop marks function words.
type token struct {
	Raw  string
	Term string
	Stop bool
}

// maxTokenBytes drops pathological "words" such as base64 blobs.
const maxTokenBytes = 64

// tokenize splits text into words. A word is a maximal run of letters and
// digits; identifiers are split at case changes and letter/digit boundaries
// ("parseHTTPRequest2" -> parse, http, request, 2) and, when split, the
// joined identifier is emitted as well so exact identifier queries still
// match. Ideographic characters become one token each.
func tokenize(text string) []token {
	toks := make([]token, 0, len(text)/6+1)
	start := -1
	flush := func(end int) {
		if start < 0 {
			return
		}
		word := text[start:end]
		start = -1
		if strings.ContainsAny(word, "'\u2019") {
			// Possessives lose their "'s" ("user's" -> user); other
			// contractions are joined ("don't" -> dont).
			for _, suf := range []string{"'s", "'S", "\u2019s", "\u2019S"} {
				word = strings.TrimSuffix(word, suf)
			}
			word = apostrophes.Replace(word)
		}
		if word == "" || len(word) > maxTokenBytes {
			return
		}
		parts := splitIdentifier(word)
		for _, p := range parts {
			toks = appendToken(toks, strings.ToLower(p))
		}
		if len(parts) > 1 {
			toks = appendToken(toks, strings.ToLower(word))
		}
	}
	for i, c := range text {
		switch {
		case isIdeograph(c):
			flush(i)
			toks = appendToken(toks, string(c))
		case isWordRune(c):
			if start < 0 {
				start = i
			}
		case (c == '\'' || c == '\u2019') && start >= 0:
			// Keep an apostrophe inside a word only when a letter follows.
			if next, _ := utf8.DecodeRuneInString(text[i+utf8.RuneLen(c):]); !unicode.IsLetter(next) {
				flush(i)
			}
		default:
			flush(i)
		}
	}
	flush(len(text))
	return toks
}

var apostrophes = strings.NewReplacer("'", "", "\u2019", "")

func isWordRune(c rune) bool { return unicode.IsLetter(c) || unicode.IsDigit(c) }

func isIdeograph(c rune) bool {
	return c >= 0x2E80 && (unicode.Is(unicode.Han, c) || unicode.Is(unicode.Hiragana, c) ||
		unicode.Is(unicode.Katakana, c) || unicode.Is(unicode.Hangul, c))
}

func appendToken(toks []token, raw string) []token {
	if raw == "" {
		return toks
	}
	return append(toks, makeToken(raw))
}

func makeToken(raw string) token {
	return token{Raw: raw, Term: stem(raw), Stop: stopwords[raw]}
}

// splitIdentifier splits camelCase, PascalCase, ACRONYMWord and
// letter/digit boundaries. Words without boundaries come back unchanged.
func splitIdentifier(w string) []string {
	var parts []string
	start := 0
	prev := rune(-1)
	for i, c := range w {
		if prev >= 0 && i > start {
			split := false
			switch {
			case unicode.IsLower(prev) && unicode.IsUpper(c):
				split = true
			case unicode.IsLetter(prev) && unicode.IsDigit(c), unicode.IsDigit(prev) && unicode.IsLetter(c):
				split = true
			case unicode.IsUpper(prev) && unicode.IsUpper(c):
				// "HTTPServer": split before the last capital of a run
				// when a lower-case letter follows it.
				next, _ := utf8.DecodeRuneInString(w[i+utf8.RuneLen(c):])
				split = unicode.IsLower(next)
			}
			if split {
				parts = append(parts, w[start:i])
				start = i
			}
		}
		prev = c
	}
	if start == 0 {
		return []string{w}
	}
	return append(parts, w[start:])
}

// stem is a deliberately conservative English suffix stripper: it folds
// plurals and the common -ing/-ed/-ly forms so that "tests", "testing" and
// "tested" meet, without the aggressive conflation of a full Porter stemmer
// that hurts identifiers. Non-ASCII words are returned unchanged.
func stem(w string) string {
	n := len(w)
	if n <= 3 {
		return w
	}
	for i := 0; i < n; i++ {
		if c := w[i]; c >= utf8.RuneSelf || (c >= '0' && c <= '9') {
			return w
		}
	}
	switch {
	case strings.HasSuffix(w, "sses"):
		return w[:n-2]
	case strings.HasSuffix(w, "ies") && n > 4:
		return w[:n-3] + "y"
	case strings.HasSuffix(w, "ness") && n > 6:
		return w[:n-4]
	case strings.HasSuffix(w, "ing") && n > 5:
		return undouble(w[:n-3])
	case strings.HasSuffix(w, "edly") && n > 6:
		return undouble(w[:n-4])
	case strings.HasSuffix(w, "ed") && n > 4 && hasVowel(w[:n-2]):
		return undouble(w[:n-2])
	case strings.HasSuffix(w, "ly") && n > 5:
		return w[:n-2]
	case strings.HasSuffix(w, "es") && n > 4 && strings.ContainsRune("sxz", rune(w[n-3])):
		return w[:n-2]
	case strings.HasSuffix(w, "ches") || strings.HasSuffix(w, "shes"):
		return w[:n-2]
	case w[n-1] == 's' && !strings.HasSuffix(w, "ss") && !strings.HasSuffix(w, "us") && !strings.HasSuffix(w, "is"):
		return w[:n-1]
	}
	return w
}

func hasVowel(s string) bool { return strings.ContainsAny(s, "aeiouy") }

// undouble turns "runn" into "run" and "stopp" into "stop" but keeps
// "ll", "ss" and "zz", which are usually part of the root ("install").
func undouble(s string) string {
	n := len(s)
	if n >= 3 && s[n-1] == s[n-2] && !strings.ContainsRune("aeiouylsz", rune(s[n-1])) {
		return s[:n-1]
	}
	return s
}

// stopwords are English function words: dropped from the lexical index and
// from word-level embedding features (character n-grams still see them via
// neighbouring words, so phrase structure is not lost entirely).
var stopwords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`a about above after again against all am an and any are as at be
		because been before being below between both but by can could did do does doing down during
		each few for from further had has have having he her here hers herself him himself his how i
		if in into is it its itself just me more most my myself no nor not now of off on once only or
		other our ours ourselves out over own same she should so some such than that the their theirs
		them themselves then there these they this those through to too under until up very was we
		were what when where which while who whom why will with would you your yours yourself
		yourselves also let lets us via`) {
		m[w] = true
	}
	return m
}()

// lexicalTerms returns the BM25 terms of text: stemmed, without stopwords.
func lexicalTerms(text string) []string {
	toks := tokenize(text)
	out := make([]string, 0, len(toks))
	for _, t := range toks {
		if !t.Stop {
			out = append(out, t.Term)
		}
	}
	return out
}

// normalizedKey is a canonical form of text used for exact-duplicate
// detection: content words in order, stemmed, separated by spaces.
func normalizedKey(text string) string {
	return strings.Join(lexicalTerms(text), " ")
}
