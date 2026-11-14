package supplychain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// SBOMOptions configures SBOM generation.
type SBOMOptions struct {
	// Name and Version describe the root component (the workspace).
	Name    string
	Version string
	// Timestamp is recorded as the creation time. A zero value uses the
	// current time; set it for reproducible output.
	Timestamp time.Time
	// ToolName and ToolVersion identify the generator.
	ToolName    string
	ToolVersion string
	// Namespace is the SPDX documentNamespace prefix
	// (default "https://spdx.org/spdxdocs").
	Namespace string
	// IncludeDev includes development-only packages.
	IncludeDev bool
}

func (o SBOMOptions) normalized() SBOMOptions {
	if o.Name == "" {
		o.Name = "workspace"
	}
	if o.ToolName == "" {
		o.ToolName = "gta"
	}
	if o.ToolVersion == "" {
		o.ToolVersion = "dev"
	}
	if o.Timestamp.IsZero() {
		o.Timestamp = time.Now()
	}
	o.Timestamp = o.Timestamp.UTC().Truncate(time.Second)
	if o.Namespace == "" {
		o.Namespace = "https://spdx.org/spdxdocs"
	}
	return o
}

func sbomPackages(g *Graph, includeDev bool) []*Package {
	var out []*Package
	for _, p := range g.Packages() {
		if p.Dev && !includeDev {
			continue
		}
		out = append(out, p)
	}
	return out
}

// contentUUID derives a deterministic RFC 4122 (version 5 layout) UUID
// from the SBOM content.
func contentUUID(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	b := h.Sum(nil)[:16]
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	x := hex.EncodeToString(b)
	return x[0:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:32]
}

func graphFingerprint(pkgs []*Package, g *Graph) []string {
	parts := make([]string, 0, len(pkgs)*2)
	for _, p := range pkgs {
		parts = append(parts, p.PURL, strings.Join(g.DependenciesOf(p.ID()), ","))
	}
	return parts
}

