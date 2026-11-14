package dlp

import "strings"

// This file holds the checksum and structural validators the detectors use
// to reject look-alikes. Every function takes canonical input (digits only,
// or upper-case alphanumerics) and allocates nothing.

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isUpper(c byte) bool { return c >= 'A' && c <= 'Z' }
func isLower(c byte) bool { return c >= 'a' && c <= 'z' }
func isAlpha(c byte) bool { return isUpper(c) || isLower(c) }
func isAlnum(c byte) bool { return isDigit(c) || isAlpha(c) }

// isWordByte reports whether c continues an identifier; values glued to
// identifier characters are not standalone and are never reported.
func isWordByte(c byte) bool { return isAlnum(c) || c == '_' }

func isHex(c byte) bool { return isDigit(c) || (c|0x20) >= 'a' && (c|0x20) <= 'f' }

func toUpper(c byte) byte {
	if isLower(c) {
		return c - 32
	}
	return c
}

func allSame(d []byte) bool {
	for i := 1; i < len(d); i++ {
		if d[i] != d[0] {
			return false
		}
	}
	return true
}

// luhnValid reports whether the ASCII digits pass the Luhn (mod 10) check.
func luhnValid(d []byte) bool {
	if len(d) < 2 {
		return false
	}
	sum, dbl := 0, false
	for i := len(d) - 1; i >= 0; i-- {
		x := int(d[i] - '0')
		if dbl {
			x *= 2
			if x > 9 {
				x -= 9
			}
		}
		sum += x
		dbl = !dbl
	}
	return sum%10 == 0
}

// cardRange is an issuer identification number range: card numbers whose
// first plen digits fall in [lo, hi] belong to brand, with a length whose bit
// is set in lengths.
type cardRange struct {
	brand   string
	lo, hi  int
	plen    int
	lengths uint32
}

func lens(ls ...int) uint32 {
	var m uint32
	for _, l := range ls {
		m |= 1 << uint(l)
	}
	return m
}

func lenRange(a, b int) uint32 {
	var m uint32
	for l := a; l <= b; l++ {
		m |= 1 << uint(l)
	}
	return m
}

// cardRanges lists issuer ranges, most specific prefixes first so that a
// six-digit co-brand range wins over the two-digit range it sits inside.
var cardRanges = []cardRange{
	{"rupay", 652150, 653149, 6, lens(16)},
	{"discover", 622126, 622925, 6, lenRange(16, 19)},
	{"verve", 506099, 506198, 6, lenRange(16, 19)},
	{"verve", 650002, 650027, 6, lenRange(16, 19)},
	{"maestro", 676770, 676770, 6, lenRange(12, 19)},
	{"maestro", 676774, 676774, 6, lenRange(12, 19)},
	{"diners", 3095, 3095, 4, lenRange(14, 19)},
	{"discover", 6011, 6011, 4, lenRange(16, 19)},
	{"jcb", 3528, 3589, 4, lenRange(16, 19)},
	{"mir", 2200, 2204, 4, lenRange(16, 19)},
	{"mastercard", 2221, 2720, 4, lens(16)},
	{"maestro", 5018, 5018, 4, lenRange(12, 19)},
	{"maestro", 5020, 5020, 4, lenRange(12, 19)},
	{"maestro", 5038, 5038, 4, lenRange(12, 19)},
	{"maestro", 5893, 5893, 4, lenRange(12, 19)},
	{"maestro", 6304, 6304, 4, lenRange(12, 19)},
	{"maestro", 6759, 6759, 4, lenRange(12, 19)},
	{"maestro", 6761, 6763, 4, lenRange(12, 19)},
	{"troy", 9792, 9792, 4, lens(16)},
	{"diners", 300, 305, 3, lenRange(14, 19)},
	{"discover", 644, 649, 3, lenRange(16, 19)},
	{"rupay", 508, 508, 3, lens(16)},
	{"amex", 34, 34, 2, lens(15)},
	{"amex", 37, 37, 2, lens(15)},
	{"diners", 36, 36, 2, lenRange(14, 19)},
	{"diners", 38, 39, 2, lenRange(16, 19)},
	{"mastercard", 51, 55, 2, lens(16)},
	{"rupay", 60, 60, 2, lens(16)},
	{"discover", 65, 65, 2, lenRange(16, 19)},
	{"unionpay", 62, 62, 2, lenRange(16, 19)},
	{"uatp", 1, 1, 1, lens(15)},
	{"visa", 4, 4, 1, lens(13, 16, 19)},
}

