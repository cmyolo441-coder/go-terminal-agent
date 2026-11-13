package codeintel

import (
	"regexp"
	"sort"
	"strings"
)

// scopeReq says where a pattern may match.
type scopeReq int

const (
	// scopeAny: anywhere except inside a function or method body.
	scopeAny scopeReq = iota
	// scopeMember: a direct member of a class-like container.
	scopeMember
)

// blockStyle says how the extent of a definition is found.
type blockStyle int

const (
	blockBraces blockStyle = iota
	blockIndent
	blockRubyEnd
)

// pattern recognises one kind of definition on a masked source line.
type pattern struct {
	re   *regexp.Regexp
	kind SymbolKind
	// kindFrom maps the text of the "kind" group to a symbol kind.
	kindFrom map[string]SymbolKind
	scope    scopeReq
	// container: members found inside become methods of this symbol.
	container bool
	// noEmit: the pattern only opens a scope (Rust impl, extern "C").
	noEmit bool
	// qualified: the name may be Container::name.
	qualified bool
	// atBodyDepth: the line must sit directly in the file, a module or a
	// container body, not in some nested block.
	atBodyDepth bool
	// ctorOnly: the name must equal the enclosing container's name.
	ctorOnly bool
	// skipIf rejects lines that match the pattern but are not definitions.
	skipIf *regexp.Regexp
	// ignoreScope: the pattern is anchored at column 0 and always names a
	// top-level definition, even when broken braces confuse the scopes.
	ignoreScope bool
}

// langPattern is the extractor configuration for one language.
type langPattern struct {
	block     blockStyle
	patterns  []pattern
	reject    map[string]bool
	decorator func(trimmed string) bool
	exported  func(masked, name string) bool
	docstring bool
}

func rx(s string) *regexp.Regexp { return regexp.MustCompile(s) }

var jsReject = setOf("if", "for", "while", "switch", "catch", "return", "function", "typeof",
	"new", "else", "do", "try", "with", "super", "await", "yield", "throw", "delete", "void",
	"case", "import", "export", "constructor_", "require")

var cReject = setOf("if", "for", "while", "switch", "return", "sizeof", "else", "case", "do",
	"goto", "defined", "alignof", "decltype", "static_assert", "typeof", "__attribute__",
	"catch", "throw", "new", "delete")

var javaReject = setOf("if", "for", "while", "switch", "catch", "return", "new", "throw",
	"else", "synchronized", "try", "do", "super", "this", "case", "assert")

func atDecorator(t string) bool {
	return strings.HasPrefix(t, "@") && !strings.HasPrefix(t, "@interface")
}

