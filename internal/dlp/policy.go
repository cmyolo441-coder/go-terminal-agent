package dlp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// PolicyVersion is the only policy schema version this package accepts.
const PolicyVersion = 1

// MaxPolicySize bounds the size of a policy document.
const MaxPolicySize = 4 << 20

// Policy configures an Engine. It is usually loaded from JSON with
// ParsePolicy, which rejects unknown fields, duplicate keys, trailing data
// and out-of-range values. The zero value of every optional field selects
// the documented default.
//
// Example:
//
//	{
//	  "version": 1,
//	  "name": "corp-egress",
//	  "min_confidence": 0.6,
//	  "detectors": {
//	    "CREDIT_CARD": {"action": "block"},
//	    "EMAIL":       {"action": "tokenize"},
//	    "IP_ADDRESS":  {"enabled": false}
//	  },
//	  "custom_regex": [{
//	    "name": "EMPLOYEE_ID", "pattern": "\\bEMP-\\d{6}\\b",
//	    "label": "Confidential", "frameworks": ["GDPR"], "action": "mask"
//	  }],
//	  "dictionaries": [{
//	    "name": "PROJECT_CODENAME", "terms": ["bluebird", "nightjar"],
//	    "label": "Restricted", "action": "redact"
//	  }]
//	}
type Policy struct {
	Version int `json:"version"`
	// Name identifies the policy in reports and block explanations.
	Name string `json:"name,omitempty"`
	// MinConfidence drops findings below it (default 0.5).
	MinConfidence *float64 `json:"min_confidence,omitempty"`
	// ContextWindow is how many bytes around a value are searched for
	// context keywords (default 64, at most 1024).
	ContextWindow int `json:"context_window,omitempty"`
	// DefaultAction, when set, replaces the built-in default action of every
	// detector that does not name its own.
	DefaultAction Action `json:"default_action,omitempty"`
	// MaskChar replaces masked letters and digits (default "*").
	MaskChar string `json:"mask_char,omitempty"`
	// KeepLast is how many trailing letters or digits masking keeps
	// (default 4, at most 8). Masking never reveals more than half.
	KeepLast *int `json:"keep_last,omitempty"`
	// BlockScore blocks a payload whose document score reaches it (1-100;
	// 0 disables).
	BlockScore float64 `json:"block_score,omitempty"`
	// BlockLabel blocks a payload whose document label reaches it.
	BlockLabel *Label `json:"block_label,omitempty"`
	// Detectors overrides the built-in detectors, keyed by InfoType.
	Detectors map[InfoType]DetectorPolicy `json:"detectors,omitempty"`
	// IP configures IP address detection.
	IP IPPolicy `json:"ip"`
	// AllowValues are exact values that are never reported.
	AllowValues []string `json:"allow_values,omitempty"`
	// AllowEmailDomains are domains whose addresses are never reported
	// (subdomains included).
	AllowEmailDomains []string `json:"allow_email_domains,omitempty"`
	// CustomRegex adds regular-expression detectors.
	CustomRegex []RegexDetector `json:"custom_regex,omitempty"`
	// Dictionaries adds exact-term detectors matched with Aho–Corasick.
	Dictionaries []Dictionary `json:"dictionaries,omitempty"`
}

// DetectorPolicy overrides one built-in detector.
type DetectorPolicy struct {
	// Enabled defaults to true.
	Enabled       *bool       `json:"enabled,omitempty"`
	Action        Action      `json:"action,omitempty"`
	MinConfidence *float64    `json:"min_confidence,omitempty"`
	Label         *Label      `json:"label,omitempty"`
	Frameworks    []Framework `json:"frameworks,omitempty"`
	KeepLast      *int        `json:"keep_last,omitempty"`
}

// IPPolicy configures IP address detection.
type IPPolicy struct {
	// IncludePrivate also reports loopback, private, link-local,
	// unspecified and multicast addresses.
	IncludePrivate bool `json:"include_private,omitempty"`
	// DisableV4 and DisableV6 turn off one address family.
	DisableV4 bool `json:"disable_v4,omitempty"`
	DisableV6 bool `json:"disable_v6,omitempty"`
}

// RegexDetector is a custom detector. The pattern uses Go RE2 syntax and is
// compiled once when the Engine is built.
type RegexDetector struct {
	Name    InfoType `json:"name"`
	Pattern string   `json:"pattern"`
	// Group selects the capture group that is the sensitive value (0 =
	// whole match).
	Group int `json:"group,omitempty"`
	// Keywords, when set, restrict matching to the context window after
	// one of them, which is both more precise and much faster.
	Keywords   []string    `json:"keywords,omitempty"`
	Confidence float64     `json:"confidence,omitempty"`
	Label      *Label      `json:"label,omitempty"`
	Frameworks []Framework `json:"frameworks,omitempty"`
	Action     Action      `json:"action,omitempty"`
	KeepLast   *int        `json:"keep_last,omitempty"`
}

