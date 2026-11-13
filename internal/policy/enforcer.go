package policy

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/audit"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/budget"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/config"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permission"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/sandbox"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/secrets"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

// Decision is the outcome of a policy check.
type Decision struct {
	Allowed bool
	// Reason explains the decision in a sentence.
	Reason string
	// Rule names the deciding rule, e.g. `tools.deny["mcp__*"]`.
	Rule string
	// Source is the policy file the rule came from.
	Source string
	// Message is that policy's message for users, on denials.
	Message string
}

// String is the reason with the rule and source, for logs and errors.
func (d Decision) String() string {
	if d.Rule == "" {
		return d.Reason
	}
	return fmt.Sprintf("%s [%s in %s]", d.Reason, d.Rule, d.Source)
}

// UserText is what to show the user for a denial: the reason and the
// policy's message.
func (d Decision) UserText() string {
	if d.Message == "" {
		return d.Reason
	}
	return d.Reason + ". " + d.Message
}

// Record converts the decision to an audit record about subject.
func (d Decision) Record(subject string) audit.Record {
	verdict := audit.DecisionAllow
	if !d.Allowed {
		verdict = audit.DecisionDeny
	}
	details := map[string]string{}
	if d.Rule != "" {
		details["rule"] = d.Rule
	}
	if d.Source != "" {
		details["source"] = d.Source
	}
	return audit.PolicyDecision(subject, verdict, d.Reason, details)
}

var allowed = Decision{Allowed: true, Reason: "allowed by policy"}

// Change is one adjustment ClampConfig or ClampBudget made.
type Change struct {
	Field  string
	From   string
	To     string
	Reason string
	Source string
}

func (c Change) String() string {
	return fmt.Sprintf("%s: %s → %s (%s; %s)", c.Field, c.From, c.To, c.Reason, c.Source)
}

// layer is one compiled policy.
type layer struct {
	p                         *Policy
	toolAllow, toolDeny       []namePattern
	cmdAllow, cmdDeny         []cmdPattern
	deny, denyRead, denyWrite []pathPattern
	provAllow, provDeny       []namePattern
	modelAllow, modelDeny     []namePattern
	disabled                  map[string]bool
}

func (l *layer) refuse(reason, rule string) Decision {
	return Decision{Reason: reason, Rule: rule, Source: l.p.Source, Message: l.p.Message}
}

// Enforcer applies one or more policies. A nil or empty Enforcer allows
// everything. It is immutable and safe for concurrent use.
type Enforcer struct {
	layers []*layer
	home   string
	fold   bool
}

// NewEnforcer compiles policies, most authoritative first. Paths are
// compared case-insensitively on macOS and Windows, whose default file
// systems are.
func NewEnforcer(ps ...*Policy) (*Enforcer, error) {
	home, _ := os.UserHomeDir()
	return newEnforcer(home, runtime.GOOS == "darwin" || runtime.GOOS == "windows", ps...)
}

