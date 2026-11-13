package lsp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

// Tools returns the diagnostics and code_nav tools backed by m.
func Tools(m *Manager) []tools.Tool {
	return []tools.Tool{DiagnosticsTool{Manager: m}, NavTool{Manager: m}}
}

// limitOutput keeps the head and tail of s within limit bytes, cutting on
// rune boundaries.
func limitOutput(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	head := limit * 2 / 3
	tail := limit - head
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	start := len(s) - tail
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[:head] + fmt.Sprintf("\n… [%d bytes omitted] …\n", start-head) + s[start:]
}

func outputLimit(env *tools.Env) int {
	if env != nil && env.OutputLimit > 0 {
		return env.OutputLimit
	}
	return 30000
}

func decodeInput(input json.RawMessage, v any) error {
	if len(bytes.TrimSpace(input)) == 0 {
		input = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid tool input: %w", err)
	}
	if dec.More() {
		return fmt.Errorf("invalid tool input: trailing data")
	}
	return nil
}

// DiagnosticsTool reports compiler and linter diagnostics from the
// language server for files.
type DiagnosticsTool struct{ Manager *Manager }

type diagnosticsInput struct {
	Path     string   `json:"path"`
	Paths    []string `json:"paths"`
	Severity string   `json:"severity"`
	WaitMS   int      `json:"wait_ms"`
}

const maxDiagnosticFiles = 20

func (DiagnosticsTool) Kind() tools.Kind { return tools.KindRead }

func (DiagnosticsTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "diagnostics",
		Description: "Compiler, type-checker and linter diagnostics for files from the language server (gopls, " +
			"typescript-language-server, pyright, rust-analyzer, clangd), computed on the current file content. " +
			"Use it to check a file compiles after changes without running a build.",
		Properties: map[string]any{
			"path":     map[string]any{"type": "string", "description": "File to check."},
			"paths":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Several files to check (max 20)."},
			"severity": map[string]any{"type": "string", "enum": []string{"error", "warning", "info", "hint"}, "description": "Lowest severity to report (default warning)."},
			"wait_ms":  map[string]any{"type": "integer", "description": "How long to wait for the server's analysis, in milliseconds (default 5000, max 60000)."},
		},
	}
}

