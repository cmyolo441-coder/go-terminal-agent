package supplychain

import (
	"fmt"
	"sort"
	"strings"
)

// Policy is the supply-chain gate configuration.
type Policy struct {
	// FailOnSeverity fails the gate for findings at or above this rating.
	// SeverityUnknown disables the vulnerability gate.
	FailOnSeverity Severity `json:"failOnSeverity"`
	// UnknownSeverityAs is the rating assumed for advisories without one
	// (default: SeverityUnknown, i.e. they only warn).
	UnknownSeverityAs Severity `json:"unknownSeverityAs"`
	// IgnoreVulns lists advisory IDs or aliases to suppress.
	IgnoreVulns []string `json:"ignoreVulns,omitempty"`
	// SkipUnreachable suppresses Go findings whose package is not imported.
	SkipUnreachable bool `json:"skipUnreachable,omitempty"`
	// DenyLicenses lists denied license ids/families/globs ("GPL-3.0",
	// "AGPL-*"). A package fails when its declared expression cannot be
	// satisfied without a denied license.
	DenyLicenses []string `json:"denyLicenses,omitempty"`
	// FailOnInvalidLicense fails packages whose license is not a valid
	// SPDX expression (otherwise a warning).
	FailOnInvalidLicense bool `json:"failOnInvalidLicense,omitempty"`
	// Typosquat enables name-similarity checks for npm and PyPI.
	Typosquat bool `json:"typosquat"`
	// FailOnTyposquat turns typosquat warnings into failures.
	FailOnTyposquat bool `json:"failOnTyposquat,omitempty"`
	// InstallScripts warns about npm packages running install scripts;
	// FailOnInstallScripts fails on them instead. AllowInstallScripts lists
	// trusted package names.
	InstallScripts       bool     `json:"installScripts"`
	FailOnInstallScripts bool     `json:"failOnInstallScripts,omitempty"`
	AllowInstallScripts  []string `json:"allowInstallScripts,omitempty"`
	// FailOnUnpinned fails direct dependencies without a locked version.
	FailOnUnpinned bool `json:"failOnUnpinned,omitempty"`
	// IncludeDev applies the license, typosquat and script rules to dev
	// dependencies too.
	IncludeDev bool `json:"includeDev,omitempty"`
}

// DefaultPolicy fails on HIGH or worse and warns on typosquats and
// install scripts.
func DefaultPolicy() Policy {
	return Policy{FailOnSeverity: SeverityHigh, Typosquat: true, InstallScripts: true}
}

// Level of a policy violation.
type Level string

// Violation levels.
const (
	LevelError Level = "error"
	LevelWarn  Level = "warning"
)

// Violation is one policy result.
type Violation struct {
	Rule      string `json:"rule"`
	Level     Level  `json:"level"`
	PackageID string `json:"package,omitempty"`
	Message   string `json:"message"`
}

// PolicyReport is the gate outcome.
type PolicyReport struct {
	Passed     bool        `json:"passed"`
	Violations []Violation `json:"violations"`
}

// Errors returns the failing violations.
func (r PolicyReport) Errors() []Violation {
	var out []Violation
	for _, v := range r.Violations {
		if v.Level == LevelError {
			out = append(out, v)
		}
	}
	return out
}

