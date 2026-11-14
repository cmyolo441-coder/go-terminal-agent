package supplychain

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// GoMod is a parsed go.mod file.
type GoMod struct {
	Module    string
	Go        string
	Toolchain string
	Require   []GoRequire
	Replace   []GoReplace
	Exclude   []GoModuleVersion
	Retract   []string
	Tool      []string
}

// GoModuleVersion is a module path at a version.
type GoModuleVersion struct {
	Path    string
	Version string
}

// GoRequire is a require directive.
type GoRequire struct {
	Path     string
	Version  string
	Indirect bool
	Line     int
}

// GoReplace is a replace directive. NewVersion is empty for filesystem
// replacements; OldVersion is empty when every version is replaced.
type GoReplace struct {
	Old        string
	OldVersion string
	New        string
	NewVersion string
	Line       int
}

// IsLocal reports whether the replacement is a filesystem path.
func (r GoReplace) IsLocal() bool {
	return strings.HasPrefix(r.New, "./") || strings.HasPrefix(r.New, "../") ||
		strings.HasPrefix(r.New, "/") || r.New == "." || r.New == ".." ||
		(len(r.New) > 2 && r.New[1] == ':' && (r.New[2] == '\\' || r.New[2] == '/'))
}

// tokenizeGoModLine splits a go.mod line into tokens; the trailing "//"
// comment is returned separately.
func tokenizeGoModLine(line string, file string, num int) ([]string, string, error) {
	var toks []string
	i := 0
	for i < len(line) {
		c := line[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r':
			i++
		case strings.HasPrefix(line[i:], "//"):
			return toks, strings.TrimSpace(line[i+2:]), nil
		case c == '"':
			j := i + 1
			for j < len(line) && line[j] != '"' {
				if line[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(line) {
				return nil, "", perr(file, num, "unterminated quoted string")
			}
			s, err := strconv.Unquote(line[i : j+1])
			if err != nil {
				return nil, "", perr(file, num, "invalid quoted string: %v", err)
			}
			toks = append(toks, s)
			i = j + 1
		case c == '`':
			j := strings.IndexByte(line[i+1:], '`')
			if j < 0 {
				return nil, "", perr(file, num, "unterminated raw string")
			}
			toks = append(toks, line[i+1:i+1+j])
			i += j + 2
		case c == '(' || c == ')' || c == '[' || c == ']' || c == ',':
			toks = append(toks, string(c))
			i++
		case strings.HasPrefix(line[i:], "=>"):
			toks = append(toks, "=>")
			i += 2
		default:
			j := i
			for j < len(line) && !strings.ContainsRune(" \t\r()[],\"`", rune(line[j])) &&
				!strings.HasPrefix(line[j:], "//") && !strings.HasPrefix(line[j:], "=>") {
				j++
			}
			toks = append(toks, line[i:j])
			i = j
		}
	}
	return toks, "", nil
}

// ParseGoMod parses a go.mod file strictly: unknown directives, malformed
// module paths and non-canonical versions are errors.
func ParseGoMod(file string, data []byte) (*GoMod, error) {
	m := &GoMod{}
	lines := strings.Split(string(data), "\n")
	block := ""
	for idx, raw := range lines {
		num := idx + 1
		toks, comment, err := tokenizeGoModLine(raw, file, num)
		if err != nil {
			return nil, err
		}
		if len(toks) == 0 {
			continue
		}
		if block != "" {
			if len(toks) == 1 && toks[0] == ")" {
				block = ""
				continue
			}
			if err := m.directive(block, toks, comment, file, num); err != nil {
				return nil, err
			}
			continue
		}
		verb := toks[0]
		if len(toks) == 2 && toks[1] == "(" {
			switch verb {
			case "require", "replace", "exclude", "retract", "godebug", "tool", "ignore":
				block = verb
				continue
			}
			return nil, perr(file, num, "unknown block directive %q", verb)
		}
		if err := m.directive(verb, toks[1:], comment, file, num); err != nil {
			return nil, err
		}
	}
	if block != "" {
		return nil, perr(file, len(lines), "unterminated %s block", block)
	}
	if m.Module == "" {
		return nil, perr(file, 0, "missing module directive")
	}
	return m, nil
}

func (m *GoMod) directive(verb string, args []string, comment, file string, num int) error {
	switch verb {
	case "module":
		if len(args) != 1 {
			return perr(file, num, "usage: module path")
		}
		if m.Module != "" {
			return perr(file, num, "repeated module directive")
		}
		if err := checkGoModulePath(args[0]); err != nil {
			return perr(file, num, "%v", err)
		}
		m.Module = args[0]
	case "go":
		if len(args) != 1 || !isGoLangVersion(args[0]) {
			return perr(file, num, "usage: go 1.N[.P]")
		}
		m.Go = args[0]
	case "toolchain":
		if len(args) != 1 || !strings.HasPrefix(args[0], "go") && args[0] != "default" {
			return perr(file, num, "usage: toolchain goVERSION")
		}
		m.Toolchain = args[0]
	case "require":
		if len(args) != 2 {
			return perr(file, num, "usage: require module/path v1.2.3")
		}
		if err := checkGoModuleVersion(args[0], args[1]); err != nil {
			return perr(file, num, "%v", err)
		}
		indirect := false
		for _, f := range strings.FieldsFunc(comment, func(r rune) bool { return r == ' ' || r == ';' }) {
			if f == "indirect" {
				indirect = true
			}
		}
		m.Require = append(m.Require, GoRequire{Path: args[0], Version: args[1], Indirect: indirect, Line: num})
	case "exclude":
		if len(args) != 2 {
			return perr(file, num, "usage: exclude module/path v1.2.3")
		}
		if err := checkGoModuleVersion(args[0], args[1]); err != nil {
			return perr(file, num, "%v", err)
		}
		m.Exclude = append(m.Exclude, GoModuleVersion{args[0], args[1]})
	case "replace":
		arrow := -1
		for i, a := range args {
			if a == "=>" {
				arrow = i
			}
		}
		if arrow != 1 && arrow != 2 || len(args)-arrow-1 < 1 || len(args)-arrow-1 > 2 {
			return perr(file, num, "usage: replace old [v] => new [v]")
		}
		r := GoReplace{Old: args[0], Line: num}
		if arrow == 2 {
			r.OldVersion = args[1]
			if err := checkGoModuleVersion(r.Old, r.OldVersion); err != nil {
				return perr(file, num, "%v", err)
			}
		} else if err := checkGoModulePath(r.Old); err != nil {
			return perr(file, num, "%v", err)
		}
		r.New = args[arrow+1]
		if len(args)-arrow-1 == 2 {
			r.NewVersion = args[arrow+2]
			if r.IsLocal() {
				return perr(file, num, "filesystem replacement %q must not have a version", r.New)
			}
			if err := checkGoModuleVersion(r.New, r.NewVersion); err != nil {
				return perr(file, num, "%v", err)
			}
		} else if !r.IsLocal() {
			return perr(file, num, "replacement module %q needs a version", r.New)
		}
		m.Replace = append(m.Replace, r)
	case "retract":
		if len(args) == 0 {
			return perr(file, num, "usage: retract version | [low, high]")
		}
		m.Retract = append(m.Retract, strings.Join(args, " "))
	case "tool":
		if len(args) != 1 {
			return perr(file, num, "usage: tool package/path")
		}
		m.Tool = append(m.Tool, args[0])
	case "godebug", "ignore":
		if len(args) == 0 {
			return perr(file, num, "usage: %s ...", verb)
		}
	default:
		return perr(file, num, "unknown directive %q", verb)
	}
	return nil
}

func isGoLangVersion(s string) bool {
	parts := strings.SplitN(s, ".", 3)
	if len(parts) < 2 || !isDigits(parts[0]) {
		return false
	}
	if !isDigits(parts[1]) {
		// 1.21rc1 style
		i := 0
		for i < len(parts[1]) && parts[1][i] >= '0' && parts[1][i] <= '9' {
			i++
		}
		return i > 0 && len(parts) == 2
	}
	if len(parts) == 3 {
		return isDigits(parts[2]) || len(parts[2]) > 0
	}
	return true
}

func checkGoModulePath(p string) error {
	if p == "" {
		return fmt.Errorf("empty module path")
	}
	if strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") || strings.Contains(p, "//") {
		return fmt.Errorf("malformed module path %q", p)
	}
	for _, elem := range strings.Split(p, "/") {
		if elem == "." || elem == ".." || strings.HasPrefix(elem, ".") || strings.HasSuffix(elem, ".") {
			return fmt.Errorf("malformed module path %q: invalid element %q", p, elem)
		}
	}
	for i := 0; i < len(p); i++ {
		c := p[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-._~/+", c) >= 0) {
			return fmt.Errorf("malformed module path %q: invalid char %q", p, c)
		}
	}
	return nil
}

// goPathMajor returns the major-version suffix of a module path ("v2"),
// or "" if none, and whether the path is a gopkg.in path.
func goPathMajor(p string) (string, bool) {
	if strings.HasPrefix(p, "gopkg.in/") {
		if i := strings.LastIndex(p, ".v"); i >= 0 && isDigits(strings.TrimSuffix(p[i+2:], "-unstable")) {
			return "v" + strings.TrimSuffix(p[i+2:], "-unstable"), true
		}
		return "", true
	}
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		last := p[i+1:]
		if len(last) >= 2 && last[0] == 'v' && isDigits(last[1:]) && last[1] != '0' && last != "v1" {
			return last, false
		}
	}
	return "", false
}

