package codeintel

import (
	"regexp"
	"regexp/syntax"
	"strings"
	"sync"
)

// lazyRegexp is a regular expression compiled on first use, so the many
// extractor patterns cost nothing at program start. It also knows literal
// strings one of which every match must contain, which lets most lines be
// rejected with a substring search instead of running the regexp.
type lazyRegexp struct {
	src  string
	once sync.Once
	re   *regexp.Regexp
	need []string // a match contains one of these; nil when unknown
}

func newLazyRegexp(src string) *lazyRegexp { return &lazyRegexp{src: src} }

func (l *lazyRegexp) get() *regexp.Regexp {
	l.once.Do(func() {
		l.re = regexp.MustCompile(l.src)
		if parsed, err := syntax.Parse(l.src, syntax.Perl); err == nil {
			l.need = requiredLiterals(parsed)
		}
	})
	return l.re
}

// mayMatch reports whether s can match: false means it certainly does not.
func (l *lazyRegexp) mayMatch(s string) bool {
	l.get()
	if l.need == nil {
		return true
	}
	for _, n := range l.need {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

func (l *lazyRegexp) FindStringSubmatchIndex(s string) []int {
	if !l.mayMatch(s) {
		return nil
	}
	return l.re.FindStringSubmatchIndex(s)
}

func (l *lazyRegexp) MatchString(s string) bool {
	return l.mayMatch(s) && l.re.MatchString(s)
}

func (l *lazyRegexp) SubexpIndex(name string) int { return l.get().SubexpIndex(name) }

// requiredLiterals returns strings one of which every match of re must
// contain, or nil when it cannot tell. It errs on the side of nil: a
// non-nil answer is always safe to filter on.
func requiredLiterals(re *syntax.Regexp) []string {
	switch re.Op {
	case syntax.OpLiteral:
		if re.Flags&syntax.FoldCase != 0 || len(re.Rune) == 0 {
			return nil
		}
		return []string{string(re.Rune)}
	case syntax.OpCapture, syntax.OpPlus:
		return requiredLiterals(re.Sub[0])
	case syntax.OpRepeat:
		if re.Min >= 1 {
			return requiredLiterals(re.Sub[0])
		}
	case syntax.OpConcat:
		var best []string
		for _, sub := range re.Sub {
			if c := requiredLiterals(sub); c != nil && betterLiterals(c, best) {
				best = c
			}
		}
		return best
	case syntax.OpAlternate:
		var out []string
		for _, sub := range re.Sub {
			c := requiredLiterals(sub)
			if c == nil {
				return nil
			}
			out = append(out, c...)
		}
		return out
	}
	return nil
}

// betterLiterals prefers the set whose shortest literal is longer, then
// the smaller set.
func betterLiterals(a, b []string) bool {
	if b == nil {
		return true
	}
	ma, mb := minLen(a), minLen(b)
	if ma != mb {
		return ma > mb
	}
	return len(a) < len(b)
}

func minLen(ss []string) int {
	m := -1
	for _, s := range ss {
		if m < 0 || len(s) < m {
			m = len(s)
		}
	}
	return m
}