var langPatterns = map[string]*langPattern{
	LangPython: {
		block:     blockIndent,
		docstring: true,
		decorator: atDecorator,
		exported:  func(_, name string) bool { return !strings.HasPrefix(name, "_") || isDunder(name) },
		patterns: []pattern{
			{re: rx(`^\s*(?:async\s+)?def\s+(?P<name>[A-Za-z_]\w*)\s*[\(\[]`), kind: KindFunction},
			{re: rx(`^\s*class\s+(?P<name>[A-Za-z_]\w*)`), kind: KindClass, container: true},
			{re: rx(`^type\s+(?P<name>[A-Za-z_]\w*)\s*(?:\[[^\]]*\])?\s*=`), kind: KindType},
			{re: rx(`^(?P<name>[A-Za-z_]\w*)\s*:\s*(?:typing\.)?TypeAlias\s*=`), kind: KindType},
			{re: rx(`^(?P<name>[A-Z][A-Z0-9_]*)\s*(?::[^=]*)?=[^=]`), kind: KindConst},
		},
	},
	LangJavaScript: jsPatterns(),
	LangTypeScript: jsPatterns(),
	LangRust: {
		block: blockBraces,
		decorator: func(t string) bool {
			return strings.HasPrefix(t, "#[") || strings.HasPrefix(t, "#![")
		},
		exported: func(m, _ string) bool { return strings.Contains(m, "pub ") || strings.Contains(m, "pub(") },
		patterns: []pattern{
			{re: rx(`^\s*(?:pub(?:\s*\([^)]*\))?\s+)?(?:default\s+)?(?:const\s+)?(?:async\s+)?(?:unsafe\s+)?(?:extern\s+(?:"[^"]*"\s+)?)?fn\s+(?P<name>[A-Za-z_]\w*)`), kind: KindFunction},
			{re: rx(`^\s*(?:pub(?:\s*\([^)]*\))?\s+)?struct\s+(?P<name>[A-Za-z_]\w*)`), kind: KindStruct},
			{re: rx(`^\s*(?:pub(?:\s*\([^)]*\))?\s+)?union\s+(?P<name>[A-Za-z_]\w*)`), kind: KindStruct},
			{re: rx(`^\s*(?:pub(?:\s*\([^)]*\))?\s+)?enum\s+(?P<name>[A-Za-z_]\w*)`), kind: KindEnum},
			{re: rx(`^\s*(?:pub(?:\s*\([^)]*\))?\s+)?(?:unsafe\s+)?(?:auto\s+)?trait\s+(?P<name>[A-Za-z_]\w*)`), kind: KindTrait, container: true},
			{re: rx(`^\s*(?:unsafe\s+)?impl\b(?:\s*<[^{]*?>)?\s+(?:[^{]*?\s+for\s+)?&?(?:\w+::)*(?P<name>[A-Za-z_]\w*)`), kind: KindStruct, container: true, noEmit: true},
			{re: rx(`^\s*(?:pub(?:\s*\([^)]*\))?\s+)?mod\s+(?P<name>[A-Za-z_]\w*)`), kind: KindModule, container: true},
			{re: rx(`^\s*(?:pub(?:\s*\([^)]*\))?\s+)?type\s+(?P<name>[A-Za-z_]\w*)`), kind: KindType},
			{re: rx(`^\s*(?:pub(?:\s*\([^)]*\))?\s+)?(?P<kind>const|static)\s+(?:mut\s+)?(?P<name>[A-Za-z_]\w*)\s*:`), kindFrom: map[string]SymbolKind{"const": KindConst, "static": KindVar}},
			{re: rx(`^\s*macro_rules!\s*(?P<name>[A-Za-z_]\w*)`), kind: KindMacro},
		},
	},
	LangJava: {
		block:     blockBraces,
		reject:    javaReject,
		decorator: atDecorator,
		exported:  func(m, _ string) bool { return strings.Contains(m, "public") },
		patterns: []pattern{
			{re: rx(`^\s*(?:@\w+(?:\([^)]*\))?\s+)*(?:(?:public|protected|private|static|final|abstract|sealed|non-sealed|strictfp)\s+)*(?P<kind>class|interface|enum|record|@interface)\s+(?P<name>[A-Za-z_$][\w$]*)`),
				kindFrom: map[string]SymbolKind{"class": KindClass, "interface": KindInterface, "enum": KindEnum, "record": KindClass, "@interface": KindInterface}, container: true},
			{re: rx(`^\s*(?:(?:public|protected|private)\s+)?(?:static\s+final|final\s+static)\s+[\w$.<>\[\], ?]+?\s+(?P<name>[A-Za-z_$][\w$]*)\s*[=;]`), kind: KindConst, scope: scopeMember},
			{re: rx(`^\s*(?:@\w+(?:\([^)]*\))?\s+)*(?:(?:public|protected|private|static|final|abstract|synchronized|native|default|strictfp)\s+)*(?:<[^>]+>\s+)?(?:[\w$.]+(?:<[^()]*>)?(?:\[\])*\s+)?(?P<name>[A-Za-z_$][\w$]*)\s*\(`),
				kind: KindFunction, scope: scopeMember, skipIf: rx(`^\s*[A-Z][A-Z0-9_]*\s*\(`)},
		},
	},
	LangC:   cPatterns(),
	LangCPP: cPatterns(),
	LangRuby: {
		block:    blockRubyEnd,
		exported: func(_, name string) bool { return !strings.HasPrefix(name, "_") },
		patterns: []pattern{
			{re: rx(`^\s*def\s+(?:self\.)?(?P<name>[A-Za-z_]\w*[?!=]?|\[\]=?|[+\-*/%<>=!~^&|]+)`), kind: KindFunction},
			{re: rx(`^\s*class\s+(?P<name>[A-Z]\w*(?:::[A-Z]\w*)*)`), kind: KindClass, container: true},
			{re: rx(`^\s*module\s+(?P<name>[A-Z]\w*(?:::[A-Z]\w*)*)`), kind: KindModule, container: true},
			{re: rx(`^\s*(?P<name>[A-Z][A-Z0-9_]*)\s*=[^=~]`), kind: KindConst},
		},
	},
	LangGo: {
		block:    blockBraces,
		exported: func(_, name string) bool { return name != "" && name[0] >= 'A' && name[0] <= 'Z' },
		patterns: []pattern{
			{re: rx(`^func\s+(?:\(\s*\w*\s*\*?\s*(?P<container>[A-Za-z_]\w*)[^)]*\)\s*)?(?P<name>[A-Za-z_]\w*)`), kind: KindFunction, ignoreScope: true},
			{re: rx(`^type\s+(?P<name>[A-Za-z_]\w*)(?:\[[^\]]*\])?\s+(?P<kind>struct|interface)`), kindFrom: map[string]SymbolKind{"struct": KindStruct, "interface": KindInterface}, ignoreScope: true},
			{re: rx(`^type\s+(?P<name>[A-Za-z_]\w*)`), kind: KindType, ignoreScope: true},
		},
	},
}

