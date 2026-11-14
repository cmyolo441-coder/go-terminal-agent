// Package supplychain implements software supply-chain security for the
// workspace the agent operates in: strict manifest and lockfile parsers that
// build a dependency graph with package URLs, CycloneDX and SPDX SBOM
// generation, OSV vulnerability matching with per-ecosystem version
// comparators and a CVSS v3.1 calculator, a policy gate (severity threshold,
// SPDX license deny-list, typosquat and install-script detection) and SLSA v1
// provenance signed as a DSSE envelope.
package supplychain

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Ecosystem identifies a package ecosystem. The values are the ecosystem
// names used by the OSV schema so they can be sent to OSV verbatim.
type Ecosystem string

// Supported ecosystems.
const (
	EcosystemGo        Ecosystem = "Go"
	EcosystemNPM       Ecosystem = "npm"
	EcosystemPyPI      Ecosystem = "PyPI"
	EcosystemCargo     Ecosystem = "crates.io"
	EcosystemRubyGems  Ecosystem = "RubyGems"
	EcosystemPackagist Ecosystem = "Packagist"
)

// Ecosystems lists every supported ecosystem in a stable order.
func Ecosystems() []Ecosystem {
	return []Ecosystem{EcosystemGo, EcosystemNPM, EcosystemPyPI, EcosystemCargo, EcosystemRubyGems, EcosystemPackagist}
}

// PURLType returns the package-url type for the ecosystem.
func (e Ecosystem) PURLType() string {
	switch e {
	case EcosystemGo:
		return "golang"
	case EcosystemNPM:
		return "npm"
	case EcosystemPyPI:
		return "pypi"
	case EcosystemCargo:
		return "cargo"
	case EcosystemRubyGems:
		return "gem"
	case EcosystemPackagist:
		return "composer"
	}
	return "generic"
}

// HashAlg is a hash algorithm name using CycloneDX spelling.
type HashAlg string

// Hash algorithms that lockfiles provide.
const (
	HashMD5    HashAlg = "MD5"
	HashSHA1   HashAlg = "SHA-1"
	HashSHA256 HashAlg = "SHA-256"
	HashSHA384 HashAlg = "SHA-384"
	HashSHA512 HashAlg = "SHA-512"
)

// spdxName returns the SPDX 2.3 checksum algorithm spelling.
func (h HashAlg) spdxName() string { return strings.ReplaceAll(string(h), "-", "") }

// Hash is a digest of a package artifact, hex encoded (lower case).
type Hash struct {
	Alg HashAlg `json:"alg"`
	Hex string  `json:"hex"`
}

// Package is a node of the dependency graph.
type Package struct {
	Ecosystem Ecosystem `json:"ecosystem"`
	Name      string    `json:"name"`
	Version   string    `json:"version,omitempty"`
	PURL      string    `json:"purl"`
	// Direct is true when a workspace manifest depends on the package.
	Direct bool `json:"direct,omitempty"`
	// Dev is true when the package is only needed for development.
	Dev bool `json:"dev,omitempty"`
	// Optional marks optional dependencies (npm optionalDependencies, extras).
	Optional bool `json:"optional,omitempty"`
	// Local marks workspace members and path/link dependencies that do not
	// come from a registry.
	Local bool `json:"local,omitempty"`
	// Unpinned marks dependencies whose exact version is not locked.
	Unpinned bool `json:"unpinned,omitempty"`
	// Constraint is the requested version range when the version is unpinned.
	Constraint string `json:"constraint,omitempty"`
	// Licenses holds declared license strings (ideally SPDX expressions).
	Licenses []string `json:"licenses,omitempty"`
	Hashes   []Hash   `json:"hashes,omitempty"`
	// Resolved is the download location recorded by the lockfile.
	Resolved string `json:"resolved,omitempty"`
	// InstallScript is true when the package runs lifecycle scripts on
	// install (npm preinstall/install/postinstall).
	InstallScript bool `json:"installScript,omitempty"`
	// Marker is a PEP 508 environment marker.
	Marker string `json:"marker,omitempty"`
	// Qualifiers are extra purl qualifiers (e.g. gem platform).
	Qualifiers map[string]string `json:"qualifiers,omitempty"`
	// Sources lists the manifest paths that mention the package.
	Sources []string `json:"sources,omitempty"`
}

// ID returns the stable graph identifier of the package.
func (p *Package) ID() string {
	return PackageID(p.Ecosystem, p.Name, p.Version)
}

// PackageID builds a graph identifier.
func PackageID(eco Ecosystem, name, version string) string {
	if eco == EcosystemPyPI {
		name = NormalizePyPIName(name)
	}
	return string(eco) + "/" + name + "@" + version
}

func (p *Package) ensurePURL() {
	if p.PURL != "" {
		return
	}
	p.PURL = PURLFor(p.Ecosystem, p.Name, p.Version, p.Qualifiers).String()
}

// AddHash records a hash, ignoring duplicates.
func (p *Package) AddHash(h Hash) {
	h.Hex = strings.ToLower(h.Hex)
	if h.Hex == "" || h.Alg == "" {
		return
	}
	for _, e := range p.Hashes {
		if e == h {
			return
		}
	}
	p.Hashes = append(p.Hashes, h)
}

// AddLicense records a declared license, ignoring empties and duplicates.
func (p *Package) AddLicense(l string) {
	l = strings.TrimSpace(l)
	if l == "" || slices.Contains(p.Licenses, l) {
		return
	}
	p.Licenses = append(p.Licenses, l)
}

func (p *Package) addSource(s string) {
	if s == "" || slices.Contains(p.Sources, s) {
		return
	}
	p.Sources = append(p.Sources, s)
}

