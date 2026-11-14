package evals

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// GraderType names a grader.
type GraderType string

const (
	GradeFileContains     GraderType = "file_contains"
	GradeFileMatchesRegex GraderType = "file_matches_regex"
	GradeFileUnchanged    GraderType = "file_unchanged"
	GradeCommandSucceeds  GraderType = "command_succeeds"
	GradeToolCalled       GraderType = "tool_called"
	GradeToolNotCalled    GraderType = "tool_not_called"
	GradeOutputMatches    GraderType = "output_matches"
	GradeJSONSchema       GraderType = "json_schema"
	GradeLLMJudge         GraderType = "llm_judge"
)

// ArgPredicate tests one value inside a tool call's JSON input.
type ArgPredicate struct {
	// Path is a dotted path into the input ("file_path", "edits.0.old").
	// Empty means the whole input.
	Path string `json:"path,omitempty"`
	// Op is one of equals, not_equals, contains, not_contains, matches,
	// exists, absent. Default is equals.
	Op string `json:"op,omitempty"`
	// Value is the operand: any JSON for equals/not_equals, a string for
	// contains/not_contains/matches.
	Value json.RawMessage `json:"value,omitempty"`

	re     *regexp.Regexp
	val    any
	strVal string
}

// GraderSpec configures one grader. Which fields apply depends on Type.
type GraderSpec struct {
	Type GraderType `json:"type"`
	Name string     `json:"name,omitempty"`
	// Weight in the case score (default 1; 0 makes the grader advisory).
	Weight *float64 `json:"weight,omitempty"`
	// Required graders must pass for the run to pass, whatever the score.
	Required bool `json:"required,omitempty"`

	// Path is a workspace file (file_*, json_schema with source "file").
	// For file_unchanged it may be a glob over fixture files ("src/*.go",
	// "**" for every file).
	Path string `json:"path,omitempty"`
	// Text is a literal substring (file_contains, output_matches).
	Text string `json:"text,omitempty"`
	// Pattern is a regular expression (file_matches_regex,
	// output_matches, and optionally command output).
	Pattern string `json:"pattern,omitempty"`
	// IgnoreCase makes Text and Pattern matching case-insensitive.
	IgnoreCase bool `json:"ignore_case,omitempty"`
	// Negate inverts file_contains, file_matches_regex and output_matches.
	Negate bool `json:"negate,omitempty"`

	// Command is run through the injected CommandRunner in the workspace.
	Command string `json:"command,omitempty"`
	// Timeout bounds the command (default 2m).
	Timeout Duration `json:"timeout,omitempty"`

	// Tool is a tool name or path.Match glob ("mcp__*").
	Tool string         `json:"tool,omitempty"`
	Args []ArgPredicate `json:"args,omitempty"`
	// MinCount and MaxCount bound how many matching calls tool_called
	// accepts (default at least 1, no maximum).
	MinCount *int `json:"min_count,omitempty"`
	MaxCount *int `json:"max_count,omitempty"`

	// Source for json_schema: "output" (default; JSON is extracted from
	// the final answer, including fenced code blocks) or "file".
	Source string          `json:"source,omitempty"`
	Schema json.RawMessage `json:"schema,omitempty"`

	// Rubric for llm_judge.
	Rubric string `json:"rubric,omitempty"`
	// Scale is the judge's maximum score when it does not state one
	// (default 10).
	Scale float64 `json:"scale,omitempty"`
	// Threshold is the normalised judge score needed to pass (default 0.7).
	Threshold *float64 `json:"threshold,omitempty"`

	re     *regexp.Regexp
	schema *Schema
}

func (g *GraderSpec) weight() float64 {
	if g.Weight == nil {
		return 1
	}
	return *g.Weight
}

func (g *GraderSpec) judgeThreshold() float64 {
	if g.Threshold == nil {
		return 0.7
	}
	return *g.Threshold
}