func isDunder(name string) bool {
	return len(name) > 4 && strings.HasPrefix(name, "__") && strings.HasSuffix(name, "__")
}

func jsPatterns() *langPattern {
	mods := `(?:(?:public|private|protected|static|readonly|abstract|override|async|declare|get|set|accessor)\s+)*`
	return &langPattern{
		block:     blockBraces,
		reject:    jsReject,
		decorator: atDecorator,
		exported: func(m, name string) bool {
			return strings.Contains(m, "export") ||
				(!strings.HasPrefix(name, "#") && !strings.HasPrefix(name, "_") && !strings.Contains(m, "private"))
		},
		patterns: []pattern{
			{re: rx(`^\s*(?:export\s+)?(?:default\s+)?(?:declare\s+)?(?:async\s+)?function\s*\*?\s*(?P<name>[A-Za-z_$][\w$]*)`), kind: KindFunction},
			{re: rx(`^\s*(?:export\s+)?(?:default\s+)?(?:declare\s+)?(?:abstract\s+)?class\s+(?P<name>[A-Za-z_$][\w$]*)`), kind: KindClass, container: true},
			{re: rx(`^\s*(?:export\s+)?(?:declare\s+)?interface\s+(?P<name>[A-Za-z_$][\w$]*)`), kind: KindInterface, container: true},
			{re: rx(`^\s*(?:export\s+)?(?:declare\s+)?type\s+(?P<name>[A-Za-z_$][\w$]*)\s*(?:<.*?>)?\s*=`), kind: KindType},
			{re: rx(`^\s*(?:export\s+)?(?:declare\s+)?(?:const\s+)?enum\s+(?P<name>[A-Za-z_$][\w$]*)`), kind: KindEnum},
			{re: rx(`^\s*(?:export\s+)?(?:declare\s+)?(?:namespace|module)\s+(?P<name>[A-Za-z_$][\w$.]*)\s*\{`), kind: KindModule, container: true},
			{re: rx(`^\s*(?:export\s+)?(?:default\s+)?(?:const|let|var)\s+(?P<name>[A-Za-z_$][\w$]*)\s*(?::[^=]+)?=\s*(?:async\s+)?(?:function\b|\([^)]*\)\s*(?::[^=]+?)?\s*=>|[A-Za-z_$][\w$]*\s*=>|\(\s*$|\([^)]*$)`),
				kind: KindFunction, atBodyDepth: true},
			{re: rx(`^\s*(?:export\s+)?(?P<kind>const|let|var)\s+(?P<name>[A-Za-z_$][\w$]*)\s*(?::[^=]+)?=`),
				kindFrom: map[string]SymbolKind{"const": KindConst, "let": KindVar, "var": KindVar}, atBodyDepth: true,
				skipIf: rx(`=\s*(?:require|import)\s*\(`)},
			{re: rx(`^\s*` + mods + `\*?\s*(?P<name>#?[A-Za-z_$][\w$]*)\s*[?!]?\s*(?:<[^>]*>)?\s*\(`), kind: KindFunction, scope: scopeMember},
			{re: rx(`^\s*` + mods + `(?P<name>#?[A-Za-z_$][\w$]*)\s*(?::[^=]+)?=\s*(?:async\s+)?(?:\([^)]*\)|[A-Za-z_$][\w$]*)\s*(?::[^=]+?)?\s*=>`), kind: KindFunction, scope: scopeMember},
			{re: rx(`^\s*` + mods + `(?P<name>#?[A-Za-z_$][\w$]*)\s*[?!]?\s*:\s*[^=;(]+;?\s*$`), kind: KindField, scope: scopeMember},
		},
	}
}

