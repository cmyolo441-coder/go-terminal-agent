package dlp

import "testing"

func TestLuhnAndBrands(t *testing.T) {
	valid := map[string]string{
		"4111111111111111": "visa",
		"4012888888881881": "visa",
		"4222222222222":    "visa",
		"4000056655665556": "visa",
		"5555555555554444": "mastercard",
		"5105105105105100": "mastercard",
		"2223003122003222": "mastercard",
		"378282246310005":  "amex",
		"371449635398431":  "amex",
		"6011111111111117": "discover",
		"6011000990139424": "discover",
		"3530111333300000": "jcb",
		"3566002020360505": "jcb",
		"30569309025904":   "diners",
		"36227206271667":   "diners",
		"6200000000000005": "unionpay",
		"6759649826438453": "maestro",
		"2200000000000004": "mir",
	}
	for n, brand := range valid {
		if got := validCard([]byte(n)); got != brand {
			t.Errorf("validCard(%s) = %q, want %q", n, got, brand)
		}
	}
	invalid := []string{
		"4111111111111112",     // Luhn
		"0000000000000000",     // repeated, no IIN
		"1234567812345670",     // Luhn-valid, UATP needs 15 digits
		"9111111111111111",     // no issuer
		"37828224631000",       // amex, wrong length
		"5555555555554",        // mastercard, wrong length
		"411111111111111",      // visa, 15 digits
		"41111111111111111111", // too long
	}
	for _, n := range invalid {
		if got := validCard([]byte(n)); got != "" {
			t.Errorf("validCard(%s) = %q, want invalid", n, got)
		}
	}
}

func TestIBAN(t *testing.T) {
	valid := []string{
		"DE89370400440532013000",
		"GB82WEST12345698765432",
		"FR1420041010050500013M02606",
		"NL91ABNA0417164300",
		"ES9121000418450200051332",
		"CH9300762011623852957",
		"BE68539007547034",
		"IT60X0542811101000000123456",
		"NO9386011117947",
		"AT611904300234573201",
		"PL61109010140000071219812874",
		"SA0380000000608010167519",
	}
	for _, s := range valid {
		if !ibanValid([]byte(s)) {
			t.Errorf("ibanValid(%s) = false", s)
		}
	}
	invalid := []string{
		"DE89370400440532013001", // checksum
		"DE8937040044053201300",  // length for DE
		"XX89370400440532013000", // unknown country
		"GB82WEST1234569876543!", // character
		"DE00370400440532013000", // check digits 00
		"NO938601111794",         // short
	}
	for _, s := range invalid {
		if ibanValid([]byte(s)) {
			t.Errorf("ibanValid(%s) = true", s)
		}
	}
}

func TestSSN(t *testing.T) {
	for _, s := range []string{"536221234", "001010001", "665999999", "899451120", "078051121"} {
		if !ssnValid([]byte(s)) {
			t.Errorf("ssnValid(%s) = false", s)
		}
	}
	for _, s := range []string{
		"000121234", "666121234", "900121234", "999121234", // area
		"536001234",                           // group
		"536220000",                           // serial
		"078051120", "219099999", "123456789", // voided
		"53622123",
	} {
		if ssnValid([]byte(s)) {
			t.Errorf("ssnValid(%s) = true", s)
		}
	}
}

func TestVerhoeffAndAadhaar(t *testing.T) {
	// Published Verhoeff example: the check digit for 236 is 3.
	if c := verhoeffDigit([]byte("236")); c != '3' {
		t.Fatalf("verhoeffDigit(236) = %c, want 3", c)
	}
	if !verhoeffValid([]byte("2363")) || verhoeffValid([]byte("2364")) {
		t.Fatal("verhoeffValid wrong on 2363/2364")
	}
	for _, s := range []string{"234567890124", "987654321096", "500000000006"} {
		if !aadhaarValid([]byte(s)) {
			t.Errorf("aadhaarValid(%s) = false", s)
		}
	}
	for _, s := range []string{"234567890123", "123456789012", "012345678901", "222222222222", "23456789012"} {
		if aadhaarValid([]byte(s)) {
			t.Errorf("aadhaarValid(%s) = true", s)
		}
	}
	// Every single-digit error is caught.
	base := []byte("234567890124")
	for i := range base {
		for d := byte('0'); d <= '9'; d++ {
			if d == base[i] {
				continue
			}
			m := append([]byte(nil), base...)
			m[i] = d
			if verhoeffValid(m) {
				t.Fatalf("single-digit error %s not caught", m)
			}
		}
	}
	// And every adjacent transposition.
	for i := 0; i+1 < len(base); i++ {
		if base[i] == base[i+1] {
			continue
		}
		m := append([]byte(nil), base...)
		m[i], m[i+1] = m[i+1], m[i]
		if verhoeffValid(m) {
			t.Fatalf("transposition %s not caught", m)
		}
	}
}

