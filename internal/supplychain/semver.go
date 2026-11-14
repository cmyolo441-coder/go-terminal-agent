package supplychain

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SemVer is a Semantic Versioning 2.0.0 version.
type SemVer struct {
	Major, Minor, Patch uint64
	Pre                 []PreID
	Build               []string
}

// PreID is one dot-separated pre-release identifier.
type PreID struct {
	Num   uint64
	Str   string
	IsNum bool
}

func (p PreID) String() string {
	if p.IsNum {
		return strconv.FormatUint(p.Num, 10)
	}
	return p.Str
}

// ParseSemVer parses a strict SemVer 2.0.0 version. A single leading "v"
// is accepted (Go module versions).
func ParseSemVer(s string) (SemVer, error) {
	var v SemVer
	orig := s
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return v, fmt.Errorf("semver: empty version %q", orig)
	}
	if i := strings.IndexByte(s, '+'); i >= 0 {
		build := s[i+1:]
		s = s[:i]
		if build == "" {
			return v, fmt.Errorf("semver: empty build metadata in %q", orig)
		}
		for _, id := range strings.Split(build, ".") {
			if id == "" || !isIdentChars(id) {
				return v, fmt.Errorf("semver: invalid build identifier %q in %q", id, orig)
			}
			v.Build = append(v.Build, id)
		}
	}
	if i := strings.IndexByte(s, '-'); i >= 0 {
		pre := s[i+1:]
		s = s[:i]
		if pre == "" {
			return v, fmt.Errorf("semver: empty pre-release in %q", orig)
		}
		ids, err := parsePreIDs(pre)
		if err != nil {
			return v, fmt.Errorf("semver: %w in %q", err, orig)
		}
		v.Pre = ids
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return v, fmt.Errorf("semver: %q must have major.minor.patch", orig)
	}
	nums := [3]*uint64{&v.Major, &v.Minor, &v.Patch}
	for i, p := range parts {
		n, err := parseNumericIdent(p)
		if err != nil {
			return v, fmt.Errorf("semver: %w in %q", err, orig)
		}
		*nums[i] = n
	}
	return v, nil
}

func parsePreIDs(pre string) ([]PreID, error) {
	var ids []PreID
	for _, id := range strings.Split(pre, ".") {
		if id == "" || !isIdentChars(id) {
			return nil, fmt.Errorf("invalid pre-release identifier %q", id)
		}
		if isDigits(id) {
			n, err := parseNumericIdent(id)
			if err != nil {
				return nil, err
			}
			ids = append(ids, PreID{Num: n, IsNum: true})
		} else {
			ids = append(ids, PreID{Str: id})
		}
	}
	return ids, nil
}

func parseNumericIdent(p string) (uint64, error) {
	if p == "" || !isDigits(p) {
		return 0, fmt.Errorf("invalid numeric identifier %q", p)
	}
	if len(p) > 1 && p[0] == '0' {
		return 0, fmt.Errorf("numeric identifier %q has a leading zero", p)
	}
	n, err := strconv.ParseUint(p, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("numeric identifier %q overflows", p)
	}
	return n, nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func isIdentChars(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-') {
			return false
		}
	}
	return true
}

// String renders the version without a "v" prefix.
func (v SemVer) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d.%d.%d", v.Major, v.Minor, v.Patch)
	if len(v.Pre) > 0 {
		b.WriteByte('-')
		for i, p := range v.Pre {
			if i > 0 {
				b.WriteByte('.')
			}
			b.WriteString(p.String())
		}
	}
	if len(v.Build) > 0 {
		b.WriteByte('+')
		b.WriteString(strings.Join(v.Build, "."))
	}
	return b.String()
}

// Compare orders versions by SemVer precedence; build metadata is ignored.
func (v SemVer) Compare(o SemVer) int {
	if c := cmpU(v.Major, o.Major); c != 0 {
		return c
	}
	if c := cmpU(v.Minor, o.Minor); c != 0 {
		return c
	}
	if c := cmpU(v.Patch, o.Patch); c != 0 {
		return c
	}
	return comparePre(v.Pre, o.Pre)
}

func comparePre(a, b []PreID) int {
	switch {
	case len(a) == 0 && len(b) == 0:
		return 0
	case len(a) == 0:
		return 1
	case len(b) == 0:
		return -1
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		x, y := a[i], b[i]
		switch {
		case x.IsNum && y.IsNum:
			if c := cmpU(x.Num, y.Num); c != 0 {
				return c
			}
		case x.IsNum:
			return -1
		case y.IsNum:
			return 1
		default:
			if c := strings.Compare(x.Str, y.Str); c != 0 {
				return c
			}
		}
	}
	return cmpInt(len(a), len(b))
}

