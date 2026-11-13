package secrets

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

// Test credentials are generated at run time so no secret-shaped literal
// lives in the repository (and push protection has nothing to flag).
const (
	alnum   = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	upperD  = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	letters = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	b64     = alnum + "+/"
	urlSafe = alnum + "_-"
)

func fake(seed int64, charset string, n int) string {
	rng := rand.New(rand.NewSource(seed))
	b := make([]byte, n)
	for i := range b {
		b[i] = charset[rng.Intn(len(charset))]
	}
	// Make sure every class shows up so mixed-class checks are meaningful.
	if strings.ContainsAny(charset, "a") && strings.ContainsAny(charset, "7") && n > 6 {
		b[1], b[3], b[5] = 'q', '7', 'Z'
	}
	return string(b)
}

func newTest(t testing.TB, opts Options) *Redactor {
	t.Helper()
	if opts.Key == nil {
		opts.Key = []byte("test-key")
	}
	r, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type sample struct {
	name, rule, text, secret string
}

func positives() []sample {
	ghp := "ghp_" + fake(1, alnum, 36)
	ghFine := "github_pat_" + fake(2, alnum+"_", 82)
	glpat := "glpat-" + fake(3, urlSafe, 20)
	slack := "xoxb-" + fake(4, "0123456789", 12) + "-" + fake(5, "0123456789", 13) + "-" + fake(6, alnum, 24)
	hook := "https://hooks.slack.com/services/T" + fake(7, upperD, 8) + "/B" + fake(8, upperD, 8) + "/" + fake(9, alnum, 24)
	stripe := "sk_live_" + fake(10, alnum, 24)
	google := "AIza" + fake(11, urlSafe, 35)
	ant := "sk-ant-api03-" + fake(12, urlSafe, 93) + "AA"
	oai := "sk-proj-" + fake(13, urlSafe, 120)
	oaiLegacy := "sk-" + fake(14, alnum, 20) + "T3BlbkFJ" + fake(15, alnum, 20)
	nv := "nvapi-" + fake(16, urlSafe, 64)
	hf := "hf_" + fake(17, letters, 34)
	npm := "npm_" + fake(18, alnum, 36)
	sg := "SG." + fake(19, urlSafe, 22) + "." + fake(20, urlSafe, 43)
	akid := "AKIA" + fake(21, upperD, 16)
	awsSecret := fake(22, b64, 40)
	azure := fake(23, b64, 86) + "=="
	jwt := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9." + "eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiaWF0IjoxNTE2MjM5MDIyfQ." + fake(24, urlSafe, 43)
	pem := "-----BEGIN RSA PRIVATE KEY-----\n" + fake(25, b64, 64) + "\n" + fake(26, b64, 64) + "\n-----END RSA PRIVATE KEY-----"
	pgp := "-----BEGIN PGP PRIVATE KEY BLOCK-----\n\n" + fake(27, b64, 64) + "\n-----END PGP PRIVATE KEY BLOCK-----"
	bearer := fake(28, urlSafe, 40)
	dbpw := "Tr0ub4dor-" + fake(29, alnum, 8)
	entropyTok := fake(30, alnum, 40)
	return []sample{
		{"github classic", RuleGitHubToken, "export GITHUB_TOKEN=" + ghp + "\n", ghp},
		{"github fine-grained", RuleGitHubToken, "token: " + ghFine, ghFine},
		{"gitlab", RuleGitLabToken, "CI_TOKEN " + glpat + " end", glpat},
		{"slack bot", RuleSlackToken, `SLACK_BOT="` + slack + `"`, slack},
		{"slack webhook", RuleSlackWebhook, "curl -X POST " + hook + " -d @msg.json", hook},
		{"stripe", RuleStripeKey, "stripe.api_key = '" + stripe + "'", stripe},
		{"google", RuleGoogleAPIKey, "maps?key=" + google + "&q=x", google},
		{"anthropic", RuleAnthropicKey, "ANTHROPIC_API_KEY=" + ant, ant},
		{"openai project", RuleOpenAIKey, `{"key":"` + oai + `"}`, oai},
		{"openai legacy", RuleOpenAIKey, "openai.api_key = " + oaiLegacy, oaiLegacy},
		{"nvidia", RuleNVIDIAKey, "Authorization header uses " + nv, nv},
		{"huggingface", RuleHuggingFaceToken, "HF " + hf, hf},
		{"npm", RuleNPMToken, "//registry.npmjs.org/:_authToken=" + npm, npm},
		{"sendgrid", RuleSendGridKey, "SENDGRID " + sg, sg},
		{"aws access key", RuleAWSAccessKey, "aws_access_key_id = " + akid, akid},
		{"aws secret", RuleAWSSecretKey, "aws_secret_access_key = " + awsSecret + "\n", awsSecret},
		{"azure", RuleAzureStorageKey, "DefaultEndpointsProtocol=https;AccountName=acct;AccountKey=" + azure + ";EndpointSuffix=core.windows.net", azure},
		{"jwt", RuleJWT, "cookie session=" + jwt + "; Path=/", jwt},
		{"pem", RulePrivateKey, "key:\n" + pem + "\ndone", pem},
		{"pgp", RulePrivateKey, pgp, pgp},
		{"bearer header", RuleAuthHeader, `curl -H "Authorization: Bearer ` + bearer + `" https://api.example.org`, bearer},
		{"postgres url", RuleConnString, "DATABASE_URL=postgres://app:" + dbpw + "@db.internal:5432/app", dbpw},
		{"env file", RuleGenericSecret, "DB_PASSWORD=" + dbpw + "\n", dbpw},
		{"yaml quoted", RuleGenericSecret, `  password: "` + dbpw + `"`, dbpw},
		{"json field", RuleGenericSecret, `{"client_secret": "` + dbpw + `"}`, dbpw},
		{"python", RuleGenericSecret, "SECRET_KEY = '" + dbpw + "'", dbpw},
		{"go raw string", RuleGenericSecret, "apiKey := `" + dbpw + "`", dbpw},
		{"yaml digits", RuleGenericSecret, "password: hunter22x", "hunter22x"},
		{"high entropy", RuleHighEntropy, "value " + entropyTok + " here", entropyTok},
	}
}

func TestRulesFindRealisticSecrets(t *testing.T) {
	r := newTest(t, Options{})
	for _, s := range positives() {
		t.Run(s.name, func(t *testing.T) {
			out, fs := r.Redact(s.text)
			if len(fs) != 1 {
				t.Fatalf("want 1 finding, got %d: %v\ntext: %q\nout:  %q", len(fs), fs, s.text, out)
			}
			f := fs[0]
			if f.RuleID != s.rule {
				t.Fatalf("rule = %s, want %s (out %q)", f.RuleID, s.rule, out)
			}
			if got := s.text[f.Start:f.End]; got != s.secret {
				t.Fatalf("span = %q, want %q", got, s.secret)
			}
			if strings.Contains(out, s.secret) {
				t.Fatalf("secret survived: %q", out)
			}
			want := "[REDACTED:" + s.rule + ":" + f.Fingerprint + "]"
			if f.Placeholder != want || !strings.Contains(out, want) {
				t.Fatalf("placeholder %q not in %q", want, out)
			}
			// Findings must never carry the secret.
			js, _ := json.Marshal(fs)
			if strings.Contains(string(js), s.secret) || strings.Contains(f.String(), s.secret) {
				t.Fatal("finding leaks the secret")
			}
		})
	}
}

func TestFalsePositives(t *testing.T) {
	r := newTest(t, Options{})
	cases := map[string]string{
		"go.sum":             "github.com/bmatcuk/doublestar/v4 v4.10.4 h1:" + fake(40, b64, 43) + "=\n",
		"git sha":            "commit 0f2d6c1e9b8a7f6e5d4c3b2a1908f7e6d5c4b3a2\nMerge: 1a2b3c4 5d6e7f8",
		"uuid":               "request_id=3f2a9c1e-7b4d-4e8a-9f1c-2d3e4f5a6b7c",
		"sha256 hex":         "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855  file.tar.gz",
		"data uri":           `<img src="data:image/png;base64,` + fake(41, b64, 400) + `">`,
		"npm integrity":      `"integrity": "sha512-` + fake(42, b64, 86) + `=="`,
		"docker digest":      "image: alpine@sha256:" + fake(43, b64, 44),
		"go struct":          "type Config struct {\n\tPassword string `json:\"password\"`\n\tAPIKey string\n}",
		"getenv":             `password := os.Getenv("DB_PASSWORD")`,
		"function call":      "token = fetchToken(ctx)\nsecret = loadSecret()",
		"variable ref":       "password = cfg.Database.Password\nconst apiKey = settings.apiKey",
		"js object":          "login({ password: newPassword, token: authToken })",
		"max tokens":         "max_tokens: 64000\nmaxTokens = 128000\ninput_tokens=1234",
		"tokenizer":          `tokenizer = "cl100k_base_tokenizer"`,
		"password file":      "password_file: /run/secrets/db_password\nsecret_name: prod-db-credentials",
		"templates":          "password: ${DB_PASSWORD}\ntoken: {{ .Values.token }}\napi_key=<your-api-key>\nsecret: %(secret)s",
		"aws docs example":   "AKIAIOSFODNN7EXAMPLE and wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		"url placeholder":    "postgres://user:password@localhost:5432/db and https://example.com/a:b@c",
		"bearer var":         `curl -H "Authorization: Bearer $TOKEN" https://api.example.org`,
		"type annotations":   "password: string\napi_key: str\ntoken: Optional[str] = None",
		"module path":        "github.com/cmyolo441-coder/go-terminal-agent/internal/llm/openai",
		"camel identifier":   "func TestRedactMessagesHandlesNative2JSONPayloads(t *testing.T) {}",
		"masked":             "password: ********\napi_key = xxxxxxxxxxxx",
		"short":              "pwd=abc",
		"redacted already":   "token=[REDACTED:github-token:3f2a]",
		"prose":              "The password policy requires a token rotation every 90 days; see the secret store docs.",
		"base64 image chunk": "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg== is a pixel? no: data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==",
	}
	for name, text := range cases {
		if name == "base64 image chunk" {
			// Only the data: URI copy is exempt; check that one.
			text = text[strings.Index(text, "data:"):]
		}
		t.Run(name, func(t *testing.T) {
			if out, fs := r.Redact(text); len(fs) != 0 {
				t.Fatalf("false positive: %v\nin:  %q\nout: %q", fs, text, out)
			}
		})
	}
}

func TestPublicMaterialIsNotRedacted(t *testing.T) {
	r := newTest(t, Options{})
	var cert strings.Builder
	cert.WriteString("-----BEGIN CERTIFICATE-----\n")
	for i := 0; i < 12; i++ {
		cert.WriteString(fake(int64(90+i), b64, 64) + "\n")
	}
	cert.WriteString("-----END CERTIFICATE-----\n")
	oneLine := strings.ReplaceAll(cert.String(), "\n", `\n`)
	var blob strings.Builder
	for i := 0; i < 5; i++ {
		blob.WriteString(fake(int64(110+i), b64, 76) + "\r\n")
	}
	cases := map[string]string{
		"pem certificate":     cert.String(),
		"escaped certificate": `{"ca":"` + oneLine + `"}`,
		"base64 file":         blob.String(),
		"ssh public key":      "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI" + fake(120, b64, 43) + " dev@laptop",
	}
	for name, text := range cases {
		if _, fs := r.Redact(text); len(fs) != 0 {
			t.Errorf("%s: %v", name, fs)
		}
	}
	// A private key next to a certificate is still found.
	_, fs := r.Redact(cert.String() + "-----BEGIN PRIVATE KEY-----\n" + fake(121, b64, 64) + "\n-----END PRIVATE KEY-----")
	if len(fs) != 1 || fs[0].RuleID != RulePrivateKey {
		t.Fatalf("findings = %v", fs)
	}
}

func TestParseAssignmentMatchesRegexp(t *testing.T) {
	lines := []string{
		`password = "abc def"`, `PASSWORD=hunter22`, `api_key: 'x1y2z3'`, "token := `raw-val`", `"secret": "v"`,
		`key => value123`, `pwd:abc`, `password=`, `password "x"`, `token: [a]`, `secret = 'unterminated`,
	}
	for _, l := range lines {
		want := genericAssign.FindStringSubmatchIndex(l)
		if want != nil && want[0] != 0 {
			want = nil
		}
		got := parseAssignment(l)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%q: parser %v, regexp %v", l, got, want)
		}
	}
}

