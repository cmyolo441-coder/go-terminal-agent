package codeintel

import (
	"bufio"
	"bytes"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// ignoreFileNames are the per-directory ignore files the walker honours, in
// the order their rules apply. .ignore follows the ripgrep convention for
// rules that should not be committed to .gitignore.
var ignoreFileNames = []string{".gitignore", ".ignore"}

// ignoreRule is one compiled gitignore pattern.
type ignoreRule struct {
	// base is the slash-separated directory, relative to the index root,
	// holding the ignore file. Empty for the root.
	base    string
	re      *regexp.Regexp
	negate  bool
	dirOnly bool
	// source is the original pattern, kept for diagnostics and tests.
	source string
}

// Ignorer evaluates gitignore rules. It is immutable once built: Extend
// returns a new Ignorer, so directory walkers can share parents safely
// across goroutines.
type Ignorer struct {
	rules []ignoreRule
}

// NewIgnorer returns an Ignorer with no rules.
func NewIgnorer() *Ignorer { return &Ignorer{} }

// Extend returns an Ignorer with the patterns from content appended. base is
// the directory, relative to the index root, that the patterns are relative
// to ("" for the root). The receiver is not modified.
func (ig *Ignorer) Extend(base string, content []byte) *Ignorer {
	added := parseIgnore(base, content)
	if len(added) == 0 {
		return ig
	}
	var prev []ignoreRule
	if ig != nil {
		prev = ig.rules
	}
	rules := make([]ignoreRule, 0, len(prev)+len(added))
	rules = append(rules, prev...)
	rules = append(rules, added...)
	return &Ignorer{rules: rules}
}

// Len reports the number of compiled rules.
func (ig *Ignorer) Len() int {
	if ig == nil {
		return 0
	}
	return len(ig.rules)
}

// Match reports whether the slash-separated path rel (relative to the index
// root) is ignored by the rules themselves, without looking at its parent
// directories. The last matching rule wins, so a negated pattern can
// re-include a path excluded earlier.
func (ig *Ignorer) Match(rel string, isDir bool) bool {
	if ig == nil {
		return false
	}
	ignored := false
	for i := range ig.rules {
		r := &ig.rules[i]
		if r.dirOnly && !isDir {
			continue
		}
		sub := rel
		if r.base != "" {
			if !strings.HasPrefix(rel, r.base+"/") {
				continue
			}
			sub = rel[len(r.base)+1:]
		}
		if r.re.MatchString(sub) {
			ignored = !r.negate
		}
	}
	return ignored
}

// Ignored reports whether rel is excluded, either directly or because one
// of its parent directories is. Git never descends into an excluded
// directory, so a file below one cannot be re-included.
func (ig *Ignorer) Ignored(rel string, isDir bool) bool {
	rel = strings.Trim(rel, "/")
	if rel == "" {
		return false
	}
	parts := strings.Split(rel, "/")
	for i := 1; i < len(parts); i++ {
		if ig.Match(strings.Join(parts[:i], "/"), true) {
			return true
		}
	}
	return ig.Match(rel, isDir)
}

// parseIgnore compiles the patterns of one ignore file.
func parseIgnore(base string, content []byte) []ignoreRule {
	base = strings.Trim(path.Clean("/"+filepath.ToSlash(base)), "/")
	var out []ignoreRule
	sc := bufio.NewScanner(bytes.NewReader(content))
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		if r, ok := compileIgnoreLine(base, sc.Text()); ok {
			out = append(out, r)
		}
	}
	return out
}

// compileIgnoreLine turns one gitignore line into a rule, following the
// rules in gitignore(5).
func compileIgnoreLine(base, line string) (ignoreRule, bool) {
	line = strings.TrimSuffix(line, "\r")
	line = strings.TrimPrefix(line, "\ufeff")
	line = trimIgnoreTrailingSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return ignoreRule{}, false
	}
	r := ignoreRule{base: base, source: line}
	if strings.HasPrefix(line, "!") {
		r.negate = true
		line = line[1:]
	} else if strings.HasPrefix(line, `\!`) || strings.HasPrefix(line, `\#`) {
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") && !strings.HasSuffix(line, `\/`) {
		r.dirOnly = true
		line = strings.TrimRight(line, "/")
	}
	if line == "" {
		return ignoreRule{}, false
	}
	// A slash at the start or in the middle anchors the pattern to the
	// directory of the ignore file; otherwise it matches at any depth.
	anchored := strings.Contains(line, "/")
	line = strings.TrimPrefix(line, "/")
	if line == "" {
		return ignoreRule{}, false
	}
	body, ok := globToRegexp(line)
	if !ok {
		return ignoreRule{}, false
	}
	var expr string
	if anchored {
		expr = "^" + body + "$"
	} else {
		expr = "^(?:.*/)?" + body + "$"
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return ignoreRule{}, false
	}
	r.re = re
	return r, true
}

