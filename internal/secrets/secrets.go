// Package secrets finds credentials in text and replaces them with stable
// placeholders before the text reaches a model, a log or an audit record.
//
// Detection combines provider-specific rules (AWS, GitHub, GitLab, Slack,
// Stripe, Google, Anthropic, OpenAI, NVIDIA, JWTs, private keys, connection
// strings, password=/api_key= assignments) with a Shannon-entropy check for
// random-looking tokens. Each rule runs only near its keywords, so scanning a
// megabyte of ordinary source code takes a few milliseconds.
//
// A secret is replaced by a placeholder such as
// [REDACTED:github-token:3f2a]. The four hex digits are a keyed fingerprint
// of the secret: the same secret gets the same placeholder for the life of a
// Redactor, so a model can still tell two credentials apart and refer to
// them, but the fingerprint is too short and too strongly keyed to confirm a
// guess of the secret. Findings never carry the secret itself.
package secrets

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Finding describes one detected secret without revealing it.
type Finding struct {
	// RuleID is the rule that matched, e.g. "aws-access-key-id".
	RuleID string `json:"rule"`
	// Line and Column are 1-based; Column counts bytes.
	Line   int `json:"line"`
	Column int `json:"column"`
	// Start and End are byte offsets of the secret in the scanned text.
	Start int `json:"start"`
	End   int `json:"end"`
	// Fingerprint is four hex digits of a keyed hash of the secret.
	Fingerprint string `json:"fingerprint"`
	// Placeholder is the text that replaces the secret.
	Placeholder string `json:"placeholder"`
}

// String formats a finding for logs.
func (f Finding) String() string {
	return fmt.Sprintf("%s at %d:%d (%s)", f.RuleID, f.Line, f.Column, f.Fingerprint)
}

// Options configures a Redactor. The zero value uses the built-in rules, the
// entropy check and a random per-process fingerprint key.
type Options struct {
	// Key keys the fingerprints. Use the same key to get the same
	// placeholders across processes; leave it empty for a random key.
	Key []byte
	// Rules replaces the built-in rules when non-nil.
	Rules []Rule
	// ExtraRules are added after the built-in rules (for example an
	// organization's own token formats).
	ExtraRules []Rule
	// Disabled lists rule IDs to skip, including RuleHighEntropy.
	Disabled []string
	// AllowValues are exact strings that are never redacted, such as a
	// documented public test key.
	AllowValues []string
	// DisableEntropy turns off the high-entropy check.
	DisableEntropy bool
	// EntropyMinLength is the shortest token the entropy check considers
	// (default 20).
	EntropyMinLength int
}

// Redactor detects and redacts secrets. It is safe for concurrent use.
type Redactor struct {
	rules       []compiledRule
	key         []byte
	allow       map[string]bool
	entropy     bool
	entropyMin  int
	entropyMax  int
	disabledIDs map[string]bool
	// kwIndex maps the first two bytes of a keyword to 1 + its bucket.
	kwIndex   []uint16
	kwBuckets [][]kwRef
	hmacs     sync.Pool
}

type kwRef struct {
	kw   string
	rule int
}

type compiledRule struct {
	Rule
	kws      []string
	priority int
}

var ruleIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)

