package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/ui"
)

const (
	maxReadLines = 2000
	maxLineLen   = 2000
	maxImageSize = 5 << 20
	maxReadBytes = 256 << 10 // per Read call, after offset/limit
)

func userHome() (string, error) { return os.UserHomeDir() }

var imageTypes = map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp"}

func pathKey(in json.RawMessage, env *Env) string     { return env.Abs(str(in, "file_path")) }
func pathSummary(in json.RawMessage, env *Env) string { return env.Rel(env.Abs(str(in, "file_path"))) }

func readTool() *Tool {
	return &Tool{
		Name: "Read",
		Description: `Reads a file and returns its contents with line numbers (like cat -n).

- Reads up to 2000 lines from the start by default; use offset and limit for a specific range of a large file.
- Reads PNG, JPEG, GIF and WebP images and shows them to you visually.
- It reads files, not directories; use Glob or "ls" via Bash to list a directory.`,
		Properties: map[string]any{
			"file_path": prop("string", "Path to the file, absolute or relative to the working directory"),
			"offset":    prop("integer", "1-based line number to start from"),
			"limit":     prop("integer", "Maximum number of lines to read"),
		},
		Required: []string{"file_path"},
		ReadOnly: true,
		Key:      pathKey,
		Summary:  pathSummary,
		Run: func(_ context.Context, raw json.RawMessage, env *Env) Result {
			var in struct {
				FilePath string `json:"file_path"`
				Offset   int    `json:"offset"`
				Limit    int    `json:"limit"`
			}
			if err := decode(raw, []string{"file_path"}, &in); err != nil {
				return Errorf("%v", err)
			}
			path := env.Abs(in.FilePath)
			st, err := os.Stat(path)
			if err != nil {
				return Errorf("cannot read %s: %v", in.FilePath, err)
			}
			if st.IsDir() {
				return Errorf("%s is a directory; use Glob or `ls` to list it", in.FilePath)
			}
			if mt, ok := imageTypes[strings.ToLower(filepath.Ext(path))]; ok {
				if st.Size() > maxImageSize {
					return Errorf("image is %d bytes; the limit is %d", st.Size(), maxImageSize)
				}
				b, err := os.ReadFile(path)
				if err != nil {
					return Errorf("cannot read %s: %v", in.FilePath, err)
				}
				return Result{Text: fmt.Sprintf("Image %s (%d bytes)", in.FilePath, len(b)),
					Images: []Image{{MediaType: mt, Data: base64.StdEncoding.EncodeToString(b)}}}
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return Errorf("cannot read %s: %v", in.FilePath, err)
			}
			if bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0 {
				return Errorf("%s looks like a binary file", in.FilePath)
			}
			env.markRead(path, st.ModTime())
			if len(b) == 0 {
				return Result{Text: "(the file exists but is empty)"}
			}
			lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
			start := max(in.Offset, 1)
			if start > len(lines) {
				return Errorf("offset %d is past the end of the file (%d lines)", start, len(lines))
			}
			limit := maxReadLines
			if in.Limit > 0 {
				limit = in.Limit
			}
			end := min(start-1+limit, len(lines))
			var sb strings.Builder
			for i := start - 1; i < end; i++ {
				l := lines[i]
				if len(l) > maxLineLen {
					l = l[:maxLineLen] + "… [line truncated]"
				}
				fmt.Fprintf(&sb, "%6d\t%s\n", i+1, l)
				if sb.Len() > maxReadBytes {
					end = i + 1
					break
				}
			}
			if end < len(lines) {
				fmt.Fprintf(&sb, "\n[showing lines %d-%d of %d; pass offset=%d to continue]\n", start, end, len(lines), end+1)
			}
			return Result{Text: sb.String(), Display: fmt.Sprintf("Read %d lines", end-start+1)}
		},
	}
}