// compile validates the grader and prepares regexes and schemas.
func (g *GraderSpec) compile() error {
	if w := g.weight(); w < 0 || math.IsNaN(w) || math.IsInf(w, 0) {
		return errors.New("weight must be a non-negative number")
	}
	need := func(field, val string) error {
		if strings.TrimSpace(val) == "" {
			return fmt.Errorf("%s is required", field)
		}
		return nil
	}
	compilePattern := func() error {
		if g.Pattern == "" {
			return nil
		}
		p := g.Pattern
		if g.IgnoreCase {
			p = "(?i)" + p
		}
		re, err := regexp.Compile(p)
		if err != nil {
			return fmt.Errorf("pattern: %w", err)
		}
		g.re = re
		return nil
	}
	checkPath := func() error {
		if err := need("path", g.Path); err != nil {
			return err
		}
		_, err := cleanRelPath(g.Path)
		return err
	}
	switch g.Type {
	case GradeFileContains:
		if err := checkPath(); err != nil {
			return err
		}
		return need("text", g.Text)
	case GradeFileMatchesRegex:
		if err := checkPath(); err != nil {
			return err
		}
		if err := need("pattern", g.Pattern); err != nil {
			return err
		}
		return compilePattern()
	case GradeFileUnchanged:
		if err := need("path", g.Path); err != nil {
			return err
		}
		if g.Path != "**" {
			if _, err := cleanRelPath(g.Path); err != nil {
				return err
			}
			if _, err := path.Match(g.Path, ""); err != nil {
				return fmt.Errorf("path glob: %w", err)
			}
		}
		return nil
	case GradeCommandSucceeds:
		if err := need("command", g.Command); err != nil {
			return err
		}
		return compilePattern()
	case GradeToolCalled, GradeToolNotCalled:
		if err := need("tool", g.Tool); err != nil {
			return err
		}
		if _, err := path.Match(g.Tool, ""); err != nil {
			return fmt.Errorf("tool glob: %w", err)
		}
		if g.MinCount != nil && *g.MinCount < 0 {
			return errors.New("min_count must not be negative")
		}
		if g.MaxCount != nil && *g.MaxCount < 0 {
			return errors.New("max_count must not be negative")
		}
		if g.MinCount != nil && g.MaxCount != nil && *g.MinCount > *g.MaxCount {
			return errors.New("min_count exceeds max_count")
		}
		if g.Type == GradeToolNotCalled && (g.MinCount != nil || g.MaxCount != nil) {
			return errors.New("min_count/max_count apply only to tool_called")
		}
		for i := range g.Args {
			if err := g.Args[i].compile(); err != nil {
				return fmt.Errorf("args[%d]: %w", i, err)
			}
		}
		return nil
	case GradeOutputMatches:
		if g.Text == "" && g.Pattern == "" {
			return errors.New("text or pattern is required")
		}
		return compilePattern()
	case GradeJSONSchema:
		switch g.Source {
		case "", "output":
		case "file":
			if err := checkPath(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("source must be \"output\" or \"file\", got %q", g.Source)
		}
		if len(bytes.TrimSpace(g.Schema)) == 0 {
			return errors.New("schema is required")
		}
		s, err := CompileSchema(g.Schema)
		if err != nil {
			return err
		}
		g.schema = s
		return nil
	case GradeLLMJudge:
		if err := need("rubric", g.Rubric); err != nil {
			return err
		}
		if g.Scale < 0 || math.IsNaN(g.Scale) || math.IsInf(g.Scale, 0) {
			return errors.New("scale must be positive")
		}
		if g.Threshold != nil && !validUnit(*g.Threshold) {
			return errors.New("threshold must be between 0 and 1")
		}
		return nil
	case "":
		return errors.New("type is required")
	default:
		return fmt.Errorf("unknown grader type %q", g.Type)
	}
}

func (a *ArgPredicate) compile() error {
	if a.Op == "" {
		a.Op = "equals"
	}
	switch a.Op {
	case "exists", "absent":
		return nil
	case "equals", "not_equals":
		if len(bytes.TrimSpace(a.Value)) == 0 {
			return fmt.Errorf("%s needs a value", a.Op)
		}
		v, err := decodeJSONValue(a.Value)
		if err != nil {
			return fmt.Errorf("value: %w", err)
		}
		a.val = v
		return nil
	case "contains", "not_contains", "matches":
		var s string
		if err := json.Unmarshal(a.Value, &s); err != nil {
			return fmt.Errorf("%s needs a string value", a.Op)
		}
		a.strVal = s
		if a.Op == "matches" {
			re, err := regexp.Compile(s)
			if err != nil {
				return fmt.Errorf("value: %w", err)
			}
			a.re = re
		}
		return nil
	default:
		return fmt.Errorf("unknown op %q", a.Op)
	}
}

// lookupPath walks a dotted path through decoded JSON.
func lookupPath(v any, p string) (any, bool) {
	if p == "" {
		return v, true
	}
	cur := v
	for _, seg := range strings.Split(p, ".") {
		switch x := cur.(type) {
		case map[string]any:
			n, ok := x[seg]
			if !ok {
				return nil, false
			}
			cur = n
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(x) {
				return nil, false
			}
			cur = x[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

func (a *ArgPredicate) match(input any) bool {
	v, ok := lookupPath(input, a.Path)
	switch a.Op {
	case "exists":
		return ok
	case "absent":
		return !ok
	case "equals":
		return ok && jsonEqual(v, a.val)
	case "not_equals":
		return !ok || !jsonEqual(v, a.val)
	}
	s, isStr := v.(string)
	if !isStr && ok {
		raw, _ := canonicalJSON(v)
		s = string(raw)
	}
	switch a.Op {
	case "contains":
		return ok && strings.Contains(s, a.strVal)
	case "not_contains":
		return !ok || !strings.Contains(s, a.strVal)
	case "matches":
		return ok && a.re.MatchString(s)
	}
	return false
}

func (a ArgPredicate) String() string {
	p := a.Path
	if p == "" {
		p = "input"
	}
	switch a.Op {
	case "exists", "absent":
		return p + " " + a.Op
	}
	return fmt.Sprintf("%s %s %s", p, a.Op, bytes.TrimSpace(a.Value))
}

// CommandResult is the outcome of a grader command.
type CommandResult struct {
	ExitCode int
	Output   string
}

// CommandRunner runs a shell command in dir. A non-zero exit is reported
// through ExitCode, not as an error; errors mean the command could not be
// run at all.
type CommandRunner func(ctx context.Context, dir, command string) (CommandResult, error)

// maxCommandOutput caps captured command output.
const maxCommandOutput = 1 << 20

// ShellRunner runs commands with "sh -c" (or the given shell and flag),
// capturing combined output up to 1 MiB. It is a ready-made CommandRunner
// for wiring the harness into a CLI.
func ShellRunner(shell ...string) CommandRunner {
	argv := []string{"sh", "-c"}
	if len(shell) > 0 {
		argv = shell
	}
	return func(ctx context.Context, dir, command string) (CommandResult, error) {
		args := append(append([]string(nil), argv[1:]...), command)
		cmd := exec.CommandContext(ctx, argv[0], args...)
		cmd.Dir = dir
		cmd.WaitDelay = 2 * time.Second
		var out cappedBuffer
		out.limit = maxCommandOutput
		cmd.Stdout = &out
		cmd.Stderr = &out
		err := cmd.Run()
		res := CommandResult{Output: out.String()}
		var exitErr *exec.ExitError
		switch {
		case err == nil:
		case errors.As(err, &exitErr):
			res.ExitCode = exitErr.ExitCode()
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
		default:
			return res, err
		}
		return res, nil
	}
}

type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.limit - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
			c.truncated = true
		} else {
			c.buf.Write(p)
		}
	} else if len(p) > 0 {
		c.truncated = true
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	if c.truncated {
		return c.buf.String() + "\n[output truncated]"
	}
	return c.buf.String()
}

// GradeContext is everything a grader may inspect.
type GradeContext struct {
	Workspace string
	// Fixture maps each original fixture file to its SHA-256.
	Fixture    map[string]string
	Prompt     string
	Transcript *Transcript
	Commands   CommandRunner
	Judge      JudgeFunc
}

// GradeResult is one grader's verdict.
type GradeResult struct {
	Name     string     `json:"name"`
	Type     GraderType `json:"type"`
	Score    float64    `json:"score"`
	Passed   bool       `json:"passed"`
	Weight   float64    `json:"weight"`
	Required bool       `json:"required,omitempty"`
	Detail   string     `json:"detail,omitempty"`
	// Error is set when the grader itself could not run (as opposed to
	// the agent failing the check).
	Error string `json:"error,omitempty"`
}

// Grade runs the grader. It never panics on agent-produced data; problems
// running the check are reported in GradeResult.Error with a zero score.
func (g *GraderSpec) Grade(ctx context.Context, gc *GradeContext) GradeResult {
	res := GradeResult{Name: g.Name, Type: g.Type, Weight: g.weight(), Required: g.Required}
	if gc.Transcript == nil {
		gc.Transcript = &Transcript{}
	}
	pass := func(ok bool, detail string) GradeResult {
		res.Passed = ok
		if ok {
			res.Score = 1
		}
		res.Detail = detail
		return res
	}
	fail := func(err error) GradeResult {
		res.Error = err.Error()
		res.Detail = err.Error()
		return res
	}
	switch g.Type {
	case GradeFileContains:
		data, err := readWorkspaceFile(gc.Workspace, g.Path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && g.Negate {
				return pass(true, fmt.Sprintf("%s does not exist", g.Path))
			}
			if errors.Is(err, fs.ErrNotExist) {
				return pass(false, fmt.Sprintf("%s does not exist", g.Path))
			}
			return fail(err)
		}
		hay, needle := string(data), g.Text
		if g.IgnoreCase {
			hay, needle = strings.ToLower(hay), strings.ToLower(needle)
		}
		found := strings.Contains(hay, needle)
		return pass(found != g.Negate, describeMatch(g.Path, "contains", strconv.Quote(truncateRunes(g.Text, 80)), found))
	case GradeFileMatchesRegex:
		data, err := readWorkspaceFile(gc.Workspace, g.Path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return pass(g.Negate, fmt.Sprintf("%s does not exist", g.Path))
			}
			return fail(err)
		}
		found := g.re.Match(data)
		return pass(found != g.Negate, describeMatch(g.Path, "matches", "/"+g.Pattern+"/", found))
	case GradeFileUnchanged:
		return g.gradeUnchanged(gc, pass, fail)
	case GradeCommandSucceeds:
		return g.gradeCommand(ctx, gc, pass, fail)
	case GradeToolCalled, GradeToolNotCalled:
		return g.gradeTool(gc, pass, fail)
	case GradeOutputMatches:
		out := gc.Transcript.Output
		var found bool
		var what string
		if g.Text != "" {
			hay, needle := out, g.Text
			if g.IgnoreCase {
				hay, needle = strings.ToLower(hay), strings.ToLower(needle)
			}
			found = strings.Contains(hay, needle)
			what = strconv.Quote(truncateRunes(g.Text, 80))
		}
		if g.re != nil && (g.Text == "" || found) {
			found = g.re.MatchString(out)
			what = "/" + g.Pattern + "/"
		}
		return pass(found != g.Negate, describeMatch("output", "matches", what, found))
	case GradeJSONSchema:
		return g.gradeSchema(gc, pass, fail)
	case GradeLLMJudge:
		return g.gradeJudge(ctx, gc, res, fail)
	}
	return fail(fmt.Errorf("unknown grader type %q", g.Type))
}

func describeMatch(subject, verb, what string, found bool) string {
	if found {
		return fmt.Sprintf("%s %s %s", subject, verb, what)
	}
	neg := map[string]string{"contains": "does not contain", "matches": "does not match"}[verb]
	return fmt.Sprintf("%s %s %s", subject, neg, what)
}

// maxGradedFile caps how much of a file graders read.
const maxGradedFile = 32 << 20

func readWorkspaceFile(root, rel string) ([]byte, error) {
	full, err := resolveInside(root, rel)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(full)
	if err != nil {
		return nil, err
	}
	if st.IsDir() {
		return nil, fmt.Errorf("%s is a directory", rel)
	}
	if st.Size() > maxGradedFile {
		return nil, fmt.Errorf("%s is larger than %d bytes", rel, maxGradedFile)
	}
	return os.ReadFile(full)
}

func (g *GraderSpec) gradeUnchanged(gc *GradeContext, pass func(bool, string) GradeResult, fail func(error) GradeResult) GradeResult {
	var targets []string
	for _, p := range sortedKeys(gc.Fixture) {
		if g.Path == "**" || p == g.Path {
			targets = append(targets, p)
			continue
		}
		if ok, _ := path.Match(g.Path, p); ok {
			targets = append(targets, p)
		}
	}
	if len(targets) == 0 {
		return fail(fmt.Errorf("no fixture file matches %q", g.Path))
	}
	var changed []string
	for _, p := range targets {
		full, err := resolveInside(gc.Workspace, p)
		if err != nil {
			changed = append(changed, p+" (escaped workspace)")
			continue
		}
		sum, err := hashFile(full)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			changed = append(changed, p+" (deleted)")
		case err != nil:
			return fail(err)
		case sum != gc.Fixture[p]:
			changed = append(changed, p+" (modified)")
		}
	}
	if len(changed) > 0 {
		return pass(false, "changed: "+strings.Join(changed, ", "))
	}
	return pass(true, fmt.Sprintf("%d file(s) unchanged", len(targets)))
}

func (g *GraderSpec) gradeCommand(ctx context.Context, gc *GradeContext, pass func(bool, string) GradeResult, fail func(error) GradeResult) GradeResult {
	if gc.Commands == nil {
		return fail(errors.New("no command runner configured"))
	}
	timeout := g.Timeout.Std()
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := gc.Commands(cctx, gc.Workspace, g.Command)
	if err != nil {
		if cctx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			return pass(false, fmt.Sprintf("%q timed out after %s", g.Command, timeout))
		}
		return fail(fmt.Errorf("run %q: %w", g.Command, err))
	}
	tail := lastLines(res.Output, 20)
	if res.ExitCode != 0 {
		return pass(false, fmt.Sprintf("%q exited %d\n%s", g.Command, res.ExitCode, tail))
	}
	if g.re != nil && !g.re.MatchString(res.Output) {
		return pass(false, fmt.Sprintf("%q succeeded but its output does not match /%s/\n%s", g.Command, g.Pattern, tail))
	}
	return pass(true, fmt.Sprintf("%q exited 0", g.Command))
}

