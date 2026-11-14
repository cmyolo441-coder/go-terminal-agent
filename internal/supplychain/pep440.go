package supplychain

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// PEP440Version is a parsed Python package version (PEP 440).
type PEP440Version struct {
	Epoch   uint64
	Release []uint64
	// PreKind is "a", "b" or "rc" ("" when there is no pre-release).
	PreKind string
	PreNum  uint64
	HasPost bool
	Post    uint64
	HasDev  bool
	Dev     uint64
	Local   []string
}

// The canonical VERSION_PATTERN from PEP 440 Appendix B.
var pep440Re = regexp.MustCompile(`(?i)^\s*v?` +
	`(?:(?P<epoch>[0-9]+)!)?` +
	`(?P<release>[0-9]+(?:\.[0-9]+)*)` +
	`(?P<pre>[-_.]?(?P<pre_l>alpha|beta|preview|pre|rc|a|b|c)[-_.]?(?P<pre_n>[0-9]+)?)?` +
	`(?P<post>(?:-(?P<post_n1>[0-9]+))|(?:[-_.]?(?P<post_l>post|rev|r)[-_.]?(?P<post_n2>[0-9]+)?))?` +
	`(?P<dev>[-_.]?(?P<dev_l>dev)[-_.]?(?P<dev_n>[0-9]+)?)?` +
	`(?:\+(?P<local>[a-z0-9]+(?:[-_.][a-z0-9]+)*))?\s*$`)

var pep440Idx = func() map[string]int {
	m := map[string]int{}
	for i, n := range pep440Re.SubexpNames() {
		if n != "" {
			m[n] = i
		}
	}
	return m
}()

// ParsePEP440 parses a PEP 440 version, accepting all permitted spellings
// and normalising them.
func ParsePEP440(s string) (PEP440Version, error) {
	var v PEP440Version
	m := pep440Re.FindStringSubmatch(s)
	if m == nil {
		return v, fmt.Errorf("pep440: invalid version %q", s)
	}
	g := func(n string) string { return m[pep440Idx[n]] }
	num := func(x string) (uint64, error) {
		if x == "" {
			return 0, nil
		}
		n, err := strconv.ParseUint(x, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("pep440: number %q out of range in %q", x, s)
		}
		return n, nil
	}
	var err error
	if v.Epoch, err = num(g("epoch")); err != nil {
		return v, err
	}
	for _, r := range strings.Split(g("release"), ".") {
		n, err := num(r)
		if err != nil {
			return v, err
		}
		v.Release = append(v.Release, n)
	}
	if l := strings.ToLower(g("pre_l")); l != "" {
		switch l {
		case "a", "alpha":
			v.PreKind = "a"
		case "b", "beta":
			v.PreKind = "b"
		default: // c, rc, pre, preview
			v.PreKind = "rc"
		}
		if v.PreNum, err = num(g("pre_n")); err != nil {
			return v, err
		}
	}
	if g("post") != "" {
		v.HasPost = true
		n := g("post_n1")
		if n == "" {
			n = g("post_n2")
		}
		if v.Post, err = num(n); err != nil {
			return v, err
		}
	}
	if g("dev") != "" {
		v.HasDev = true
		if v.Dev, err = num(g("dev_n")); err != nil {
			return v, err
		}
	}
	if l := g("local"); l != "" {
		v.Local = strings.FieldsFunc(strings.ToLower(l), func(r rune) bool { return r == '-' || r == '_' || r == '.' })
	}
	return v, nil
}

// String renders the normalised form.
func (v PEP440Version) String() string {
	var b strings.Builder
	if v.Epoch != 0 {
		fmt.Fprintf(&b, "%d!", v.Epoch)
	}
	b.WriteString(v.releaseString())
	if v.PreKind != "" {
		fmt.Fprintf(&b, "%s%d", v.PreKind, v.PreNum)
	}
	if v.HasPost {
		fmt.Fprintf(&b, ".post%d", v.Post)
	}
	if v.HasDev {
		fmt.Fprintf(&b, ".dev%d", v.Dev)
	}
	if len(v.Local) > 0 {
		b.WriteByte('+')
		b.WriteString(strings.Join(v.Local, "."))
	}
	return b.String()
}

