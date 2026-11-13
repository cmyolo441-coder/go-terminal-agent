package secrets

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

var placeholderRE = regexp.MustCompile(`\[REDACTED:[a-z0-9-]+:[0-9a-f]{4}\]`)

// FuzzRedact checks that redaction never panics, never leaves a found
// secret behind, is idempotent, and keeps JSON valid.
func FuzzRedact(f *testing.F) {
	r, err := New(Options{Key: []byte("fuzz")})
	if err != nil {
		f.Fatal(err)
	}
	for _, s := range []string{"", "password=hunter22x", `{"api_key":"abcdef123456"}`, "ghp_" + strings.Repeat("a1B", 12),
		"-----BEGIN PRIVATE KEY-----\nabc", "postgres://u:p4ssw0rd!@h/db", "é\xff\x00", `[" ",1e5,null]`,
		"token=s3cr3tValue1 and again s3cr3tValue1", "PASSWORD=abc\r\nx", `{"name":"DB_PASSWORD","value":"hunter2hunter2"}`,
		"Authorization: Bearer abcdefghijklmnop", "AKIAABCDEFGHIJKLMNOP", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.c2lnbmF0dXJlMTIz",
		"secret: \"" + strings.Repeat("Ab1", 30) + "\"", "x=" + strings.Repeat("Zq9", 10) + " " + strings.Repeat("Zq9", 10)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out, fs := r.Redact(s)
		// Placeholders name the rule ("connection-string"), so a secret
		// that happens to be such a word is looked for outside them.
		bare := placeholderRE.ReplaceAllString(out, "\x00")
		for _, fd := range fs {
			if fd.Start < 0 || fd.End > len(s) || fd.Start >= fd.End {
				t.Fatalf("bad span %+v", fd)
			}
			secret := s[fd.Start:fd.End]
			if len(secret) >= minPropagateLen && strings.Contains(bare, secret) {
				t.Fatalf("redacted output %q still contains secret %q (%s)", out, secret, fd.RuleID)
			}
		}
		again, fs2 := r.Redact(out)
		if again != out || len(fs2) != 0 {
			t.Fatalf("not idempotent: %q -> %q (%v)", out, again, fs2)
		}
		if json.Valid([]byte(s)) {
			j, jfs, err := r.RedactJSON([]byte(s))
			if err != nil || !json.Valid(j) {
				t.Fatalf("RedactJSON broke valid JSON %q: %q %v", s, j, err)
			}
			j2, jfs2, err := r.RedactJSON(j)
			if err != nil || string(j2) != string(j) || len(jfs2) != 0 {
				t.Fatalf("RedactJSON not idempotent: %q -> %q -> %q (%v)", s, j, j2, jfs2)
			}
			_ = jfs
		}
		msgs := []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: s}, {Type: llm.BlockToolUse, Input: json.RawMessage(s)}}, Native: json.RawMessage(s)}}
		red, _ := r.RedactMessages(msgs)
		if red[0].Blocks[0].Text != out {
			t.Fatalf("RedactMessages text %q != Redact %q", red[0].Blocks[0].Text, out)
		}
		if msgs[0].Blocks[0].Text != s {
			t.Fatal("RedactMessages modified its input")
		}
	})
}

// FuzzParseAssignment checks the hand-written assignment parser against
// its contract: indexes in range and ordered, never a panic.
func FuzzParseAssignment(f *testing.F) {
	for _, s := range []string{"PASSWORD=x", `api_key: "v"`, "k := `v`", "a=>b", "x", `"password": 'abc'`, "tok=\"unterminated"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		m := parseAssignment(s)
		if m == nil {
			return
		}
		for i := 0; i < len(m); i += 2 {
			if m[i] < 0 && m[i+1] < 0 {
				continue
			}
			if m[i] < 0 || m[i+1] < m[i] || m[i+1] > len(s) {
				t.Fatalf("bad group %d [%d,%d] in %q", i/2, m[i], m[i+1], s)
			}
		}
		_, _, _ = extractGeneric(s, m)
		_ = IsSecretKey(s)
	})
}
