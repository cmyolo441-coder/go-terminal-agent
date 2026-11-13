package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

const (
	defaultReadLines = 2000
	maxLineLen       = 2000
)

// ReadFile reads a text file with line numbers.
type ReadFile struct{}

type readInput struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

func (ReadFile) Kind() Kind { return KindRead }

func (ReadFile) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "read_file",
		Description: "Read a text file. Output lines are prefixed with their 1-based line number and a tab. " +
			"Reads up to 2000 lines; use offset and limit for larger files. Read a file before editing or overwriting it.",
		Properties: map[string]any{
			"path":   map[string]any{"type": "string", "description": "File path, absolute or relative to the workspace root."},
			"offset": map[string]any{"type": "integer", "description": "1-based line to start from."},
			"limit":  map[string]any{"type": "integer", "description": "Maximum number of lines to return."},
		},
		Required: []string{"path"},
	}
}

func (ReadFile) Prepare(env *Env, raw json.RawMessage) (*Call, error) {
	var in readInput
	if err := decode(raw, &in); err != nil {
		return nil, err
	}
	if err := required("path", in.Path); err != nil {
		return nil, err
	}
	if in.Offset < 0 || in.Limit < 0 {
		return nil, fmt.Errorf("invalid tool input: offset and limit must be positive")
	}
	abs := env.Resolve(in.Path)
	return &Call{
		Tool:    "read_file",
		Kind:    KindRead,
		Summary: "Read " + env.Rel(abs),
		Path:    abs,
		run: func(ctx context.Context) (string, error) {
			return readFile(env, abs, in.Offset, in.Limit)
		},
	}, nil
}

func readFile(env *Env, abs string, offset, limit int) (string, error) {
	st, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if st.IsDir() {
		return "", fmt.Errorf("%s is a directory; use list_dir", env.Rel(abs))
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", err
	}
	if isBinary(data) {
		return "", fmt.Errorf("%s looks like a binary file (%d bytes)", env.Rel(abs), len(data))
	}
	env.markRead(abs)
	if len(data) == 0 {
		return "(empty file)", nil
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	start := 1
	if offset > 0 {
		start = offset
	}
	if limit <= 0 {
		limit = defaultReadLines
	}
	if start > len(lines) {
		return "", fmt.Errorf("offset %d is past the end of the file (%d lines)", start, len(lines))
	}
	end := start - 1 + limit
	if end > len(lines) {
		end = len(lines)
	}
	var b strings.Builder
	for i := start - 1; i < end; i++ {
		// CRLF files are shown with plain line ends; edit_file matches
		// LF strings against CRLF content.
		line := strings.TrimSuffix(lines[i], "\r")
		if len(line) > maxLineLen {
			cut := maxLineLen
			for cut > 0 && !utf8.RuneStart(line[cut]) {
				cut--
			}
			line = line[:cut] + " …[line truncated]"
		}
		fmt.Fprintf(&b, "%6d\t%s\n", i+1, line)
	}
	if end < len(lines) {
		fmt.Fprintf(&b, "… %d more lines (file has %d). Continue with offset=%d.\n", len(lines)-end, len(lines), end+1)
	}
	return b.String(), nil
}