// Graph is a dependency graph. Nodes are packages, keyed by ID; an edge
// a -> b means a depends on b. Direct dependencies of the workspace are
// flagged with Package.Direct. The zero value is not usable; call NewGraph.
type Graph struct {
	pkgs  map[string]*Package
	edges map[string]map[string]struct{}
}

// NewGraph returns an empty graph.
func NewGraph() *Graph {
	return &Graph{pkgs: map[string]*Package{}, edges: map[string]map[string]struct{}{}}
}

// Len returns the number of packages.
func (g *Graph) Len() int { return len(g.pkgs) }

// Add inserts p, merging it with an existing node of the same ID, and
// returns the canonical node.
func (g *Graph) Add(p *Package) *Package {
	if p.Ecosystem == EcosystemPyPI {
		p.Name = NormalizePyPIName(p.Name)
	}
	id := p.ID()
	cur, ok := g.pkgs[id]
	if !ok {
		cp := *p
		cp.Licenses = slices.Clone(p.Licenses)
		cp.Hashes = slices.Clone(p.Hashes)
		cp.Sources = slices.Clone(p.Sources)
		if p.Qualifiers != nil {
			cp.Qualifiers = make(map[string]string, len(p.Qualifiers))
			for k, v := range p.Qualifiers {
				cp.Qualifiers[k] = v
			}
		}
		cp.ensurePURL()
		g.pkgs[id] = &cp
		return &cp
	}
	cur.Direct = cur.Direct || p.Direct
	cur.Dev = cur.Dev && p.Dev
	cur.Optional = cur.Optional && p.Optional
	cur.Local = cur.Local || p.Local
	cur.Unpinned = cur.Unpinned && p.Unpinned
	cur.InstallScript = cur.InstallScript || p.InstallScript
	if cur.Resolved == "" {
		cur.Resolved = p.Resolved
	}
	if cur.Constraint == "" {
		cur.Constraint = p.Constraint
	}
	if cur.Marker == "" {
		cur.Marker = p.Marker
	}
	for _, l := range p.Licenses {
		cur.AddLicense(l)
	}
	for _, h := range p.Hashes {
		cur.AddHash(h)
	}
	for _, s := range p.Sources {
		cur.addSource(s)
	}
	return cur
}

// Package returns the node with the given ID or nil.
func (g *Graph) Package(id string) *Package { return g.pkgs[id] }

// AddEdge records that from depends on to. Both nodes must exist.
func (g *Graph) AddEdge(from, to string) error {
	if _, ok := g.pkgs[from]; !ok {
		return fmt.Errorf("supplychain: edge from unknown package %q", from)
	}
	if _, ok := g.pkgs[to]; !ok {
		return fmt.Errorf("supplychain: edge to unknown package %q", to)
	}
	if from == to {
		return nil
	}
	m := g.edges[from]
	if m == nil {
		m = map[string]struct{}{}
		g.edges[from] = m
	}
	m[to] = struct{}{}
	return nil
}

// Packages returns all packages sorted by ID.
func (g *Graph) Packages() []*Package {
	out := make([]*Package, 0, len(g.pkgs))
	for _, p := range g.pkgs {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}

// Direct returns the direct dependencies sorted by ID.
func (g *Graph) Direct() []*Package {
	var out []*Package
	for _, p := range g.Packages() {
		if p.Direct {
			out = append(out, p)
		}
	}
	return out
}

// DependenciesOf returns the sorted IDs that id depends on.
func (g *Graph) DependenciesOf(id string) []string {
	m := g.edges[id]
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Dependents returns the sorted IDs that depend on id.
func (g *Graph) Dependents(id string) []string {
	var out []string
	for from, m := range g.edges {
		if _, ok := m[id]; ok {
			out = append(out, from)
		}
	}
	sort.Strings(out)
	return out
}

// Merge adds every package and edge of o into g.
func (g *Graph) Merge(o *Graph) {
	if o == nil {
		return
	}
	for _, p := range o.Packages() {
		g.Add(p)
	}
	for from, m := range o.edges {
		for to := range m {
			_ = g.AddEdge(from, to)
		}
	}
}

// PathTo returns the shortest dependency chain from a direct dependency to
// id (inclusive), or nil when id is unreachable from any direct dependency.
// It is useful to explain why a transitive package is present.
func (g *Graph) PathTo(id string) []string {
	if _, ok := g.pkgs[id]; !ok {
		return nil
	}
	prev := map[string]string{}
	var queue []string
	for _, p := range g.Direct() {
		pid := p.ID()
		prev[pid] = ""
		queue = append(queue, pid)
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == id {
			var path []string
			for n := cur; n != ""; n = prev[n] {
				path = append(path, n)
			}
			slices.Reverse(path)
			return path
		}
		for _, next := range g.DependenciesOf(cur) {
			if _, seen := prev[next]; !seen {
				prev[next] = cur
				queue = append(queue, next)
			}
		}
	}
	return nil
}

// ErrUnsupportedFormat is returned for recognised but unsupported formats
// (for example a Yarn Berry lockfile).
var ErrUnsupportedFormat = errors.New("supplychain: unsupported lockfile format")

// ParseError reports a syntax or schema error in a manifest.
type ParseError struct {
	File string
	Line int
	Msg  string
}

func (e *ParseError) Error() string {
	switch {
	case e.File != "" && e.Line > 0:
		return fmt.Sprintf("%s:%d: %s", e.File, e.Line, e.Msg)
	case e.File != "":
		return e.File + ": " + e.Msg
	case e.Line > 0:
		return fmt.Sprintf("line %d: %s", e.Line, e.Msg)
	}
	return e.Msg
}

func perr(file string, line int, format string, args ...any) error {
	return &ParseError{File: file, Line: line, Msg: fmt.Sprintf(format, args...)}
}