// trimIgnoreTrailingSpace removes trailing spaces unless they are escaped
// with a backslash.
func trimIgnoreTrailingSpace(s string) string {
	for strings.HasSuffix(s, " ") || strings.HasSuffix(s, "\t") {
		if strings.HasSuffix(s, `\ `) {
			// Count the backslashes: an even run means the backslash is
			// itself escaped and the space is not.
			n := 0
			for i := len(s) - 2; i >= 0 && s[i] == '\\'; i-- {
				n++
			}
			if n%2 == 1 {
				return s
			}
		}
		s = s[:len(s)-1]
	}
	return s
}

// globToRegexp translates the body of a gitignore glob into a regular
// expression. "**" has its special meaning only as a whole path segment.
func globToRegexp(p string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(p); {
		c := p[i]
		switch c {
		case '*':
			j := i
			for j < len(p) && p[j] == '*' {
				j++
			}
			if j-i >= 2 && (i == 0 || p[i-1] == '/') && (j == len(p) || p[j] == '/') {
				switch {
				case j == len(p):
					// Trailing "**" (or a lone "**"): everything below.
					b.WriteString(".*")
					i = j
				default:
					// "**/" matches zero or more directories.
					b.WriteString("(?:.*/)?")
					i = j + 1
				}
				continue
			}
			b.WriteString("[^/]*")
			i = j
		case '?':
			b.WriteString("[^/]")
			i++
		case '[':
			cls, n, ok := bracketClass(p[i:])
			if !ok {
				b.WriteString(`\[`)
				i++
				continue
			}
			b.WriteString(cls)
			i += n
		case '\\':
			if i+1 < len(p) {
				b.WriteString(regexp.QuoteMeta(p[i+1 : i+2]))
				i += 2
				continue
			}
			// A trailing backslash matches nothing in git; treat it as a
			// literal so the pattern stays usable.
			b.WriteString(`\\`)
			i++
		default:
			b.WriteString(regexp.QuoteMeta(p[i : i+1]))
			i++
		}
	}
	return b.String(), true
}

// bracketClass converts a glob character class starting at s[0] == '['. It
// returns the regexp class, the number of bytes consumed and whether the
// class was well formed.
func bracketClass(s string) (string, int, bool) {
	i := 1
	var b strings.Builder
	b.WriteString("[")
	if i < len(s) && (s[i] == '!' || s[i] == '^') {
		b.WriteString("^/")
		i++
	}
	first := true
	for i < len(s) {
		c := s[i]
		if c == ']' && !first {
			b.WriteString("]")
			return b.String(), i + 1, true
		}
		first = false
		switch c {
		case '\\':
			if i+1 >= len(s) {
				return "", 0, false
			}
			b.WriteString(regexp.QuoteMeta(s[i+1 : i+2]))
			i += 2
			continue
		case '[':
			// POSIX classes such as [:alpha:] are passed through.
			if strings.HasPrefix(s[i:], "[:") {
				end := strings.Index(s[i+2:], ":]")
				if end >= 0 {
					b.WriteString(s[i : i+2+end+2])
					i += 2 + end + 2
					continue
				}
			}
			b.WriteString(`\[`)
		case ']', '^':
			b.WriteString(`\` + string(c))
		case '-':
			b.WriteByte('-')
		default:
			b.WriteByte(c)
		}
		i++
	}
	return "", 0, false
}

// loadIgnoreFiles extends ig with the ignore files found in dir (absolute),
// whose path relative to the root is rel.
func loadIgnoreFiles(ig *Ignorer, dir, rel string, names map[string]bool) *Ignorer {
	for _, name := range ignoreFileNames {
		if names != nil && !names[name] {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || len(data) == 0 {
			continue
		}
		if len(data) > maxIgnoreFileSize {
			data = data[:maxIgnoreFileSize]
		}
		ig = ig.Extend(rel, data)
	}
	return ig
}

// maxIgnoreFileSize bounds how much of one ignore file is read.
const maxIgnoreFileSize = 1 << 20

// globalExcludesFile returns the user's global git excludes file following
// git's default location rules. core.excludesFile overrides are honoured
// when they are set in ~/.gitconfig with a plain value.
func globalExcludesFile() string {
	home, _ := os.UserHomeDir()
	if home != "" {
		if data, err := os.ReadFile(filepath.Join(home, ".gitconfig")); err == nil {
			if p := parseExcludesFile(string(data)); p != "" {
				if strings.HasPrefix(p, "~/") {
					p = filepath.Join(home, p[2:])
				}
				return p
			}
		}
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "git", "ignore")
	}
	if home != "" {
		return filepath.Join(home, ".config", "git", "ignore")
	}
	return ""
}

// parseExcludesFile extracts core.excludesFile from git config text.
func parseExcludesFile(cfg string) string {
	section := ""
	for _, line := range strings.Split(cfg, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			section = ""
			if f := strings.Fields(strings.Trim(line, "[]")); len(f) > 0 {
				section = strings.ToLower(strings.Trim(f[0], `"`))
			}
			continue
		}
		if section != "core" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "excludesfile") {
			continue
		}
		return strings.Trim(strings.TrimSpace(v), `"`)
	}
	return ""
}
