package secrets

import (
	"regexp"
	"strings"
)

// Rule detects one kind of secret.
//
// A rule's Pattern runs only on windows of text around an occurrence of one
// of its Keywords (matched case-insensitively), which keeps scanning fast on
// large inputs. A rule with no keywords runs over the whole text.
type Rule struct {
	// ID names the rule in findings and placeholders, e.g. "github-token".
	// It must be lower case letters, digits and dashes.
	ID string
	// Description says what the rule finds.
	Description string
	// Keywords are ASCII strings, at least one of which must appear near a
	// match. They are compared case-insensitively.
	Keywords []string
	// Pattern finds candidates.
	Pattern *regexp.Regexp
	// SecretGroup is the submatch that holds the secret; 0 means the whole
	// match.
	SecretGroup int
	// MinLength rejects shorter secrets.
	MinLength int
	// Multiline lets a match span lines (for example key blocks).
	Multiline bool

	// extract, when set, picks the secret span out of a match and may reject
	// it. m holds submatch indexes relative to s.
	extract func(s string, m []int) (start, end int, ok bool)
	// boundary requires the secret not to be glued to other word characters.
	boundary bool
	// keyAnchored rules start at a variable name around a keyword
	// occurrence; the name is checked with IsSecretKey before the rest of the
	// line is parsed as an assignment (see parseAssignment).
	keyAnchored bool
}

// Built-in rule IDs.
const (
	RuleAWSAccessKey     = "aws-access-key-id"
	RuleAWSSecretKey     = "aws-secret-access-key"
	RuleGitHubToken      = "github-token"
	RuleGitLabToken      = "gitlab-token"
	RuleSlackToken       = "slack-token"
	RuleSlackWebhook     = "slack-webhook"
	RuleStripeKey        = "stripe-key"
	RuleGoogleAPIKey     = "google-api-key"
	RuleAnthropicKey     = "anthropic-api-key"
	RuleOpenAIKey        = "openai-api-key"
	RuleNVIDIAKey        = "nvidia-api-key"
	RuleHuggingFaceToken = "huggingface-token"
	RuleNPMToken         = "npm-token"
	RuleSendGridKey      = "sendgrid-api-key"
	RuleAzureStorageKey  = "azure-storage-key"
	RuleJWT              = "jwt"
	RulePrivateKey       = "private-key"
	RuleAuthHeader       = "authorization-header"
	RuleConnString       = "connection-string"
	RuleGenericSecret    = "generic-secret"
	RuleHighEntropy      = "high-entropy-string"
)