func newEnforcer(home string, fold bool, ps ...*Policy) (*Enforcer, error) {
	e := &Enforcer{home: home, fold: fold}
	for _, p := range ps {
		if p == nil {
			continue
		}
		if err := p.Validate(); err != nil {
			return nil, fmt.Errorf("policy %s: %w", p.Source, err)
		}
		l := &layer{p: p, disabled: map[string]bool{}}
		var err error
		names := func(list []string, fold bool) []namePattern {
			var out []namePattern
			for _, s := range list {
				np, cerr := compileName(s, fold)
				if cerr != nil && err == nil {
					err = cerr
				}
				out = append(out, np)
			}
			return out
		}
		cmds := func(list []string) []cmdPattern {
			var out []cmdPattern
			for _, s := range list {
				cp, cerr := compileCommand(s)
				if cerr != nil && err == nil {
					err = cerr
				}
				out = append(out, cp)
			}
			return out
		}
		paths := func(list []string) []pathPattern {
			var out []pathPattern
			for _, s := range list {
				pp, cerr := compilePath(s, home, fold)
				if cerr != nil && err == nil {
					err = cerr
				}
				out = append(out, pp)
			}
			return out
		}
		l.toolAllow, l.toolDeny = names(p.Tools.Allow, false), names(p.Tools.Deny, false)
		l.cmdAllow, l.cmdDeny = cmds(p.Commands.Allow), cmds(p.Commands.Deny)
		l.deny, l.denyRead, l.denyWrite = paths(p.Paths.Deny), paths(p.Paths.DenyRead), paths(p.Paths.DenyWrite)
		l.provAllow, l.provDeny = names(p.Providers.Allow, true), names(p.Providers.Deny, true)
		l.modelAllow, l.modelDeny = names(p.Models.Allow, true), names(p.Models.Deny, true)
		for _, f := range p.DisabledFeatures {
			l.disabled[f] = true
		}
		if err != nil {
			return nil, fmt.Errorf("policy %s: %w", p.Source, err)
		}
		e.layers = append(e.layers, l)
	}
	return e, nil
}

// Active reports whether any policy is in force.
func (e *Enforcer) Active() bool { return e != nil && len(e.layers) > 0 }

// Sources lists the policy files in force.
func (e *Enforcer) Sources() []string {
	if e == nil {
		return nil
	}
	var out []string
	for _, l := range e.layers {
		out = append(out, l.p.Source)
	}
	return out
}

// Message returns the policies' messages for users, joined.
func (e *Enforcer) Message() string {
	if e == nil {
		return ""
	}
	var msgs []string
	for _, l := range e.layers {
		if m := strings.TrimSpace(l.p.Message); m != "" {
			msgs = append(msgs, m)
		}
	}
	return strings.Join(msgs, "\n")
}

// ToolRequest describes a tool call for CheckTool.
type ToolRequest struct {
	Name string
	Kind tools.Kind
	// Path is the file the call reads or writes, if any.
	Path string
	// Command is the shell command line when Shell is set.
	Command string
	Shell   bool
	// Root resolves relative paths (the workspace root).
	Root string
}

// shellTools run their Command through a shell.
var shellTools = map[string]bool{"bash": true}

// RequestFor builds a ToolRequest from a prepared call and its raw input.
// Reads take their path from the input's "path" field, since a read call
// carries no Path.
func RequestFor(call *tools.Call, input json.RawMessage, root string) ToolRequest {
	r := ToolRequest{Name: call.Tool, Kind: call.Kind, Path: call.Path, Root: root}
	if shellTools[call.Tool] {
		r.Command, r.Shell = call.Command, true
	}
	if r.Path == "" && len(input) > 0 {
		var in struct {
			Path     string `json:"path"`
			FilePath string `json:"file_path"`
		}
		if json.Unmarshal(input, &in) == nil {
			r.Path = in.Path
			if r.Path == "" {
				r.Path = in.FilePath
			}
		}
	}
	return r
}

// CheckTool decides whether a tool call may run. It runs before the
// permission gate: a denial here cannot be approved by the user.
func (e *Enforcer) CheckTool(req ToolRequest) Decision {
	if !e.Active() {
		return allowed
	}
	for _, l := range e.layers {
		for f := range l.disabled {
			for _, pat := range featureTools[f] {
				if globMatch(pat, req.Name) {
					return l.refuse(fmt.Sprintf("tool %q is unavailable: the %s feature is disabled by policy", req.Name, f),
						fmt.Sprintf("disabled_features[%q]", f))
				}
			}
		}
		for _, p := range l.toolDeny {
			if p.match(req.Name) {
				return l.refuse(fmt.Sprintf("tool %q is denied by policy", req.Name), fmt.Sprintf("tools.deny[%q]", p.raw))
			}
		}
	}
	for _, l := range e.layers {
		if len(l.toolAllow) > 0 && !anyName(l.toolAllow, req.Name) {
			return l.refuse(fmt.Sprintf("tool %q is not on the policy's allowed tools", req.Name), "tools.allow")
		}
	}
	if req.Path != "" && req.Kind != tools.KindExec {
		write := req.Kind == tools.KindWrite
		if d, denied := e.checkPath(req.Path, req.Root, write, !write); denied {
			return d
		}
	}
	if req.Shell && strings.TrimSpace(req.Command) != "" {
		return e.CheckCommand(req.Command, req.Root)
	}
	return allowed
}