// New builds a Redactor.
func New(opts Options) (*Redactor, error) {
	r := &Redactor{
		allow:       map[string]bool{},
		entropy:     !opts.DisableEntropy,
		entropyMin:  opts.EntropyMinLength,
		entropyMax:  512,
		disabledIDs: map[string]bool{},
	}
	if r.entropyMin <= 0 {
		r.entropyMin = 20
	}
	for _, id := range opts.Disabled {
		r.disabledIDs[id] = true
	}
	if r.disabledIDs[RuleHighEntropy] {
		r.entropy = false
	}
	for _, v := range opts.AllowValues {
		if v != "" {
			r.allow[v] = true
		}
	}
	rules := opts.Rules
	if rules == nil {
		rules = BuiltinRules()
	}
	rules = append(append([]Rule{}, rules...), opts.ExtraRules...)
	seen := map[string]bool{}
	for i, rule := range rules {
		if !ruleIDPattern.MatchString(rule.ID) {
			return nil, fmt.Errorf("secrets: rule %d: invalid id %q", i, rule.ID)
		}
		if rule.Pattern == nil {
			return nil, fmt.Errorf("secrets: rule %s: no pattern", rule.ID)
		}
		if rule.SecretGroup < 0 || rule.SecretGroup > rule.Pattern.NumSubexp() {
			return nil, fmt.Errorf("secrets: rule %s: secret group %d out of range", rule.ID, rule.SecretGroup)
		}
		if seen[rule.ID] {
			return nil, fmt.Errorf("secrets: duplicate rule id %q", rule.ID)
		}
		seen[rule.ID] = true
		if r.disabledIDs[rule.ID] {
			continue
		}
		cr := compiledRule{Rule: rule, priority: i}
		for _, kw := range rule.Keywords {
			if len(kw) < 2 {
				return nil, fmt.Errorf("secrets: rule %s: keyword %q is shorter than 2 bytes", rule.ID, kw)
			}
			cr.kws = append(cr.kws, asciiLower(kw))
		}
		r.rules = append(r.rules, cr)
	}
	r.kwIndex = make([]uint16, 1<<16)
	for ri, cr := range r.rules {
		for _, kw := range cr.kws {
			h := uint16(kw[0])<<8 | uint16(kw[1])
			if r.kwIndex[h] == 0 {
				r.kwBuckets = append(r.kwBuckets, nil)
				r.kwIndex[h] = uint16(len(r.kwBuckets))
			}
			b := r.kwIndex[h] - 1
			r.kwBuckets[b] = append(r.kwBuckets[b], kwRef{kw: kw, rule: ri})
		}
	}
	r.key = opts.Key
	if len(r.key) == 0 {
		r.key = make([]byte, 32)
		if _, err := rand.Read(r.key); err != nil {
			return nil, fmt.Errorf("secrets: fingerprint key: %w", err)
		}
	}
	key := r.key
	r.hmacs.New = func() any { return hmac.New(sha256.New, key) }
	return r, nil
}

var (
	defaultOnce sync.Once
	defaultRed  *Redactor
)

// Default returns a shared Redactor with the built-in rules and a random
// per-process fingerprint key.
func Default() *Redactor {
	defaultOnce.Do(func() {
		r, err := New(Options{})
		if err != nil {
			panic("secrets: built-in rules are invalid: " + err.Error())
		}
		defaultRed = r
	})
	return defaultRed
}

// Redact replaces secrets in text using the default Redactor.
func Redact(text string) (string, []Finding) { return Default().Redact(text) }

// Rules lists the IDs of the active rules, plus RuleHighEntropy when the
// entropy check is on.
func (r *Redactor) Rules() []string {
	out := make([]string, 0, len(r.rules)+1)
	for _, c := range r.rules {
		out = append(out, c.ID)
	}
	if r.entropy {
		out = append(out, RuleHighEntropy)
	}
	return out
}

// Redact returns text with every detected secret replaced by its
// placeholder, and the findings in text order.
func (r *Redactor) Redact(text string) (string, []Finding) {
	fs := r.Scan(text)
	if len(fs) == 0 {
		return text, nil
	}
	var b strings.Builder
	b.Grow(len(text))
	last := 0
	for _, f := range fs {
		b.WriteString(text[last:f.Start])
		b.WriteString(f.Placeholder)
		last = f.End
	}
	b.WriteString(text[last:])
	return b.String(), fs
}

// Contains reports whether text holds at least one secret.
func (r *Redactor) Contains(text string) bool { return len(r.Scan(text)) > 0 }

// Placeholder returns the placeholder that would replace secret under rule.
func (r *Redactor) Placeholder(rule, secret string) string {
	return "[REDACTED:" + rule + ":" + r.fingerprint(secret) + "]"
}

func (r *Redactor) fingerprint(secret string) string {
	m := r.hmacs.Get().(hash.Hash)
	m.Reset()
	io.WriteString(m, secret)
	var sum [sha256.Size]byte
	out := hex.EncodeToString(m.Sum(sum[:0])[:2])
	r.hmacs.Put(m)
	return out
}

type span struct {
	start, end int
	rule       string
	priority   int
}

