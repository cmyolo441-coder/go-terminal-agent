// Package subagent implements the "task" tool, which lets the main agent
// delegate focused sub-tasks to child agents that run concurrently, each in
// its own conversation, and returns their final reports.
//
// Two modes are supported:
//
//   - explore (the default) gives each child only the read-only tools
//     (read_file, list_dir, glob, grep) behind a plan-mode permission gate.
//     Launching explore sub-agents is a read-only call and needs no approval.
//   - general gives each child the parent's full tool set (minus the task
//     tool itself, so sub-agents cannot recurse) behind an ask-mode gate whose
//     approval prompts are delegated back to the parent's UI. Launching general
//     sub-agents is an exec call, so the user approves the launch first.
//
// Child agents share the parent's provider and model settings (optionally
// with a different model), run with a per-child turn limit, observe the
// parent's context for cancellation, and report progress and token usage
// through callbacks so the UI can show live status and bill the work.
package subagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/agent"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permission"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

// Name is the tool name exposed to the model.
const Name = "task"

// Mode selects what a batch of sub-agents may do.
type Mode string

const (
	// ModeExplore runs read-only research sub-agents. It is the default.
	ModeExplore Mode = "explore"
	// ModeGeneral runs sub-agents that may edit files and run commands, with
	// every gated action approved through Config.Approve.
	ModeGeneral Mode = "general"
)

// Limits and defaults.
const (
	// MaxTasks is the most tasks one call may launch.
	MaxTasks = 8
	// DefaultMaxParallel is how many children run at once when
	// Config.MaxParallel is not set.
	DefaultMaxParallel = 4
	// DefaultMaxTurns is the per-child model-call limit when Config.MaxTurns
	// is not set.
	DefaultMaxTurns = 40
	// DefaultReportLimit is the per-report character budget in the output
	// returned to the parent when Config.ReportLimit is not set.
	DefaultReportLimit = 16000

	maxDescriptionRunes = 100
	maxPromptBytes      = 100_000
	maxPreviewPrompt    = 2000
)

// readOnlyTools are the tools explore children get, in registration order.
var readOnlyTools = []string{"read_file", "list_dir", "glob", "grep"}

// Task is one unit of delegated work.
type Task struct {
	// Description is a short label shown in progress and as the report heading.
	Description string `json:"description"`
	// Prompt is the complete, self-contained instruction for the child.
	Prompt string `json:"prompt"`
}

// Config wires the task tool into a session. Only Provider is required; the
// zero value of every other field selects a sensible default.
type Config struct {
	// Provider is the model backend children use (normally the parent's).
	Provider llm.Provider
	// Settings returns the parent's current model settings. Children copy
	// them at launch, with the overrides below applied. Thinking display is
	// always off for children because their stream is not shown.
	Settings func() agent.Settings
	// Model, when set, overrides the parent's model for children (for
	// example a cheaper, faster model for exploration).
	Model string
	// Tools is the parent's tool registry. Children are built from its tool
	// implementations; nil means tools.Default(). The task tool itself is
	// never given to children.
	Tools *tools.Registry
	// MaxParallel bounds how many children run at once (default 4).
	MaxParallel int
	// MaxTurns is the per-child model-call limit (default 40).
	MaxTurns int
	// ReportLimit is the per-report character budget in the output returned
	// to the parent model (default 16000). Longer reports keep their head
	// and tail with a marker in between.
	ReportLimit int
	// Instructions are extra project instructions (for example the user's
	// memory files) appended to every child's system prompt.
	Instructions string
	// AllowCommands are command prefixes general children may run without
	// asking, normally the same list the parent's gate uses.
	AllowCommands []string
	// ParentMode, when set, returns the parent's current permission mode and
	// general children's gate uses it, so a parent in yolo or auto-edit mode
	// is not prompted for every child action. When nil, general children
	// always use permission.ModeAsk.
	ParentMode func() permission.Mode
	// Approve answers general children's approval prompts, normally by
	// showing the parent UI's approval dialog. Calls are serialized: at most
	// one prompt is outstanding at a time. The reason names the sub-agent
	// asking. When nil, every action that needs approval is declined.
	Approve func(ctx context.Context, call *tools.Call, reason string) permission.Decision
	// Progress, when set, receives live status for each child. index is the
	// task's 0-based position in the call and description its label. Event
	// is human readable: EventQueued, EventStarted, a tool call summary such
	// as "Read internal/x.go", "Waiting for approval: …", or a terminal event
	// for which Terminal reports true. Calls are serialized.
	Progress func(index int, description, event string)
	// OnUsage, when set, receives token usage for every child model call,
	// with the model that served it, so the parent can bill it. Calls are
	// serialized with Progress.
	OnUsage func(model string, u llm.Usage)
	// Setup, when set, adjusts each child agent after it is built, for
	// example to give it the parent's governance.
	Setup func(child *agent.Agent)
}