func cPatterns() *langPattern {
	quals := `(?:(?:static|inline|extern|virtual|explicit|constexpr|consteval|constinit|friend|unsigned|signed|const|volatile|struct|enum|typename|__inline|__forceinline|override)\s+)*`
	return &langPattern{
		block:  blockBraces,
		reject: cReject,
		decorator: func(t string) bool {
			return strings.HasPrefix(t, "[[") || strings.HasPrefix(t, "template") || strings.HasPrefix(t, "__attribute__")
		},
		exported: func(m, _ string) bool { return !strings.Contains(m, "static ") },
		patterns: []pattern{
			{re: rx(`^\s*extern\s+"[^"]*"\s*\{`), kind: KindModule, container: true, noEmit: true},
			{re: rx(`^\s*#\s*define\s+(?P<name>[A-Za-z_]\w*)`), kind: KindMacro},
			{re: rx(`^\s*typedef\s+[^;{]*\(\s*\*\s*(?P<name>[A-Za-z_]\w*)\s*\)`), kind: KindType},
			{re: rx(`^\s*(?:typedef\s+)?(?:template\s*<[^>]*>\s*)?(?P<kind>struct|class|union|enum\s+class|enum\s+struct|enum)\s+(?:alignas\s*\([^)]*\)\s*)?(?:\[\[[^\]]*\]\]\s*)?(?:[A-Z_][A-Z0-9_]*\s+)?(?P<name>[A-Za-z_]\w*)\s*(?:final\s*)?(?::\s*[^;{]*)?(?:\{.*)?$`),
				kindFrom: map[string]SymbolKind{"struct": KindStruct, "class": KindClass, "union": KindStruct, "enum class": KindEnum, "enum struct": KindEnum, "enum": KindEnum}, container: true},
			{re: rx(`^\s*\}\s*(?P<name>[A-Za-z_]\w*)\s*;`), kind: KindType},
			{re: rx(`^\s*typedef\s+[^;{()]*?\b(?P<name>[A-Za-z_]\w*)\s*(?:\[[^\]]*\])?\s*;`), kind: KindType},
			{re: rx(`^\s*(?:template\s*<[^>]*>\s*)?using\s+(?P<name>[A-Za-z_]\w*)\s*=`), kind: KindType},
			{re: rx(`^\s*(?:inline\s+)?namespace\s+(?P<name>[A-Za-z_][\w:]*)\s*(?:\{.*)?$`), kind: KindModule, container: true},
			{re: rx(`^\s*(?:template\s*<[^>]*>\s*)?` + quals + `(?:[A-Za-z_][\w:]*(?:\s*<[^;{}()]*>)?[\s*&]+)+(?P<name>(?:[A-Za-z_]\w*::)*~?[A-Za-z_]\w*|operator\s*(?:\(\)|[^\s(]+))\s*\(`),
				kind: KindFunction, qualified: true, atBodyDepth: true,
				skipIf: rx(`^\s*(?:return|else|if|while|for|switch|case|do|goto|throw|delete|new|sizeof|co_return|co_await|co_yield|typedef|using)\b`)},
			{re: rx(`^\s*(?P<name>(?:[A-Za-z_]\w*::)+~?[A-Za-z_]\w*)\s*\(`), kind: KindFunction, qualified: true, atBodyDepth: true},
			{re: rx(`^\s*(?:(?:explicit|virtual|inline|constexpr)\s+)*(?P<name>~?[A-Za-z_]\w*)\s*\(`), kind: KindFunction, scope: scopeMember, ctorOnly: true},
		},
	}
}

