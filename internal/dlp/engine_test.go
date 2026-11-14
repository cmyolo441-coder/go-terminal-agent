package dlp

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

func policyJSON(t testing.TB, s string) *Policy {
	t.Helper()
	p, err := ParsePolicy([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestActions(t *testing.T) {
	p := policyJSON(t, `{"version":1,"detectors":{
		"CREDIT_CARD":{"action":"mask"},
		"US_SSN":{"action":"redact"},
		"EMAIL":{"action":"tokenize"},
		"IP_ADDRESS":{"action":"allow"}}}`)
	e := mustEngine(t, p)
	v, _ := NewVault([]byte("0123456789abcdef0123456789abcdef"), 0)
	in := "card 4111-1111-1111-1111 ssn 536-22-1234 mail jane@corp.com ip 8.8.8.8"
	res, err := e.Process(in, v)
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := v.Tokenize(Email, "jane@corp.com")
	want := "card ****-****-****-1111 ssn [REDACTED:US_SSN] mail " + tok + " ip 8.8.8.8"
	if res.Text != want {
		t.Fatalf("got  %q\nwant %q", res.Text, want)
	}
	if !res.Changed || res.Blocked {
		t.Fatalf("changed=%v blocked=%v", res.Changed, res.Blocked)
	}
	acts := map[InfoType]Action{}
	for _, f := range res.Findings {
		acts[f.Type] = f.Action
	}
	if acts[CreditCard] != ActionMask || acts[USSSN] != ActionRedact || acts[Email] != ActionTokenize || acts[IPAddress] != ActionAllow {
		t.Fatalf("actions %v", acts)
	}
	if back := e.Restore(res.Text, v); !strings.Contains(back, "jane@corp.com") {
		t.Fatalf("restore: %q", back)
	}
	// Without a vault tokenize falls back to redaction.
	res, _ = e.Process("mail jane@corp.com", nil)
	if res.Text != "mail [REDACTED:EMAIL]" || res.Findings[0].Action != ActionRedact {
		t.Fatalf("fallback: %q %v", res.Text, res.Findings)
	}
}

func TestMaskFormatPreserving(t *testing.T) {
	cases := []struct {
		t    InfoType
		in   string
		keep int
		want string
	}{
		{CreditCard, "4111 1111 1111 1111", 4, "**** **** **** 1111"},
		{USSSN, "536-22-1234", 4, "***-**-1234"},
		{IBAN, "DE89 3704 0044 0532 0130 00", 4, "**** **** **** **** **30 00"},
		{MedicalCode, "E11.9", 4, "**1.9"},
		{Phone, "+44 20 7946 0958", 0, "+** ** **** ****"},
		{Email, "jane.doe@mail.corp.com", 4, "j*******@m***.c***.com"},
	}
	for _, c := range cases {
		if got := maskValue(c.t, c.in, c.keep, '*'); got != c.want {
			t.Errorf("mask(%q) = %q, want %q", c.in, got, c.want)
		}
		if c.t != Email && len([]rune(maskValue(c.t, c.in, c.keep, '*'))) != len([]rune(c.in)) {
			t.Errorf("length not preserved for %q", c.in)
		}
	}
	e := mustEngine(t, policyJSON(t, `{"version":1,"default_action":"mask","mask_char":"#","keep_last":2}`))
	res, _ := e.Process("ssn 536-22-1234", nil)
	if res.Text != "ssn ###-##-##34" {
		t.Fatalf("got %q", res.Text)
	}
}

func TestBlock(t *testing.T) {
	e := mustEngine(t, policyJSON(t, `{"version":1,"name":"egress","detectors":{"CREDIT_CARD":{"action":"block"}}}`))
	res, err := e.Process("pay 4111111111111111 and 5555555555554444 now", nil)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v", err)
	}
	var be *BlockedError
	if !errors.As(err, &be) || be.Policy != "egress" || len(be.Reasons) != 1 {
		t.Fatalf("blocked error %#v", err)
	}
	if !res.Blocked || !strings.Contains(res.Explanation, "2 CREDIT_CARD") || strings.Contains(res.Explanation, "4111") {
		t.Fatalf("explanation %q", res.Explanation)
	}
	if strings.Contains(res.Text, "4111") || res.Report.Blocked != 1 {
		t.Fatalf("text %q report %+v", res.Text, res.Report)
	}
	if _, err := e.Process("nothing here", nil); err != nil {
		t.Fatal(err)
	}

	byLabel := mustEngine(t, policyJSON(t, `{"version":1,"block_label":"Restricted"}`))
	if _, err := byLabel.Process("ssn 536-22-1234", nil); !errors.Is(err, ErrBlocked) {
		t.Fatalf("label block: %v", err)
	}
	if _, err := byLabel.Process("mail jane@corp.com", nil); err != nil {
		t.Fatalf("confidential should pass: %v", err)
	}
	byScore := mustEngine(t, policyJSON(t, `{"version":1,"block_score":50}`))
	if _, err := byScore.Process("mail jane@corp.com", nil); err != nil {
		t.Fatalf("one email should pass: %v", err)
	}
	if _, err := byScore.Process("a@corp.com b@corp.com c@corp.com d@corp.com", nil); !errors.Is(err, ErrBlocked) {
		t.Fatalf("score block: %v", err)
	}
}

func TestReportAndClassify(t *testing.T) {
	e := mustEngine(t, nil)
	r := e.Classify("ssn 536-22-1234, mail jane@corp.com, 8.8.8.8")
	if r.Findings != 3 || r.ByType[USSSN] != 1 || r.ByType[Email] != 1 || r.ByType[IPAddress] != 1 {
		t.Fatalf("%+v", r)
	}
	if r.Label != Restricted || r.ByLabel[Restricted] != 1 || r.ByLabel[Confidential] != 1 || r.ByLabel[Internal] != 1 {
		t.Fatalf("labels %+v", r)
	}
	if r.Score <= 0 || r.Score > 100 {
		t.Fatalf("score %v", r.Score)
	}
	want := []Framework{CCPA, DPDP, GDPR, HIPAA}
	if len(r.Frameworks) != len(want) {
		t.Fatalf("frameworks %v", r.Frameworks)
	}
	for i := range want {
		if r.Frameworks[i] != want[i] {
			t.Fatalf("frameworks %v", r.Frameworks)
		}
	}
	if c := e.Classify("hello world"); c.Label != Public || c.Score != 0 || c.Findings != 0 {
		t.Fatalf("clean %+v", c)
	}
	b, err := json.Marshal(r)
	if err != nil || !strings.Contains(string(b), `"label":"Restricted"`) || !strings.Contains(string(b), `"Restricted":1`) {
		t.Fatalf("json %s %v", b, err)
	}
	// Findings never carry the raw value.
	for _, f := range e.Scan("ssn 536-22-1234") {
		fb, _ := json.Marshal(f)
		if strings.Contains(string(fb), "536-22-1234") {
			t.Fatalf("finding leaks value: %s", fb)
		}
	}

	a := NewAuditor()
	a.Record("prompt", r)
	a.Record("prompt", r)
	a.Record("web", e.Classify("jane@corp.com"))
	snap := a.Snapshot()
	if snap["prompt"].Documents != 2 || snap["prompt"].ByType[USSSN] != 2 || snap["*"].Findings != 7 || snap["*"].Label != Restricted {
		t.Fatalf("auditor %+v", snap)
	}
	a.Reset()
	if len(a.Snapshot()) != 1 {
		t.Fatal("reset")
	}
}

func TestAllowLists(t *testing.T) {
	e := mustEngine(t, policyJSON(t, `{"version":1,"allow_values":["4111111111111111"],"allow_email_domains":["corp.com"]}`))
	if fs := e.Scan("4111111111111111 jane@corp.com bob@eu.corp.com"); len(fs) != 0 {
		t.Fatalf("allow-listed values reported: %v", fs)
	}
	if fs := e.Scan("5555555555554444 jane@other.org"); len(fs) != 2 {
		t.Fatalf("got %v", fs)
	}
}

func TestCustomDetectors(t *testing.T) {
	p := policyJSON(t, `{"version":1,
		"custom_regex":[
			{"name":"EMPLOYEE_ID","pattern":"\\bEMP-\\d{6}\\b","label":"Confidential","frameworks":["SOX"],"action":"mask"},
			{"name":"CASE_REF","pattern":"#(\\d{5})","group":1,"keywords":["case"],"action":"redact"}],
		"dictionaries":[
			{"name":"CODENAME","terms":["Bluebird","night jar"],"label":"Restricted","action":"redact"},
			{"name":"TICKER","terms":["ACME"],"case_sensitive":true,"whole_word":false,"action":"tokenize"}]}`)
	e := mustEngine(t, p)
	v, _ := NewVault(nil, 0)
	res, err := e.Process("ref #54321, EMP-123456 on case #12345; project bluebird / Night Jar; bluebirds; ACMEcorp acme", v)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Text, "ref #54321, ***-**3456 on case #[REDACTED:CASE_REF];") {
		t.Fatalf("got %q", res.Text)
	}
	if strings.Count(res.Text, "[REDACTED:CODENAME]") != 2 || !strings.Contains(res.Text, "bluebirds") {
		t.Fatalf("dictionary: %q", res.Text)
	}
	if !strings.Contains(res.Text, "[[PII:TICKER:") || !strings.HasSuffix(res.Text, "corp acme") {
		t.Fatalf("ticker: %q", res.Text)
	}
	r := res.Report
	if r.ByType["CODENAME"] != 2 || r.ByType["EMPLOYEE_ID"] != 1 || r.ByType["CASE_REF"] != 1 || r.Label != Restricted {
		t.Fatalf("report %+v", r)
	}
	if e.Restore(res.Text, v) == res.Text {
		t.Fatal("ticker token not restored")
	}
}

func TestEngineConcurrent(t *testing.T) {
	e := mustEngine(t, nil)
	v, _ := NewVault(nil, 0)
	text := "ssn 536-22-1234 mail jane@corp.com card 4111111111111111 +44 20 7946 0958"
	var wg sync.WaitGroup
	outs := make([]string, 16)
	for i := range outs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				r, err := e.Process(text, v)
				if err != nil {
					t.Error(err)
					return
				}
				outs[i] = r.Text
			}
		}(i)
	}
	wg.Wait()
	for _, o := range outs {
		if o != outs[0] {
			t.Fatalf("non-deterministic output %q vs %q", o, outs[0])
		}
	}
}