func lastLines(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = append([]string{"…"}, lines[len(lines)-n:]...)
	}
	return strings.Join(lines, "\n")
}

func (g *GraderSpec) gradeTool(gc *GradeContext, pass func(bool, string) GradeResult, _ func(error) GradeResult) GradeResult {
	count, nameCount := 0, 0
	for _, c := range gc.Transcript.ToolCalls {
		if ok, _ := path.Match(g.Tool, c.Name); !ok {
			continue
		}
		nameCount++
		var input any
		if len(c.Input) > 0 {
			v, err := decodeJSONValue(c.Input)
			if err != nil {
				// Unparseable input can only satisfy predicates on the raw text.
				v = string(c.Input)
			}
			input = v
		}
		ok := true
		for i := range g.Args {
			if !g.Args[i].match(input) {
				ok = false
				break
			}
		}
		if ok {
			count++
		}
	}
	desc := g.Tool
	if len(g.Args) > 0 {
		preds := make([]string, len(g.Args))
		for i, a := range g.Args {
			preds[i] = a.String()
		}
		desc += " where " + strings.Join(preds, " and ")
	}
	if g.Type == GradeToolNotCalled {
		return pass(count == 0, fmt.Sprintf("%s called %d time(s)", desc, count))
	}
	lo := 1
	if g.MinCount != nil {
		lo = *g.MinCount
	}
	ok := count >= lo && (g.MaxCount == nil || count <= *g.MaxCount)
	bounds := fmt.Sprintf("want >= %d", lo)
	if g.MaxCount != nil {
		bounds = fmt.Sprintf("want %d..%d", lo, *g.MaxCount)
	}
	detail := fmt.Sprintf("%s called %d time(s) (%s)", desc, count, bounds)
	if count != nameCount {
		detail += fmt.Sprintf("; %d call(s) to %s did not match the arguments", nameCount-count, g.Tool)
	}
	return pass(ok, detail)
}