// BuiltinRules returns the default detection rules, most specific first.
// The high-entropy check is not a Rule; it is controlled by Options.
func BuiltinRules() []Rule {
	return []Rule{
		{
			ID:          RulePrivateKey,
			Description: "PEM or PGP private key block",
			Keywords:    []string{"private key"},
			Pattern:     regexp.MustCompile(`-----BEGIN[ A-Z0-9]*PRIVATE KEY(?: BLOCK)?-----(?s:.*?)(?:-----END[ A-Z0-9]*PRIVATE KEY(?: BLOCK)?-----|\z)`),
			Multiline:   true,
		},
		{
			ID:          RuleAWSAccessKey,
			Description: "AWS access key ID",
			Keywords:    []string{"akia", "asia", "abia", "acca", "a3t"},
			Pattern:     regexp.MustCompile(`(?:AKIA|ASIA|ABIA|ACCA|A3T[A-Z0-9])[A-Z0-9]{16}`),
			boundary:    true,
		},
		{
			ID:          RuleAWSSecretKey,
			Description: "AWS secret access key assignment",
			Keywords:    []string{"secret_access_key", "secretaccesskey", "secret-access-key", "aws_secret_key", "awssecretkey"},
			Pattern:     regexp.MustCompile(`(?i)(?:aws_?secret_?access_?key|aws-secret-access-key|secret_?access_?key|aws_?secret_?key)["']?\s*(?::=|=>|=|:)\s*["']?([A-Za-z0-9/+]{40})(?:[^A-Za-z0-9/+=]|$)`),
			SecretGroup: 1,
		},
		{
			ID:          RuleGitHubToken,
			Description: "GitHub personal access, OAuth, app or fine-grained token",
			Keywords:    []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_"},
			Pattern:     regexp.MustCompile(`(?:(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36,255}|github_pat_[A-Za-z0-9_]{22,255})`),
			boundary:    true,
		},
		{
			ID:          RuleGitLabToken,
			Description: "GitLab access, trigger, deploy or runner token",
			Keywords:    []string{"glpat-", "glptt-", "gldt-", "glrt-", "glcbt-", "glsoat-", "glft-", "glimt-", "glagent-", "gloas-"},
			Pattern:     regexp.MustCompile(`gl(?:pat|ptt|dt|rt|cbt|soat|ft|imt|agent|oas)-[A-Za-z0-9_\-]{20,255}`),
			boundary:    true,
		},
		{
			ID:          RuleSlackToken,
			Description: "Slack bot, user, app or refresh token",
			Keywords:    []string{"xoxa-", "xoxb-", "xoxp-", "xoxo-", "xoxs-", "xoxr-", "xoxe-", "xapp-"},
			Pattern:     regexp.MustCompile(`(?:xox[abposre](?:\.[a-z0-9]+)?-[A-Za-z0-9-]{10,250}|xapp-\d-[A-Za-z0-9-]{10,250})`),
			boundary:    true,
		},
		{
			ID:          RuleSlackWebhook,
			Description: "Slack incoming webhook URL",
			Keywords:    []string{"hooks.slack.com"},
			Pattern:     regexp.MustCompile(`https://hooks\.slack\.com/(?:services|workflows|triggers)/[A-Za-z0-9_/]{20,200}`),
		},
		{
			ID:          RuleStripeKey,
			Description: "Stripe secret or restricted key",
			Keywords:    []string{"sk_live_", "sk_test_", "rk_live_", "rk_test_"},
			Pattern:     regexp.MustCompile(`(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{16,247}`),
			boundary:    true,
		},
		{
			ID:          RuleGoogleAPIKey,
			Description: "Google API key",
			Keywords:    []string{"aiza"},
			Pattern:     regexp.MustCompile(`AIza[0-9A-Za-z_\-]{35}`),
			boundary:    true,
		},
		{
			ID:          RuleAnthropicKey,
			Description: "Anthropic API key",
			Keywords:    []string{"sk-ant-"},
			Pattern:     regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{32,300}`),
			boundary:    true,
		},
		{
			ID:          RuleOpenAIKey,
			Description: "OpenAI API key",
			Keywords:    []string{"sk-"},
			Pattern:     regexp.MustCompile(`sk-(?:(?:proj|svcacct|admin|None)-[A-Za-z0-9_\-]{20,300}|[A-Za-z0-9]{20}T3BlbkFJ[A-Za-z0-9]{20}|[A-Za-z0-9]{40,64})`),
			boundary:    true,
			extract: func(s string, m []int) (int, int, bool) {
				v := s[m[0]:m[1]]
				if strings.HasPrefix(v, "sk-ant-") {
					return 0, 0, false
				}
				// The legacy form must look random, not like a long word.
				if !strings.Contains(v, "-proj-") && !strings.Contains(v, "-svcacct-") && !strings.Contains(v, "-admin-") && !strings.Contains(v, "-None-") && !strings.Contains(v, "T3BlbkFJ") {
					if !mixedClasses(v[3:]) {
						return 0, 0, false
					}
				}
				return m[0], m[1], true
			},
		},
		{
			ID:          RuleNVIDIAKey,
			Description: "NVIDIA API key",
			Keywords:    []string{"nvapi-"},
			Pattern:     regexp.MustCompile(`nvapi-[A-Za-z0-9_\-]{40,200}`),
			boundary:    true,
		},
		{
			ID:          RuleHuggingFaceToken,
			Description: "Hugging Face access token",
			Keywords:    []string{"hf_"},
			Pattern:     regexp.MustCompile(`hf_[A-Za-z]{34,40}`),
			boundary:    true,
			extract: func(s string, m []int) (int, int, bool) {
				v := s[m[0]+3 : m[1]]
				if !hasUpper(v) || !hasLower(v) {
					return 0, 0, false
				}
				return m[0], m[1], true
			},
		},
		{
			ID:          RuleNPMToken,
			Description: "npm access token",
			Keywords:    []string{"npm_"},
			Pattern:     regexp.MustCompile(`npm_[A-Za-z0-9]{36}`),
			boundary:    true,
		},
		{
			ID:          RuleSendGridKey,
			Description: "SendGrid API key",
			Keywords:    []string{"sg."},
			Pattern:     regexp.MustCompile(`SG\.[A-Za-z0-9_\-]{22}\.[A-Za-z0-9_\-]{43}`),
			boundary:    true,
		},
		{
			ID:          RuleAzureStorageKey,
			Description: "Azure storage account key",
			Keywords:    []string{"accountkey="},
			Pattern:     regexp.MustCompile(`(?i)AccountKey=([A-Za-z0-9+/]{86}==)`),
			SecretGroup: 1,
		},
		{
			ID:          RuleJWT,
			Description: "JSON Web Token",
			Keywords:    []string{"eyj"},
			Pattern:     regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{8,}\.eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`),
			boundary:    true,
		},
		{
			ID:          RuleAuthHeader,
			Description: "credentials in an Authorization header",
			Keywords:    []string{"authorization"},
			Pattern:     regexp.MustCompile(`(?i)authorization["']?\s*[:=]\s*["']?(?:bearer|token|basic|apikey|api-key)\s+([A-Za-z0-9._~+/\-]{8,}=*)`),
			SecretGroup: 1,
			extract: func(s string, m []int) (int, int, bool) {
				if isPlaceholder(s[m[2]:m[3]]) {
					return 0, 0, false
				}
				return m[2], m[3], true
			},
		},
		{
			ID:          RuleConnString,
			Description: "password in a URL or connection string",
			Keywords:    []string{"://"},
			Pattern:     regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.\-]{1,30}://([^:/?#\s@'"<>]{1,128}):([^@/?#\s'"<>]{1,256})@[A-Za-z0-9\[]`),
			SecretGroup: 2,
			extract: func(s string, m []int) (int, int, bool) {
				pw := s[m[4]:m[5]]
				if isPlaceholder(pw) {
					return 0, 0, false
				}
				return m[4], m[5], true
			},
		},
		{
			ID:          RuleGenericSecret,
			Description: "value assigned to a password, secret, token or key",
			Keywords:    []string{"pass", "pwd", "secret", "token", "key", "credential"},
			Pattern:     genericAssign,
			extract:     extractGeneric,
			keyAnchored: true,
		},
	}
}

