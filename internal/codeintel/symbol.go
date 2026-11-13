package codeintel

import (
	"sort"
	"strconv"
	"strings"
)

// SymbolKind classifies a definition.
type SymbolKind string

// Symbol kinds produced by the extractors.
const (
	KindFunction  SymbolKind = "function"
	KindMethod    SymbolKind = "method"
	KindType      SymbolKind = "type"
	KindStruct    SymbolKind = "struct"
	KindInterface SymbolKind = "interface"
	KindClass     SymbolKind = "class"
	KindEnum      SymbolKind = "enum"
	KindTrait     SymbolKind = "trait"
	KindConst     SymbolKind = "const"
	KindVar       SymbolKind = "var"
	KindModule    SymbolKind = "module"
	KindMacro     SymbolKind = "macro"
	KindField     SymbolKind = "field"
	KindSection   SymbolKind = "section"
)

// AllKinds lists every symbol kind, for validation and tool schemas.
var AllKinds = []SymbolKind{
	KindFunction, KindMethod, KindType, KindStruct, KindInterface, KindClass, KindEnum,
	KindTrait, KindConst, KindVar, KindModule, KindMacro, KindField, KindSection,
}

// ParseKind validates a kind name. It accepts a few common aliases.
func ParseKind(s string) (SymbolKind, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "func", "fn", "def", "functions":
		return KindFunction, true
	case "methods":
		return KindMethod, true
	case "constant", "constants":
		return KindConst, true
	case "variable", "variables", "let":
		return KindVar, true
	case "namespace", "package", "mod":
		return KindModule, true
	case "classes":
		return KindClass, true
	case "types", "typedef", "alias":
		return KindType, true
	}
	for _, k := range AllKinds {
		if string(k) == s {
			return k, true
		}
	}
	return "", false
}

// isTypeLike reports whether k names a type definition.
func (k SymbolKind) isTypeLike() bool {
	switch k {
	case KindType, KindStruct, KindInterface, KindClass, KindEnum, KindTrait:
		return true
	}
	return false
}

// isCallable reports whether k is a function or method.
func (k SymbolKind) isCallable() bool { return k == KindFunction || k == KindMethod }

// Symbol is one definition found in a file.
type Symbol struct {
	Name string     `json:"name"`
	Kind SymbolKind `json:"kind"`
	// Container is the receiver type, class, impl target or module that
	// holds the symbol, if any.
	Container string `json:"container,omitempty"`
	// Line and EndLine are the 1-based lines the definition spans.
	Line    int `json:"line"`
	EndLine int `json:"end_line"`
	// DocLine is the first line of the doc comment or decorators above the
	// definition; it equals Line when there are none.
	DocLine   int    `json:"doc_line"`
	Signature string `json:"signature,omitempty"`
	Doc       string `json:"doc,omitempty"`
	Exported  bool   `json:"exported,omitempty"`
}

// QualifiedName is Container.Name, or Name without a container.
func (s Symbol) QualifiedName() string {
	if s.Container != "" {
		return s.Container + "." + s.Name
	}
	return s.Name
}

// DocSummary is the first sentence or line of the doc comment.
func (s Symbol) DocSummary() string {
	d := strings.TrimSpace(s.Doc)
	if d == "" {
		return ""
	}
	if i := strings.IndexByte(d, '\n'); i >= 0 {
		d = d[:i]
	}
	if i := strings.Index(d, ". "); i >= 0 {
		d = d[:i+1]
	}
	return clip(d, 160)
}

// normalizeSymbols sorts symbols, clamps their ranges and drops
// declarations duplicated by a definition in the same file (C prototypes,
// TypeScript overloads).
func normalizeSymbols(syms []Symbol, lines int) []Symbol {
	if lines < 1 {
		lines = 1
	}
	for i := range syms {
		s := &syms[i]
		if s.Line < 1 {
			s.Line = 1
		}
		if s.Line > lines {
			s.Line = lines
		}
		if s.EndLine < s.Line {
			s.EndLine = s.Line
		}
		if s.EndLine > lines {
			s.EndLine = lines
		}
		if s.DocLine < 1 || s.DocLine > s.Line {
			s.DocLine = s.Line
		}
		s.Signature = clip(collapseSpace(s.Signature), 240)
		s.Doc = clip(strings.TrimSpace(s.Doc), 1200)
	}
	type key struct {
		name, container string
		callable        bool
	}
	hasBody := map[key]bool{}
	for _, s := range syms {
		if s.EndLine > s.Line {
			hasBody[key{s.Name, s.Container, s.Kind.isCallable()}] = true
		}
	}
	out := syms[:0]
	seen := map[[3]string]bool{}
	for _, s := range syms {
		k := key{s.Name, s.Container, s.Kind.isCallable()}
		if s.EndLine == s.Line && s.Kind.isCallable() && hasBody[k] {
			continue
		}
		id := [3]string{s.Name, s.Container, string(s.Kind) + ":" + strconv.Itoa(s.Line)}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		return out[i].EndLine > out[j].EndLine
	})
	return out
}

func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// clip shortens s to at most n bytes on a rune boundary, adding an ellipsis.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && cut < len(s) && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}

// ExtractSymbols returns the definitions in src, a file of language lang.
// Go is parsed exactly; other languages use a structural pattern matcher.
func ExtractSymbols(lang, src string) []Symbol {
	lines := strings.Count(src, "\n") + 1
	var syms []Symbol
	switch lang {
	case LangGo:
		var ok bool
		syms, ok = extractGo(src)
		if !ok {
			syms = extractGoFallback(src)
		}
	case LangMarkdown:
		syms = extractMarkdown(src)
	default:
		if spec, ok := langPatterns[lang]; ok {
			syms = extractPatterns(lang, spec, src)
		}
	}
	return normalizeSymbols(syms, lines)
}
