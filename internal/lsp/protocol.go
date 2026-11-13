package lsp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Position is a zero-based line and character offset; the character unit
// is the negotiated position encoding (UTF-16 unless the server chose
// otherwise).
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Range is a half-open span between two positions.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Location is a range in a document.
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// LocationLink is the richer form of Location some servers return.
type LocationLink struct {
	OriginSelectionRange *Range `json:"originSelectionRange,omitempty"`
	TargetURI            string `json:"targetUri"`
	TargetRange          Range  `json:"targetRange"`
	TargetSelectionRange Range  `json:"targetSelectionRange"`
}

// Severity of a diagnostic.
type Severity int

// Diagnostic severities.
const (
	SeverityError       Severity = 1
	SeverityWarning     Severity = 2
	SeverityInformation Severity = 3
	SeverityHint        Severity = 4
)

func (s Severity) String() string {
	switch s {
	case SeverityError:
		return "error"
	case SeverityWarning:
		return "warning"
	case SeverityInformation:
		return "info"
	case SeverityHint:
		return "hint"
	}
	return "error" // the spec says clients decide; treat unknown as error
}

// ParseSeverity accepts error, warning, info or hint.
func ParseSeverity(s string) (Severity, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "error", "errors":
		return SeverityError, true
	case "warning", "warnings", "warn":
		return SeverityWarning, true
	case "info", "information":
		return SeverityInformation, true
	case "hint", "hints", "all":
		return SeverityHint, true
	}
	return 0, false
}

// DiagnosticRelatedInformation points at a related location.
type DiagnosticRelatedInformation struct {
	Location Location `json:"location"`
	Message  string   `json:"message"`
}

// Diagnostic is one problem reported by a server.
type Diagnostic struct {
	Range              Range                          `json:"range"`
	Severity           Severity                       `json:"severity,omitempty"`
	Code               json.RawMessage                `json:"code,omitempty"`
	Source             string                         `json:"source,omitempty"`
	Message            string                         `json:"message"`
	Tags               []int                          `json:"tags,omitempty"`
	RelatedInformation []DiagnosticRelatedInformation `json:"relatedInformation,omitempty"`
}

