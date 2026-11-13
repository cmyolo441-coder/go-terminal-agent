package codeintel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

// Tools returns the code_search, symbols and repo_map tools backed by ix.
func Tools(ix *Index) []tools.Tool {
	return []tools.Tool{SearchTool{Index: ix}, SymbolsTool{Index: ix}, RepoMapTool{Index: ix}}
}

// decodeInput strictly decodes tool input: unknown fields and trailing
// data are rejected so a truncated or malformed stream never runs.
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

// limitOutput keeps the head and tail of long output.
func limitOutput(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	head := limit * 2 / 3
	tail := limit - head
	for head > 0 && (s[head]&0xC0) == 0x80 {
		head--
	}
	start := len(s) - tail
	for start < len(s) && (s[start]&0xC0) == 0x80 {
		start++
	}
	return s[:head] + fmt.Sprintf("\n\n… [%d characters omitted] …\n\n", len(s)-head-(len(s)-start)) + s[start:]
}

func outputLimit(env *tools.Env) int {
	if env != nil && env.OutputLimit > 0 {
		return env.OutputLimit
	}
	return 30000
}

// SearchTool is the code_search tool: ranked semantic search.
type SearchTool struct{ Index *Index }

type searchInput struct {
	Query string `json:"query"`
	Path  string `json:"path"`
	Limit int    `json:"limit"`
}

func (SearchTool) Kind() tools.Kind { return tools.KindRead }

func (SearchTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "code_search",
		Description: "Ranked search over the workspace code index. Accepts natural-language questions " +
			"(\"where is the permission prompt decided\") or identifiers (\"parseConfig\", \"Gate.Check\"). " +
			"Matches split identifiers, doc comments, file paths and code with synonym and abbreviation expansion, " +
			"and returns the best definitions as path:line ranges with snippets. Prefer it over grep to locate code " +
			"by concept; use grep for exact text.",
		Properties: map[string]any{
			"query": map[string]any{"type": "string", "description": "What to find, in words or identifiers."},
			"path":  map[string]any{"type": "string", "description": "Optional glob (\"internal/**/*.go\") or directory prefix to search within."},
			"limit": map[string]any{"type": "integer", "description": "Number of results (default 10, max 50)."},
		},
		Required: []string{"query"},
	}
}

func (t SearchTool) Prepare(env *tools.Env, raw json.RawMessage) (*tools.Call, error) {
	var in searchInput
	if err := decodeInput(raw, &in); err != nil {
		return nil, err
	}
	in.Query = strings.TrimSpace(in.Query)
	if in.Query == "" {
		return nil, fmt.Errorf("invalid tool input: \"query\" is required")
	}
	if len(in.Query) > 1000 {
		return nil, fmt.Errorf("invalid tool input: query is too long")
	}
	if in.Limit < 0 {
		return nil, fmt.Errorf("invalid tool input: limit must be positive")
	}
	if in.Limit == 0 {
		in.Limit = 10
	}
	if in.Limit > 50 {
		in.Limit = 50
	}
	if t.Index == nil {
		return nil, fmt.Errorf("code index is not available")
	}
	path := in.Path
	if path != "" {
		if rel, ok := t.Index.relPath(path); ok && !strings.ContainsAny(path, "*?[{") {
			path = rel
		}
	}
	summary := fmt.Sprintf("Search code for %q", clip(in.Query, 80))
	if path != "" {
		summary += " in " + path
	}
	return tools.NewCall("code_search", tools.KindRead, summary, "", "", func(ctx context.Context) (string, error) {
		hits, err := t.Index.Search(ctx, in.Query, SearchOptions{Path: path, Limit: in.Limit})
		if err != nil {
			return "", err
		}
		return limitOutput(FormatHits(in.Query, hits, t.Index.Stats()), outputLimit(env)), nil
	}), nil
}