// openScope is a definition whose body may hold other definitions.
type openScope struct {
	name      string
	kind      SymbolKind
	end       int // 0-based last line
	bodyDepth int
	indent    int
	container bool
}

func (s *openScope) classLike() bool { return s.container && s.kind != KindModule }

// extractPatterns runs a language's patterns over a masked source.
func extractPatterns(lang string, lp *langPattern, src string) []Symbol {
	lx := lexSource(src, lexSpecs[lang])
	var syms []Symbol
	var stack []openScope
	for L := 0; L < len(lx.masked); L++ {
		m := lx.masked[L]
		if strings.TrimSpace(m) == "" {
			continue
		}
		for len(stack) > 0 && stack[len(stack)-1].end < L {
			stack = stack[:len(stack)-1]
		}
		if lx.insideAt[L] {
			continue // starts inside a string or comment
		}
		if lp.block != blockBraces && lx.parenAt[L] > 0 {
			continue // continuation of a bracketed expression
		}
		var top *openScope
		if len(stack) > 0 {
			top = &stack[len(stack)-1]
		}
		for pi := range lp.patterns {
			p := &lp.patterns[pi]
			sm := p.re.FindStringSubmatchIndex(m)
			if sm == nil {
				continue
			}
			if p.skipIf != nil && p.skipIf.MatchString(m) {
				continue
			}
			name, nameCol := group(p.re, m, sm, "name")
			if name == "" && !p.noEmit {
				continue
			}
			if lp.reject[name] {
				continue
			}
			inBody := top != nil && !top.container
			if p.ignoreScope {
				inBody = false
				top = nil
				stack = stack[:0]
			}
			switch p.scope {
			case scopeAny:
				if inBody {
					continue
				}
			case scopeMember:
				if top == nil || !top.classLike() {
					continue
				}
				if lp.block == blockBraces && lx.depthAt[L] != top.bodyDepth {
					continue
				}
			}
			if p.atBodyDepth && lp.block == blockBraces {
				want := 0
				if top != nil {
					if !top.container {
						continue
					}
					want = top.bodyDepth
				}
				if lx.depthAt[L] != want {
					continue
				}
			}
			if p.ctorOnly && (top == nil || strings.TrimPrefix(name, "~") != top.name) {
				continue
			}
			kind := p.kind
			if p.kindFrom != nil {
				k, _ := group(p.re, m, sm, "kind")
				if mk, ok := p.kindFrom[strings.Join(strings.Fields(k), " ")]; ok {
					kind = mk
				}
			}
			container := ""
			if top != nil && top.container {
				container = top.name
			}
			if c, _ := group(p.re, m, sm, "container"); c != "" {
				container = c
				if kind == KindFunction {
					kind = KindMethod
				}
			}
			if p.qualified {
				if i := strings.LastIndex(name, "::"); i >= 0 {
					container = name[:i]
					name = name[i+2:]
					if kind == KindFunction {
						kind = KindMethod
					}
				}
			}
			if kind == KindFunction && top != nil && top.classLike() {
				kind = KindMethod
			}
			if nameCol < 0 {
				nameCol = 0
			}
			end, sig, bodyDepth := lx.extent(lp.block, L, nameCol)
			if lp.block != blockBraces && !p.container && !kind.isCallable() && !kind.isTypeLike() {
				// A constant or field spans only its own (bracketed) header.
				end = lx.headerEnd(L)
				sig = sigEnd{endLine: end, endCol: -1}
			}
			if !p.noEmit {
				doc, docLine := lx.docAbove(L, lp.decorator)
				if lp.docstring {
					if ds := lx.pyDocstring(sig.endLine); ds != "" {
						doc = ds
					}
				}
				exported := true
				if lp.exported != nil {
					exported = lp.exported(m, name)
				}
				syms = append(syms, Symbol{
					Name:      name,
					Kind:      kind,
					Container: container,
					Line:      L + 1,
					EndLine:   end + 1,
					DocLine:   docLine + 1,
					Signature: lx.signature(lp.block, L, sig),
					Doc:       doc,
					Exported:  exported,
				})
			}
			if end > L && (p.container || kind.isCallable() || kind.isTypeLike()) {
				scopeName := name
				if p.noEmit && scopeName == "" && top != nil {
					scopeName = top.name
				}
				stack = append(stack, openScope{
					name:      scopeName,
					kind:      kind,
					end:       end,
					bodyDepth: bodyDepth,
					indent:    indentOf(lx.raw[L]),
					container: p.container,
				})
			}
			break
		}
	}
	return syms
}