// Dictionary is a custom exact-term detector.
type Dictionary struct {
	Name  InfoType `json:"name"`
	Terms []string `json:"terms"`
	// CaseSensitive requires exact case; otherwise ASCII case is ignored.
	CaseSensitive bool `json:"case_sensitive,omitempty"`
	// WholeWord defaults to true: a term must not be glued to letters,
	// digits or underscores.
	WholeWord  *bool       `json:"whole_word,omitempty"`
	Confidence float64     `json:"confidence,omitempty"`
	Label      *Label      `json:"label,omitempty"`
	Frameworks []Framework `json:"frameworks,omitempty"`
	Action     Action      `json:"action,omitempty"`
	KeepLast   *int        `json:"keep_last,omitempty"`
}

// Limits on custom configuration.
const (
	maxCustomRegex   = 256
	maxDictTerms     = 1 << 20
	maxTermLen       = 256
	maxKeywords      = 256
	maxContextWindow = 1024
	// MaxMatchLen is the longest value a custom regex may report; longer
	// matches are ignored so streaming stays bounded.
	MaxMatchLen = 512
)

var customNamePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,39}$`)

// DefaultPolicy returns the built-in policy: every detector enabled with
// its default action and a 0.5 confidence floor.
func DefaultPolicy() *Policy {
	return &Policy{Version: PolicyVersion, Name: "default"}
}

// ParsePolicy decodes and validates a JSON policy.
func ParsePolicy(data []byte) (*Policy, error) {
	if len(data) > MaxPolicySize {
		return nil, fmt.Errorf("dlp: policy is %d bytes, larger than %d", len(data), MaxPolicySize)
	}
	if !utf8.Valid(data) {
		return nil, errors.New("dlp: policy is not valid UTF-8")
	}
	if err := checkDuplicateKeys(data); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var p Policy
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("dlp: policy: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("dlp: policy: unexpected data after the JSON object")
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// LoadPolicyFile reads and validates a JSON policy file.
func LoadPolicyFile(path string) (*Policy, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("dlp: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxPolicySize+1))
	if err != nil {
		return nil, fmt.Errorf("dlp: reading %s: %w", path, err)
	}
	p, err := ParsePolicy(data)
	if err != nil {
		return nil, fmt.Errorf("%w (in %s)", err, path)
	}
	return p, nil
}

// checkDuplicateKeys walks the JSON token stream and rejects any object
// with a repeated key, which encoding/json would otherwise resolve silently
// in favour of the last one.
func checkDuplicateKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var walk func(path string, depth int) error
	walk = func(path string, depth int) error {
		if depth > 64 {
			return errors.New("dlp: policy nests too deeply")
		}
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("dlp: policy: %w", err)
		}
		d, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch d {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return fmt.Errorf("dlp: policy: %w", err)
				}
				k, _ := kt.(string)
				if seen[k] {
					return fmt.Errorf("dlp: policy: duplicate key %q in %s", k, path)
				}
				seen[k] = true
				if err := walk(path+"."+k, depth+1); err != nil {
					return err
				}
			}
		case '[':
			for i := 0; dec.More(); i++ {
				if err := walk(fmt.Sprintf("%s[%d]", path, i), depth+1); err != nil {
					return err
				}
			}
		}
		_, err = dec.Token() // closing delimiter
		if err != nil {
			return fmt.Errorf("dlp: policy: %w", err)
		}
		return nil
	}
	return walk("$", 0)
}

// Validate checks the policy and returns every problem found, joined.
func (p *Policy) Validate() error {
	var errs []error
	bad := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("dlp: policy: "+format, args...))
	}
	if p.Version != PolicyVersion {
		bad("version is %d, want %d", p.Version, PolicyVersion)
	}
	if len(p.Name) > 128 {
		bad("name is longer than 128 bytes")
	}
	checkConf := func(where string, v *float64) {
		if v != nil && (math.IsNaN(*v) || *v < 0 || *v > 1) {
			bad("%s: min_confidence %v is outside [0, 1]", where, *v)
		}
	}
	checkKeep := func(where string, v *int) {
		if v != nil && (*v < 0 || *v > 8) {
			bad("%s: keep_last %d is outside [0, 8]", where, *v)
		}
	}
	checkAction := func(where string, a Action) {
		if a != "" && !a.valid() {
			bad("%s: unknown action %q", where, a)
		}
	}
	checkFrameworks := func(where string, fs []Framework) {
		seen := map[Framework]bool{}
		for _, f := range fs {
			if !validFramework(f) {
				bad("%s: invalid framework %q", where, f)
			}
			if seen[f] {
				bad("%s: framework %q listed twice", where, f)
			}
			seen[f] = true
		}
	}
	checkLabel := func(where string, l *Label) {
		if l != nil && int(*l) >= len(labelNames) {
			bad("%s: invalid label %d", where, uint8(*l))
		}
	}
	checkConf("policy", p.MinConfidence)
	checkKeep("policy", p.KeepLast)
	checkAction("default_action", p.DefaultAction)
	checkLabel("block_label", p.BlockLabel)
	if p.ContextWindow < 0 || p.ContextWindow > maxContextWindow {
		bad("context_window %d is outside [0, %d]", p.ContextWindow, maxContextWindow)
	}
	if p.MaskChar != "" {
		r, size := utf8.DecodeRuneInString(p.MaskChar)
		if size != len(p.MaskChar) || r == utf8.RuneError || r <= ' ' || r == 0x7f {
			bad("mask_char %q must be one printable, non-space character", p.MaskChar)
		}
	}
	if math.IsNaN(p.BlockScore) || p.BlockScore < 0 || p.BlockScore > 100 {
		bad("block_score %v is outside [0, 100]", p.BlockScore)
	}
	keys := make([]string, 0, len(p.Detectors))
	for t := range p.Detectors {
		keys = append(keys, string(t))
	}
	sort.Strings(keys)
	for _, k := range keys {
		t := InfoType(k)
		d := p.Detectors[t]
		where := "detectors." + k
		if _, ok := builtinID(t); !ok {
			bad("%s: unknown detector (built-ins: %s)", where, joinTypes(BuiltinTypes()))
			continue
		}
		checkAction(where, d.Action)
		checkConf(where, d.MinConfidence)
		checkKeep(where, d.KeepLast)
		checkLabel(where, d.Label)
		checkFrameworks(where, d.Frameworks)
	}
	if p.IP.DisableV4 && p.IP.DisableV6 {
		if d, ok := p.Detectors[IPAddress]; !ok || d.Enabled == nil || *d.Enabled {
			bad("ip: both address families disabled; disable the IP_ADDRESS detector instead")
		}
	}
	for i, v := range p.AllowValues {
		if v == "" {
			bad("allow_values[%d] is empty", i)
		}
	}
	for i, d := range p.AllowEmailDomains {
		if d == "" || strings.ContainsAny(d, "@ \t/") || strings.HasPrefix(d, ".") {
			bad("allow_email_domains[%d] %q is not a domain", i, d)
		}
	}
	names := map[InfoType]string{}
	claim := func(where string, n InfoType) {
		if !customNamePattern.MatchString(string(n)) {
			bad("%s: name %q must match %s", where, n, customNamePattern)
			return
		}
		if _, ok := builtinID(n); ok {
			bad("%s: name %q is a built-in detector", where, n)
			return
		}
		if prev, ok := names[n]; ok {
			bad("%s: name %q already used by %s", where, n, prev)
			return
		}
		names[n] = where
	}
	checkKeywords := func(where string, kws []string) {
		if len(kws) > maxKeywords {
			bad("%s: more than %d keywords", where, maxKeywords)
		}
		for j, k := range kws {
			if strings.TrimSpace(k) == "" || len(k) > maxTermLen {
				bad("%s: keywords[%d] is empty or longer than %d bytes", where, j, maxTermLen)
			}
		}
	}
	checkCustomConf := func(where string, c float64) {
		if math.IsNaN(c) || c < 0 || c > 1 {
			bad("%s: confidence %v is outside (0, 1]", where, c)
		}
	}
	if len(p.CustomRegex) > maxCustomRegex {
		bad("more than %d custom_regex detectors", maxCustomRegex)
	}
	for i, r := range p.CustomRegex {
		where := fmt.Sprintf("custom_regex[%d]", i)
		claim(where, r.Name)
		re, err := regexp.Compile(r.Pattern)
		switch {
		case r.Pattern == "":
			bad("%s: empty pattern", where)
		case err != nil:
			bad("%s: %v", where, err)
		case re.MatchString(""):
			bad("%s: pattern matches the empty string", where)
		case r.Group < 0 || r.Group > re.NumSubexp():
			bad("%s: group %d does not exist (pattern has %d)", where, r.Group, re.NumSubexp())
		}
		checkKeywords(where, r.Keywords)
		checkCustomConf(where, r.Confidence)
		checkLabel(where, r.Label)
		checkFrameworks(where, r.Frameworks)
		checkAction(where, r.Action)
		checkKeep(where, r.KeepLast)
	}
	total := 0
	for i, d := range p.Dictionaries {
		where := fmt.Sprintf("dictionaries[%d]", i)
		claim(where, d.Name)
		if len(d.Terms) == 0 {
			bad("%s: no terms", where)
		}
		total += len(d.Terms)
		for j, t := range d.Terms {
			if strings.TrimSpace(t) == "" || len(t) > maxTermLen {
				bad("%s: terms[%d] is empty or longer than %d bytes", where, j, maxTermLen)
			}
		}
		checkCustomConf(where, d.Confidence)
		checkLabel(where, d.Label)
		checkFrameworks(where, d.Frameworks)
		checkAction(where, d.Action)
		checkKeep(where, d.KeepLast)
	}
	if total > maxDictTerms {
		bad("dictionaries hold %d terms, more than %d", total, maxDictTerms)
	}
	return errors.Join(errs...)
}

func joinTypes(ts []InfoType) string {
	s := make([]string, len(ts))
	for i, t := range ts {
		s[i] = string(t)
	}
	return strings.Join(s, ", ")
}