func (v PEP440Version) releaseString() string {
	parts := make([]string, len(v.Release))
	for i, r := range v.Release {
		parts[i] = strconv.FormatUint(r, 10)
	}
	return strings.Join(parts, ".")
}

// IsPrerelease reports whether the version is a pre- or dev-release.
func (v PEP440Version) IsPrerelease() bool { return v.PreKind != "" || v.HasDev }

// Public returns the version without its local segment.
func (v PEP440Version) Public() PEP440Version {
	v.Local = nil
	return v
}

// base returns epoch and release only.
func (v PEP440Version) base() PEP440Version {
	return PEP440Version{Epoch: v.Epoch, Release: v.Release}
}

// Compare orders versions per PEP 440 (matching the reference
// implementation in "packaging").
func (v PEP440Version) Compare(o PEP440Version) int {
	if c := cmpU(v.Epoch, o.Epoch); c != 0 {
		return c
	}
	n := max(len(v.Release), len(o.Release))
	for i := 0; i < n; i++ {
		var a, b uint64
		if i < len(v.Release) {
			a = v.Release[i]
		}
		if i < len(o.Release) {
			b = o.Release[i]
		}
		if c := cmpU(a, b); c != 0 {
			return c
		}
	}
	if c := cmpInt(v.preRank(), o.preRank()); c != 0 {
		return c
	}
	if v.PreKind != "" && o.PreKind != "" {
		if c := cmpU(v.PreNum, o.PreNum); c != 0 {
			return c
		}
	}
	// post: absent sorts before any post-release
	if c := cmpBoolNum(v.HasPost, v.Post, o.HasPost, o.Post, false); c != 0 {
		return c
	}
	// dev: absent sorts after any dev-release
	if c := cmpBoolNum(v.HasDev, v.Dev, o.HasDev, o.Dev, true); c != 0 {
		return c
	}
	return compareLocal(v.Local, o.Local)
}

// preRank: dev-only release (no pre, no post, dev) < a < b < rc < final.
func (v PEP440Version) preRank() int {
	switch {
	case v.PreKind == "" && !v.HasPost && v.HasDev:
		return -1
	case v.PreKind == "a":
		return 0
	case v.PreKind == "b":
		return 1
	case v.PreKind == "rc":
		return 2
	}
	return 3
}

func cmpBoolNum(ha bool, a uint64, hb bool, b uint64, absentHigh bool) int {
	switch {
	case ha && hb:
		return cmpU(a, b)
	case !ha && !hb:
		return 0
	case !ha:
		if absentHigh {
			return 1
		}
		return -1
	default:
		if absentHigh {
			return -1
		}
		return 1
	}
}

func compareLocal(a, b []string) int {
	switch {
	case len(a) == 0 && len(b) == 0:
		return 0
	case len(a) == 0:
		return -1
	case len(b) == 0:
		return 1
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		an, aNum := localNum(a[i])
		bn, bNum := localNum(b[i])
		switch {
		case aNum && bNum:
			if c := cmpU(an, bn); c != 0 {
				return c
			}
		case aNum:
			return 1 // numeric segments sort after alphanumeric
		case bNum:
			return -1
		default:
			if c := strings.Compare(a[i], b[i]); c != 0 {
				return c
			}
		}
	}
	return cmpInt(len(a), len(b))
}

func localNum(s string) (uint64, bool) {
	if !isDigits(s) {
		return 0, false
	}
	n, err := strconv.ParseUint(s, 10, 64)
	return n, err == nil
}

// PEP440Specifier is one clause of a specifier set, e.g. ">=1.2,!=1.3.*".
type PEP440Specifier struct {
	Op       string
	Version  string
	parsed   PEP440Version
	wildcard bool
}

// PEP440SpecifierSet is a comma-separated conjunction of specifiers.
type PEP440SpecifierSet []PEP440Specifier