// group returns the text and start column of a named group.
func group(re *regexp.Regexp, s string, sm []int, name string) (string, int) {
	i := re.SubexpIndex(name)
	if i < 0 || 2*i+1 >= len(sm) || sm[2*i] < 0 {
		return "", -1
	}
	return s[sm[2*i]:sm[2*i+1]], sm[2*i]
}

// sigEnd marks where a definition's header ends.
type sigEnd struct {
	endLine int // 0-based line of the header end
	endCol  int // byte column to cut at on endLine; -1 for the whole line
}

func indentOf(s string) int {
	n := 0
	for _, c := range s {
		switch c {
		case ' ':
			n++
		case '\t':
			n += 4
		default:
			return n
		}
	}
	return n
}

// extent finds where the definition starting at line L ends, where its
// header ends and, for brace languages, the brace depth inside its body.
func (lx *lexed) extent(style blockStyle, L, col int) (end int, sig sigEnd, bodyDepth int) {
	switch style {
	case blockIndent:
		s := lx.headerEnd(L)
		return lx.indentEnd(L, s), sigEnd{endLine: s, endCol: -1}, 0
	case blockRubyEnd:
		return lx.rubyEnd(L), sigEnd{endLine: L, endCol: -1}, 0
	}
	return lx.braceSpan(L, col)
}

// braceSpan locates the body of a brace-language definition.
func (lx *lexed) braceSpan(L, col int) (int, sigEnd, int) {
	ev := lx.events
	i := sort.Search(len(ev), func(k int) bool {
		return ev[k].line > L || (ev[k].line == L && ev[k].col >= col)
	})
	for ; i < len(ev) && ev[i].line <= L+15; i++ {
		e := ev[i]
		switch e.kind {
		case '{':
			if !lx.continues(L, e.line) {
				return L, sigEnd{endLine: L, endCol: -1}, 0
			}
			return e.match, sigEnd{endLine: e.line, endCol: e.col}, lx.depthBefore(i) + 1
		case ';':
			if !lx.continues(L, e.line) {
				return L, sigEnd{endLine: L, endCol: -1}, 0
			}
			return e.line, sigEnd{endLine: e.line, endCol: e.col}, 0
		default:
			return L, sigEnd{endLine: L, endCol: -1}, 0
		}
	}
	return L, sigEnd{endLine: L, endCol: -1}, 0
}

// depthBefore is the brace depth just before event i.
func (lx *lexed) depthBefore(i int) int {
	e := lx.events[i]
	d := lx.depthAt[e.line]
	for k := i - 1; k >= 0 && lx.events[k].line == e.line; k-- {
		switch lx.events[k].kind {
		case '{':
			d++
		case '}':
			d--
		}
	}
	if d < 0 {
		d = 0
	}
	return d
}

