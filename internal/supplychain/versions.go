package supplychain

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// VersionComparator orders versions of one ecosystem.
type VersionComparator interface {
	// Compare returns -1, 0 or +1. It fails when either version is not
	// valid for the ecosystem.
	Compare(a, b string) (int, error)
}

type comparatorFunc func(a, b string) (int, error)

func (f comparatorFunc) Compare(a, b string) (int, error) { return f(a, b) }

// ComparatorFor returns the version ordering of an ecosystem.
func ComparatorFor(eco Ecosystem) VersionComparator {
	switch eco {
	case EcosystemGo:
		return comparatorFunc(compareGo)
	case EcosystemNPM, EcosystemCargo:
		return comparatorFunc(compareSemVerStrings)
	case EcosystemPyPI:
		return comparatorFunc(comparePEP440Strings)
	case EcosystemRubyGems:
		return comparatorFunc(CompareGemVersions)
	case EcosystemPackagist:
		return comparatorFunc(func(a, b string) (int, error) {
			return CompareComposerVersions(a, b), nil
		})
	}
	return comparatorFunc(compareSemVerStrings)
}

func compareSemVerStrings(a, b string) (int, error) {
	x, err := ParseSemVer(a)
	if err != nil {
		return 0, err
	}
	y, err := ParseSemVer(b)
	if err != nil {
		return 0, err
	}
	return x.Compare(y), nil
}

func compareGo(a, b string) (int, error) {
	x, err := ParseGoVersion(a)
	if err != nil {
		return 0, err
	}
	y, err := ParseGoVersion(b)
	if err != nil {
		return 0, err
	}
	return x.Compare(y.SemVer), nil
}

func comparePEP440Strings(a, b string) (int, error) {
	x, err := ParsePEP440(a)
	if err != nil {
		return 0, err
	}
	y, err := ParsePEP440(b)
	if err != nil {
		return 0, err
	}
	return x.Compare(y), nil
}

// ---- RubyGems ----------------------------------------------------------

var gemVersionRe = regexp.MustCompile(`^\s*[0-9]+(\.[0-9a-zA-Z]+)*(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?\s*$`)
var gemSegRe = regexp.MustCompile(`[0-9]+|[a-zA-Z]+`)

type gemSeg struct {
	n     uint64
	s     string
	isNum bool
}

func gemSegments(v string) ([]gemSeg, error) {
	if !gemVersionRe.MatchString(v) {
		return nil, fmt.Errorf("rubygems: invalid version %q", v)
	}
	v = strings.ReplaceAll(strings.TrimSpace(v), "-", ".pre.")
	var segs []gemSeg
	for _, m := range gemSegRe.FindAllString(v, -1) {
		if isDigits(m) {
			n, err := strconv.ParseUint(m, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("rubygems: segment %q out of range", m)
			}
			segs = append(segs, gemSeg{n: n, isNum: true})
		} else {
			segs = append(segs, gemSeg{s: m})
		}
	}
	// canonical_segments: split at the first string segment and drop
	// trailing zeros from each half.
	split := len(segs)
	for i, s := range segs {
		if !s.isNum {
			split = i
			break
		}
	}
	trim := func(x []gemSeg) []gemSeg {
		for len(x) > 0 && x[len(x)-1].isNum && x[len(x)-1].n == 0 {
			x = x[:len(x)-1]
		}
		return x
	}
	rel := trim(append([]gemSeg(nil), segs[:split]...))
	pre := trim(append([]gemSeg(nil), segs[split:]...))
	return append(rel, pre...), nil
}

// CompareGemVersions orders RubyGems versions like Gem::Version#<=>.
func CompareGemVersions(a, b string) (int, error) {
	x, err := gemSegments(a)
	if err != nil {
		return 0, err
	}
	y, err := gemSegments(b)
	if err != nil {
		return 0, err
	}
	n := max(len(x), len(y))
	for i := 0; i < n; i++ {
		l, r := gemSeg{isNum: true}, gemSeg{isNum: true}
		if i < len(x) {
			l = x[i]
		}
		if i < len(y) {
			r = y[i]
		}
		switch {
		case l.isNum && r.isNum:
			if c := cmpU(l.n, r.n); c != 0 {
				return c, nil
			}
		case l.isNum:
			return 1, nil
		case r.isNum:
			return -1, nil
		default:
			if c := strings.Compare(l.s, r.s); c != 0 {
				return c, nil
			}
		}
	}
	return 0, nil
}