// ParsePEP440Specifiers parses a PEP 440 specifier set.
func ParsePEP440Specifiers(s string) (PEP440SpecifierSet, error) {
	var set PEP440SpecifierSet
	s = strings.TrimSpace(s)
	if s == "" {
		return set, nil
	}
	for _, clause := range strings.Split(s, ",") {
		clause = strings.TrimSpace(clause)
		var op string
		for _, o := range []string{"===", "~=", "==", "!=", "<=", ">=", "<", ">"} {
			if strings.HasPrefix(clause, o) {
				op = o
				break
			}
		}
		if op == "" {
			return nil, fmt.Errorf("pep440: specifier %q lacks an operator", clause)
		}
		ver := strings.TrimSpace(clause[len(op):])
		if ver == "" {
			return nil, fmt.Errorf("pep440: specifier %q lacks a version", clause)
		}
		sp := PEP440Specifier{Op: op, Version: ver}
		if op == "===" {
			set = append(set, sp)
			continue
		}
		if strings.HasSuffix(ver, ".*") {
			if op != "==" && op != "!=" {
				return nil, fmt.Errorf("pep440: wildcard not allowed with %s in %q", op, clause)
			}
			sp.wildcard = true
			ver = strings.TrimSuffix(ver, ".*")
		}
		v, err := ParsePEP440(ver)
		if err != nil {
			return nil, err
		}
		if sp.wildcard && (v.PreKind != "" || v.HasPost || v.HasDev || len(v.Local) > 0) {
			return nil, fmt.Errorf("pep440: wildcard prefix must be a release in %q", clause)
		}
		if len(v.Local) > 0 && op != "==" && op != "!=" {
			return nil, fmt.Errorf("pep440: local version not allowed with %s in %q", op, clause)
		}
		if op == "~=" && len(v.Release) < 2 {
			return nil, fmt.Errorf("pep440: ~= needs at least two release segments in %q", clause)
		}
		sp.parsed = v
		set = append(set, sp)
	}
	return set, nil
}

// Contains reports whether v satisfies every specifier. Pre-releases are
// excluded unless allowPre is true or a specifier names a pre-release.
func (s PEP440SpecifierSet) Contains(v PEP440Version, allowPre bool) bool {
	if v.IsPrerelease() && !allowPre {
		explicit := false
		for _, sp := range s {
			if sp.Op != "===" && sp.parsed.IsPrerelease() {
				explicit = true
			}
		}
		if !explicit {
			return false
		}
	}
	for _, sp := range s {
		if !sp.contains(v) {
			return false
		}
	}
	return true
}

func (sp PEP440Specifier) contains(v PEP440Version) bool {
	spec := sp.parsed
	switch sp.Op {
	case "===":
		return strings.EqualFold(strings.TrimSpace(sp.Version), v.String())
	case "==":
		return sp.equal(v)
	case "!=":
		return !sp.equal(v)
	case "~=":
		prefix := PEP440Specifier{wildcard: true, parsed: PEP440Version{Epoch: spec.Epoch, Release: spec.Release[:len(spec.Release)-1]}}
		return v.Compare(spec) >= 0 && prefix.equal(v)
	case "<=":
		return v.Public().Compare(spec) <= 0
	case ">=":
		return v.Public().Compare(spec) >= 0
	case "<":
		if v.Compare(spec) >= 0 {
			return false
		}
		if !spec.IsPrerelease() && v.IsPrerelease() && v.base().Compare(spec.base()) == 0 {
			return false
		}
		return true
	case ">":
		if v.Compare(spec) <= 0 {
			return false
		}
		if !spec.HasPost && v.HasPost && v.base().Compare(spec.base()) == 0 {
			return false
		}
		if len(v.Local) > 0 && v.base().Compare(spec.base()) == 0 {
			return false
		}
		return true
	}
	return false
}

func (sp PEP440Specifier) equal(v PEP440Version) bool {
	spec := sp.parsed
	if sp.wildcard {
		if v.Epoch != spec.Epoch {
			return false
		}
		for i, r := range spec.Release {
			var x uint64
			if i < len(v.Release) {
				x = v.Release[i]
			}
			if x != r {
				return false
			}
		}
		return true
	}
	if len(spec.Local) == 0 {
		v = v.Public()
	}
	return v.Compare(spec) == 0
}
