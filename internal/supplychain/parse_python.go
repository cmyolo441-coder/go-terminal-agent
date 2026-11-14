package supplychain

import (
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

// Requirement is one requirement line of a pip requirements file.
type Requirement struct {
	Name      string
	Extras    []string
	Specifier string
	Marker    string
	URL       string
	Hashes    []Hash
	Editable  bool
	Line      int
}

// PinnedVersion returns the exact version for "==X" / "===X" pins.
func (r Requirement) PinnedVersion() (string, bool) {
	s := strings.TrimSpace(r.Specifier)
	if strings.Contains(s, ",") {
		return "", false
	}
	for _, op := range []string{"===", "=="} {
		if v, ok := strings.CutPrefix(s, op); ok {
			v = strings.TrimSpace(v)
			if v == "" || strings.HasSuffix(v, ".*") {
				return "", false
			}
			return v, true
		}
	}
	return "", false
}

// RequirementsFile is a parsed requirements.txt.
type RequirementsFile struct {
	Requirements []Requirement
	// Includes are -r files and Constraints are -c files, relative to the
	// including file's directory.
	Includes    []string
	Constraints []string
	IndexURLs   []string
}

var pep508NameRe = regexp.MustCompile(`^(?i)[a-z0-9](?:[a-z0-9._-]*[a-z0-9])?`)

var reqGlobalOptions = map[string]bool{
	"-i": true, "--index-url": true, "--extra-index-url": true, "-f": true, "--find-links": true,
	"--trusted-host": true, "--only-binary": true, "--no-binary": true, "--use-feature": true,
	"--progress-bar": true, "--platform": true,
}

var reqFlagOptions = map[string]bool{
	"--no-index": true, "--pre": true, "--prefer-binary": true, "--require-hashes": true,
}

// ParseRequirements parses a pip requirements file strictly: unknown
// options, malformed names, invalid specifiers and markers are errors.
func ParseRequirements(file string, data []byte) (*RequirementsFile, error) {
	rf := &RequirementsFile{}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		num := i + 1
		line := lines[i]
		for strings.HasSuffix(line, "\\") && i+1 < len(lines) {
			i++
			line = strings.TrimSuffix(line, "\\") + " " + lines[i]
		}
		line = strings.TrimSuffix(line, "\\")
		line = stripReqComment(line)
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "-") {
			if err := rf.option(line, file, num); err != nil {
				return nil, err
			}
			continue
		}
		req, err := parseRequirementLine(line, file, num)
		if err != nil {
			return nil, err
		}
		if req.Name == "" {
			continue // direct path/URL without a name: not resolvable
		}
		rf.Requirements = append(rf.Requirements, req)
	}
	return rf, nil
}

func stripReqComment(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t') {
			return s[:i]
		}
	}
	return s
}

func splitOption(line string) (opt, val string) {
	if k, v, ok := strings.Cut(line, "="); ok && !strings.ContainsAny(k, " \t") {
		return k, strings.TrimSpace(v)
	}
	f := strings.Fields(line)
	if len(f) == 1 {
		return f[0], ""
	}
	return f[0], strings.TrimSpace(line[len(f[0]):])
}

func (rf *RequirementsFile) option(line, file string, num int) error {
	opt, val := splitOption(line)
	switch {
	case opt == "-r" || opt == "--requirement":
		if val == "" {
			return perr(file, num, "requirements: %s needs a file", opt)
		}
		rf.Includes = append(rf.Includes, val)
	case opt == "-c" || opt == "--constraint":
		if val == "" {
			return perr(file, num, "requirements: %s needs a file", opt)
		}
		rf.Constraints = append(rf.Constraints, val)
	case opt == "-e" || opt == "--editable":
		if val == "" {
			return perr(file, num, "requirements: %s needs a target", opt)
		}
		req := Requirement{Editable: true, URL: val, Line: num}
		if i := strings.Index(val, "#egg="); i >= 0 {
			name := val[i+5:]
			if j := strings.IndexAny(name, "&[ "); j >= 0 {
				name = name[:j]
			}
			req.Name = name
		}
		if req.Name != "" {
			rf.Requirements = append(rf.Requirements, req)
		}
	case reqGlobalOptions[opt]:
		if val == "" {
			return perr(file, num, "requirements: %s needs a value", opt)
		}
		if opt == "-i" || opt == "--index-url" || opt == "--extra-index-url" {
			rf.IndexURLs = append(rf.IndexURLs, val)
		}
	case reqFlagOptions[opt]:
	default:
		return perr(file, num, "requirements: unknown option %q", opt)
	}
	return nil
}

