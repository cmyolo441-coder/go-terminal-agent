// Package tools implements the built-in tools the agent can call.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Image is an image returned by a tool, base64 encoded.
type Image struct {
	MediaType string
	Data      string
}

// Result is what a tool hands back to the model and the terminal.
type Result struct {
	Text    string  // sent to the model
	Display string  // optional richer rendering for the terminal (e.g. a diff)
	Images  []Image // sent to the model alongside Text
	IsError bool
}

// Errorf builds an error result.
func Errorf(format string, a ...any) Result {
	return Result{Text: fmt.Sprintf(format, a...), IsError: true}
}

// Tool is one callable tool.
type Tool struct {
	Name        string
	Description string
	Properties  map[string]any
	Required    []string
	ReadOnly    bool
	IsEdit      bool
	// Key returns the string permission rules match against (command, absolute path, URL).
	Key func(in json.RawMessage, env *Env) string
	// Summary returns the short argument text shown next to the tool name.
	Summary func(in json.RawMessage, env *Env) string
	Run     func(ctx context.Context, in json.RawMessage, env *Env) Result
}

// Todo is one item of the task list.
type Todo struct {
	Content    string `json:"content"`
	Status     string `json:"status"`
	ActiveForm string `json:"activeForm,omitempty"`
}

// Env is the shared state tools operate on.
type Env struct {
	Cwd    string
	Shells *ShellManager

	mu    sync.Mutex
	read  map[string]time.Time // absolute path -> mtime when last read
	todos []Todo
}

// NewEnv creates the tool environment for a working directory.
func NewEnv(cwd string) *Env {
	return &Env{Cwd: cwd, Shells: NewShellManager(), read: map[string]time.Time{}}
}

// Abs resolves a possibly relative path against the working directory.
func (e *Env) Abs(p string) string {
	if p == "" {
		return e.Cwd
	}
	if strings.HasPrefix(p, "~/") {
		if h, err := userHome(); err == nil {
			p = filepath.Join(h, p[2:])
		}
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(e.Cwd, p)
	}
	return filepath.Clean(p)
}

// Rel shortens a path for display.
func (e *Env) Rel(p string) string {
	if rel, err := filepath.Rel(e.Cwd, p); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return p
}

func (e *Env) markRead(path string, mtime time.Time) {
	e.mu.Lock()
	e.read[path] = mtime
	e.mu.Unlock()
}

func (e *Env) readAt(path string) (time.Time, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, ok := e.read[path]
	return t, ok
}

// Todos returns a copy of the current task list.
func (e *Env) Todos() []Todo {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Todo(nil), e.todos...)
}

func (e *Env) setTodos(t []Todo) {
	e.mu.Lock()
	e.todos = t
	e.mu.Unlock()
}

// Registry is an ordered set of tools.
type Registry struct {
	tools map[string]*Tool
}

// NewRegistry returns a registry holding the given tools.
func NewRegistry(ts ...*Tool) *Registry {
	r := &Registry{tools: map[string]*Tool{}}
	for _, t := range ts {
		r.Add(t)
	}
	return r
}

func (r *Registry) Add(t *Tool)     { r.tools[t.Name] = t }
func (r *Registry) Remove(n string) { delete(r.tools, n) }

func (r *Registry) Get(name string) (*Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// List returns tools sorted by name so the request prefix stays byte-stable for caching.
func (r *Registry) List() []*Tool {
	out := make([]*Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Clone copies the registry, leaving out the named tools.
func (r *Registry) Clone(without ...string) *Registry {
	skip := map[string]bool{}
	for _, n := range without {
		skip[n] = true
	}
	out := NewRegistry()
	for n, t := range r.tools {
		if !skip[n] {
			out.tools[n] = t
		}
	}
	return out
}

// Builtin returns the standard tool set.
func Builtin() *Registry {
	return NewRegistry(bashTool(), bashOutputTool(), killShellTool(), readTool(), writeTool(), editTool(),
		globTool(), grepTool(), webFetchTool(), todoTool())
}

// decode unmarshals tool input and checks required fields are present, since
// eagerly streamed inputs are not validated by the API.
func decode(in json.RawMessage, required []string, v any) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(in, &m); err != nil {
		return fmt.Errorf("tool input is not a JSON object: %v", err)
	}
	for _, k := range required {
		if raw, ok := m[k]; !ok || string(raw) == "null" {
			return fmt.Errorf("missing required parameter %q", k)
		}
	}
	if err := json.Unmarshal(in, v); err != nil {
		return fmt.Errorf("invalid tool input: %v", err)
	}
	return nil
}

func str(in json.RawMessage, key string) string {
	var m map[string]any
	_ = json.Unmarshal(in, &m)
	s, _ := m[key].(string)
	return s
}

// truncateMiddle keeps the head and tail of long output.
func truncateMiddle(s string, max int) string {
	if len(s) <= max {
		return s
	}
	half := max / 2
	return s[:half] + fmt.Sprintf("\n\n... [%d characters truncated] ...\n\n", len(s)-max) + s[len(s)-half:]
}

func prop(typ, desc string) map[string]any { return map[string]any{"type": typ, "description": desc} }
