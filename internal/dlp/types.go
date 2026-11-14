// Package dlp is the data-loss-prevention layer for everything that leaves
// the machine: prompts sent to a model, tool output folded back into the
// conversation, and outbound web requests.
//
// The secrets package already handles credentials (API keys, tokens, private
// keys). This package covers personal and regulated data: payment cards,
// bank accounts, national identifiers, contact details, network addresses,
// dates of birth, travel documents and health information.
//
// # Detection
//
// Every built-in detector validates what it finds instead of trusting a
// pattern: card numbers must pass Luhn and fall in an issuer (IIN) range
// whose lengths match, IBANs must pass ISO 13616 mod-97 at the registered
// per-country length, US SSNs must respect the area/group/serial rules,
// Aadhaar numbers must pass Verhoeff, MRZ lines must pass the ICAO 9303
// check digits, and so on. Weak signals (a bare nine-digit number, a date,
// an ICD-10 code) are only reported next to a context keyword ("ssn", "date
// of birth", "diagnosis"), and every finding carries a confidence that
// context keywords raise. Context keywords, custom dictionaries and custom
// keyword-anchored regular expressions are all matched in a single pass by an
// Aho–Corasick automaton.
//
// # Classification and actions
//
// Each finding carries a sensitivity label (Public, Internal, Confidential,
// Restricted) and the regulatory frameworks it falls under (GDPR, HIPAA,
// PCI-DSS, DPDP-India, ...). A policy chooses what happens to each type:
// allow it, mask it in a format-preserving way, redact it, replace it with a
// reversible token from a session Vault, or block the whole payload with an
// explanation. A document-level score and label summarise the payload for
// audit.
//
// # Streaming
//
// Writer and Reader wrap an io.Writer or io.Reader and apply the policy to a
// stream in bounded memory; a carry-over window guarantees that a value split
// across chunk boundaries is still detected.
package dlp

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// InfoType names a kind of sensitive data, e.g. "CREDIT_CARD". Custom
// detectors and dictionaries define their own InfoTypes.
type InfoType string

// Built-in information types.
const (
	CreditCard    InfoType = "CREDIT_CARD"
	IBAN          InfoType = "IBAN"
	USSSN         InfoType = "US_SSN"
	Aadhaar       InfoType = "IN_AADHAAR"
	IndiaPAN      InfoType = "IN_PAN"
	UKNINO        InfoType = "UK_NINO"
	Email         InfoType = "EMAIL"
	Phone         InfoType = "PHONE"
	IPAddress     InfoType = "IP_ADDRESS"
	DateOfBirth   InfoType = "DATE_OF_BIRTH"
	Passport      InfoType = "PASSPORT"
	MedicalRecord InfoType = "MEDICAL_RECORD"
	MedicalCode   InfoType = "MEDICAL_CODE"
)

// Label is a sensitivity classification. Labels are ordered: a higher value
// is more sensitive.
type Label uint8

// Sensitivity labels, least to most sensitive.
const (
	Public Label = iota
	Internal
	Confidential
	Restricted
)

var labelNames = [...]string{"Public", "Internal", "Confidential", "Restricted"}

// String returns the canonical name of the label.
func (l Label) String() string {
	if int(l) < len(labelNames) {
		return labelNames[l]
	}
	return fmt.Sprintf("Label(%d)", uint8(l))
}

// ParseLabel parses a label name, ignoring case.
func ParseLabel(s string) (Label, error) {
	for i, n := range labelNames {
		if strings.EqualFold(s, n) {
			return Label(i), nil
		}
	}
	return 0, fmt.Errorf("dlp: unknown label %q (want Public, Internal, Confidential or Restricted)", s)
}