// CodeString renders the diagnostic code, which may be a number or string.
func (d Diagnostic) CodeString() string {
	raw := bytes.TrimSpace(d.Code)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// EffectiveSeverity treats a missing severity as an error.
func (d Diagnostic) EffectiveSeverity() Severity {
	if d.Severity < SeverityError || d.Severity > SeverityHint {
		return SeverityError
	}
	return d.Severity
}

type publishDiagnosticsParams struct {
	URI         string       `json:"uri"`
	Version     *int         `json:"version,omitempty"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

type textDocumentItem struct {
	URI        string `json:"uri"`
	LanguageID string `json:"languageId"`
	Version    int    `json:"version"`
	Text       string `json:"text"`
}

type textDocumentIdentifier struct {
	URI string `json:"uri"`
}

type versionedTextDocumentIdentifier struct {
	URI     string `json:"uri"`
	Version int    `json:"version"`
}

type textDocumentContentChangeEvent struct {
	Text string `json:"text"`
}

type textDocumentPositionParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
}

// Position encodings.
const (
	EncodingUTF8  = "utf-8"
	EncodingUTF16 = "utf-16"
	EncodingUTF32 = "utf-32"
)

// PathToURI converts an absolute file path to a file:// URI.
func PathToURI(p string) string {
	p = filepath.ToSlash(filepath.Clean(p))
	if runtime.GOOS == "windows" && len(p) >= 2 && p[1] == ':' {
		p = "/" + p
	}
	u := url.URL{Scheme: "file", Path: p}
	return u.String()
}

// URIToPath converts a file:// URI to a clean absolute path.
func URIToPath(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", fmt.Errorf("bad uri %q: %w", uri, err)
	}
	if u.Scheme != "file" {
		return "", fmt.Errorf("not a file uri: %q", uri)
	}
	p := u.Path
	if runtime.GOOS == "windows" {
		if len(p) >= 3 && p[0] == '/' && p[2] == ':' {
			p = p[1:]
		}
		if u.Host != "" && u.Host != "localhost" {
			p = `\\` + u.Host + filepath.FromSlash(p)
		}
	}
	return filepath.Clean(filepath.FromSlash(p)), nil
}

// sameFileURI compares URIs by the paths they name, since servers escape
// URIs differently (for example %3A for a drive colon).
func sameFileURI(a, b string) bool {
	if a == b {
		return true
	}
	pa, err1 := URIToPath(a)
	pb, err2 := URIToPath(b)
	return err1 == nil && err2 == nil && pa == pb
}

// toProtocolChar converts a rune column (0-based) in line to a character
// offset in enc.
func toProtocolChar(line string, runeCol int, enc string) int {
	if runeCol <= 0 {
		return 0
	}
	n, units := 0, 0
	for i, r := range line {
		if n == runeCol {
			if enc == EncodingUTF8 {
				return i
			}
			return units
		}
		switch enc {
		case EncodingUTF8:
		case EncodingUTF32:
			units++
		default:
			units += utf16.RuneLen(r)
		}
		n++
	}
	switch enc {
	case EncodingUTF8:
		return len(line)
	}
	return units
}

// fromProtocolChar converts a character offset in enc to a 0-based rune
// column in line, clamping to the line end.
func fromProtocolChar(line string, char int, enc string) int {
	if char <= 0 {
		return 0
	}
	if enc == EncodingUTF8 {
		if char > len(line) {
			char = len(line)
		}
		return utf8.RuneCountInString(line[:char])
	}
	units, col := 0, 0
	for _, r := range line {
		if units >= char {
			return col
		}
		if enc == EncodingUTF32 {
			units++
		} else {
			units += utf16.RuneLen(r)
		}
		col++
	}
	return col
}

// hoverResult is the result of textDocument/hover.
type hoverResult struct {
	Contents json.RawMessage `json:"contents"`
	Range    *Range          `json:"range,omitempty"`
}

// hoverText flattens MarkupContent, MarkedString or MarkedString[].
func hoverText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var mc struct {
		Kind     string `json:"kind"`
		Language string `json:"language"`
		Value    string `json:"value"`
	}
	if raw[0] == '{' && json.Unmarshal(raw, &mc) == nil {
		if mc.Language != "" {
			return strings.TrimSpace("```" + mc.Language + "\n" + mc.Value + "\n```")
		}
		return strings.TrimSpace(mc.Value)
	}
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) == nil {
		var parts []string
		for _, item := range list {
			if t := hoverText(item); t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

// decodeLocations accepts Location, Location[], LocationLink[] or null.
func decodeLocations(raw json.RawMessage) ([]Location, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	if raw[0] == '{' {
		var l Location
		if err := json.Unmarshal(raw, &l); err != nil {
			return nil, err
		}
		if l.URI == "" {
			var ll LocationLink
			if err := json.Unmarshal(raw, &ll); err == nil && ll.TargetURI != "" {
				return []Location{{URI: ll.TargetURI, Range: ll.TargetSelectionRange}}, nil
			}
			return nil, nil
		}
		return []Location{l}, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	out := make([]Location, 0, len(items))
	for _, it := range items {
		var probe struct {
			URI                  string `json:"uri"`
			TargetURI            string `json:"targetUri"`
			Range                Range  `json:"range"`
			TargetSelectionRange Range  `json:"targetSelectionRange"`
		}
		if err := json.Unmarshal(it, &probe); err != nil {
			return nil, err
		}
		switch {
		case probe.TargetURI != "":
			out = append(out, Location{URI: probe.TargetURI, Range: probe.TargetSelectionRange})
		case probe.URI != "":
			out = append(out, Location{URI: probe.URI, Range: probe.Range})
		}
	}
	return out, nil
}

// languageIDs maps extensions to LSP language identifiers.
var languageIDs = map[string]string{
	".go": "go", ".mod": "go.mod", ".py": "python", ".pyi": "python",
	".ts": "typescript", ".mts": "typescript", ".cts": "typescript", ".tsx": "typescriptreact",
	".js": "javascript", ".mjs": "javascript", ".cjs": "javascript", ".jsx": "javascriptreact",
	".rs": "rust", ".c": "c", ".h": "c", ".cc": "cpp", ".cpp": "cpp", ".cxx": "cpp", ".c++": "cpp",
	".hpp": "cpp", ".hh": "cpp", ".hxx": "cpp", ".java": "java", ".rb": "ruby", ".lua": "lua",
	".zig": "zig", ".swift": "swift", ".kt": "kotlin", ".cs": "csharp", ".php": "php",
}

// languageID returns the language identifier for a path.
func languageID(path string, overrides map[string]string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if id, ok := overrides[ext]; ok {
		return id
	}
	if id, ok := languageIDs[ext]; ok {
		return id
	}
	return strings.TrimPrefix(ext, ".")
}