// ---- Composer / PHP version_compare --------------------------------------

// CompareComposerVersions orders Packagist versions using PHP's
// version_compare algorithm after stripping a leading "v".
func CompareComposerVersions(a, b string) int {
	trim := func(s string) string {
		s = strings.TrimSpace(s)
		if len(s) > 1 && (s[0] == 'v' || s[0] == 'V') && s[1] >= '0' && s[1] <= '9' {
			s = s[1:]
		}
		return s
	}
	return phpVersionCompare(trim(a), trim(b))
}

func phpCanonicalize(v string) string {
	if v == "" {
		return ""
	}
	isDig := func(c byte) bool { return c >= '0' && c <= '9' }
	isNDig := func(c byte) bool { return !isDig(c) && c != '.' }
	isAlnum := func(c byte) bool { return isDig(c) || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
	buf := []byte{v[0]}
	lp := v[0]
	for i := 1; i < len(v); i++ {
		c := v[i]
		lq := buf[len(buf)-1]
		switch {
		case c == '-' || c == '_' || c == '+':
			if lq != '.' {
				buf = append(buf, '.')
			}
		case (isNDig(lp) && isDig(c)) || (isDig(lp) && isNDig(c)):
			if lq != '.' {
				buf = append(buf, '.')
			}
			buf = append(buf, c)
		case !isAlnum(c):
			if lq != '.' {
				buf = append(buf, '.')
			}
		default:
			buf = append(buf, c)
		}
		lp = c
	}
	return string(buf)
}

func phpSpecialForm(s string) int {
	forms := []struct {
		name  string
		order int
	}{{"dev", 0}, {"alpha", 1}, {"a", 1}, {"beta", 2}, {"b", 2}, {"RC", 3}, {"rc", 3}, {"#", 4}, {"pl", 5}, {"p", 5}}
	for _, f := range forms {
		if strings.HasPrefix(s, f.name) {
			return f.order
		}
	}
	return -6
}

func phpCompareSpecial(a, b string) int {
	return cmpInt(phpSpecialForm(a), phpSpecialForm(b))
}

func phpVersionCompare(a, b string) int {
	switch {
	case a == "" && b == "":
		return 0
	case a == "":
		return -1
	case b == "":
		return 1
	}
	ca, cb := phpCanonicalize(a), phpCanonicalize(b)
	if ca == cb {
		return 0
	}
	pa := strings.Split(ca, ".")
	pb := strings.Split(cb, ".")
	isNum := func(s string) bool { return s != "" && s[0] >= '0' && s[0] <= '9' }
	i := 0
	compare := 0
	for ; i < len(pa) && i < len(pb) && pa[i] != "" && pb[i] != "" && compare == 0; i++ {
		x, y := pa[i], pb[i]
		switch {
		case isNum(x) && isNum(y):
			compare = cmpInt64(phpAtoi(x), phpAtoi(y))
		case !isNum(x) && !isNum(y):
			compare = phpCompareSpecial(x, y)
		case isNum(x):
			compare = phpCompareSpecial("#N#", y)
		default:
			compare = phpCompareSpecial(x, "#N#")
		}
	}
	if compare == 0 {
		switch {
		case i < len(pa) && pa[i] == "" && i < len(pb) && pb[i] == "":
			compare = 0
		case i < len(pa) && pa[i] == "":
			compare = -1 // C: php_version_compare("", ...) on an empty remainder
		case i < len(pb) && pb[i] == "":
			compare = 1
		case i < len(pa):
			if isNum(pa[i]) {
				compare = 1
			} else {
				compare = phpVersionCompare(strings.Join(pa[i:], "."), "#N#")
			}
		case i < len(pb):
			if isNum(pb[i]) {
				compare = -1
			} else {
				compare = phpVersionCompare("#N#", strings.Join(pb[i:], "."))
			}
		}
	}
	return compare
}

func phpAtoi(s string) int64 {
	var n int64
	for i := 0; i < len(s) && s[i] >= '0' && s[i] <= '9'; i++ {
		if n > (1<<62)/10 {
			return 1 << 62
		}
		n = n*10 + int64(s[i]-'0')
	}
	return n
}

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
