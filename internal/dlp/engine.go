package dlp

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Engine applies a compiled Policy. Build it once with New; it is immutable
// and safe for concurrent use.
type Engine struct {
	name  string
	types []typeRuntime // built-ins, then custom regex, then dictionaries

	ac  *AhoCorasick
	pay [][]payload // per AC pattern

	regexes []customRegex

	window       int
	minConf      float64
	allow        map[string]bool
	allowDomains []string
	ip           IPPolicy
	maskRune     rune
	blockScore   float64
	blockLabel   Label
	hasBlockLbl  bool
	nowYear      int
	anchoredMask uint32 // enabled built-ins with keyword-anchored detectors
}

type typeRuntime struct {
	name       InfoType
	label      Label
	frameworks []Framework
	action     Action
	minConf    float64
	enabled    bool
	keepLast   int
}

const (
	payContext = iota
	payDict
	payAnchor
)

type payload struct {
	kind          uint8
	caseSensitive bool
	wholeWord     bool
	checkLeft     bool
	checkRight    bool
	mask          uint32 // context: built-ins this keyword supports
	neg           uint32 // context: built-ins this keyword weakens
	idx           int    // dict: type index; anchor: regex index
	term          string // dict: exact term for case-sensitive checks
	conf          float64
}

type customRegex struct {
	typ      int
	re       *regexp.Regexp
	group    int
	anchored bool
	conf     float64
}

