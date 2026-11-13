package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

// ToolPrefix starts the model-facing name of every plugin tool.
const ToolPrefix = "plugin__"

// maxToolNameLen is the longest tool name the model APIs accept.
const maxToolNameLen = 64

// ParseKind maps a manifest kind to a tools.Kind. Empty means exec, the
// most restricted kind.
func ParseKind(s string) (tools.Kind, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "read":
		return tools.KindRead, nil
	case "write":
		return tools.KindWrite, nil
	case "exec", "":
		return tools.KindExec, nil
	}
	return tools.KindExec, fmt.Errorf(`"kind" %q must be read, write or exec`, s)
}

// ToolName builds "plugin__<plugin>__<tool>", shortened with a hash suffix
// when it would exceed 64 characters.
func ToolName(pluginName, tool string) string {
	full := ToolPrefix + pluginName + "__" + tool
	if len(full) <= maxToolNameLen {
		return full
	}
	h := fnv.New32a()
	h.Write([]byte(pluginName + "\x00" + tool))
	suffix := fmt.Sprintf("_%08x", h.Sum32())
	return full[:maxToolNameLen-len(suffix)] + suffix
}

// Tool adapts a plugin tool to tools.Tool.
type Tool struct {
	plugin   *Plugin
	manifest ToolManifest
	kind     tools.Kind
	schema   *schema
	spec     llm.ToolSpec
	prog     program
	timeout  time.Duration
	maxOut   int
	load     LoadOptions
}

var _ tools.Tool = (*Tool)(nil)

func newTool(p *Plugin, tm ToolManifest, o LoadOptions) (*Tool, error) {
	kind, err := ParseKind(tm.Kind)
	if err != nil {
		return nil, err
	}
	sc, err := compileSchema(tm.InputSchema)
	if err != nil {
		return nil, fmt.Errorf("input_schema: %w", err)
	}
	exp := expander(p.Dir, o.Workspace)
	prog, err := resolveProgram(p.Dir, tm.Command, expandAll(exp, tm.Args))
	if err != nil {
		return nil, err
	}
	maxOut := tm.MaxOutputBytes
	if maxOut <= 0 {
		maxOut = DefaultMaxOutput
	}
	desc := strings.TrimSpace(tm.Description) + fmt.Sprintf(" (from plugin %q)", p.Manifest.Name)
	return &Tool{
		plugin:   p,
		manifest: tm,
		kind:     kind,
		schema:   sc,
		prog:     prog,
		timeout:  seconds(tm.TimeoutSec, DefaultToolTimeout),
		maxOut:   maxOut,
		load:     o,
		spec: llm.ToolSpec{
			Name:        ToolName(p.Manifest.Name, tm.Name),
			Description: desc,
			Properties:  sc.properties(),
			Required:    append([]string(nil), sc.required...),
		},
	}, nil
}

// Plugin is the plugin the tool belongs to.
func (t *Tool) Plugin() *Plugin { return t.plugin }

// LocalName is the tool's name inside its plugin.
func (t *Tool) LocalName() string { return t.manifest.Name }

// Spec implements tools.Tool.
func (t *Tool) Spec() llm.ToolSpec { return t.spec }

// Kind implements tools.Tool.
func (t *Tool) Kind() tools.Kind { return t.kind }