func TestPANAndNINO(t *testing.T) {
	for _, s := range []string{"ABCPE1234F", "AAACB1234Z", "BNZAA2318J"} {
		if !panValid(s) {
			t.Errorf("panValid(%s) = false", s)
		}
	}
	for _, s := range []string{"ABCDE1234F", "ABCPE0000F", "ABCP1234F", "abcpe1234f", "ABCPE12345"} {
		if panValid(s) {
			t.Errorf("panValid(%s) = true", s)
		}
	}
	for _, s := range []string{"AB123456C", "JG103759A", "CE123456D"} {
		if !ninoValid(s) {
			t.Errorf("ninoValid(%s) = false", s)
		}
	}
	for _, s := range []string{"QQ123456C", "DA123456A", "AO123456A", "GB123456A", "AB123456E", "AB12345C", "ZZ123456A"} {
		if ninoValid(s) {
			t.Errorf("ninoValid(%s) = true", s)
		}
	}
}

// ICAO 9303 specimen documents.
const (
	td3L1 = "P<UTOERIKSSON<<ANNA<MARIA<<<<<<<<<<<<<<<<<<<"
	td3L2 = "L898902C36UTO7408122F1204159ZE184226B<<<<<10"
	td1L1 = "I<UTOD231458907<<<<<<<<<<<<<<<"
	td1L2 = "7408122F1204159UTO<<<<<<<<<<<6"
	td1L3 = "ERIKSSON<<ANNA<MARIA<<<<<<<<<<"
	td2L1 = "I<UTOERIKSSON<<ANNA<MARIA<<<<<<<<<<<"
	td2L2 = "D231458907UTO7408122F1204159<<<<<<<6"
)

func TestICAO(t *testing.T) {
	if c := icaoCheck("L898902C3"); c != '6' {
		t.Fatalf("icaoCheck = %c, want 6", c)
	}
	if !mrzTD3Line2(td3L2) {
		t.Error("TD3 specimen rejected")
	}
	if !mrzTD1(td1L1, td1L2) {
		t.Error("TD1 specimen rejected")
	}
	if !mrzTD2Line2(td2L2) {
		t.Error("TD2 specimen rejected")
	}
	bad := []byte(td3L2)
	bad[5] = '8'
	if mrzTD3Line2(string(bad)) {
		t.Error("TD3 with altered document number accepted")
	}
	bad = []byte(td3L2)
	bad[43] = '1'
	if mrzTD3Line2(string(bad)) {
		t.Error("TD3 with wrong composite accepted")
	}
	bad = []byte(td3L2)
	bad[15] = '9' // month 92
	if mrzTD3Line2(string(bad)) {
		t.Error("TD3 with invalid birth date accepted")
	}
}

func TestNHSAndPhone(t *testing.T) {
	if !nhsValid([]byte("9434765919")) {
		t.Error("NHS 9434765919 rejected")
	}
	for _, s := range []string{"9434765918", "1111111111", "943476591"} {
		if nhsValid([]byte(s)) {
			t.Errorf("nhsValid(%s) = true", s)
		}
	}
	valid := map[string]string{"14155552671": "1", "442079460958": "44", "919876543210": "91", "35312345678": "353", "861012345678": "86"}
	for d, cc := range valid {
		if got, ok := phoneValid([]byte(d)); !ok || got != cc {
			t.Errorf("phoneValid(%s) = %q %v, want %q", d, got, ok, cc)
		}
	}
	for _, d := range []string{"11155552671", "1415555267", "2812345678", "0441234567", "4412", "1234567890123456", "4400000000"} {
		if _, ok := phoneValid([]byte(d)); ok {
			t.Errorf("phoneValid(%s) = true", d)
		}
	}
}

func TestValidDate(t *testing.T) {
	cases := []struct {
		y, m, d int
		ok      bool
	}{{2000, 2, 29, true}, {1900, 2, 29, false}, {2023, 2, 29, false}, {2024, 4, 31, false}, {2024, 12, 31, true}, {2024, 13, 1, false}, {2024, 0, 1, false}}
	for _, c := range cases {
		if validDate(c.y, c.m, c.d) != c.ok {
			t.Errorf("validDate(%d,%d,%d) != %v", c.y, c.m, c.d, c.ok)
		}
	}
}
