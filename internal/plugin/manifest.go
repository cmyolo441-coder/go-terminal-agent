// Package plugin loads GTA plugins: directories holding a plugin.json
// manifest that adds model-callable tools (external executables speaking a
// JSON stdin/stdout protocol) and slash commands (prompt templates or
// executables). Plugins come from the user's config directory and, only
// when the user trusts the project, from the project's .gta/plugins.
//
// Tool protocol: GTA starts the executable in the workspace root with the
// tool input as one JSON object on stdin, and reads one JSON object from
// stdout: {"output": "text for the model", "is_error": false}. The process
// runs in its own process group with a timeout, a capped output size and a
// scrubbed environment (only basic variables, the manifest's pass_env names
// and env values, and GTA_* variables describing the call).
package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// ManifestFile is the manifest's name inside a plugin directory.
const ManifestFile = "plugin.json"

// Limits on manifests.
const (
	maxManifestBytes = 1 << 20
	maxTools         = 128
	maxCommands      = 128
	maxPluginName    = 32
	maxItemName      = 48
	maxDescription   = 4096
	// MaxTimeout is the longest a tool or command may run.
	MaxTimeout = 600
	// DefaultToolTimeout and DefaultCommandTimeout apply when a manifest
	// gives no timeout_sec.
	DefaultToolTimeout    = 60
	DefaultCommandTimeout = 30
	// DefaultMaxOutput caps a tool's stdout unless the manifest says
	// otherwise; MaxOutputLimit is the most it may ask for.
	DefaultMaxOutput = 1 << 20
	MaxOutputLimit   = 16 << 20
)

// Manifest is the content of plugin.json.
type Manifest struct {
	// Name identifies the plugin: lower-case letters, digits, '-' and '_'.
	Name    string `json:"name"`
	Version string `json:"version"`
	// Description says what the plugin is for.
	Description string `json:"description"`
	// MinGTAVersion is the oldest GTA release the plugin works with.
	MinGTAVersion string `json:"min_gta_version,omitempty"`
	// Env sets variables for every tool and command. ${PLUGIN_DIR} and
	// ${WORKSPACE} are expanded.
	Env map[string]string `json:"env,omitempty"`
	// PassEnv names variables copied from GTA's environment (for example
	// an API token the plugin needs). Nothing else is inherited beyond a
	// small set of basics such as PATH, HOME and locale settings.
	PassEnv  []string          `json:"pass_env,omitempty"`
	Tools    []ToolManifest    `json:"tools,omitempty"`
	Commands []CommandManifest `json:"commands,omitempty"`
}

// ToolManifest declares one model-callable tool.
type ToolManifest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Kind is read, write or exec (the default). It drives permission
	// checks: reads run without asking, writes follow the file-edit
	// rules and exec follows the shell-command rules.
	Kind string `json:"kind,omitempty"`
	// InputSchema is a JSON Schema object for the tool input.
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	// Command is the executable: a path relative to the plugin directory
	// (./bin/tool), an absolute path, or a program name found in PATH.
	Command string `json:"command"`
	// Args are passed before anything else; ${PLUGIN_DIR} and
	// ${WORKSPACE} are expanded.
	Args           []string `json:"args,omitempty"`
	TimeoutSec     int      `json:"timeout_sec,omitempty"`
	MaxOutputBytes int      `json:"max_output_bytes,omitempty"`
}

// CommandManifest declares one slash command. Exactly one of Prompt and
// Command is set.
type CommandManifest struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	ArgumentHint string `json:"argument_hint,omitempty"`
	// Prompt is a template sent to the model. {{args}} is the whole
	// argument text and {{1}} to {{9}} are its shell-style words. A
	// template without placeholders gets the arguments appended.
	Prompt string `json:"prompt,omitempty"`
	// Command is an executable whose stdout is shown to the user. The
	// arguments are appended to Args as shell-style words and are also
	// in $GTA_ARGS.
	Command    string   `json:"command,omitempty"`
	Args       []string `json:"args,omitempty"`
	TimeoutSec int      `json:"timeout_sec,omitempty"`
}

// ValidationError lists every problem found in a manifest.
type ValidationError struct {
	File     string
	Problems []string
}

func (e *ValidationError) Error() string {
	if len(e.Problems) == 1 {
		return fmt.Sprintf("%s: %s", e.File, e.Problems[0])
	}
	return fmt.Sprintf("%s:\n  - %s", e.File, strings.Join(e.Problems, "\n  - "))
}

