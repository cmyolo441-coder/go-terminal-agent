package supplychain

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
)

// LicenseExpr is a node of a parsed SPDX license expression (SPDX 2.3
// Annex D). Implementations are *LicenseTerm, *LicenseAnd and *LicenseOr.
type LicenseExpr interface {
	String() string
	licenseExpr()
}

// LicenseTerm is a simple expression: a license identifier or reference,
// optionally with "+" and a WITH exception.
type LicenseTerm struct {
	// ID is the license id ("MIT") or "LicenseRef-..." reference.
	ID string
	// DocumentRef is set for "DocumentRef-x:LicenseRef-y" references.
	DocumentRef string
	Plus        bool
	Exception   string
}

// LicenseAnd is a conjunction.
type LicenseAnd struct{ Left, Right LicenseExpr }

// LicenseOr is a disjunction.
type LicenseOr struct{ Left, Right LicenseExpr }

func (*LicenseTerm) licenseExpr() {}
func (*LicenseAnd) licenseExpr()  {}
func (*LicenseOr) licenseExpr()   {}

func (t *LicenseTerm) String() string {
	s := t.ID
	if t.DocumentRef != "" {
		s = t.DocumentRef + ":" + s
	}
	if t.Plus {
		s += "+"
	}
	if t.Exception != "" {
		s += " WITH " + t.Exception
	}
	return s
}

func (a *LicenseAnd) String() string {
	wrap := func(e LicenseExpr) string {
		if _, ok := e.(*LicenseOr); ok {
			return "(" + e.String() + ")"
		}
		return e.String()
	}
	return wrap(a.Left) + " AND " + wrap(a.Right)
}

func (o *LicenseOr) String() string { return o.Left.String() + " OR " + o.Right.String() }

type licTokKind int

const (
	ltWord licTokKind = iota
	ltLParen
	ltRParen
	ltAnd
	ltOr
	ltWith
	ltEOF
)

type licTok struct {
	kind licTokKind
	text string
	pos  int
}

func tokenizeLicense(s string) ([]licTok, error) {
	var toks []licTok
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '(':
			toks = append(toks, licTok{ltLParen, "(", i})
			i++
		case c == ')':
			toks = append(toks, licTok{ltRParen, ")", i})
			i++
		case isLicenseChar(c) || c == ':' || c == '+':
			start := i
			for i < len(s) && (isLicenseChar(s[i]) || s[i] == ':' || s[i] == '+') {
				i++
			}
			w := s[start:i]
			switch w {
			case "AND", "and":
				toks = append(toks, licTok{ltAnd, w, start})
			case "OR", "or":
				toks = append(toks, licTok{ltOr, w, start})
			case "WITH", "with":
				toks = append(toks, licTok{ltWith, w, start})
			default:
				toks = append(toks, licTok{ltWord, w, start})
			}
		default:
			return nil, fmt.Errorf("spdx: unexpected character %q at offset %d", c, i)
		}
	}
	toks = append(toks, licTok{ltEOF, "", len(s)})
	return toks, nil
}

func isLicenseChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-'
}

type licParser struct {
	toks  []licTok
	pos   int
	depth int
}

const maxLicenseDepth = 64

func (p *licParser) peek() licTok { return p.toks[p.pos] }
func (p *licParser) next() licTok {
	t := p.toks[p.pos]
	if t.kind != ltEOF {
		p.pos++
	}
	return t
}

// ParseLicenseExpression parses an SPDX license expression. Operator
// precedence is WITH > AND > OR, with parentheses for grouping. Known
// license and exception identifiers are canonicalised to their official
// case; unknown identifiers are accepted (see UnknownLicenseIDs).
func ParseLicenseExpression(s string) (LicenseExpr, error) {
	if strings.TrimSpace(s) == "" {
		return nil, errors.New("spdx: empty license expression")
	}
	if len(s) > 4096 {
		return nil, errors.New("spdx: license expression too long")
	}
	toks, err := tokenizeLicense(s)
	if err != nil {
		return nil, err
	}
	p := &licParser{toks: toks}
	e, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t.kind != ltEOF {
		return nil, fmt.Errorf("spdx: unexpected %q at offset %d", t.text, t.pos)
	}
	return e, nil
}

