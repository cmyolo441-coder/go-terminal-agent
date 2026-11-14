package dlp

import (
	"net/netip"
	"regexp"
	"sort"
	"strings"
)

// cand is a detected value before policy actions are applied.
type cand struct {
	typ        int
	start, end int
	conf       float64
	detail     string
	ctx        bool
}

// kwHit is a context keyword occurrence.
type kwHit struct {
	start, end int
	mask, neg  uint32
}

type anchorHit struct {
	regex int
	end   int
}

type scanner struct {
	e       *Engine
	text    string
	from    int
	hits    []kwHit
	anchors []anchorHit
	cands   []cand
	dbuf    [48]byte
	enabled [numBuiltins]bool
	numOn   bool
}

// Character classes for the main pass.
const (
	cDigit = 1 << iota
	cUpper
	cLower
	cUnder
	cAlnum = cDigit | cUpper | cLower
	cWord  = cAlnum | cUnder
)

var ctab = func() (t [256]uint8) {
	for c := '0'; c <= '9'; c++ {
		t[c] = cDigit
	}
	for c := 'A'; c <= 'Z'; c++ {
		t[c] = cUpper
		t[c+32] = cLower
	}
	t['_'] = cUnder
	return
}()

const ctxBoost = 0.75

// detect runs every enabled detector over text and returns the accepted
// candidates, sorted by start, that begin at or after from. Bytes before
// from are context only (the stream carry-over window).
func (e *Engine) detect(text string, from int) []cand {
	sc := &scanner{e: e, text: text, from: from}
	for i := range sc.enabled {
		sc.enabled[i] = e.types[i].enabled
	}
	sc.numOn = sc.enabled[idCard] || sc.enabled[idSSN] || sc.enabled[idAadhaar] || sc.enabled[idDOB] ||
		sc.enabled[idPhone] || (sc.enabled[idIP] && !e.ip.DisableV4) || sc.enabled[idPAN] ||
		sc.enabled[idNINO] || sc.enabled[idIBAN]
	if e.ac != nil {
		sc.keywords()
	}
	if sc.numOn {
		sc.mainPass()
	}
	if sc.enabled[idEmail] {
		sc.emails()
	}
	if sc.enabled[idIP] && !e.ip.DisableV6 {
		sc.ipv6()
	}
	if sc.enabled[idPassport] {
		sc.mrz()
	}
	if e.anchoredMask != 0 && len(sc.hits) > 0 {
		sc.anchored()
	}
	if len(e.regexes) > 0 {
		sc.custom()
	}
	return sc.resolve()
}

// keywords runs the Aho–Corasick automaton once and sorts its hits into
// context keywords, dictionary findings and custom-regex anchors.
func (sc *scanner) keywords() {
	t := sc.text
	n := len(t)
	e := sc.e
	e.ac.Iterate(t, func(m ACMatch) bool {
		leftWord := m.Start > 0 && ctab[t[m.Start-1]]&cWord != 0
		rightWord := m.End < n && ctab[t[m.End]]&cWord != 0
		for i := range e.pay[m.Pattern] {
			p := &e.pay[m.Pattern][i]
			switch p.kind {
			case payContext:
				// Keywords may take a plural "s" ("phones", "passports").
				if (p.checkLeft && leftWord) || (p.checkRight && rightWord &&
					!((t[m.End] == 's' || t[m.End] == 'S') && (m.End+1 == n || ctab[t[m.End+1]]&cWord == 0))) {
					continue
				}
				sc.hits = append(sc.hits, kwHit{m.Start, m.End, p.mask, p.neg})
			case payAnchor:
				if (p.checkLeft && leftWord) || (p.checkRight && rightWord) {
					continue
				}
				sc.anchors = append(sc.anchors, anchorHit{p.idx, m.End})
			case payDict:
				if p.wholeWord && ((p.checkLeft && leftWord) || (p.checkRight && rightWord)) {
					continue
				}
				if p.caseSensitive && t[m.Start:m.End] != p.term {
					continue
				}
				sc.addRaw(p.idx, m.Start, m.End, p.conf, "", false)
			}
		}
		return true
	})
	if len(sc.hits) > 1 {
		sort.Slice(sc.hits, func(i, j int) bool { return sc.hits[i].start < sc.hits[j].start })
	}
}