var (
	pluginNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	itemNameRE   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)
	envNameRE    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	placeholder  = regexp.MustCompile(`\{\{\s*([^{}]*?)\s*\}\}`)
)

// reservedPluginNames would make tool names ambiguous.
var reservedPluginNames = map[string]bool{"gta": true, "mcp": true, "plugin": true, "builtin": true}

// ParseManifest decodes and validates plugin.json content. file names the
// manifest in errors. Unknown fields are errors, so typos are caught.
func ParseManifest(file string, r io.Reader) (*Manifest, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxManifestBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	if len(data) > maxManifestBytes {
		return nil, &ValidationError{File: file, Problems: []string{fmt.Sprintf("manifest is larger than %d bytes", maxManifestBytes)}}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, &ValidationError{File: file, Problems: []string{describeJSONError(data, err)}}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, &ValidationError{File: file, Problems: []string{"unexpected data after the JSON object"}}
	}
	if probs := m.validate(); len(probs) > 0 {
		return nil, &ValidationError{File: file, Problems: probs}
	}
	return &m, nil
}

// describeJSONError adds a line and column to JSON syntax errors.
func describeJSONError(data []byte, err error) string {
	var se *json.SyntaxError
	var te *json.UnmarshalTypeError
	off := int64(-1)
	switch {
	case errors.As(err, &se):
		off = se.Offset
	case errors.As(err, &te):
		off = te.Offset
	}
	if off < 0 || off > int64(len(data)) {
		return "invalid JSON: " + err.Error()
	}
	line := 1 + bytes.Count(data[:off], []byte("\n"))
	col := off - int64(bytes.LastIndexByte(data[:off], '\n'))
	return fmt.Sprintf("invalid JSON at line %d, column %d: %v", line, col, err)
}

func (m *Manifest) validate() []string {
	var p []string
	add := func(format string, args ...any) { p = append(p, fmt.Sprintf(format, args...)) }
	switch {
	case m.Name == "":
		add(`"name" is required`)
	case len(m.Name) > maxPluginName || !pluginNameRE.MatchString(m.Name):
		add(`"name" %q must be 1-%d lower-case letters, digits, '-' or '_', starting with a letter or digit`, m.Name, maxPluginName)
	case reservedPluginNames[m.Name]:
		add(`"name" %q is reserved`, m.Name)
	}
	if m.Version == "" {
		add(`"version" is required (semantic version such as 1.0.0)`)
	} else if _, err := ParseVersion(m.Version); err != nil {
		add(`"version": %v`, err)
	}
	if strings.TrimSpace(m.Description) == "" {
		add(`"description" is required`)
	} else if len(m.Description) > maxDescription {
		add(`"description" is longer than %d bytes`, maxDescription)
	}
	if m.MinGTAVersion != "" {
		if _, err := ParseVersion(m.MinGTAVersion); err != nil {
			add(`"min_gta_version": %v`, err)
		}
	}
	for k := range m.Env {
		if !envNameRE.MatchString(k) {
			add(`"env": %q is not a valid variable name`, k)
		} else if strings.HasPrefix(strings.ToUpper(k), "GTA_") {
			add(`"env": %q uses the reserved GTA_ prefix`, k)
		}
	}
	for _, k := range m.PassEnv {
		if !envNameRE.MatchString(k) {
			add(`"pass_env": %q is not a valid variable name`, k)
		}
	}
	if len(m.Tools) == 0 && len(m.Commands) == 0 {
		add(`the plugin declares no "tools" and no "commands"`)
	}
	if len(m.Tools) > maxTools {
		add(`too many tools (%d, the limit is %d)`, len(m.Tools), maxTools)
	}
	if len(m.Commands) > maxCommands {
		add(`too many commands (%d, the limit is %d)`, len(m.Commands), maxCommands)
	}
	seen := map[string]bool{}
	for i, t := range m.Tools {
		where := fmt.Sprintf("tools[%d]", i)
		if t.Name != "" {
			where = fmt.Sprintf("tool %q", t.Name)
		}
		p = append(p, validateName(where, t.Name)...)
		if seen[strings.ToLower(t.Name)] && t.Name != "" {
			add(`%s is declared twice`, where)
		}
		seen[strings.ToLower(t.Name)] = true
		if strings.TrimSpace(t.Description) == "" {
			add(`%s: "description" is required so the model knows when to use it`, where)
		} else if len(t.Description) > maxDescription {
			add(`%s: "description" is longer than %d bytes`, where, maxDescription)
		}
		if _, err := ParseKind(t.Kind); err != nil {
			add(`%s: %v`, where, err)
		}
		if strings.TrimSpace(t.Command) == "" {
			add(`%s: "command" is required`, where)
		}
		if t.TimeoutSec < 0 || t.TimeoutSec > MaxTimeout {
			add(`%s: "timeout_sec" must be between 1 and %d`, where, MaxTimeout)
		}
		if t.MaxOutputBytes < 0 || t.MaxOutputBytes > MaxOutputLimit {
			add(`%s: "max_output_bytes" must be between 1 and %d`, where, MaxOutputLimit)
		}
		if _, err := compileSchema(t.InputSchema); err != nil {
			add(`%s: "input_schema": %v`, where, err)
		}
	}
	seen = map[string]bool{}
	for i, c := range m.Commands {
		where := fmt.Sprintf("commands[%d]", i)
		if c.Name != "" {
			where = fmt.Sprintf("command %q", c.Name)
		}
		p = append(p, validateName(where, c.Name)...)
		if seen[strings.ToLower(c.Name)] && c.Name != "" {
			add(`%s is declared twice`, where)
		}
		seen[strings.ToLower(c.Name)] = true
		if strings.TrimSpace(c.Description) == "" {
			add(`%s: "description" is required`, where)
		}
		hasPrompt, hasCmd := strings.TrimSpace(c.Prompt) != "", strings.TrimSpace(c.Command) != ""
		switch {
		case hasPrompt && hasCmd:
			add(`%s: set either "prompt" or "command", not both`, where)
		case !hasPrompt && !hasCmd:
			add(`%s: set "prompt" (a template) or "command" (an executable)`, where)
		case hasPrompt:
			if len(c.Args) > 0 || c.TimeoutSec != 0 {
				add(`%s: "args" and "timeout_sec" only apply to executable commands`, where)
			}
			for _, ph := range placeholder.FindAllStringSubmatch(c.Prompt, -1) {
				if !validPlaceholder(ph[1]) {
					add(`%s: unknown placeholder {{%s}}; use {{args}} or {{1}} to {{9}}`, where, ph[1])
				}
			}
		}
		if c.TimeoutSec < 0 || c.TimeoutSec > MaxTimeout {
			add(`%s: "timeout_sec" must be between 1 and %d`, where, MaxTimeout)
		}
	}
	return p
}

