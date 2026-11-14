package dlp

import (
	"strings"
	"testing"
)

func mustEngine(t testing.TB, p *Policy) *Engine {
	t.Helper()
	e, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// found returns the values of findings of type typ in text.
func found(e *Engine, text string, typ InfoType) []string {
	var out []string
	for _, f := range e.Scan(text) {
		if f.Type == typ {
			out = append(out, text[f.Start:f.End])
		}
	}
	return out
}

type detCase struct {
	text string
	want []string // expected values; nil means none
}

func runDetCases(t *testing.T, e *Engine, typ InfoType, cases []detCase) {
	t.Helper()
	for _, c := range cases {
		got := found(e, c.text, typ)
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%s in %q: got %q, want %q", typ, c.text, got, c.want)
		}
	}
}

func TestDetectCreditCard(t *testing.T) {
	runDetCases(t, mustEngine(t, nil), CreditCard, []detCase{
		{"pay with 4111111111111111 today", []string{"4111111111111111"}},
		{"card: 4111 1111 1111 1111.", []string{"4111 1111 1111 1111"}},
		{"amex 3782-822463-10005", []string{"3782-822463-10005"}},
		{"mc 5555 5555 5555 4444 exp 12/29", []string{"5555 5555 5555 4444"}},
		{"Order 1234 4111111111111111 shipped", []string{"4111111111111111"}},
		{"jcb=3530111333300000;", []string{"3530111333300000"}},
		{"bad luhn 4111111111111112", nil},
		{"glued x4111111111111111", nil},
		{"glued 4111111111111111abc", nil},
		{"id_4111111111111111", nil},
		{"float 3.4111111111111111", nil},
		{"mixed seps 4111-1111 1111-1111", nil},
		{"timestamp 1700000000000000", nil},
	})
}

func TestDetectIBAN(t *testing.T) {
	runDetCases(t, mustEngine(t, nil), IBAN, []detCase{
		{"IBAN: DE89 3704 0044 0532 0130 00 thanks", []string{"DE89 3704 0044 0532 0130 00"}},
		{"to GB82WEST12345698765432.", []string{"GB82WEST12345698765432"}},
		{"FR14 2004 1010 0505 0001 3M02 606", []string{"FR14 2004 1010 0505 0001 3M02 606"}},
		{"NL91ABNA0417164300 and NO9386011117947", []string{"NL91ABNA0417164300", "NO9386011117947"}},
		{"DE89370400440532013001", nil},
		{"DE893704004405320130001", nil},
		{"DE89370400440532013000X", nil},
		{"XX89370400440532013000", nil},
	})
}

func TestDetectSSN(t *testing.T) {
	runDetCases(t, mustEngine(t, nil), USSSN, []detCase{
		{"employee 536-22-1234 onboarded", []string{"536-22-1234"}},
		{"SSN 536221234", []string{"536221234"}},
		{"social security number: 536 22 1234", []string{"536 22 1234"}},
		{"order 536221234", nil},
		{"536 22 1234", nil}, // spaced without context is too weak
		{"666-22-1234", nil},
		{"900-22-1234", nil},
		{"536-00-1234", nil},
		{"536-22-0000", nil},
		{"078-05-1120", nil},
		{"tel 536-22-12345", nil},
	})
}

func TestDetectAadhaar(t *testing.T) {
	runDetCases(t, mustEngine(t, nil), Aadhaar, []detCase{
		{"Aadhaar: 2345 6789 0124", []string{"2345 6789 0124"}},
		{"id 2345-6789-0124.", []string{"2345-6789-0124"}},
		{"aadhaar number 234567890124", []string{"234567890124"}},
		{"value 234567890124", nil}, // bare 12 digits need context
		{"Aadhaar: 2345 6789 0123", nil},
		{"Aadhaar: 1345 6789 0124", nil},
		{"4111 2345 6789 0124", nil}, // ambiguous grouping
	})
}

func TestDetectPANAndNINO(t *testing.T) {
	e := mustEngine(t, nil)
	runDetCases(t, e, IndiaPAN, []detCase{
		{"PAN ABCPE1234F filed", []string{"ABCPE1234F"}},
		{"company AAACB1234Z.", []string{"AAACB1234Z"}},
		{"ABCDE1234F", nil},
		{"abcpe1234f", nil},
		{"XABCPE1234F", nil},
	})
	runDetCases(t, e, UKNINO, []detCase{
		{"NI number AB123456C", []string{"AB123456C"}},
		{"nino: AB 12 34 56 C.", []string{"AB 12 34 56 C"}},
		{"QQ123456C", nil},
		{"GB123456A", nil},
		{"AB123456E", nil},
		{"AB123456CD", nil},
	})
}