// context reports whether a keyword supporting (pos) or weakening (neg)
// built-in id lies within the window around [s, e): the full window before
// the value and half of it after.
func (sc *scanner) context(id int, s, e int) (pos, neg bool) {
	if len(sc.hits) == 0 {
		return false, false
	}
	w := sc.e.window
	lo := s - w
	bit := uint32(1) << uint(id)
	i := sort.Search(len(sc.hits), func(i int) bool { return sc.hits[i].end > lo })
	for ; i < len(sc.hits); i++ {
		h := &sc.hits[i]
		if h.start > e+w/2 {
			break
		}
		if h.end > s && h.start < e { // inside the value itself
			continue
		}
		if h.mask&bit != 0 {
			pos = true
		}
		if h.neg&bit != 0 {
			neg = true
		}
	}
	return pos, neg
}

// add records a built-in candidate after context adjustment. needCtx
// candidates are only kept when a context keyword is near.
func (sc *scanner) add(id, s, e int, conf float64, detail string, needCtx bool) {
	if s < sc.from {
		return
	}
	pos, neg := sc.context(id, s, e)
	if needCtx && !pos {
		return
	}
	// Candidates a detector deliberately demoted (reserved domains, role
	// accounts, documentation ranges) stay demoted.
	if pos && conf >= 0.35 {
		conf += (1 - conf) * ctxBoost
	}
	if neg {
		conf *= 0.4
	}
	sc.cands = append(sc.cands, cand{id, s, e, conf, detail, pos})
}

func (sc *scanner) addRaw(typ, s, e int, conf float64, detail string, ctx bool) {
	if s < sc.from {
		return
	}
	sc.cands = append(sc.cands, cand{typ, s, e, conf, detail, ctx})
}

// mainPass walks the text once, token by token, and hands every token that
// starts at a word boundary to the numeric or alphanumeric detectors.
func (sc *scanner) mainPass() {
	t := sc.text
	n := len(t)
	for i := 0; i < n; {
		cl := ctab[t[i]]
		if cl&cAlnum == 0 {
			i++
			continue
		}
		if i > 0 && ctab[t[i-1]]&cWord != 0 {
			for i < n && ctab[t[i]]&cWord != 0 {
				i++
			}
			continue
		}
		if cl&cDigit != 0 {
			// "v1.2.3.4", "a:12": the token continues a dotted or
			// colon-separated identifier.
			if i >= 2 && (t[i-1] == '.' || t[i-1] == ':') && ctab[t[i-2]]&cAlnum != 0 {
				for i < n && ctab[t[i]]&cWord != 0 {
					i++
				}
				continue
			}
			if i > 0 && t[i-1] == '+' && sc.enabled[idPhone] {
				if end, ok := sc.phoneE164(i - 1); ok {
					i = end
					continue
				}
			}
			i = sc.numeric(i)
			continue
		}
		i = sc.word(i)
	}
}

const maxGroups = 24

type span32 struct{ s, e int }

func isNumSep(c byte) bool { return c == ' ' || c == '-' || c == '.' || c == '/' }

// numeric parses a run of digit groups joined by single separators
// (space, '-', '.', '/') and tries every short sub-run that is delimited by
// spaces, so "Order 1234 4111111111111111" still yields the card.
func (sc *scanner) numeric(i int) int {
	t := sc.text
	n := len(t)
	var gs [maxGroups]span32
	var seps [maxGroups]byte
	ng := 0
	j := i
	for {
		k := j
		for k < n && isDigit(t[k]) {
			k++
		}
		gs[ng] = span32{j, k}
		ng++
		if ng < maxGroups && k+1 < n && isNumSep(t[k]) && isDigit(t[k+1]) {
			seps[ng-1] = t[k]
			j = k + 1
			continue
		}
		j = k
		break
	}
	end := j
	tainted := end < n && ctab[t[end]]&cWord != 0
	var after byte
	if end+1 < n && isNumSep(t[end]) && isDigit(t[end+1]) {
		after = t[end]
	}
	for a := 0; a < ng; a++ {
		if a > 0 && seps[a-1] != ' ' {
			continue
		}
		total := 0
		sep := byte(0)
		for b := a + 1; b <= ng && b-a <= 6; b++ {
			total += gs[b-1].e - gs[b-1].s
			if total > 19 {
				break
			}
			if b-a >= 2 {
				s := seps[b-2]
				if b-a == 2 {
					sep = s
				} else if s != sep {
					break
				}
			}
			trailing := after
			if b < ng {
				trailing = seps[b-1]
			} else if tainted {
				break
			}
			if trailing != 0 && trailing != ' ' {
				continue
			}
			ambiguous := sep != 0 && ((a > 0 && seps[a-1] == sep) || trailing == sep)
			sc.numericCandidate(gs[a:b], sep, total, ambiguous)
		}
	}
	return end
}

