package supplychain

import (
	"encoding/hex"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// ---- Cargo.lock ------------------------------------------------------------

// ParseCargoLock parses Cargo.lock (format versions 1 through 4).
// Packages without a source are workspace members; their dependencies
// are the direct dependencies of the workspace.
func ParseCargoLock(file string, data []byte) (*Graph, error) {
	doc, err := parseTOML(file, data)
	if err != nil {
		return nil, err
	}
	if v, ok := doc["version"]; ok {
		n, isInt := v.(int64)
		if !isInt || n < 1 || n > 4 {
			return nil, perr(file, 0, "Cargo.lock: unsupported version %v", v)
		}
	}
	type crate struct {
		id, name, version, source string
		deps                      []string
	}
	var crates []crate
	g := NewGraph()
	legacy := tomlTableOf(doc, "metadata")
	for _, pt := range tomlTables(doc, "package") {
		c := crate{name: tomlString(pt, "name"), version: tomlString(pt, "version"), source: tomlString(pt, "source")}
		if c.name == "" || c.version == "" {
			return nil, perr(file, 0, "Cargo.lock: package without name or version")
		}
		if _, err := ParseSemVer(c.version); err != nil {
			return nil, perr(file, 0, "Cargo.lock: package %s: %v", c.name, err)
		}
		p := &Package{Ecosystem: EcosystemCargo, Name: c.name, Version: c.version, Sources: []string{file}}
		switch {
		case c.source == "":
			p.Local = true
		case strings.HasPrefix(c.source, "git+"):
			p.Resolved = strings.TrimPrefix(c.source, "git+")
		}
		sum := tomlString(pt, "checksum")
		if sum == "" && legacy != nil {
			sum = tomlString(legacy, "checksum "+c.name+" "+c.version+" ("+c.source+")")
		}
		if sum != "" {
			if b, err := hex.DecodeString(sum); err != nil || len(b) != 32 {
				return nil, perr(file, 0, "Cargo.lock: package %s: malformed checksum", c.name)
			}
			p.AddHash(Hash{Alg: HashSHA256, Hex: sum})
		}
		if arr, ok := pt["dependencies"].([]any); ok {
			for _, d := range arr {
				s, ok := d.(string)
				if !ok {
					return nil, perr(file, 0, "Cargo.lock: package %s: dependency is not a string", c.name)
				}
				c.deps = append(c.deps, s)
			}
		}
		c.id = g.Add(p).ID()
		crates = append(crates, c)
	}
	for _, c := range crates {
		for _, d := range c.deps {
			f := strings.Fields(d)
			var match []crate
			for _, o := range crates {
				if o.name != f[0] {
					continue
				}
				if len(f) >= 2 && o.version != f[1] {
					continue
				}
				if len(f) >= 3 && "("+o.source+")" != strings.Join(f[2:], " ") {
					continue
				}
				match = append(match, o)
			}
			if len(match) != 1 {
				return nil, perr(file, 0, "Cargo.lock: package %s: dependency %q resolves to %d packages", c.name, d, len(match))
			}
			if err := g.AddEdge(c.id, match[0].id); err != nil {
				return nil, err
			}
			if c.source == "" && match[0].source != "" {
				g.Package(match[0].id).Direct = true
			}
		}
	}
	return g, nil
}

// ---- Gemfile.lock ----------------------------------------------------------

var gemSpecRe = regexp.MustCompile(`^([^ (]+)(?: \(([^-)]*)(?:-([^)]*))?\))?$`)
var gemDepRe = regexp.MustCompile(`^([^ (!]+)(?: \(([^)]*)\))?(!)?$`)

// ParseGemfileLock parses a Bundler Gemfile.lock.
func ParseGemfileLock(file string, data []byte) (*Graph, error) {
	g := NewGraph()
	type spec struct {
		id   string
		deps []string
	}
	var specs []spec
	byName := map[string][]string{}
	var direct []string
	checksums := map[string]string{} // "name version" -> hex
	section := ""
	var cur *spec
	srcType, remote := "", ""
	for idx, raw := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		num := idx + 1
		if strings.TrimSpace(raw) == "" {
			continue
		}
		trimmed := strings.TrimLeft(raw, " ")
		indent := len(raw) - len(trimmed)
		trimmed = strings.TrimRight(trimmed, " ")
		if indent == 0 {
			section = trimmed
			cur = nil
			switch section {
			case "GEM", "GIT", "PATH", "PLUGIN SOURCE":
				srcType, remote = section, ""
			case "PLATFORMS", "DEPENDENCIES", "RUBY VERSION", "BUNDLED WITH", "CHECKSUMS":
			default:
				return nil, perr(file, num, "Gemfile.lock: unknown section %q", section)
			}
			continue
		}
		switch section {
		case "GEM", "GIT", "PATH", "PLUGIN SOURCE":
			switch indent {
			case 2:
				if k, v, ok := strings.Cut(trimmed, ": "); ok && k == "remote" {
					remote = v
				} else if trimmed != "specs:" && !ok {
					return nil, perr(file, num, "Gemfile.lock: unexpected %q", trimmed)
				}
			case 4:
				m := gemSpecRe.FindStringSubmatch(trimmed)
				if m == nil || m[2] == "" {
					return nil, perr(file, num, "Gemfile.lock: malformed spec %q", trimmed)
				}
				if _, err := gemSegments(m[2]); err != nil {
					return nil, perr(file, num, "Gemfile.lock: %v", err)
				}
				p := &Package{Ecosystem: EcosystemRubyGems, Name: m[1], Version: m[2], Sources: []string{file}}
				if m[3] != "" && m[3] != "ruby" {
					p.Qualifiers = map[string]string{"platform": m[3]}
				}
				switch srcType {
				case "GIT":
					p.Resolved = remote
				case "PATH":
					p.Local = true
				case "GEM":
					if remote != "" && remote != "https://rubygems.org/" {
						p.Qualifiers = mergeQual(p.Qualifiers, "repository_url", remote)
					}
				}
				id := g.Add(p).ID()
				specs = append(specs, spec{id: id})
				cur = &specs[len(specs)-1]
				byName[m[1]] = append(byName[m[1]], id)
			case 6:
				if cur == nil {
					return nil, perr(file, num, "Gemfile.lock: dependency outside a spec")
				}
				m := gemDepRe.FindStringSubmatch(trimmed)
				if m == nil {
					return nil, perr(file, num, "Gemfile.lock: malformed dependency %q", trimmed)
				}
				cur.deps = append(cur.deps, m[1])
			default:
				return nil, perr(file, num, "Gemfile.lock: unexpected indentation")
			}
		case "DEPENDENCIES":
			m := gemDepRe.FindStringSubmatch(trimmed)
			if indent != 2 || m == nil {
				return nil, perr(file, num, "Gemfile.lock: malformed dependency %q", trimmed)
			}
			direct = append(direct, m[1])
		case "CHECKSUMS":
			name, rest, ok := strings.Cut(trimmed, " (")
			if !ok {
				return nil, perr(file, num, "Gemfile.lock: malformed checksum %q", trimmed)
			}
			ver, sums, _ := strings.Cut(rest, ")")
			if i := strings.IndexByte(ver, '-'); i >= 0 {
				ver = ver[:i]
			}
			for _, s := range strings.Split(strings.TrimSpace(sums), ",") {
				if hx, ok := strings.CutPrefix(strings.TrimSpace(s), "sha256="); ok {
					checksums[name+" "+ver] = hx
				}
			}
		}
	}
	for _, s := range specs {
		for _, d := range s.deps {
			for _, to := range byName[d] {
				if err := g.AddEdge(s.id, to); err != nil {
					return nil, err
				}
			}
		}
	}
	for _, d := range direct {
		for _, id := range byName[d] {
			g.Package(id).Direct = true
		}
	}
	for _, p := range g.Packages() {
		if hx, ok := checksums[p.Name+" "+p.Version]; ok {
			if b, err := hex.DecodeString(hx); err == nil && len(b) == 32 {
				p.AddHash(Hash{Alg: HashSHA256, Hex: hx})
			}
		}
	}
	return g, nil
}