func TestStableAndKeyedPlaceholders(t *testing.T) {
	tok := "ghp_" + fake(50, alnum, 36)
	other := "ghp_" + fake(51, alnum, 36)
	r := newTest(t, Options{})
	out, fs := r.Redact(tok + " " + other + " " + tok)
	if len(fs) != 3 {
		t.Fatalf("findings = %d", len(fs))
	}
	if fs[0].Placeholder != fs[2].Placeholder {
		t.Fatal("same secret should get the same placeholder")
	}
	if fs[0].Placeholder == fs[1].Placeholder {
		t.Fatal("different secrets should get different placeholders")
	}
	if !regexp.MustCompile(`^\[REDACTED:github-token:[0-9a-f]{4}\] \[REDACTED:github-token:[0-9a-f]{4}\] \[REDACTED:github-token:[0-9a-f]{4}\]$`).MatchString(out) {
		t.Fatalf("out = %q", out)
	}
	r2 := newTest(t, Options{Key: []byte("other-key")})
	_, fs2 := r2.Redact(tok)
	if fs2[0].Fingerprint == fs[0].Fingerprint {
		// 1 in 65536 by chance; these fixed inputs do differ.
		t.Fatal("fingerprint should depend on the key")
	}
	if r.Placeholder(RuleGitHubToken, tok) != fs[0].Placeholder {
		t.Fatal("Placeholder disagrees with Redact")
	}
}

