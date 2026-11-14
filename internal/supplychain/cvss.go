package supplychain

import (
	"fmt"
	"math"
	"strings"
)

// Severity is a qualitative severity rating. Ratings order naturally, with
// SeverityUnknown lowest.
type Severity int

// Severity ratings (CVSS v3.1 qualitative scale plus Unknown).
const (
	SeverityUnknown Severity = iota
	SeverityNone
	SeverityLow
	SeverityMedium
	SeverityHigh
	SeverityCritical
)

func (s Severity) String() string {
	switch s {
	case SeverityNone:
		return "NONE"
	case SeverityLow:
		return "LOW"
	case SeverityMedium:
		return "MEDIUM"
	case SeverityHigh:
		return "HIGH"
	case SeverityCritical:
		return "CRITICAL"
	}
	return "UNKNOWN"
}

// MarshalText implements encoding.TextMarshaler.
func (s Severity) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (s *Severity) UnmarshalText(b []byte) error {
	v, err := ParseSeverity(string(b))
	if err != nil {
		return err
	}
	*s = v
	return nil
}

// ParseSeverity parses a rating name (case-insensitive). "moderate" (used
// by GitHub advisories) maps to MEDIUM.
func ParseSeverity(s string) (Severity, error) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "NONE":
		return SeverityNone, nil
	case "LOW":
		return SeverityLow, nil
	case "MEDIUM", "MODERATE":
		return SeverityMedium, nil
	case "HIGH", "IMPORTANT":
		return SeverityHigh, nil
	case "CRITICAL":
		return SeverityCritical, nil
	case "UNKNOWN", "":
		return SeverityUnknown, nil
	}
	return SeverityUnknown, fmt.Errorf("unknown severity %q", s)
}

// SeverityFromScore maps a CVSS base score to its qualitative rating.
func SeverityFromScore(score float64) Severity {
	switch {
	case score <= 0:
		return SeverityNone
	case score < 4.0:
		return SeverityLow
	case score < 7.0:
		return SeverityMedium
	case score < 9.0:
		return SeverityHigh
	}
	return SeverityCritical
}

// CVSS3 is a parsed CVSS v3.0/v3.1 vector.
type CVSS3 struct {
	Version string            // "3.0" or "3.1"
	Metrics map[string]string // all metrics present in the vector
}

var cvss3Allowed = map[string][]string{
	// base
	"AV": {"N", "A", "L", "P"}, "AC": {"L", "H"}, "PR": {"N", "L", "H"}, "UI": {"N", "R"},
	"S": {"U", "C"}, "C": {"H", "L", "N"}, "I": {"H", "L", "N"}, "A": {"H", "L", "N"},
	// temporal
	"E": {"X", "U", "P", "F", "H"}, "RL": {"X", "O", "T", "W", "U"}, "RC": {"X", "U", "R", "C"},
	// environmental
	"CR": {"X", "L", "M", "H"}, "IR": {"X", "L", "M", "H"}, "AR": {"X", "L", "M", "H"},
	"MAV": {"X", "N", "A", "L", "P"}, "MAC": {"X", "L", "H"}, "MPR": {"X", "N", "L", "H"},
	"MUI": {"X", "N", "R"}, "MS": {"X", "U", "C"}, "MC": {"X", "H", "L", "N"},
	"MI": {"X", "H", "L", "N"}, "MA": {"X", "H", "L", "N"},
}

var cvss3BaseOrder = []string{"AV", "AC", "PR", "UI", "S", "C", "I", "A"}

// ParseCVSS3 parses a CVSS v3.x vector string such as
// "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H". Every base metric must be
// present exactly once; temporal and environmental metrics are validated.
func ParseCVSS3(vector string) (CVSS3, error) {
	var c CVSS3
	parts := strings.Split(strings.TrimSpace(vector), "/")
	if len(parts) == 0 || !strings.HasPrefix(parts[0], "CVSS:") {
		return c, fmt.Errorf("cvss: vector %q must start with CVSS:3.x", vector)
	}
	c.Version = strings.TrimPrefix(parts[0], "CVSS:")
	if c.Version != "3.0" && c.Version != "3.1" {
		return c, fmt.Errorf("cvss: unsupported version %q", c.Version)
	}
	c.Metrics = map[string]string{}
	for _, p := range parts[1:] {
		k, v, ok := strings.Cut(p, ":")
		if !ok {
			return c, fmt.Errorf("cvss: malformed metric %q", p)
		}
		allowed, known := cvss3Allowed[k]
		if !known {
			return c, fmt.Errorf("cvss: unknown metric %q", k)
		}
		if _, dup := c.Metrics[k]; dup {
			return c, fmt.Errorf("cvss: metric %q repeated", k)
		}
		valid := false
		for _, a := range allowed {
			if a == v {
				valid = true
				break
			}
		}
		if !valid {
			return c, fmt.Errorf("cvss: invalid value %q for metric %s", v, k)
		}
		c.Metrics[k] = v
	}
	for _, k := range cvss3BaseOrder {
		if _, ok := c.Metrics[k]; !ok {
			return c, fmt.Errorf("cvss: missing base metric %s", k)
		}
	}
	return c, nil
}

// roundUp31 is the Roundup function from CVSS v3.1 Appendix A, which
// avoids floating point artefacts.
func roundUp31(x float64) float64 {
	i := int64(math.Round(x * 100000))
	if i%10000 == 0 {
		return float64(i) / 100000.0
	}
	return float64(i/10000+1) / 10.0
}

// roundUp30 is the CVSS v3.0 definition: smallest one-decimal number >= x.
func roundUp30(x float64) float64 { return math.Ceil(x*10) / 10 }

// BaseScore computes the CVSS v3 base score exactly as specified in
// section 7.1 of the CVSS v3.1 specification.
func (c CVSS3) BaseScore() float64 {
	m := c.Metrics
	av := map[string]float64{"N": 0.85, "A": 0.62, "L": 0.55, "P": 0.2}[m["AV"]]
	ac := map[string]float64{"L": 0.77, "H": 0.44}[m["AC"]]
	ui := map[string]float64{"N": 0.85, "R": 0.62}[m["UI"]]
	changed := m["S"] == "C"
	var pr float64
	switch m["PR"] {
	case "N":
		pr = 0.85
	case "L":
		pr = 0.62
		if changed {
			pr = 0.68
		}
	case "H":
		pr = 0.27
		if changed {
			pr = 0.5
		}
	}
	cia := map[string]float64{"H": 0.56, "L": 0.22, "N": 0}
	iss := 1 - (1-cia[m["C"]])*(1-cia[m["I"]])*(1-cia[m["A"]])
	var impact float64
	if changed {
		impact = 7.52*(iss-0.029) - 3.25*math.Pow(iss-0.02, 15)
	} else {
		impact = 6.42 * iss
	}
	exploitability := 8.22 * av * ac * pr * ui
	if impact <= 0 {
		return 0
	}
	round := roundUp31
	if c.Version == "3.0" {
		round = roundUp30
	}
	if changed {
		return round(math.Min(1.08*(impact+exploitability), 10))
	}
	return round(math.Min(impact+exploitability, 10))
}

// Severity returns the qualitative rating of the base score.
func (c CVSS3) Severity() Severity { return SeverityFromScore(c.BaseScore()) }

// CVSS3BaseScore parses vector and returns its base score.
func CVSS3BaseScore(vector string) (float64, error) {
	c, err := ParseCVSS3(vector)
	if err != nil {
		return 0, err
	}
	return c.BaseScore(), nil
}