// Scan finds secrets without changing text. Overlapping matches are
// resolved in favour of the more specific rule.
func (r *Redactor) Scan(text string) []Finding {
	if len(text) == 0 {
		return nil
	}
	lower := asciiLower(text)
	occ := r.findKeywords(lower)
	var spans []span
	for i := range r.rules {
		if len(r.rules[i].kws) > 0 && len(occ[i]) == 0 {
			continue
		}
		spans = r.scanRule(&r.rules[i], text, occ[i], spans)
	}
	if r.entropy {
		spans = r.scanEntropy(text, lower, spans)
	}
	if len(spans) == 0 {
		return nil
	}
	// Most specific first, then earliest, then longest.
	sort.Slice(spans, func(i, j int) bool {
		a, b := spans[i], spans[j]
		if a.priority != b.priority {
			return a.priority < b.priority
		}
		if a.start != b.start {
			return a.start < b.start
		}
		return a.end > b.end
	})
	// A bitmap of claimed bytes keeps overlap resolution linear in the
	// total span length, even for inputs with thousands of findings.
	claimed := make([]uint64, (len(text)+63)/64)
	free := func(s span) bool {
		for i := s.start; i < s.end; i++ {
			if claimed[i>>6]&(1<<(uint(i)&63)) != 0 {
				return false
			}
		}
		return true
	}
	var kept []span
	for _, s := range spans {
		if r.allow[text[s.start:s.end]] || !free(s) {
			continue
		}
		for i := s.start; i < s.end; i++ {
			claimed[i>>6] |= 1 << (uint(i) & 63)
		}
		kept = append(kept, s)
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].start < kept[j].start })
	out := make([]Finding, 0, len(kept))
	line, lineStart, pos := 1, 0, 0
	for _, s := range kept {
		for pos < s.start {
			nl := strings.IndexByte(text[pos:s.start], '\n')
			if nl < 0 {
				pos = s.start
				break
			}
			pos += nl + 1
			line++
			lineStart = pos
		}
		secret := text[s.start:s.end]
		out = append(out, Finding{
			RuleID:      s.rule,
			Line:        line,
			Column:      s.start - lineStart + 1,
			Start:       s.start,
			End:         s.end,
			Fingerprint: r.fingerprint(secret),
			Placeholder: r.Placeholder(s.rule, secret),
		})
	}
	return out
}

const (
	windowReach    = 2048
	multilineReach = 128 << 10
)

// scanRule runs one rule over the windows of text around its keywords.
// findKeywords finds every keyword of every rule in one pass over the
// lower-cased text, using a table indexed by the first two bytes. It
// returns, per rule, the sorted keyword positions.
func (r *Redactor) findKeywords(lower string) [][]int {
	occ := make([][]int, len(r.rules))
	for i := 0; i+1 < len(lower); i++ {
		b := r.kwIndex[uint16(lower[i])<<8|uint16(lower[i+1])]
		if b == 0 {
			continue
		}
		for _, k := range r.kwBuckets[b-1] {
			if strings.HasPrefix(lower[i:], k.kw) {
				o := occ[k.rule]
				// Several keywords of one rule can start at the same byte.
				if n := len(o); n == 0 || o[n-1] != i {
					occ[k.rule] = append(o, i)
				}
			}
		}
	}
	return occ
}

func (r *Redactor) scanRule(rule *compiledRule, text string, occ []int, spans []span) []span {
	type window struct{ start, end int }
	var wins []window
	if len(rule.kws) == 0 {
		wins = []window{{0, len(text)}}
	} else {
		if rule.keyAnchored {
			return r.scanKeyAnchored(rule, text, occ, spans)
		}
		for _, p := range occ {
			ws := strings.LastIndexByte(text[max(0, p-windowReach):p], '\n')
			if ws < 0 {
				ws = max(0, p-windowReach)
			} else {
				ws += max(0, p-windowReach) + 1
			}
			reach := windowReach
			if rule.Multiline {
				reach = multilineReach
			}
			limit := min(len(text), p+reach)
			we := limit
			if !rule.Multiline {
				if nl := strings.IndexByte(text[p:limit], '\n'); nl >= 0 {
					we = p + nl
				}
			}
			if n := len(wins); n > 0 && ws <= wins[n-1].end {
				wins[n-1].end = max(wins[n-1].end, we)
				continue
			}
			wins = append(wins, window{ws, we})
		}
	}
	for _, w := range wins {
		s := text[w.start:w.end]
		for _, m := range rule.Pattern.FindAllStringSubmatchIndex(s, -1) {
			var start, end int
			if rule.extract != nil {
				var ok bool
				if start, end, ok = rule.extract(s, m); !ok {
					continue
				}
			} else {
				g := rule.SecretGroup
				if m[2*g] < 0 {
					continue
				}
				start, end = m[2*g], m[2*g+1]
			}
			if end-start < max(rule.MinLength, 1) || isDocExample(s[start:end]) {
				continue
			}
			if rule.boundary {
				if (start > 0 && isWordByte(s[start-1])) || (end < len(s) && isWordByte(s[end])) {
					continue
				}
			}
			spans = append(spans, span{start: w.start + start, end: w.start + end, rule: rule.ID, priority: rule.priority})
		}
	}
	return spans
}

func isKeyByte(c byte) bool { return isWordByte(c) || c == '_' || c == '.' || c == '-' }