// Tool is the "task" tool. It is safe for concurrent use.
type Tool struct {
	cfg Config

	// approvals serializes delegated approval prompts (capacity 1), and
	// unlike a mutex lets a waiting child give up when its context ends.
	approvals chan struct{}
	// cbMu serializes Progress and OnUsage callbacks.
	cbMu sync.Mutex

	usageMu sync.Mutex
	usage   llm.Usage
	calls   int
}

var _ tools.Tool = (*Tool)(nil)

// New builds the task tool.
func New(cfg Config) *Tool {
	if cfg.MaxParallel <= 0 {
		cfg.MaxParallel = DefaultMaxParallel
	}
	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = DefaultMaxTurns
	}
	if cfg.ReportLimit <= 0 {
		cfg.ReportLimit = DefaultReportLimit
	}
	return &Tool{cfg: cfg, approvals: make(chan struct{}, 1)}
}

// Usage returns the total token usage of every child model call made through
// this tool since it was created, and the number of those calls.
func (t *Tool) Usage() (llm.Usage, int) {
	t.usageMu.Lock()
	defer t.usageMu.Unlock()
	return t.usage, t.calls
}

// Kind reports the most a task call can do. Each prepared call carries its
// own kind: KindRead for explore, KindExec for general.
func (t *Tool) Kind() tools.Kind { return tools.KindExec }

// Spec describes the tool to the model.
func (t *Tool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: Name,
		Description: fmt.Sprintf("Launch sub-agents that work on independent tasks in parallel and return their final reports. "+
			"Each sub-agent starts with an empty context and sees only the prompt you give it, so every prompt must be self-contained: "+
			"the goal, the relevant paths, names and background, and exactly what to report back. Sub-agents cannot ask you questions. "+
			"Use mode \"explore\" (the default) for read-only research such as locating code, tracing call paths or answering questions "+
			"that need many searches; it needs no approval. Use mode \"general\" only for tasks that must edit files or run commands; "+
			"the user approves the launch and the sub-agents' actions, and parallel sub-agents must not edit the same files. "+
			"Pass 1 to %d tasks per call; up to %d run at once. For a single file or a quick search, use the tools directly instead. "+
			"Reports can be incomplete or wrong: verify anything critical before you act on it.", MaxTasks, t.cfg.MaxParallel),
		Properties: map[string]any{
			"tasks": map[string]any{
				"type":        "array",
				"minItems":    1,
				"maxItems":    MaxTasks,
				"description": "The sub-tasks to run in parallel.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"description": map[string]any{"type": "string", "description": "A short label (3-8 words) for progress display and the report heading."},
						"prompt":      map[string]any{"type": "string", "description": "Complete, self-contained instructions for the sub-agent, including what its final report must contain."},
					},
					"required":             []string{"description", "prompt"},
					"additionalProperties": false,
				},
			},
			"mode": map[string]any{
				"type":        "string",
				"enum":        []string{string(ModeExplore), string(ModeGeneral)},
				"description": "explore (default): read-only tools, no approval needed. general: full tools, actions need the user's approval.",
			},
		},
		Required: []string{"tasks"},
	}
}

type input struct {
	Tasks *[]Task `json:"tasks"`
	Mode  string  `json:"mode"`
}

// Prepare validates input and returns a call that runs the batch.
func (t *Tool) Prepare(env *tools.Env, raw json.RawMessage) (*tools.Call, error) {
	mode, tasks, err := parseInput(raw)
	if err != nil {
		return nil, err
	}
	if err := t.ready(env); err != nil {
		return nil, err
	}
	kind := tools.KindRead
	command := ""
	if mode == ModeGeneral {
		kind = tools.KindExec
		// A fixed command lets "allow for this session" cover later general
		// launches (the gate grants by command prefix).
		command = Name + " " + string(ModeGeneral)
	}
	run := func(ctx context.Context) (string, error) {
		rep, err := t.Run(ctx, env, mode, tasks)
		if rep == nil {
			return "", err
		}
		out := rep.Format(t.cfg.ReportLimit)
		if err != nil {
			return out, err
		}
		if n := rep.Failed(); n == len(rep.Results) {
			return out, fmt.Errorf("all %d sub-agents failed", n)
		}
		return out, nil
	}
	return tools.NewCall(Name, kind, summary(mode, tasks), preview(mode, tasks), command, run), nil
}

