package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var skipDirs = map[string]bool{".git": true, "node_modules": true, ".hg": true, ".svn": true, "__pycache__": true, ".venv": true}

// globToRegexp converts a glob with ** support into an anchored regexp.
func globToRegexp(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		ch := pattern[i]
		switch ch {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i++
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '{':
			if j := strings.IndexByte(pattern[i:], '}'); j > 0 {
				alts := strings.Split(pattern[i+1:i+j], ",")
				for k := range alts {
					alts[k] = regexp.QuoteMeta(alts[k])
				}
				b.WriteString("(?:" + strings.Join(alts, "|") + ")")
				i += j
			} else {
				b.WriteString(`\{`)
			}
		case '[':
			if j := strings.IndexByte(pattern[i:], ']'); j > 0 {
				b.WriteString(pattern[i : i+j+1])
				i += j
			} else {
				b.WriteString(`\[`)
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(ch)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

type fileHit struct {
	path  string
	mtime time.Time
}

// walkFiles visits regular files under root, skipping VCS and dependency directories.
func walkFiles(ctx context.Context, root string, fn func(path, rel string, d fs.DirEntry)) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if p != root && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		fn(p, filepath.ToSlash(rel), d)
		return nil
	})
}

func globTool() *Tool {
	return &Tool{
		Name:        "Glob",
		Description: `Finds files by glob pattern, e.g. "**/*.go" or "src/**/*.{ts,tsx}". Returns matching paths, most recently modified first. Skips .git and node_modules.`,
		Properties: map[string]any{
			"pattern": prop("string", "Glob pattern, matched against paths relative to the search directory"),
			"path":    prop("string", "Directory to search (defaults to the working directory)"),
		},
		Required: []string{"pattern"},
		ReadOnly: true,
		Summary:  func(in json.RawMessage, _ *Env) string { return str(in, "pattern") },
		Run: func(ctx context.Context, raw json.RawMessage, env *Env) Result {
			var in struct {
				Pattern string `json:"pattern"`
				Path    string `json:"path"`
			}
			if err := decode(raw, []string{"pattern"}, &in); err != nil {
				return Errorf("%v", err)
			}
			root := env.Abs(in.Path)
			pattern := filepath.ToSlash(in.Pattern)
			if filepath.IsAbs(in.Pattern) {
				// Split an absolute pattern into its literal directory and the glob part.
				i := strings.IndexAny(pattern, "*?[{")
				if i < 0 {
					i = len(pattern)
				}
				j := strings.LastIndexByte(pattern[:i], '/')
				root, pattern = pattern[:j+1], pattern[j+1:]
			}
			// A pattern without a slash matches at any depth, like most glob tools.
			if !strings.Contains(pattern, "/") {
				pattern = "**/" + pattern
			}
			re, err := globToRegexp(pattern)
			if err != nil {
				return Errorf("bad pattern: %v", err)
			}
			var hits []fileHit
			_ = walkFiles(ctx, root, func(p, rel string, d fs.DirEntry) {
				if re.MatchString(rel) {
					h := fileHit{path: p}
					if info, err := d.Info(); err == nil {
						h.mtime = info.ModTime()
					}
					hits = append(hits, h)
				}
			})
			if len(hits) == 0 {
				return Result{Text: "No files found", Display: "No files found"}
			}
			sort.Slice(hits, func(i, j int) bool { return hits[i].mtime.After(hits[j].mtime) })
			const limit = 200
			var sb strings.Builder
			for i, h := range hits {
				if i == limit {
					fmt.Fprintf(&sb, "[%d more files not shown; narrow the pattern]\n", len(hits)-limit)
					break
				}
				sb.WriteString(env.Rel(h.path) + "\n")
			}
			return Result{Text: sb.String(), Display: fmt.Sprintf("Found %d files", len(hits))}
		},
	}
}

type grepInput struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	Glob       string `json:"glob"`
	OutputMode string `json:"output_mode"`
	IgnoreCase bool   `json:"-i"`
	Context    int    `json:"-C"`
	HeadLimit  int    `json:"head_limit"`
}