// scanKeyAnchored expands each keyword occurrence to the surrounding name,
// and parses an assignment only where the name is a credential name.
func (r *Redactor) scanKeyAnchored(rule *compiledRule, text string, occ []int, spans []span) []span {
	lastKey := -1
	for _, p := range occ {
		ks := p
		for ks > 0 && p-ks < 80 && isKeyByte(text[ks-1]) {
			ks--
		}
		if ks <= lastKey {
			continue
		}
		ke := p
		for ke < len(text) && ke-ks < 80 && isKeyByte(text[ke]) {
			ke++
		}
		if !IsSecretKey(text[ks:ke]) {
			continue
		}
		lastKey = ks
		// Skip opening quotes so `"password": ...` anchors at the name.
		end := min(len(text), ks+700)
		if nl := strings.IndexByte(text[ks:end], '\n'); nl >= 0 {
			end = ks + nl
		}
		s := text[ks:end]
		m := parseAssignment(s)
		if m == nil {
			continue
		}
		start, stop, ok := rule.extract(s, m)
		if !ok || stop-start < max(rule.MinLength, 1) || isDocExample(s[start:stop]) {
			continue
		}
		spans = append(spans, span{start: ks + start, end: ks + stop, rule: rule.ID, priority: rule.priority})
	}
	return spans
}

// entropyPriority ranks entropy findings after every rule.
const entropyPriority = 1 << 30

func isTokenByte(c byte) bool {
	return isWordByte(c) || c == '+' || c == '/' || c == '=' || c == '_' || c == '-'
}

// scanEntropy flags random-looking tokens that no rule recognised.
func (r *Redactor) scanEntropy(text, lower string, spans []span) []span {
	n := len(text)
	public := publicBlocks(lower)
	for i := 0; i < n; {
		if !isTokenByte(text[i]) {
			i++
			continue
		}
		j := i
		for j < n && isTokenByte(text[j]) {
			j++
		}
		start, end := i, j
		rawStart := i
		i = j
		// Trim separators and base64 padding at the edges.
		for start < end && (text[start] == '-' || text[start] == '_' || text[start] == '/' || text[start] == '+' || text[start] == '=') {
			start++
		}
		for end > start && (text[end-1] == '-' || text[end-1] == '_' || text[end-1] == '/' || text[end-1] == '=') {
			end--
		}
		if end-start < r.entropyMin || end-start > r.entropyMax {
			continue
		}
		tok := text[start:end]
		if isBlobLine(text, rawStart, j) || !looksRandom(tok) || allowlistedContext(text, lower, start) || inBlocks(public, start) {
			continue
		}
		spans = append(spans, span{start: start, end: end, rule: RuleHighEntropy, priority: entropyPriority})
	}
	return spans
}

// publicBlocks returns the spans of certificates and public keys, whose
// base64 bodies are public.
func publicBlocks(lower string) [][2]int {
	var out [][2]int
	for _, begin := range []string{"-----begin certificate-----", "-----begin public key-----", "-----begin rsa public key-----",
		"-----begin pgp public key block-----", "-----begin certificate request-----", "-----begin x509 crl-----"} {
		for from := 0; ; {
			k := strings.Index(lower[from:], begin)
			if k < 0 {
				break
			}
			start := from + k
			end := min(len(lower), start+multilineReach)
			if e := strings.Index(lower[start+len(begin):end], "-----end "); e >= 0 {
				end = start + len(begin) + e
			}
			out = append(out, [2]int{start, end})
			from = end
		}
	}
	return out
}

func inBlocks(blocks [][2]int, pos int) bool {
	for _, b := range blocks {
		if pos >= b[0] && pos < b[1] {
			return true
		}
	}
	return false
}

// isBlobLine reports whether the token text[start:end] fills its line and
// a neighbouring line is also a long base64 line: an encoded file or
// certificate body, not a credential.
func isBlobLine(text string, start, end int) bool {
	if (start > 0 && text[start-1] != '\n') || (end < len(text) && text[end] != '\n' && text[end] != '\r') {
		return false
	}
	full := func(a, b int) bool {
		if b-a < 40 {
			return false
		}
		for k := a; k < b; k++ {
			if !isTokenByte(text[k]) {
				return false
			}
		}
		return true
	}
	if start > 1 {
		ps := strings.LastIndexByte(text[:start-1], '\n') + 1
		pe := start - 1
		if pe > ps && text[pe-1] == '\r' {
			pe--
		}
		if full(ps, pe) {
			return true
		}
	}
	if end < len(text) {
		ns := end + 1
		if text[end] == '\r' && ns < len(text) && text[ns] == '\n' {
			ns++
		}
		ne := len(text)
		if k := strings.IndexByte(text[ns:], '\n'); k >= 0 {
			ne = ns + k
		}
		if ne > ns && text[ne-1] == '\r' {
			ne--
		}
		// The last line of a blob may be short.
		if full(ns, ne) {
			return true
		}
	}
	return false
}