func cmpU(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (v SemVer) sameTriple(o SemVer) bool {
	return v.Major == o.Major && v.Minor == o.Minor && v.Patch == o.Patch
}

// ---- Go module versions -------------------------------------------------

// GoVersion is a parsed Go module version (canonical "vX.Y.Z[-pre][+incompatible]").
type GoVersion struct {
	SemVer
	Incompatible bool
}

// ParseGoVersion parses a Go module version. The "v" prefix is optional so
// that OSV's Go ranges (written without it) are accepted.
func ParseGoVersion(s string) (GoVersion, error) {
	sv, err := ParseSemVer(s)
	if err != nil {
		return GoVersion{}, err
	}
	gv := GoVersion{SemVer: sv}
	if len(sv.Build) > 0 {
		if len(sv.Build) != 1 || sv.Build[0] != "incompatible" {
			return GoVersion{}, fmt.Errorf("go version %q: only +incompatible build metadata is allowed", s)
		}
		if sv.Major < 2 {
			return GoVersion{}, fmt.Errorf("go version %q: +incompatible requires major >= 2", s)
		}
		gv.Incompatible = true
	}
	return gv, nil
}

// PseudoVersion describes the components of a Go pseudo-version.
type PseudoVersion struct {
	// Base is the release the pseudo-version is derived from ("" for the
	// vX.0.0-yyyymmddhhmmss-abcdef form with no base tag).
	Base     string
	Time     time.Time
	Revision string
}

// ParsePseudoVersion recognises the three pseudo-version forms:
//
//	vX.0.0-yyyymmddhhmmss-abcdefabcdef          (no base)
//	vX.Y.Z-pre.0.yyyymmddhhmmss-abcdefabcdef    (base is a pre-release)
//	vX.Y.(Z+1)-0.yyyymmddhhmmss-abcdefabcdef    (base is a release)
func ParsePseudoVersion(s string) (PseudoVersion, error) {
	gv, err := ParseGoVersion(s)
	if err != nil {
		return PseudoVersion{}, err
	}
	pre := gv.Pre
	if len(pre) == 0 {
		return PseudoVersion{}, fmt.Errorf("%q is not a pseudo-version", s)
	}
	last := pre[len(pre)-1]
	if last.IsNum {
		return PseudoVersion{}, fmt.Errorf("%q is not a pseudo-version", s)
	}
	ts, rev, ok := strings.Cut(last.Str, "-")
	// The timestamp and revision always form one identifier because '-' is
	// an identifier character: "20191109021931-daa7c04131f5".
	if !ok || !isDigits(ts) || len(ts) != 14 || len(rev) != 12 || !isLowerHex(rev) {
		return PseudoVersion{}, fmt.Errorf("%q is not a pseudo-version", s)
	}
	t, err := time.Parse("20060102150405", ts)
	if err != nil {
		return PseudoVersion{}, fmt.Errorf("%q: bad pseudo-version timestamp: %w", s, err)
	}
	pv := PseudoVersion{Time: t.UTC(), Revision: rev}
	prefix := pre[:len(pre)-1]
	switch {
	case len(prefix) == 0:
		// vX.0.0-yyyymmddhhmmss-rev
		if gv.Minor != 0 || gv.Patch != 0 {
			return PseudoVersion{}, fmt.Errorf("%q: base-less pseudo-version must be vX.0.0", s)
		}
	case len(prefix) == 1 && prefix[0].IsNum && prefix[0].Num == 0:
		// vX.Y.(Z+1)-0.ts-rev
		if gv.Patch == 0 {
			return PseudoVersion{}, fmt.Errorf("%q: release-based pseudo-version needs patch > 0", s)
		}
		pv.Base = fmt.Sprintf("v%d.%d.%d", gv.Major, gv.Minor, gv.Patch-1)
	case len(prefix) >= 2 && prefix[len(prefix)-1].IsNum && prefix[len(prefix)-1].Num == 0:
		base := SemVer{Major: gv.Major, Minor: gv.Minor, Patch: gv.Patch, Pre: prefix[:len(prefix)-1]}
		pv.Base = "v" + base.String()
	default:
		return PseudoVersion{}, fmt.Errorf("%q is not a pseudo-version", s)
	}
	return pv, nil
}

// IsPseudoVersion reports whether s is a Go pseudo-version.
func IsPseudoVersion(s string) bool {
	_, err := ParsePseudoVersion(s)
	return err == nil
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// ---- Ranges (npm and Cargo) ---------------------------------------------

type primOp uint8

const (
	opEQ primOp = iota
	opGT
	opGE
	opLT
	opLE
)

type primCmp struct {
	op primOp
	v  SemVer
}

func (c primCmp) match(v SemVer) bool {
	r := v.Compare(c.v)
	switch c.op {
	case opEQ:
		return r == 0
	case opGT:
		return r > 0
	case opGE:
		return r >= 0
	case opLT:
		return r < 0
	case opLE:
		return r <= 0
	}
	return false
}

// cmpSet is a conjunction of primitive comparators.
type cmpSet []primCmp

// match implements the shared pre-release rule of npm and Cargo: a
// pre-release version satisfies the set only if some comparator names a
// pre-release of the same major.minor.patch.
func (s cmpSet) match(v SemVer) bool {
	for _, c := range s {
		if !c.match(v) {
			return false
		}
	}
	if len(v.Pre) == 0 {
		return true
	}
	for _, c := range s {
		if len(c.v.Pre) > 0 && c.v.sameTriple(v) {
			return true
		}
	}
	return false
}

type partialVer struct {
	major, minor, patch uint64
	hasMinor, hasPatch  bool
	any                 bool // "*" / "x"
	pre                 []PreID
}

func (p partialVer) full() SemVer {
	return SemVer{Major: p.major, Minor: p.minor, Patch: p.patch, Pre: p.pre}
}

func parsePartial(s string) (partialVer, error) {
	var p partialVer
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "="), "v")
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i] // build metadata is ignored in ranges
	}
	core, pre, hasPre := strings.Cut(s, "-")
	if s == "" {
		return p, errors.New("empty version")
	}
	parts := strings.Split(core, ".")
	if len(parts) > 3 {
		return p, fmt.Errorf("too many version components in %q", s)
	}
	isWild := func(x string) bool { return x == "*" || x == "x" || x == "X" }
	for i, part := range parts {
		if isWild(part) {
			if i == 0 {
				p.any = true
			}
			for _, rest := range parts[i+1:] {
				if !isWild(rest) {
					return p, fmt.Errorf("non-wildcard after wildcard in %q", s)
				}
			}
			break
		}
		n, err := parseNumericIdent(part)
		if err != nil {
			return p, err
		}
		switch i {
		case 0:
			p.major = n
		case 1:
			p.minor, p.hasMinor = n, true
		case 2:
			p.patch, p.hasPatch = n, true
		}
	}
	if hasPre {
		if p.any || !p.hasPatch {
			return p, fmt.Errorf("pre-release on partial version %q", s)
		}
		ids, err := parsePreIDs(pre)
		if err != nil {
			return p, err
		}
		p.pre = ids
	}
	return p, nil
}

