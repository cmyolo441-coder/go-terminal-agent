package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/agent"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/codeintel"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/config"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/lsp"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/ui"
)

// repoMapWait bounds how long startup waits for the first index scan
// before it gives up on the repository map for this session.
const repoMapWait = 4 * time.Second

// codeIntel holds the workspace code index and the language servers. Both
// are optional; a nil part is simply left out.
type codeIntel struct {
	ix     *codeintel.Index
	lm     *lsp.Manager
	budget int
	// mapTokens is the approximate size of the repository map added to
	// the system prompt, or 0 when there is none.
	mapTokens int
	notes     []string
}

// startCodeIntel opens the index and the language server manager that cfg
// asks for. Failures turn the part off with a note; they never stop GTA.
func startCodeIntel(cfg config.Config, root string) *codeIntel {
	c := &codeIntel{budget: cfg.RepoMapBudget}
	if cfg.CodeIndex {
		ix, err := codeintel.Open(codeintel.Options{
			Root:        root,
			CacheDir:    filepath.Join(config.UserDir(), "index"),
			MaxFileSize: cfg.CodeIndexMaxFileSize,
			ExtraIgnore: cfg.CodeIndexIgnore,
		})
		if err != nil {
			c.notes = append(c.notes, "code index off: "+err.Error())
		} else {
			c.ix = ix
		}
	}
	if cfg.LSP {
		wait := 4 * time.Second
		if cfg.LSPDiagnosticsWaitMs > 0 {
			wait = time.Duration(cfg.LSPDiagnosticsWaitMs) * time.Millisecond
		}
		lm, err := lsp.NewManager(lsp.Options{Root: root, Servers: cfg.LSPServers, DiagnosticsWait: wait})
		if err != nil {
			c.notes = append(c.notes, "language servers off: "+err.Error())
		} else {
			c.lm = lm
		}
	}
	return c
}

// tools are the model-facing tools of the parts that are on.
func (c *codeIntel) tools() []tools.Tool {
	var out []tools.Tool
	if c.ix != nil {
		out = append(out, codeintel.Tools(c.ix)...)
	}
	if c.lm != nil {
		out = append(out, lsp.Tools(c.lm)...)
	}
	return out
}

// systemPrompt returns the repository map block for the system prompt,
// building the index first within repoMapWait. It returns "" when the map
// is off, the workspace is empty or the scan takes too long.
func (c *codeIntel) systemPrompt() string {
	if c.ix == nil || c.budget <= 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), repoMapWait)
	defer cancel()
	m, err := c.ix.RepoMap(ctx, codeintel.RepoMapOptions{Budget: c.budget})
	if err != nil || strings.TrimSpace(m) == "" {
		if err != nil && ctx.Err() != nil {
			c.notes = append(c.notes, "repository map skipped: indexing took longer than "+repoMapWait.String())
			// Keep indexing in the background for the tools.
			go func() { _, _ = c.ix.Refresh(context.Background()) }()
		}
		return ""
	}
	c.mapTokens = len(m) / 4
	return "<repo-map>\nA ranked map of this repository's most important files and symbols, from GTA's code index. Use the code_search, symbols and repo_map tools to look further.\n\n" +
		strings.TrimSpace(m) + "\n</repo-map>"
}

// warm starts the language server for the workspace's main language in
// the background, so the first edit does not wait for it.
func (c *codeIntel) warm(root string) {
	if c.lm == nil {
		return
	}
	if c.ix != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		files, err := c.ix.RankedFiles(ctx, 20)
		cancel()
		if err == nil {
			for _, f := range files {
				abs := filepath.Join(root, f)
				if c.lm.Available(abs) {
					c.lm.Warm(abs)
					return
				}
			}
		}
	}
	for _, f := range []string{"main.go", "go.mod", "package.json", "pyproject.toml", "Cargo.toml"} {
		if _, err := os.Stat(filepath.Join(root, f)); err == nil && c.lm.Available(filepath.Join(root, f)) {
			c.lm.Warm(filepath.Join(root, f))
			return
		}
	}
}

// afterWrite keeps the index current and returns any new compiler errors
// for the edited file, for the model to fix in the same turn.
func (c *codeIntel) afterWrite(ctx context.Context, call *tools.Call) string {
	if call.Path == "" {
		return ""
	}
	if c.ix != nil {
		c.ix.Invalidate(call.Path)
	}
	if c.lm == nil || call.OutsideRoot {
		return ""
	}
	return c.lm.AfterEdit(ctx, call.Path)
}

// attach hooks the index and diagnostics into ag's file writes.
func (c *codeIntel) attach(ag *agent.Agent) {
	if c.ix == nil && c.lm == nil {
		return
	}
	ag.AfterWrite = c.afterWrite
}

// close stops the language servers and saves the index.
func (c *codeIntel) close() {
	if c.lm != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = c.lm.Close(ctx)
		cancel()
	}
	if c.ix != nil {
		_ = c.ix.Close()
	}
}