func mergeQual(q map[string]string, k, v string) map[string]string {
	if q == nil {
		q = map[string]string{}
	}
	q[k] = v
	return q
}

// ---- composer.lock -------------------------------------------------------

type composerPackage struct {
	Name    string            `json:"name"`
	Version string            `json:"version"`
	Require map[string]string `json:"require"`
	License []string          `json:"license"`
	Source  *struct {
		Type, URL, Reference string
	} `json:"source"`
	Dist *struct {
		Type, URL, Reference, Shasum string
	} `json:"dist"`
}

type composerLock struct {
	Packages    []composerPackage `json:"packages"`
	PackagesDev []composerPackage `json:"packages-dev"`
	ContentHash string            `json:"content-hash"`
}

type composerJSON struct {
	Require    map[string]string `json:"require"`
	RequireDev map[string]string `json:"require-dev"`
}

func composerPlatformPackage(name string) bool {
	n := strings.ToLower(name)
	return n == "php" || n == "hhvm" || n == "composer" || n == "composer-plugin-api" ||
		n == "composer-runtime-api" || strings.HasPrefix(n, "ext-") || strings.HasPrefix(n, "lib-") ||
		strings.HasPrefix(n, "php-")
}

// ParseComposerLock parses composer.lock; manifest (may be nil) is the
// sibling composer.json used to identify direct dependencies.
func ParseComposerLock(file string, data, manifest []byte) (*Graph, error) {
	var lf composerLock
	if err := strictJSON(data, &lf); err != nil {
		return nil, perr(file, 0, "composer.lock: %v", err)
	}
	g := NewGraph()
	byName := map[string]string{}
	add := func(list []composerPackage, dev bool) error {
		for _, cp := range list {
			if cp.Name == "" || cp.Version == "" || !strings.Contains(cp.Name, "/") {
				return perr(file, 0, "composer.lock: invalid package %q", cp.Name)
			}
			p := &Package{Ecosystem: EcosystemPackagist, Name: strings.ToLower(cp.Name), Version: cp.Version, Dev: dev, Sources: []string{file}}
			if len(cp.License) == 1 {
				p.AddLicense(NormalizeLicense(cp.License[0]))
			} else if len(cp.License) > 1 {
				parts := make([]string, len(cp.License))
				for i, l := range cp.License {
					parts[i] = NormalizeLicense(l)
				}
				p.AddLicense("(" + strings.Join(parts, " OR ") + ")")
			}
			if cp.Dist != nil {
				if strings.HasPrefix(cp.Dist.URL, "http") {
					p.Resolved = cp.Dist.URL
				}
				if cp.Dist.Shasum != "" {
					if b, err := hex.DecodeString(cp.Dist.Shasum); err == nil && len(b) == 20 {
						p.AddHash(Hash{Alg: HashSHA1, Hex: cp.Dist.Shasum})
					}
				}
				if cp.Dist.Type == "path" {
					p.Local = true
				}
			}
			byName[p.Name] = g.Add(p).ID()
		}
		return nil
	}
	if err := add(lf.Packages, false); err != nil {
		return nil, err
	}
	if err := add(lf.PackagesDev, true); err != nil {
		return nil, err
	}
	required := map[string]bool{}
	for _, list := range [][]composerPackage{lf.Packages, lf.PackagesDev} {
		for _, cp := range list {
			from := byName[strings.ToLower(cp.Name)]
			names := make([]string, 0, len(cp.Require))
			for n := range cp.Require {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				if composerPlatformPackage(n) {
					continue
				}
				if to, ok := byName[strings.ToLower(n)]; ok {
					required[to] = true
					if err := g.AddEdge(from, to); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	if manifest != nil {
		var cj composerJSON
		if err := json.Unmarshal(manifest, &cj); err != nil {
			return nil, perr(file, 0, "composer.json: %v", err)
		}
		for _, req := range []map[string]string{cj.Require, cj.RequireDev} {
			for n := range req {
				if id, ok := byName[strings.ToLower(n)]; ok {
					g.Package(id).Direct = true
				}
			}
		}
	} else {
		for _, p := range g.Packages() {
			if !required[p.ID()] {
				p.Direct = true
			}
		}
	}
	return g, nil
}