func TestRedactIsIdempotent(t *testing.T) {
	r := newTest(t, Options{})
	var b strings.Builder
	for _, s := range positives() {
		b.WriteString(s.text)
		b.WriteString("\n")
	}
	once, fs := r.Redact(b.String())
	if len(fs) < len(positives()) {
		t.Fatalf("found %d of %d", len(fs), len(positives()))
	}
	twice, fs2 := r.Redact(once)
	if twice != once || len(fs2) != 0 {
		t.Fatalf("second pass changed output: %v", fs2)
	}
}

func TestLineAndColumn(t *testing.T) {
	r := newTest(t, Options{})
	tok := "AKIA" + fake(60, upperD, 16)
	text := "first line\nsecond\n   key=" + tok + "\n"
	_, fs := r.Redact(text)
	if len(fs) != 1 || fs[0].Line != 3 || fs[0].Column != 8 {
		t.Fatalf("findings = %+v", fs)
	}
}

func TestOverlapPrefersSpecificRule(t *testing.T) {
	r := newTest(t, Options{})
	ant := "sk-ant-api03-" + fake(61, urlSafe, 90)
	_, fs := r.Redact(`api_key = "` + ant + `"`)
	if len(fs) != 1 || fs[0].RuleID != RuleAnthropicKey {
		t.Fatalf("findings = %v", fs)
	}
}