// continues reports whether the statement on line from plausibly runs on
// to line to, so that a brace or semicolon there belongs to it.
func (lx *lexed) continues(from, to int) bool {
	for cur := from; cur < to; cur++ {
		next := cur + 1
		if next < len(lx.parenAt) && lx.parenAt[next] > 0 {
			continue
		}
		t := strings.TrimSpace(lx.masked[cur])
		if t == "" {
			continue
		}
		nt := ""
		if next < len(lx.masked) {
			nt = strings.TrimSpace(lx.masked[next])
		}
		if next == to && strings.HasPrefix(nt, "{") {
			continue
		}
		if strings.ContainsAny(t[len(t)-1:], ")=,(<>:|&+-*/?.\\") {
			continue
		}
		last := t
		if i := strings.LastIndexAny(t, " \t"); i >= 0 {
			last = t[i+1:]
		}
		switch last {
		case "where", "extends", "implements", "throws", "const", "override", "noexcept", "final", "mut":
			continue
		}
		if !continuationStart(nt, next == to) {
			return false
		}
	}
	return true
}

// continuationStart reports whether a line beginning with t continues the
// statement above it. atTarget is true when t holds the brace or semicolon
// being attributed.
func continuationStart(t string, atTarget bool) bool {
	for _, p := range []string{":", ",", "->", "where", "extends", "implements", "throws", "&&", "||", "?", "=>", "{"} {
		if strings.HasPrefix(t, p) && (!atTarget || p == "{" || p == ":" || p == "where") {
			return true
		}
	}
	return false
}

// headerEnd is the last line of a Python-style header that may span lines
// inside brackets.
func (lx *lexed) headerEnd(L int) int {
	s := L
	for s+1 < len(lx.parenAt) && lx.parenAt[s+1] > 0 && s-L < 30 {
		s++
	}
	return s
}

// indentEnd finds the last line of an indentation-delimited block.
func (lx *lexed) indentEnd(L, header int) int {
	base := indentOf(lx.masked[L])
	last := header
	for k := header + 1; k < len(lx.masked); k++ {
		t := strings.TrimSpace(lx.masked[k])
		if t == "" {
			continue
		}
		if lx.parenAt[k] > 0 || lx.insideAt[k] {
			last = k
			continue
		}
		if indentOf(lx.masked[k]) <= base {
			break
		}
		last = k
	}
	// Multi-line strings are masked to blanks; extend over raw lines that
	// belong to the block so a trailing docstring is included.
	for last+1 < len(lx.raw) && strings.TrimSpace(lx.masked[last+1]) == "" && strings.TrimSpace(lx.raw[last+1]) != "" &&
		indentOf(lx.raw[last+1]) > base {
		last++
	}
	return last
}

var rubyOneLine = regexp.MustCompile(`;\s*end\b|\)\s*=[^=~>]|^\s*def\s+[\w?!.]+\s*=[^=~>]`)

// rubyEnd finds the "end" closing a Ruby definition.
func (lx *lexed) rubyEnd(L int) int {
	m := lx.masked[L]
	if rubyOneLine.MatchString(m) {
		return L
	}
	base := indentOf(m)
	for k := L + 1; k < len(lx.masked); k++ {
		t := strings.TrimSpace(lx.masked[k])
		if t == "" || lx.insideAt[k] {
			continue
		}
		ind := indentOf(lx.masked[k])
		if ind == base && (t == "end" || (strings.HasPrefix(t, "end") && len(t) > 3 && !isIdentByte(t[3]))) {
			return k
		}
		if ind < base {
			return k - 1
		}
	}
	return lx.indentEnd(L, L)
}