// desugar expands an operator applied to a partial version into primitive
// comparators. The semantics are shared by node-semver and the Rust semver
// crate; the only dialect difference (bare versions) is resolved by the
// caller choosing op.
func desugar(op string, p partialVer) ([]primCmp, error) {
	zero := SemVer{}
	if p.any {
		switch op {
		case "<", ">":
			// "<*" and ">*" match nothing.
			return []primCmp{{opLT, zero}}, nil
		}
		return []primCmp{{opGE, zero}}, nil
	}
	M, m, pt := p.major, p.minor, p.patch
	lo := SemVer{Major: M, Minor: m, Patch: pt, Pre: p.pre}
	nextMajor := SemVer{Major: M + 1}
	nextMinor := SemVer{Major: M, Minor: m + 1}
	switch op {
	case "=", "":
		switch {
		case !p.hasMinor:
			return []primCmp{{opGE, lo}, {opLT, nextMajor}}, nil
		case !p.hasPatch:
			return []primCmp{{opGE, lo}, {opLT, nextMinor}}, nil
		}
		return []primCmp{{opEQ, lo}}, nil
	case "^":
		switch {
		case !p.hasMinor:
			return []primCmp{{opGE, lo}, {opLT, nextMajor}}, nil
		case M > 0:
			return []primCmp{{opGE, lo}, {opLT, nextMajor}}, nil
		case !p.hasPatch || m > 0:
			return []primCmp{{opGE, lo}, {opLT, nextMinor}}, nil
		}
		return []primCmp{{opGE, lo}, {opLT, SemVer{Major: 0, Minor: 0, Patch: pt + 1}}}, nil
	case "~", "~>":
		if !p.hasMinor {
			return []primCmp{{opGE, lo}, {opLT, nextMajor}}, nil
		}
		return []primCmp{{opGE, lo}, {opLT, nextMinor}}, nil
	case ">":
		switch {
		case !p.hasMinor:
			return []primCmp{{opGE, nextMajor}}, nil
		case !p.hasPatch:
			return []primCmp{{opGE, nextMinor}}, nil
		}
		return []primCmp{{opGT, lo}}, nil
	case ">=":
		return []primCmp{{opGE, lo}}, nil
	case "<":
		return []primCmp{{opLT, lo}}, nil
	case "<=":
		switch {
		case !p.hasMinor:
			return []primCmp{{opLT, nextMajor}}, nil
		case !p.hasPatch:
			return []primCmp{{opLT, nextMinor}}, nil
		}
		return []primCmp{{opLE, lo}}, nil
	}
	return nil, fmt.Errorf("unknown operator %q", op)
}