// cardBrand returns the brand whose IIN range and length match the digits,
// or "" when none does.
func cardBrand(d []byte) string {
	n := len(d)
	if n < 12 || n > 19 {
		return ""
	}
	for i := range cardRanges {
		r := &cardRanges[i]
		if r.lengths&(1<<uint(n)) == 0 {
			continue
		}
		p := 0
		for _, c := range d[:r.plen] {
			p = p*10 + int(c-'0')
		}
		if p >= r.lo && p <= r.hi {
			return r.brand
		}
	}
	return ""
}

// validCard reports the brand of a valid card number (Luhn plus IIN range
// and length) or "".
func validCard(d []byte) string {
	if allSame(d) || !luhnValid(d) {
		return ""
	}
	return cardBrand(d)
}

// ibanLengths is the registered IBAN length per country (SWIFT IBAN
// registry, ISO 13616).
var ibanLengths = map[string]int{
	"AD": 24, "AE": 23, "AL": 28, "AT": 20, "AZ": 28, "BA": 20, "BE": 16, "BG": 22,
	"BH": 22, "BI": 27, "BR": 29, "BY": 28, "CH": 21, "CR": 22, "CY": 28, "CZ": 24,
	"DE": 22, "DJ": 27, "DK": 18, "DO": 28, "EE": 20, "EG": 29, "ES": 24, "FI": 18,
	"FK": 18, "FO": 18, "FR": 27, "GB": 22, "GE": 22, "GI": 23, "GL": 18, "GR": 27,
	"GT": 28, "HN": 28, "HR": 21, "HU": 28, "IE": 22, "IL": 23, "IQ": 23, "IS": 26,
	"IT": 27, "JO": 30, "KW": 30, "KZ": 20, "LB": 28, "LC": 32, "LI": 21, "LT": 20,
	"LU": 20, "LV": 21, "LY": 25, "MC": 27, "MD": 24, "ME": 22, "MK": 19, "MN": 20,
	"MR": 27, "MT": 31, "MU": 30, "NI": 28, "NL": 18, "NO": 15, "OM": 23, "PK": 24,
	"PL": 28, "PS": 29, "PT": 25, "QA": 29, "RO": 24, "RS": 22, "RU": 33, "SA": 24,
	"SC": 31, "SD": 18, "SE": 24, "SI": 19, "SK": 24, "SM": 27, "SO": 23, "ST": 25,
	"SV": 28, "TL": 23, "TN": 24, "TR": 26, "UA": 29, "VA": 22, "VG": 24, "XK": 20,
	"YE": 30,
}

// ibanValid checks an upper-case, separator-free IBAN: registered country,
// exact length, check digits 02..98, alphanumeric BBAN and mod-97 == 1.
func ibanValid(s []byte) bool {
	if len(s) < 15 || len(s) > 34 || !isUpper(s[0]) || !isUpper(s[1]) || !isDigit(s[2]) || !isDigit(s[3]) {
		return false
	}
	want, ok := ibanLengths[string(s[:2])]
	if !ok || want != len(s) {
		return false
	}
	if cd := int(s[2]-'0')*10 + int(s[3]-'0'); cd < 2 || cd > 98 {
		return false
	}
	rem := 0
	step := func(c byte) bool {
		switch {
		case isDigit(c):
			rem = (rem*10 + int(c-'0')) % 97
		case isUpper(c):
			rem = (rem*100 + int(c-'A') + 10) % 97
		default:
			return false
		}
		return true
	}
	for _, c := range s[4:] {
		if !step(c) {
			return false
		}
	}
	for _, c := range s[:4] {
		step(c)
	}
	return rem == 1
}