func (t DiagnosticsTool) Prepare(env *tools.Env, raw json.RawMessage) (*tools.Call, error) {
	var in diagnosticsInput
	if err := decodeInput(raw, &in); err != nil {
		return nil, err
	}
	paths := append([]string(nil), in.Paths...)
	if strings.TrimSpace(in.Path) != "" {
		paths = append([]string{in.Path}, paths...)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("invalid tool input: \"path\" or \"paths\" is required")
	}
	if len(paths) > maxDiagnosticFiles {
		return nil, fmt.Errorf("invalid tool input: at most %d files", maxDiagnosticFiles)
	}
	min := SeverityWarning
	if in.Severity != "" {
		s, ok := ParseSeverity(in.Severity)
		if !ok {
			return nil, fmt.Errorf("invalid tool input: unknown severity %q", in.Severity)
		}
		min = s
	}
	if in.WaitMS < 0 {
		return nil, fmt.Errorf("invalid tool input: wait_ms must be positive")
	}
	wait := 5 * time.Second
	if in.WaitMS > 0 {
		wait = time.Duration(min64(int64(in.WaitMS), 60000)) * time.Millisecond
	}
	if t.Manager == nil {
		return nil, fmt.Errorf("language servers are not available")
	}
	abs := make([]string, len(paths))
	for i, p := range paths {
		if strings.TrimSpace(p) == "" {
			return nil, fmt.Errorf("invalid tool input: empty path")
		}
		abs[i] = env.Resolve(p)
		if st, err := os.Stat(abs[i]); err == nil && st.IsDir() {
			return nil, fmt.Errorf("%s is a directory; give files", env.Rel(abs[i]))
		}
	}
	summary := "Diagnostics for " + env.Rel(abs[0])
	if len(abs) > 1 {
		summary = fmt.Sprintf("Diagnostics for %d files", len(abs))
	}
	return tools.NewCall("diagnostics", tools.KindRead, summary, "", "", func(ctx context.Context) (string, error) {
		var b strings.Builder
		for i, p := range abs {
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString(t.Manager.describeDiagnostics(ctx, env.Rel(p), p, wait, min))
			if ctx.Err() != nil {
				return limitOutput(b.String(), outputLimit(env)), ctx.Err()
			}
		}
		return limitOutput(b.String(), outputLimit(env)), nil
	}), nil
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// describeDiagnostics renders one file's diagnostics for the tool.
func (m *Manager) describeDiagnostics(ctx context.Context, rel, abs string, wait time.Duration, min Severity) string {
	res, err := m.Diagnostics(ctx, abs, wait)
	switch {
	case errors.Is(err, ErrNoServer):
		return fmt.Sprintf("%s: no language server is configured for this file type.\n", rel)
	case errors.Is(err, ErrUnavailable):
		return fmt.Sprintf("%s: %v. Install it or configure another server.\n", rel, strings.TrimPrefix(err.Error(), "lsp: "))
	case err != nil:
		return fmt.Sprintf("%s: diagnostics unavailable: %v\n", rel, err)
	}
	var kept []Diagnostic
	counts := map[Severity]int{}
	for _, d := range res.Diagnostics {
		sev := d.EffectiveSeverity()
		counts[sev]++
		if sev <= min {
			kept = append(kept, d)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s): %s", rel, res.Server, countSummary(counts))
	if !res.Fresh {
		fmt.Fprintf(&b, " — the server had not finished analysing the current content after %s; results may be stale or incomplete", wait)
	}
	b.WriteString("\n")
	if len(kept) > 0 {
		shown := *res
		shown.Diagnostics = kept
		b.WriteString(FormatDiagnostics(&shown, 100))
	}
	return b.String()
}

func countSummary(c map[Severity]int) string {
	var parts []string
	for _, s := range []Severity{SeverityError, SeverityWarning, SeverityInformation, SeverityHint} {
		n := c[s]
		if n == 0 {
			continue
		}
		name := s.String()
		if n > 1 && s != SeverityInformation {
			name += "s"
		}
		parts = append(parts, fmt.Sprintf("%d %s", n, name))
	}
	if len(parts) == 0 {
		return "no problems"
	}
	return strings.Join(parts, ", ")
}

// NavTool answers definition, references and hover questions.
type NavTool struct{ Manager *Manager }

type navInput struct {
	Operation          string `json:"operation"`
	Path               string `json:"path"`
	Line               int    `json:"line"`
	Column             int    `json:"column"`
	Symbol             string `json:"symbol"`
	IncludeDeclaration *bool  `json:"include_declaration"`
	Limit              int    `json:"limit"`
}

func (NavTool) Kind() tools.Kind { return tools.KindRead }

func (NavTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "code_nav",
		Description: "Precise code navigation from the language server: \"definition\" (where a symbol is defined), " +
			"\"references\" (every use of it) or \"hover\" (its type and documentation). Point at the symbol with " +
			"path, line and either the symbol text on that line or a 1-based column.",
		Properties: map[string]any{
			"operation":           map[string]any{"type": "string", "enum": []string{"definition", "references", "hover"}},
			"path":                map[string]any{"type": "string", "description": "File containing the symbol."},
			"line":                map[string]any{"type": "integer", "description": "1-based line number."},
			"symbol":              map[string]any{"type": "string", "description": "The identifier on that line (its first occurrence is used)."},
			"column":              map[string]any{"type": "integer", "description": "1-based column, instead of symbol."},
			"include_declaration": map[string]any{"type": "boolean", "description": "references: include the declaration (default true)."},
			"limit":               map[string]any{"type": "integer", "description": "Maximum locations (default 100)."},
		},
		Required: []string{"operation", "path", "line"},
	}
}

func (t NavTool) Prepare(env *tools.Env, raw json.RawMessage) (*tools.Call, error) {
	var in navInput
	if err := decodeInput(raw, &in); err != nil {
		return nil, err
	}
	switch in.Operation {
	case "definition", "references", "hover":
	default:
		return nil, fmt.Errorf("invalid tool input: operation must be definition, references or hover")
	}
	if strings.TrimSpace(in.Path) == "" {
		return nil, fmt.Errorf("invalid tool input: \"path\" is required")
	}
	if in.Line < 1 {
		return nil, fmt.Errorf("invalid tool input: \"line\" must be 1 or more")
	}
	if in.Column < 0 {
		return nil, fmt.Errorf("invalid tool input: \"column\" must be positive")
	}
	if in.Column == 0 && strings.TrimSpace(in.Symbol) == "" {
		return nil, fmt.Errorf("invalid tool input: give \"symbol\" or \"column\"")
	}
	if in.Limit <= 0 {
		in.Limit = 100
	}
	if t.Manager == nil {
		return nil, fmt.Errorf("language servers are not available")
	}
	abs := env.Resolve(in.Path)
	what := in.Symbol
	if what == "" {
		what = fmt.Sprintf("column %d", in.Column)
	}
	summary := fmt.Sprintf("%s of %s at %s:%d", strings.ToUpper(in.Operation[:1])+in.Operation[1:], what, env.Rel(abs), in.Line)
	return tools.NewCall("code_nav", tools.KindRead, summary, "", "", func(ctx context.Context) (string, error) {
		out, err := t.run(ctx, in, abs)
		return limitOutput(out, outputLimit(env)), err
	}), nil
}

func (t NavTool) run(ctx context.Context, in navInput, abs string) (string, error) {
	col := in.Column
	if col == 0 {
		c, err := symbolColumn(abs, in.Line, in.Symbol)
		if err != nil {
			return "", err
		}
		col = c
	}
	switch in.Operation {
	case "hover":
		text, err := t.Manager.Hover(ctx, abs, in.Line, col)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(text) == "" {
			return "No hover information at that position.", nil
		}
		return text, nil
	case "definition":
		locs, err := t.Manager.Definition(ctx, abs, in.Line, col)
		if err != nil {
			return "", err
		}
		return FormatLocations(t.Manager.root, locs, t.Manager.encodingFor, in.Limit), nil
	default:
		include := true
		if in.IncludeDeclaration != nil {
			include = *in.IncludeDeclaration
		}
		locs, err := t.Manager.References(ctx, abs, in.Line, col, include)
		if err != nil {
			return "", err
		}
		header := fmt.Sprintf("%d references\n", len(locs))
		return header + FormatLocations(t.Manager.root, locs, t.Manager.encodingFor, in.Limit), nil
	}
}

// symbolColumn finds the 1-based rune column of the first whole-word
// occurrence of symbol on a 1-based line of a file.
func symbolColumn(path string, line int, symbol string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	lines := splitLines(string(data))
	if line > len(lines) {
		return 0, fmt.Errorf("line %d is past the end of the file (%d lines)", line, len(lines))
	}
	text := lines[line-1]
	symbol = strings.TrimSpace(symbol)
	// Accept qualified names: point at the last component.
	if i := strings.LastIndexAny(symbol, ".:"); i >= 0 && i+1 < len(symbol) {
		symbol = symbol[i+1:]
	}
	from := 0
	for {
		i := strings.Index(text[from:], symbol)
		if i < 0 {
			break
		}
		i += from
		before, _ := utf8.DecodeLastRuneInString(text[:i])
		after, _ := utf8.DecodeRuneInString(text[i+len(symbol):])
		if !isWordRune(before) && !isWordRune(after) {
			return utf8.RuneCountInString(text[:i]) + 1, nil
		}
		from = i + len(symbol)
	}
	return 0, fmt.Errorf("%q does not occur on line %d: %s", symbol, line, clipLine(strings.TrimSpace(text), 160))
}

func isWordRune(r rune) bool {
	return r != utf8.RuneError && (r == '_' || r == '$' || unicode.IsLetter(r) || unicode.IsDigit(r))
}
