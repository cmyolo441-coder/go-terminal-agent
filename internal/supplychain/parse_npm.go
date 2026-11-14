package supplychain

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
)

// PackageJSON is the subset of package.json relevant to supply-chain
// analysis.
type PackageJSON struct {
	Name                 string            `json:"name"`
	Version              string            `json:"version"`
	License              json.RawMessage   `json:"license"`
	Licenses             json.RawMessage   `json:"licenses"`
	Dependencies         map[string]string `json:"dependencies"`
	DevDependencies      map[string]string `json:"devDependencies"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
	PeerDependencies     map[string]string `json:"peerDependencies"`
	Scripts              map[string]string `json:"scripts"`
}

// InstallScriptHooks are the npm lifecycle scripts that run on install.
var InstallScriptHooks = []string{"preinstall", "install", "postinstall", "prepare"}

// ParsePackageJSON parses package.json.
func ParsePackageJSON(file string, data []byte) (*PackageJSON, error) {
	var p PackageJSON
	if err := strictJSON(data, &p); err != nil {
		return nil, perr(file, 0, "package.json: %v", err)
	}
	return &p, nil
}

// LicenseExpression returns the declared license, handling the legacy
// object and array forms.
func (p *PackageJSON) LicenseExpression() string {
	return npmLicenseString(p.License, p.Licenses)
}

// InstallScripts returns the install-time lifecycle scripts declared.
func (p *PackageJSON) InstallScripts() map[string]string {
	out := map[string]string{}
	for _, h := range InstallScriptHooks {
		if s, ok := p.Scripts[h]; ok && strings.TrimSpace(s) != "" {
			out[h] = s
		}
	}
	return out
}

func npmLicenseString(license, licenses json.RawMessage) string {
	if len(license) > 0 {
		var s string
		if json.Unmarshal(license, &s) == nil {
			return NormalizeLicense(s)
		}
		var o struct{ Type string }
		if json.Unmarshal(license, &o) == nil && o.Type != "" {
			return NormalizeLicense(o.Type)
		}
	}
	if len(licenses) > 0 {
		var arr []struct{ Type string }
		if json.Unmarshal(licenses, &arr) == nil {
			var parts []string
			for _, l := range arr {
				if l.Type != "" {
					parts = append(parts, NormalizeLicense(l.Type))
				}
			}
			if len(parts) > 1 {
				return "(" + strings.Join(parts, " OR ") + ")"
			}
			if len(parts) == 1 {
				return parts[0]
			}
		}
	}
	return ""
}

func strictJSON(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("trailing data after JSON value")
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err == nil {
		return fmt.Errorf("trailing data after JSON value")
	}
	return nil
}

// parseSRI converts a Subresource Integrity string (possibly several
// space-separated hashes) to hex hashes.
func parseSRI(s string) []Hash {
	var out []Hash
	for _, part := range strings.Fields(s) {
		alg, b64, ok := strings.Cut(part, "-")
		if !ok {
			continue
		}
		if i := strings.IndexByte(b64, '?'); i >= 0 {
			b64 = b64[:i]
		}
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			continue
		}
		var h HashAlg
		switch strings.ToLower(alg) {
		case "sha1":
			h = HashSHA1
		case "sha256":
			h = HashSHA256
		case "sha384":
			h = HashSHA384
		case "sha512":
			h = HashSHA512
		default:
			continue
		}
		out = append(out, Hash{Alg: h, Hex: hex.EncodeToString(raw)})
	}
	return out
}

// ---- package-lock.json --------------------------------------------------

type npmLockPackage struct {
	Name                 string            `json:"name"`
	Version              string            `json:"version"`
	Resolved             string            `json:"resolved"`
	Integrity            string            `json:"integrity"`
	Dev                  bool              `json:"dev"`
	Optional             bool              `json:"optional"`
	DevOptional          bool              `json:"devOptional"`
	Link                 bool              `json:"link"`
	HasInstallScript     bool              `json:"hasInstallScript"`
	License              json.RawMessage   `json:"license"`
	Dependencies         map[string]string `json:"dependencies"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
	DevDependencies      map[string]string `json:"devDependencies"`
	PeerDependencies     map[string]string `json:"peerDependencies"`
}