func (g *GraderSpec) gradeSchema(gc *GradeContext, pass func(bool, string) GradeResult, fail func(error) GradeResult) GradeResult {
	var raw []byte
	if g.Source == "file" {
		data, err := readWorkspaceFile(gc.Workspace, g.Path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return pass(false, fmt.Sprintf("%s does not exist", g.Path))
			}
			return fail(err)
		}
		raw = data
	} else {
		var ok bool
		raw, ok = ExtractJSON(gc.Transcript.Output)
		if !ok {
			return pass(false, "no JSON value found in the output")
		}
	}
	errs, err := g.schema.ValidateJSON(raw)
	if err != nil {
		return pass(false, "invalid JSON: "+err.Error())
	}
	if len(errs) > 0 {
		return pass(false, fmt.Sprintf("%d schema violation(s): %s", len(errs), formatSchemaErrors(errs, 10)))
	}
	return pass(true, "JSON matches the schema")
}

var fenceRe = regexp.MustCompile("(?s)```[ \t]*(?:json|JSON|jsonc)?[ \t]*\r?\n(.*?)```")

// ExtractJSON finds a JSON object or array in free text: the whole text,
// then each fenced code block, then the first balanced {...} or [...]
// that parses.
func ExtractJSON(text string) ([]byte, bool) {
	t := strings.TrimSpace(text)
	if t == "" {
		return nil, false
	}
	if json.Valid([]byte(t)) {
		return []byte(t), true
	}
	for _, m := range fenceRe.FindAllStringSubmatch(t, -1) {
		if c := strings.TrimSpace(m[1]); json.Valid([]byte(c)) {
			return []byte(c), true
		}
	}
	for i := 0; i < len(t); i++ {
		if t[i] != '{' && t[i] != '[' {
			continue
		}
		if end := balancedEnd(t, i); end > i {
			if c := t[i : end+1]; json.Valid([]byte(c)) {
				return []byte(c), true
			}
		}
	}
	return nil, false
}