func TestTruncatedPrivateKeyStillRedacted(t *testing.T) {
	r := newTest(t, Options{})
	body := fake(62, b64, 64)
	text := "-----BEGIN OPENSSH PRIVATE KEY-----\n" + body + "\n" + body
	out, fs := r.Redact(text)
	if len(fs) != 1 || strings.Contains(out, body) {
		t.Fatalf("out = %q", out)
	}
}

func TestOptions(t *testing.T) {
	tok := "ghp_" + fake(63, alnum, 36)
	if _, fs := newTest(t, Options{AllowValues: []string{tok}}).Redact(tok); len(fs) != 0 {
		t.Fatal("allow-listed value was redacted")
	}
	if _, fs := newTest(t, Options{Disabled: []string{RuleGitHubToken, RuleHighEntropy}}).Redact(tok); len(fs) != 0 {
		t.Fatalf("disabled rule fired: %v", fs)
	}
	custom := Rule{ID: "acme-key", Keywords: []string{"acme_"}, Pattern: regexp.MustCompile(`acme_[a-z0-9]{12}`)}
	r := newTest(t, Options{ExtraRules: []Rule{custom}})
	if out, fs := r.Redact("use acme_0123456789ab now"); len(fs) != 1 || fs[0].RuleID != "acme-key" || strings.Contains(out, "acme_0") {
		t.Fatalf("custom rule: %v %q", fs, out)
	}
	for _, bad := range []Rule{
		{ID: "Bad ID", Pattern: regexp.MustCompile(`x`)},
		{ID: "nopattern"},
		{ID: "group", Pattern: regexp.MustCompile(`x`), SecretGroup: 2},
		{ID: "kw", Pattern: regexp.MustCompile(`x`), Keywords: []string{""}},
		{ID: RuleJWT, Pattern: regexp.MustCompile(`x`)},
	} {
		if _, err := New(Options{ExtraRules: []Rule{bad}}); err == nil {
			t.Errorf("rule %+v: want error", bad.ID)
		}
	}
	if ids := newTest(t, Options{DisableEntropy: true}).Rules(); strings.Contains(strings.Join(ids, ","), RuleHighEntropy) {
		t.Fatal("entropy listed while disabled")
	}
}