func TestDetectEmail(t *testing.T) {
	runDetCases(t, mustEngine(t, nil), Email, []detCase{
		{"contact jane.doe+news@corp.co.uk now", []string{"jane.doe+news@corp.co.uk"}},
		{"<j_smith@mail.acme.io>", []string{"j_smith@mail.acme.io"}},
		{"mailto:Jane@Example.org", nil}, // reserved domain
		{"noreply@github.com", nil},      // role account
		{"icon@2x.png", nil},
		{"pkg@1.2.3", nil},
		{"@types/node", nil},
		{"a..b@corp.com", nil},
		{"user@localhost", nil},
		{"user@corp", nil},
		{"user@-corp.com", nil},
	})
}

func TestDetectPhone(t *testing.T) {
	runDetCases(t, mustEngine(t, nil), Phone, []detCase{
		{"call +44 20 7946 0958 now", []string{"+44 20 7946 0958"}},
		{"+1 (415) 555-2671", []string{"+1 (415) 555-2671"}},
		{"+919876543210", []string{"+919876543210"}},
		{"phone: (415) 555-2671", []string{"(415) 555-2671"}},
		{"mobile 98765 43210", []string{"98765 43210"}},
		{"x+14155552671", nil},
		{"+1 115 555 2671", nil},     // NANP area code
		{"+28 1234 5678", nil},       // unassigned country code
		{"+44 20 7946 0958 1234", nil}, // too long
		{"1+2", nil},
		{"version 4155552671", nil},
	})
}

func TestDetectIP(t *testing.T) {
	e := mustEngine(t, nil)
	runDetCases(t, e, IPAddress, []detCase{
		{"client 8.8.8.8 connected", []string{"8.8.8.8"}},
		{"ip=203.0.114.7", []string{"203.0.114.7"}},
		{"addr 2001:4860:4860::8888.", []string{"2001:4860:4860::8888"}},
		{"10.0.0.1 and 127.0.0.1 and 192.168.1.1", nil},
		{"fe80::1 ::1", nil},
		{"doc 192.0.2.1 2001:db8::1", nil},
		{"version 1.2.3.4", nil},
		{"v1.2.3.4", nil},
		{"256.1.1.1", nil},
		{"01.2.3.4", nil},
		{"1.2.3.4.5", nil},
		{"time 12:30:45", nil},
		{"x[1::2] std::vector a::b", nil},
		{"mac 00:1A:2B:3C:4D:5E", nil},
	})
	priv := mustEngine(t, &Policy{Version: 1, IP: IPPolicy{IncludePrivate: true}})
	runDetCases(t, priv, IPAddress, []detCase{
		{"10.0.0.1 and fe80::1", []string{"10.0.0.1", "fe80::1"}},
	})
	v4only := mustEngine(t, &Policy{Version: 1, IP: IPPolicy{DisableV6: true}})
	runDetCases(t, v4only, IPAddress, []detCase{
		{"8.8.8.8 2001:4860:4860::8888", []string{"8.8.8.8"}},
	})
}

func TestDetectDOB(t *testing.T) {
	runDetCases(t, mustEngine(t, nil), DateOfBirth, []detCase{
		{"DOB: 12/05/1985", []string{"12/05/1985"}},
		{"date of birth 1985-03-12", []string{"1985-03-12"}},
		{"born on 3rd March 1979", []string{"3rd March 1979"}},
		{"Birthday: Sept. 9, 1990", []string{"Sept. 9, 1990"}},
		{"d.o.b. 29.02.2000", []string{"29.02.2000"}},
		{"released 12/05/1985", nil}, // no birth context
		{"DOB: 31/02/1985", nil},
		{"DOB: 12/05/1885", nil},
		{"DOB: 12/05/2999", nil},
		{"born on 3 Marmalade 1979", nil},
	})
}