type npmLockDepV1 struct {
	Version      string                   `json:"version"`
	Resolved     string                   `json:"resolved"`
	Integrity    string                   `json:"integrity"`
	Dev          bool                     `json:"dev"`
	Optional     bool                     `json:"optional"`
	Bundled      bool                     `json:"bundled"`
	Requires     map[string]string        `json:"requires"`
	Dependencies map[string]*npmLockDepV1 `json:"dependencies"`
}

type npmLockFile struct {
	Name            string                     `json:"name"`
	Version         string                     `json:"version"`
	LockfileVersion int                        `json:"lockfileVersion"`
	Packages        map[string]*npmLockPackage `json:"packages"`
	Dependencies    map[string]*npmLockDepV1   `json:"dependencies"`
}

// ParsePackageLock parses package-lock.json / npm-shrinkwrap.json
// (lockfileVersion 1, 2 and 3). manifest is the sibling package.json and
// may be nil; it is only needed to identify direct dependencies for v1.
func ParsePackageLock(file string, data []byte, manifest *PackageJSON) (*Graph, error) {
	var lf npmLockFile
	if err := strictJSON(data, &lf); err != nil {
		return nil, perr(file, 0, "package-lock: %v", err)
	}
	var pkgs map[string]*npmLockPackage
	switch lf.LockfileVersion {
	case 1:
		if lf.Dependencies == nil {
			return nil, perr(file, 0, "package-lock v1: missing dependencies")
		}
		pkgs = flattenNPMv1(lf.Dependencies, manifest)
	case 2, 3:
		if lf.Packages == nil {
			return nil, perr(file, 0, "package-lock v%d: missing packages", lf.LockfileVersion)
		}
		pkgs = lf.Packages
		if _, ok := pkgs[""]; !ok {
			root := &npmLockPackage{Name: lf.Name, Version: lf.Version}
			if manifest != nil {
				root.Dependencies = manifest.Dependencies
				root.DevDependencies = manifest.DevDependencies
				root.OptionalDependencies = manifest.OptionalDependencies
			}
			pkgs[""] = root
		}
	default:
		return nil, perr(file, 0, "package-lock: unsupported lockfileVersion %d", lf.LockfileVersion)
	}
	for k, v := range pkgs {
		if v == nil {
			return nil, perr(file, 0, "package-lock: null entry for %q", k)
		}
		if k != "" && !v.Link && !strings.Contains(k, "node_modules/") && v.Version == "" {
			return nil, perr(file, 0, "package-lock: workspace %q has no version", k)
		}
	}
	return npmTreeGraph(file, pkgs)
}

func flattenNPMv1(deps map[string]*npmLockDepV1, manifest *PackageJSON) map[string]*npmLockPackage {
	out := map[string]*npmLockPackage{}
	var walk func(prefix string, deps map[string]*npmLockDepV1)
	walk = func(prefix string, deps map[string]*npmLockDepV1) {
		for name, d := range deps {
			if d == nil {
				continue
			}
			key := prefix + "node_modules/" + name
			p := &npmLockPackage{Version: d.Version, Resolved: d.Resolved, Integrity: d.Integrity,
				Dev: d.Dev, Optional: d.Optional, Dependencies: d.Requires}
			// v1 encodes aliases as "npm:real@version".
			if real, ok := strings.CutPrefix(d.Version, "npm:"); ok {
				if i := strings.LastIndexByte(real, '@'); i > 0 {
					p.Name, p.Version = real[:i], real[i+1:]
				}
			}
			out[key] = p
			walk(key+"/", d.Dependencies)
		}
	}
	walk("", deps)
	root := &npmLockPackage{}
	if manifest != nil {
		root.Name, root.Version = manifest.Name, manifest.Version
		root.Dependencies = manifest.Dependencies
		root.DevDependencies = manifest.DevDependencies
		root.OptionalDependencies = manifest.OptionalDependencies
	} else {
		// Without a manifest, treat top-level packages nobody requires as
		// direct dependencies.
		required := map[string]bool{}
		for _, p := range out {
			for n := range p.Dependencies {
				required[n] = true
			}
		}
		root.Dependencies = map[string]string{}
		for name := range deps {
			if !required[name] {
				root.Dependencies[name] = "*"
			}
		}
	}
	out[""] = root
	return out
}

func npmNameFromPath(key string) string {
	i := strings.LastIndex(key, "node_modules/")
	if i < 0 {
		return path.Base(key)
	}
	return key[i+len("node_modules/"):]
}

func npmParentDir(dir string) (string, bool) {
	if dir == "" {
		return "", false
	}
	if i := strings.LastIndex(dir, "/node_modules/"); i >= 0 {
		return dir[:i], true
	}
	return "", true
}