func (p *licParser) parseOr() (LicenseExpr, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.peek().kind == ltOr {
		p.next()
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &LicenseOr{left, right}
	}
	return left, nil
}

func (p *licParser) parseAnd() (LicenseExpr, error) {
	left, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for p.peek().kind == ltAnd {
		p.next()
		right, err := p.parsePrimary()
		if err != nil {
			return nil, err
		}
		left = &LicenseAnd{left, right}
	}
	return left, nil
}

func (p *licParser) parsePrimary() (LicenseExpr, error) {
	t := p.next()
	switch t.kind {
	case ltLParen:
		p.depth++
		if p.depth > maxLicenseDepth {
			return nil, errors.New("spdx: expression nested too deeply")
		}
		e, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if c := p.next(); c.kind != ltRParen {
			return nil, fmt.Errorf("spdx: expected ')' at offset %d", c.pos)
		}
		p.depth--
		return e, nil
	case ltWord:
		term, err := parseLicenseTerm(t)
		if err != nil {
			return nil, err
		}
		if p.peek().kind == ltWith {
			p.next()
			ex := p.next()
			if ex.kind != ltWord || strings.ContainsAny(ex.text, ":+") || !validIDString(ex.text) {
				return nil, fmt.Errorf("spdx: expected exception identifier after WITH at offset %d", ex.pos)
			}
			term.Exception = canonicalException(ex.text)
		}
		return term, nil
	case ltEOF:
		return nil, errors.New("spdx: unexpected end of expression")
	}
	return nil, fmt.Errorf("spdx: unexpected %q at offset %d", t.text, t.pos)
}

func validIDString(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isLicenseChar(s[i]) {
			return false
		}
	}
	return true
}

func parseLicenseTerm(t licTok) (*LicenseTerm, error) {
	w := t.text
	term := &LicenseTerm{}
	if strings.HasSuffix(w, "+") {
		term.Plus = true
		w = w[:len(w)-1]
	}
	if strings.Contains(w, "+") {
		return nil, fmt.Errorf("spdx: misplaced '+' in %q at offset %d", t.text, t.pos)
	}
	if doc, ref, ok := strings.Cut(w, ":"); ok {
		if !strings.HasPrefix(doc, "DocumentRef-") || !validIDString(doc) || len(doc) == len("DocumentRef-") {
			return nil, fmt.Errorf("spdx: invalid document reference %q", doc)
		}
		if !strings.HasPrefix(ref, "LicenseRef-") || !validIDString(ref) || len(ref) == len("LicenseRef-") {
			return nil, fmt.Errorf("spdx: invalid license reference %q", ref)
		}
		if term.Plus {
			return nil, errors.New("spdx: '+' not allowed on a license reference")
		}
		term.DocumentRef, term.ID = doc, ref
		return term, nil
	}
	if !validIDString(w) {
		return nil, fmt.Errorf("spdx: invalid license identifier %q at offset %d", t.text, t.pos)
	}
	if strings.HasPrefix(w, "LicenseRef-") {
		if len(w) == len("LicenseRef-") || term.Plus {
			return nil, fmt.Errorf("spdx: invalid license reference %q", t.text)
		}
		term.ID = w
		return term, nil
	}
	term.ID = canonicalLicense(w)
	return term, nil
}

// LicenseTerms returns every simple term in the expression, left to right.
func LicenseTerms(e LicenseExpr) []*LicenseTerm {
	var out []*LicenseTerm
	var walk func(LicenseExpr)
	walk = func(e LicenseExpr) {
		switch n := e.(type) {
		case *LicenseTerm:
			out = append(out, n)
		case *LicenseAnd:
			walk(n.Left)
			walk(n.Right)
		case *LicenseOr:
			walk(n.Left)
			walk(n.Right)
		}
	}
	walk(e)
	return out
}