func parseRequirementLine(line, file string, num int) (Requirement, error) {
	req := Requirement{Line: num}
	// per-requirement options start at the first whitespace-preceded "--"
	body, opts := line, ""
	for i := 1; i+1 < len(line); i++ {
		if line[i] == '-' && line[i+1] == '-' && (line[i-1] == ' ' || line[i-1] == '\t') {
			body, opts = strings.TrimSpace(line[:i]), line[i:]
			break
		}
	}
	for _, f := range strings.Fields(opts) {
		k, v, _ := strings.Cut(f, "=")
		switch k {
		case "--hash":
			alg, hx, ok := strings.Cut(v, ":")
			if !ok {
				return req, perr(file, num, "requirements: malformed --hash %q", v)
			}
			h, err := hashFromAlgHex(alg, hx)
			if err != nil {
				return req, perr(file, num, "requirements: %v", err)
			}
			req.Hashes = append(req.Hashes, h)
		case "--global-option", "--install-option", "--config-settings":
		default:
			if strings.HasPrefix(k, "--") {
				return req, perr(file, num, "requirements: unknown per-requirement option %q", k)
			}
		}
	}
	// unnamed path or URL requirements
	if strings.HasPrefix(body, ".") || strings.HasPrefix(body, "/") || strings.Contains(strings.SplitN(body, " ", 2)[0], "://") {
		base := path.Base(strings.SplitN(body, " ", 2)[0])
		if strings.HasSuffix(base, ".whl") {
			parts := strings.Split(strings.TrimSuffix(base, ".whl"), "-")
			if len(parts) >= 5 {
				req.Name, req.Specifier, req.URL = parts[0], "=="+parts[1], body
				return req, nil
			}
		}
		req.URL = body
		return req, nil
	}
	if spec, marker, ok := strings.Cut(body, ";"); ok {
		body = strings.TrimSpace(spec)
		req.Marker = strings.TrimSpace(marker)
		if req.Marker == "" {
			return req, perr(file, num, "requirements: empty environment marker")
		}
		if _, err := ParseMarker(req.Marker); err != nil {
			return req, perr(file, num, "requirements: %v", err)
		}
	}
	name := pep508NameRe.FindString(body)
	if name == "" {
		return req, perr(file, num, "requirements: invalid requirement %q", body)
	}
	req.Name = name
	rest := strings.TrimSpace(body[len(name):])
	if strings.HasPrefix(rest, "[") {
		end := strings.IndexByte(rest, ']')
		if end < 0 {
			return req, perr(file, num, "requirements: unterminated extras")
		}
		for _, e := range strings.Split(rest[1:end], ",") {
			e = strings.TrimSpace(e)
			if e == "" {
				continue
			}
			if pep508NameRe.FindString(e) != e {
				return req, perr(file, num, "requirements: invalid extra %q", e)
			}
			req.Extras = append(req.Extras, e)
		}
		rest = strings.TrimSpace(rest[end+1:])
	}
	if u, ok := strings.CutPrefix(rest, "@"); ok {
		req.URL = strings.TrimSpace(u)
		if req.URL == "" {
			return req, perr(file, num, "requirements: empty URL")
		}
		return req, nil
	}
	if strings.HasPrefix(rest, "(") && strings.HasSuffix(rest, ")") {
		rest = rest[1 : len(rest)-1]
	}
	rest = strings.TrimSpace(rest)
	if rest != "" {
		if _, err := ParsePEP440Specifiers(rest); err != nil {
			return req, perr(file, num, "requirements: %v", err)
		}
		req.Specifier = strings.Join(strings.Fields(rest), "")
	}
	return req, nil
}