func npmResolve(pkgs map[string]*npmLockPackage, from, dep string) (string, bool) {
	dir := from
	for {
		var cand string
		if dir == "" {
			cand = "node_modules/" + dep
		} else {
			cand = dir + "/node_modules/" + dep
		}
		if _, ok := pkgs[cand]; ok {
			return cand, true
		}
		parent, ok := npmParentDir(dir)
		if !ok {
			return "", false
		}
		dir = parent
	}
}

func npmTreeGraph(file string, pkgs map[string]*npmLockPackage) (*Graph, error) {
	g := NewGraph()
	ids := map[string]string{} // path key -> package id
	keys := make([]string, 0, len(pkgs))
	for k := range pkgs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// follow links to their targets
	target := func(k string) (string, *npmLockPackage) {
		p := pkgs[k]
		for hops := 0; p != nil && p.Link && hops < 8; hops++ {
			t := strings.TrimPrefix(p.Resolved, "file:")
			if _, ok := pkgs[t]; !ok {
				return k, p
			}
			k, p = t, pkgs[t]
		}
		return k, p
	}
	for _, k := range keys {
		if k == "" {
			continue
		}
		p := pkgs[k]
		if p.Link {
			continue
		}
		// p.Name is set for aliases (folder name differs from the package
		// name) and for workspace folders.
		name := p.Name
		if name == "" {
			name = npmNameFromPath(k)
		}
		if name == "" || p.Version == "" && strings.Contains(k, "node_modules/") {
			// extraneous or bundled entries without a version cannot be
			// identified; skip them rather than invent a version.
			continue
		}
		local := !strings.Contains(k, "node_modules/") || strings.HasPrefix(p.Resolved, "file:")
		node := &Package{Ecosystem: EcosystemNPM, Name: name, Version: p.Version, Dev: p.Dev || p.DevOptional,
			Optional: p.Optional, Local: local, InstallScript: p.HasInstallScript, Sources: []string{file}}
		if strings.HasPrefix(p.Resolved, "http") {
			node.Resolved = p.Resolved
		}
		if l := npmLicenseString(p.License, nil); l != "" {
			node.AddLicense(l)
		}
		for _, h := range parseSRI(p.Integrity) {
			node.AddHash(h)
		}
		ids[k] = g.Add(node).ID()
	}
	idOf := func(k string) (string, bool) {
		tk, _ := target(k)
		id, ok := ids[tk]
		return id, ok
	}
	for _, k := range keys {
		p := pkgs[k]
		if p.Link {
			continue
		}
		fromID, fromOK := ids[k]
		isRoot := k == "" || !strings.Contains(k, "node_modules/")
		depMaps := []map[string]string{p.Dependencies, p.OptionalDependencies, p.PeerDependencies}
		if isRoot {
			// devDependencies of installed packages are never installed.
			depMaps = append(depMaps, p.DevDependencies)
		}
		for _, deps := range depMaps {
			for dep := range deps {
				rk, ok := npmResolve(pkgs, k, dep)
				if !ok {
					continue
				}
				toID, ok := idOf(rk)
				if !ok {
					continue
				}
				if isRoot {
					g.Package(toID).Direct = true
				}
				if fromOK && fromID != toID {
					if err := g.AddEdge(fromID, toID); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	return g, nil
}

// ---- yarn.lock (v1) ----------------------------------------------------

type yarnEntry struct {
	specs        []string
	name         string
	version      string
	resolved     string
	integrity    string
	dependencies map[string]string
	optional     map[string]string
	line         int
}

func splitNPMSpec(spec string) (name, rng string, ok bool) {
	at := strings.IndexByte(spec[min(1, len(spec)):], '@')
	if at < 0 {
		return "", "", false
	}
	at += min(1, len(spec))
	return spec[:at], spec[at+1:], true
}

func yarnUnquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		var out string
		if json.Unmarshal([]byte(s), &out) == nil {
			return out
		}
		return s[1 : len(s)-1]
	}
	return s
}

// ParseYarnLock parses a Yarn classic (v1) lockfile. Yarn Berry lockfiles
// return ErrUnsupportedFormat. manifest may be nil.
func ParseYarnLock(file string, data []byte, manifest *PackageJSON) (*Graph, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if strings.Contains(text, "\n__metadata:") || strings.HasPrefix(text, "__metadata:") {
		return nil, fmt.Errorf("%s: yarn berry lockfile: %w", file, ErrUnsupportedFormat)
	}
	var entries []*yarnEntry
	var cur *yarnEntry
	var section map[string]string
	for idx, raw := range strings.Split(text, "\n") {
		num := idx + 1
		if strings.TrimSpace(raw) == "" || strings.HasPrefix(strings.TrimSpace(raw), "#") {
			continue
		}
		trimmed := strings.TrimLeft(raw, " ")
		indent := len(raw) - len(trimmed)
		trimmed = strings.TrimRight(trimmed, " \t")
		switch indent {
		case 0:
			if !strings.HasSuffix(trimmed, ":") {
				return nil, perr(file, num, "yarn.lock: expected entry header ending in ':'")
			}
			cur = &yarnEntry{line: num, dependencies: map[string]string{}, optional: map[string]string{}}
			for _, spec := range strings.Split(strings.TrimSuffix(trimmed, ":"), ",") {
				spec = yarnUnquote(spec)
				if spec == "" {
					return nil, perr(file, num, "yarn.lock: empty specifier")
				}
				name, _, ok := splitNPMSpec(spec)
				if !ok {
					return nil, perr(file, num, "yarn.lock: malformed specifier %q", spec)
				}
				if cur.name == "" {
					cur.name = name
				} else if cur.name != name {
					return nil, perr(file, num, "yarn.lock: entry mixes packages %q and %q", cur.name, name)
				}
				cur.specs = append(cur.specs, spec)
			}
			entries = append(entries, cur)
			section = nil
		case 2:
			if cur == nil {
				return nil, perr(file, num, "yarn.lock: field outside of an entry")
			}
			section = nil
			if strings.HasSuffix(trimmed, ":") {
				switch strings.TrimSuffix(trimmed, ":") {
				case "dependencies":
					section = cur.dependencies
				case "optionalDependencies":
					section = cur.optional
				default:
					section = map[string]string{} // ignored nested section
				}
				continue
			}
			key, val, ok := strings.Cut(trimmed, " ")
			if !ok {
				return nil, perr(file, num, "yarn.lock: expected 'key value'")
			}
			val = yarnUnquote(val)
			switch yarnUnquote(key) {
			case "version":
				cur.version = val
			case "resolved":
				cur.resolved = val
			case "integrity":
				cur.integrity = val
			}
		case 4:
			if section == nil {
				return nil, perr(file, num, "yarn.lock: unexpected indentation")
			}
			k, v, ok := strings.Cut(trimmed, " ")
			if !ok {
				return nil, perr(file, num, "yarn.lock: expected 'name range'")
			}
			section[yarnUnquote(k)] = yarnUnquote(v)
		default:
			return nil, perr(file, num, "yarn.lock: unexpected indentation %d", indent)
		}
	}
	g := NewGraph()
	bySpec := map[string]string{}
	required := map[string]bool{}
	for _, e := range entries {
		if e.version == "" {
			return nil, perr(file, e.line, "yarn.lock: entry %q has no version", e.name)
		}
		name := e.name
		// aliases: "alias@npm:real@^1"
		if _, rng, _ := splitNPMSpec(e.specs[0]); strings.HasPrefix(rng, "npm:") {
			if real, _, ok := splitNPMSpec(strings.TrimPrefix(rng, "npm:")); ok {
				name = real
			}
		}
		p := &Package{Ecosystem: EcosystemNPM, Name: name, Version: e.version, Sources: []string{file}}
		if strings.HasPrefix(e.resolved, "http") {
			p.Resolved = e.resolved
			if i := strings.IndexByte(e.resolved, '#'); i >= 0 && e.integrity == "" {
				if sha1, err := hex.DecodeString(e.resolved[i+1:]); err == nil && len(sha1) == 20 {
					p.AddHash(Hash{Alg: HashSHA1, Hex: e.resolved[i+1:]})
				}
			}
		} else if strings.HasPrefix(e.resolved, "file:") || strings.HasPrefix(e.version, "0.0.0-use.local") {
			p.Local = true
		}
		for _, h := range parseSRI(e.integrity) {
			p.AddHash(h)
		}
		id := g.Add(p).ID()
		for _, s := range e.specs {
			bySpec[s] = id
		}
	}
	for _, e := range entries {
		from := bySpec[e.specs[0]]
		for _, deps := range []map[string]string{e.dependencies, e.optional} {
			for n, r := range deps {
				if to, ok := bySpec[n+"@"+r]; ok {
					required[to] = true
					if err := g.AddEdge(from, to); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	if manifest != nil {
		for _, deps := range []map[string]string{manifest.Dependencies, manifest.OptionalDependencies, manifest.DevDependencies} {
			for n, r := range deps {
				if id, ok := bySpec[n+"@"+r]; ok {
					g.Package(id).Direct = true
				}
			}
		}
		markDevOnly(g, manifest.DevDependencies, bySpec)
	} else {
		for _, p := range g.Packages() {
			if !required[p.ID()] {
				p.Direct = true
			}
		}
	}
	return g, nil
}

// markDevOnly flags packages reachable only from dev dependencies.
func markDevOnly(g *Graph, devDeps map[string]string, bySpec map[string]string) {
	if len(devDeps) == 0 {
		return
	}
	devRoots := map[string]bool{}
	for n, r := range devDeps {
		if id, ok := bySpec[n+"@"+r]; ok {
			devRoots[id] = true
		}
	}
	var prodRoots []string
	for _, p := range g.Direct() {
		if !devRoots[p.ID()] {
			prodRoots = append(prodRoots, p.ID())
		}
	}
	prod := reachable(g, prodRoots)
	var devList []string
	for id := range devRoots {
		devList = append(devList, id)
	}
	for id := range reachable(g, devList) {
		if !prod[id] {
			g.Package(id).Dev = true
		}
	}
}

func reachable(g *Graph, roots []string) map[string]bool {
	seen := map[string]bool{}
	stack := append([]string(nil), roots...)
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[n] {
			continue
		}
		seen[n] = true
		stack = append(stack, g.DependenciesOf(n)...)
	}
	return seen
}

// ---- pnpm-lock.yaml ------------------------------------------------------

// ParsePNPMLock parses pnpm-lock.yaml (lockfile versions 5.x, 6.x and 9.x).
func ParsePNPMLock(file string, data []byte) (*Graph, error) {
	root, err := parseYAMLSubset(file, data)
	if err != nil {
		return nil, err
	}
	if root.kind != yMap {
		return nil, perr(file, 0, "pnpm-lock: top level must be a mapping")
	}
	verStr := root.get("lockfileVersion").str()
	if verStr == "" {
		return nil, perr(file, 0, "pnpm-lock: missing lockfileVersion")
	}
	major := 0
	for _, c := range verStr {
		if c < '0' || c > '9' {
			break
		}
		major = major*10 + int(c-'0')
	}
	if major < 5 || major > 9 {
		return nil, perr(file, 0, "pnpm-lock: unsupported lockfileVersion %q", verStr)
	}
	g := NewGraph()
	nodeID := map[string]string{} // snapshot/package key (with peers) -> package id
	packages := root.get("packages")
	snapshots := root.get("snapshots")
	addPkg := func(key string, n *yNode) error {
		name, version, ok := pnpmParseKey(key, major)
		if !ok {
			return perr(file, nodeLine(n), "pnpm-lock: cannot parse package key %q", key)
		}
		p := &Package{Ecosystem: EcosystemNPM, Name: name, Version: version, Sources: []string{file}}
		if n != nil && n.kind == yMap {
			if res := n.get("resolution"); res != nil {
				for _, h := range parseSRI(res.get("integrity").str()) {
					p.AddHash(h)
				}
				if t := res.get("tarball").str(); strings.HasPrefix(t, "http") {
					p.Resolved = t
				}
				if res.get("directory").str() != "" {
					p.Local = true
				}
			}
			p.Dev = n.get("dev").str() == "true"
			p.Optional = n.get("optional").str() == "true"
			p.InstallScript = n.get("requiresBuild").str() == "true"
			if n.get("name").str() != "" && n.get("version").str() != "" {
				p.Name, p.Version = n.get("name").str(), n.get("version").str()
			}
		}
		nodeID[key] = g.Add(p).ID()
		return nil
	}
	if packages != nil {
		if packages.kind != yMap {
			return nil, perr(file, packages.line, "pnpm-lock: packages must be a mapping")
		}
		for _, k := range packages.keys {
			if err := addPkg(k, packages.fields[k]); err != nil {
				return nil, err
			}
		}
	}
	if snapshots != nil {
		if snapshots.kind != yMap {
			return nil, perr(file, snapshots.line, "pnpm-lock: snapshots must be a mapping")
		}
		for _, k := range snapshots.keys {
			name, version, ok := pnpmParseKey(k, major)
			if !ok {
				return nil, perr(file, nodeLine(snapshots.fields[k]), "pnpm-lock: cannot parse snapshot key %q", k)
			}
			id := PackageID(EcosystemNPM, name, version)
			if g.Package(id) == nil {
				g.Add(&Package{Ecosystem: EcosystemNPM, Name: name, Version: version, Sources: []string{file}})
			}
			nodeID[k] = id
		}
	}
	resolveRef := func(depName, ref string) (string, bool) {
		if strings.HasPrefix(ref, "link:") || strings.HasPrefix(ref, "file:") {
			return "", false
		}
		candidates := []string{}
		if strings.HasPrefix(ref, "/") || strings.Contains(strings.SplitN(ref, "(", 2)[0], "@") {
			candidates = append(candidates, ref) // full key (aliases)
		}
		if major >= 9 {
			candidates = append(candidates, depName+"@"+ref)
		} else if major >= 6 {
			candidates = append(candidates, "/"+depName+"@"+ref)
		} else {
			candidates = append(candidates, "/"+depName+"/"+ref)
		}
		for _, c := range candidates {
			if id, ok := nodeID[c]; ok {
				return id, true
			}
		}
		// fall back to the peer-less key
		base := strings.SplitN(ref, "(", 2)[0]
		if major < 6 {
			base = strings.SplitN(base, "_", 2)[0]
		}
		id := PackageID(EcosystemNPM, depName, base)
		if g.Package(id) != nil {
			return id, true
		}
		return "", false
	}
	edgeSource := packages
	if snapshots != nil {
		edgeSource = snapshots
	}
	if edgeSource != nil {
		for _, k := range edgeSource.keys {
			n := edgeSource.fields[k]
			from := nodeID[k]
			for _, sec := range []string{"dependencies", "optionalDependencies"} {
				deps := n.get(sec)
				if deps == nil {
					continue
				}
				for _, dn := range deps.keys {
					if to, ok := resolveRef(dn, deps.fields[dn].str()); ok {
						if err := g.AddEdge(from, to); err != nil {
							return nil, err
						}
					}
				}
			}
		}
	}
	// importers: v9 always, v6 for workspaces; otherwise the root itself.
	var importers []*yNode
	if imp := root.get("importers"); imp != nil && imp.kind == yMap {
		for _, k := range imp.keys {
			importers = append(importers, imp.fields[k])
		}
	} else {
		importers = append(importers, root)
	}
	var prodRoots, devRoots []string
	for _, imp := range importers {
		for _, sec := range []string{"dependencies", "devDependencies", "optionalDependencies"} {
			deps := imp.get(sec)
			if deps == nil || deps.kind != yMap {
				continue
			}
			for _, dn := range deps.keys {
				d := deps.fields[dn]
				ref := d.str()
				if d.kind == yMap {
					ref = d.get("version").str()
				}
				id, ok := resolveRef(dn, ref)
				if !ok {
					continue
				}
				g.Package(id).Direct = true
				if sec == "devDependencies" {
					devRoots = append(devRoots, id)
				} else {
					prodRoots = append(prodRoots, id)
				}
			}
		}
	}
	if major >= 9 && len(devRoots) > 0 {
		prod := reachable(g, prodRoots)
		for id := range reachable(g, devRoots) {
			if !prod[id] {
				g.Package(id).Dev = true
			}
		}
	}
	return g, nil
}

func nodeLine(n *yNode) int {
	if n == nil {
		return 0
	}
	return n.line
}

// pnpmParseKey extracts name and version from a pnpm package key:
// v5 "/name/1.0.0_peer", v6 "/name@1.0.0(peer)", v9 "name@1.0.0(peer)".
func pnpmParseKey(key string, major int) (string, string, bool) {
	k := strings.TrimPrefix(key, "/")
	if major < 6 && strings.HasPrefix(key, "/") {
		i := strings.LastIndexByte(k, '/')
		if i <= 0 {
			return "", "", false
		}
		name, ver := k[:i], k[i+1:]
		ver = strings.SplitN(ver, "_", 2)[0]
		return name, ver, name != "" && ver != ""
	}
	k = strings.SplitN(k, "(", 2)[0]
	name, ver, ok := splitNPMSpec(k)
	if !ok || name == "" || ver == "" {
		return "", "", false
	}
	return name, ver, true
}