func anyName(ps []namePattern, s string) bool {
	for _, p := range ps {
		if p.match(s) {
			return true
		}
	}
	return false
}

// CheckPath decides whether a file may be read (write false) or written.
// Tools that walk directories should call it for every file they open.
func (e *Enforcer) CheckPath(path, root string, write bool) Decision {
	if !e.Active() {
		return allowed
	}
	if d, denied := e.checkPath(path, root, write, !write); denied {
		return d
	}
	return allowed
}

func (e *Enforcer) checkPath(path, root string, write, read bool) (Decision, bool) {
	cands := pathCandidates(path, root, e.home, e.fold)
	for _, l := range e.layers {
		type set struct {
			name string
			ps   []pathPattern
			on   bool
		}
		for _, s := range []set{{"paths.deny", l.deny, true}, {"paths.deny_write", l.denyWrite, write}, {"paths.deny_read", l.denyRead, read}} {
			if !s.on {
				continue
			}
			for _, p := range s.ps {
				for _, c := range cands {
					if p.match(c) {
						verb := "reading"
						if write {
							verb = "writing"
						}
						if read && write {
							verb = "accessing"
						}
						return l.refuse(fmt.Sprintf("%s %s is denied by policy", verb, path), fmt.Sprintf("%s[%q]", s.name, p.raw)), true
					}
				}
			}
		}
	}
	return Decision{}, false
}

// CheckCommand decides whether a shell command line may run. Every simple
// command in it, including those inside $(...), backticks, pipelines,
// sh -c strings and wrappers such as sudo or env, must pass the command
// rules, and no argument or redirection may name a protected path.
func (e *Enforcer) CheckCommand(cmd, root string) Decision {
	if !e.Active() {
		return allowed
	}
	whole := strings.Join(strings.Fields(cmd), " ")
	segs := parseShell(cmd)
	for _, l := range e.layers {
		for _, p := range l.cmdDeny {
			if p.re != nil && p.match(whole) {
				return l.refuse("command is denied by policy", fmt.Sprintf("commands.deny[%q]", p.raw))
			}
		}
	}
	for _, s := range segs {
		text := s.text()
		for _, l := range e.layers {
			for _, p := range l.cmdDeny {
				if s.prog != "" && p.match(text) {
					return l.refuse(fmt.Sprintf("command `%s` is denied by policy", clip(text)), fmt.Sprintf("commands.deny[%q]", p.raw))
				}
			}
		}
		for _, l := range e.layers {
			if len(l.cmdAllow) == 0 || s.prog == "" {
				continue
			}
			ok := false
			for _, p := range l.cmdAllow {
				if p.match(text) {
					ok = true
					break
				}
			}
			if !ok {
				return l.refuse(fmt.Sprintf("command `%s` is not on the policy's allowed commands", clip(text)), "commands.allow")
			}
		}
		for _, a := range pathArgs(s) {
			if d, denied := e.checkPath(a.path, root, a.write || !a.read, a.read || !a.write); denied {
				d.Reason = fmt.Sprintf("command `%s` touches %s, which is protected by policy", clip(text), a.path)
				return d
			}
		}
	}
	return allowed
}

type pathArg struct {
	path        string
	read, write bool
}