// FormatHits renders search hits for the model.
func FormatHits(query string, hits []Hit, st Stats) string {
	var b strings.Builder
	if len(hits) == 0 {
		fmt.Fprintf(&b, "No results for %q in %d indexed files. Try other words, a symbol name, or grep for exact text.\n", query, st.Files)
		return b.String()
	}
	fmt.Fprintf(&b, "%d results for %q (best first; %d files indexed)\n", len(hits), query, st.Files)
	for i, h := range hits {
		b.WriteString("\n")
		fmt.Fprintf(&b, "%d. %s:%d-%d", i+1, h.Path, h.StartLine, h.EndLine)
		if h.Symbol != nil {
			fmt.Fprintf(&b, "  %s %s", h.Symbol.Kind, h.Symbol.QualifiedName())
		}
		if len(h.Matched) > 0 {
			fmt.Fprintf(&b, "  [matched: %s]", strings.Join(h.Matched, ", "))
		}
		b.WriteString("\n")
		if h.Symbol != nil && !snippetShowsDoc(h) {
			if d := h.Symbol.DocSummary(); d != "" {
				fmt.Fprintf(&b, "   // %s\n", d)
			}
		}
		for _, l := range h.Snippet {
			if l.Gap {
				b.WriteString("   …\n")
				continue
			}
			sep := "-"
			if l.Match {
				sep = ":"
			}
			fmt.Fprintf(&b, "   %d%s %s\n", l.Line, sep, l.Text)
		}
	}
	return b.String()
}

// snippetShowsDoc reports whether the snippet already includes the
// symbol's doc comment.
func snippetShowsDoc(h Hit) bool {
	for _, l := range h.Snippet {
		if !l.Gap && l.Line >= h.Symbol.DocLine && l.Line < h.Symbol.Line {
			return true
		}
	}
	return false
}

// SymbolsTool is the symbols tool: find definitions by name.
type SymbolsTool struct{ Index *Index }

type symbolsInput struct {
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	Path         string `json:"path"`
	Limit        int    `json:"limit"`
	ExportedOnly bool   `json:"exported_only"`
}

func (SymbolsTool) Kind() tools.Kind { return tools.KindRead }

func (SymbolsTool) Spec() llm.ToolSpec {
	kinds := make([]string, len(AllKinds))
	for i, k := range AllKinds {
		kinds[i] = string(k)
	}
	return llm.ToolSpec{
		Name: "symbols",
		Description: "Find definitions (functions, methods, types, classes, constants…) by name across the workspace. " +
			"Matches exact names, prefixes, substrings and camel-case abbreviations (\"NewReg\" finds NewRegistry); " +
			"\"Type.Method\" narrows to a container. Returns kind, qualified name, path:line range, signature and doc summary. " +
			"With only path set it lists the definitions in those files.",
		Properties: map[string]any{
			"name":          map[string]any{"type": "string", "description": "Name or part of a name; optionally Container.Name."},
			"kind":          map[string]any{"type": "string", "enum": kinds, "description": "Only this kind of definition."},
			"path":          map[string]any{"type": "string", "description": "Optional glob or directory prefix."},
			"limit":         map[string]any{"type": "integer", "description": "Maximum results (default 30, max 200)."},
			"exported_only": map[string]any{"type": "boolean", "description": "Only exported/public definitions."},
		},
	}
}