func (sc *scanner) numericCandidate(gs []span32, sep byte, total int, ambiguous bool) {
	t := sc.text
	d := sc.dbuf[:0]
	for _, g := range gs {
		d = append(d, t[g.s:g.e]...)
	}
	s, e := gs[0].s, gs[len(gs)-1].e
	ng := len(gs)
	glen := func(i int) int { return gs[i].e - gs[i].s }
	amb := 1.0
	if ambiguous {
		amb = 0.5
	}

	if sc.enabled[idCard] && total >= 12 && (sep == 0 || sep == ' ' || sep == '-') {
		ok := true
		if ng > 1 {
			for i := range gs {
				if l := glen(i); l < 3 || l > 6 {
					ok = false
				}
			}
		}
		if ok {
			if brand := validCard(d); brand != "" {
				conf := 0.85
				if ng > 1 {
					conf = 0.9
				}
				if total == 12 {
					conf = 0.55
				}
				sc.add(idCard, s, e, conf*amb, brand, false)
			}
		}
	}
	if sc.enabled[idSSN] && total == 9 && ssnValid(d) {
		switch {
		case ng == 3 && glen(0) == 3 && glen(1) == 2 && glen(2) == 4 && sep == '-':
			sc.add(idSSN, s, e, 0.85*amb, "", false)
		case ng == 3 && glen(0) == 3 && glen(1) == 2 && glen(2) == 4 && sep == ' ':
			sc.add(idSSN, s, e, 0.45*amb, "", false)
		case ng == 1:
			sc.add(idSSN, s, e, 0.6, "", true)
		}
	}
	if sc.enabled[idAadhaar] && total == 12 && aadhaarValid(d) {
		switch {
		case ng == 3 && glen(0) == 4 && glen(1) == 4 && glen(2) == 4 && (sep == ' ' || sep == '-'):
			sc.add(idAadhaar, s, e, 0.85*amb, "", false)
		case ng == 1:
			sc.add(idAadhaar, s, e, 0.4, "", false)
		}
	}
	if sc.enabled[idIP] && !sc.e.ip.DisableV4 && ng == 4 && sep == '.' {
		sc.ipv4(gs, s, e)
	}
	if sc.enabled[idDOB] && ng == 3 && (sep == '/' || sep == '-' || sep == '.') {
		if y, ok := sc.numericDate(gs); ok && y > 0 {
			sc.add(idDOB, s, e, 0.6, "", true)
		}
	}
}

func atoiSpan(t string, g span32) int {
	v := 0
	for i := g.s; i < g.e; i++ {
		v = v*10 + int(t[i]-'0')
	}
	return v
}

// numericDate accepts Y-M-D, D/M/Y and M/D/Y dates with a four-digit (or,
// for day-first and month-first forms, two-digit) year that is a plausible
// birth year.
func (sc *scanner) numericDate(gs []span32) (int, bool) {
	t := sc.text
	l0, l1, l2 := gs[0].e-gs[0].s, gs[1].e-gs[1].s, gs[2].e-gs[2].s
	a, b, c := atoiSpan(t, gs[0]), atoiSpan(t, gs[1]), atoiSpan(t, gs[2])
	plausible := func(y int) bool { return y >= 1900 && y <= sc.e.nowYear }
	switch {
	case l0 == 4 && l1 <= 2 && l2 <= 2:
		return a, plausible(a) && validDate(a, b, c)
	case l0 <= 2 && l1 <= 2 && (l2 == 4 || l2 == 2):
		y := c
		if l2 == 2 {
			y += 1900
			if y+100 <= sc.e.nowYear {
				y += 100
			}
		}
		return y, plausible(y) && (validDate(y, b, a) || validDate(y, a, b))
	}
	return 0, false
}

func (sc *scanner) ipv4(gs []span32, s, e int) {
	t := sc.text
	var b [4]byte
	for i, g := range gs {
		l := g.e - g.s
		if l > 3 || (l > 1 && t[g.s] == '0') {
			return
		}
		v := atoiSpan(t, g)
		if v > 255 {
			return
		}
		b[i] = byte(v)
	}
	sc.ipCandidate(netip.AddrFrom4(b), s, e)
}

var docPrefixes = []netip.Prefix{
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("2001:db8::/32"),
}

func (sc *scanner) ipCandidate(a netip.Addr, s, e int) {
	private := a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsUnspecified() || a.IsMulticast() || a.IsInterfaceLocalMulticast()
	if a.Is4() && a.As4()[0] == 0 || a.Is4() && a.As4() == [4]byte{255, 255, 255, 255} {
		private = true
	}
	family := "ipv4"
	if a.Is6() && !a.Is4In6() {
		family = "ipv6"
	}
	for _, p := range docPrefixes {
		if p.Contains(a) {
			sc.add(idIP, s, e, 0.2, family+",documentation", false)
			return
		}
	}
	if private {
		if !sc.e.ip.IncludePrivate {
			return
		}
		sc.add(idIP, s, e, 0.7, family+",private", false)
		return
	}
	sc.add(idIP, s, e, 0.7, family, false)
}

