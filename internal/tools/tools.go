// Package tools implements the client-side tools the agent can call: file
// reading and editing, search, directory listing, shell commands and a task
// list. Every tool validates its input strictly before it runs, because tool
// input streams eagerly and is not validated by the API.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

// Kind says what a tool call can affect, which drives permission checks.
type Kind int

const (
	KindRead Kind = iota
	KindWrite
	KindExec
)

func (k Kind) String() string {
	switch k {
	case KindWrite:
		return "write"
	case KindExec:
		return "exec"
	default:
		return "read"
	}
}

// Call is a validated tool invocation ready to run.
type Call struct {
	Tool string
	Kind Kind
	// Summary is a short human label, e.g. "Edit internal/agent/agent.go".
	Summary string
	// Preview shows what will change: a diff for edits, the command for shell.
	Preview string
	// Path is the absolute target path for file writes.
	Path string
	// Command is the shell command for exec calls.
	Command string
	// OutsideRoot is true when a write targets a path outside the workspace.
	OutsideRoot bool
	run         func(ctx context.Context) (string, error)
}

// Run executes the call.
func (c *Call) Run(ctx context.Context) (string, error) { return c.run(ctx) }

// Tool is one capability exposed to the model.
type Tool interface {
	Spec() llm.ToolSpec
	Kind() Kind
	// Prepare decodes and validates input and returns a runnable call.
	Prepare(env *Env, input json.RawMessage) (*Call, error)
}

// Todo is one item in the agent's task list.
type Todo struct {
	Content string `json:"content"`
	Status  string `json:"status"` // pending | in_progress | completed
}

// Env is the shared state tools operate on.
type Env struct {
	Root         string
	ShellTimeout time.Duration
	OutputLimit  int
	// Wrap, when set, turns a shell command into the argv that runs it,
	// for example inside a sandbox. An error means the command must not run.
	Wrap func(shell, command string) ([]string, error)

	mu       sync.Mutex
	readMods map[string]time.Time
	todos    []Todo
	onTodos  func([]Todo)
}

// NewEnv returns an environment rooted at root.
func NewEnv(root string) (*Env, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	return &Env{Root: abs, ShellTimeout: 2 * time.Minute, OutputLimit: 30000, readMods: map[string]time.Time{}}, nil
}

// OnTodos registers a callback for task list updates.
func (e *Env) OnTodos(f func([]Todo)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.onTodos = f
}

// Todos returns the current task list.
func (e *Env) Todos() []Todo {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Todo(nil), e.todos...)
}

func (e *Env) setTodos(t []Todo) {
	e.mu.Lock()
	e.todos = append([]Todo(nil), t...)
	f := e.onTodos
	e.mu.Unlock()
	if f != nil {
		f(t)
	}
}

// Resolve turns a model-supplied path into an absolute path.
func (e *Env) Resolve(p string) string {
	p = strings.TrimSpace(p)
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[2:])
		}
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(e.Root, p)
	}
	return filepath.Clean(p)
}

// Rel renders a path relative to the root when it is inside it.
func (e *Env) Rel(abs string) string {
	if r, err := filepath.Rel(e.Root, abs); err == nil && e.Inside(abs) {
		return r
	}
	return abs
}

// Inside reports whether abs is within the workspace root.
func (e *Env) Inside(abs string) bool {
	r, err := filepath.Rel(e.Root, abs)
	return err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator))
}

// Outside reports whether a write to abs lands outside the workspace: abs
// itself is outside, or it is a symlink (or under a symlinked directory)
// whose target is. Writes follow symlinks, so a link inside the workspace
// must not make a write to ~/.bashrc look like a workspace edit.
func (e *Env) Outside(abs string) bool {
	if !e.Inside(abs) {
		return true
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		real, err = filepath.EvalSymlinks(filepath.Dir(abs))
		if err != nil {
			return false
		}
		real = filepath.Join(real, filepath.Base(abs))
	}
	root, err := filepath.EvalSymlinks(e.Root)
	if err != nil {
		root = e.Root
	}
	r, err := filepath.Rel(root, real)
	return err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator))
}

