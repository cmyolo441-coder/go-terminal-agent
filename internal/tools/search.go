package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

const (
	maxGlobResults = 500
	maxGrepFile    = 2 << 20
)

// ListDir lists a directory.
type ListDir struct{}

type listInput struct {
	Path  string `json:"path"`
	Depth int    `json:"depth"`
}

func (ListDir) Kind() Kind { return KindRead }

func (ListDir) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "list_dir",
		Description: "List a directory as a tree (default depth 2, max 5). Skips .git, node_modules and other build or vendor directories.",
		Properties: map[string]any{
			"path":  map[string]any{"type": "string", "description": "Directory path; defaults to the workspace root."},
			"depth": map[string]any{"type": "integer", "description": "How many levels to descend (1-5)."},
		},
	}
}

func (ListDir) Prepare(env *Env, raw json.RawMessage) (*Call, error) {
	var in listInput
	if err := decode(raw, &in); err != nil {
		return nil, err
	}
	if in.Path == "" {
		in.Path = "."
	}
	if in.Depth <= 0 {
		in.Depth = 2
	}
	if in.Depth > 5 {
		in.Depth = 5
	}
	abs := env.Resolve(in.Path)
	return &Call{Tool: "list_dir", Kind: KindRead, Summary: "List " + env.Rel(abs), run: func(ctx context.Context) (string, error) {
		var b strings.Builder
		count := 0
		var walk func(dir, indent string, depth int) error
		walk = func(dir, indent string, depth int) error {
			entries, err := os.ReadDir(dir)
			if err != nil {
				return err
			}
			sort.Slice(entries, func(i, j int) bool {
				if entries[i].IsDir() != entries[j].IsDir() {
					return entries[i].IsDir()
				}
				return entries[i].Name() < entries[j].Name()
			})
			for _, e := range entries {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if count >= 1000 {
					b.WriteString(indent + "… (listing truncated)\n")
					return nil
				}
				count++
				name := e.Name()
				if e.IsDir() {
					b.WriteString(indent + name + "/\n")
					if skipDir[name] {
						continue
					}
					if depth > 1 {
						if err := walk(filepath.Join(dir, name), indent+"  ", depth-1); err != nil {
							return err
						}
					}
					continue
				}
				b.WriteString(indent + name + "\n")
			}
			return nil
		}
		if err := walk(abs, "", in.Depth); err != nil {
			return "", err
		}
		if b.Len() == 0 {
			return "(empty directory)", nil
		}
		return b.String(), nil
	}}, nil
}

// Glob finds files by pattern.
type Glob struct{}

type globInput struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}

func (Glob) Kind() Kind { return KindRead }

func (Glob) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "glob",
		Description: "Find files by glob pattern such as \"**/*.go\" or \"src/**/test_*.py\". Results are sorted newest first, up to 500.",
		Properties: map[string]any{
			"pattern": map[string]any{"type": "string", "description": "Glob pattern; ** matches across directories."},
			"path":    map[string]any{"type": "string", "description": "Directory to search; defaults to the workspace root."},
		},
		Required: []string{"pattern"},
	}
}

func (Glob) Prepare(env *Env, raw json.RawMessage) (*Call, error) {
	var in globInput
	if err := decode(raw, &in); err != nil {
		return nil, err
	}
	if err := required("pattern", in.Pattern); err != nil {
		return nil, err
	}
	if !doublestar.ValidatePattern(in.Pattern) {
		return nil, fmt.Errorf("invalid glob pattern %q", in.Pattern)
	}
	if in.Path == "" {
		in.Path = "."
	}
	base := env.Resolve(in.Path)
	return &Call{Tool: "glob", Kind: KindRead, Summary: fmt.Sprintf("Glob %s", in.Pattern), run: func(ctx context.Context) (string, error) {
		type hit struct {
			rel string
			mod int64
		}
		var hits []hit
		err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if d.IsDir() {
				if p != base && skipDir[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			rel, _ := filepath.Rel(base, p)
			rel = filepath.ToSlash(rel)
			if ok, _ := doublestar.Match(in.Pattern, rel); ok {
				var mod int64
				if info, err := d.Info(); err == nil {
					mod = info.ModTime().UnixNano()
				}
				hits = append(hits, hit{rel, mod})
			}
			return nil
		})
		if err != nil {
			return "", err
		}
		if len(hits) == 0 {
			return "no files matched", nil
		}
		sort.Slice(hits, func(i, j int) bool { return hits[i].mod > hits[j].mod })
		var b strings.Builder
		for i, h := range hits {
			if i == maxGlobResults {
				fmt.Fprintf(&b, "… %d more matches; narrow the pattern\n", len(hits)-maxGlobResults)
				break
			}
			b.WriteString(env.Rel(filepath.Join(base, h.rel)) + "\n")
		}
		return b.String(), nil
	}}, nil
}

// Grep searches file contents with a regular expression.
type Grep struct{}

type grepInput struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	Include    string `json:"include"`
	IgnoreCase bool   `json:"ignore_case"`
	Context    int    `json:"context"`
	OutputMode string `json:"output_mode"`
	MaxResults int    `json:"max_results"`
}

func (Grep) Kind() Kind { return KindRead }