func hashFromAlgHex(alg, hx string) (Hash, error) {
	var a HashAlg
	switch strings.ToLower(alg) {
	case "md5":
		a = HashMD5
	case "sha1":
		a = HashSHA1
	case "sha256":
		a = HashSHA256
	case "sha384":
		a = HashSHA384
	case "sha512":
		a = HashSHA512
	default:
		return Hash{}, fmt.Errorf("unsupported hash algorithm %q", alg)
	}
	b, err := hex.DecodeString(hx)
	if err != nil {
		return Hash{}, fmt.Errorf("malformed %s digest %q", alg, hx)
	}
	want := map[HashAlg]int{HashMD5: 16, HashSHA1: 20, HashSHA256: 32, HashSHA384: 48, HashSHA512: 64}[a]
	if len(b) != want {
		return Hash{}, fmt.Errorf("%s digest has %d bytes, want %d", alg, len(b), want)
	}
	return Hash{Alg: a, Hex: strings.ToLower(hx)}, nil
}

// RequirementsGraph converts parsed requirements into a graph. Every
// requirement is a direct dependency; unpinned ones are flagged.
func RequirementsGraph(file string, rf *RequirementsFile) *Graph {
	g := NewGraph()
	for _, r := range rf.Requirements {
		p := &Package{Ecosystem: EcosystemPyPI, Name: r.Name, Direct: true, Marker: r.Marker, Sources: []string{file}}
		if v, ok := r.PinnedVersion(); ok {
			p.Version = v
		} else {
			p.Unpinned = true
			p.Constraint = r.Specifier
			if r.URL != "" {
				p.Resolved = r.URL
				p.Local = r.Editable || !strings.Contains(r.URL, "://")
			}
		}
		for _, h := range r.Hashes {
			p.AddHash(h)
		}
		g.Add(p)
	}
	return g
}

// ---- PEP 508 environment markers -----------------------------------------

// Marker is a parsed PEP 508 environment marker.
type Marker struct{ root markerNode }

type markerNode interface {
	eval(env map[string]string) bool
}

type markerAnd struct{ l, r markerNode }
type markerOr struct{ l, r markerNode }
type markerCmp struct {
	lhs, op, rhs   string
	lhsVar, rhsVar bool
}

func (m markerAnd) eval(env map[string]string) bool { return m.l.eval(env) && m.r.eval(env) }
func (m markerOr) eval(env map[string]string) bool  { return m.l.eval(env) || m.r.eval(env) }

var markerVars = map[string]bool{
	"python_version": true, "python_full_version": true, "os_name": true, "sys_platform": true,
	"platform_release": true, "platform_system": true, "platform_version": true,
	"platform_machine": true, "platform_python_implementation": true,
	"implementation_name": true, "implementation_version": true, "extra": true,
	"dependency_groups": true, "extras": true,
	// legacy (PEP 345) spellings still accepted by pip
	"os.name": true, "sys.platform": true, "platform.version": true, "platform.machine": true,
	"platform.python_implementation": true, "python_implementation": true,
}

var versionMarkerVars = map[string]bool{"python_version": true, "python_full_version": true, "implementation_version": true}

func (m markerCmp) eval(env map[string]string) bool {
	l, r := m.lhs, m.rhs
	if m.lhsVar {
		l = env[m.lhs]
	}
	if m.rhsVar {
		r = env[m.rhs]
	}
	switch m.op {
	case "in":
		return strings.Contains(r, l)
	case "not in":
		return !strings.Contains(r, l)
	}
	varName := m.lhs
	if !m.lhsVar {
		varName = m.rhs
	}
	if versionMarkerVars[varName] && m.lhsVar && !m.rhsVar {
		if set, err := ParsePEP440Specifiers(m.op + r); err == nil {
			if v, err := ParsePEP440(l); err == nil {
				return set.Contains(v, true)
			}
		}
	}
	if varName == "extra" {
		l, r = NormalizePyPIName(l), NormalizePyPIName(r)
	}
	switch m.op {
	case "==", "===":
		return l == r
	case "!=":
		return l != r
	case "<":
		return l < r
	case "<=":
		return l <= r
	case ">":
		return l > r
	case ">=":
		return l >= r
	case "~=":
		return false
	}
	return false
}

// ParseMarker parses a PEP 508 marker expression.
func ParseMarker(s string) (*Marker, error) {
	toks, err := tokenizeMarker(s)
	if err != nil {
		return nil, err
	}
	p := &markerParser{toks: toks}
	n, err := p.or()
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.toks) {
		return nil, fmt.Errorf("marker: unexpected %q", p.toks[p.pos])
	}
	return &Marker{root: n}, nil
}