// genericAssign matches key/value assignments in code, config and env files:
// KEY=value, key: value, key = "value", "key": "value", key := `value`.
// Scanning uses parseAssignment, an equivalent hand-written parser that
// produces the same submatch layout without the cost of the regexp engine.
var genericAssign = regexp.MustCompile(`([A-Za-z0-9_.\-]{2,80})["']?(\s*)(:=|=>|=|:)(\s*)(?:"([^"\r\n]+)"|'([^'\r\n]+)'|` + "`([^`\\r\\n]+)`" + `|([^\s"'` + "`" + `,;)}\]]+))`)

// maxValueLen bounds a generic secret value.
const maxValueLen = 512

func isSpaceNoNL(c byte) bool { return c == ' ' || c == '\t' || c == '\v' || c == '\f' }

func isBareValueByte(c byte) bool {
	switch c {
	case ' ', '\t', '\r', '\n', '\v', '\f', '"', '\'', '`', ',', ';', ')', '}', ']':
		return false
	}
	return true
}

// parseAssignment parses `key["']? sep value` at the start of s, where s
// holds one line. It returns submatch indexes laid out like genericAssign's,
// or nil.
func parseAssignment(s string) []int {
	m := make([]int, 18)
	for i := range m {
		m[i] = -1
	}
	m[0], m[2] = 0, 0
	i := 0
	for i < len(s) && i < 80 && isKeyByte(s[i]) {
		i++
	}
	if i < 2 {
		return nil
	}
	m[3] = i
	if i < len(s) && (s[i] == '"' || s[i] == '\'') {
		i++
	}
	m[4] = i
	for i < len(s) && isSpaceNoNL(s[i]) {
		i++
	}
	m[5], m[6] = i, i
	switch {
	case strings.HasPrefix(s[i:], ":="), strings.HasPrefix(s[i:], "=>"):
		i += 2
	case i < len(s) && (s[i] == '=' || s[i] == ':'):
		i++
	default:
		return nil
	}
	m[7], m[8] = i, i
	for i < len(s) && isSpaceNoNL(s[i]) {
		i++
	}
	m[9] = i
	if i >= len(s) {
		return nil
	}
	if q := s[i]; q == '"' || q == '\'' || q == '`' {
		end := strings.IndexByte(s[i+1:], q)
		if end <= 0 || end > maxValueLen {
			return nil
		}
		g := 14
		switch q {
		case '"':
			g = 10
		case '\'':
			g = 12
		}
		m[g], m[g+1] = i+1, i+1+end
		m[1] = i + 2 + end
		return m
	}
	j := i
	for j < len(s) && isBareValueByte(s[j]) {
		j++
	}
	if j == i || j-i > maxValueLen {
		return nil
	}
	m[16], m[17], m[1] = i, j, j
	return m
}