// pathArgs lists arguments that may name files: every non-flag argument,
// the value of --flag=value, and redirection targets. URLs and the
// substitution marker are skipped. Plain arguments count as both read and
// write, since the program decides which it does.
func pathArgs(s segment) []pathArg {
	var out []pathArg
	add := func(a string, read, write bool) {
		if a == "" || a == "-" || strings.Contains(a, "://") || strings.Contains(a, "$(…)") || a == "/dev/null" {
			return
		}
		out = append(out, pathArg{path: a, read: read, write: write})
	}
	for _, a := range s.args {
		if strings.HasPrefix(a, "-") {
			if _, v, ok := strings.Cut(a, "="); ok {
				add(v, true, true)
			}
			continue
		}
		add(a, true, true)
	}
	for _, a := range s.redirIn {
		add(a, true, false)
	}
	for _, a := range s.redirOut {
		add(a, false, true)
	}
	return out
}

func clip(s string) string {
	if len(s) > 120 {
		return s[:117] + "..."
	}
	return s
}

// CheckModel decides whether a provider and model may be used. An empty
// provider means anthropic.
func (e *Enforcer) CheckModel(provider, model string) Decision {
	if !e.Active() {
		return allowed
	}
	if provider == "" {
		provider = "anthropic"
	}
	for _, l := range e.layers {
		for _, p := range l.provDeny {
			if p.match(provider) {
				return l.refuse(fmt.Sprintf("provider %q is denied by policy", provider), fmt.Sprintf("providers.deny[%q]", p.raw))
			}
		}
		for _, p := range l.modelDeny {
			if p.match(model) {
				return l.refuse(fmt.Sprintf("model %q is denied by policy", model), fmt.Sprintf("models.deny[%q]", p.raw))
			}
		}
	}
	for _, l := range e.layers {
		if len(l.provAllow) > 0 && !anyName(l.provAllow, provider) {
			return l.refuse(fmt.Sprintf("provider %q is not on the policy's allowed providers (%s)", provider, strings.Join(l.p.Providers.Allow, ", ")), "providers.allow")
		}
		if len(l.modelAllow) > 0 && !anyName(l.modelAllow, model) {
			return l.refuse(fmt.Sprintf("model %q is not on the policy's allowed models (%s)", model, strings.Join(l.p.Models.Allow, ", ")), "models.allow")
		}
	}
	return allowed
}

// FeatureEnabled reports whether a feature is allowed. Disabling "web"
// disables web_search and web_fetch.
func (e *Enforcer) FeatureEnabled(feature string) bool {
	if !e.Active() {
		return true
	}
	for _, l := range e.layers {
		if l.disabled[feature] {
			return false
		}
		if (feature == FeatureWebSearch || feature == FeatureWebFetch) && l.disabled[FeatureWeb] {
			return false
		}
		if feature == FeatureWeb && l.disabled[FeatureWebSearch] && l.disabled[FeatureWebFetch] {
			return false
		}
	}
	return true
}

var modeRank = map[permission.Mode]int{permission.ModePlan: 0, permission.ModeAsk: 1, permission.ModeAutoEdit: 2, permission.ModeYolo: 3}

// MaxMode returns the most permissive permission mode allowed, and false
// when the policy sets none.
func (e *Enforcer) MaxMode() (permission.Mode, bool) {
	if !e.Active() {
		return "", false
	}
	best, found := permission.Mode(""), false
	for _, l := range e.layers {
		if l.p.MaxPermissionMode == "" {
			continue
		}
		m := permission.Mode(l.p.MaxPermissionMode)
		if !found || modeRank[m] < modeRank[best] {
			best, found = m, true
		}
	}
	return best, found
}

