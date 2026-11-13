package autopilot

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

// Plan limits.
const (
	// MaxPlanSteps is the most steps a plan may have.
	MaxPlanSteps   = 40
	maxTitleRunes  = 200
	maxGoalRunes   = 1000
	maxPlanBytes   = 256 << 10
	maxPlanFiles   = 200
	maxListEntries = 50
)

// File actions.
const (
	ActionCreate = "create"
	ActionModify = "modify"
	ActionDelete = "delete"
)

// Plan is a structured implementation plan.
type Plan struct {
	// Goal restates the task as the outcome to achieve.
	Goal string `json:"goal"`
	// Steps are executed in order. IDs are 1..len(Steps).
	Steps []Step `json:"steps"`
	// Files lists every file the plan creates, modifies or deletes.
	Files []FileTouch `json:"files"`
	// Risks are what could go wrong, with mitigations.
	Risks []Risk `json:"risks"`
	// Verification says how to prove the task is done.
	Verification Verification `json:"verification"`
}

// Step is one plan step.
type Step struct {
	ID      int      `json:"id"`
	Title   string   `json:"title"`
	Details string   `json:"details,omitempty"`
	Files   []string `json:"files,omitempty"`
}

// FileTouch is a file the plan changes.
type FileTouch struct {
	Path   string `json:"path"`
	Action string `json:"action"`
	Reason string `json:"reason,omitempty"`
}

// UnmarshalJSON accepts an object or a bare path string.
func (f *FileTouch) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*f = FileTouch{Path: s, Action: ActionModify}
		return nil
	}
	type plain FileTouch
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*f = FileTouch(p)
	return nil
}

// Risk is a risk and how the plan mitigates it.
type Risk struct {
	Risk       string `json:"risk"`
	Mitigation string `json:"mitigation,omitempty"`
}

// UnmarshalJSON accepts an object or a bare string.
func (r *Risk) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*r = Risk{Risk: s}
		return nil
	}
	type plain Risk
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*r = Risk(p)
	return nil
}

// Verification is how the result will be checked.
type Verification struct {
	// Commands are exact shell commands, e.g. "go test ./...".
	Commands []string `json:"commands,omitempty"`
	// Checks are other things to confirm, e.g. "the help text lists /plan".
	Checks []string `json:"checks,omitempty"`
}

// UnmarshalJSON accepts an object, a string or a list of strings.
func (v *Verification) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*v = Verification{Checks: []string{s}}
		return nil
	}
	var list []string
	if json.Unmarshal(b, &list) == nil {
		*v = Verification{Checks: list}
		return nil
	}
	type plain Verification
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*v = Verification(p)
	return nil
}

// ParsePlan extracts, decodes and validates a plan from a model reply.
// root is the workspace root used to validate and normalize paths.
func ParsePlan(text, root string) (*Plan, error) {
	raw, err := extractObject(text, "goal", "steps")
	if err != nil {
		return nil, err
	}
	if len(raw) > maxPlanBytes {
		return nil, fmt.Errorf("the plan JSON is %d bytes; keep it under %d by moving code out of the plan", len(raw), maxPlanBytes)
	}
	var p Plan
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("the plan JSON does not match the schema: %v", err)
	}
	if err := p.Normalize(root); err != nil {
		return nil, err
	}
	return &p, nil
}

