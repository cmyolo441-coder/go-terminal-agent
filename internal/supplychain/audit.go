package supplychain

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
)

// Finding is one vulnerability affecting one package of the graph.
type Finding struct {
	PackageID    string       `json:"package"`
	Ecosystem    Ecosystem    `json:"ecosystem"`
	Name         string       `json:"name"`
	Version      string       `json:"version"`
	Direct       bool         `json:"direct"`
	Dev          bool         `json:"dev,omitempty"`
	VulnID       string       `json:"id"`
	Aliases      []string     `json:"aliases,omitempty"`
	Summary      string       `json:"summary,omitempty"`
	Severity     Severity     `json:"severity"`
	Score        float64      `json:"score,omitempty"`
	Vector       string       `json:"vector,omitempty"`
	Fixed        []string     `json:"fixed,omitempty"`
	Reachability Reachability `json:"reachability"`
	// Path is a dependency chain from a direct dependency, when known.
	Path []string `json:"path,omitempty"`
}

// AuditOptions configures Audit.
type AuditOptions struct {
	// IncludeDev also audits development-only packages.
	IncludeDev bool
	// GoImports enables the Go reachability hint.
	GoImports *GoImportIndex
}

// Audit checks every pinned, non-local package of g against src and
// returns findings sorted by descending severity, then package and ID.
func Audit(ctx context.Context, g *Graph, src VulnSource, opts AuditOptions) ([]Finding, error) {
	if src == nil {
		return nil, errors.New("supplychain: nil vulnerability source")
	}
	var pkgs []*Package
	var qs []PackageQuery
	for _, p := range g.Packages() {
		if p.Local || p.Unpinned || p.Version == "" || (p.Dev && !opts.IncludeDev) {
			continue
		}
		pkgs = append(pkgs, p)
		qs = append(qs, PackageQuery{Ecosystem: p.Ecosystem, Name: p.Name, Version: p.Version})
	}
	if len(qs) == 0 {
		return nil, nil
	}
	res, err := src.Query(ctx, qs)
	if err != nil {
		return nil, err
	}
	var out []Finding
	for i, vulns := range res {
		if i >= len(pkgs) {
			break
		}
		p := pkgs[i]
		for _, v := range vulns {
			if v == nil || v.Withdrawn != "" {
				continue
			}
			f := Finding{PackageID: p.ID(), Ecosystem: p.Ecosystem, Name: p.Name, Version: p.Version,
				Direct: p.Direct, Dev: p.Dev, VulnID: v.ID, Aliases: v.Aliases, Summary: v.Summary,
				Reachability: ReachUnknown, Path: g.PathTo(p.ID())}
			a, _ := v.AffectedEntry(p.Ecosystem, p.Name, p.Version)
			if a == nil {
				// the source asserted the match; use the package's entry
				for j := range v.Affected {
					if osvEcosystemMatches(v.Affected[j].Package.Ecosystem, p.Ecosystem) &&
						osvNameMatches(p.Ecosystem, v.Affected[j].Package.Name, p.Name) {
						a = &v.Affected[j]
						break
					}
				}
			}
			f.Severity, f.Score, f.Vector = VulnSeverity(v, a)
			if a != nil {
				f.Fixed = a.FixedVersions()
				if p.Ecosystem == EcosystemGo && opts.GoImports != nil {
					f.Reachability = opts.GoImports.Reachability(a)
				}
			}
			out = append(out, f)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Severity != out[j].Severity {
			return out[i].Severity > out[j].Severity
		}
		if out[i].PackageID != out[j].PackageID {
			return out[i].PackageID < out[j].PackageID
		}
		return out[i].VulnID < out[j].VulnID
	})
	return out, nil
}

// VulnSeverity derives the severity of an advisory: the highest CVSS v3
// base score among the advisory's and the affected entry's severity
// entries, else a textual database_specific.severity, else Unknown.
func VulnSeverity(v *OSVVulnerability, a *OSVAffected) (Severity, float64, string) {
	var entries []OSVSeverity
	entries = append(entries, v.Severity...)
	if a != nil {
		entries = append(entries, a.Severity...)
	}
	best, bestVec := -1.0, ""
	for _, s := range entries {
		if s.Type != "CVSS_V3" && !strings.HasPrefix(s.Score, "CVSS:3.") {
			continue
		}
		c, err := ParseCVSS3(s.Score)
		if err != nil {
			continue
		}
		if sc := c.BaseScore(); sc > best {
			best, bestVec = sc, s.Score
		}
	}
	if best >= 0 {
		return SeverityFromScore(best), best, bestVec
	}
	for _, raw := range []json.RawMessage{v.DatabaseSpecific, affectedDB(a)} {
		var ds struct {
			Severity string `json:"severity"`
		}
		if len(raw) > 0 && json.Unmarshal(raw, &ds) == nil && ds.Severity != "" {
			if s, err := ParseSeverity(ds.Severity); err == nil {
				return s, 0, ""
			}
		}
	}
	return SeverityUnknown, 0, ""
}

func affectedDB(a *OSVAffected) json.RawMessage {
	if a == nil {
		return nil
	}
	return a.DatabaseSpecific
}