// phoneE164 parses an international number starting at the '+' at plus.
func (sc *scanner) phoneE164(plus int) (int, bool) {
	t := sc.text
	n := len(t)
	if plus > 0 && (ctab[t[plus-1]]&cWord != 0 || t[plus-1] == '+') {
		return 0, false
	}
	d := sc.dbuf[:0]
	last := plus
	prevSep := false
	parens := 0
	for k := plus + 1; k < n; k++ {
		c := t[k]
		switch {
		case isDigit(c):
			if len(d) == 15 {
				return 0, false
			}
			d = append(d, c)
			last = k + 1
			prevSep = false
		case c == '(':
			if parens != 0 {
				goto done
			}
			parens++
			prevSep = false
		case c == ')':
			if parens != 1 {
				goto done
			}
			parens--
			prevSep = false
		case c == ' ' || c == '-' || c == '.':
			if prevSep || k+1 >= n || !(isDigit(t[k+1]) || t[k+1] == '(') {
				goto done
			}
			prevSep = true
		default:
			goto done
		}
	}
done:
	if last < n && ctab[t[last]]&cWord != 0 {
		return 0, false
	}
	cc, ok := phoneValid(d)
	if !ok {
		return 0, false
	}
	sc.add(idPhone, plus, last, 0.75, "+"+cc, false)
	return last, true
}

// word handles a token that starts with a letter: Indian PAN, UK NINO
// (compact or spaced) and IBAN (compact or in groups of four).
func (sc *scanner) word(i int) int {
	t := sc.text
	n := len(t)
	j := i
	for j < n && ctab[t[j]]&cAlnum != 0 {
		j++
	}
	if j < n && t[j] == '_' {
		for j < n && ctab[t[j]]&cWord != 0 {
			j++
		}
		return j
	}
	if !isUpper(t[i]) || j-i < 2 {
		return j
	}
	tok := t[i:j]
	switch len(tok) {
	case 10:
		if sc.enabled[idPAN] && panValid(tok) {
			sc.add(idPAN, i, j, 0.7, panEntity(tok[3]), false)
		}
	case 9:
		if sc.enabled[idNINO] && ninoValid(tok) {
			sc.add(idNINO, i, j, 0.75, "", false)
		}
	case 2:
		if sc.enabled[idNINO] {
			if end, ok := sc.ninoSpaced(i); ok {
				return end
			}
		}
	}
	if sc.enabled[idIBAN] && len(tok) >= 4 && isUpper(tok[1]) && isDigit(tok[2]) && isDigit(tok[3]) {
		if end, ok := sc.iban(i); ok {
			return end
		}
	}
	return j
}

func panEntity(c byte) string {
	switch c {
	case 'P':
		return "individual"
	case 'C':
		return "company"
	case 'H':
		return "huf"
	case 'F':
		return "firm"
	case 'A':
		return "aop"
	case 'T':
		return "trust"
	case 'B':
		return "boi"
	case 'L':
		return "local-authority"
	case 'J':
		return "artificial-juridical-person"
	case 'G':
		return "government"
	}
	return ""
}

// ninoSpaced matches "AB 12 34 56 C".
func (sc *scanner) ninoSpaced(i int) (int, bool) {
	t := sc.text
	const shape = "LL DD DD DD L"
	if i+len(shape) > len(t) {
		return 0, false
	}
	b := sc.dbuf[:0]
	for k := 0; k < len(shape); k++ {
		c := t[i+k]
		switch shape[k] {
		case 'L':
			if !isUpper(c) {
				return 0, false
			}
			b = append(b, c)
		case 'D':
			if !isDigit(c) {
				return 0, false
			}
			b = append(b, c)
		case ' ':
			if c != ' ' {
				return 0, false
			}
		}
	}
	end := i + len(shape)
	if end < len(t) && ctab[t[end]]&cWord != 0 {
		return 0, false
	}
	if !ninoValid(string(b)) {
		return 0, false
	}
	sc.add(idNINO, i, end, 0.75, "", false)
	return end, true
}

