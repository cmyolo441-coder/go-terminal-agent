package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

// EditFile replaces exact text in a file.
type EditFile struct{}

type editInput struct {
	Path       string  `json:"path"`
	OldString  *string `json:"old_string"`
	NewString  *string `json:"new_string"`
	ReplaceAll bool    `json:"replace_all"`
}

func (EditFile) Kind() Kind { return KindWrite }

func (EditFile) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "edit_file",
		Description: "Replace an exact string in a file. old_string must match the file exactly, including whitespace, " +
			"and must be unique unless replace_all is true; include surrounding lines to make it unique. " +
			"The file must have been read first. Line-number prefixes from read_file are not part of the file.",
		Properties: map[string]any{
			"path":        map[string]any{"type": "string", "description": "File path, absolute or relative to the workspace root."},
			"old_string":  map[string]any{"type": "string", "description": "Exact text to replace."},
			"new_string":  map[string]any{"type": "string", "description": "Replacement text."},
			"replace_all": map[string]any{"type": "boolean", "description": "Replace every occurrence (default false)."},
		},
		Required: []string{"path", "old_string", "new_string"},
	}
}

func (EditFile) Prepare(env *Env, raw json.RawMessage) (*Call, error) {
	var in editInput
	if err := decode(raw, &in); err != nil {
		return nil, err
	}
	if err := required("path", in.Path); err != nil {
		return nil, err
	}
	if in.OldString == nil || in.NewString == nil {
		return nil, fmt.Errorf("invalid tool input: old_string and new_string are required")
	}
	if *in.OldString == *in.NewString {
		return nil, fmt.Errorf("old_string and new_string are identical; nothing to change")
	}
	abs := env.Resolve(in.Path)
	data, err := os.ReadFile(abs)
	if err != nil {
		if os.IsNotExist(err) && *in.OldString == "" {
			// Creating a new file through edit is allowed with an empty old_string.
			data = nil
		} else {
			return nil, err
		}
	}
	if data != nil {
		if err := env.checkFresh(abs); err != nil {
			return nil, err
		}
	}
	updated, count, err := applyEdit(string(data), *in.OldString, *in.NewString, in.ReplaceAll)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", env.Rel(abs), err)
	}
	return &Call{
		Tool:        "edit_file",
		Kind:        KindWrite,
		Summary:     "Edit " + env.Rel(abs),
		Preview:     UnifiedDiff(env.Rel(abs), string(data), updated),
		Path:        abs,
		OutsideRoot: env.Outside(abs),
		run: func(ctx context.Context) (string, error) {
			if err := writeAtomic(abs, []byte(updated)); err != nil {
				return "", err
			}
			env.markRead(abs)
			return fmt.Sprintf("edited %s (%d replacement%s)", env.Rel(abs), count, plural(count)), nil
		},
	}, nil
}

func applyEdit(content, oldS, newS string, all bool) (string, int, error) {
	if oldS == "" {
		if content != "" {
			return "", 0, fmt.Errorf("old_string is empty but the file is not; use write_file to replace a whole file")
		}
		return newS, 1, nil
	}
	n := strings.Count(content, oldS)
	if n == 0 && strings.Contains(content, "\r\n") && strings.Contains(oldS, "\n") && !strings.Contains(oldS, "\r") {
		// The file uses CRLF line endings, but read_file output and the
		// model's strings use LF. Match and replace in the file's style.
		if crlf := toCRLF(oldS); strings.Count(content, crlf) > 0 {
			oldS, newS = crlf, toCRLF(newS)
			n = strings.Count(content, oldS)
		}
	}
	switch {
	case n == 0:
		hint := ""
		if strings.Count(content, strings.TrimSpace(oldS)) > 0 {
			hint = " (a whitespace-trimmed version does match; check indentation)"
		}
		return "", 0, fmt.Errorf("old_string not found%s", hint)
	case n > 1 && !all:
		return "", 0, fmt.Errorf("old_string matches %d places; add surrounding context or set replace_all", n)
	}
	if all {
		return strings.ReplaceAll(content, oldS, newS), n, nil
	}
	return strings.Replace(content, oldS, newS, 1), 1, nil
}

// toCRLF turns bare LF line endings into CRLF.
func toCRLF(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