// signature renders the header of a definition on one line.
func (lx *lexed) signature(style blockStyle, L int, sig sigEnd) string {
	endLine := sig.endLine
	if endLine-L > 5 {
		endLine = L + 5
	}
	var parts []string
	for k := L; k <= endLine && k < len(lx.raw); k++ {
		line := lx.raw[k]
		if k == sig.endLine && sig.endCol >= 0 && sig.endCol <= len(line) {
			line = line[:sig.endCol]
		}
		parts = append(parts, strings.TrimSpace(line))
	}
	s := collapseSpace(strings.Join(parts, " "))
	if style == blockIndent {
		s = strings.TrimSuffix(s, ":")
	}
	return strings.TrimSpace(s)
}

// docAbove collects the comment block directly above line L, skipping
// decorators and attributes. It returns the doc text and the first line of
// the block (L when there is none).
func (lx *lexed) docAbove(L int, decorator func(string) bool) (string, int) {
	first := L
	var lines []string
	for k := L - 1; k >= 0 && L-k <= 60; k-- {
		raw := strings.TrimSpace(lx.raw[k])
		if raw == "" {
			break
		}
		masked := strings.TrimSpace(lx.masked[k])
		if masked == "" {
			lines = append(lines, stripCommentMarkers(raw))
			first = k
			continue
		}
		if decorator != nil && decorator(raw) {
			first = k
			continue
		}
		break
	}
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	return strings.TrimSpace(strings.Join(dropEmptyEdges(lines), "\n")), first
}

func dropEmptyEdges(lines []string) []string {
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// stripCommentMarkers removes comment syntax from one comment line.
func stripCommentMarkers(s string) string {
	s = strings.TrimSpace(s)
	for _, p := range []string{"///", "//!", "//", "/**", "/*!", "/*", "*/", "#", "*", "=begin", "=end"} {
		if strings.HasPrefix(s, p) {
			s = s[len(p):]
			break
		}
	}
	s = strings.TrimSuffix(strings.TrimSpace(s), "*/")
	return strings.TrimSpace(s)
}

var docstringOpen = regexp.MustCompile(`^[rRuUbB]?("""|''')`)

// pyDocstring returns the docstring that follows a header ending on line h.
func (lx *lexed) pyDocstring(h int) string {
	k := h + 1
	for k < len(lx.raw) && strings.TrimSpace(lx.raw[k]) == "" {
		k++
	}
	if k >= len(lx.raw) {
		return ""
	}
	t := strings.TrimSpace(lx.raw[k])
	loc := docstringOpen.FindStringSubmatchIndex(t)
	if loc == nil {
		return ""
	}
	q := t[loc[2]:loc[3]]
	rest := t[loc[1]:]
	if i := strings.Index(rest, q); i >= 0 {
		return strings.TrimSpace(rest[:i])
	}
	parts := []string{rest}
	for j := k + 1; j < len(lx.raw) && j-k < 80; j++ {
		line := lx.raw[j]
		if i := strings.Index(line, q); i >= 0 {
			parts = append(parts, line[:i])
			break
		}
		parts = append(parts, line)
	}
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return strings.TrimSpace(strings.Join(dropEmptyEdges(parts), "\n"))
}

var mdHeading = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s*#*\s*$`)

// extractMarkdown turns headings into section symbols.
func extractMarkdown(src string) []Symbol {
	lines := strings.Split(src, "\n")
	type head struct{ line, level int }
	var heads []head
	var syms []Symbol
	fence := ""
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			mark := t[:3]
			if fence == "" {
				fence = mark
			} else if fence == mark {
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}
		m := mdHeading.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		name := strings.TrimSpace(strings.NewReplacer("`", "", "**", "", "*", "", "_", " ").Replace(m[2]))
		if name == "" {
			continue
		}
		heads = append(heads, head{i, len(m[1])})
		syms = append(syms, Symbol{Name: name, Kind: KindSection, Line: i + 1, DocLine: i + 1, Signature: strings.TrimSpace(l), Exported: true})
	}
	for i := range heads {
		end := len(lines) - 1
		for j := i + 1; j < len(heads); j++ {
			if heads[j].level <= heads[i].level {
				end = heads[j].line - 1
				break
			}
		}
		for end > heads[i].line && strings.TrimSpace(lines[end]) == "" {
			end--
		}
		syms[i].EndLine = end + 1
	}
	return syms
}