// iban matches an IBAN written compactly or in space-separated groups of
// four, at the registered length for its country.
func (sc *scanner) iban(i int) (int, bool) {
	t := sc.text
	n := len(t)
	want, ok := ibanLengths[t[i:i+2]]
	if !ok {
		return 0, false
	}
	buf := sc.dbuf[:0]
	k := i
	for k < n && len(buf) < want {
		c := t[k]
		if ctab[c]&cAlnum != 0 {
			buf = append(buf, toUpper(c))
			k++
			continue
		}
		if c == ' ' && len(buf)%4 == 0 && k+1 < n && ctab[t[k+1]]&cAlnum != 0 {
			k++
			continue
		}
		break
	}
	if len(buf) != want || (k < n && ctab[t[k]]&cWord != 0) || !ibanValid(buf) {
		return 0, false
	}
	sc.add(idIBAN, i, k, 0.95, string(buf[:2]), false)
	return k, true
}

func isLocalByte(c byte) bool {
	return ctab[c]&cWord != 0 || c == '.' || c == '%' || c == '+' || c == '-'
}

func isDomainByte(c byte) bool { return ctab[c]&cAlnum != 0 || c == '-' || c == '.' }

// Top-level "domains" that are really file extensions ("icon@2x.png",
// "pkg@1.2.3/index.js").
var fileExtTLD = map[string]bool{
	"png": true, "jpg": true, "jpeg": true, "gif": true, "svg": true, "webp": true, "ico": true,
	"css": true, "js": true, "mjs": true, "cjs": true, "ts": true, "tsx": true, "jsx": true,
	"json": true, "yaml": true, "yml": true, "lock": true, "html": true, "htm": true, "txt": true,
	"go": true, "py": true, "rb": true, "rs": true, "java": true, "kt": true, "swift": true,
	"md": true, "map": true, "wasm": true, "toml": true, "xml": true, "csv": true, "log": true,
}

// Local parts of role accounts that do not identify a person.
var roleLocal = map[string]bool{
	"noreply": true, "no-reply": true, "donotreply": true, "do-not-reply": true, "postmaster": true,
	"mailer-daemon": true, "abuse": true, "hostmaster": true, "webmaster": true, "git": true,
	"root": true, "nobody": true, "notifications": true, "notification": true, "bounce": true,
	"bounces": true, "security": true, "privacy": true, "support": true, "info": true,
}

func reservedDomain(d string) bool {
	switch d {
	case "example.com", "example.org", "example.net", "localhost":
		return true
	}
	for _, s := range []string{".example", ".test", ".invalid", ".localhost", ".local", ".example.com", ".example.org", ".example.net"} {
		if strings.HasSuffix(d, s) {
			return true
		}
	}
	return false
}

// emails finds addresses around each '@'.
func (sc *scanner) emails() {
	t := sc.text
	n := len(t)
	for off := 0; off < n; {
		k := strings.IndexByte(t[off:], '@')
		if k < 0 {
			return
		}
		at := off + k
		off = at + 1
		s := at
		for s > 0 && isLocalByte(t[s-1]) && at-s <= 64 {
			s--
		}
		for s < at && (t[s] == '.' || t[s] == '-' || t[s] == '+') {
			s++
		}
		if s == at || at-s > 64 || (s > 0 && isLocalByte(t[s-1])) || t[at-1] == '.' ||
			strings.Contains(t[s:at], "..") {
			continue
		}
		e := at + 1
		for e < n && isDomainByte(t[e]) && e-at <= 254 {
			e++
		}
		for e > at+1 && (t[e-1] == '.' || t[e-1] == '-') {
			e--
		}
		if e < n && (ctab[t[e]]&cWord != 0 || t[e] == '@') {
			continue
		}
		dom := t[at+1 : e]
		tld, ok := checkDomain(dom)
		if !ok {
			continue
		}
		if fileExtTLD[strings.ToLower(tld)] {
			continue
		}
		off = e
		ld := strings.ToLower(dom)
		if sc.allowedDomain(ld) {
			continue
		}
		conf := 0.9
		if reservedDomain(ld) {
			conf = 0.2
		} else if roleLocal[strings.ToLower(t[s:at])] {
			conf = 0.3
		}
		sc.add(idEmail, s, e, conf, "", false)
	}
}

func (sc *scanner) allowedDomain(d string) bool {
	for _, a := range sc.e.allowDomains {
		if d == a || strings.HasSuffix(d, "."+a) {
			return true
		}
	}
	return false
}