func TestIsSecretKey(t *testing.T) {
	yes := []string{"password", "DB_PASSWORD", "dbPass", "apiKey", "API_KEY", "x-api-key", "client_secret", "GITHUB_TOKEN",
		"accessToken", "privateKey", "aws.secretKey", "passphrase", "SECRET", "credentials", "authToken", "signing_key"}
	no := []string{"max_tokens", "maxTokens", "password_file", "tokenizer", "keyboard", "monkey", "key", "secret_name",
		"token_url", "passwordField", "user", "passage", "bypass_cache", "primary_key_id", "sort_key"}
	for _, k := range yes {
		if !IsSecretKey(k) {
			t.Errorf("IsSecretKey(%q) = false", k)
		}
	}
	for _, k := range no {
		if IsSecretKey(k) {
			t.Errorf("IsSecretKey(%q) = true", k)
		}
	}
}

func TestRedactJSON(t *testing.T) {
	r := newTest(t, Options{})
	pw := "S3cure-" + fake(70, alnum, 10)
	tok := "ghp_" + fake(71, alnum, 36)
	in := []byte(`{"zeta":1,"command":"git push https://x:` + tok + `@github.com/o/r","alpha":{"password":"` + pw + `","n":null,"ok":true,"f":1.5e3},` +
		`"env":[{"name":"DB_PASSWORD","value":"` + pw + `"},{"name":"LOG_LEVEL","value":"debug-verbose"}],"html":"<a&b>","u":"\u2028"}`)
	out, fs, err := r.RedactJSON(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 3 {
		t.Fatalf("findings = %d %v\n%s", len(fs), fs, out)
	}
	if strings.Contains(string(out), pw) || strings.Contains(string(out), tok) {
		t.Fatalf("secret survived: %s", out)
	}
	if !json.Valid(out) {
		t.Fatalf("invalid JSON: %s", out)
	}
	// Key order and untouched values are kept.
	s := string(out)
	if !(strings.Index(s, `"zeta"`) < strings.Index(s, `"command"`) && strings.Index(s, `"command"`) < strings.Index(s, `"alpha"`)) {
		t.Fatalf("key order changed: %s", s)
	}
	for _, keep := range []string{`"n":null`, `"ok":true`, `"f":1.5e3`, `"debug-verbose"`, `"<a&b>"`, `\u2028`} {
		if !strings.Contains(s, keep) {
			t.Errorf("lost %s in %s", keep, s)
		}
	}
	clean := []byte(`{ "b": 1,  "a": "plain" }`)
	if out, fs, err := r.RedactJSON(clean); err != nil || len(fs) != 0 || string(out) != string(clean) {
		t.Fatalf("clean JSON should be returned unchanged: %s %v %v", out, fs, err)
	}
	for _, bad := range []string{`{"a":`, `{"a":1} {"b":2}`, `nope`, strings.Repeat("[", 600) + strings.Repeat("]", 600)} {
		if _, _, err := r.RedactJSON([]byte(bad)); err == nil {
			t.Errorf("RedactJSON(%.20q) want error", bad)
		}
	}
}

func TestRedactMessages(t *testing.T) {
	r := newTest(t, Options{})
	tok := "ghp_" + fake(80, alnum, 36)
	nv := "nvapi-" + fake(81, urlSafe, 64)
	pw := "Pa55-" + fake(82, alnum, 12)
	input := json.RawMessage(`{"command":"curl -H 'Authorization: Bearer ` + tok + `' https://api.github.com","timeout":30}`)
	native := json.RawMessage(`[{"type":"thinking","thinking":"the key ` + nv + ` is set","signature":"sig"},{"type":"text","text":"Your key is ` + nv + `"},{"type":"tool_use","id":"tu_1","name":"bash","input":{"command":"echo ` + nv + `"}}]`)
	msgs := []llm.Message{
		llm.UserText("my db password=" + pw + " please connect"),
		{Role: llm.RoleAssistant, Provider: "anthropic", Native: native, Blocks: []llm.Block{
			{Type: llm.BlockThinking, Text: "the key " + nv + " is set"},
			{Type: llm.BlockText, Text: "Your key is " + nv},
			{Type: llm.BlockToolUse, ToolUseID: "tu_1", ToolName: "bash", Input: input},
		}},
		{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockToolResult, ToolUseID: "tu_1", Text: "GITHUB_TOKEN=" + tok + "\nok"}}},
		{Role: llm.RoleAssistant, Provider: "anthropic", Native: json.RawMessage(`not json ` + tok)},
	}
	orig, _ := json.Marshal(msgs)
	out, rep := r.RedactMessages(msgs)
	after, _ := json.Marshal(msgs)
	if string(orig) != string(after) {
		t.Fatal("RedactMessages modified its input")
	}
	all, _ := json.Marshal(out)
	nativeOut := string(out[1].Native)
	// The signed thinking block is kept byte-for-byte; everything else is clean.
	if !strings.Contains(nativeOut, `"thinking":"the key `+nv+` is set","signature":"sig"`) {
		t.Fatalf("signed thinking changed: %s", nativeOut)
	}
	rest := strings.Replace(string(all), `"thinking":"the key `+nv+` is set"`, "", 1)
	for _, s := range []string{tok, pw, nv} {
		if strings.Contains(rest, s) {
			t.Fatalf("secret %q survived in %s", s[:8], rest)
		}
	}
	if !json.Valid(out[1].Blocks[2].Input) || !json.Valid(out[1].Native) {
		t.Fatal("redaction broke JSON")
	}
	if rep.SignedUnredacted != 1 {
		t.Fatalf("SignedUnredacted = %d", rep.SignedUnredacted)
	}
	if rep.DroppedNative != 1 || out[3].Native != nil || out[3].Provider != "" {
		t.Fatalf("unparseable native with a secret should be dropped: %+v", rep)
	}
	fields := map[string]int{}
	for _, f := range rep.Findings {
		fields[f.Field]++
	}
	if fields["text"] < 4 || fields["input"] != 1 || fields["native"] != 2 || rep.Count() != len(rep.Findings) {
		t.Fatalf("fields = %v", fields)
	}
}