func (t SymbolsTool) Prepare(env *tools.Env, raw json.RawMessage) (*tools.Call, error) {
	var in symbolsInput
	if err := decodeInput(raw, &in); err != nil {
		return nil, err
	}
	q := SymbolQuery{Name: strings.TrimSpace(in.Name), Path: in.Path, Limit: in.Limit, ExportedOnly: in.ExportedOnly}
	if in.Kind != "" {
		k, ok := ParseKind(in.Kind)
		if !ok {
			return nil, fmt.Errorf("invalid tool input: unknown kind %q", in.Kind)
		}
		q.Kind = k
	}
	if q.Name == "" && q.Path == "" {
		return nil, fmt.Errorf("invalid tool input: give \"name\" or \"path\"")
	}
	if in.Limit < 0 {
		return nil, fmt.Errorf("invalid tool input: limit must be positive")
	}
	if q.Limit == 0 {
		q.Limit = 30
	}
	if q.Limit > 200 {
		q.Limit = 200
	}
	if t.Index == nil {
		return nil, fmt.Errorf("code index is not available")
	}
	if q.Path != "" {
		if rel, ok := t.Index.relPath(q.Path); ok && !strings.ContainsAny(q.Path, "*?[{") {
			q.Path = rel
		}
	}
	summary := "Find symbols"
	if q.Name != "" {
		summary += fmt.Sprintf(" named %q", clip(q.Name, 60))
	}
	if q.Path != "" {
		summary += " in " + q.Path
	}
	return tools.NewCall("symbols", tools.KindRead, summary, "", "", func(ctx context.Context) (string, error) {
		ms, err := t.Index.Symbols(ctx, q)
		if err != nil {
			return "", err
		}
		return limitOutput(FormatSymbols(q, ms), outputLimit(env)), nil
	}), nil
}

// FormatSymbols renders symbol matches for the model.
func FormatSymbols(q SymbolQuery, ms []SymbolMatch) string {
	if len(ms) == 0 {
		what := q.Name
		if what == "" {
			what = q.Path
		}
		return fmt.Sprintf("No definitions match %q. Try a shorter name or code_search.\n", what)
	}
	if q.Name == "" {
		// A file listing reads best in file order.
		sort.SliceStable(ms, func(i, j int) bool {
			if ms[i].Path != ms[j].Path {
				return ms[i].Path < ms[j].Path
			}
			return ms[i].Line < ms[j].Line
		})
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d definitions\n", len(ms))
	for _, m := range ms {
		fmt.Fprintf(&b, "\n%s %s  %s:%d-%d\n", m.Kind, m.QualifiedName(), m.Path, m.Line, m.EndLine)
		if m.Signature != "" {
			fmt.Fprintf(&b, "    %s\n", clip(m.Signature, 200))
		}
		if d := m.DocSummary(); d != "" {
			fmt.Fprintf(&b, "    // %s\n", d)
		}
	}
	return b.String()
}

// RepoMapTool is the repo_map tool: a ranked outline of the codebase.
type RepoMapTool struct{ Index *Index }

type repoMapInput struct {
	Budget int      `json:"budget"`
	Focus  []string `json:"focus"`
}

func (RepoMapTool) Kind() tools.Kind { return tools.KindRead }

func (RepoMapTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "repo_map",
		Description: "Outline of the most important files and their key definitions, ranked by how much the rest of " +
			"the code references them, within a token budget. Use it to orient in an unfamiliar codebase; pass focus " +
			"paths to center the map on the area you are working in.",
		Properties: map[string]any{
			"budget": map[string]any{"type": "integer", "description": "Approximate size in tokens (default 2048, max 16384)."},
			"focus": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Files, directories or globs to center the map on.",
			},
		},
	}
}

func (t RepoMapTool) Prepare(env *tools.Env, raw json.RawMessage) (*tools.Call, error) {
	var in repoMapInput
	if err := decodeInput(raw, &in); err != nil {
		return nil, err
	}
	if in.Budget < 0 {
		return nil, fmt.Errorf("invalid tool input: budget must be positive")
	}
	if in.Budget == 0 {
		in.Budget = defaultMapBudget
	}
	if in.Budget > 16384 {
		in.Budget = 16384
	}
	if len(in.Focus) > 50 {
		return nil, fmt.Errorf("invalid tool input: at most 50 focus paths")
	}
	if t.Index == nil {
		return nil, fmt.Errorf("code index is not available")
	}
	summary := fmt.Sprintf("Map the repository (~%d tokens)", in.Budget)
	return tools.NewCall("repo_map", tools.KindRead, summary, "", "", func(ctx context.Context) (string, error) {
		m, err := t.Index.RepoMap(ctx, RepoMapOptions{Budget: in.Budget, Focus: in.Focus})
		if err != nil {
			return "", err
		}
		return limitOutput(m, outputLimit(env)), nil
	}), nil
}