// capabilities are the /status rows for the code index and LSP.
func (c *codeIntel) capabilities() []ui.Capability {
	var out []ui.Capability
	add := func(name, value string, state ui.CapState, banner bool) {
		out = append(out, ui.Capability{Group: "Code", Name: name, Value: value, State: state, Banner: banner})
	}
	if c.ix == nil {
		add("index", "off", ui.CapOff, false)
	} else {
		st := c.ix.Stats()
		v := "indexing…"
		if !st.LastRefresh.IsZero() {
			v = fmt.Sprintf("%d files · %d symbols · %s", st.Files, st.Symbols, topLanguages(st.Languages, 3))
		}
		add("index", v, ui.CapOn, true)
		if c.mapTokens > 0 {
			add("repo map", fmt.Sprintf("~%d tokens in the system prompt", c.mapTokens), ui.CapOn, false)
		} else {
			add("repo map", "off", ui.CapOff, false)
		}
	}
	if c.lm == nil {
		add("lsp", "off", ui.CapOff, false)
		return out
	}
	status := c.lm.Status()
	if len(status) == 0 {
		add("lsp", "on · starts with the first edit ("+strings.Join(installedServers(c.lm), ", ")+")", ui.CapOn, true)
		return out
	}
	var parts []string
	state := ui.CapOn
	for _, s := range status {
		parts = append(parts, s.Name+" "+s.State)
		if s.State == "failed" || s.State == "crashed" {
			state = ui.CapWarn
		}
	}
	add("lsp", strings.Join(parts, ", "), state, true)
	return out
}

// installedServers names the configured language servers found on PATH.
func installedServers(m *lsp.Manager) []string {
	seen := map[string]bool{}
	var out []string
	for _, ext := range []string{".go", ".ts", ".js", ".py", ".rs", ".c", ".cpp"} {
		f := filepath.Join(m.Root(), "x"+ext)
		if cfg, ok := m.ServerFor(f); ok && !seen[cfg.Name] && m.Available(f) {
			seen[cfg.Name] = true
			out = append(out, cfg.Name)
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		return []string{"none on PATH"}
	}
	return out
}

// topLanguages lists the n most common languages of an index.
func topLanguages(langs map[string]int, n int) string {
	type kv struct {
		k string
		v int
	}
	var all []kv
	for k, v := range langs {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].v != all[j].v {
			return all[i].v > all[j].v
		}
		return all[i].k < all[j].k
	})
	var names []string
	for i := 0; i < len(all) && i < n; i++ {
		names = append(names, all[i].k)
	}
	if len(names) == 0 {
		return "no source files"
	}
	return strings.Join(names, ", ")
}

// registerCodeCommands adds /index and /lsp.
func registerCodeCommands(c *codeIntel) {
	ui.Register(ui.Extra{Name: "index", Args: "[refresh]", Help: "Show the code index, or re-scan the workspace", Run: func(ctx context.Context, arg string) (string, error) {
		if c.ix == nil {
			return "The code index is off (`\"code_index\": false`).", nil
		}
		var st codeintel.Stats
		switch strings.TrimSpace(arg) {
		case "":
			st = c.ix.Stats()
			if st.LastRefresh.IsZero() {
				var err error
				if st, err = c.ix.Refresh(ctx); err != nil {
					return "", err
				}
			}
		case "refresh":
			var err error
			if st, err = c.ix.Refresh(ctx); err != nil {
				return "", err
			}
		default:
			return "", fmt.Errorf("usage: /index [refresh]")
		}
		var b strings.Builder
		fmt.Fprintf(&b, "**Code index** · %d files · %d symbols · %d chunks\n\n", st.Files, st.Symbols, st.Chunks)
		fmt.Fprintf(&b, "- Languages: %s\n", topLanguages(st.Languages, 6))
		fmt.Fprintf(&b, "- Last scan: %d parsed, %d reused, %d removed in %s\n", st.Parsed, st.Reused, st.Removed, st.Duration.Round(time.Millisecond))
		if st.Truncated {
			b.WriteString("- The workspace has more files than the index cap; some were left out.\n")
		}
		if c.mapTokens > 0 {
			fmt.Fprintf(&b, "- Repository map: ~%d tokens in the system prompt\n", c.mapTokens)
		}
		b.WriteString("\nThe model searches it with `code_search`, `symbols` and `repo_map`.")
		return b.String(), nil
	}})
	ui.Register(ui.Extra{Name: "lsp", Help: "Show the language servers", Run: func(ctx context.Context, arg string) (string, error) {
		if c.lm == nil {
			return "Language servers are off (`\"lsp\": false`).", nil
		}
		status := c.lm.Status()
		var b strings.Builder
		b.WriteString("**Language servers**\n\n")
		if len(status) == 0 {
			fmt.Fprintf(&b, "None running yet. Installed: %s. A server starts when a file it handles is edited or inspected.\n", strings.Join(installedServers(c.lm), ", "))
			return b.String(), nil
		}
		for _, s := range status {
			fmt.Fprintf(&b, "- **%s**: %s", s.Name, s.State)
			if s.PID > 0 {
				fmt.Fprintf(&b, " · pid %d · %d open files", s.PID, s.OpenDocs)
			}
			if s.Crashes > 0 {
				fmt.Fprintf(&b, " · %d crashes", s.Crashes)
			}
			if s.LastError != "" {
				fmt.Fprintf(&b, " · %s", s.LastError)
			}
			b.WriteString("\n")
		}
		b.WriteString("\nAfter each edit, new errors are sent back to the model so it can fix them right away.")
		return b.String(), nil
	}})
}