func checkGoModuleVersion(path, version string) error {
	if err := checkGoModulePath(path); err != nil {
		return err
	}
	if !strings.HasPrefix(version, "v") {
		return fmt.Errorf("module %s: version %q must start with 'v'", path, version)
	}
	gv, err := ParseGoVersion(version)
	if err != nil {
		return fmt.Errorf("module %s: %v", path, err)
	}
	major, gopkg := goPathMajor(path)
	switch {
	case gopkg:
		if major != "" && fmt.Sprintf("v%d", gv.Major) != major && !(major == "v0" || major == "v1") {
			return fmt.Errorf("module %s: version %s does not match gopkg.in major %s", path, version, major)
		}
	case major != "":
		if fmt.Sprintf("v%d", gv.Major) != major || gv.Incompatible {
			return fmt.Errorf("module %s: version %s does not match major suffix %s", path, version, major)
		}
	default:
		if gv.Major >= 2 && !gv.Incompatible {
			return fmt.Errorf("module %s: version %s requires a /v%d path suffix or +incompatible", path, version, gv.Major)
		}
	}
	return nil
}

// GoSumLine is one go.sum entry.
type GoSumLine struct {
	Path    string
	Version string
	// GoMod is true for "/go.mod" hashes (hash of the go.mod file only).
	GoMod bool
	// Hash is the raw "h1:" value.
	Hash string
}