// ssnValid applies the SSA rules to nine ASCII digits: area not 000, 666 or
// 900-999; group not 00; serial not 0000; and none of the numbers the SSA
// has voided after public misuse.
func ssnValid(d []byte) bool {
	if len(d) != 9 {
		return false
	}
	area := int(d[0]-'0')*100 + int(d[1]-'0')*10 + int(d[2]-'0')
	group := int(d[3]-'0')*10 + int(d[4]-'0')
	serial := int(d[5]-'0')*1000 + int(d[6]-'0')*100 + int(d[7]-'0')*10 + int(d[8]-'0')
	if area == 0 || area == 666 || area >= 900 || group == 0 || serial == 0 {
		return false
	}
	switch string(d) {
	case "078051120", "219099999", "123456789", "111111111", "333333333":
		return false
	}
	return true
}

var verhoeffD = [10][10]uint8{
	{0, 1, 2, 3, 4, 5, 6, 7, 8, 9},
	{1, 2, 3, 4, 0, 6, 7, 8, 9, 5},
	{2, 3, 4, 0, 1, 7, 8, 9, 5, 6},
	{3, 4, 0, 1, 2, 8, 9, 5, 6, 7},
	{4, 0, 1, 2, 3, 9, 5, 6, 7, 8},
	{5, 9, 8, 7, 6, 0, 4, 3, 2, 1},
	{6, 5, 9, 8, 7, 1, 0, 4, 3, 2},
	{7, 6, 5, 9, 8, 2, 1, 0, 4, 3},
	{8, 7, 6, 5, 9, 3, 2, 1, 0, 4},
	{9, 8, 7, 6, 5, 4, 3, 2, 1, 0},
}

var verhoeffP = [8][10]uint8{
	{0, 1, 2, 3, 4, 5, 6, 7, 8, 9},
	{1, 5, 7, 6, 2, 8, 3, 0, 9, 4},
	{5, 8, 0, 3, 7, 9, 6, 1, 4, 2},
	{8, 9, 1, 6, 0, 4, 3, 5, 2, 7},
	{9, 4, 5, 3, 1, 2, 6, 8, 7, 0},
	{4, 2, 8, 6, 5, 7, 3, 9, 0, 1},
	{2, 7, 9, 3, 8, 0, 6, 4, 1, 5},
	{7, 0, 4, 6, 9, 1, 3, 2, 5, 8},
}

var verhoeffInv = [10]uint8{0, 4, 3, 2, 1, 5, 6, 7, 8, 9}

// verhoeffValid reports whether the ASCII digits, check digit last, pass
// the Verhoeff dihedral-group check.
func verhoeffValid(d []byte) bool {
	c := uint8(0)
	for i := 0; i < len(d); i++ {
		c = verhoeffD[c][verhoeffP[i%8][d[len(d)-1-i]-'0']]
	}
	return c == 0
}

// verhoeffDigit computes the Verhoeff check digit for the ASCII digits.
func verhoeffDigit(d []byte) byte {
	c := uint8(0)
	for i := 0; i < len(d); i++ {
		c = verhoeffD[c][verhoeffP[(i+1)%8][d[len(d)-1-i]-'0']]
	}
	return '0' + verhoeffInv[c]
}

// aadhaarValid checks twelve ASCII digits: the first is 2-9 (UIDAI never
// issues numbers starting with 0 or 1) and the Verhoeff check passes.
func aadhaarValid(d []byte) bool {
	return len(d) == 12 && d[0] >= '2' && !allSame(d) && verhoeffValid(d)
}