// checkFresh enforces read-before-write on existing files.
func checkFresh(env *Env, path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return nil // new file
	}
	readAt, ok := env.readAt(path)
	if !ok {
		return fmt.Errorf("%s exists but has not been read in this session; Read it first", env.Rel(path))
	}
	if st.ModTime().After(readAt) {
		return fmt.Errorf("%s changed on disk since it was read; Read it again before editing", env.Rel(path))
	}
	return nil
}

func writeFile(env *Env, path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		return err
	}
	if st, err := os.Stat(path); err == nil {
		env.markRead(path, st.ModTime())
	}
	return nil
}

func writeTool() *Tool {
	return &Tool{
		Name: "Write",
		Description: `Creates a file or completely replaces an existing one with the given content. Parent directories are created.

An existing file must have been Read in this session first. To change part of a file, use Edit instead.`,
		Properties: map[string]any{
			"file_path": prop("string", "Path to the file"),
			"content":   prop("string", "The full content to write"),
		},
		Required: []string{"file_path", "content"},
		IsEdit:   true,
		Key:      pathKey,
		Summary:  pathSummary,
		Run: func(_ context.Context, raw json.RawMessage, env *Env) Result {
			var in struct {
				FilePath string `json:"file_path"`
				Content  string `json:"content"`
			}
			if err := decode(raw, []string{"file_path", "content"}, &in); err != nil {
				return Errorf("%v", err)
			}
			path := env.Abs(in.FilePath)
			if err := checkFresh(env, path); err != nil {
				return Errorf("%v", err)
			}
			_, statErr := os.Stat(path)
			if err := writeFile(env, path, in.Content); err != nil {
				return Errorf("write failed: %v", err)
			}
			n := strings.Count(in.Content, "\n")
			if in.Content != "" && !strings.HasSuffix(in.Content, "\n") {
				n++
			}
			verb := "Created"
			if statErr == nil {
				verb = "Overwrote"
			}
			preview, more := ui.FirstLines(in.Content, 8)
			disp := fmt.Sprintf("%s %s (%d lines)\n%s", verb, env.Rel(path), n, colorLines(preview, "+"))
			if more > 0 {
				disp += ui.Dim(fmt.Sprintf("\n… +%d lines", more))
			}
			return Result{Text: fmt.Sprintf("%s %s (%d lines)", verb, path, n), Display: disp}
		},
	}
}

func editTool() *Tool {
	return &Tool{
		Name: "Edit",
		Description: `Replaces an exact string in a file.

- old_string must match the file exactly, including whitespace and indentation, and must be unique unless replace_all is true. Include enough surrounding lines to make it unique.
- Do not include the line-number prefix that Read prints.
- The file must have been Read in this session first.`,
		Properties: map[string]any{
			"file_path":   prop("string", "Path to the file"),
			"old_string":  prop("string", "The exact text to replace"),
			"new_string":  prop("string", "The replacement text"),
			"replace_all": prop("boolean", "Replace every occurrence instead of exactly one"),
		},
		Required: []string{"file_path", "old_string", "new_string"},
		IsEdit:   true,
		Key:      pathKey,
		Summary:  pathSummary,
		Run: func(_ context.Context, raw json.RawMessage, env *Env) Result {
			var in struct {
				FilePath   string `json:"file_path"`
				OldString  string `json:"old_string"`
				NewString  string `json:"new_string"`
				ReplaceAll bool   `json:"replace_all"`
			}
			if err := decode(raw, []string{"file_path", "old_string", "new_string"}, &in); err != nil {
				return Errorf("%v", err)
			}
			path := env.Abs(in.FilePath)
			b, err := os.ReadFile(path)
			if err != nil {
				return Errorf("cannot edit %s: %v", in.FilePath, err)
			}
			if err := checkFresh(env, path); err != nil {
				return Errorf("%v", err)
			}
			if in.OldString == in.NewString {
				return Errorf("old_string and new_string are identical")
			}
			if in.OldString == "" {
				return Errorf("old_string is empty; use Write to create a file")
			}
			content := string(b)
			n := strings.Count(content, in.OldString)
			switch {
			case n == 0:
				return Errorf("old_string was not found in %s. Check whitespace and indentation against a fresh Read.", in.FilePath)
			case n > 1 && !in.ReplaceAll:
				return Errorf("old_string occurs %d times in %s. Add surrounding context to make it unique, or set replace_all.", n, in.FilePath)
			}
			line := 1 + strings.Count(content[:strings.Index(content, in.OldString)], "\n")
			if in.ReplaceAll {
				content = strings.ReplaceAll(content, in.OldString, in.NewString)
			} else {
				content = strings.Replace(content, in.OldString, in.NewString, 1)
			}
			if err := writeFile(env, path, content); err != nil {
				return Errorf("write failed: %v", err)
			}
			text := fmt.Sprintf("Edited %s (%d replacement", path, n)
			if n != 1 {
				text += "s"
			}
			text += ")"
			return Result{Text: text, Display: fmt.Sprintf("Updated %s at line %d\n%s", env.Rel(path), line, Diff(in.OldString, in.NewString))}
		},
	}
}