// Evaluate applies the policy to a graph and its audit findings.
// scripts maps workspace package.json paths to their own install scripts
// (ScanResult.InstallScripts) and may be nil.
func (p Policy) Evaluate(g *Graph, findings []Finding, scripts map[string]map[string]string) PolicyReport {
	var vs []Violation
	level := func(fail bool) Level {
		if fail {
			return LevelError
		}
		return LevelWarn
	}
	ignored := map[string]bool{}
	for _, id := range p.IgnoreVulns {
		ignored[strings.ToUpper(strings.TrimSpace(id))] = true
	}
	for _, f := range findings {
		if isIgnored(ignored, f) {
			continue
		}
		if p.SkipUnreachable && f.Reachability == ReachNotImported {
			continue
		}
		sev := f.Severity
		if sev == SeverityUnknown {
			sev = p.UnknownSeverityAs
		}
		fail := p.FailOnSeverity != SeverityUnknown && sev >= p.FailOnSeverity
		msg := fmt.Sprintf("%s %s@%s: %s (%s", f.VulnID, f.Name, f.Version, strings.TrimSpace(f.Summary), f.Severity)
		if f.Score > 0 {
			msg += fmt.Sprintf(" %.1f", f.Score)
		}
		msg += ")"
		if len(f.Fixed) > 0 {
			msg += "; fixed in " + strings.Join(f.Fixed, ", ")
		}
		if f.Reachability != ReachUnknown {
			msg += "; reachability: " + f.Reachability.String()
		}
		vs = append(vs, Violation{Rule: "vulnerability", Level: level(fail), PackageID: f.PackageID, Message: msg})
	}
	var tc *TyposquatChecker
	if p.Typosquat {
		tc = NewTyposquatChecker(nil)
	}
	allowScripts := map[string]bool{}
	for _, n := range p.AllowInstallScripts {
		allowScripts[n] = true
	}
	for _, pkg := range g.Packages() {
		if pkg.Dev && !p.IncludeDev {
			continue
		}
		id := pkg.ID()
		if len(p.DenyLicenses) > 0 || p.FailOnInvalidLicense {
			for _, lic := range pkg.Licenses {
				expr, err := ParseLicenseExpression(lic)
				if err != nil {
					vs = append(vs, Violation{Rule: "license-invalid", Level: level(p.FailOnInvalidLicense), PackageID: id,
						Message: fmt.Sprintf("%s: license %q is not a valid SPDX expression: %v", pkg.Name, lic, err)})
					continue
				}
				if ok, bad := LicenseSatisfiable(expr, p.DenyLicenses); !ok {
					vs = append(vs, Violation{Rule: "license-denied", Level: LevelError, PackageID: id,
						Message: fmt.Sprintf("%s: license %q requires denied license(s) %s", pkg.Name, expr.String(), strings.Join(bad, ", "))})
				}
			}
		}
		if tc != nil && (pkg.Ecosystem == EcosystemNPM || pkg.Ecosystem == EcosystemPyPI) && !pkg.Local {
			if m := tc.Check(pkg.Ecosystem, pkg.Name); m != nil {
				vs = append(vs, Violation{Rule: "typosquat", Level: level(p.FailOnTyposquat), PackageID: id,
					Message: fmt.Sprintf("%s resembles popular package %q (%s, distance %d)", pkg.Name, m.Target, m.Reason, m.Distance)})
			}
		}
		if p.InstallScripts && pkg.InstallScript && pkg.Ecosystem == EcosystemNPM && !allowScripts[pkg.Name] {
			vs = append(vs, Violation{Rule: "install-script", Level: level(p.FailOnInstallScripts), PackageID: id,
				Message: fmt.Sprintf("%s@%s runs install-time lifecycle scripts", pkg.Name, pkg.Version)})
		}
		if p.FailOnUnpinned && pkg.Direct && pkg.Unpinned && !pkg.Local {
			vs = append(vs, Violation{Rule: "unpinned", Level: LevelError, PackageID: id,
				Message: fmt.Sprintf("%s is not pinned to an exact version (constraint %q)", pkg.Name, pkg.Constraint)})
		}
	}
	if p.InstallScripts {
		files := make([]string, 0, len(scripts))
		for f := range scripts {
			files = append(files, f)
		}
		sort.Strings(files)
		for _, f := range files {
			hooks := make([]string, 0, len(scripts[f]))
			for h := range scripts[f] {
				hooks = append(hooks, h)
			}
			sort.Strings(hooks)
			for _, h := range hooks {
				vs = append(vs, Violation{Rule: "install-script", Level: LevelWarn,
					Message: fmt.Sprintf("%s declares %q script: %s", f, h, scripts[f][h])})
			}
		}
	}
	sort.SliceStable(vs, func(i, j int) bool {
		if vs[i].Level != vs[j].Level {
			return vs[i].Level == LevelError
		}
		if vs[i].Rule != vs[j].Rule {
			return vs[i].Rule < vs[j].Rule
		}
		return vs[i].PackageID < vs[j].PackageID
	})
	rep := PolicyReport{Passed: true, Violations: vs}
	for _, v := range vs {
		if v.Level == LevelError {
			rep.Passed = false
		}
	}
	return rep
}

func isIgnored(ignored map[string]bool, f Finding) bool {
	if ignored[strings.ToUpper(f.VulnID)] {
		return true
	}
	for _, a := range f.Aliases {
		if ignored[strings.ToUpper(a)] {
			return true
		}
	}
	return false
}