// panEntityTypes are the valid fourth characters of an Indian PAN: the
// holder's entity type (Person, Company, HUF, Firm, AOP, Trust, BOI, Local
// authority, Artificial juridical person, Government).
const panEntityTypes = "PCHFATBLJG"

// panValid checks the Indian Permanent Account Number structure
// AAAEA9999A where E is the entity type.
func panValid(s string) bool {
	if len(s) != 10 {
		return false
	}
	for i := 0; i < 5; i++ {
		if !isUpper(s[i]) {
			return false
		}
	}
	if strings.IndexByte(panEntityTypes, s[3]) < 0 {
		return false
	}
	for i := 5; i < 9; i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return isUpper(s[9]) && s[5:9] != "0000"
}

// ninoValid checks a UK National Insurance number written without spaces:
// two prefix letters (first not D, F, I, Q, U, V; second not D, F, I, O, Q,
// U, V; not an administrative prefix), six digits and a suffix A-D.
func ninoValid(s string) bool {
	if len(s) != 9 {
		return false
	}
	a, b := s[0], s[1]
	if !isUpper(a) || !isUpper(b) || strings.IndexByte("DFIQUV", a) >= 0 || strings.IndexByte("DFIOQUV", b) >= 0 {
		return false
	}
	switch s[:2] {
	case "BG", "GB", "KN", "NK", "NT", "TN", "ZZ":
		return false
	}
	for i := 2; i < 8; i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return s[8] >= 'A' && s[8] <= 'D'
}

// icaoValue is the ICAO 9303 numeric value of an MRZ character.
func icaoValue(c byte) int {
	switch {
	case isDigit(c):
		return int(c - '0')
	case isUpper(c):
		return int(c-'A') + 10
	}
	return 0 // '<' filler
}

// icaoCheck computes the ICAO 9303 check digit (weights 7, 3, 1) over the
// concatenation of the fields.
func icaoCheck(fields ...string) byte {
	w := [3]int{7, 3, 1}
	sum, k := 0, 0
	for _, f := range fields {
		for i := 0; i < len(f); i++ {
			sum += icaoValue(f[i]) * w[k%3]
			k++
		}
	}
	return byte('0' + sum%10)
}

// icaoField checks a field against its check digit. An all-filler optional
// field may carry '<' as its check digit.
func icaoField(field string, check byte) bool {
	if check == '<' {
		return strings.Trim(field, "<") == ""
	}
	return icaoCheck(field) == check
}

// mrzDate checks a YYMMDD MRZ date.
func mrzDate(s string) bool {
	if len(s) != 6 {
		return false
	}
	for i := 0; i < 6; i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	m := int(s[2]-'0')*10 + int(s[3]-'0')
	d := int(s[4]-'0')*10 + int(s[5]-'0')
	// The century is unknown; 2000 is a leap year so 29 February passes.
	return validDate(2000+int(s[0]-'0')*10+int(s[1]-'0'), m, d)
}

func mrzCharset(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isUpper(c) && !isDigit(c) && c != '<' {
			return false
		}
	}
	return true
}

// mrzTD3Line2 validates the second line of a passport (TD3) MRZ.
func mrzTD3Line2(l string) bool {
	return len(l) == 44 &&
		icaoField(l[0:9], l[9]) && mrzDate(l[13:19]) && icaoField(l[13:19], l[19]) &&
		strings.IndexByte("MFX<", l[20]) >= 0 &&
		mrzDate(l[21:27]) && icaoField(l[21:27], l[27]) && icaoField(l[28:42], l[42]) &&
		icaoCheck(l[0:10], l[13:20], l[21:43]) == l[43]
}

// mrzTD2Line2 validates the second line of a TD2 MRZ.
func mrzTD2Line2(l string) bool {
	return len(l) == 36 &&
		icaoField(l[0:9], l[9]) && mrzDate(l[13:19]) && icaoField(l[13:19], l[19]) &&
		strings.IndexByte("MFX<", l[20]) >= 0 &&
		mrzDate(l[21:27]) && icaoField(l[21:27], l[27]) &&
		icaoCheck(l[0:10], l[13:20], l[21:35]) == l[35]
}