// ClampMode returns m, or the policy's maximum when m is more permissive.
// The decision explains a clamp. Call it at startup and whenever the user
// switches modes.
func (e *Enforcer) ClampMode(m permission.Mode) (permission.Mode, Decision) {
	if !e.Active() {
		return m, allowed
	}
	for _, l := range e.layers {
		if l.p.MaxPermissionMode == "" {
			continue
		}
		max := permission.Mode(l.p.MaxPermissionMode)
		if rank, ok := modeRank[m]; ok && rank > modeRank[max] {
			lim, _ := e.MaxMode()
			return lim, l.refuse(fmt.Sprintf("permission mode %q is not allowed; the policy allows up to %q", m, lim), "max_permission_mode")
		}
	}
	return m, allowed
}

var sandboxRank = map[sandbox.Mode]int{sandbox.ModeOff: 0, sandbox.ModeWorkspaceWrite: 1, sandbox.ModeReadOnly: 2}

// ForcedSandbox returns the weakest sandbox mode allowed, and false when
// the policy forces none.
func (e *Enforcer) ForcedSandbox() (sandbox.Mode, bool) {
	if !e.Active() {
		return "", false
	}
	best, found := sandbox.ModeOff, false
	for _, l := range e.layers {
		if l.p.Sandbox.Mode == "" {
			continue
		}
		found = true
		if m, _ := sandbox.ParseMode(l.p.Sandbox.Mode); sandboxRank[m] > sandboxRank[best] {
			best = m
		}
	}
	return best, found
}

// NetworkForcedOff reports whether the policy turns sandbox network off.
func (e *Enforcer) NetworkForcedOff() bool {
	if !e.Active() {
		return false
	}
	for _, l := range e.layers {
		if l.p.Sandbox.Network != nil && !*l.p.Sandbox.Network {
			return true
		}
	}
	return false
}

// Budget returns the strictest budget caps across policies.
func (e *Enforcer) Budget() budget.Limits {
	var b budget.Limits
	if !e.Active() {
		return b
	}
	for _, l := range e.layers {
		b = b.Tighten(l.p.Budget)
	}
	return b
}

// ClampBudget tightens a user's caps to the policy's.
func (e *Enforcer) ClampBudget(user budget.Limits) (budget.Limits, []Change) {
	if !e.Active() {
		return user, nil
	}
	out := user
	var changes []Change
	for _, l := range e.layers {
		next := out.Tighten(l.p.Budget)
		f := func(field string, from, to float64) {
			if from != to {
				changes = append(changes, Change{Field: field, From: money(from), To: money(to),
					Reason: "capped by policy", Source: l.p.Source})
			}
		}
		f("budget.session_usd", out.SessionUSD, next.SessionUSD)
		f("budget.daily_usd", out.DailyUSD, next.DailyUSD)
		f("budget.monthly_usd", out.MonthlyUSD, next.MonthlyUSD)
		if out.WarnAt != next.WarnAt {
			changes = append(changes, Change{Field: "budget.warn_at", From: strconv.FormatFloat(out.WarnAt, 'g', -1, 64),
				To: strconv.FormatFloat(next.WarnAt, 'g', -1, 64), Reason: "set by policy", Source: l.p.Source})
		}
		out = next
	}
	return out, changes
}

func money(v float64) string {
	if v <= 0 {
		return "unlimited"
	}
	return "$" + strconv.FormatFloat(v, 'f', 2, 64)
}

// AuditRequirement is the merged audit configuration.
type AuditRequirement struct {
	Required    bool
	Dir         string
	Sync        audit.SyncMode
	HMACKeyFile string
	// Source names the policy that made the log required.
	Source string
}

var syncRank = map[audit.SyncMode]int{audit.SyncNever: 0, audit.SyncInterval: 1, audit.SyncAlways: 2}

