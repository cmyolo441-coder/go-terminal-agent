package codeintel

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
)

// extractGo parses Go source exactly with go/parser. It returns false only
// when the file is too broken to yield an AST at all; a file with syntax
// errors still yields the declarations the parser recovered.
func extractGo(src string) ([]Symbol, bool) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ParseComments|parser.SkipObjectResolution)
	if f == nil || (err != nil && (!f.Package.IsValid() || len(f.Decls) == 0)) {
		return nil, false
	}
	g := goExtractor{fset: fset, src: src}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			g.funcDecl(d)
		case *ast.GenDecl:
			g.genDecl(d)
		}
	}
	if err != nil {
		// The parser drops declarations after a syntax error; recover the
		// ones it missed with the structural matcher.
		have := map[string]bool{}
		for _, s := range g.syms {
			have[s.QualifiedName()] = true
		}
		for _, s := range extractGoFallback(src) {
			if !have[s.QualifiedName()] {
				g.syms = append(g.syms, s)
			}
		}
	}
	return g.syms, true
}

// extractGoFallback recovers top-level declarations from Go source the
// parser could not handle.
func extractGoFallback(src string) []Symbol {
	return extractPatterns(LangGo, langPatterns[LangGo], src)
}

type goExtractor struct {
	fset *token.FileSet
	src  string
	syms []Symbol
}

func (g *goExtractor) line(p token.Pos) int {
	if !p.IsValid() {
		return 0
	}
	return g.fset.Position(p).Line
}

func (g *goExtractor) off(p token.Pos) int {
	if !p.IsValid() {
		return -1
	}
	o := g.fset.Position(p).Offset
	if o > len(g.src) {
		o = len(g.src)
	}
	return o
}

// text returns the source between two positions.
func (g *goExtractor) text(from, to token.Pos) string {
	a, b := g.off(from), g.off(to)
	if a < 0 || b < a {
		return ""
	}
	return g.src[a:b]
}

func docText(cg *ast.CommentGroup) string {
	if cg == nil {
		return ""
	}
	return strings.TrimSpace(cg.Text())
}

func (g *goExtractor) docLine(cg *ast.CommentGroup, fallback int) int {
	if cg == nil {
		return fallback
	}
	return g.line(cg.Pos())
}

func (g *goExtractor) funcDecl(d *ast.FuncDecl) {
	if d.Name == nil {
		return
	}
	s := Symbol{
		Name:      d.Name.Name,
		Kind:      KindFunction,
		Line:      g.line(d.Pos()),
		EndLine:   g.line(d.End()),
		Doc:       docText(d.Doc),
		Exported:  ast.IsExported(d.Name.Name),
		Signature: g.text(d.Pos(), d.Type.End()),
	}
	s.DocLine = g.docLine(d.Doc, s.Line)
	if d.Recv != nil && len(d.Recv.List) > 0 {
		s.Kind = KindMethod
		s.Container = recvTypeName(d.Recv.List[0].Type)
	}
	g.syms = append(g.syms, s)
}

// recvTypeName is the base type name of a method receiver: *T[K] -> T.
func recvTypeName(e ast.Expr) string {
	for {
		switch t := e.(type) {
		case *ast.StarExpr:
			e = t.X
		case *ast.ParenExpr:
			e = t.X
		case *ast.IndexExpr:
			e = t.X
		case *ast.IndexListExpr:
			e = t.X
		case *ast.SelectorExpr:
			return t.Sel.Name
		case *ast.Ident:
			return t.Name
		default:
			return ""
		}
	}
}

func (g *goExtractor) genDecl(d *ast.GenDecl) {
	grouped := d.Lparen.IsValid()
	for _, spec := range d.Specs {
		switch sp := spec.(type) {
		case *ast.TypeSpec:
			g.typeSpec(d, sp, grouped)
		case *ast.ValueSpec:
			g.valueSpec(d, sp, grouped)
		}
	}
}

