package lsp

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// displayPos converts a protocol position to a 1-based line and 1-based
// rune column using the document text.
func displayPos(lines []string, p Position, enc string) (int, int) {
	line := p.Line + 1
	col := p.Character + 1
	if p.Line >= 0 && p.Line < len(lines) {
		col = fromProtocolChar(lines[p.Line], p.Character, enc) + 1
	}
	return line, col
}

// sortDiagnostics orders diagnostics by severity, then position.
func sortDiagnostics(ds []Diagnostic) {
	sort.SliceStable(ds, func(i, j int) bool {
		a, b := ds[i], ds[j]
		if a.EffectiveSeverity() != b.EffectiveSeverity() {
			return a.EffectiveSeverity() < b.EffectiveSeverity()
		}
		if a.Range.Start.Line != b.Range.Start.Line {
			return a.Range.Start.Line < b.Range.Start.Line
		}
		return a.Range.Start.Character < b.Range.Start.Character
	})
}

// FormatDiagnostics renders up to limit diagnostics as
// "L12:5 error: message [source code]" lines with the offending source
// line, most severe first.
func FormatDiagnostics(res *FileDiagnostics, limit int) string {
	ds := append([]Diagnostic(nil), res.Diagnostics...)
	sortDiagnostics(ds)
	var b strings.Builder
	for i, d := range ds {
		if limit > 0 && i >= limit {
			fmt.Fprintf(&b, "… %d more not shown\n", len(ds)-limit)
			break
		}
		line, col := displayPos(res.Lines, d.Range.Start, res.Encoding)
		msg := strings.TrimSpace(d.Message)
		if i := strings.IndexByte(msg, '\n'); i >= 0 {
			msg = strings.TrimSpace(msg[:i]) + " …"
		}
		fmt.Fprintf(&b, "L%d:%d %s: %s", line, col, d.EffectiveSeverity(), msg)
		var tags []string
		if d.Source != "" {
			tags = append(tags, d.Source)
		}
		if c := d.CodeString(); c != "" {
			tags = append(tags, c)
		}
		if len(tags) > 0 {
			fmt.Fprintf(&b, " [%s]", strings.Join(tags, " "))
		}
		b.WriteString("\n")
		if d.Range.Start.Line >= 0 && d.Range.Start.Line < len(res.Lines) {
			src := strings.TrimRight(strings.ReplaceAll(res.Lines[d.Range.Start.Line], "\t", "    "), " \r")
			if strings.TrimSpace(src) != "" {
				fmt.Fprintf(&b, "    %s\n", clipLine(src, 200))
			}
		}
	}
	return b.String()
}

func clipLine(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}

// lineCache reads files once per formatting pass.
type lineCache map[string][]string

func (lc lineCache) lines(path string) []string {
	if l, ok := lc[path]; ok {
		return l
	}
	var l []string
	if st, err := os.Stat(path); err == nil && st.Size() <= maxDocumentSize {
		if data, err := os.ReadFile(path); err == nil {
			l = splitLines(string(data))
		}
	}
	lc[path] = l
	return l
}

// FormatLocations renders locations as path:line:col with the source line,
// paths relative to root when inside it.
func FormatLocations(root string, locs []Location, enc func(path string) string, limit int) string {
	if len(locs) == 0 {
		return "No locations found.\n"
	}
	lc := lineCache{}
	var b strings.Builder
	for i, l := range locs {
		if limit > 0 && i >= limit {
			fmt.Fprintf(&b, "… %d more not shown\n", len(locs)-limit)
			break
		}
		p, err := URIToPath(l.URI)
		if err != nil {
			fmt.Fprintf(&b, "%s\n", l.URI)
			continue
		}
		lines := lc.lines(p)
		line, col := displayPos(lines, l.Range.Start, enc(p))
		shown := p
		if r, err := filepath.Rel(root, p); err == nil && !strings.HasPrefix(r, "..") {
			shown = filepath.ToSlash(r)
		}
		fmt.Fprintf(&b, "%s:%d:%d", shown, line, col)
		if l.Range.Start.Line < len(lines) {
			if src := strings.TrimSpace(lines[l.Range.Start.Line]); src != "" {
				fmt.Fprintf(&b, "  %s", clipLine(src, 160))
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}