// Audit merges the policies' audit settings: required if any policy
// requires it, the first directory and key file given, and the most
// durable sync mode.
func (e *Enforcer) Audit() AuditRequirement {
	var a AuditRequirement
	if !e.Active() {
		return a
	}
	for _, l := range e.layers {
		r := l.p.Audit
		if r.Required && !a.Required {
			a.Required, a.Source = true, l.p.Source
		}
		if a.Dir == "" {
			a.Dir = r.Dir
		}
		if a.HMACKeyFile == "" {
			a.HMACKeyFile = r.HMACKeyFile
		}
		if r.Sync != "" {
			m, _ := audit.ParseSyncMode(r.Sync)
			if a.Sync == "" || syncRank[m] > syncRank[a.Sync] {
				a.Sync = m
			}
		}
	}
	return a
}

// Secrets returns whether redaction is forced and the policies' extra
// secret rules.
func (e *Enforcer) Secrets() (bool, []secrets.Rule, error) {
	if !e.Active() {
		return false, nil, nil
	}
	redact := false
	var rules []secrets.Rule
	seen := map[string]bool{}
	for _, l := range e.layers {
		redact = redact || l.p.Secrets.Redact
		for _, r := range l.p.Secrets.Rules {
			if seen[r.ID] {
				continue
			}
			seen[r.ID] = true
			re, err := regexp.Compile(r.Pattern)
			if err != nil {
				return false, nil, fmt.Errorf("policy %s: secrets rule %s: %w", l.p.Source, r.ID, err)
			}
			rules = append(rules, secrets.Rule{ID: r.ID, Description: "policy rule from " + l.p.Source, Keywords: r.Keywords, Pattern: re})
		}
	}
	return redact, rules, nil
}

// ClampConfig adjusts cfg to the policy and reports every change. It
// returns an error for violations it cannot fix by itself, such as a
// provider or model the policy does not allow; gta must not start then.
func (e *Enforcer) ClampConfig(cfg *config.Config) ([]Change, error) {
	if !e.Active() {
		return nil, nil
	}
	var changes []Change
	src := func() string { return strings.Join(e.Sources(), ", ") }
	if d := e.CheckModel(cfg.Provider, cfg.Model); !d.Allowed {
		return nil, fmt.Errorf("%s", d.UserText())
	}
	if cfg.SubagentModel != "" {
		if d := e.CheckModel(cfg.Provider, cfg.SubagentModel); !d.Allowed {
			changes = append(changes, Change{Field: "subagent_model", From: cfg.SubagentModel, To: "(main model)", Reason: d.Reason, Source: d.Source})
			cfg.SubagentModel = ""
		}
	}
	if m, d := e.ClampMode(permission.Mode(cfg.PermissionMode)); !d.Allowed {
		changes = append(changes, Change{Field: "permission_mode", From: cfg.PermissionMode, To: string(m), Reason: d.Reason, Source: d.Source})
		cfg.PermissionMode = string(m)
	}
	if forced, ok := e.ForcedSandbox(); ok {
		cur, err := sandbox.ParseMode(cfg.Sandbox)
		if err != nil || sandboxRank[cur] < sandboxRank[forced] {
			changes = append(changes, Change{Field: "sandbox", From: orNone(cfg.Sandbox), To: string(forced), Reason: "sandbox required by policy", Source: src()})
			cfg.Sandbox = string(forced)
		}
	}
	if cfg.SandboxNetwork && e.NetworkForcedOff() {
		changes = append(changes, Change{Field: "sandbox_network", From: "true", To: "false", Reason: "network inside the sandbox is disabled by policy", Source: src()})
		cfg.SandboxNetwork = false
	}
	if cfg.WebSearch && !e.FeatureEnabled(FeatureWebSearch) {
		changes = append(changes, Change{Field: "web_search", From: "true", To: "false", Reason: "web search is disabled by policy", Source: src()})
		cfg.WebSearch = false
	}
	if cfg.WebFetch && !e.FeatureEnabled(FeatureWebFetch) {
		changes = append(changes, Change{Field: "web_fetch", From: "true", To: "false", Reason: "web fetch is disabled by policy", Source: src()})
		cfg.WebFetch = false
	}
	if cfg.ClientWebTools && !e.FeatureEnabled(FeatureWeb) {
		changes = append(changes, Change{Field: "client_web_tools", From: "true", To: "false", Reason: "web tools are disabled by policy", Source: src()})
		cfg.ClientWebTools = false
	}
	if !cfg.DisablePlugins && !e.FeatureEnabled(FeaturePlugins) {
		changes = append(changes, Change{Field: "disable_plugins", From: "false", To: "true", Reason: "plugins are disabled by policy", Source: src()})
		cfg.DisablePlugins = true
	}
	if len(cfg.MCPServers) > 0 && !e.FeatureEnabled(FeatureMCP) {
		changes = append(changes, Change{Field: "mcp_servers", From: strconv.Itoa(len(cfg.MCPServers)) + " servers", To: "none", Reason: "MCP is disabled by policy", Source: src()})
		cfg.MCPServers = nil
	}
	if !e.FeatureEnabled(FeatureHooks) && (cfg.Hooks.Count() > 0 || cfg.TrustProjectHooks) {
		changes = append(changes, Change{Field: "hooks", From: strconv.Itoa(cfg.Hooks.Count()) + " hooks", To: "none", Reason: "hooks are disabled by policy", Source: src()})
		cfg.Hooks = nil
		cfg.TrustProjectHooks = false
	}
	var keep []string
	for _, c := range cfg.AllowCommands {
		if d := e.CheckCommand(c, ""); !d.Allowed {
			changes = append(changes, Change{Field: "allow_commands", From: c, To: "(removed)", Reason: d.Reason, Source: d.Source})
			continue
		}
		keep = append(keep, c)
	}
	if len(keep) != len(cfg.AllowCommands) {
		cfg.AllowCommands = keep
	}
	return changes, nil
}