// Prepare implements tools.Tool: the input is checked against the tool's
// schema before a process is ever started.
func (t *Tool) Prepare(env *tools.Env, input json.RawMessage) (*tools.Call, error) {
	raw := bytes.TrimSpace(input)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		raw = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("invalid tool input: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid tool input: trailing data after the JSON object")
	}
	if _, ok := v.(map[string]any); !ok {
		return nil, errors.New("invalid tool input: must be a JSON object")
	}
	if err := t.schema.validate(v, ""); err != nil {
		return nil, fmt.Errorf("invalid tool input: %w", err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, fmt.Errorf("invalid tool input: %w", err)
	}
	payload := compact.Bytes()

	workspace, limit := t.load.Workspace, 0
	if env != nil {
		workspace, limit = env.Root, env.OutputLimit
	}
	preview := "{}"
	var pretty bytes.Buffer
	if json.Indent(&pretty, payload, "", "  ") == nil {
		preview = pretty.String()
	}
	summary := fmt.Sprintf("%s (plugin %s)", t.manifest.Name, t.plugin.Manifest.Name)
	name := t.spec.Name
	run := func(ctx context.Context) (string, error) {
		return t.execute(ctx, payload, workspace, limit)
	}
	call := tools.NewCall(name, t.kind, summary, clip(preview, 4000), name, run)
	return call, nil
}

// toolResult is the JSON a plugin tool prints on stdout.
type toolResult struct {
	Output  *string `json:"output"`
	IsError bool    `json:"is_error"`
}

func (t *Tool) execute(ctx context.Context, payload []byte, workspace string, limit int) (string, error) {
	opts := t.load
	opts.Workspace = workspace
	env := t.plugin.procEnv(opts, map[string]string{"GTA_TOOL_NAME": t.manifest.Name, "GTA_TOOL_KIND": t.kind.String()})
	dir := workspace
	if dir == "" {
		dir = t.plugin.Dir
	}
	res, err := runProcess(ctx, runSpec{
		prog: t.prog, dir: dir, env: env, stdin: payload, timeout: t.timeout, maxOut: t.maxOut,
	})
	if err != nil {
		return "", err
	}
	name := t.spec.Name
	switch {
	case res.timedOut:
		return "", fmt.Errorf("plugin tool %s timed out after %s%s", name, t.timeout, stderrTail(res.stderr))
	case res.overflow:
		return "", fmt.Errorf("plugin tool %s printed more than %d bytes and was stopped%s", name, t.maxOut, stderrTail(res.stderr))
	}
	out := bytes.TrimSpace(res.stdout)
	var tr toolResult
	parseErr := errors.New("no output")
	if len(out) > 0 {
		dec := json.NewDecoder(bytes.NewReader(out))
		parseErr = dec.Decode(&tr)
		if parseErr == nil {
			if _, err := dec.Token(); !errors.Is(err, io.EOF) {
				parseErr = errors.New("trailing data after the JSON result")
			}
		}
	}
	if parseErr != nil {
		if res.exitCode != 0 {
			return "", fmt.Errorf("plugin tool %s exited with status %d%s", name, res.exitCode, stderrTail(res.stderr))
		}
		return "", fmt.Errorf(`plugin tool %s did not print a JSON result such as {"output": "...", "is_error": false}: %v (stdout began %q)`, name, parseErr, clip(string(out), 120))
	}
	text := ""
	if tr.Output != nil {
		text = *tr.Output
	}
	text = truncate(strings.ToValidUTF8(text, "�"), limit)
	if tr.IsError || res.exitCode != 0 {
		if strings.TrimSpace(text) == "" {
			text = "(no details)"
		}
		if res.exitCode != 0 && !tr.IsError {
			return text, fmt.Errorf("plugin tool %s exited with status %d", name, res.exitCode)
		}
		return text, fmt.Errorf("plugin tool %s reported an error", name)
	}
	if strings.TrimSpace(text) == "" {
		text = "(tool returned no output)"
	}
	return text, nil
}

func stderrTail(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) > 2000 {
		cut := len(s) - 2000
		for cut < len(s) && !utf8.RuneStart(s[cut]) {
			cut++
		}
		s = "…" + s[cut:]
	}
	return ":\n" + s
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// truncate keeps the head and tail of long output.
func truncate(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	head := limit * 2 / 3
	tail := limit - head
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	start := len(s) - tail
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[:head] + fmt.Sprintf("\n\n… [%d characters omitted] …\n\n", start-head) + s[start:]
}
