package codeintel

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Tokens are the unit of the ranked search. Code and queries are analysed
// the same way: identifiers are split into their camelCase and snake_case
// parts, lower-cased, stripped of stop words and reduced to a light stem so
// that "decided" matches "Decision" and "permission" matches "permit".

const (
	minTokenLen = 2
	maxTokenLen = 64
)

// SplitIdentifier splits an identifier into its lower-case words:
// "HTTPServerError" -> [http server error], "parse_config2" -> [parse config2].
// Digits stay attached to the letters before them.
func SplitIdentifier(s string) []string {
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
func identifiers(text string, fn func(word string)) {
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

// analyzer turns text into index terms.
type analyzer struct {
	// keepKeywords keeps language keywords; used for symbol names, where a
	// word like "default" is meaningful.
	keepKeywords bool
}

// terms calls emit for every term of text with its multiplicity of one.
// Compound identifiers also emit their whole lower-cased form so exact
// identifier queries score highest.
func (a analyzer) terms(text string, emit func(term string)) {
	identifiers(text, func(w string) {
		if len(w) > maxTokenLen*2 {
			return
		}
		parts := SplitIdentifier(w)
		for _, p := range parts {
			if t, ok := a.normalize(p); ok {
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
func (a analyzer) normalize(w string) (string, bool) {
	if len(w) < minTokenLen || len(w) > maxTokenLen || allDigits(w) {
		return "", false
	}
	if isStopword(w) {
		return "", false
	}
	if !a.keepKeywords && codeKeywords[w] {
		return "", false
	}
	return Stem(w), true
}

func allDigits(s string) bool {
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
func Stem(w string) string {
	if len(w) <= 3 || !isASCIILower(w) {
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
		if a == b && !isVowel(a) && a != 'l' && a != 's' && a != 'z' {
			s = s[:len(s)-1]
		}
	}
	if len(s) >= 5 && strings.HasSuffix(s, "e") {
		s = s[:len(s)-1]
	}
	return s
}

func isVowel(c byte) bool { return strings.IndexByte("aeiouy", c) >= 0 }

func isASCIILower(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 'a' || c > 'z' {
			return false
		}
	}
	return true
}

func isStopword(w string) bool { return stopwords[w] }

var stopwords = setOf(
	"a", "an", "the", "is", "are", "was", "were", "be", "been", "being", "of", "in", "on", "at",
	"to", "for", "by", "with", "and", "or", "it", "its", "this", "that", "these", "those",
	"there", "here", "what", "which", "who", "whom", "whose", "when", "why", "how", "does",
	"did", "can", "could", "should", "would", "will", "shall", "may", "might", "must", "we",
	"you", "he", "she", "they", "me", "us", "my", "our", "your", "their", "them", "so", "than",
	"too", "very", "just", "also", "into", "about", "some", "each", "every", "such", "only",
	"own", "same", "other", "out", "up", "down", "over", "under", "again", "further", "once",
	"both", "few", "more", "most", "any", "all", "because", "while", "until", "against",
	"between", "through", "during", "before", "after", "above", "below", "off", "nor",
)

// queryFiller are words common in natural-language questions about code
// that carry no signal of their own.
var queryFiller = setOf(
	"where", "code", "logic", "located", "locate", "location", "happens", "happen", "place",
	"part", "piece", "somewhere", "implemented", "implementation", "implement", "handled",
	"defined", "definition", "function", "functions", "method", "methods", "func", "file",
	"files", "find", "show", "me", "get", "look", "looking", "see", "want", "need", "know",
	"thing", "things", "stuff", "way", "use", "used", "using", "work", "works", "working",
	"do", "done", "does", "not", "no", "if", "then", "else", "from", "as", "i",
)

// codeKeywords are language keywords dropped from code text; they occur
// everywhere and only add postings.
var codeKeywords = setOf(
	"func", "return", "if", "else", "elif", "for", "while", "do", "switch", "case", "break",
	"continue", "default", "goto", "var", "let", "const", "nil", "null", "none", "undefined",
	"true", "false", "self", "this", "def", "end", "import", "package", "from", "as", "in",
	"not", "and", "or", "try", "catch", "except", "finally", "throw", "throws", "raise",
	"new", "void", "static", "public", "private", "protected", "final", "pub", "fn", "mut",
	"impl", "use", "mod", "crate", "super", "extends", "implements", "async", "await",
	"yield", "lambda", "go", "defer", "chan", "range", "int", "string", "bool", "byte",
	"float", "double", "char", "long", "unsigned", "signed", "include", "define", "endif",
	"ifdef", "ifndef", "pragma", "typeof", "instanceof", "elsif", "unless", "begin", "then",
	"fi", "esac", "done", "err", "ok",
)

func setOf(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

// synonymGroups are code-domain equivalences: abbreviations and words that
// name the same concept. They let a natural-language query reach code that
// uses a different word for the same idea.
var synonymGroups = [][]string{
	{"prompt", "ask", "approve", "approval", "confirm", "consent"},
	{"decide", "decision", "check", "verdict", "evaluate", "determine", "judge"},
	{"permission", "permit", "allow", "deny", "authorize", "authorization", "gate", "policy"},
	{"error", "fail", "failure", "exception", "panic", "fault"},
	{"delete", "remove", "rm", "del", "erase", "unlink", "drop", "purge"},
	{"create", "new", "make", "add", "insert", "spawn"},
	{"config", "configuration", "settings", "cfg", "conf", "options", "opts", "preferences"},
	{"auth", "authenticate", "authentication", "login", "signin", "logon", "credential", "credentials"},
	{"logout", "signout", "logoff"},
	{"start", "init", "initialize", "setup", "launch", "boot", "bootstrap"},
	{"stop", "shutdown", "close", "exit", "terminate", "kill", "quit", "cancel"},
	{"message", "msg"},
	{"request", "req"},
	{"response", "resp", "reply"},
	{"directory", "dir", "folder"},
	{"function", "func", "fn", "procedure"},
	{"test", "spec", "assert"},
	{"parse", "decode", "unmarshal", "deserialize", "lex", "scan"},
	{"serialize", "encode", "marshal", "stringify", "format"},
	{"database", "db", "sql", "storage", "persist"},
	{"argument", "arg", "args", "param", "parameter", "flag"},
	{"context", "ctx"},
	{"connection", "conn", "socket"},
	{"count", "cnt", "num", "number", "total"},
	{"buffer", "buf"},
	{"length", "len", "size"},
	{"command", "cmd"},
	{"execute", "exec", "run", "invoke", "dispatch"},
	{"environment", "env"},
	{"value", "val"},
	{"manager", "mgr"},
	{"source", "src"},
	{"destination", "dst", "dest", "target"},
	{"previous", "prev"},
	{"current", "cur", "curr"},
	{"maximum", "max", "limit", "cap"},
	{"minimum", "min"},
	{"reference", "ref", "refs"},
	{"repository", "repo"},
	{"document", "doc", "docs"},
	{"password", "pwd", "passwd", "secret"},
	{"temporary", "tmp", "temp"},
	{"index", "idx"},
	{"image", "img"},
	{"character", "char", "rune"},
	{"string", "str", "text"},
	{"callback", "cb", "handler", "hook", "listener"},
	{"iterate", "loop", "walk", "traverse"},
	{"search", "find", "lookup", "query", "grep", "match"},
	{"cache", "memo", "memoize"},
	{"log", "logger", "logging", "trace", "logf"},
	{"user", "usr", "account"},
	{"url", "uri", "link", "href"},
	{"timeout", "deadline", "ttl", "expire", "expiry"},
	{"retry", "backoff", "attempt"},
	{"render", "draw", "display", "show", "view", "print"},
	{"update", "modify", "change", "edit", "patch", "mutate"},
	{"get", "fetch", "retrieve", "load", "read"},
	{"save", "store", "write", "persist", "flush"},
	{"send", "emit", "publish", "post"},
	{"receive", "recv", "consume", "subscribe", "listen"},
	{"concurrent", "parallel", "goroutine", "thread", "worker"},
	{"lock", "mutex", "mu", "semaphore"},
	{"validate", "validation", "verify", "sanitize"},
	{"tool", "plugin", "extension"},
	{"model", "llm", "provider"},
	{"cost", "price", "pricing", "billing", "usage"},
	{"checkpoint", "snapshot", "backup", "restore", "undo", "rewind"},
	{"history", "transcript", "session", "conversation"},
	{"stream", "streaming", "chunk", "delta"},
}

// synonyms maps a stem to the stems that share a group with it.
var synonyms = func() map[string][]string {
	m := map[string][]string{}
	for _, g := range synonymGroups {
		stems := make([]string, 0, len(g))
		seen := map[string]bool{}
		for _, w := range g {
			s := Stem(w)
			if !seen[s] {
				seen[s] = true
				stems = append(stems, s)
			}
		}
		for _, s := range stems {
			for _, o := range stems {
				if o != s && !contains(m[s], o) {
					m[s] = append(m[s], o)
				}
			}
		}
	}
	return m
}()

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