func orNone(s string) string {
	if s == "" {
		return "off"
	}
	return s
}

// Describe summarises the policy for display, one line per rule group.
func (e *Enforcer) Describe() []string {
	if !e.Active() {
		return []string{"No managed policy."}
	}
	var out []string
	list := func(label string, xs []string) {
		if len(xs) > 0 {
			out = append(out, fmt.Sprintf("  %s: %s", label, strings.Join(sortedCopy(xs), ", ")))
		}
	}
	for _, l := range e.layers {
		p := l.p
		out = append(out, "Policy "+p.Source)
		if p.Message != "" {
			out = append(out, "  message: "+p.Message)
		}
		list("tools allowed", p.Tools.Allow)
		list("tools denied", p.Tools.Deny)
		list("commands allowed", p.Commands.Allow)
		list("commands denied", p.Commands.Deny)
		list("paths denied", p.Paths.Deny)
		list("paths denied for reading", p.Paths.DenyRead)
		list("paths denied for writing", p.Paths.DenyWrite)
		list("providers allowed", p.Providers.Allow)
		list("providers denied", p.Providers.Deny)
		list("models allowed", p.Models.Allow)
		list("models denied", p.Models.Deny)
		list("disabled features", p.DisabledFeatures)
		if p.MaxPermissionMode != "" {
			out = append(out, "  max permission mode: "+p.MaxPermissionMode)
		}
		if p.Sandbox.Mode != "" {
			out = append(out, "  sandbox: at least "+p.Sandbox.Mode)
		}
		if p.Sandbox.Network != nil && !*p.Sandbox.Network {
			out = append(out, "  sandbox network: off")
		}
		if p.Audit.Required {
			out = append(out, "  audit log: required")
		}
		if p.Budget.Active() {
			out = append(out, fmt.Sprintf("  budget: session %s, daily %s, monthly %s", money(p.Budget.SessionUSD), money(p.Budget.DailyUSD), money(p.Budget.MonthlyUSD)))
		}
		if p.Secrets.Redact {
			out = append(out, "  secrets: redacted before reaching the model")
		}
	}
	return out
}