// checkDomain validates dot-separated labels and returns the TLD, which
// must be alphabetic (or an IDN "xn--" label) and at least two letters.
func checkDomain(d string) (string, bool) {
	if len(d) < 4 || len(d) > 253 {
		return "", false
	}
	dots := 0
	start := 0
	for i := 0; i <= len(d); i++ {
		if i < len(d) && d[i] != '.' {
			continue
		}
		l := d[start:i]
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return "", false
		}
		if i < len(d) {
			dots++
		}
		start = i + 1
	}
	if dots == 0 {
		return "", false
	}
	tld := d[strings.LastIndexByte(d, '.')+1:]
	if strings.HasPrefix(strings.ToLower(tld), "xn--") {
		return tld, len(tld) > 4
	}
	if len(tld) < 2 || len(tld) > 24 {
		return "", false
	}
	for i := 0; i < len(tld); i++ {
		if !isAlpha(tld[i]) {
			return "", false
		}
	}
	return tld, true
}

func isV6Byte(c byte) bool { return isHex(c) || c == ':' || c == '.' }

// ipv6 finds IPv6 addresses around each ':'.
func (sc *scanner) ipv6() {
	t := sc.text
	n := len(t)
	for off := 0; off < n; {
		k := strings.IndexByte(t[off:], ':')
		if k < 0 {
			return
		}
		p := off + k
		off = p + 1
		// Cheap rejection: a colon must touch a hex digit or another colon.
		if !(p+1 < n && (isHex(t[p+1]) || t[p+1] == ':')) && !(p > 0 && isHex(t[p-1])) {
			continue
		}
		s := p
		for s > 0 && isV6Byte(t[s-1]) && p-s < 45 {
			s--
		}
		e := p
		for e < n && isV6Byte(t[e]) && e-s < 46 {
			e++
		}
		off = e + 1
		for e > s && t[e-1] == '.' {
			e--
		}
		if e-s < 3 || (s > 0 && ctab[t[s-1]]&cWord != 0) || (e < n && ctab[t[e]]&cWord != 0) {
			continue
		}
		cs := t[s:e]
		if strings.Count(cs, ":") < 2 || !strings.ContainsAny(cs, "0123456789") {
			continue
		}
		groups, maxLen, cur := 0, 0, 0
		for i := 0; i <= len(cs); i++ {
			if i == len(cs) || cs[i] == ':' {
				if cur > 0 {
					groups++
				}
				if cur > maxLen {
					maxLen = cur
				}
				cur = 0
				continue
			}
			cur++
		}
		if groups < 3 && maxLen < 4 {
			continue
		}
		a, err := netip.ParseAddr(cs)
		if err != nil || !a.Is6() {
			continue
		}
		if a.Is4In6() {
			a = a.Unmap()
		}
		sc.ipCandidate(a, s, e)
	}
}

// mrz finds ICAO 9303 machine-readable zones (TD1, TD2, TD3) and validates
// every check digit.
func (sc *scanner) mrz() {
	t := sc.text
	done := 0
	for off := 0; off < len(t); {
		k := strings.Index(t[off:], "<<")
		if k < 0 {
			return
		}
		p := off + k
		ls, le := lineBounds(t, p)
		off = le + 1
		if ls < done {
			continue
		}
		line := trimCR(t[ls:le])
		if !mrzCharset(line) {
			continue
		}
		// Gather up to two lines before and after.
		type ln struct {
			s, e int
			v    string
		}
		lines := []ln{{ls, ls + len(line), line}}
		ps := ls
		for i := 0; i < 2 && ps > 0; i++ {
			a, b := lineBounds(t, ps-1)
			v := trimCR(t[a:b])
			lines = append([]ln{{a, a + len(v), v}}, lines...)
			ps = a
		}
		cur := 0
		for i := range lines {
			if lines[i].s == ls {
				cur = i
			}
		}
		ne := le
		for i := 0; i < 2 && ne < len(t); i++ {
			a, b := lineBounds(t, ne+1)
			v := trimCR(t[a:b])
			lines = append(lines, ln{a, a + len(v), v})
			ne = b
		}
		found := false
		try := func(first, count int) bool {
			if first < 0 || first+count > len(lines) || lines[first].s < done {
				return false
			}
			ls := lines[first : first+count]
			for _, l := range ls {
				if !mrzCharset(l.v) || len(l.v) != len(ls[0].v) {
					return false
				}
			}
			var detail string
			switch {
			case count == 2 && len(ls[0].v) == 44 && ls[0].v[0] == 'P' && mrzTD3Line2(ls[1].v):
				detail = "mrz-td3"
			case count == 2 && len(ls[0].v) == 36 && mrzTD2Line2(ls[1].v):
				detail = "mrz-td2"
			case count == 3 && len(ls[0].v) == 30 && mrzTD1(ls[0].v, ls[1].v):
				detail = "mrz-td1"
			case count == 1 && mrzTD3Line2(ls[0].v):
				detail = "mrz-td3-line2"
			default:
				return false
			}
			sc.add(idPassport, ls[0].s, ls[count-1].e, 0.99, detail, false)
			done = ls[count-1].e
			return true
		}
		for _, c := range [][2]int{{cur - 2, 3}, {cur - 1, 3}, {cur, 3}, {cur - 1, 2}, {cur, 2}} {
			if try(c[0], c[1]) {
				found = true
				break
			}
		}
		if !found {
			try(cur, 1)
		}
		if done > off {
			off = done
		}
	}
}

