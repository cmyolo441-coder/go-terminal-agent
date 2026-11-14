package supplychain

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// PURL is a package URL as defined by the package-url specification
// (ECMA-427): pkg:type/namespace/name@version?qualifiers#subpath.
type PURL struct {
	Type       string
	Namespace  string
	Name       string
	Version    string
	Qualifiers map[string]string
	Subpath    string
}

// PURLFor builds the canonical purl for a package of the given ecosystem,
// applying the type-specific normalisation rules of the specification.
func PURLFor(eco Ecosystem, name, version string, qualifiers map[string]string) PURL {
	p := PURL{Type: eco.PURLType(), Version: version}
	if len(qualifiers) > 0 {
		p.Qualifiers = make(map[string]string, len(qualifiers))
		for k, v := range qualifiers {
			p.Qualifiers[k] = v
		}
	}
	switch eco {
	case EcosystemPyPI:
		p.Name = NormalizePyPIName(name)
	case EcosystemNPM:
		if strings.HasPrefix(name, "@") {
			if i := strings.IndexByte(name, '/'); i > 0 {
				p.Namespace, p.Name = name[:i], name[i+1:]
				break
			}
		}
		p.Name = name
	case EcosystemGo:
		if i := strings.LastIndexByte(name, '/'); i > 0 {
			p.Namespace, p.Name = name[:i], name[i+1:]
		} else {
			p.Name = name
		}
	case EcosystemPackagist:
		lower := strings.ToLower(name)
		if i := strings.IndexByte(lower, '/'); i > 0 {
			p.Namespace, p.Name = lower[:i], lower[i+1:]
		} else {
			p.Name = lower
		}
	default:
		p.Name = name
	}
	return p
}

// String renders the canonical purl string. Qualifiers are sorted by key
// and empty qualifier values are dropped, as the specification requires.
func (p PURL) String() string {
	var b strings.Builder
	b.WriteString("pkg:")
	b.WriteString(strings.ToLower(p.Type))
	b.WriteByte('/')
	if p.Namespace != "" {
		for _, seg := range strings.Split(strings.Trim(p.Namespace, "/"), "/") {
			if seg == "" {
				continue
			}
			b.WriteString(purlEscape(seg))
			b.WriteByte('/')
		}
	}
	b.WriteString(purlEscape(p.Name))
	if p.Version != "" {
		b.WriteByte('@')
		b.WriteString(purlEscape(p.Version))
	}
	if len(p.Qualifiers) > 0 {
		keys := make([]string, 0, len(p.Qualifiers))
		for k, v := range p.Qualifiers {
			if v != "" {
				keys = append(keys, k)
			}
		}
		sort.Slice(keys, func(i, j int) bool { return strings.ToLower(keys[i]) < strings.ToLower(keys[j]) })
		for i, k := range keys {
			if i == 0 {
				b.WriteByte('?')
			} else {
				b.WriteByte('&')
			}
			b.WriteString(strings.ToLower(k))
			b.WriteByte('=')
			b.WriteString(purlEscape(p.Qualifiers[k]))
		}
	}
	if sp := strings.Trim(p.Subpath, "/"); sp != "" {
		b.WriteByte('#')
		first := true
		for _, seg := range strings.Split(sp, "/") {
			if seg == "" || seg == "." || seg == ".." {
				continue
			}
			if !first {
				b.WriteByte('/')
			}
			first = false
			b.WriteString(purlEscape(seg))
		}
	}
	return b.String()
}

// purlEscape percent-encodes everything except the unreserved characters
// and ':' (which the specification says must not be encoded).
func purlEscape(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '.' || c == '-' || c == '_' || c == '~' || c == ':' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&15])
	}
	return b.String()
}

func purlUnescape(s string) (string, error) {
	if !strings.Contains(s, "%") {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			b.WriteByte(s[i])
			continue
		}
		if i+2 >= len(s) {
			return "", fmt.Errorf("purl: truncated percent escape in %q", s)
		}
		hi, ok1 := unhex(s[i+1])
		lo, ok2 := unhex(s[i+2])
		if !ok1 || !ok2 {
			return "", fmt.Errorf("purl: invalid percent escape in %q", s)
		}
		b.WriteByte(hi<<4 | lo)
		i += 2
	}
	return b.String(), nil
}

func unhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// ParsePURL parses a purl string following the specification's parsing
// algorithm (right-to-left split of subpath, qualifiers and version).
func ParsePURL(s string) (PURL, error) {
	var p PURL
	rest := s
	if !strings.HasPrefix(strings.ToLower(rest), "pkg:") {
		return p, errors.New("purl: missing pkg: scheme")
	}
	rest = rest[4:]
	if i := strings.LastIndexByte(rest, '#'); i >= 0 {
		var segs []string
		for _, seg := range strings.Split(strings.Trim(rest[i+1:], "/"), "/") {
			if seg == "" || seg == "." || seg == ".." {
				continue
			}
			u, err := purlUnescape(seg)
			if err != nil {
				return p, err
			}
			segs = append(segs, u)
		}
		p.Subpath = strings.Join(segs, "/")
		rest = rest[:i]
	}
	if i := strings.LastIndexByte(rest, '?'); i >= 0 {
		q := rest[i+1:]
		rest = rest[:i]
		for _, kv := range strings.Split(q, "&") {
			if kv == "" {
				continue
			}
			k, v, ok := strings.Cut(kv, "=")
			if !ok || k == "" {
				return p, fmt.Errorf("purl: malformed qualifier %q", kv)
			}
			if !validQualifierKey(k) {
				return p, fmt.Errorf("purl: invalid qualifier key %q", k)
			}
			uv, err := purlUnescape(v)
			if err != nil {
				return p, err
			}
			if uv == "" {
				continue
			}
			if p.Qualifiers == nil {
				p.Qualifiers = map[string]string{}
			}
			k = strings.ToLower(k)
			if _, dup := p.Qualifiers[k]; dup {
				return p, fmt.Errorf("purl: duplicate qualifier %q", k)
			}
			p.Qualifiers[k] = uv
		}
	}
	rest = strings.TrimLeft(rest, "/")
	typ, path, ok := strings.Cut(rest, "/")
	if !ok || typ == "" {
		return p, errors.New("purl: missing type or name")
	}
	if !validPURLType(typ) {
		return p, fmt.Errorf("purl: invalid type %q", typ)
	}
	p.Type = strings.ToLower(typ)
	if i := strings.LastIndexByte(path, '@'); i >= 0 {
		v, err := purlUnescape(path[i+1:])
		if err != nil {
			return p, err
		}
		if v == "" {
			return p, errors.New("purl: empty version after '@'")
		}
		p.Version = v
		path = path[:i]
	}
	path = strings.Trim(path, "/")
	if path == "" {
		return p, errors.New("purl: missing name")
	}
	segs := strings.Split(path, "/")
	name, err := purlUnescape(segs[len(segs)-1])
	if err != nil {
		return p, err
	}
	if name == "" {
		return p, errors.New("purl: empty name")
	}
	p.Name = name
	var ns []string
	for _, seg := range segs[:len(segs)-1] {
		if seg == "" {
			continue
		}
		u, err := purlUnescape(seg)
		if err != nil {
			return p, err
		}
		if strings.Contains(u, "/") {
			return p, errors.New("purl: namespace segment contains '/'")
		}
		ns = append(ns, u)
	}
	p.Namespace = strings.Join(ns, "/")
	return p, nil
}

func validPURLType(t string) bool {
	for i := 0; i < len(t); i++ {
		c := t[i]
		alpha := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		if i == 0 && !alpha {
			return false
		}
		if !alpha && !(c >= '0' && c <= '9') && c != '.' && c != '+' && c != '-' {
			return false
		}
	}
	return t != ""
}

func validQualifierKey(k string) bool {
	for i := 0; i < len(k); i++ {
		c := k[i]
		alpha := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		if i == 0 && !alpha {
			return false
		}
		if !alpha && !(c >= '0' && c <= '9') && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// NormalizePyPIName applies PEP 503 normalisation: lower case, with runs
// of '-', '_' and '.' collapsed to a single '-'.
func NormalizePyPIName(name string) string {
	var b strings.Builder
	sep := false
	for _, r := range strings.TrimSpace(name) {
		if r == '-' || r == '_' || r == '.' {
			sep = true
			continue
		}
		if sep && b.Len() > 0 {
			b.WriteByte('-')
		}
		sep = false
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}