func splitOp(s string) (op, rest string) {
	for _, o := range []string{">=", "<=", "~>", "^", "~", ">", "<", "="} {
		if strings.HasPrefix(s, o) {
			return o, strings.TrimSpace(s[len(o):])
		}
	}
	return "", s
}

// NPMRange is a parsed node-semver range ("||"-separated comparator sets).
type NPMRange struct {
	raw  string
	sets []cmpSet
}

// ParseNPMRange parses a node-semver range: "||" alternatives, hyphen
// ranges, x-ranges, tilde and caret ranges and primitive comparators.
func ParseNPMRange(s string) (NPMRange, error) {
	r := NPMRange{raw: s}
	for _, alt := range strings.Split(s, "||") {
		alt = strings.TrimSpace(alt)
		var set cmpSet
		if alt == "" {
			set = cmpSet{{opGE, SemVer{}}}
			r.sets = append(r.sets, set)
			continue
		}
		fields := strings.Fields(alt)
		// hyphen range "A - B"
		if len(fields) == 3 && fields[1] == "-" {
			a, err := parsePartial(fields[0])
			if err != nil {
				return r, fmt.Errorf("npm range %q: %w", s, err)
			}
			b, err := parsePartial(fields[2])
			if err != nil {
				return r, fmt.Errorf("npm range %q: %w", s, err)
			}
			lo, _ := desugar(">=", a)
			hi, _ := desugar("<=", b)
			set = append(append(set, lo...), hi...)
			r.sets = append(r.sets, set)
			continue
		}
		// re-join operators separated from their version by spaces (">= 1.2").
		var toks []string
		for i := 0; i < len(fields); i++ {
			f := fields[i]
			if op, rest := splitOp(f); op != "" && rest == "" && i+1 < len(fields) {
				f = op + fields[i+1]
				i++
			}
			toks = append(toks, f)
		}
		for _, t := range toks {
			op, rest := splitOp(t)
			p, err := parsePartial(rest)
			if err != nil {
				return r, fmt.Errorf("npm range %q: %w", s, err)
			}
			prims, err := desugar(op, p)
			if err != nil {
				return r, fmt.Errorf("npm range %q: %w", s, err)
			}
			set = append(set, prims...)
		}
		r.sets = append(r.sets, set)
	}
	return r, nil
}

// Match reports whether v satisfies the range.
func (r NPMRange) Match(v SemVer) bool {
	for _, s := range r.sets {
		if s.match(v) {
			return true
		}
	}
	return false
}

func (r NPMRange) String() string { return r.raw }

// MatchNPMRange parses rng and version and reports whether they match.
func MatchNPMRange(rng, version string) (bool, error) {
	r, err := ParseNPMRange(rng)
	if err != nil {
		return false, err
	}
	v, err := ParseSemVer(version)
	if err != nil {
		return false, err
	}
	return r.Match(v), nil
}

// CargoReq is a parsed Cargo version requirement (comma-separated
// comparators; a bare version means caret).
type CargoReq struct {
	raw string
	set cmpSet
}

// ParseCargoReq parses a Cargo/Rust semver VersionReq.
func ParseCargoReq(s string) (CargoReq, error) {
	r := CargoReq{raw: s}
	if strings.TrimSpace(s) == "" {
		return r, errors.New("cargo req: empty requirement")
	}
	for _, c := range strings.Split(s, ",") {
		c = strings.TrimSpace(c)
		if c == "" {
			return r, fmt.Errorf("cargo req %q: empty comparator", s)
		}
		op, rest := splitOp(c)
		if op == "~>" {
			return r, fmt.Errorf("cargo req %q: '~>' is not valid in Cargo", s)
		}
		p, err := parsePartial(rest)
		if err != nil {
			return r, fmt.Errorf("cargo req %q: %w", s, err)
		}
		if op == "" {
			op = "^"
			if strings.ContainsAny(rest, "*xX") {
				op = "=" // wildcard requirements such as "1.2.*"
			}
		}
		prims, err := desugar(op, p)
		if err != nil {
			return r, fmt.Errorf("cargo req %q: %w", s, err)
		}
		r.set = append(r.set, prims...)
	}
	return r, nil
}

// Match reports whether v satisfies the requirement.
func (r CargoReq) Match(v SemVer) bool { return r.set.match(v) }

func (r CargoReq) String() string { return r.raw }

// MatchCargoReq parses req and version and reports whether they match.
func MatchCargoReq(req, version string) (bool, error) {
	r, err := ParseCargoReq(req)
	if err != nil {
		return false, err
	}
	v, err := ParseSemVer(version)
	if err != nil {
		return false, err
	}
	return r.Match(v), nil
}
