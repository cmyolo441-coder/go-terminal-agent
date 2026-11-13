// Package hooks runs user-configured shell commands at points in the agent's
// lifecycle: before and after each tool call, when the user submits a
// prompt, when a turn finishes and when a session starts.
//
// Hooks are configured in JSON as a map from event name to an ordered list
// of hooks:
//
//	{
//	  "pre_tool_use": [
//	    {"matcher": "bash", "command": "./scripts/check-cmd.sh", "timeout_sec": 10}
//	  ],
//	  "post_tool_use": [
//	    {"matcher": "write_file|edit", "command": "gofmt -l . >&2 || true"}
//	  ],
//	  "user_prompt_submit": [{"command": "git status --short"}],
//	  "stop": [{"command": "notify-send 'GTA finished'"}],
//	  "session_start": [{"command": "cat .gta/context.md"}]
//	}
//
// Every hook runs as `bash -c <command>` (`sh -c` when bash is missing,
// `cmd /C` on Windows) in the workspace root. It receives a JSON [Payload]
// on stdin and the variables GTA_EVENT, GTA_TOOL_NAME, GTA_SESSION_ID and
// GTA_PROJECT_DIR in its environment.
//
// The exit code decides what happens next:
//
//   - 0: continue. Non-empty stdout is collected as feedback; for
//     user_prompt_submit and session_start it is collected as extra context
//     for the model instead.
//   - 2: block. stderr is the reason. The remaining hooks for the event are
//     skipped.
//   - anything else: a non-blocking warning carrying stderr.
//
// A hook may instead print a JSON object such as
// {"decision":"block","reason":"..."} or {"decision":"allow"} on stdout; a
// decision overrides the exit code.
package hooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Event names a point in the agent's lifecycle at which hooks run.
type Event string

const (
	// PreToolUse runs after a tool call is prepared and before it executes.
	// A block stops the tool from running; the reason goes to the model.
	PreToolUse Event = "pre_tool_use"
	// PostToolUse runs after a tool call finished, with its output. A block
	// does not undo the call; its reason should be appended to the tool
	// result so the model sees it.
	PostToolUse Event = "post_tool_use"
	// UserPromptSubmit runs when the user submits a prompt, before it is
	// sent to the model. stdout is extra context; a block rejects the prompt.
	UserPromptSubmit Event = "user_prompt_submit"
	// Stop runs when the agent finished a turn. A block asks the agent to
	// keep working, with the reason as the next instruction.
	Stop Event = "stop"
	// SessionStart runs once when a session starts or resumes. stdout is
	// extra context. A session cannot be blocked: exit 2 is a warning.
	SessionStart Event = "session_start"
)

// Events lists every supported event in lifecycle order.
var Events = []Event{SessionStart, UserPromptSubmit, PreToolUse, PostToolUse, Stop}

// Valid reports whether e is a supported event.
func (e Event) Valid() bool { return slices.Contains(Events, e) }

// usesMatcher reports whether hooks for e are filtered by tool name.
func (e Event) usesMatcher() bool { return e == PreToolUse || e == PostToolUse }

const (
	// DefaultTimeout bounds a hook that sets no timeout_sec.
	DefaultTimeout = 60 * time.Second
	// MaxTimeoutSec is the largest accepted timeout_sec.
	MaxTimeoutSec = 600
)

// Hook is one configured command.
type Hook struct {
	// Matcher is a regular expression that must match the whole tool name
	// (it is anchored as ^(?:matcher)$). Empty or "*" matches every tool.
	// Only pre_tool_use and post_tool_use hooks may set it.
	Matcher string `json:"matcher,omitempty"`
	// Command is the shell command to run.
	Command string `json:"command"`
	// TimeoutSec bounds the command; 0 means DefaultTimeout.
	TimeoutSec int `json:"timeout_sec,omitempty"`

	re *regexp.Regexp // compiled Matcher; nil matches everything
}

