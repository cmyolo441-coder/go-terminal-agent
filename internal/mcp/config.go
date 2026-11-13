package mcp

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// DefaultTimeout bounds a single request to a server when its config sets no
// timeout_sec. It covers the initialize handshake and every tool call.
const DefaultTimeout = 60 * time.Second

// MaxTimeout is the largest timeout_sec a server may configure.
const MaxTimeout = time.Hour

// ServerConfig describes how to reach one MCP server. Exactly one of Command
// (a local process spoken to over stdio) or URL (a Streamable HTTP endpoint)
// is set.
//
// String values in Args, Env, URL and Headers may reference environment
// variables as ${NAME} or ${NAME:-default}, so secrets such as API tokens can
// stay out of the config file. A reference to an unset variable without a
// default is an error when the server starts.
type ServerConfig struct {
	// Command is the executable to spawn for a stdio server.
	Command string `json:"command,omitempty"`
	// Args are passed to Command.
	Args []string `json:"args,omitempty"`
	// Env adds or overrides variables in the process environment.
	Env map[string]string `json:"env,omitempty"`
	// URL is the Streamable HTTP endpoint of a remote server.
	URL string `json:"url,omitempty"`
	// Headers are sent with every HTTP request, e.g. Authorization.
	Headers map[string]string `json:"headers,omitempty"`
	// Disabled keeps the server configured but not started.
	Disabled bool `json:"disabled,omitempty"`
	// TimeoutSec bounds each request; zero means DefaultTimeout.
	TimeoutSec int `json:"timeout_sec,omitempty"`
}

// Config maps server names to their settings. It is meant to be embedded in
// the application config as a field such as
//
//	MCPServers mcp.Config `json:"mcp_servers,omitempty"`
type Config map[string]ServerConfig