// MarshalText implements encoding.TextMarshaler.
func (l Label) MarshalText() ([]byte, error) {
	if int(l) >= len(labelNames) {
		return nil, fmt.Errorf("dlp: invalid label %d", uint8(l))
	}
	return []byte(labelNames[l]), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (l *Label) UnmarshalText(b []byte) error {
	v, err := ParseLabel(string(b))
	if err != nil {
		return err
	}
	*l = v
	return nil
}

// labelWeight is how much a finding with this label contributes to the
// document score, per unit of confidence.
func labelWeight(l Label) float64 {
	switch l {
	case Internal:
		return 1
	case Confidential:
		return 3
	case Restricted:
		return 10
	}
	return 0
}

// Framework names a regulatory or compliance framework.
type Framework string

// Well-known frameworks. Policies may name others (for example "SOX").
const (
	GDPR   Framework = "GDPR"
	UKGDPR Framework = "UK-GDPR"
	HIPAA  Framework = "HIPAA"
	PCIDSS Framework = "PCI-DSS"
	DPDP   Framework = "DPDP-India"
	CCPA   Framework = "CCPA"
)

var frameworkPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{1,31}$`)

func validFramework(f Framework) bool { return frameworkPattern.MatchString(string(f)) }

// Action is what a policy does with a finding.
type Action string

// Policy actions.
const (
	// ActionAllow leaves the value in place; the finding is still reported.
	ActionAllow Action = "allow"
	// ActionMask replaces every letter and digit except the last few with a
	// mask character, keeping length and punctuation.
	ActionMask Action = "mask"
	// ActionRedact replaces the value with [REDACTED:<TYPE>].
	ActionRedact Action = "redact"
	// ActionTokenize replaces the value with a deterministic, reversible
	// token from a session Vault.
	ActionTokenize Action = "tokenize"
	// ActionBlock refuses the whole payload.
	ActionBlock Action = "block"
)

func (a Action) valid() bool {
	switch a {
	case ActionAllow, ActionMask, ActionRedact, ActionTokenize, ActionBlock:
		return true
	}
	return false
}

// UnmarshalText implements encoding.TextUnmarshaler and rejects unknown
// actions.
func (a *Action) UnmarshalText(b []byte) error {
	v := Action(strings.ToLower(string(b)))
	if !v.valid() {
		return fmt.Errorf("dlp: unknown action %q (want allow, mask, redact, tokenize or block)", string(b))
	}
	*a = v
	return nil
}

// Finding is one detected value. It never carries the value itself, so it
// is safe to log; Start and End locate the value in the scanned text.
type Finding struct {
	Type InfoType `json:"type"`
	// Start and End are byte offsets of the value in the scanned text (for
	// streams, in the whole stream).
	Start int `json:"start"`
	End   int `json:"end"`
	// Confidence is in (0, 1].
	Confidence float64     `json:"confidence"`
	Label      Label       `json:"label"`
	Frameworks []Framework `json:"frameworks,omitempty"`
	// Action is the action that was applied.
	Action Action `json:"action"`
	// Detail is non-identifying metadata: a card brand, an IBAN country,
	// an MRZ format, a phone country code.
	Detail string `json:"detail,omitempty"`
	// Context is true when a context keyword near the value raised the
	// confidence.
	Context bool `json:"context,omitempty"`
	// Replacement is the text that replaced the value (empty for allow).
	Replacement string `json:"replacement,omitempty"`
}

// String formats a finding for logs.
func (f Finding) String() string {
	s := fmt.Sprintf("%s [%d:%d] %.2f %s %s", f.Type, f.Start, f.End, f.Confidence, f.Label, f.Action)
	if f.Detail != "" {
		s += " (" + f.Detail + ")"
	}
	return s
}

// MarshalJSON keeps the confidence short in reports.
func (f Finding) MarshalJSON() ([]byte, error) {
	type plain Finding
	p := plain(f)
	p.Confidence = float64(int(f.Confidence*1000+0.5)) / 1000
	return json.Marshal(p)
}

// builtinSpec describes a built-in detector's defaults.
type builtinSpec struct {
	t          InfoType
	label      Label
	frameworks []Framework
	action     Action
	// keywords raise confidence (and enable context-only detection).
	keywords []string
	// negative keywords lower confidence.
	negative []string
}

// Built-in detector indices; also bit positions in keyword masks.
const (
	idCard = iota
	idIBAN
	idSSN
	idAadhaar
	idPAN
	idNINO
	idEmail
	idPhone
	idIP
	idDOB
	idPassport
	idMRN
	idICD
	numBuiltins
)

var builtins = [numBuiltins]builtinSpec{
	idCard: {CreditCard, Restricted, []Framework{PCIDSS, GDPR}, ActionRedact,
		[]string{"credit card", "card number", "card no", "card#", "cc", "ccnum", "cc number", "visa", "mastercard", "master card", "amex", "american express", "discover", "maestro", "jcb", "diners", "unionpay", "rupay", "debit card", "cardholder", "card holder", "payment card", "cvv", "cvc", "expiry", "exp date", "pan number"},
		nil},
	idIBAN: {IBAN, Confidential, []Framework{GDPR, PCIDSS}, ActionRedact,
		[]string{"iban", "bank account", "account number", "account no", "bic", "swift", "sepa", "wire transfer", "kontonummer", "rib"},
		nil},
	idSSN: {USSSN, Restricted, []Framework{CCPA, HIPAA}, ActionRedact,
		[]string{"ssn", "ss#", "ssn#", "social security", "social security number", "social sec", "soc sec", "ssa number", "tax id", "taxpayer id", "itin"},
		nil},
	idAadhaar: {Aadhaar, Restricted, []Framework{DPDP}, ActionRedact,
		[]string{"aadhaar", "aadhar", "adhaar", "aadhaar number", "uidai", "uid", "uid number", "enrolment", "आधार"},
		nil},
	idPAN: {IndiaPAN, Confidential, []Framework{DPDP}, ActionRedact,
		[]string{"pan", "pan card", "pan number", "pan no", "permanent account number", "income tax", "itr"},
		nil},
	idNINO: {UKNINO, Confidential, []Framework{UKGDPR, GDPR}, ActionRedact,
		[]string{"nino", "national insurance", "national insurance number", "ni number", "ni no", "nin"},
		nil},
	idEmail: {Email, Confidential, []Framework{GDPR, CCPA, DPDP}, ActionTokenize,
		[]string{"email", "e-mail", "mail", "mailto", "contact", "reply-to", "cc"},
		nil},
	idPhone: {Phone, Confidential, []Framework{GDPR, CCPA, DPDP}, ActionTokenize,
		[]string{"phone", "phone number", "tel", "telephone", "mobile", "mob", "cell", "cellphone", "fax", "whatsapp", "call", "contact", "sms", "landline"},
		nil},
	idIP: {IPAddress, Internal, []Framework{GDPR}, ActionAllow,
		[]string{"ip", "ip address", "ip addr", "ipv4", "ipv6", "client ip", "remote_addr", "remote addr", "x-forwarded-for", "x-real-ip", "source ip", "src ip", "user ip"},
		[]string{"version", "ver", "release", "build", "oid", "semver"}},
	idDOB: {DateOfBirth, Confidential, []Framework{GDPR, HIPAA, CCPA, DPDP}, ActionRedact,
		[]string{"dob", "d.o.b", "d.o.b.", "date of birth", "birth date", "birthdate", "birthday", "born", "born on", "geburtsdatum", "fecha de nacimiento", "date de naissance", "data di nascita"},
		nil},
	idPassport: {Passport, Restricted, []Framework{GDPR, CCPA, DPDP}, ActionRedact,
		[]string{"passport", "passport no", "passport number", "passport #", "travel document", "reisepass", "passeport", "pasaporte", "passaporto"},
		nil},
	idMRN: {MedicalRecord, Restricted, []Framework{HIPAA, GDPR}, ActionRedact,
		[]string{"mrn", "medical record", "medical record number", "patient id", "patient number", "patient no", "chart number", "health record", "hospital number", "nhs", "nhs number", "nhs no", "ehr id", "member id"},
		nil},
	idICD: {MedicalCode, Restricted, []Framework{HIPAA, GDPR}, ActionRedact,
		[]string{"icd", "icd-10", "icd10", "icd-10-cm", "diagnosis", "diagnoses", "diagnosed", "dx", "condition", "diagnostic code", "problem list", "comorbidity", "comorbidities"},
		nil},
}

// BuiltinTypes returns the built-in information types in a stable order.
func BuiltinTypes() []InfoType {
	out := make([]InfoType, numBuiltins)
	for i := range builtins {
		out[i] = builtins[i].t
	}
	return out
}

func builtinID(t InfoType) (int, bool) {
	for i := range builtins {
		if builtins[i].t == t {
			return i, true
		}
	}
	return -1, false
}
