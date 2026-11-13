// Package policy loads and enforces a managed policy: rules an organization
// deploys to every machine that users cannot loosen.
//
// The policy lives at a system path (/etc/gta/policy.json on Linux,
// /Library/Application Support/GTA/policy.json on macOS,
// %ProgramData%\GTA\policy.json on Windows). GTA_POLICY_FILE names one more
// policy file; it is layered on top and can only add restrictions, so an
// environment variable can never switch the system policy off. A missing
// file means no policy; a file that exists but is invalid is an error, and
// gta must refuse to start rather than run unmanaged.
//
// The JSON is strict: unknown fields, unknown feature names, invalid
// patterns and trailing data are rejected, because a typo in a security
// policy must not silently weaken it.
//
// Every decision carries a reason that names the rule and the file it came
// from. Deny always wins over allow; with several policy files, every
// file's allow list must admit a call and any file's deny list refuses it.
package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/audit"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/budget"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permission"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/sandbox"
)

// EnvFile names an extra, stricter policy file.
const EnvFile = "GTA_POLICY_FILE"

// Version is the policy format version this build understands.
const Version = 1

// maxPolicyBytes bounds the policy file.
const maxPolicyBytes = 1 << 20

// Rules are allow and deny pattern lists.
type Rules struct {
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

// PathRules protect files from tools. Deny applies to reads and writes.
type PathRules struct {
	Deny      []string `json:"deny,omitempty"`
	DenyRead  []string `json:"deny_read,omitempty"`
	DenyWrite []string `json:"deny_write,omitempty"`
}

// SandboxRules force the shell sandbox.
type SandboxRules struct {
	// Mode is the weakest sandbox allowed: workspace-write or read-only.
	Mode string `json:"mode,omitempty"`
	// Network, when false, forces network access off inside the sandbox.
	Network *bool `json:"network,omitempty"`
}

// AuditRules control the audit log.
type AuditRules struct {
	// Required makes gta refuse to run when the audit log cannot be
	// written.
	Required bool `json:"required,omitempty"`
	// Dir overrides the log directory.
	Dir string `json:"dir,omitempty"`
	// Sync is always, interval or never.
	Sync string `json:"sync,omitempty"`
	// HMACKeyFile holds the key for an HMAC-keyed hash chain.
	HMACKeyFile string `json:"hmac_key_file,omitempty"`
}

// SecretRule is an organization-specific secret format.
type SecretRule struct {
	ID       string   `json:"id"`
	Pattern  string   `json:"pattern"`
	Keywords []string `json:"keywords,omitempty"`
}

// SecretsRules control secret redaction.
type SecretsRules struct {
	// Redact forces redaction of conversation content before it is sent to
	// the model.
	Redact bool `json:"redact,omitempty"`
	// Rules add detection rules.
	Rules []SecretRule `json:"rules,omitempty"`
}

// Policy is one policy file.
type Policy struct {
	Version int `json:"version"`
	// Message is shown to users (for example where to ask for exceptions).
	Message string `json:"message,omitempty"`

	// Tools match tool names, including mcp__server__tool and
	// plugin__name__tool, with * and ? wildcards.
	Tools Rules `json:"tools"`
	// Commands match each simple command the bash tool would run.
	Commands  Rules     `json:"commands"`
	Paths     PathRules `json:"paths"`
	Providers Rules     `json:"providers"`
	Models    Rules     `json:"models"`

	// MaxPermissionMode is the most permissive mode allowed:
	// plan < ask < auto-edit < yolo.
	MaxPermissionMode string       `json:"max_permission_mode,omitempty"`
	Sandbox           SandboxRules `json:"sandbox"`
	// DisabledFeatures turns off whole features; see Features.
	DisabledFeatures []string      `json:"disabled_features,omitempty"`
	Audit            AuditRules    `json:"audit"`
	Budget           budget.Limits `json:"budget"`
	Secrets          SecretsRules  `json:"secrets"`

	// Source is the file the policy was read from.
	Source string `json:"-"`
}

// Features lists the names DisabledFeatures accepts.
var Features = []string{FeatureWeb, FeatureWebSearch, FeatureWebFetch, FeatureMCP, FeatureHooks, FeaturePlugins, FeatureSubagents}

// Feature names.
const (
	FeatureWeb       = "web"
	FeatureWebSearch = "web_search"
	FeatureWebFetch  = "web_fetch"
	FeatureMCP       = "mcp"
	FeatureHooks     = "hooks"
	FeaturePlugins   = "plugins"
	FeatureSubagents = "subagents"
)

// featureTools are the tool names each feature provides.
var featureTools = map[string][]string{
	FeatureWeb:       {"web_search", "web_fetch"},
	FeatureWebSearch: {"web_search"},
	FeatureWebFetch:  {"web_fetch"},
	FeatureMCP:       {"mcp__*"},
	FeaturePlugins:   {"plugin__*"},
	FeatureSubagents: {"task"},
}

// SystemPath is the managed policy location for this platform.
func SystemPath() string { return systemPathFor(runtime.GOOS, os.Getenv) }

func systemPathFor(goos string, getenv func(string) string) string {
	switch goos {
	case "windows":
		base := getenv("ProgramData")
		if base == "" {
			base = `C:\ProgramData`
		}
		return base + `\GTA\policy.json`
	case "darwin":
		return "/Library/Application Support/GTA/policy.json"
	default:
		return "/etc/gta/policy.json"
	}
}

// Parse decodes and validates one policy file's contents.
func Parse(data []byte, source string) (*Policy, error) {
	if len(data) > maxPolicyBytes {
		return nil, fmt.Errorf("policy %s: larger than %d bytes", source, maxPolicyBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var p Policy
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("policy %s: %w", source, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("policy %s: unexpected data after the policy object", source)
	}
	p.Source = source
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("policy %s: %w", source, err)
	}
	return &p, nil
}

var secretRuleID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)

// Validate checks the policy.
func (p *Policy) Validate() error {
	if p.Version != Version {
		return fmt.Errorf("version must be %d, got %d", Version, p.Version)
	}
	check := func(field string, list []string, compile func(string) error) error {
		for i, s := range list {
			if err := compile(s); err != nil {
				return fmt.Errorf("%s[%d] %q: %w", field, i, s, err)
			}
		}
		return nil
	}
	name := func(s string) error { _, err := compileName(s, false); return err }
	cmd := func(s string) error { _, err := compileCommand(s); return err }
	// Validation uses a stand-in home so ~ patterns are checked for syntax.
	pth := func(s string) error { _, err := compilePath(s, "/home", false); return err }
	for _, c := range []struct {
		field string
		list  []string
		f     func(string) error
	}{
		{"tools.allow", p.Tools.Allow, name}, {"tools.deny", p.Tools.Deny, name},
		{"commands.allow", p.Commands.Allow, cmd}, {"commands.deny", p.Commands.Deny, cmd},
		{"paths.deny", p.Paths.Deny, pth}, {"paths.deny_read", p.Paths.DenyRead, pth}, {"paths.deny_write", p.Paths.DenyWrite, pth},
		{"providers.allow", p.Providers.Allow, name}, {"providers.deny", p.Providers.Deny, name},
		{"models.allow", p.Models.Allow, name}, {"models.deny", p.Models.Deny, name},
	} {
		if err := check(c.field, c.list, c.f); err != nil {
			return err
		}
	}
	if p.MaxPermissionMode != "" {
		if _, err := permission.ParseMode(p.MaxPermissionMode); err != nil {
			return fmt.Errorf("max_permission_mode: %w", err)
		}
	}
	if p.Sandbox.Mode != "" {
		if _, err := sandbox.ParseMode(p.Sandbox.Mode); err != nil {
			return fmt.Errorf("sandbox.mode: %w", err)
		}
	}
	for _, f := range p.DisabledFeatures {
		if !knownFeature(f) {
			return fmt.Errorf("disabled_features: unknown feature %q (want one of %s)", f, strings.Join(Features, ", "))
		}
	}
	if p.Audit.Sync != "" {
		if _, err := audit.ParseSyncMode(p.Audit.Sync); err != nil {
			return fmt.Errorf("audit.sync: %w", err)
		}
	}
	if err := p.Budget.Validate(); err != nil {
		return err
	}
	for i, r := range p.Secrets.Rules {
		if !secretRuleID.MatchString(r.ID) {
			return fmt.Errorf("secrets.rules[%d]: invalid id %q", i, r.ID)
		}
		if _, err := regexp.Compile(r.Pattern); err != nil || r.Pattern == "" {
			return fmt.Errorf("secrets.rules[%d] %s: invalid pattern: %v", i, r.ID, err)
		}
		for _, k := range r.Keywords {
			if len(k) < 2 {
				return fmt.Errorf("secrets.rules[%d] %s: keyword %q is shorter than 2 bytes", i, r.ID, k)
			}
		}
	}
	return nil
}

func knownFeature(f string) bool {
	for _, k := range Features {
		if k == f {
			return true
		}
	}
	return false
}

// LoadFile reads one policy file. A missing file returns (nil, nil).
func LoadFile(path string) (*Policy, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("policy %s: %w", path, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("policy %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("policy %s: not a regular file", path)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o002 != 0 {
		return nil, fmt.Errorf("policy %s: refusing a world-writable policy file (mode %v)", path, fi.Mode().Perm())
	}
	data, err := io.ReadAll(io.LimitReader(f, maxPolicyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("policy %s: %w", path, err)
	}
	return Parse(data, path)
}

// Load reads the system policy and the GTA_POLICY_FILE policy, if any.
func Load() (*Enforcer, error) {
	return LoadFiles(SystemPath(), os.Getenv(EnvFile))
}

// LoadFiles reads the given policy files, skipping empty and missing paths,
// and layers them. The first path is the most authoritative.
func LoadFiles(paths ...string) (*Enforcer, error) {
	var ps []*Policy
	seen := map[string]bool{}
	for _, p := range paths {
		if p == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err == nil {
			if seen[abs] {
				continue
			}
			seen[abs] = true
		}
		pol, err := LoadFile(p)
		if err != nil {
			return nil, err
		}
		if pol != nil {
			ps = append(ps, pol)
		}
	}
	return NewEnforcer(ps...)
}

// sortedCopy returns a sorted, de-duplicated copy.
func sortedCopy(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	n := 0
	for i, s := range out {
		if i == 0 || s != out[i-1] {
			out[n] = s
			n++
		}
	}
	return out[:n]
}