// UnknownLicenseIDs returns identifiers in e that are not on the embedded
// list of SPDX license and exception identifiers (references excluded).
func UnknownLicenseIDs(e LicenseExpr) []string {
	seen := map[string]bool{}
	for _, t := range LicenseTerms(e) {
		if !strings.HasPrefix(t.ID, "LicenseRef-") {
			if _, ok := spdxLicenseIndex[strings.ToLower(t.ID)]; !ok {
				seen[t.ID] = true
			}
		}
		if t.Exception != "" {
			if _, ok := spdxExceptionIndex[strings.ToLower(t.Exception)]; !ok {
				seen[t.Exception] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// licenseFamily strips version-range suffixes so that deny entries such as
// "GPL-3.0" match "GPL-3.0-only", "GPL-3.0-or-later" and "GPL-3.0+".
func licenseFamily(id string) string {
	id = strings.ToLower(strings.TrimSuffix(id, "+"))
	for _, suf := range []string{"-only", "-or-later"} {
		id = strings.TrimSuffix(id, suf)
	}
	return id
}

// LicenseDenied reports whether id (a term's license id) matches any deny
// pattern. Patterns are matched case-insensitively against the license
// family and may use shell globs ("AGPL-*").
func LicenseDenied(id string, deny []string) bool {
	fam := licenseFamily(id)
	for _, d := range deny {
		pat := licenseFamily(strings.TrimSpace(d))
		if pat == "" {
			continue
		}
		if pat == fam {
			return true
		}
		if ok, err := path.Match(pat, fam); err == nil && ok {
			return true
		}
	}
	return false
}

// LicenseSatisfiable reports whether the expression can be satisfied
// without any denied license: an OR needs one acceptable branch, an AND
// needs both. When it cannot, the offending identifiers are returned.
func LicenseSatisfiable(e LicenseExpr, deny []string) (bool, []string) {
	var eval func(LicenseExpr) (bool, []string)
	eval = func(e LicenseExpr) (bool, []string) {
		switch n := e.(type) {
		case *LicenseTerm:
			if LicenseDenied(n.ID, deny) {
				return false, []string{n.String()}
			}
			return true, nil
		case *LicenseAnd:
			okL, badL := eval(n.Left)
			okR, badR := eval(n.Right)
			return okL && okR, append(badL, badR...)
		case *LicenseOr:
			okL, badL := eval(n.Left)
			okR, badR := eval(n.Right)
			if okL || okR {
				return true, nil
			}
			return false, append(badL, badR...)
		}
		return true, nil
	}
	ok, bad := eval(e)
	if ok {
		return true, nil
	}
	return false, bad
}

// NormalizeLicense converts common free-text license names found in
// package metadata to SPDX identifiers; other input is returned trimmed.
func NormalizeLicense(s string) string {
	s = strings.TrimSpace(s)
	if alias, ok := licenseAliases[strings.ToLower(s)]; ok {
		return alias
	}
	return s
}

var licenseAliases = map[string]string{
	"apache 2.0": "Apache-2.0", "apache2": "Apache-2.0", "apache-2": "Apache-2.0",
	"apache license 2.0": "Apache-2.0", "apache license, version 2.0": "Apache-2.0",
	"apache software license": "Apache-2.0", "mit license": "MIT", "the mit license": "MIT",
	"bsd license": "BSD-3-Clause", "new bsd license": "BSD-3-Clause", "simplified bsd": "BSD-2-Clause",
	"isc license": "ISC", "mozilla public license 2.0": "MPL-2.0", "mpl 2.0": "MPL-2.0",
	"gplv2": "GPL-2.0-only", "gplv3": "GPL-3.0-only", "lgplv3": "LGPL-3.0-only",
	"public domain": "LicenseRef-Public-Domain", "unlicense": "Unlicense",
}

func canonicalLicense(id string) string {
	if c, ok := spdxLicenseIndex[strings.ToLower(id)]; ok {
		return c
	}
	return id
}

func canonicalException(id string) string {
	if c, ok := spdxExceptionIndex[strings.ToLower(id)]; ok {
		return c
	}
	return id
}

// A representative subset of the SPDX license list (3.x), including
// deprecated identifiers that still appear in package metadata.
var spdxLicenseIDs = []string{
	"0BSD", "AFL-3.0", "AGPL-1.0", "AGPL-3.0", "AGPL-3.0-only", "AGPL-3.0-or-later", "Apache-1.0",
	"Apache-1.1", "Apache-2.0", "APSL-2.0", "Artistic-1.0", "Artistic-2.0", "BlueOak-1.0.0",
	"BSD-1-Clause", "BSD-2-Clause", "BSD-2-Clause-Patent", "BSD-3-Clause", "BSD-3-Clause-Clear",
	"BSD-4-Clause", "BSL-1.0", "BUSL-1.1", "CC-BY-3.0", "CC-BY-4.0", "CC-BY-NC-4.0", "CC-BY-SA-3.0",
	"CC-BY-SA-4.0", "CC0-1.0", "CDDL-1.0", "CDDL-1.1", "CECILL-2.1", "CPL-1.0", "ECL-2.0", "EPL-1.0",
	"EPL-2.0", "EUPL-1.1", "EUPL-1.2", "GFDL-1.3-only", "GFDL-1.3-or-later", "GPL-1.0", "GPL-1.0+",
	"GPL-1.0-only", "GPL-1.0-or-later", "GPL-2.0", "GPL-2.0+", "GPL-2.0-only", "GPL-2.0-or-later",
	"GPL-3.0", "GPL-3.0+", "GPL-3.0-only", "GPL-3.0-or-later", "Hippocratic-2.1", "ICU", "IJG",
	"ISC", "JSON", "LGPL-2.0", "LGPL-2.0+", "LGPL-2.0-only", "LGPL-2.0-or-later", "LGPL-2.1",
	"LGPL-2.1+", "LGPL-2.1-only", "LGPL-2.1-or-later", "LGPL-3.0", "LGPL-3.0+", "LGPL-3.0-only",
	"LGPL-3.0-or-later", "libpng-2.0", "MIT", "MIT-0", "MIT-CMU", "MPL-1.0", "MPL-1.1", "MPL-2.0",
	"MPL-2.0-no-copyleft-exception", "MS-PL", "MS-RL", "MulanPSL-2.0", "NCSA", "ODbL-1.0", "OFL-1.1",
	"OpenSSL", "OSL-3.0", "PHP-3.0", "PHP-3.01", "PostgreSQL", "PSF-2.0", "Python-2.0", "Ruby",
	"SSPL-1.0", "Unicode-3.0", "Unicode-DFS-2016", "Unlicense", "UPL-1.0", "Vim", "W3C", "WTFPL",
	"X11", "Zlib", "ZPL-2.1",
}

var spdxExceptionIDs = []string{
	"389-exception", "Autoconf-exception-3.0", "Bison-exception-2.2", "Classpath-exception-2.0",
	"eCos-exception-2.0", "Font-exception-2.0", "freertos-exception-2.0", "GCC-exception-2.0",
	"GCC-exception-3.1", "LLVM-exception", "Linux-syscall-note", "OpenJDK-assembly-exception-1.0",
	"Qt-GPL-exception-1.0", "Qt-LGPL-exception-1.1", "Swift-exception", "u-boot-exception-2.0",
	"WxWindows-exception-3.1",
}

var spdxLicenseIndex = indexLower(spdxLicenseIDs)
var spdxExceptionIndex = indexLower(spdxExceptionIDs)

func indexLower(ids []string) map[string]string {
	m := make(map[string]string, len(ids))
	for _, id := range ids {
		m[strings.ToLower(id)] = id
	}
	return m
}
