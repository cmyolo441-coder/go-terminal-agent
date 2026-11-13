package secrets

import (
	"encoding/json"
	"strings"
	"testing"
)

// FuzzRedact checks that redaction never panics, never leaves a found
// secret behind, is idempotent, and keeps JSON valid.
func FuzzRedact(f *testing.F) {
	r, err := New(Options{Key: []byte("fuzz")})
	if err != nil {
		f.Fatal(err)
	}
	for _, s := range []string{"", "password=hunter22x", `{"api_key":"abcdef123456"}`, "ghp_" + strings.Repeat("a1B", 12),
		"-----BEGIN PRIVATE KEY-----\nabc", "postgres://u:p4ssw0rd!@h/db", "é\xff\x00", `[" ",1e5,null]`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out, fs := r.Redact(s)
		for _, fd := range fs {
			if fd.Start < 0 || fd.End > len(s) || fd.Start >= fd.End {
				t.Fatalf("bad span %+v", fd)
			}
		}
		again, fs2 := r.Redact(out)
		if again != out || len(fs2) != 0 {
			t.Fatalf("not idempotent: %q -> %q (%v)", out, again, fs2)
		}
		if json.Valid([]byte(s)) {
			j, _, err := r.RedactJSON([]byte(s))
			if err != nil || !json.Valid(j) {
				t.Fatalf("RedactJSON broke valid JSON %q: %q %v", s, j, err)
			}
		}
	})
}