// mrzTD1 validates the first two lines of an identity-card (TD1) MRZ.
func mrzTD1(l1, l2 string) bool {
	return len(l1) == 30 && len(l2) == 30 &&
		icaoField(l1[5:14], l1[14]) && mrzDate(l2[0:6]) && icaoField(l2[0:6], l2[6]) &&
		strings.IndexByte("MFX<", l2[7]) >= 0 &&
		mrzDate(l2[8:14]) && icaoField(l2[8:14], l2[14]) &&
		icaoCheck(l1[5:30], l2[0:7], l2[8:15], l2[18:29]) == l2[29]
}

// nhsValid checks a ten-digit NHS number with its modulus-11 check digit.
func nhsValid(d []byte) bool {
	if len(d) != 10 || allSame(d) {
		return false
	}
	sum := 0
	for i := 0; i < 9; i++ {
		sum += int(d[i]-'0') * (10 - i)
	}
	c := 11 - sum%11
	if c == 11 {
		c = 0
	}
	return c != 10 && c == int(d[9]-'0')
}

func leap(y int) bool { return y%4 == 0 && (y%100 != 0 || y%400 == 0) }

// validDate reports whether y-m-d is a real calendar date.
func validDate(y, m, d int) bool {
	if m < 1 || m > 12 || d < 1 {
		return false
	}
	days := [13]int{0, 31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}
	if m == 2 && leap(y) {
		return d <= 29
	}
	return d <= days[m]
}

// e164Codes is the set of assigned ITU-T E.164 country calling codes.
var e164Codes = func() map[string]bool {
	m := map[string]bool{}
	add := func(codes ...string) {
		for _, c := range codes {
			m[c] = true
		}
	}
	addRange := func(lo, hi int) {
		for c := lo; c <= hi; c++ {
			m[itoa(c)] = true
		}
	}
	add("1", "7", "20", "27", "30", "31", "32", "33", "34", "36", "39", "40", "41")
	addRange(43, 49)
	addRange(51, 58)
	addRange(60, 66)
	add("81", "82", "84", "86")
	addRange(90, 95)
	add("98", "211", "212", "213", "216", "218")
	addRange(220, 258)
	addRange(260, 269)
	add("290", "291", "297", "298", "299")
	addRange(350, 359)
	addRange(370, 378)
	add("380", "381", "382", "383", "385", "386", "387", "389", "420", "421", "423")
	addRange(500, 509)
	addRange(590, 599)
	add("670")
	addRange(672, 683)
	addRange(685, 692)
	add("800", "808", "850", "852", "853", "855", "856", "870", "878", "880", "881", "882", "883", "886", "888")
	addRange(960, 968)
	addRange(970, 977)
	add("979")
	addRange(992, 996)
	add("998")
	return m
}()

// e164CountryCode returns the country calling code that prefixes the
// digits, or "" if none is assigned. E.164 codes are prefix-free.
func e164CountryCode(d []byte) string {
	for l := 1; l <= 3 && l < len(d); l++ {
		if e164Codes[string(d[:l])] {
			return string(d[:l])
		}
	}
	return ""
}

// phoneValid checks an international number's digits (without '+'): an
// assigned country code, a plausible total length, and the NANP rules for
// country code 1.
func phoneValid(d []byte) (string, bool) {
	if len(d) < 8 || len(d) > 15 || d[0] == '0' {
		return "", false
	}
	cc := e164CountryCode(d)
	if cc == "" {
		return "", false
	}
	if cc == "1" && (len(d) != 11 || d[1] < '2' || d[4] < '2') {
		return "", false
	}
	if allSame(d[len(cc):]) {
		return "", false
	}
	return cc, true
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
