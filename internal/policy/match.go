package policy

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// globMatch matches s against a pattern where * matches any run of
// characters (including spaces and slashes) and ? matches one character.
func globMatch(pattern, s string) bool {
	p, i := 0, 0
	star, mark := -1, 0
	for i < len(s) {
		switch {
		case p < len(pattern) && (pattern[p] == '?' || pattern[p] == s[i]):
			p++
			i++
		case p < len(pattern) && pattern[p] == '*':
			star, mark = p, i
			p++
		case star >= 0:
			p = star + 1
			mark++
			i = mark
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

// namePattern matches tool, provider and model names.
type namePattern struct {
	raw  string
	glob string
	fold bool
}

func compileName(p string, fold bool) (namePattern, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return namePattern{}, fmt.Errorf("empty pattern")
	}
	g := p
	if fold {
		g = strings.ToLower(g)
	}
	return namePattern{raw: p, glob: g, fold: fold}, nil
}

func (n namePattern) match(s string) bool {
	if n.fold {
		s = strings.ToLower(s)
	}
	return globMatch(n.glob, s)
}

// cmdPattern matches one simple command (program and arguments).
//
//   - "re:<regexp>" is a regular expression over the normalised command.
//   - A pattern without * or ? matches the command exactly or as a prefix
//     followed by arguments: "curl" matches "curl -s https://x".
//   - Otherwise * matches anything; a trailing " *" also matches the bare
//     program: "rm -rf *" matches "rm -rf /tmp/x", "git push *" matches
//     "git push".
type cmdPattern struct {
	raw  string
	glob string
	re   *regexp.Regexp
}

func compileCommand(p string) (cmdPattern, error) {
	if strings.TrimSpace(p) == "" {
		return cmdPattern{}, fmt.Errorf("empty pattern")
	}
	if expr, ok := strings.CutPrefix(p, "re:"); ok {
		re, err := regexp.Compile(expr)
		if err != nil {
			return cmdPattern{}, err
		}
		return cmdPattern{raw: p, re: re}, nil
	}
	return cmdPattern{raw: p, glob: strings.Join(strings.Fields(p), " ")}, nil
}

func (c cmdPattern) match(cmd string) bool {
	if c.re != nil {
		return c.re.MatchString(cmd)
	}
	if !strings.ContainsAny(c.glob, "*?") {
		return cmd == c.glob || strings.HasPrefix(cmd, c.glob+" ")
	}
	if globMatch(c.glob, cmd) {
		return true
	}
	if bare, ok := strings.CutSuffix(c.glob, " *"); ok && cmd == bare {
		return true
	}
	return false
}

// pathPattern matches absolute, slash-separated paths with doublestar
// globs. A pattern also covers everything under a matching directory, so
// "~/.ssh" protects "~/.ssh/id_ed25519".
type pathPattern struct {
	raw  string
	glob string
	// alt is the pattern with symlinks in its literal prefix resolved
	// (macOS /var is /private/var), or "" when that changes nothing. A
	// path matches either form, so a rule holds however it was written.
	alt string
}

func compilePath(p, home string, fold bool) (pathPattern, error) {
	raw := p
	p = strings.TrimSpace(p)
	if p == "" {
		return pathPattern{}, fmt.Errorf("empty pattern")
	}
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home == "" {
			return pathPattern{}, fmt.Errorf("%q needs a home directory", raw)
		}
		p = filepath.Join(home, p[1:])
	}
	p = filepath.ToSlash(p)
	if !isAbsSlash(p) {
		p = "**/" + strings.TrimPrefix(p, "./")
	}
	if len(p) > 1 {
		p = strings.TrimSuffix(p, "/")
	}
	alt := resolvePatternPrefix(p)
	if fold {
		p, alt = strings.ToLower(p), strings.ToLower(alt)
	}
	if !doublestar.ValidatePattern(p) {
		return pathPattern{}, fmt.Errorf("invalid glob %q", raw)
	}
	if alt == p || !doublestar.ValidatePattern(alt) {
		alt = ""
	}
	return pathPattern{raw: raw, glob: p, alt: alt}, nil
}

// resolvePatternPrefix resolves symlinks in the literal leading directories
// of an absolute slash pattern and returns the rewritten pattern, or "" when
// the pattern is relative or its prefix does not exist.
func resolvePatternPrefix(p string) string {
	if !isAbsSlash(p) {
		return ""
	}
	parts := strings.Split(p, "/")
	n := len(parts)
	for i, seg := range parts {
		if strings.ContainsAny(seg, "*?[{\\") {
			n = i
			break
		}
	}
	for ; n > 1; n-- {
		prefix := strings.Join(parts[:n], "/")
		if prefix == "" {
			return ""
		}
		r, err := filepath.EvalSymlinks(filepath.FromSlash(prefix))
		if err != nil {
			continue
		}
		rest := strings.Join(parts[n:], "/")
		out := filepath.ToSlash(r)
		if rest != "" {
			out = strings.TrimSuffix(out, "/") + "/" + rest
		}
		return out
	}
	return ""
}

// isAbsSlash reports /x and C:/x style absolute paths.
func isAbsSlash(p string) bool {
	if strings.HasPrefix(p, "/") {
		return true
	}
	return len(p) >= 3 && p[1] == ':' && p[2] == '/' && ((p[0] >= 'a' && p[0] <= 'z') || (p[0] >= 'A' && p[0] <= 'Z'))
}

// match reports whether p or one of its ancestors matches.
func (pp pathPattern) match(p string) bool {
	for {
		if ok, _ := doublestar.Match(pp.glob, p); ok {
			return true
		}
		if pp.alt != "" {
			if ok, _ := doublestar.Match(pp.alt, p); ok {
				return true
			}
		}
		i := strings.LastIndexByte(p, '/')
		if i <= 0 || (i == 2 && p[1] == ':') {
			// Root: try it, then stop.
			root := p[:i+1]
			if root != p && root != "" {
				ok, _ := doublestar.Match(pp.glob, root)
				if !ok && pp.alt != "" {
					ok, _ = doublestar.Match(pp.alt, root)
				}
				return ok
			}
			return false
		}
		p = p[:i]
	}
}

// pathCandidates returns the forms of p a rule must be checked against:
// the cleaned absolute path and, when it differs, the path with symlinks
// resolved (for a file that does not exist yet, its directory's). All are
// slash-separated and lower-cased when fold is set.
func pathCandidates(p, root, home string, fold bool) []string {
	if p == "" {
		return nil
	}
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home != "" {
			p = filepath.Join(home, p[1:])
		}
	}
	if !filepath.IsAbs(p) {
		if root == "" {
			if wd, err := os.Getwd(); err == nil {
				root = wd
			}
		}
		p = filepath.Join(root, p)
	}
	p = filepath.Clean(p)
	out := []string{p}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		out = append(out, r)
	} else if d, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		out = append(out, filepath.Join(d, filepath.Base(p)))
	}
	seen := map[string]bool{}
	res := out[:0]
	for _, c := range out {
		c = filepath.ToSlash(c)
		if fold {
			c = strings.ToLower(c)
		}
		if !seen[c] {
			seen[c] = true
			res = append(res, c)
		}
	}
	return res
}