// MarkRead records that the user or model has seen a file's current content.
func (e *Env) MarkRead(abs string) { e.markRead(abs) }

func (e *Env) markRead(abs string) {
	if st, err := os.Stat(abs); err == nil {
		e.mu.Lock()
		e.readMods[abs] = st.ModTime()
		e.mu.Unlock()
	}
}

// checkFresh makes sure an existing file was read and has not changed on
// disk since, so edits never clobber work the model has not seen.
func (e *Env) checkFresh(abs string) error {
	st, err := os.Stat(abs)
	if err != nil {
		return nil // new file
	}
	e.mu.Lock()
	seen, ok := e.readMods[abs]
	e.mu.Unlock()
	if !ok {
		return fmt.Errorf("%s exists but has not been read in this session; read it first", e.Rel(abs))
	}
	if !st.ModTime().Equal(seen) {
		return fmt.Errorf("%s changed on disk since it was last read; read it again first", e.Rel(abs))
	}
	return nil
}

// Registry holds the available tools.
type Registry struct {
	byName map[string]Tool
	order  []string
}

// NewRegistry builds a registry from tools.
func NewRegistry(ts ...Tool) *Registry {
	r := &Registry{byName: map[string]Tool{}}
	for _, t := range ts {
		name := t.Spec().Name
		r.byName[name] = t
		r.order = append(r.order, name)
	}
	return r
}

// Default returns the standard tool set.
func Default() *Registry {
	return NewRegistry(ReadFile{}, WriteFile{}, EditFile{}, ListDir{}, Glob{}, Grep{}, Bash{}, TodoWrite{})
}

// Get looks up a tool.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.byName[name]
	return t, ok
}

// Specs lists tool specs in registration order.
func (r *Registry) Specs() []llm.ToolSpec {
	out := make([]llm.ToolSpec, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.byName[n].Spec())
	}
	return out
}

// Names lists the tool names, sorted.
func (r *Registry) Names() []string {
	out := append([]string(nil), r.order...)
	sort.Strings(out)
	return out
}

// decode strictly decodes tool input: unknown fields and trailing data are
// rejected so a truncated or malformed eager stream never runs.
func decode(input json.RawMessage, v any) error {
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

func required(name, val string) error {
	if strings.TrimSpace(val) == "" {
		return fmt.Errorf("invalid tool input: %q is required", name)
	}
	return nil
}

// truncate keeps the head and tail of long output.
func truncate(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	// Cut on character boundaries so the output stays valid UTF-8.
	head := limit * 2 / 3
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	tail := len(s) - (limit - limit*2/3)
	for tail < len(s) && !utf8.RuneStart(s[tail]) {
		tail++
	}
	return s[:head] + fmt.Sprintf("\n\n… [%d characters omitted] …\n\n", tail-head) + s[tail:]
}

// skipDir lists directories that searches never descend into.
var skipDir = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, ".venv": true, "venv": true,
	"__pycache__": true, ".idea": true, ".next": true, "dist": true, "build": true,
	"target": true, ".cache": true, ".gradle": true, ".terraform": true,
}

func isBinary(b []byte) bool {
	n := len(b)
	if n > 8000 {
		n = 8000
	}
	return bytes.IndexByte(b[:n], 0) >= 0
}

// NewCall builds a runnable call. Tools outside this package (MCP, sub-agents)
// use it to plug into the agent loop and the permission gate.
func NewCall(tool string, kind Kind, summary, preview, command string, run func(ctx context.Context) (string, error)) *Call {
	return &Call{Tool: tool, Kind: kind, Summary: summary, Preview: preview, Command: command, run: run}
}

// All returns the registered tools in registration order.
func (r *Registry) All() []Tool {
	out := make([]Tool, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.byName[n])
	}
	return out
}
