package supplychain

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Reachability is a hint of whether vulnerable code is used.
type Reachability int

// Reachability levels, from least to most evidence of use.
const (
	// ReachUnknown: no analysis available for the ecosystem.
	ReachUnknown Reachability = iota
	// ReachNotImported: no workspace file imports an affected package.
	ReachNotImported
	// ReachImported: an affected package is imported.
	ReachImported
	// ReachSymbolUsed: an affected symbol is referenced (pkg.Symbol).
	ReachSymbolUsed
)

func (r Reachability) String() string {
	switch r {
	case ReachNotImported:
		return "not-imported"
	case ReachImported:
		return "imported"
	case ReachSymbolUsed:
		return "symbol-used"
	}
	return "unknown"
}

// MarshalText implements encoding.TextMarshaler.
func (r Reachability) MarshalText() ([]byte, error) { return []byte(r.String()), nil }

// GoImportIndex records the import paths and qualified identifiers used
// by the Go files of a workspace.
type GoImportIndex struct {
	// Imports maps an import path to the files importing it.
	Imports map[string][]string
	// Selectors maps an import path to the set of identifiers referenced
	// through it (e.g. "Get" for http.Get).
	Selectors map[string]map[string]bool
	// Errors collects files that failed to parse.
	Errors []error
}

// IndexGoImports parses every .go file under root (skipping vendor,
// testdata, hidden and underscore directories) with go/parser.
func IndexGoImports(root string) (*GoImportIndex, error) {
	ix := &GoImportIndex{Imports: map[string][]string{}, Selectors: map[string]map[string]bool{}}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			ix.Errors = append(ix.Errors, err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if p != root && (name == "vendor" || name == "testdata" || name == "node_modules" ||
				strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > 8<<20 {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			ix.Errors = append(ix.Errors, err)
			return nil
		}
		f, err := parser.ParseFile(fset, p, src, parser.SkipObjectResolution)
		if err != nil {
			ix.Errors = append(ix.Errors, err)
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		ix.addFile(filepath.ToSlash(rel), f)
		return nil
	})
	return ix, err
}

func (ix *GoImportIndex) addFile(rel string, f *ast.File) {
	local := map[string]string{} // local name -> import path
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		ix.Imports[path] = append(ix.Imports[path], rel)
		name := ""
		if imp.Name != nil {
			name = imp.Name.Name
		} else {
			name = defaultImportName(path)
		}
		if name == "_" || name == "." {
			continue
		}
		local[name] = path
	}
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if path, ok := local[id.Name]; ok {
			m := ix.Selectors[path]
			if m == nil {
				m = map[string]bool{}
				ix.Selectors[path] = m
			}
			m[sel.Sel.Name] = true
		}
		return true
	})
}

// defaultImportName guesses the package name of an import path: the last
// element, skipping a major-version suffix and gopkg.in ".vN" suffixes.
func defaultImportName(path string) string {
	elems := strings.Split(path, "/")
	last := elems[len(elems)-1]
	if len(elems) > 1 && len(last) >= 2 && last[0] == 'v' && isDigits(last[1:]) {
		last = elems[len(elems)-2]
	}
	if i := strings.Index(last, ".v"); i > 0 && isDigits(last[i+2:]) {
		last = last[:i]
	}
	last = strings.TrimPrefix(last, "go-")
	return strings.Map(func(r rune) rune {
		if r == '-' || r == '.' {
			return '_'
		}
		return r
	}, last)
}

type goEcosystemSpecific struct {
	Imports []struct {
		Path    string   `json:"path"`
		Symbols []string `json:"symbols"`
	} `json:"imports"`
}

// Reachability evaluates an affected Go module entry against the index.
// When the advisory lists affected import paths and symbols they are
// used; otherwise any import inside the module path counts.
func (ix *GoImportIndex) Reachability(a *OSVAffected) Reachability {
	if ix == nil || a == nil {
		return ReachUnknown
	}
	var es goEcosystemSpecific
	if len(a.EcosystemSpecific) > 0 {
		_ = json.Unmarshal(a.EcosystemSpecific, &es)
	}
	if len(es.Imports) == 0 {
		mod := a.Package.Name
		for p := range ix.Imports {
			if p == mod || strings.HasPrefix(p, mod+"/") {
				return ReachImported
			}
		}
		return ReachNotImported
	}
	best := ReachNotImported
	for _, imp := range es.Imports {
		if _, ok := ix.Imports[imp.Path]; !ok {
			continue
		}
		if best < ReachImported {
			best = ReachImported
		}
		used := ix.Selectors[imp.Path]
		for _, sym := range imp.Symbols {
			// "Type.Method" symbols are referenced through the type name.
			head, _, _ := strings.Cut(sym, ".")
			if used[sym] || used[head] {
				return ReachSymbolUsed
			}
		}
	}
	return best
}

// ImportPaths returns the sorted import paths seen.
func (ix *GoImportIndex) ImportPaths() []string {
	out := make([]string, 0, len(ix.Imports))
	for p := range ix.Imports {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
