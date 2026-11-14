package dlp

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestVaultDeterministic(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	v1, _ := NewVault(key, 0)
	v2, _ := NewVault(key, 0)
	a, _ := v1.Tokenize(CreditCard, "4111 1111 1111 1111")
	b, _ := v1.Tokenize(CreditCard, "4111-1111-1111-1111")
	c, _ := v2.Tokenize(CreditCard, "4111111111111111")
	if a != b || a != c {
		t.Fatalf("tokens differ: %s %s %s", a, b, c)
	}
	if !strings.HasPrefix(a, "[[PII:CREDIT_CARD:") || parseToken(a) != len(a) {
		t.Fatalf("format %q", a)
	}
	d, _ := v1.Tokenize(USSSN, "4111111111111111")
	if d == a {
		t.Fatal("type not part of token")
	}
	e1, _ := v1.Tokenize(Email, "Jane@Corp.com")
	e2, _ := v1.Tokenize(Email, "jane@corp.com")
	if e1 != e2 {
		t.Fatal("email not case-normalized")
	}
	other, _ := NewVault([]byte("another key of sixteen+"), 0)
	if x, _ := other.Tokenize(CreditCard, "4111111111111111"); x == a {
		t.Fatal("key not used")
	}
	if _, err := NewVault([]byte("short"), 0); err == nil {
		t.Fatal("short key accepted")
	}
	if _, err := v1.Tokenize("bad type", "x"); err == nil {
		t.Fatal("bad type accepted")
	}
}

func TestVaultRoundTrip(t *testing.T) {
	e := mustEngine(t, policyJSON(t, `{"version":1,"default_action":"tokenize"}`))
	v, _ := NewVault(nil, 0)
	in := "Jane (jane@corp.com, +44 20 7946 0958, ssn 536-22-1234, IBAN DE89370400440532013000) wrote again from jane@corp.com"
	res, err := e.Process(in, v)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"jane@corp.com", "7946", "536-22", "DE89"} {
		if strings.Contains(res.Text, s) {
			t.Fatalf("%q leaked: %q", s, res.Text)
		}
	}
	if strings.Count(res.Text, "[[PII:EMAIL:") != 2 || v.Len() != 4 {
		t.Fatalf("got %q (vault %d)", res.Text, v.Len())
	}
	// A model echoes the tokens back inside its own text.
	model := "Sure — I'll email " + strings.SplitN(strings.SplitN(res.Text, "(", 2)[1], ",", 2)[0] + " today."
	back, n := v.Detokenize(model)
	if n != 1 || back != "Sure — I'll email jane@corp.com today." {
		t.Fatalf("detokenize %q %d", back, n)
	}
	if out, n := v.Detokenize(res.Text); n != 5 || out != in {
		t.Fatalf("full round trip %q %d", out, n)
	}
	// Unknown, forged and malformed tokens are left alone.
	for _, s := range []string{"[[PII:EMAIL:000000000000]]", "[[PII:EMAIL:abc]]", "[[PII:email:8face5895a68]]", "[[PII:", "[[[PII:EMAIL"} {
		if out, n := v.Detokenize(s); out != s || n != 0 {
			t.Fatalf("Detokenize(%q) = %q %d", s, out, n)
		}
	}
	v.Purge()
	if out, _ := v.Detokenize(res.Text); out != res.Text {
		t.Fatal("purge did not forget values")
	}
}

func TestVaultFullAndConcurrent(t *testing.T) {
	v, _ := NewVault(nil, 3)
	for i := 0; i < 3; i++ {
		if _, err := v.Tokenize(Email, fmt.Sprintf("u%d@corp.com", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := v.Tokenize(Email, "u9@corp.com"); !errors.Is(err, ErrVaultFull) {
		t.Fatalf("err %v", err)
	}
	if _, err := v.Tokenize(Email, "u1@corp.com"); err != nil {
		t.Fatal("existing value refused when full")
	}
	e := mustEngine(t, policyJSON(t, `{"version":1,"default_action":"tokenize"}`))
	res, _ := e.Process("u9@corp.com", v)
	if res.Text != "[REDACTED:EMAIL]" {
		t.Fatalf("full vault fallback %q", res.Text)
	}

	big, _ := NewVault(nil, 0)
	var wg sync.WaitGroup
	toks := make([][]string, 8)
	for g := range toks {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				tok, err := big.Tokenize(Phone, fmt.Sprintf("+4420794%05d", i))
				if err != nil {
					t.Error(err)
					return
				}
				toks[g] = append(toks[g], tok)
				big.Detokenize(tok)
			}
		}(g)
	}
	wg.Wait()
	for g := range toks {
		for i := range toks[g] {
			if toks[g][i] != toks[0][i] {
				t.Fatal("concurrent tokens differ")
			}
		}
	}
	if big.Len() != 200 {
		t.Fatalf("len %d", big.Len())
	}
}