// ready reports configuration problems before anything is launched.
func (t *Tool) ready(env *tools.Env) error {
	if t.cfg.Provider == nil {
		return errors.New("sub-agents are not available: no model provider is configured")
	}
	if env == nil || env.Root == "" {
		return errors.New("sub-agents are not available: no workspace environment")
	}
	if t.settings().Model == "" {
		return errors.New("sub-agents are not available: no model is configured")
	}
	return nil
}

// settings returns the model settings children run with.
func (t *Tool) settings() agent.Settings {
	var s agent.Settings
	if t.cfg.Settings != nil {
		s = t.cfg.Settings()
	}
	if t.cfg.Model != "" {
		s.Model = t.cfg.Model
	}
	s.MaxTurns = t.cfg.MaxTurns
	s.ShowThinking = false
	return s
}

// parseInput strictly decodes and validates tool input.
func parseInput(raw json.RawMessage) (Mode, []Task, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var in input
	if err := dec.Decode(&in); err != nil {
		return "", nil, fmt.Errorf("invalid tool input: %w", err)
	}
	if dec.More() {
		return "", nil, errors.New("invalid tool input: trailing data")
	}
	mode := ModeExplore
	switch Mode(in.Mode) {
	case "", ModeExplore:
	case ModeGeneral:
		mode = ModeGeneral
	default:
		return "", nil, fmt.Errorf("invalid tool input: unknown mode %q (want %q or %q)", in.Mode, ModeExplore, ModeGeneral)
	}
	if in.Tasks == nil {
		return "", nil, errors.New(`invalid tool input: "tasks" is required`)
	}
	tasks := *in.Tasks
	switch {
	case len(tasks) == 0:
		return "", nil, errors.New("invalid tool input: at least one task is required")
	case len(tasks) > MaxTasks:
		return "", nil, fmt.Errorf("invalid tool input: %d tasks given, at most %d per call", len(tasks), MaxTasks)
	}
	out := make([]Task, len(tasks))
	seen := map[string]int{}
	for i, tk := range tasks {
		n := i + 1
		desc := strings.Join(strings.Fields(tk.Description), " ")
		if desc == "" {
			return "", nil, fmt.Errorf("invalid tool input: task %d: \"description\" is required", n)
		}
		if utf8.RuneCountInString(desc) > maxDescriptionRunes {
			return "", nil, fmt.Errorf("invalid tool input: task %d: description is longer than %d characters; keep it to a short label", n, maxDescriptionRunes)
		}
		key := strings.ToLower(desc)
		if j, dup := seen[key]; dup {
			return "", nil, fmt.Errorf("invalid tool input: tasks %d and %d have the same description %q", j, n, desc)
		}
		seen[key] = n
		prompt := strings.TrimSpace(tk.Prompt)
		if prompt == "" {
			return "", nil, fmt.Errorf("invalid tool input: task %d (%s): \"prompt\" is required", n, desc)
		}
		if len(prompt) > maxPromptBytes {
			return "", nil, fmt.Errorf("invalid tool input: task %d (%s): prompt is longer than %d bytes", n, desc, maxPromptBytes)
		}
		if !utf8.ValidString(prompt) || !utf8.ValidString(desc) {
			return "", nil, fmt.Errorf("invalid tool input: task %d: text is not valid UTF-8", n)
		}
		out[i] = Task{Description: desc, Prompt: prompt}
	}
	return mode, out, nil
}

func summary(mode Mode, tasks []Task) string {
	verb := "Explore"
	if mode == ModeGeneral {
		verb = "Delegate"
	}
	if len(tasks) == 1 {
		return verb + ": " + tasks[0].Description
	}
	return fmt.Sprintf("%s with %d sub-agents", verb, len(tasks))
}

// preview lists the tasks so the user sees what a general launch will do.
func preview(mode Mode, tasks []Task) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d %s sub-agent", len(tasks), mode)
	if len(tasks) != 1 {
		b.WriteString("s")
	}
	b.WriteString(":\n")
	for i, tk := range tasks {
		fmt.Fprintf(&b, "\n%d. %s\n", i+1, tk.Description)
		p := tk.Prompt
		if len(p) > maxPreviewPrompt {
			p = clipBytes(p, maxPreviewPrompt) + " …"
		}
		for _, line := range strings.Split(p, "\n") {
			b.WriteString("   " + line + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// clipBytes cuts s to at most n bytes without splitting a UTF-8 sequence.
func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