// matches reports whether the hook applies to toolName.
func (h *Hook) matches(toolName string) bool {
	return h.re == nil || h.re.MatchString(toolName)
}

// timeout returns the effective timeout of the hook.
func (h *Hook) timeout() time.Duration {
	if h.TimeoutSec <= 0 {
		return DefaultTimeout
	}
	return time.Duration(h.TimeoutSec) * time.Second
}

// label is a short, single-line name for the hook used in messages.
func (h *Hook) label() string {
	s := strings.Join(strings.Fields(h.Command), " ")
	if r := []rune(s); len(r) > 60 {
		s = string(r[:59]) + "…"
	}
	return s
}

// Config maps each event to the hooks that run for it, in order.
type Config map[Event][]Hook

// Validate checks every hook and compiles the matchers once, storing the
// compiled form in the hooks themselves. It reports every problem found,
// not only the first.
func (c Config) Validate() error {
	var errs []error
	for _, ev := range sortedEvents(c) {
		list := c[ev]
		if !ev.Valid() {
			errs = append(errs, fmt.Errorf("hooks: unknown event %q (want one of %s)", ev, eventNames()))
			continue
		}
		for i := range list {
			h := &list[i]
			where := fmt.Sprintf("hooks: %s[%d]", ev, i)
			if strings.TrimSpace(h.Command) == "" {
				errs = append(errs, fmt.Errorf("%s: command is empty", where))
			}
			if h.TimeoutSec < 0 || h.TimeoutSec > MaxTimeoutSec {
				errs = append(errs, fmt.Errorf("%s: timeout_sec %d is out of range (0 to %d)", where, h.TimeoutSec, MaxTimeoutSec))
			}
			h.re = nil
			m := strings.TrimSpace(h.Matcher)
			if m == "" || m == "*" {
				continue
			}
			if !ev.usesMatcher() {
				errs = append(errs, fmt.Errorf("%s: matcher is only supported for %s and %s", where, PreToolUse, PostToolUse))
				continue
			}
			re, err := regexp.Compile("^(?:" + m + ")$")
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: invalid matcher %q: %v", where, h.Matcher, err))
				continue
			}
			h.re = re
		}
	}
	return errors.Join(errs...)
}

// Count returns the total number of configured hooks.
func (c Config) Count() int {
	n := 0
	for _, list := range c {
		n += len(list)
	}
	return n
}

// clone returns a deep copy, so a Runner never shares state with the caller.
func (c Config) clone() Config {
	out := make(Config, len(c))
	for ev, list := range c {
		if len(list) > 0 {
			out[ev] = slices.Clone(list)
		}
	}
	return out
}

// Parse decodes and validates a hooks configuration. Unknown fields are
// rejected so that typos do not silently disable a hook.
func Parse(data []byte) (Config, error) {
	var c Config
	if len(bytes.TrimSpace(data)) == 0 {
		return Config{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("hooks: parse config: %w", err)
	}
	if dec.More() {
		return nil, errors.New("hooks: parse config: trailing data after JSON object")
	}
	if c == nil {
		c = Config{}
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// LoadFile reads and validates a hooks configuration file. A missing file is
// an empty configuration, not an error.
func LoadFile(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("hooks: %w", err)
	}
	c, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// sortedEvents returns the configured events in lifecycle order, followed by
// any unknown ones sorted by name, so validation errors are deterministic.
func sortedEvents(c Config) []Event {
	var known, unknown []Event
	for _, ev := range Events {
		if _, ok := c[ev]; ok {
			known = append(known, ev)
		}
	}
	for ev := range c {
		if !ev.Valid() {
			unknown = append(unknown, ev)
		}
	}
	slices.Sort(unknown)
	return append(known, unknown...)
}

func eventNames() string {
	names := make([]string, len(Events))
	for i, ev := range Events {
		names[i] = string(ev)
	}
	return strings.Join(names, ", ")
}