func TestTruncatedToolInputIsRedactedAsText(t *testing.T) {
	r := newTest(t, Options{})
	tok := "ghp_" + fake(83, alnum, 36)
	out, _ := r.RedactMessages([]llm.Message{{Role: llm.RoleAssistant, Blocks: []llm.Block{
		{Type: llm.BlockToolUse, Input: json.RawMessage(`{"command":"echo ` + tok)},
	}}})
	if strings.Contains(string(out[0].Blocks[0].Input), tok) {
		t.Fatal("secret survived in truncated input")
	}
}

func TestConcurrentUse(t *testing.T) {
	r := Default()
	tok := "ghp_" + fake(84, alnum, 36)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				out, fs := Redact(fmt.Sprintf("n=%d token %s", j, tok))
				if len(fs) != 1 || strings.Contains(out, tok) {
					t.Error("concurrent redact failed")
					return
				}
			}
			_, _ = RedactMessages([]llm.Message{llm.UserText(tok)})
		}()
	}
	wg.Wait()
	if r != Default() {
		t.Fatal("Default should be shared")
	}
}

// corpus builds about size bytes of realistic source code and logs.
func corpus(size int, withSecrets bool) (string, int) {
	chunk := `package server

import (
	"context"
	"net/http"
	"os"
)

// Handler serves the API. Config comes from the environment.
type Handler struct {
	Password string ` + "`json:\"password\"`" + `
	MaxTokens int
	client *http.Client
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("Authorization")
	if token == "" {
		http.Error(w, "missing token", http.StatusUnauthorized)
		return
	}
	key := os.Getenv("API_KEY")
	_ = key
	// commit 9fceb02d0ae598e95dc970b74767f19372d61af8 fixed the cache key
	h.client.Do(r.WithContext(context.Background()))
}
2026-11-13T10:00:00Z INFO request_id=3f2a9c1e-7b4d-4e8a-9f1c-2d3e4f5a6b7c path=/v1/messages status=200 dur=12ms
`
	var b strings.Builder
	b.Grow(size + 1024)
	n, i := 0, 0
	for b.Len() < size {
		b.WriteString(chunk)
		if withSecrets && i%20 == 0 {
			fmt.Fprintf(&b, "export GITHUB_TOKEN=ghp_%s\nDB_PASSWORD=Xy9-%s\n", fake(int64(1000+i), alnum, 36), fake(int64(2000+i), alnum, 10))
			n += 2
		}
		i++
	}
	return b.String(), n
}