func TestDetectPassportMRZ(t *testing.T) {
	e := mustEngine(t, nil)
	td3 := td3L1 + "\n" + td3L2
	td1 := td1L1 + "\r\n" + td1L2 + "\r\n" + td1L3
	td2 := td2L1 + "\n" + td2L2
	runDetCases(t, e, Passport, []detCase{
		{"scan:\n" + td3 + "\nend", []string{td3}},
		{td1 + "\n", []string{td1L1 + "\r\n" + td1L2 + "\r\n" + td1L3}},
		{td2, []string{td2}},
		{"line " + "\n" + td3L2, []string{td3L2}},
		{"Passport No: X1234567", []string{"X1234567"}},
		{"passport number is 123456789.", []string{"123456789"}},
		{td3L1 + "\n" + strings.Replace(td3L2, "L898902C3", "L898902C4", 1), nil},
		{"passport: NUMBERS", nil},
		{"P<<<<<<", nil},
	})
}

func TestDetectMedical(t *testing.T) {
	e := mustEngine(t, nil)
	runDetCases(t, e, MedicalRecord, []detCase{
		{"MRN: 00123456", []string{"00123456"}},
		{"patient id P-2024-00917", []string{"P-2024-00917"}},
		{"NHS number 943 476 5919", []string{"943 476 5919"}},
		{"NHS number 943 476 5918", nil},
		{"order 00123456", nil},
	})
	runDetCases(t, e, MedicalCode, []detCase{
		{"Diagnosis: E11.9, I10 and J45.909", []string{"E11.9", "I10", "J45.909"}},
		{"dx C50.911", []string{"C50.911"}},
		{"the E11 bus", nil},
		{"diagnosis: U07.1", nil}, // U codes are reserved
	})
}

func TestContextBoost(t *testing.T) {
	e := mustEngine(t, nil)
	plain := e.Scan("value 2345 6789 0124 here")
	ctx := e.Scan("aadhaar 2345 6789 0124 here")
	if len(plain) != 1 || len(ctx) != 1 {
		t.Fatalf("got %v / %v", plain, ctx)
	}
	if !(ctx[0].Confidence > plain[0].Confidence) || !ctx[0].Context || plain[0].Context {
		t.Fatalf("context did not boost: %v vs %v", plain[0], ctx[0])
	}
	// Keywords inside other words are not context.
	if f := e.Scan("discard 536221234"); len(f) != 0 {
		t.Fatalf("got %v", f)
	}
	// A keyword far away is not context.
	if f := e.Scan("ssn" + strings.Repeat(" ", 200) + "536221234"); len(f) != 0 {
		t.Fatalf("got %v", f)
	}
}

func TestOverlapResolution(t *testing.T) {
	e := mustEngine(t, nil)
	// A 16-digit card contains a Verhoeff-valid 12-digit prefix candidate
	// in some numbers; the card must win and nothing else overlap it.
	text := "card 4111 1111 1111 1111"
	fs := e.Scan(text)
	if len(fs) != 1 || fs[0].Type != CreditCard {
		t.Fatalf("got %v", fs)
	}
}

func TestDetectorsDisabled(t *testing.T) {
	off := false
	p := &Policy{Version: 1, Detectors: map[InfoType]DetectorPolicy{CreditCard: {Enabled: &off}, Email: {Enabled: &off}}}
	e := mustEngine(t, p)
	if fs := e.Scan("4111111111111111 jane@corp.com"); len(fs) != 0 {
		t.Fatalf("disabled detectors reported %v", fs)
	}
}

func TestCleanTextNoFindings(t *testing.T) {
	e := mustEngine(t, nil)
	src := `package main

import "fmt"

// Version 1.2.3 of the build, see https://golang.org/doc and RFC 3339.
func main() {
	x := map[string]int{"a": 1, "b": 22}
	for i := 0; i < 10; i++ { fmt.Println(i, x["a"]*1024, 3.14159, 0xFF) }
	ts := "2024-01-15T10:00:00Z" // 1700000000
	_ = ts
	ver := "go1.26.0"; sha := "3f2a9c21d0e4b5a1"; uuid := "123e4567-e89b-12d3-a456-426614174000"
	std::vector<int> v; a[1::2]; port := ":8080"; host := "localhost:8080"
	println(ver, sha, uuid, port, host)
}`
	if fs := e.Scan(src); len(fs) != 0 {
		t.Fatalf("false positives in clean code: %v", fs)
	}
}