func validateName(where, name string) []string {
	switch {
	case name == "":
		return []string{where + `: "name" is required`}
	case len(name) > maxItemName || !itemNameRE.MatchString(name):
		return []string{fmt.Sprintf(`%s: "name" must be 1-%d letters, digits, '-' or '_', starting with a letter or digit`, where, maxItemName)}
	}
	return nil
}

func validPlaceholder(s string) bool {
	if s == "args" {
		return true
	}
	n, err := strconv.Atoi(s)
	return err == nil && n >= 1 && n <= 9 && s == strconv.Itoa(n)
}

// Version is a parsed semantic version.
type Version struct {
	Major, Minor, Patch int
	Pre                 string
}

var versionRE = regexp.MustCompile(`^v?(0|[1-9]\d{0,8})\.(0|[1-9]\d{0,8})\.(0|[1-9]\d{0,8})(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$`)

// ParseVersion parses MAJOR.MINOR.PATCH with an optional leading v, an
// optional pre-release suffix (-rc.1) and optional build metadata.
func ParseVersion(s string) (Version, error) {
	m := versionRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return Version{}, fmt.Errorf("%q is not a semantic version (want MAJOR.MINOR.PATCH, e.g. 1.2.0)", s)
	}
	maj, _ := strconv.Atoi(m[1])
	mnr, _ := strconv.Atoi(m[2])
	pat, _ := strconv.Atoi(m[3])
	return Version{Major: maj, Minor: mnr, Patch: pat, Pre: m[4]}, nil
}

// AtLeast compares release numbers only: a development build such as
// 0.4.0-dev satisfies a plugin that needs 0.4.0.
func (v Version) AtLeast(o Version) bool {
	if v.Major != o.Major {
		return v.Major > o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor > o.Minor
	}
	return v.Patch >= o.Patch
}

func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Pre != "" {
		s += "-" + v.Pre
	}
	return s
}