var (
	serverNameRE = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
	headerNameRE = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
	envNameRE    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// ValidateName checks that a server name is 1-64 characters of
// [a-zA-Z0-9_-]. Names become part of tool names, so the alphabet is strict.
func ValidateName(name string) error {
	if !serverNameRE.MatchString(name) {
		return fmt.Errorf("invalid MCP server name %q: use 1-64 characters from [a-zA-Z0-9_-]", name)
	}
	return nil
}

// Transport reports "stdio" or "http" for a valid config.
func (s ServerConfig) Transport() string {
	if s.URL != "" {
		return "http"
	}
	return "stdio"
}

// Target is a short human description of where the server lives: the
// command line for stdio, or the URL without credentials for HTTP.
func (s ServerConfig) Target() string {
	if s.URL != "" {
		return redactURL(s.URL)
	}
	return strings.TrimSpace(strings.Join(append([]string{s.Command}, s.Args...), " "))
}

// Timeout returns the per-request timeout.
func (s ServerConfig) Timeout() time.Duration {
	if s.TimeoutSec <= 0 {
		return DefaultTimeout
	}
	return time.Duration(s.TimeoutSec) * time.Second
}

// Validate checks one server's settings. It does not expand variables, so a
// config that references an unset variable still validates; Connect reports
// that instead.
func (s ServerConfig) Validate(name string) error {
	var errs []error
	if err := ValidateName(name); err != nil {
		errs = append(errs, err)
	}
	hasCmd := strings.TrimSpace(s.Command) != ""
	hasURL := strings.TrimSpace(s.URL) != ""
	switch {
	case hasCmd && hasURL:
		errs = append(errs, errors.New("set either command or url, not both"))
	case !hasCmd && !hasURL:
		errs = append(errs, errors.New("set command (stdio server) or url (HTTP server)"))
	}
	if hasURL {
		if len(s.Args) > 0 || len(s.Env) > 0 {
			errs = append(errs, errors.New("args and env apply only to command servers"))
		}
		if !strings.Contains(s.URL, "${") {
			if err := checkURL(s.URL); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if hasCmd && len(s.Headers) > 0 {
		errs = append(errs, errors.New("headers apply only to url servers"))
	}
	for _, k := range sortedKeys(s.Env) {
		if !envNameRE.MatchString(k) {
			errs = append(errs, fmt.Errorf("invalid env variable name %q", k))
		}
		if strings.ContainsRune(s.Env[k], 0) {
			errs = append(errs, fmt.Errorf("env %s contains a NUL byte", k))
		}
	}
	for _, k := range sortedKeys(s.Headers) {
		if !headerNameRE.MatchString(k) {
			errs = append(errs, fmt.Errorf("invalid header name %q", k))
		}
		if strings.ContainsAny(s.Headers[k], "\r\n\x00") {
			errs = append(errs, fmt.Errorf("header %s contains a line break or NUL", k))
		}
		switch strings.ToLower(k) {
		case "mcp-session-id", "mcp-protocol-version", "content-type", "accept", "content-length", "last-event-id":
			errs = append(errs, fmt.Errorf("header %s is managed by the client and cannot be set", k))
		}
	}
	if s.TimeoutSec < 0 {
		errs = append(errs, errors.New("timeout_sec must not be negative"))
	}
	if time.Duration(s.TimeoutSec)*time.Second > MaxTimeout {
		errs = append(errs, fmt.Errorf("timeout_sec must be at most %d", int(MaxTimeout/time.Second)))
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("mcp server %q: %w", name, errors.Join(errs...))
}

// Validate checks every server, reporting all problems at once in name order.
func (c Config) Validate() error {
	var errs []error
	for _, name := range c.Names() {
		if err := c[name].Validate(name); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Names lists the configured server names in sorted order.
func (c Config) Names() []string { return sortedKeys(c) }

// expanded returns a copy with ${VAR} references resolved via lookup.
func (s ServerConfig) expanded(lookup func(string) (string, bool)) (ServerConfig, error) {
	out := s
	var err error
	exp := func(field, v string) string {
		if err != nil {
			return v
		}
		r, e := expandVars(v, lookup)
		if e != nil {
			err = fmt.Errorf("%s: %w", field, e)
		}
		return r
	}
	out.Command = exp("command", s.Command)
	out.URL = exp("url", s.URL)
	if s.Args != nil {
		out.Args = make([]string, len(s.Args))
		for i, a := range s.Args {
			out.Args[i] = exp(fmt.Sprintf("args[%d]", i), a)
		}
	}
	if s.Env != nil {
		out.Env = make(map[string]string, len(s.Env))
		for _, k := range sortedKeys(s.Env) {
			out.Env[k] = exp("env "+k, s.Env[k])
		}
	}
	if s.Headers != nil {
		out.Headers = make(map[string]string, len(s.Headers))
		for _, k := range sortedKeys(s.Headers) {
			out.Headers[k] = exp("header "+k, s.Headers[k])
		}
	}
	if err != nil {
		return ServerConfig{}, err
	}
	if out.URL != "" {
		if e := checkURL(out.URL); e != nil {
			return ServerConfig{}, e
		}
	}
	for _, k := range sortedKeys(out.Headers) {
		if strings.ContainsAny(out.Headers[k], "\r\n\x00") {
			return ServerConfig{}, fmt.Errorf("header %s contains a line break or NUL after expansion", k)
		}
	}
	return out, nil
}

// expandVars replaces ${NAME} and ${NAME:-default}. "$$" is a literal "$".
// A bare $NAME is left untouched so ordinary dollar signs survive.
func expandVars(s string, lookup func(string) (string, bool)) (string, error) {
	if !strings.Contains(s, "$") {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '$' || i+1 >= len(s) {
			b.WriteByte(c)
			continue
		}
		switch s[i+1] {
		case '$':
			b.WriteByte('$')
			i++
		case '{':
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				return "", fmt.Errorf("unterminated ${ in %q", s)
			}
			expr := s[i+2 : i+2+end]
			name, def, hasDef := strings.Cut(expr, ":-")
			if !envNameRE.MatchString(name) {
				return "", fmt.Errorf("invalid variable reference ${%s}", expr)
			}
			v, ok := lookup(name)
			if !ok || (hasDef && v == "") {
				if !hasDef {
					return "", fmt.Errorf("environment variable %s is not set", name)
				}
				v = def
			}
			b.WriteString(v)
			i += 2 + end
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), nil
}

func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("url %q must use http or https", redactURL(raw))
	}
	if u.Host == "" {
		return fmt.Errorf("url %q has no host", redactURL(raw))
	}
	return nil
}

// redactURL drops user info and the query string, which often carry tokens.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(invalid url)"
	}
	u.User = nil
	if u.RawQuery != "" {
		u.RawQuery = "…"
	}
	u.Fragment = ""
	return u.String()
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// mergeEnv overlays extra on base (KEY=VALUE pairs), replacing existing keys.
func mergeEnv(base []string, extra map[string]string) []string {
	if len(extra) == 0 {
		return base
	}
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if _, over := extra[k]; over {
			continue
		}
		out = append(out, kv)
	}
	for _, k := range sortedKeys(extra) {
		out = append(out, k+"="+extra[k])
	}
	return out
}

func osLookup(name string) (string, bool) { return os.LookupEnv(name) }