func marshalIndent(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ---- CycloneDX 1.6 ------------------------------------------------------------

type cdxBOM struct {
	BOMFormat    string          `json:"bomFormat"`
	SpecVersion  string          `json:"specVersion"`
	SerialNumber string          `json:"serialNumber"`
	Version      int             `json:"version"`
	Metadata     cdxMetadata     `json:"metadata"`
	Components   []cdxComponent  `json:"components"`
	Dependencies []cdxDependency `json:"dependencies"`
}

type cdxMetadata struct {
	Timestamp string       `json:"timestamp"`
	Tools     cdxTools     `json:"tools"`
	Component cdxComponent `json:"component"`
}

type cdxTools struct {
	Components []cdxComponent `json:"components"`
}

type cdxComponent struct {
	Type       string        `json:"type"`
	BOMRef     string        `json:"bom-ref,omitempty"`
	Group      string        `json:"group,omitempty"`
	Name       string        `json:"name"`
	Version    string        `json:"version,omitempty"`
	Scope      string        `json:"scope,omitempty"`
	Hashes     []cdxHash     `json:"hashes,omitempty"`
	Licenses   []cdxLicense  `json:"licenses,omitempty"`
	PURL       string        `json:"purl,omitempty"`
	ExtRefs    []cdxExtRef   `json:"externalReferences,omitempty"`
	Properties []cdxProperty `json:"properties,omitempty"`
}

type cdxHash struct {
	Alg     string `json:"alg"`
	Content string `json:"content"`
}

type cdxLicense struct {
	License    *cdxLicenseChoice `json:"license,omitempty"`
	Expression string            `json:"expression,omitempty"`
}

type cdxLicenseChoice struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

type cdxExtRef struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

type cdxProperty struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type cdxDependency struct {
	Ref       string   `json:"ref"`
	DependsOn []string `json:"dependsOn"`
}

func cdxLicenses(lics []string) []cdxLicense {
	var out []cdxLicense
	// CycloneDX allows either several license objects or exactly one
	// expression; combine multiple declarations with AND.
	var exprs []string
	for _, l := range lics {
		if e, err := ParseLicenseExpression(l); err == nil {
			exprs = append(exprs, e.String())
		} else {
			out = append(out, cdxLicense{License: &cdxLicenseChoice{Name: l}})
		}
	}
	if len(out) > 0 {
		for _, e := range exprs {
			out = append(out, cdxLicense{License: &cdxLicenseChoice{Name: e}})
		}
		return out
	}
	switch len(exprs) {
	case 0:
		return nil
	case 1:
		e, _ := ParseLicenseExpression(exprs[0])
		if t, ok := e.(*LicenseTerm); ok && !t.Plus && t.Exception == "" && t.DocumentRef == "" {
			if _, known := spdxLicenseIndex[strings.ToLower(t.ID)]; known {
				return []cdxLicense{{License: &cdxLicenseChoice{ID: t.ID}}}
			}
			if !strings.HasPrefix(t.ID, "LicenseRef-") {
				return []cdxLicense{{License: &cdxLicenseChoice{Name: t.ID}}}
			}
		}
		return []cdxLicense{{Expression: exprs[0]}}
	}
	wrapped := make([]string, len(exprs))
	for i, e := range exprs {
		wrapped[i] = "(" + e + ")"
	}
	return []cdxLicense{{Expression: strings.Join(wrapped, " AND ")}}
}

// CycloneDX renders the graph as a CycloneDX 1.6 JSON document. Output is
// deterministic for a fixed graph and options (sorted components and
// dependencies, content-derived serial number).
func CycloneDX(g *Graph, opts SBOMOptions) ([]byte, error) {
	opts = opts.normalized()
	pkgs := sbomPackages(g, opts.IncludeDev)
	included := map[string]string{} // id -> bom-ref
	for _, p := range pkgs {
		included[p.ID()] = p.PURL
	}
	rootRef := "root:" + opts.Name
	bom := cdxBOM{BOMFormat: "CycloneDX", SpecVersion: "1.6", Version: 1,
		SerialNumber: "urn:uuid:" + contentUUID(append([]string{"cyclonedx", opts.Name, opts.Version}, graphFingerprint(pkgs, g)...)...),
		Metadata: cdxMetadata{
			Timestamp: opts.Timestamp.Format(time.RFC3339),
			Tools:     cdxTools{Components: []cdxComponent{{Type: "application", Name: opts.ToolName, Version: opts.ToolVersion}}},
			Component: cdxComponent{Type: "application", BOMRef: rootRef, Name: opts.Name, Version: opts.Version},
		},
		Components:   []cdxComponent{},
		Dependencies: []cdxDependency{},
	}
	var rootDeps []string
	for _, p := range pkgs {
		c := cdxComponent{Type: "library", BOMRef: p.PURL, Name: p.Name, Version: p.Version, PURL: p.PURL,
			Licenses: cdxLicenses(p.Licenses)}
		if p.Dev || p.Optional {
			c.Scope = "optional"
		} else {
			c.Scope = "required"
		}
		hashes := append([]Hash(nil), p.Hashes...)
		sort.Slice(hashes, func(i, j int) bool {
			if hashes[i].Alg != hashes[j].Alg {
				return hashes[i].Alg < hashes[j].Alg
			}
			return hashes[i].Hex < hashes[j].Hex
		})
		for _, h := range hashes {
			c.Hashes = append(c.Hashes, cdxHash{Alg: string(h.Alg), Content: h.Hex})
		}
		if p.Resolved != "" && strings.Contains(p.Resolved, "://") {
			c.ExtRefs = []cdxExtRef{{Type: "distribution", URL: p.Resolved}}
		}
		props := []cdxProperty{{Name: "gta:ecosystem", Value: string(p.Ecosystem)}}
		if p.Direct {
			props = append(props, cdxProperty{Name: "gta:direct", Value: "true"})
		}
		if p.Dev {
			props = append(props, cdxProperty{Name: "gta:dev", Value: "true"})
		}
		if p.Local {
			props = append(props, cdxProperty{Name: "gta:local", Value: "true"})
		}
		if p.Unpinned {
			props = append(props, cdxProperty{Name: "gta:constraint", Value: p.Constraint})
		}
		c.Properties = props
		bom.Components = append(bom.Components, c)
		deps := []string{}
		for _, d := range g.DependenciesOf(p.ID()) {
			if ref, ok := included[d]; ok {
				deps = append(deps, ref)
			}
		}
		sort.Strings(deps)
		bom.Dependencies = append(bom.Dependencies, cdxDependency{Ref: p.PURL, DependsOn: deps})
		if p.Direct {
			rootDeps = append(rootDeps, p.PURL)
		}
	}
	sort.Slice(bom.Components, func(i, j int) bool { return bom.Components[i].BOMRef < bom.Components[j].BOMRef })
	sort.Strings(rootDeps)
	if rootDeps == nil {
		rootDeps = []string{}
	}
	bom.Dependencies = append(bom.Dependencies, cdxDependency{Ref: rootRef, DependsOn: rootDeps})
	sort.Slice(bom.Dependencies, func(i, j int) bool { return bom.Dependencies[i].Ref < bom.Dependencies[j].Ref })
	if dup := firstDuplicate(bom.Components); dup != "" {
		return nil, fmt.Errorf("cyclonedx: duplicate bom-ref %q", dup)
	}
	return marshalIndent(bom)
}

func firstDuplicate(cs []cdxComponent) string {
	for i := 1; i < len(cs); i++ {
		if cs[i].BOMRef == cs[i-1].BOMRef {
			return cs[i].BOMRef
		}
	}
	return ""
}

// ---- SPDX 2.3 -----------------------------------------------------------------

type spdxDoc struct {
	SPDXVersion       string             `json:"spdxVersion"`
	DataLicense       string             `json:"dataLicense"`
	SPDXID            string             `json:"SPDXID"`
	Name              string             `json:"name"`
	DocumentNamespace string             `json:"documentNamespace"`
	CreationInfo      spdxCreationInfo   `json:"creationInfo"`
	Packages          []spdxPackage      `json:"packages"`
	Relationships     []spdxRelationship `json:"relationships"`
}

type spdxCreationInfo struct {
	Created  string   `json:"created"`
	Creators []string `json:"creators"`
}

type spdxPackage struct {
	Name             string         `json:"name"`
	SPDXID           string         `json:"SPDXID"`
	VersionInfo      string         `json:"versionInfo,omitempty"`
	DownloadLocation string         `json:"downloadLocation"`
	FilesAnalyzed    bool           `json:"filesAnalyzed"`
	LicenseConcluded string         `json:"licenseConcluded"`
	LicenseDeclared  string         `json:"licenseDeclared"`
	CopyrightText    string         `json:"copyrightText"`
	Checksums        []spdxChecksum `json:"checksums,omitempty"`
	ExternalRefs     []spdxExtRef   `json:"externalRefs,omitempty"`
	PrimaryPurpose   string         `json:"primaryPackagePurpose,omitempty"`
	Comment          string         `json:"comment,omitempty"`
}

type spdxChecksum struct {
	Algorithm     string `json:"algorithm"`
	ChecksumValue string `json:"checksumValue"`
}

type spdxExtRef struct {
	Category string `json:"referenceCategory"`
	Type     string `json:"referenceType"`
	Locator  string `json:"referenceLocator"`
}

type spdxRelationship struct {
	Element string `json:"spdxElementId"`
	Type    string `json:"relationshipType"`
	Related string `json:"relatedSpdxElement"`
}

// spdxIDFor builds a valid, unique SPDX identifier ([A-Za-z0-9.-]) from a
// package ID, suffixed with a short hash to avoid collisions after
// sanitisation.
func spdxIDFor(id string) string {
	var b strings.Builder
	b.WriteString("SPDXRef-Package-")
	for _, r := range id {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	sum := sha256.Sum256([]byte(id))
	b.WriteByte('-')
	b.WriteString(hex.EncodeToString(sum[:4]))
	return b.String()
}

func spdxDeclared(lics []string) (string, string) {
	if len(lics) == 0 {
		return "NOASSERTION", ""
	}
	var exprs []string
	for _, l := range lics {
		e, err := ParseLicenseExpression(l)
		if err != nil {
			return "NOASSERTION", "declared license not a valid SPDX expression: " + strings.Join(lics, "; ")
		}
		// unknown ids are not valid SPDX; map them to LicenseRef-.
		for _, t := range LicenseTerms(e) {
			if _, ok := spdxLicenseIndex[strings.ToLower(t.ID)]; !ok && !strings.HasPrefix(t.ID, "LicenseRef-") {
				return "NOASSERTION", "declared license uses non-SPDX identifier " + t.ID
			}
		}
		exprs = append(exprs, e.String())
	}
	if len(exprs) == 1 {
		return exprs[0], ""
	}
	for i := range exprs {
		exprs[i] = "(" + exprs[i] + ")"
	}
	return strings.Join(exprs, " AND "), ""
}

// SPDX renders the graph as an SPDX 2.3 JSON document. Output is
// deterministic for a fixed graph and options.
func SPDX(g *Graph, opts SBOMOptions) ([]byte, error) {
	opts = opts.normalized()
	pkgs := sbomPackages(g, opts.IncludeDev)
	uuid := contentUUID(append([]string{"spdx", opts.Name, opts.Version}, graphFingerprint(pkgs, g)...)...)
	doc := spdxDoc{SPDXVersion: "SPDX-2.3", DataLicense: "CC0-1.0", SPDXID: "SPDXRef-DOCUMENT",
		Name:              opts.Name,
		DocumentNamespace: strings.TrimRight(opts.Namespace, "/") + "/" + sanitizeNSName(opts.Name) + "-" + uuid,
		CreationInfo: spdxCreationInfo{Created: opts.Timestamp.Format("2006-01-02T15:04:05Z"),
			Creators: []string{"Tool: " + opts.ToolName + "-" + opts.ToolVersion}},
		Packages:      []spdxPackage{},
		Relationships: []spdxRelationship{},
	}
	rootID := "SPDXRef-Package-root"
	doc.Packages = append(doc.Packages, spdxPackage{Name: opts.Name, SPDXID: rootID, VersionInfo: opts.Version,
		DownloadLocation: "NOASSERTION", LicenseConcluded: "NOASSERTION", LicenseDeclared: "NOASSERTION",
		CopyrightText: "NOASSERTION", PrimaryPurpose: "APPLICATION"})
	doc.Relationships = append(doc.Relationships, spdxRelationship{"SPDXRef-DOCUMENT", "DESCRIBES", rootID})
	ids := map[string]string{}
	for _, p := range pkgs {
		ids[p.ID()] = spdxIDFor(p.ID())
	}
	var pk []spdxPackage
	var rels []spdxRelationship
	for _, p := range pkgs {
		sid := ids[p.ID()]
		decl, comment := spdxDeclared(p.Licenses)
		sp := spdxPackage{Name: p.Name, SPDXID: sid, VersionInfo: p.Version, DownloadLocation: "NOASSERTION",
			LicenseConcluded: "NOASSERTION", LicenseDeclared: decl, CopyrightText: "NOASSERTION",
			PrimaryPurpose: "LIBRARY", Comment: comment,
			ExternalRefs: []spdxExtRef{{Category: "PACKAGE-MANAGER", Type: "purl", Locator: p.PURL}}}
		if p.Resolved != "" && strings.Contains(p.Resolved, "://") {
			sp.DownloadLocation = p.Resolved
		}
		hashes := append([]Hash(nil), p.Hashes...)
		sort.Slice(hashes, func(i, j int) bool {
			if hashes[i].Alg != hashes[j].Alg {
				return hashes[i].Alg < hashes[j].Alg
			}
			return hashes[i].Hex < hashes[j].Hex
		})
		for _, h := range hashes {
			sp.Checksums = append(sp.Checksums, spdxChecksum{Algorithm: h.Alg.spdxName(), ChecksumValue: h.Hex})
		}
		pk = append(pk, sp)
		if p.Direct {
			typ := "DEPENDS_ON"
			if p.Dev {
				typ = "DEV_DEPENDENCY_OF"
				rels = append(rels, spdxRelationship{sid, typ, rootID})
			} else {
				rels = append(rels, spdxRelationship{rootID, typ, sid})
			}
		}
		for _, d := range g.DependenciesOf(p.ID()) {
			if to, ok := ids[d]; ok {
				rels = append(rels, spdxRelationship{sid, "DEPENDS_ON", to})
			}
		}
	}
	sort.Slice(pk, func(i, j int) bool { return pk[i].SPDXID < pk[j].SPDXID })
	sort.Slice(rels, func(i, j int) bool {
		a, b := rels[i], rels[j]
		if a.Element != b.Element {
			return a.Element < b.Element
		}
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		return a.Related < b.Related
	})
	doc.Packages = append(doc.Packages, pk...)
	doc.Relationships = append(doc.Relationships, rels...)
	return marshalIndent(doc)
}

func sanitizeNSName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, s)
}