func lineBounds(t string, p int) (int, int) {
	s := strings.LastIndexByte(t[:p], '\n') + 1
	e := strings.IndexByte(t[p:], '\n')
	if e < 0 {
		e = len(t)
	} else {
		e += p
	}
	return s, e
}

func trimCR(s string) string { return strings.TrimRight(s, "\r \t") }

// Keyword-anchored detectors run only in the window after a context
// keyword of their type.
type anchoredSpec struct {
	id    int
	re    *regexp.Regexp
	all   bool // report every match in the window, not just the first
	wide  bool // search twice the window
	check func(sc *scanner, m string, sub []string) (float64, string, bool)
}

var monthNames = []string{"january", "february", "march", "april", "may", "june", "july", "august", "september", "october", "november", "december"}

func monthNumber(s string) int {
	s = strings.ToLower(strings.TrimSuffix(s, "."))
	if len(s) < 3 {
		return 0
	}
	if s == "sept" {
		return 9
	}
	for i, m := range monthNames {
		if strings.HasPrefix(m, s) {
			return i + 1
		}
	}
	return 0
}

func countDigits(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if isDigit(s[i]) {
			n++
		}
	}
	return n
}

var anchoredSpecs = []anchoredSpec{
	{id: idDOB, re: regexp.MustCompile(`(?i)\b(\d{1,2})(?:st|nd|rd|th)?(?:\s+of)?\s+([a-z]{3,9}\.?),?\s+(\d{4})\b`),
		check: func(sc *scanner, _ string, sub []string) (float64, string, bool) {
			return sc.textDate(sub[3], sub[2], sub[1])
		}},
	{id: idDOB, re: regexp.MustCompile(`(?i)\b([a-z]{3,9}\.?)\s+(\d{1,2})(?:st|nd|rd|th)?,?\s+(\d{4})\b`),
		check: func(sc *scanner, _ string, sub []string) (float64, string, bool) {
			return sc.textDate(sub[3], sub[1], sub[2])
		}},
	{id: idPhone, re: regexp.MustCompile(`(?:\+?\(\d|\+\d|\b\d)[\d ().-]{5,22}\d\b`),
		check: func(_ *scanner, m string, _ []string) (float64, string, bool) {
			n := countDigits(m)
			if n < 9 || n > 15 || strings.Contains(m, "  ") || strings.Contains(m, "--") || strings.Count(m, ".") > 3 ||
				strings.Count(m, "(") != strings.Count(m, ")") {
				return 0, "", false
			}
			return 0.8, "national", true
		}},
	{id: idPassport, re: regexp.MustCompile(`\b[A-Z0-9]{6,9}\b`),
		check: func(_ *scanner, m string, _ []string) (float64, string, bool) {
			return 0.85, "", countDigits(m) >= 2
		}},
	{id: idMRN, re: regexp.MustCompile(`\b\d{3}[ -]?\d{3}[ -]?\d{4}\b`),
		check: func(_ *scanner, m string, _ []string) (float64, string, bool) {
			var b [10]byte
			d := b[:0]
			for i := 0; i < len(m); i++ {
				if isDigit(m[i]) {
					d = append(d, m[i])
				}
			}
			return 0.95, "nhs", nhsValid(d)
		}},
	{id: idMRN, re: regexp.MustCompile(`\b[A-Za-z0-9][A-Za-z0-9-]{4,19}\b`),
		check: func(_ *scanner, m string, _ []string) (float64, string, bool) {
			return 0.85, "", countDigits(m) >= 4
		}},
	{id: idICD, all: true, wide: true, re: regexp.MustCompile(`\b[A-TV-Z][0-9][0-9AB](?:\.[0-9A-TV-Z]{1,4})?\b`),
		check: func(_ *scanner, _ string, _ []string) (float64, string, bool) {
			return 0.85, "icd-10", true
		}},
}

func (sc *scanner) textDate(year, month, day string) (float64, string, bool) {
	m := monthNumber(month)
	if m == 0 {
		return 0, "", false
	}
	y := 0
	for i := 0; i < len(year); i++ {
		y = y*10 + int(year[i]-'0')
	}
	d := 0
	for i := 0; i < len(day); i++ {
		d = d*10 + int(day[i]-'0')
	}
	if y < 1900 || y > sc.e.nowYear || !validDate(y, m, d) {
		return 0, "", false
	}
	return 0.9, "", true
}