// Evaluate evaluates the marker against an environment.
func (m *Marker) Evaluate(env map[string]string) bool { return m.root.eval(env) }

func tokenizeMarker(s string) ([]string, error) {
	var toks []string
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t':
			i++
		case c == '(' || c == ')':
			toks = append(toks, string(c))
			i++
		case c == '"' || c == '\'':
			j := strings.IndexByte(s[i+1:], c)
			if j < 0 {
				return nil, fmt.Errorf("marker: unterminated string in %q", s)
			}
			toks = append(toks, s[i:i+j+2])
			i += j + 2
		case strings.ContainsRune("<>=!~", rune(c)):
			j := i
			for j < len(s) && strings.ContainsRune("<>=!~", rune(s[j])) {
				j++
			}
			op := s[i:j]
			switch op {
			case "<", "<=", ">", ">=", "==", "!=", "~=", "===":
			default:
				return nil, fmt.Errorf("marker: invalid operator %q", op)
			}
			toks = append(toks, op)
			i = j
		default:
			j := i
			for j < len(s) && (s[j] == '_' || s[j] == '.' || s[j] >= 'a' && s[j] <= 'z' || s[j] >= 'A' && s[j] <= 'Z' || s[j] >= '0' && s[j] <= '9') {
				j++
			}
			if j == i {
				return nil, fmt.Errorf("marker: unexpected character %q", c)
			}
			toks = append(toks, s[i:j])
			i = j
		}
	}
	return toks, nil
}

type markerParser struct {
	toks  []string
	pos   int
	depth int
}

func (p *markerParser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return ""
}

func (p *markerParser) or() (markerNode, error) {
	l, err := p.and()
	if err != nil {
		return nil, err
	}
	for p.peek() == "or" {
		p.pos++
		r, err := p.and()
		if err != nil {
			return nil, err
		}
		l = markerOr{l, r}
	}
	return l, nil
}

func (p *markerParser) and() (markerNode, error) {
	l, err := p.atom()
	if err != nil {
		return nil, err
	}
	for p.peek() == "and" {
		p.pos++
		r, err := p.atom()
		if err != nil {
			return nil, err
		}
		l = markerAnd{l, r}
	}
	return l, nil
}

func (p *markerParser) atom() (markerNode, error) {
	if p.peek() == "(" {
		p.pos++
		p.depth++
		if p.depth > 32 {
			return nil, fmt.Errorf("marker: nesting too deep")
		}
		n, err := p.or()
		if err != nil {
			return nil, err
		}
		if p.peek() != ")" {
			return nil, fmt.Errorf("marker: expected ')'")
		}
		p.pos++
		p.depth--
		return n, nil
	}
	lhs, lv, err := p.value()
	if err != nil {
		return nil, err
	}
	op := p.peek()
	switch op {
	case "<", "<=", ">", ">=", "==", "!=", "~=", "===", "in":
		p.pos++
	case "not":
		p.pos++
		if p.peek() != "in" {
			return nil, fmt.Errorf("marker: expected 'in' after 'not'")
		}
		p.pos++
		op = "not in"
	default:
		return nil, fmt.Errorf("marker: expected comparison operator, got %q", op)
	}
	rhs, rv, err := p.value()
	if err != nil {
		return nil, err
	}
	if !lv && !rv {
		return nil, fmt.Errorf("marker: comparison needs a variable")
	}
	return markerCmp{lhs: lhs, op: op, rhs: rhs, lhsVar: lv, rhsVar: rv}, nil
}

func (p *markerParser) value() (string, bool, error) {
	t := p.peek()
	if t == "" {
		return "", false, fmt.Errorf("marker: unexpected end")
	}
	p.pos++
	if t[0] == '"' || t[0] == '\'' {
		return t[1 : len(t)-1], false, nil
	}
	if !markerVars[t] {
		return "", false, fmt.Errorf("marker: unknown variable %q", t)
	}
	return t, true, nil
}

// ---- poetry.lock ---------------------------------------------------------