// New compiles a policy. A nil policy means DefaultPolicy(). The policy is
// validated again, so a hand-built Policy gets the same checks as JSON.
func New(p *Policy) (*Engine, error) {
	if p == nil {
		p = DefaultPolicy()
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	e := &Engine{
		name:     p.Name,
		window:   64,
		minConf:  0.5,
		allow:    map[string]bool{},
		ip:       p.IP,
		maskRune: '*',
		nowYear:  time.Now().Year(),
	}
	if p.ContextWindow > 0 {
		e.window = p.ContextWindow
	}
	if p.MinConfidence != nil {
		e.minConf = *p.MinConfidence
	}
	if p.MaskChar != "" {
		e.maskRune, _ = utf8.DecodeRuneInString(p.MaskChar)
	}
	keep := 4
	if p.KeepLast != nil {
		keep = *p.KeepLast
	}
	e.blockScore = p.BlockScore
	if p.BlockLabel != nil {
		e.blockLabel, e.hasBlockLbl = *p.BlockLabel, true
	}
	for _, v := range p.AllowValues {
		e.allow[v] = true
	}
	for _, d := range p.AllowEmailDomains {
		e.allowDomains = append(e.allowDomains, strings.ToLower(strings.TrimSuffix(d, ".")))
	}
	pick := func(override, fallback Action) Action {
		if override != "" {
			return override
		}
		return fallback
	}
	keepOf := func(v *int) int {
		if v != nil {
			return *v
		}
		return keep
	}
	labelOf := func(v *Label, def Label) Label {
		if v != nil {
			return *v
		}
		return def
	}
	confOf := func(c, def float64) float64 {
		if c > 0 {
			return c
		}
		return def
	}

	// Built-ins.
	for i := range builtins {
		b := &builtins[i]
		tr := typeRuntime{
			name: b.t, label: b.label, frameworks: b.frameworks,
			action: pick(p.DefaultAction, b.action), minConf: e.minConf, enabled: true, keepLast: keep,
		}
		if d, ok := p.Detectors[b.t]; ok {
			if d.Enabled != nil {
				tr.enabled = *d.Enabled
			}
			tr.action = pick(d.Action, tr.action)
			if d.MinConfidence != nil {
				tr.minConf = *d.MinConfidence
			}
			tr.label = labelOf(d.Label, tr.label)
			if d.Frameworks != nil {
				tr.frameworks = d.Frameworks
			}
			tr.keepLast = keepOf(d.KeepLast)
		}
		e.types = append(e.types, tr)
	}
	if e.ip.DisableV4 && e.ip.DisableV6 {
		e.types[idIP].enabled = false
	}
	for _, id := range []int{idDOB, idPhone, idPassport, idMRN, idICD} {
		if e.types[id].enabled {
			e.anchoredMask |= 1 << uint(id)
		}
	}

	// Aho–Corasick patterns: context keywords, dictionary terms and custom
	// regex keywords share one automaton.
	var pats []string
	index := map[string]int{}
	addPattern := func(s string, pl payload) {
		key := strings.ToLower(s)
		if pl.kind == payContext {
			if i, ok := index[key]; ok {
				for j := range e.pay[i] {
					if e.pay[i][j].kind == payContext {
						e.pay[i][j].mask |= pl.mask
						e.pay[i][j].neg |= pl.neg
						return
					}
				}
				e.pay[i] = append(e.pay[i], pl)
				return
			}
		} else if i, ok := index[key]; ok {
			e.pay[i] = append(e.pay[i], pl)
			return
		}
		index[key] = len(pats)
		pats = append(pats, s)
		e.pay = append(e.pay, []payload{pl})
	}
	edges := func(s string) (bool, bool) {
		return isAlnum(s[0]), isAlnum(s[len(s)-1])
	}
	for i := range builtins {
		if !e.types[i].enabled {
			continue
		}
		for _, k := range builtins[i].keywords {
			l, r := edges(k)
			addPattern(k, payload{kind: payContext, mask: 1 << uint(i), checkLeft: l, checkRight: r})
		}
		for _, k := range builtins[i].negative {
			l, r := edges(k)
			addPattern(k, payload{kind: payContext, neg: 1 << uint(i), checkLeft: l, checkRight: r})
		}
	}
	for i, r := range p.CustomRegex {
		typ := len(e.types)
		e.types = append(e.types, typeRuntime{
			name: r.Name, label: labelOf(r.Label, Confidential), frameworks: r.Frameworks,
			action: pick(r.Action, pick(p.DefaultAction, ActionRedact)), minConf: e.minConf, enabled: true,
			keepLast: keepOf(r.KeepLast),
		})
		cr := customRegex{typ: typ, re: regexp.MustCompile(r.Pattern), group: r.Group, conf: confOf(r.Confidence, 0.9)}
		if len(r.Keywords) > 0 {
			cr.anchored = true
			for _, k := range r.Keywords {
				l, rt := edges(k)
				addPattern(k, payload{kind: payAnchor, idx: i, checkLeft: l, checkRight: rt})
			}
		}
		e.regexes = append(e.regexes, cr)
	}
	for _, d := range p.Dictionaries {
		typ := len(e.types)
		e.types = append(e.types, typeRuntime{
			name: d.Name, label: labelOf(d.Label, Confidential), frameworks: d.Frameworks,
			action: pick(d.Action, pick(p.DefaultAction, ActionRedact)), minConf: e.minConf, enabled: true,
			keepLast: keepOf(d.KeepLast),
		})
		whole := d.WholeWord == nil || *d.WholeWord
		for _, t := range d.Terms {
			l, r := edges(t)
			addPattern(t, payload{
				kind: payDict, idx: typ, term: t, caseSensitive: d.CaseSensitive,
				wholeWord: whole, checkLeft: l, checkRight: r, conf: confOf(d.Confidence, 0.9),
			})
		}
	}
	if len(pats) > 0 {
		ac, err := NewAhoCorasick(pats, ACOptions{CaseInsensitive: true})
		if err != nil {
			return nil, err
		}
		e.ac = ac
	}
	return e, nil
}

// Name returns the policy name.
func (e *Engine) Name() string { return e.name }

// Result is the outcome of Process.
type Result struct {
	// Text is the transformed text. When Blocked is true it must not be
	// sent; it is still returned (with blocked values redacted) so callers
	// can show the user what was refused.
	Text     string    `json:"-"`
	Findings []Finding `json:"findings"`
	Report   Report    `json:"report"`
	// Changed reports whether Text differs from the input.
	Changed     bool   `json:"changed"`
	Blocked     bool   `json:"blocked"`
	Explanation string `json:"explanation,omitempty"`
}

// ErrBlocked matches every *BlockedError with errors.Is.
var ErrBlocked = errors.New("dlp: blocked by policy")

// BlockedError explains why a payload was refused. It names types, labels
// and counts, never values.
type BlockedError struct {
	Policy  string
	Reasons []string
	Report  Report
}

func (b *BlockedError) Error() string {
	name := b.Policy
	if name == "" {
		name = "dlp"
	}
	return fmt.Sprintf("dlp: blocked by policy %q: %s", name, strings.Join(b.Reasons, "; "))
}

// Is makes errors.Is(err, ErrBlocked) true.
func (b *BlockedError) Is(target error) bool { return target == ErrBlocked }

// Scan reports what the policy finds in text without changing it. The
// Action of each finding is the action Process would apply.
func (e *Engine) Scan(text string) []Finding {
	cands := e.detect(text, 0)
	out := make([]Finding, len(cands))
	for i, c := range cands {
		out[i] = e.finding(c, 0, e.types[c.typ].action, "")
	}
	return out
}

// Classify scans text and returns only the report: counts, document label,
// frameworks and score.
func (e *Engine) Classify(text string) Report {
	f := e.Scan(text)
	r := buildReport(e.name, int64(len(text)), f)
	return r
}

// Process applies the policy to text. Tokenize actions use v; with a nil
// Vault they fall back to redaction. When the policy blocks the payload,
// the error is a *BlockedError (errors.Is(err, ErrBlocked)) and the Result
// says why.
func (e *Engine) Process(text string, v *Vault) (*Result, error) {
	cands := e.detect(text, 0)
	out, findings, blocks := e.apply(text, 0, len(text), cands, v, 0)
	res := &Result{Text: out, Findings: findings, Changed: out != text}
	res.Report = buildReport(e.name, int64(len(text)), findings)
	if err := e.decide(res, blocks); err != nil {
		return res, err
	}
	return res, nil
}

// Restore replaces the Vault's tokens in text (typically model output)
// with the original values.
func (e *Engine) Restore(text string, v *Vault) string {
	if v == nil {
		return text
	}
	out, _ := v.Detokenize(text)
	return out
}

// decide applies the block rules and fills in the explanation.
func (e *Engine) decide(res *Result, blocks map[InfoType]int) error {
	var reasons []string
	if len(blocks) > 0 {
		types := make([]string, 0, len(blocks))
		for t := range blocks {
			types = append(types, string(t))
		}
		sort.Strings(types)
		for _, t := range types {
			tr := e.typeByName(InfoType(t))
			reasons = append(reasons, fmt.Sprintf("%d %s value(s) (%s%s) are not allowed to leave this machine",
				blocks[InfoType(t)], t, tr.label, fwSuffix(tr.frameworks)))
		}
	}
	if e.blockScore > 0 && res.Report.Score >= e.blockScore {
		reasons = append(reasons, fmt.Sprintf("sensitivity score %.1f reaches the limit %.1f", res.Report.Score, e.blockScore))
	}
	if e.hasBlockLbl && res.Report.Findings > 0 && res.Report.Label >= e.blockLabel {
		reasons = append(reasons, fmt.Sprintf("document is classified %s (limit %s)", res.Report.Label, e.blockLabel))
	}
	if len(reasons) == 0 {
		return nil
	}
	res.Blocked = true
	res.Report.Blocked = 1
	berr := &BlockedError{Policy: e.name, Reasons: reasons, Report: res.Report}
	res.Explanation = berr.Error()
	return berr
}

func fwSuffix(fs []Framework) string {
	if len(fs) == 0 {
		return ""
	}
	s := make([]string, len(fs))
	for i, f := range fs {
		s[i] = string(f)
	}
	return "; " + strings.Join(s, ", ")
}

func (e *Engine) typeByName(t InfoType) *typeRuntime {
	for i := range e.types {
		if e.types[i].name == t {
			return &e.types[i]
		}
	}
	return &typeRuntime{name: t}
}

func (e *Engine) finding(c cand, offset int, act Action, rep string) Finding {
	tr := &e.types[c.typ]
	return Finding{
		Type: tr.name, Start: c.start + offset, End: c.end + offset, Confidence: c.conf,
		Label: tr.label, Frameworks: tr.frameworks, Action: act, Detail: c.detail,
		Context: c.ctx, Replacement: rep,
	}
}

// apply rewrites text[from:to] using cands (sorted, inside the range) and
// returns the new text, the findings (offsets shifted by offset into the
// caller's coordinates) and the count of blocked values per type.
func (e *Engine) apply(text string, from, to int, cands []cand, v *Vault, offset int) (string, []Finding, map[InfoType]int) {
	if len(cands) == 0 {
		return text[from:to], nil, nil
	}
	var b strings.Builder
	b.Grow(to - from + 16*len(cands))
	findings := make([]Finding, 0, len(cands))
	var blocks map[InfoType]int
	pos := from
	for _, c := range cands {
		tr := &e.types[c.typ]
		b.WriteString(text[pos:c.start])
		val := text[c.start:c.end]
		act := tr.action
		var rep string
		switch act {
		case ActionAllow:
			rep = val
		case ActionMask:
			rep = maskValue(tr.name, val, tr.keepLast, e.maskRune)
		case ActionTokenize:
			if v != nil {
				if tok, err := v.Tokenize(tr.name, val); err == nil {
					rep = tok
					break
				}
			}
			act = ActionRedact
			rep = redactPlaceholder(tr.name)
		case ActionBlock:
			if blocks == nil {
				blocks = map[InfoType]int{}
			}
			blocks[tr.name]++
			rep = redactPlaceholder(tr.name)
		default:
			rep = redactPlaceholder(tr.name)
		}
		b.WriteString(rep)
		if act == ActionAllow {
			rep = ""
		}
		findings = append(findings, e.finding(c, offset, act, rep))
		pos = c.end
	}
	b.WriteString(text[pos:to])
	return b.String(), findings, blocks
}

func redactPlaceholder(t InfoType) string { return "[REDACTED:" + string(t) + "]" }

// maskValue masks letters and digits, keeping punctuation, spacing and
// length, and keeps the last keep letters or digits (never more than half).
// Email addresses keep the first character of the local part and of each
// domain label, and the top-level domain.
func maskValue(t InfoType, v string, keep int, r rune) string {
	if t == Email {
		if at := strings.LastIndexByte(v, '@'); at > 0 {
			return maskEmail(v[:at], v[at+1:], r)
		}
	}
	n := 0
	for _, c := range v {
		if unicode.IsLetter(c) || unicode.IsDigit(c) {
			n++
		}
	}
	if keep > n/2 {
		keep = n / 2
	}
	var b strings.Builder
	b.Grow(len(v))
	seen := 0
	for _, c := range v {
		if unicode.IsLetter(c) || unicode.IsDigit(c) {
			seen++
			if seen <= n-keep {
				b.WriteRune(r)
				continue
			}
		}
		b.WriteRune(c)
	}
	return b.String()
}

func maskEmail(local, domain string, r rune) string {
	var b strings.Builder
	maskPart := func(s string) {
		for i, c := range s {
			if i == 0 {
				b.WriteRune(c)
			} else {
				b.WriteRune(r)
			}
		}
	}
	maskPart(local)
	b.WriteByte('@')
	labels := strings.Split(domain, ".")
	for i, l := range labels {
		if i > 0 {
			b.WriteByte('.')
		}
		if i == len(labels)-1 && len(labels) > 1 {
			b.WriteString(l)
		} else {
			maskPart(l)
		}
	}
	return b.String()
}