// hashPrefixes mark digests and checksums, which are random but public.
var hashPrefixes = []string{
	"sha1:", "sha1-", "sha224:", "sha256:", "sha256-", "sha384:", "sha384-", "sha512:", "sha512-",
	"md5:", "md5-", "h1:", "blake2b:", "integrity\": \"", "integrity=\"", "base64,", "digest:", "@sha256:",
	"checksum:", "checksum=", "commit ", "commit:", "sum:", "nonce-",
	"ssh-rsa ", "ssh-ed25519 ", "ssh-dss ", "ecdsa-sha2-nistp256 ", "ecdsa-sha2-nistp384 ", "ecdsa-sha2-nistp521 ",
}

// allowlistedContext reports whether the token at start follows a marker of
// a digest, an inline image or another public random value.
func allowlistedContext(text, lower string, start int) bool {
	from := max(0, start-24)
	before := lower[from:start]
	for _, p := range hashPrefixes {
		if strings.HasSuffix(before, p) {
			return true
		}
	}
	// data:image/png;base64,.... and similar inline payloads.
	if i := strings.LastIndex(lower[max(0, start-80):start], "data:"); i >= 0 && strings.HasSuffix(before, ";base64,") {
		return true
	}
	return false
}

// looksRandom applies the entropy test with allowances for hex digests,
// UUIDs, paths and identifiers.
func looksRandom(tok string) bool {
	var upper, lowerN, digit, other, hexOnly int
	hexOnly = 1
	for i := 0; i < len(tok); i++ {
		c := tok[i]
		switch {
		case c >= 'A' && c <= 'Z':
			upper++
			if c > 'F' {
				hexOnly = 0
			}
		case c >= 'a' && c <= 'z':
			lowerN++
			if c > 'f' {
				hexOnly = 0
			}
		case c >= '0' && c <= '9':
			digit++
		case c == '-':
			other++
		default:
			other++
			hexOnly = 0
		}
	}
	// Git SHAs, digests and UUIDs: hex (with dashes) is never flagged here;
	// only context rules catch hex secrets.
	if hexOnly == 1 {
		return false
	}
	if upper == 0 || lowerN == 0 || digit < 2 {
		return false
	}
	letters := upper + lowerN
	if float64(digit) < 0.08*float64(len(tok)) || letters < len(tok)/3 {
		return false
	}
	if isPathLike(tok) || isWordy(tok) {
		return false
	}
	return shannon(tok) >= entropyThreshold(len(tok))
}

// entropyThreshold scales with length: short random strings cannot reach
// high entropy, long ones do.
func entropyThreshold(n int) float64 {
	t := math.Log2(float64(n)) - 0.7
	return math.Max(3.5, math.Min(4.5, t))
}

// shannon returns the Shannon entropy of s in bits per byte.
func shannon(s string) float64 {
	var counts [256]int
	for i := 0; i < len(s); i++ {
		counts[s[i]]++
	}
	n := float64(len(s))
	h := 0.0
	for _, c := range counts {
		if c > 0 {
			p := float64(c) / n
			h -= p * math.Log2(p)
		}
	}
	return h
}

// isPathLike reports slash-separated names such as module paths.
func isPathLike(tok string) bool {
	if strings.Count(tok, "/") < 2 || strings.ContainsAny(tok, "+=") {
		return false
	}
	for _, seg := range strings.Split(tok, "/") {
		if len(seg) > 32 || (seg != "" && hasUpper(seg) && hasLower(seg) && hasDigit(seg)) {
			return false
		}
	}
	return true
}

// isWordy reports tokens made mostly of lower-case runs, such as
// camelCase identifiers with a version number.
func isWordy(tok string) bool {
	inRun, run, total := false, 0, 0
	flush := func() {
		if inRun && run >= 4 {
			total += run
		}
		inRun, run = false, 0
	}
	for i := 0; i < len(tok); i++ {
		c := tok[i]
		if c >= 'a' && c <= 'z' {
			if !inRun && i > 0 && tok[i-1] >= 'A' && tok[i-1] <= 'Z' {
				// Count the capital that starts a camelCase word.
				run = 1
			}
			inRun = true
			run++
			continue
		}
		flush()
	}
	flush()
	return float64(total) > 0.45*float64(len(tok))
}