// ParsePoetryLock parses poetry.lock (lock-version 1.x and 2.x). pyproject
// is the sibling pyproject.toml (may be nil) used to find direct deps.
func ParsePoetryLock(file string, data, pyproject []byte) (*Graph, error) {
	doc, err := parseTOML(file, data)
	if err != nil {
		return nil, err
	}
	meta := tomlTableOf(doc, "metadata")
	if lv := tomlString(meta, "lock-version"); lv != "" && !strings.HasPrefix(lv, "1.") && !strings.HasPrefix(lv, "2.") {
		return nil, perr(file, 0, "poetry.lock: unsupported lock-version %q", lv)
	}
	legacyFiles := tomlTableOf(meta, "files")
	g := NewGraph()
	byName := map[string][]string{}
	type depRef struct{ from, name string }
	var deps []depRef
	for _, pt := range tomlTables(doc, "package") {
		name, version := tomlString(pt, "name"), tomlString(pt, "version")
		if name == "" || version == "" {
			return nil, perr(file, 0, "poetry.lock: package without name or version")
		}
		if _, err := ParsePEP440(version); err != nil && tomlTableOf(pt, "source") == nil {
			return nil, perr(file, 0, "poetry.lock: package %s: %v", name, err)
		}
		p := &Package{Ecosystem: EcosystemPyPI, Name: name, Version: version, Sources: []string{file}}
		if b, ok := pt["optional"].(bool); ok {
			p.Optional = b
		}
		if tomlString(pt, "category") == "dev" {
			p.Dev = true
		}
		if src := tomlTableOf(pt, "source"); src != nil {
			switch tomlString(src, "type") {
			case "directory", "file":
				p.Local = true
			case "git", "url":
				p.Resolved = tomlString(src, "url")
			}
		}
		files, _ := pt["files"].([]any)
		if files == nil {
			files, _ = legacyFiles[name].([]any)
		}
		for _, f := range files {
			ft, ok := f.(tomlTable)
			if !ok {
				continue
			}
			if alg, hx, ok := strings.Cut(tomlString(ft, "hash"), ":"); ok {
				h, err := hashFromAlgHex(alg, hx)
				if err != nil {
					return nil, perr(file, 0, "poetry.lock: package %s: %v", name, err)
				}
				p.AddHash(h)
			}
		}
		id := g.Add(p).ID()
		norm := NormalizePyPIName(name)
		byName[norm] = append(byName[norm], id)
		for _, dn := range tomlKeys(tomlTableOf(pt, "dependencies")) {
			deps = append(deps, depRef{id, NormalizePyPIName(dn)})
		}
	}
	required := map[string]bool{}
	for _, d := range deps {
		for _, to := range byName[d.name] {
			required[to] = true
			if err := g.AddEdge(d.from, to); err != nil {
				return nil, err
			}
		}
	}
	if pyproject != nil {
		direct, dev, err := pyprojectDirect(file, pyproject)
		if err != nil {
			return nil, err
		}
		for name := range direct {
			for _, id := range byName[name] {
				g.Package(id).Direct = true
			}
		}
		var prodRoots, devRoots []string
		for name := range direct {
			for _, id := range byName[name] {
				if dev[name] {
					devRoots = append(devRoots, id)
				} else {
					prodRoots = append(prodRoots, id)
				}
			}
		}
		if len(devRoots) > 0 {
			prod := reachable(g, prodRoots)
			for id := range reachable(g, devRoots) {
				if !prod[id] {
					g.Package(id).Dev = true
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

// pyprojectDirect returns normalised names of direct dependencies declared
// in pyproject.toml (Poetry tables and PEP 621), and which of them are dev.
func pyprojectDirect(file string, data []byte) (direct, dev map[string]bool, err error) {
	doc, err := parseTOML(file, data)
	if err != nil {
		return nil, nil, err
	}
	direct, dev = map[string]bool{}, map[string]bool{}
	add := func(name string, isDev bool) {
		n := NormalizePyPIName(name)
		if n == "python" || n == "" {
			return
		}
		if !direct[n] {
			dev[n] = isDev
		} else if !isDev {
			dev[n] = false
		}
		direct[n] = true
	}
	poetry := tomlTableOf(tomlTableOf(doc, "tool"), "poetry")
	for _, k := range tomlKeys(tomlTableOf(poetry, "dependencies")) {
		add(k, false)
	}
	for _, k := range tomlKeys(tomlTableOf(poetry, "dev-dependencies")) {
		add(k, true)
	}
	groups := tomlTableOf(poetry, "group")
	for _, gname := range tomlKeys(groups) {
		for _, k := range tomlKeys(tomlTableOf(tomlTableOf(groups, gname), "dependencies")) {
			add(k, gname != "main")
		}
	}
	project := tomlTableOf(doc, "project")
	addReqs := func(v any, isDev bool) {
		arr, _ := v.([]any)
		for _, e := range arr {
			if s, ok := e.(string); ok {
				if n := pep508NameRe.FindString(strings.TrimSpace(s)); n != "" {
					add(n, isDev)
				}
			}
		}
	}
	addReqs(project["dependencies"], false)
	opt := tomlTableOf(project, "optional-dependencies")
	for _, k := range tomlKeys(opt) {
		addReqs(opt[k], false)
	}
	dg := tomlTableOf(doc, "dependency-groups")
	for _, k := range tomlKeys(dg) {
		addReqs(dg[k], true)
	}
	return direct, dev, nil
}

// ---- Pipfile.lock --------------------------------------------------------

type pipfileLockEntry struct {
	Version  string   `json:"version"`
	Hashes   []string `json:"hashes"`
	Markers  string   `json:"markers"`
	Index    string   `json:"index"`
	Git      string   `json:"git"`
	Ref      string   `json:"ref"`
	Path     string   `json:"path"`
	File     string   `json:"file"`
	Editable bool     `json:"editable"`
	Extras   []string `json:"extras"`
}

type pipfileLock struct {
	Meta struct {
		PipfileSpec int `json:"pipfile-spec"`
	} `json:"_meta"`
	Default map[string]pipfileLockEntry `json:"default"`
	Develop map[string]pipfileLockEntry `json:"develop"`
}

// ParsePipfileLock parses Pipfile.lock. pipfile (may be nil) is the
// sibling Pipfile used to identify direct dependencies.
func ParsePipfileLock(file string, data, pipfile []byte) (*Graph, error) {
	var lf pipfileLock
	if err := strictJSON(data, &lf); err != nil {
		return nil, perr(file, 0, "Pipfile.lock: %v", err)
	}
	if lf.Meta.PipfileSpec != 0 && lf.Meta.PipfileSpec != 6 {
		return nil, perr(file, 0, "Pipfile.lock: unsupported pipfile-spec %d", lf.Meta.PipfileSpec)
	}
	directSet := map[string]bool{}
	if pipfile != nil {
		doc, err := parseTOML(file, pipfile)
		if err != nil {
			return nil, err
		}
		for _, sec := range []string{"packages", "dev-packages"} {
			for _, k := range tomlKeys(tomlTableOf(doc, sec)) {
				directSet[NormalizePyPIName(k)] = true
			}
		}
	}
	g := NewGraph()
	add := func(entries map[string]pipfileLockEntry, dev bool) error {
		names := make([]string, 0, len(entries))
		for n := range entries {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, name := range names {
			e := entries[name]
			p := &Package{Ecosystem: EcosystemPyPI, Name: name, Dev: dev, Marker: e.Markers,
				Direct: directSet[NormalizePyPIName(name)], Sources: []string{file}}
			switch {
			case strings.HasPrefix(e.Version, "=="):
				p.Version = strings.TrimPrefix(e.Version, "==")
				if _, err := ParsePEP440(p.Version); err != nil {
					return perr(file, 0, "Pipfile.lock: %s: %v", name, err)
				}
			case e.Path != "" || e.File != "":
				p.Local, p.Unpinned = true, true
			case e.Git != "":
				p.Unpinned, p.Resolved = true, e.Git
			default:
				p.Unpinned, p.Constraint = true, e.Version
			}
			for _, h := range e.Hashes {
				alg, hx, ok := strings.Cut(h, ":")
				if !ok {
					return perr(file, 0, "Pipfile.lock: %s: malformed hash %q", name, h)
				}
				hh, err := hashFromAlgHex(alg, hx)
				if err != nil {
					return perr(file, 0, "Pipfile.lock: %s: %v", name, err)
				}
				p.AddHash(hh)
			}
			g.Add(p)
		}
		return nil
	}
	if err := add(lf.Default, false); err != nil {
		return nil, err
	}
	if err := add(lf.Develop, true); err != nil {
		return nil, err
	}
	return g, nil
}