func grepTool() *Tool {
	return &Tool{
		Name: "Grep",
		Description: `Searches file contents with a regular expression (ripgrep when installed, a built-in engine otherwise).

- output_mode "files_with_matches" (default) lists paths, "content" shows matching lines with line numbers, "count" shows match counts per file.
- Filter files with glob, e.g. "*.go". Use -i for case-insensitive search and -C for context lines in content mode.`,
		Properties: map[string]any{
			"pattern":     prop("string", "Regular expression to search for"),
			"path":        prop("string", "File or directory to search (defaults to the working directory)"),
			"glob":        prop("string", `Only search files matching this glob, e.g. "*.go"`),
			"output_mode": map[string]any{"type": "string", "enum": []string{"files_with_matches", "content", "count"}},
			"-i":          prop("boolean", "Case-insensitive search"),
			"-C":          prop("integer", "Lines of context around each match (content mode)"),
			"head_limit":  prop("integer", "Maximum number of output lines (default 250)"),
		},
		Required: []string{"pattern"},
		ReadOnly: true,
		Summary:  func(in json.RawMessage, _ *Env) string { return str(in, "pattern") },
		Run: func(ctx context.Context, raw json.RawMessage, env *Env) Result {
			var in grepInput
			if err := decode(raw, []string{"pattern"}, &in); err != nil {
				return Errorf("%v", err)
			}
			if in.OutputMode == "" {
				in.OutputMode = "files_with_matches"
			}
			if in.HeadLimit <= 0 {
				in.HeadLimit = 250
			}
			root := env.Abs(in.Path)
			var out string
			var err error
			if rg, lookErr := exec.LookPath("rg"); lookErr == nil && os.Getenv("GOCODE_NO_RG") == "" {
				out, err = grepRipgrep(ctx, rg, in, root)
			} else {
				out, err = grepBuiltin(ctx, in, root)
			}
			if err != nil {
				return Errorf("%v", err)
			}
			lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
			if out == "" || len(lines) == 0 {
				return Result{Text: "No matches found", Display: "No matches found"}
			}
			total := len(lines)
			if total > in.HeadLimit {
				lines = append(lines[:in.HeadLimit], fmt.Sprintf("[%d more lines not shown; narrow the search or raise head_limit]", total-in.HeadLimit))
			}
			for i, l := range lines {
				lines[i] = strings.TrimPrefix(l, env.Cwd+string(filepath.Separator))
			}
			unit := "lines"
			if in.OutputMode == "files_with_matches" {
				unit = "files"
			}
			return Result{Text: truncateMiddle(strings.Join(lines, "\n"), 60000), Display: fmt.Sprintf("Found %d %s", total, unit)}
		},
	}
}

func grepRipgrep(ctx context.Context, rg string, in grepInput, root string) (string, error) {
	args := []string{"--no-heading", "--color=never", "--glob", "!node_modules", "--glob", "!.git"}
	switch in.OutputMode {
	case "files_with_matches":
		args = append(args, "-l")
	case "count":
		args = append(args, "-c")
	default:
		args = append(args, "-n")
		if in.Context > 0 {
			args = append(args, fmt.Sprintf("-C%d", in.Context))
		}
	}
	if in.IgnoreCase {
		args = append(args, "-i")
	}
	if in.Glob != "" {
		args = append(args, "--glob", in.Glob)
	}
	args = append(args, "-e", in.Pattern, root)
	cmd := exec.CommandContext(ctx, rg, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
		return "", nil // no matches
	}
	if err != nil {
		return "", fmt.Errorf("rg failed: %s", strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func grepBuiltin(ctx context.Context, in grepInput, root string) (string, error) {
	expr := in.Pattern
	if in.IgnoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return "", fmt.Errorf("bad regular expression: %v", err)
	}
	var globRe *regexp.Regexp
	if in.Glob != "" {
		g := filepath.ToSlash(in.Glob)
		if !strings.Contains(g, "/") {
			g = "**/" + g
		}
		if globRe, err = globToRegexp(g); err != nil {
			return "", fmt.Errorf("bad glob: %v", err)
		}
	}
	var sb strings.Builder
	search := func(path string) {
		f, err := os.Open(path)
		if err != nil {
			return
		}
		defer f.Close()
		head := make([]byte, 512)
		n, _ := f.Read(head)
		if bytes.IndexByte(head[:n], 0) >= 0 {
			return // binary
		}
		_, _ = f.Seek(0, 0)
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 4<<20)
		var lines []string
		for sc.Scan() {
			lines = append(lines, sc.Text())
		}
		count, lastPrinted := 0, -1
		for i, l := range lines {
			if !re.MatchString(l) {
				continue
			}
			count++
			if in.OutputMode != "content" {
				continue
			}
			from, to := max(i-in.Context, lastPrinted+1), min(i+in.Context, len(lines)-1)
			for j := from; j <= to; j++ {
				sep := "-"
				if j == i || re.MatchString(lines[j]) {
					sep = ":"
				}
				fmt.Fprintf(&sb, "%s%s%d%s%s\n", path, sep, j+1, sep, lines[j])
			}
			lastPrinted = to
		}
		if count == 0 {
			return
		}
		switch in.OutputMode {
		case "files_with_matches":
			sb.WriteString(path + "\n")
		case "count":
			fmt.Fprintf(&sb, "%s:%d\n", path, count)
		}
	}
	if st, err := os.Stat(root); err == nil && !st.IsDir() {
		search(root)
		return sb.String(), nil
	}
	err = walkFiles(ctx, root, func(p, rel string, _ fs.DirEntry) {
		if globRe == nil || globRe.MatchString(rel) {
			search(p)
		}
	})
	return sb.String(), err
}