// anchored runs the keyword-anchored detectors.
func (sc *scanner) anchored() {
	t := sc.text
	w := sc.e.window
	for i := range anchoredSpecs {
		sp := &anchoredSpecs[i]
		if sc.e.anchoredMask&(1<<uint(sp.id)) == 0 {
			continue
		}
		bit := uint32(1) << uint(sp.id)
		covered := 0
		for _, h := range sc.hits {
			if h.mask&bit == 0 {
				continue
			}
			lim := w
			if sp.wide {
				lim = 2 * w
			}
			from := h.end
			if sp.all && covered > from {
				from = covered
			}
			to := min(len(t), h.end+lim)
			if from >= to {
				continue
			}
			region := t[from:to]
			for _, m := range sp.re.FindAllStringSubmatchIndex(region, -1) {
				sub := make([]string, len(m)/2)
				for g := range sub {
					if m[2*g] >= 0 {
						sub[g] = region[m[2*g]:m[2*g+1]]
					}
				}
				conf, detail, ok := sp.check(sc, sub[0], sub)
				if !ok {
					continue
				}
				s, e := from+m[0], from+m[1]
				if s >= sc.from {
					sc.cands = append(sc.cands, cand{sp.id, s, e, conf, detail, true})
				}
				if !sp.all {
					break
				}
			}
			covered = to
		}
	}
}

// custom runs the policy's regular-expression detectors.
func (sc *scanner) custom() {
	t := sc.text
	w := sc.e.window
	emit := func(cr *customRegex, base int, region string, m []int) {
		gs, ge := m[2*cr.group], m[2*cr.group+1]
		if gs < 0 || ge <= gs || ge-gs > MaxMatchLen {
			return
		}
		sc.addRaw(cr.typ, base+gs, base+ge, cr.conf, "", false)
	}
	for ri := range sc.e.regexes {
		cr := &sc.e.regexes[ri]
		if !cr.anchored {
			for _, m := range cr.re.FindAllStringSubmatchIndex(t, -1) {
				emit(cr, 0, t, m)
			}
			continue
		}
		covered := 0
		for _, a := range sc.anchors {
			if a.regex != ri {
				continue
			}
			from := max(a.end, covered)
			to := min(len(t), a.end+w)
			if from >= to {
				continue
			}
			region := t[from:to]
			for _, m := range cr.re.FindAllStringSubmatchIndex(region, -1) {
				emit(cr, from, region, m)
			}
			covered = to
		}
	}
}

// resolve drops low-confidence and allow-listed candidates and settles
// overlaps: within each cluster of overlapping candidates the most
// confident (then longest) wins.
func (sc *scanner) resolve() []cand {
	e := sc.e
	cs := sc.cands[:0]
	for _, c := range sc.cands {
		if c.start < sc.from || c.conf < e.types[c.typ].minConf || c.end <= c.start {
			continue
		}
		if len(e.allow) > 0 && e.allow[sc.text[c.start:c.end]] {
			continue
		}
		if c.conf > 1 {
			c.conf = 1
		}
		cs = append(cs, c)
	}
	if len(cs) < 2 {
		return cs
	}
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].start != cs[j].start {
			return cs[i].start < cs[j].start
		}
		return cs[i].end > cs[j].end
	})
	out := make([]cand, 0, len(cs))
	for i := 0; i < len(cs); {
		j := i + 1
		end := cs[i].end
		for j < len(cs) && cs[j].start < end {
			end = max(end, cs[j].end)
			j++
		}
		if j == i+1 {
			out = append(out, cs[i])
			i = j
			continue
		}
		cl := append([]cand(nil), cs[i:j]...)
		sort.SliceStable(cl, func(a, b int) bool {
			if cl[a].conf != cl[b].conf {
				return cl[a].conf > cl[b].conf
			}
			if la, lb := cl[a].end-cl[a].start, cl[b].end-cl[b].start; la != lb {
				return la > lb
			}
			return cl[a].typ < cl[b].typ
		})
		var kept []cand
		for _, c := range cl {
			ok := true
			for _, k := range kept {
				if c.start < k.end && k.start < c.end {
					ok = false
					break
				}
			}
			if ok {
				kept = append(kept, c)
			}
		}
		sort.Slice(kept, func(a, b int) bool { return kept[a].start < kept[b].start })
		out = append(out, kept...)
		i = j
	}
	return out
}