// ParseGoSum parses go.sum strictly: three fields per line, a valid module
// version, and an h1 hash that decodes to 32 bytes.
func ParseGoSum(file string, data []byte) ([]GoSumLine, error) {
	var out []GoSumLine
	for idx, raw := range strings.Split(string(data), "\n") {
		num := idx + 1
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 3 {
			return nil, perr(file, num, "go.sum: expected 3 fields, got %d", len(f))
		}
		e := GoSumLine{Path: f[0], Version: f[1], Hash: f[2]}
		if v, ok := strings.CutSuffix(e.Version, "/go.mod"); ok {
			e.Version, e.GoMod = v, true
		}
		if err := checkGoModuleVersion(e.Path, e.Version); err != nil {
			return nil, perr(file, num, "go.sum: %v", err)
		}
		h, ok := strings.CutPrefix(e.Hash, "h1:")
		if !ok {
			return nil, perr(file, num, "go.sum: unsupported hash algorithm in %q", e.Hash)
		}
		b, err := base64.StdEncoding.DecodeString(h)
		if err != nil || len(b) != 32 {
			return nil, perr(file, num, "go.sum: malformed h1 hash %q", e.Hash)
		}
		out = append(out, e)
	}
	return out, nil
}

// GoGraph builds a dependency graph from go.mod and (optionally) go.sum.
// Requirements without "// indirect" are direct dependencies; replace
// directives are applied; module zip hashes from go.sum are attached
// (the h1 dirhash is a SHA-256 over the module's file list).
func GoGraph(modFile string, mod *GoMod, sum []GoSumLine) *Graph {
	g := NewGraph()
	hashes := map[GoModuleVersion]string{}
	for _, s := range sum {
		if !s.GoMod {
			b, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(s.Hash, "h1:"))
			hashes[GoModuleVersion{s.Path, s.Version}] = hex.EncodeToString(b)
		}
	}
	excluded := map[GoModuleVersion]bool{}
	for _, e := range mod.Exclude {
		excluded[e] = true
	}
	for _, r := range mod.Require {
		if excluded[GoModuleVersion{r.Path, r.Version}] {
			continue
		}
		p := &Package{Ecosystem: EcosystemGo, Name: r.Path, Version: r.Version, Direct: !r.Indirect, Sources: []string{modFile}}
		if rep, ok := findGoReplace(mod.Replace, r.Path, r.Version); ok {
			if rep.IsLocal() {
				p.Local = true
				p.Resolved = rep.New
			} else {
				p.Name, p.Version = rep.New, rep.NewVersion
			}
		}
		if h, ok := hashes[GoModuleVersion{p.Name, p.Version}]; ok {
			p.AddHash(Hash{Alg: HashSHA256, Hex: h})
		}
		g.Add(p)
	}
	return g
}

func findGoReplace(reps []GoReplace, path, version string) (GoReplace, bool) {
	var wildcard *GoReplace
	for i := range reps {
		r := reps[i]
		if r.Old != path {
			continue
		}
		if r.OldVersion == version {
			return r, true
		}
		if r.OldVersion == "" {
			wildcard = &reps[i]
		}
	}
	if wildcard != nil {
		return *wildcard, true
	}
	return GoReplace{}, false
}