func (Grep) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "grep",
		Description: "Search file contents with a Go (RE2) regular expression. Skips binary files, files over 2 MB and vendor/build directories. " +
			"output_mode is \"content\" (matching lines with file:line, the default), \"files\" (matching file paths) or \"count\".",
		Properties: map[string]any{
			"pattern":     map[string]any{"type": "string", "description": "RE2 regular expression."},
			"path":        map[string]any{"type": "string", "description": "File or directory to search; defaults to the workspace root."},
			"include":     map[string]any{"type": "string", "description": "Glob filter on file paths, e.g. \"**/*.go\"."},
			"ignore_case": map[string]any{"type": "boolean"},
			"context":     map[string]any{"type": "integer", "description": "Lines of context around each match (content mode, max 10)."},
			"output_mode": map[string]any{"type": "string", "enum": []string{"content", "files", "count"}},
			"max_results": map[string]any{"type": "integer", "description": "Maximum matches or files to return (default 200)."},
		},
		Required: []string{"pattern"},
	}
}

func (Grep) Prepare(env *Env, raw json.RawMessage) (*Call, error) {
	var in grepInput
	if err := decode(raw, &in); err != nil {
		return nil, err
	}
	if err := required("pattern", in.Pattern); err != nil {
		return nil, err
	}
	expr := in.Pattern
	if in.IgnoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, fmt.Errorf("invalid regular expression: %w", err)
	}
	if in.Include != "" && !doublestar.ValidatePattern(in.Include) {
		return nil, fmt.Errorf("invalid include pattern %q", in.Include)
	}
	switch in.OutputMode {
	case "":
		in.OutputMode = "content"
	case "content", "files", "count":
	default:
		return nil, fmt.Errorf("invalid output_mode %q", in.OutputMode)
	}
	if in.MaxResults <= 0 {
		in.MaxResults = 200
	}
	if in.Context < 0 {
		in.Context = 0
	}
	if in.Context > 10 {
		in.Context = 10
	}
	if in.Path == "" {
		in.Path = "."
	}
	base := env.Resolve(in.Path)
	return &Call{Tool: "grep", Kind: KindRead, Summary: fmt.Sprintf("Grep %q", in.Pattern), run: func(ctx context.Context) (string, error) {
		return grep(ctx, env, base, re, in)
	}}, nil
}

func grep(ctx context.Context, env *Env, base string, re *regexp.Regexp, in grepInput) (string, error) {
	var out strings.Builder
	results, files, total := 0, 0, 0
	limited := false
	searchFile := func(p string) error {
		info, err := os.Stat(p)
		if err != nil || info.Size() > maxGrepFile {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil || isBinary(data) {
			return nil
		}
		var lines []string
		sc := bufio.NewScanner(strings.NewReader(string(data)))
		sc.Buffer(make([]byte, 1024*1024), maxGrepFile)
		for sc.Scan() {
			lines = append(lines, sc.Text())
		}
		var matched []int
		for i, l := range lines {
			if re.MatchString(l) {
				matched = append(matched, i)
			}
		}
		if len(matched) == 0 {
			return nil
		}
		rel := env.Rel(p)
		files++
		total += len(matched)
		switch in.OutputMode {
		case "files":
			if results >= in.MaxResults {
				limited = true
				return filepath.SkipAll
			}
			results++
			out.WriteString(rel + "\n")
		case "count":
			fmt.Fprintf(&out, "%s:%d\n", rel, len(matched))
		default:
			last := -1
			for _, m := range matched {
				if results >= in.MaxResults {
					limited = true
					return filepath.SkipAll
				}
				results++
				lo, hi := m-in.Context, m+in.Context
				if lo < 0 {
					lo = 0
				}
				if hi >= len(lines) {
					hi = len(lines) - 1
				}
				if in.Context > 0 && last >= 0 && lo > last+1 {
					out.WriteString("--\n")
				}
				if lo <= last {
					lo = last + 1
				}
				for i := lo; i <= hi; i++ {
					sep := "-"
					if i == m {
						sep = ":"
					}
					line := lines[i]
					if len(line) > 500 {
						line = line[:500] + "…"
					}
					fmt.Fprintf(&out, "%s%s%d%s%s\n", rel, sep, i+1, sep, line)
				}
				last = hi
			}
		}
		return nil
	}
	st, err := os.Stat(base)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		if err := searchFile(base); err != nil && err != filepath.SkipAll {
			return "", err
		}
	} else {
		err = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if d.IsDir() {
				if p != base && skipDir[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			if in.Include != "" {
				rel, _ := filepath.Rel(base, p)
				rel = filepath.ToSlash(rel)
				okRel, _ := doublestar.Match(in.Include, rel)
				okBase, _ := doublestar.Match(in.Include, d.Name())
				if !okRel && !okBase {
					return nil
				}
			}
			return searchFile(p)
		})
		if err != nil && err != filepath.SkipAll {
			return "", err
		}
	}
	if files == 0 {
		return "no matches", nil
	}
	if limited {
		fmt.Fprintf(&out, "… results limited to %d; narrow the search\n", in.MaxResults)
	}
	if in.OutputMode == "count" {
		fmt.Fprintf(&out, "total: %d matches in %d files\n", total, files)
	}
	return truncate(out.String(), env.OutputLimit), nil
}