func colorLines(s, sign string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if sign == "+" {
			lines[i] = ui.Green("+ " + l)
		} else {
			lines[i] = ui.Red("- " + l)
		}
	}
	return strings.Join(lines, "\n")
}

// Diff renders a compact removed/added view, trimming shared leading and trailing lines.
func Diff(oldS, newS string) string {
	a, b := strings.Split(oldS, "\n"), strings.Split(newS, "\n")
	for len(a) > 0 && len(b) > 0 && a[0] == b[0] {
		a, b = a[1:], b[1:]
	}
	for len(a) > 0 && len(b) > 0 && a[len(a)-1] == b[len(b)-1] {
		a, b = a[:len(a)-1], b[:len(b)-1]
	}
	const maxShown = 20
	var out []string
	emit := func(lines []string, sign string) {
		for i, l := range lines {
			if i == maxShown {
				out = append(out, ui.Dim(fmt.Sprintf("  … %d more lines", len(lines)-maxShown)))
				break
			}
			out = append(out, colorLines(l, sign))
		}
	}
	emit(a, "-")
	emit(b, "+")
	return strings.Join(out, "\n")
}

func todoTool() *Tool {
	return &Tool{
		Name: "TodoWrite",
		Description: `Replaces the session task list. Use it to plan and track multi-step work so the user can see progress.

Send the full list every time. Keep exactly one item in_progress while working, and mark items completed as soon as they are done. Skip it for trivial single-step requests.`,
		Properties: map[string]any{
			"todos": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"content":    prop("string", "The task, in imperative form"),
						"status":     map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed"}},
						"activeForm": prop("string", "Present-continuous form shown while in progress"),
					},
					"required": []string{"content", "status"},
				},
			},
		},
		Required: []string{"todos"},
		ReadOnly: true,
		Summary:  func(json.RawMessage, *Env) string { return "" },
		Run: func(_ context.Context, raw json.RawMessage, env *Env) Result {
			var in struct {
				Todos []Todo `json:"todos"`
			}
			if err := decode(raw, []string{"todos"}, &in); err != nil {
				return Errorf("%v", err)
			}
			for _, t := range in.Todos {
				if t.Status != "pending" && t.Status != "in_progress" && t.Status != "completed" {
					return Errorf("invalid status %q; use pending, in_progress or completed", t.Status)
				}
			}
			env.setTodos(in.Todos)
			return Result{Text: "Task list updated.", Display: RenderTodos(in.Todos)}
		},
	}
}

// RenderTodos formats the task list as a checklist.
func RenderTodos(todos []Todo) string {
	if len(todos) == 0 {
		return ui.Dim("(no tasks)")
	}
	var out []string
	for _, t := range todos {
		switch t.Status {
		case "completed":
			out = append(out, ui.Green("☒ ")+ui.Dim(t.Content))
		case "in_progress":
			out = append(out, ui.Accent("◐ ")+ui.Bold(t.Content))
		default:
			out = append(out, "☐ "+t.Content)
		}
	}
	return strings.Join(out, "\n")
}
