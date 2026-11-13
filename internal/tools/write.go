package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

// WriteFile creates or overwrites a file.
type WriteFile struct{}

type writeInput struct {
	Path    string  `json:"path"`
	Content *string `json:"content"`
}

func (WriteFile) Kind() Kind { return KindWrite }

func (WriteFile) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "write_file",
		Description: "Create a file or overwrite it completely, creating parent directories as needed. " +
			"An existing file must be read first. Prefer edit_file for changes to existing files.",
		Properties: map[string]any{
			"path":    map[string]any{"type": "string", "description": "File path, absolute or relative to the workspace root."},
			"content": map[string]any{"type": "string", "description": "The complete new file content."},
		},
		Required: []string{"path", "content"},
	}
}

func (WriteFile) Prepare(env *Env, raw json.RawMessage) (*Call, error) {
	var in writeInput
	if err := decode(raw, &in); err != nil {
		return nil, err
	}
	if err := required("path", in.Path); err != nil {
		return nil, err
	}
	if in.Content == nil {
		return nil, fmt.Errorf("invalid tool input: \"content\" is required")
	}
	abs := env.Resolve(in.Path)
	if st, err := os.Stat(abs); err == nil && st.IsDir() {
		return nil, fmt.Errorf("%s is a directory", env.Rel(abs))
	}
	if err := env.checkFresh(abs); err != nil {
		return nil, err
	}
	old, _ := os.ReadFile(abs)
	verb := "Create"
	if old != nil {
		verb = "Overwrite"
	}
	content := *in.Content
	return &Call{
		Tool:        "write_file",
		Kind:        KindWrite,
		Summary:     fmt.Sprintf("%s %s", verb, env.Rel(abs)),
		Preview:     UnifiedDiff(env.Rel(abs), string(old), content),
		Path:        abs,
		OutsideRoot: env.Outside(abs),
		run: func(ctx context.Context) (string, error) {
			if err := writeAtomic(abs, []byte(content)); err != nil {
				return "", err
			}
			env.markRead(abs)
			n := strings.Count(content, "\n")
			if content != "" && !strings.HasSuffix(content, "\n") {
				n++
			}
			return fmt.Sprintf("%sd %s (%d lines)", strings.ToLower(verb), env.Rel(abs), n), nil
		},
	}, nil
}

// writeAtomic writes via a temp file and rename, keeping the file mode.
// A symlink is written through, to its target, rather than replaced by a
// regular file (which would silently break the link).
func writeAtomic(abs string, data []byte) error {
	if st, err := os.Lstat(abs); err == nil && st.Mode()&os.ModeSymlink != 0 {
		if real, err := filepath.EvalSymlinks(abs); err == nil {
			abs = real
		}
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if st, err := os.Stat(abs); err == nil {
		mode = st.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(abs), "."+filepath.Base(abs)+".gta-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), abs)
}