// balancedEnd returns the index of the bracket closing the one at start,
// honouring JSON strings, or -1.
func balancedEnd(s string, start int) int {
	var stack []byte
	inStr, esc := false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			stack = append(stack, '}')
		case '[':
			stack = append(stack, ']')
		case '}', ']':
			if len(stack) == 0 || stack[len(stack)-1] != c {
				return -1
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return i
			}
		}
	}
	return -1
}

func (g *GraderSpec) gradeJudge(ctx context.Context, gc *GradeContext, res GradeResult, fail func(error) GradeResult) GradeResult {
	if gc.Judge == nil {
		return fail(errors.New("no judge configured"))
	}
	scale := g.Scale
	if scale == 0 {
		scale = 10
	}
	reply, err := gc.Judge(ctx, JudgeRequest{
		Rubric:     g.Rubric,
		Prompt:     gc.Prompt,
		Output:     gc.Transcript.Output,
		Transcript: gc.Transcript,
		Workspace:  gc.Workspace,
		Scale:      scale,
	})
	if err != nil {
		return fail(fmt.Errorf("judge: %w", err))
	}
	score, err := ParseJudgeScore(reply, scale)
	if err != nil {
		return fail(err)
	}
	res.Score = score
	res.Passed = score >= g.judgeThreshold()
	res.Detail = fmt.Sprintf("judge score %.3f (threshold %.2f): %s", score, g.judgeThreshold(), truncateRunes(strings.TrimSpace(reply), 300))
	return res
}