func extractGeneric(s string, m []int) (int, int, bool) {
	key := s[m[2]:m[3]]
	if !IsSecretKey(key) {
		return 0, 0, false
	}
	pre, sep, post := s[m[4]:m[5]], s[m[6]:m[7]], s[m[8]:m[9]]
	var start, end int
	quoted := true
	switch {
	case m[10] >= 0:
		start, end = m[10], m[11]
	case m[12] >= 0:
		start, end = m[12], m[13]
	case m[14] >= 0:
		start, end = m[14], m[15]
	default:
		start, end, quoted = m[16], m[17], false
	}
	v := s[start:end]
	if !plausibleSecretValue(v) {
		return 0, 0, false
	}
	if !quoted {
		// A call, an index or a reference to another variable is code, not
		// a secret.
		if strings.ContainsAny(v, "([") || (end < len(s) && s[end] == '(') {
			return 0, 0, false
		}
		envStyle := sep == "=" && pre == "" && post == ""
		if !envStyle && isIdentifier(v) && !hasDigit(v) {
			return 0, 0, false
		}
		if sep == ":" && post == "" && isAllDigits(v) {
			return 0, 0, false
		}
	}
	return start, end, true
}

// IsSecretKey reports whether a variable or field name names a credential,
// such as DB_PASSWORD, apiKey, client_secret or GITHUB_TOKEN, and not a
// neighbour such as max_tokens, password_file or tokenizer.
func IsSecretKey(key string) bool {
	words := splitWords(key)
	if len(words) == 0 {
		return false
	}
	secret := false
	for i, w := range words {
		switch w {
		case "password", "passwd", "passphrase", "pwd", "pass", "secret", "secrets", "token",
			"credential", "credentials", "apikey", "privatekey", "accesskey", "secretkey", "authtoken":
			secret = true
		case "key":
			if i > 0 {
				switch words[i-1] {
				case "api", "access", "private", "secret", "signing", "encryption", "master", "client", "auth", "account", "service", "license", "app", "admin":
					secret = true
				}
			}
		}
	}
	if !secret {
		return false
	}
	last := words[len(words)-1]
	return !nonSecretSuffix[last]
}

var nonSecretSuffix = map[string]bool{
	"file": true, "files": true, "path": true, "dir": true, "url": true, "uri": true, "endpoint": true,
	"name": true, "names": true, "id": true, "ids": true, "type": true, "count": true, "limit": true,
	"length": true, "len": true, "size": true, "ttl": true, "expiry": true, "expires": true,
	"expiration": true, "header": true, "field": true, "env": true, "var": true, "prefix": true,
	"hint": true, "policy": true, "rotation": true, "version": true, "mode": true, "format": true,
	"kind": true, "enabled": true, "required": true, "tokens": true, "at": true, "regex": true,
	"pattern": true, "label": true, "placeholder": true, "min": true, "max": true, "store": true,
	"provider": true, "source": true, "manager": true, "ref": true, "location": true, "usage": true,
	"budget": true, "rate": true, "index": true, "cmd": true, "command": true, "helper": true,
	"strength": true, "reset": true, "changed": true, "set": true, "valid": true, "error": true,
}

// splitWords splits snake_case, kebab-case, dotted and camelCase names into
// lower-case words.
func splitWords(s string) []string {
	var words []string
	var cur []byte
	flush := func() {
		if len(cur) > 0 {
			words = append(words, string(cur))
			cur = cur[:0]
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
			// Start a word at a lower→upper boundary or before the last upper
			// in a run followed by a lower (HTTPServer → http, server).
			if i > 0 {
				p := s[i-1]
				nextLower := i+1 < len(s) && s[i+1] >= 'a' && s[i+1] <= 'z'
				if (p >= 'a' && p <= 'z') || (p >= '0' && p <= '9') || (p >= 'A' && p <= 'Z' && nextLower) {
					flush()
				}
			}
			cur = append(cur, c+('a'-'A'))
		case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9'):
			cur = append(cur, c)
		default:
			flush()
		}
	}
	flush()
	return words
}