// Normalize validates the plan and puts it in canonical form: trimmed
// text, numbered steps, workspace-relative slash paths, every file a step
// mentions listed in Files.
func (p *Plan) Normalize(root string) error {
	p.Goal = strings.TrimSpace(p.Goal)
	switch {
	case p.Goal == "":
		return errors.New(`"goal" is required`)
	case utf8.RuneCountInString(p.Goal) > maxGoalRunes:
		return fmt.Errorf(`"goal" is longer than %d characters; state the outcome in a sentence or two`, maxGoalRunes)
	case len(p.Steps) == 0:
		return errors.New(`"steps" must list at least one step`)
	case len(p.Steps) > MaxPlanSteps:
		return fmt.Errorf(`"steps" has %d entries; merge them into at most %d`, len(p.Steps), MaxPlanSteps)
	}
	allZero := true
	for _, s := range p.Steps {
		if s.ID != 0 {
			allZero = false
		}
	}
	for i := range p.Steps {
		s := &p.Steps[i]
		if allZero {
			s.ID = i + 1
		} else if s.ID != i+1 {
			return fmt.Errorf("steps must be numbered 1 to %d in order; the step at position %d has id %d", len(p.Steps), i+1, s.ID)
		}
		s.Title = strings.Join(strings.Fields(s.Title), " ")
		if s.Title == "" {
			return fmt.Errorf("step %d has no title", s.ID)
		}
		if utf8.RuneCountInString(s.Title) > maxTitleRunes {
			return fmt.Errorf("step %d's title is longer than %d characters; put the detail in \"details\"", s.ID, maxTitleRunes)
		}
		s.Details = strings.TrimSpace(s.Details)
		var files []string
		seen := map[string]bool{}
		for _, f := range s.Files {
			np, err := normalizePlanPath(root, f)
			if err != nil {
				return fmt.Errorf("step %d: %w", s.ID, err)
			}
			if !seen[np] {
				seen[np] = true
				files = append(files, np)
			}
		}
		s.Files = files
	}

	var files []FileTouch
	index := map[string]int{}
	for _, f := range p.Files {
		np, err := normalizePlanPath(root, f.Path)
		if err != nil {
			return fmt.Errorf("files: %w", err)
		}
		action, err := normalizeAction(f.Action)
		if err != nil {
			return fmt.Errorf("files: %s: %w", np, err)
		}
		if _, dup := index[np]; dup {
			continue
		}
		index[np] = len(files)
		files = append(files, FileTouch{Path: np, Action: action, Reason: strings.TrimSpace(f.Reason)})
	}
	for _, s := range p.Steps {
		for _, f := range s.Files {
			if _, ok := index[f]; !ok {
				index[f] = len(files)
				files = append(files, FileTouch{Path: f, Action: ActionModify, Reason: fmt.Sprintf("referenced by step %d", s.ID)})
			}
		}
	}
	if len(files) > maxPlanFiles {
		return fmt.Errorf("the plan touches %d files; split the task (at most %d)", len(files), maxPlanFiles)
	}
	p.Files = files

	var risks []Risk
	for _, r := range p.Risks {
		r.Risk, r.Mitigation = strings.TrimSpace(r.Risk), strings.TrimSpace(r.Mitigation)
		if r.Risk != "" {
			risks = append(risks, r)
		}
	}
	if len(risks) > maxListEntries {
		risks = risks[:maxListEntries]
	}
	p.Risks = risks

	p.Verification.Commands = cleanList(p.Verification.Commands)
	p.Verification.Checks = cleanList(p.Verification.Checks)
	if len(p.Verification.Commands)+len(p.Verification.Checks) == 0 {
		return errors.New(`"verification" must list at least one command or check that proves the task is done`)
	}
	return nil
}

func cleanList(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
		if len(out) == maxListEntries {
			break
		}
	}
	return out
}

func normalizeAction(a string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(a)) {
	case "", "modify", "edit", "update", "change", "patch":
		return ActionModify, nil
	case "create", "add", "new":
		return ActionCreate, nil
	case "delete", "remove", "rm":
		return ActionDelete, nil
	}
	return "", fmt.Errorf("unknown action %q (want create, modify or delete)", a)
}

// normalizePlanPath makes p a clean, slash-separated path relative to root
// and refuses paths that leave the workspace.
func normalizePlanPath(root, p string) (string, error) {
	p = strings.Trim(strings.TrimSpace(p), "`\"'")
	if p == "" {
		return "", errors.New("empty file path")
	}
	if strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("invalid file path %q", p)
	}
	native := filepath.FromSlash(p)
	if filepath.IsAbs(native) {
		if root == "" {
			return filepath.ToSlash(filepath.Clean(native)), nil
		}
		rel, err := filepath.Rel(root, native)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("path %q is outside the workspace", p)
		}
		native = rel
	}
	c := filepath.Clean(native)
	if c == "." || c == ".." || strings.HasPrefix(c, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside the workspace", p)
	}
	return filepath.ToSlash(c), nil
}