func (g *goExtractor) typeSpec(d *ast.GenDecl, ts *ast.TypeSpec, grouped bool) {
	if ts.Name == nil {
		return
	}
	name := ts.Name.Name
	kind := KindType
	head := strings.TrimSpace(g.text(ts.Pos(), ts.Type.Pos()))
	switch ts.Type.(type) {
	case *ast.StructType:
		kind = KindStruct
		head += " struct"
	case *ast.InterfaceType:
		kind = KindInterface
		head += " interface"
	default:
		typ := g.text(ts.Type.Pos(), ts.Type.End())
		if i := strings.IndexByte(typ, '\n'); i >= 0 {
			typ = typ[:i] + " …"
		}
		head += " " + typ
	}
	s := Symbol{
		Name:      name,
		Kind:      kind,
		Line:      g.line(ts.Pos()),
		EndLine:   g.line(ts.End()),
		Doc:       docText(ts.Doc),
		Exported:  ast.IsExported(name),
		Signature: "type " + head,
	}
	doc := ts.Doc
	if !grouped {
		s.Line = g.line(d.Pos())
		if doc == nil {
			doc = d.Doc
			s.Doc = docText(d.Doc)
		}
	}
	if s.Doc == "" {
		s.Doc = docText(ts.Comment)
	}
	s.DocLine = g.docLine(doc, s.Line)
	g.syms = append(g.syms, s)

	switch t := ts.Type.(type) {
	case *ast.InterfaceType:
		if t.Methods == nil {
			return
		}
		for _, f := range t.Methods.List {
			if _, ok := f.Type.(*ast.FuncType); !ok || len(f.Names) == 0 {
				continue
			}
			for _, n := range f.Names {
				m := Symbol{
					Name:      n.Name,
					Kind:      KindMethod,
					Container: name,
					Line:      g.line(f.Pos()),
					EndLine:   g.line(f.End()),
					Doc:       firstNonEmpty(docText(f.Doc), docText(f.Comment)),
					Exported:  ast.IsExported(n.Name),
					Signature: g.text(f.Pos(), f.End()),
				}
				m.DocLine = g.docLine(f.Doc, m.Line)
				g.syms = append(g.syms, m)
			}
		}
	case *ast.StructType:
		if t.Fields == nil {
			return
		}
		for _, f := range t.Fields.List {
			for _, n := range f.Names {
				if n.Name == "_" {
					continue
				}
				fs := Symbol{
					Name:      n.Name,
					Kind:      KindField,
					Container: name,
					Line:      g.line(n.Pos()),
					EndLine:   g.line(f.End()),
					Doc:       firstNonEmpty(docText(f.Doc), docText(f.Comment)),
					Exported:  ast.IsExported(n.Name),
					Signature: strings.TrimSpace(n.Name + " " + g.text(f.Type.Pos(), f.Type.End())),
				}
				fs.DocLine = g.docLine(f.Doc, fs.Line)
				g.syms = append(g.syms, fs)
			}
		}
	}
}

func (g *goExtractor) valueSpec(d *ast.GenDecl, vs *ast.ValueSpec, grouped bool) {
	kind := KindVar
	if d.Tok == token.CONST {
		kind = KindConst
	}
	text := g.text(vs.Pos(), vs.End())
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[:i] + " …"
	}
	doc := vs.Doc
	if !grouped && doc == nil {
		doc = d.Doc
	}
	for _, n := range vs.Names {
		if n.Name == "_" {
			continue
		}
		s := Symbol{
			Name:      n.Name,
			Kind:      kind,
			Line:      g.line(n.Pos()),
			EndLine:   g.line(vs.End()),
			Doc:       firstNonEmpty(docText(doc), docText(vs.Comment)),
			Exported:  ast.IsExported(n.Name),
			Signature: d.Tok.String() + " " + text,
		}
		if !grouped {
			s.Line = g.line(d.Pos())
		}
		s.DocLine = g.docLine(doc, s.Line)
		g.syms = append(g.syms, s)
	}
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