// plausibleSecretValue rejects empty values, placeholders and values too
// short to be credentials.
func plausibleSecretValue(v string) bool {
	if len(v) < 6 {
		return false
	}
	return !isPlaceholder(v)
}

var placeholderWords = map[string]bool{
	"password": true, "passwd": true, "secret": true, "changeme": true, "example": true, "redacted": true,
	"dummy": true, "test": true, "testing": true, "null": true, "none": true, "nil": true, "undefined": true,
	"true": true, "false": true, "string": true, "str": true, "required": true, "optional": true,
	"todo": true, "fixme": true, "placeholder": true, "sample": true, "foobar": true, "token": true,
	"apikey": true, "api_key": true, "number": true, "boolean": true, "object": true, "secret_key": true,
	"your_password": true, "yourpassword": true, "your-password": true, "mypassword": true,
	"your_token": true, "your-token": true, "your_api_key": true, "your-api-key": true,
	"notasecret": true, "not-a-secret": true, "insert_here": true, "hidden": true, "masked": true,
	"empty": true, "default": true, "unset": true,
}

// isPlaceholder reports values that are templates, references or obvious
// dummies rather than real credentials.
func isPlaceholder(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return true
	}
	lv := asciiLower(v)
	if placeholderWords[lv] {
		return true
	}
	switch {
	case strings.HasPrefix(v, "$"), strings.HasPrefix(v, "%"), strings.HasPrefix(v, "<"),
		strings.HasPrefix(v, "{{"), strings.HasPrefix(v, "#{"), strings.HasPrefix(v, "[REDACTED"),
		strings.HasPrefix(v, "@"), strings.HasPrefix(v, "&"), strings.HasPrefix(v, "*"):
		return true
	case strings.HasPrefix(lv, "env:"), strings.HasPrefix(lv, "process.env"), strings.HasPrefix(lv, "os.environ"),
		strings.HasPrefix(lv, "os.getenv"), strings.HasPrefix(lv, "vault:"), strings.HasPrefix(lv, "secretref:"),
		strings.HasPrefix(lv, "your_"), strings.HasPrefix(lv, "your-"), strings.HasPrefix(lv, "<your"):
		return true
	case strings.Contains(lv, "example"), strings.Contains(lv, "placeholder"), strings.Contains(lv, "redacted"),
		strings.Contains(lv, "xxxx"), strings.Contains(lv, "changeme"), strings.Contains(v, "${"),
		strings.Contains(v, "{{"):
		return true
	}
	// A run of one repeated character (xxxxxx, ******, 000000).
	same := true
	for i := 1; i < len(v); i++ {
		if v[i] != v[0] {
			same = false
			break
		}
	}
	return same
}

// isDocExample reports the well-known documentation credentials, such as
// AWS's AKIAIOSFODNN7EXAMPLE, which are public on purpose.
func isDocExample(v string) bool { return strings.Contains(v, "EXAMPLE") }

func isIdentifier(v string) bool {
	if v == "" {
		return false
	}
	start := true
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c == '.':
			if start {
				return false
			}
			start = true
			continue
		case c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
		case c >= '0' && c <= '9':
			if start {
				return false
			}
		default:
			return false
		}
		start = false
	}
	return !start
}

func isAllDigits(v string) bool {
	for i := 0; i < len(v); i++ {
		if v[i] < '0' || v[i] > '9' {
			return false
		}
	}
	return v != ""
}

func hasDigit(v string) bool {
	for i := 0; i < len(v); i++ {
		if v[i] >= '0' && v[i] <= '9' {
			return true
		}
	}
	return false
}

func hasUpper(v string) bool {
	for i := 0; i < len(v); i++ {
		if v[i] >= 'A' && v[i] <= 'Z' {
			return true
		}
	}
	return false
}

func hasLower(v string) bool {
	for i := 0; i < len(v); i++ {
		if v[i] >= 'a' && v[i] <= 'z' {
			return true
		}
	}
	return false
}

func mixedClasses(v string) bool { return hasUpper(v) && hasLower(v) && hasDigit(v) }

// asciiLower lower-cases ASCII letters only, so byte offsets are preserved.
func asciiLower(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if b[j] >= 'A' && b[j] <= 'Z' {
					b[j] += 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}

func isWordByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