// Step returns the step with the given id.
func (p *Plan) Step(id int) (Step, bool) {
	if id < 1 || id > len(p.Steps) {
		return Step{}, false
	}
	return p.Steps[id-1], true
}

// JSON renders the plan as indented JSON.
func (p *Plan) JSON() string {
	b, _ := json.MarshalIndent(p, "", "  ")
	return string(b)
}

// Markdown renders the plan for people and for the executing agent.
func (p *Plan) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Goal:** %s\n\n**Steps**\n", p.Goal)
	for _, s := range p.Steps {
		fmt.Fprintf(&b, "%d. %s\n", s.ID, s.Title)
		if s.Details != "" {
			for _, l := range strings.Split(s.Details, "\n") {
				b.WriteString("   " + l + "\n")
			}
		}
		if len(s.Files) > 0 {
			fmt.Fprintf(&b, "   Files: %s\n", strings.Join(s.Files, ", "))
		}
	}
	if len(p.Files) > 0 {
		b.WriteString("\n**Files**\n")
		for _, f := range p.Files {
			fmt.Fprintf(&b, "- %s `%s`", f.Action, f.Path)
			if f.Reason != "" {
				b.WriteString(" — " + f.Reason)
			}
			b.WriteString("\n")
		}
	}
	if len(p.Risks) > 0 {
		b.WriteString("\n**Risks**\n")
		for _, r := range p.Risks {
			b.WriteString("- " + r.Risk)
			if r.Mitigation != "" {
				b.WriteString(" — mitigation: " + r.Mitigation)
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("\n**Verification**\n")
	for _, c := range p.Verification.Commands {
		fmt.Fprintf(&b, "- `%s`\n", c)
	}
	for _, c := range p.Verification.Checks {
		b.WriteString("- " + c + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// TodoContent is the todo-list label for a step: "N. Title".
func TodoContent(s Step) string { return fmt.Sprintf("%d. %s", s.ID, s.Title) }

// Todos returns the plan as a todo list, every step pending.
func (p *Plan) Todos() []tools.Todo {
	out := make([]tools.Todo, len(p.Steps))
	for i, s := range p.Steps {
		out[i] = tools.Todo{Content: TodoContent(s), Status: StatusPending}
	}
	return out
}

// Step statuses, matching the todo list's.
const (
	StatusPending    = "pending"
	StatusInProgress = "in_progress"
	StatusCompleted  = "completed"
)

// StepState is a step's execution status.
type StepState struct {
	ID     int    `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

// StepsDone counts completed steps.
func StepsDone(states []StepState) int {
	n := 0
	for _, s := range states {
		if s.Status == StatusCompleted {
			n++
		}
	}
	return n
}

// stepStates maps the executor's todo list onto the plan's steps. Todos
// labelled "N. …" (or "Step N: …") map by number; otherwise, when the list
// has exactly one item per step, by position.
func stepStates(p *Plan, todos []tools.Todo) []StepState {
	states := make([]StepState, len(p.Steps))
	for i, s := range p.Steps {
		states[i] = StepState{ID: s.ID, Title: s.Title, Status: StatusPending}
	}
	matched := false
	for _, t := range todos {
		if id, ok := todoStepID(t.Content); ok && id >= 1 && id <= len(states) {
			states[id-1].Status = t.Status
			matched = true
		}
	}
	if !matched && len(todos) == len(states) {
		for i, t := range todos {
			states[i].Status = t.Status
		}
	}
	return states
}

// todoStepID reads the leading step number of a todo label.
func todoStepID(s string) (int, bool) {
	s = strings.TrimSpace(s)
	low := strings.ToLower(s)
	if rest, ok := strings.CutPrefix(low, "step"); ok {
		s = strings.TrimSpace(rest)
	}
	n := 0
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' && i < 4 {
		n = n*10 + int(s[i]-'0')
		i++
	}
	if i == 0 || i >= len(s) {
		return 0, false
	}
	switch s[i] {
	case '.', ':', ')', '-', ' ':
		return n, true
	}
	return 0, false
}