func TestLargeInput(t *testing.T) {
	r := newTest(t, Options{})
	text, want := corpus(1<<20, true)
	out, fs := r.Redact(text)
	if len(fs) != want {
		byRule := map[string]int{}
		for _, f := range fs {
			byRule[f.RuleID]++
		}
		t.Fatalf("found %d, want %d: %v", len(fs), want, byRule)
	}
	if strings.Contains(out, "ghp_") && strings.Contains(out, "GITHUB_TOKEN=ghp_") {
		t.Fatal("token survived")
	}
	clean, _ := corpus(1<<20, false)
	if fs := r.Scan(clean); len(fs) != 0 {
		t.Fatalf("false positives in clean corpus: %v", fs[:min(len(fs), 5)])
	}
}

func BenchmarkRedactClean1MB(b *testing.B) {
	r := newTest(b, Options{})
	text, _ := corpus(1<<20, false)
	b.SetBytes(int64(len(text)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Redact(text)
	}
}

func BenchmarkRedactSecrets1MB(b *testing.B) {
	r := newTest(b, Options{})
	text, _ := corpus(1<<20, true)
	b.SetBytes(int64(len(text)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Redact(text)
	}
}

// A secret found once is redacted wherever else it appears, even where no
// rule would recognise it on its own; short secrets do not spread.
func TestFoundSecretIsRedactedEverywhere(t *testing.T) {
	r := newTest(t, Options{})
	in := "DB_PASSWORD=s3cr3tValue1\nconnecting with s3cr3tValue1 now"
	out, fs := r.Redact(in)
	if strings.Contains(out, "s3cr3tValue1") || len(fs) != 2 || fs[0].Placeholder != fs[1].Placeholder {
		t.Fatalf("second occurrence leaked: %q %v", out, fs)
	}
	if fs[1].Line != 2 {
		t.Fatalf("propagated finding has the wrong line: %+v", fs[1])
	}
	out, _ = r.Redact("postgres://u:xy@db/x and xy elsewhere")
	if !strings.Contains(out, "xy elsewhere") {
		t.Fatalf("a short secret spread to unrelated text: %q", out)
	}
}

func BenchmarkRedactRandom1MB(b *testing.B) {
	// Worst case for the entropy check: one long run of base64 lines.
	r := newTest(b, Options{})
	var sb strings.Builder
	for i := 0; sb.Len() < 1<<20; i++ {
		sb.WriteString(fake(int64(i), b64, 76))
		sb.WriteByte('\n')
	}
	text := sb.String()
	b.SetBytes(int64(len(text)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Redact(text)
	}
}

func BenchmarkRedactJSON(b *testing.B) {
	r := newTest(b, Options{})
	text, _ := corpus(256<<10, true)
	in, _ := json.Marshal(map[string]any{"path": "a.go", "content": text, "password": "Xy9-abcdef123"})
	b.SetBytes(int64(len(in)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := r.RedactJSON(in); err != nil {
			b.Fatal(err)
		}
	}
}
