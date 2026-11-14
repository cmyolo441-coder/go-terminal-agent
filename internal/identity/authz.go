package identity

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bmatcuk/doublestar/v4"
)

// PolicyVersion is the only accepted authorization policy version.
const PolicyVersion = 1

// Effect is a rule's effect.
type Effect string

// Rule effects.
const (
	EffectAllow Effect = "allow"
	EffectDeny  Effect = "deny"
)

// Policy is the JSON authorization policy.
//
//	{
//	  "version": 1,
//	  "default_roles": ["viewer"],
//	  "roles": {
//	    "viewer":    {"permissions": ["repo:read", "session:read"]},
//	    "developer": {"inherits": ["viewer"], "permissions": ["repo:write", "tools:*"]},
//	    "admin":     {"inherits": ["developer"], "permissions": ["*"]}
//	  },
//	  "group_mappings": [
//	    {"claim": "groups", "values": ["eng"], "roles": ["developer"]},
//	    {"claim": "realm_access.roles", "values": ["gta-admin"], "roles": ["admin"]}
//	  ],
//	  "rules": [
//	    {"id": "no-shell-in-prod", "effect": "deny", "permissions": ["tools:shell"],
//	     "conditions": {"environments": ["prod"]}},
//	    {"id": "oncall-deploy", "effect": "allow", "permissions": ["deploy:run"],
//	     "subjects": {"roles": ["developer"]},
//	     "conditions": {"time": {"days": ["mon","tue","wed","thu","fri"], "start": "09:00", "end": "17:00", "timezone": "UTC"},
//	                    "workspaces": ["/srv/app/**"]}}
//	  ]
//	}
type Policy struct {
	Version       int                `json:"version"`
	DefaultRoles  []string           `json:"default_roles,omitempty"`
	Roles         map[string]RoleDef `json:"roles"`
	GroupMappings []GroupMapping     `json:"group_mappings,omitempty"`
	Rules         []Rule             `json:"rules,omitempty"`
}

// RoleDef defines a role.
type RoleDef struct {
	Description string   `json:"description,omitempty"`
	Permissions []string `json:"permissions,omitempty"`
	Inherits    []string `json:"inherits,omitempty"`
}

// GroupMapping grants Roles to principals whose token claim at Claim (see
// LookupClaim) contains any of Values.
type GroupMapping struct {
	Claim  string   `json:"claim"`
	Values []string `json:"values"`
	Roles  []string `json:"roles"`
}

// Rule is a conditional allow or deny.
type Rule struct {
	ID          string      `json:"id"`
	Description string      `json:"description,omitempty"`
	Effect      Effect      `json:"effect"`
	Permissions []string    `json:"permissions"`
	Subjects    *Subjects   `json:"subjects,omitempty"`
	Conditions  *Conditions `json:"conditions,omitempty"`
}

// Subjects select the principals a rule applies to. Every non-empty field
// must match (AND); within a field any entry may match (OR). Nil or empty
// selects every authenticated principal.
type Subjects struct {
	// Roles are effective role names; "*" matches any principal.
	Roles []string `json:"roles,omitempty"`
	// Principals are subject globs ("*" any run of characters).
	Principals []string        `json:"principals,omitempty"`
	Types      []PrincipalType `json:"types,omitempty"`
}

// Conditions are attribute conditions; all present conditions must hold.
type Conditions struct {
	Time         *TimeWindow         `json:"time,omitempty"`
	Workspaces   []string            `json:"workspaces,omitempty"`
	Environments []string            `json:"environments,omitempty"`
	Attributes   map[string][]string `json:"attributes,omitempty"`
}

// TimeWindow restricts a rule to days of the week and a daily time range
// [Start, End) in Timezone. A range with Start after End wraps midnight.
type TimeWindow struct {
	Days     []string `json:"days,omitempty"`
	Start    string   `json:"start,omitempty"`
	End      string   `json:"end,omitempty"`
	Timezone string   `json:"timezone,omitempty"`
}

// Request is an authorization question.
type Request struct {
	Principal  *Principal
	Permission string
	// Workspace is the absolute workspace path the action touches.
	Workspace string
	// Environment names the deployment environment ("dev", "prod", ...).
	Environment string
	// Attributes are request attributes; they take precedence over the
	// principal's attributes in conditions.
	Attributes map[string]string
	// Time is the evaluation time (default: now).
	Time time.Time
}

// Decision is an explainable authorization result.
type Decision struct {
	Allowed bool `json:"allowed"`
	// Reason explains the decision in one sentence.
	Reason string `json:"reason"`
	// MatchedRule names what decided: a rule ID, "role:<name>", "scope" or
	// "" for the default deny.
	MatchedRule string `json:"matched_rule,omitempty"`
}

// Authorizer decides requests and maps identity-provider claims to roles.
type Authorizer interface {
	Authorize(Request) Decision
	RolesFromClaims(claims map[string]any) []string
}

var (
	nameRe       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	permSegRe    = regexp.MustCompile(`^[a-z0-9_.*?/-]+$`)
	concreteSegR = regexp.MustCompile(`^[a-z0-9_./-]+$`)
	clockRe      = regexp.MustCompile(`^([01][0-9]|2[0-3]):([0-5][0-9])$`)
	weekdays     = map[string]time.Weekday{
		"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
		"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
	}
)

// permPattern is a compiled "resource:action" pattern.
type permPattern struct {
	raw, resource, action string
}

func parsePermissionPattern(s string) (permPattern, error) {
	if s == "*" {
		return permPattern{raw: s, resource: "*", action: "*"}, nil
	}
	res, act, ok := strings.Cut(s, ":")
	if !ok || strings.Contains(act, ":") || !permSegRe.MatchString(res) || !permSegRe.MatchString(act) {
		return permPattern{}, fmt.Errorf("invalid permission pattern %q (want resource:action, lowercase, wildcards * and ?)", s)
	}
	return permPattern{raw: s, resource: res, action: act}, nil
}

func parsePermission(s string) (permPattern, error) {
	res, act, ok := strings.Cut(s, ":")
	if !ok || strings.Contains(act, ":") || !concreteSegR.MatchString(res) || !concreteSegR.MatchString(act) {
		return permPattern{}, fmt.Errorf("invalid permission %q (want resource:action without wildcards)", s)
	}
	return permPattern{raw: s, resource: res, action: act}, nil
}

func (p permPattern) matches(perm permPattern) bool {
	return globMatch(p.resource, perm.resource) && globMatch(p.action, perm.action)
}

// globMatch matches s against a pattern where '*' matches any run of
// characters and '?' exactly one. It runs in O(len(p)*len(s)) worst case
// without recursion, so hostile patterns cannot cause exponential work.
func globMatch(p, s string) bool {
	px, sx := 0, 0
	nextPx, nextSx := -1, -1
	for px < len(p) || sx < len(s) {
		if px < len(p) {
			switch c := p[px]; c {
			case '*':
				nextPx, nextSx = px, sx+1
				px++
				continue
			case '?':
				if sx < len(s) {
					px++
					sx++
					continue
				}
			default:
				if sx < len(s) && s[sx] == c {
					px++
					sx++
					continue
				}
			}
		}
		if nextSx > 0 && nextSx <= len(s) {
			px, sx = nextPx, nextSx
			continue
		}
		return false
	}
	return true
}

// MatchPermission reports whether pattern (which may contain wildcards)
// grants the concrete permission perm. Invalid input never matches.
func MatchPermission(pattern, perm string) bool {
	pp, err := parsePermissionPattern(pattern)
	if err != nil {
		return false
	}
	cp, err := parsePermission(perm)
	if err != nil {
		return false
	}
	return pp.matches(cp)
}

// ParsePolicy decodes and validates a policy. Decoding is strict: unknown
// fields, wrong types and trailing data are errors, because a typo in an
// authorization policy must never silently widen access.
func ParsePolicy(data []byte) (*Policy, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var p Policy
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("identity: policy: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("identity: policy: trailing data after JSON document")
	}
	if _, err := compilePolicy(&p); err != nil {
		return nil, err
	}
	return &p, nil
}

// LoadPolicyFile reads and validates a policy file and returns its Engine.
func LoadPolicyFile(path string) (*Engine, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	p, err := ParsePolicy(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return NewEngine(p)
}

// Engine evaluates a compiled Policy. It is immutable and safe for
// concurrent use; reload by building a new Engine (see ReloadableEngine).
type Engine struct {
	roles        map[string]*compiledRole
	defaultRoles []string
	mappings     []compiledMapping
	denyRules    []*compiledRule
	allowRules   []*compiledRule
}

type compiledRole struct {
	name  string
	perms []permPattern
	// closure lists this role and every role it inherits, in breadth-first
	// order, each with the inheritance path that reaches it.
	closure []roleVia
}

type roleVia struct {
	role string
	via  string // the role through which it was inherited ("" if direct)
}

type compiledMapping struct {
	claim  string
	values map[string]bool
	roles  []string
}

type compiledRule struct {
	id         string
	effect     Effect
	perms      []permPattern
	roles      []string
	anyRole    bool
	principals []string
	types      []PrincipalType
	cond       *compiledConditions
}

type compiledConditions struct {
	days         map[time.Weekday]bool
	hasClock     bool
	start, end   int // minutes since midnight
	loc          *time.Location
	workspaces   []string
	environments []string
	attributes   map[string][]string
	attrKeys     []string
}

// NewEngine validates p and compiles it.
func NewEngine(p *Policy) (*Engine, error) {
	if p == nil {
		return nil, errors.New("identity: nil policy")
	}
	return compilePolicy(p)
}

func compilePolicy(p *Policy) (*Engine, error) {
	if p.Version != PolicyVersion {
		return nil, fmt.Errorf("identity: policy: version must be %d, got %d", PolicyVersion, p.Version)
	}
	if len(p.Roles) == 0 {
		return nil, errors.New("identity: policy: at least one role is required")
	}
	e := &Engine{roles: make(map[string]*compiledRole, len(p.Roles))}
	names := make([]string, 0, len(p.Roles))
	for name := range p.Roles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !nameRe.MatchString(name) {
			return nil, fmt.Errorf("identity: policy: invalid role name %q", name)
		}
		def := p.Roles[name]
		cr := &compiledRole{name: name}
		for _, ps := range def.Permissions {
			pp, err := parsePermissionPattern(ps)
			if err != nil {
				return nil, fmt.Errorf("identity: policy: role %q: %w", name, err)
			}
			cr.perms = append(cr.perms, pp)
		}
		seen := map[string]bool{}
		for _, parent := range def.Inherits {
			if _, ok := p.Roles[parent]; !ok {
				return nil, fmt.Errorf("identity: policy: role %q inherits unknown role %q", name, parent)
			}
			if seen[parent] {
				return nil, fmt.Errorf("identity: policy: role %q inherits %q twice", name, parent)
			}
			seen[parent] = true
		}
		e.roles[name] = cr
	}
	if cycle := findInheritanceCycle(p.Roles, names); cycle != nil {
		return nil, fmt.Errorf("identity: policy: role inheritance cycle: %s", strings.Join(cycle, " -> "))
	}
	for _, name := range names {
		e.roles[name].closure = roleClosure(p.Roles, name)
	}
	for _, r := range p.DefaultRoles {
		if _, ok := e.roles[r]; !ok {
			return nil, fmt.Errorf("identity: policy: default role %q is not defined", r)
		}
	}
	e.defaultRoles = slices.Clone(p.DefaultRoles)

	for i, m := range p.GroupMappings {
		if m.Claim == "" {
			return nil, fmt.Errorf("identity: policy: group_mappings[%d]: claim is required", i)
		}
		if len(m.Values) == 0 || slices.Contains(m.Values, "") {
			return nil, fmt.Errorf("identity: policy: group_mappings[%d]: values must be non-empty strings", i)
		}
		if len(m.Roles) == 0 {
			return nil, fmt.Errorf("identity: policy: group_mappings[%d]: roles are required", i)
		}
		for _, r := range m.Roles {
			if _, ok := e.roles[r]; !ok {
				return nil, fmt.Errorf("identity: policy: group_mappings[%d]: unknown role %q", i, r)
			}
		}
		cm := compiledMapping{claim: m.Claim, values: map[string]bool{}, roles: slices.Clone(m.Roles)}
		for _, v := range m.Values {
			cm.values[v] = true
		}
		e.mappings = append(e.mappings, cm)
	}

	ids := map[string]bool{}
	for i, r := range p.Rules {
		cr, err := e.compileRule(r)
		if err != nil {
			return nil, fmt.Errorf("identity: policy: rules[%d]: %w", i, err)
		}
		if ids[cr.id] {
			return nil, fmt.Errorf("identity: policy: rules[%d]: duplicate rule id %q", i, cr.id)
		}
		ids[cr.id] = true
		if cr.effect == EffectDeny {
			e.denyRules = append(e.denyRules, cr)
		} else {
			e.allowRules = append(e.allowRules, cr)
		}
	}
	return e, nil
}

// findInheritanceCycle returns a cycle as a path ("a", "b", "a") or nil.
func findInheritanceCycle(roles map[string]RoleDef, names []string) []string {
	const (
		white = iota
		grey
		black
	)
	color := make(map[string]int, len(roles))
	var stack []string
	var visit func(string) []string
	visit = func(n string) []string {
		color[n] = grey
		stack = append(stack, n)
		for _, parent := range roles[n].Inherits {
			switch color[parent] {
			case grey:
				i := slices.Index(stack, parent)
				return append(slices.Clone(stack[i:]), parent)
			case white:
				if c := visit(parent); c != nil {
					return c
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[n] = black
		return nil
	}
	for _, n := range names {
		if color[n] == white {
			if c := visit(n); c != nil {
				return c
			}
		}
	}
	return nil
}

func roleClosure(roles map[string]RoleDef, name string) []roleVia {
	out := []roleVia{{role: name}}
	seen := map[string]bool{name: true}
	for i := 0; i < len(out); i++ {
		for _, parent := range roles[out[i].role].Inherits {
			if !seen[parent] {
				seen[parent] = true
				out = append(out, roleVia{role: parent, via: out[i].role})
			}
		}
	}
	return out
}

func (e *Engine) compileRule(r Rule) (*compiledRule, error) {
	if !nameRe.MatchString(r.ID) {
		return nil, fmt.Errorf("invalid or missing rule id %q", r.ID)
	}
	if r.Effect != EffectAllow && r.Effect != EffectDeny {
		return nil, fmt.Errorf("rule %q: effect must be \"allow\" or \"deny\", got %q", r.ID, r.Effect)
	}
	if len(r.Permissions) == 0 {
		return nil, fmt.Errorf("rule %q: permissions are required", r.ID)
	}
	cr := &compiledRule{id: r.ID, effect: r.Effect}
	for _, ps := range r.Permissions {
		pp, err := parsePermissionPattern(ps)
		if err != nil {
			return nil, fmt.Errorf("rule %q: %w", r.ID, err)
		}
		cr.perms = append(cr.perms, pp)
	}
	if s := r.Subjects; s != nil {
		for _, role := range s.Roles {
			if role == "*" {
				cr.anyRole = true
				continue
			}
			if _, ok := e.roles[role]; !ok {
				return nil, fmt.Errorf("rule %q: unknown role %q", r.ID, role)
			}
			cr.roles = append(cr.roles, role)
		}
		for _, pr := range s.Principals {
			if pr == "" {
				return nil, fmt.Errorf("rule %q: empty principal pattern", r.ID)
			}
			cr.principals = append(cr.principals, pr)
		}
		for _, t := range s.Types {
			if t != PrincipalUser && t != PrincipalServiceAccount {
				return nil, fmt.Errorf("rule %q: unknown principal type %q", r.ID, t)
			}
			cr.types = append(cr.types, t)
		}
	}
	if c := r.Conditions; c != nil {
		cc, err := compileConditions(c)
		if err != nil {
			return nil, fmt.Errorf("rule %q: %w", r.ID, err)
		}
		cr.cond = cc
	}
	return cr, nil
}

func compileConditions(c *Conditions) (*compiledConditions, error) {
	cc := &compiledConditions{}
	empty := true
	if tw := c.Time; tw != nil {
		empty = false
		cc.loc = time.UTC
		if tw.Timezone != "" {
			loc, err := time.LoadLocation(tw.Timezone)
			if err != nil {
				return nil, fmt.Errorf("time.timezone: %w", err)
			}
			cc.loc = loc
		}
		if len(tw.Days) > 0 {
			cc.days = map[time.Weekday]bool{}
			for _, d := range tw.Days {
				wd, ok := weekdays[strings.ToLower(d)]
				if !ok {
					return nil, fmt.Errorf("time.days: unknown day %q (use mon..sun)", d)
				}
				if cc.days[wd] {
					return nil, fmt.Errorf("time.days: %q listed twice", d)
				}
				cc.days[wd] = true
			}
		}
		if (tw.Start == "") != (tw.End == "") {
			return nil, errors.New("time: start and end must be given together")
		}
		if tw.Start != "" {
			s, err1 := parseClock(tw.Start)
			en, err2 := parseClock(tw.End)
			if err := errors.Join(err1, err2); err != nil {
				return nil, err
			}
			if s == en {
				return nil, errors.New("time: start and end must differ")
			}
			cc.hasClock, cc.start, cc.end = true, s, en
		}
		if cc.days == nil && !cc.hasClock {
			return nil, errors.New("time: give days, start/end or both")
		}
	}
	for _, w := range c.Workspaces {
		empty = false
		if w == "" || !doublestar.ValidatePattern(w) {
			return nil, fmt.Errorf("workspaces: invalid glob %q", w)
		}
		cc.workspaces = append(cc.workspaces, filepath.ToSlash(w))
	}
	for _, env := range c.Environments {
		empty = false
		if env == "" {
			return nil, errors.New("environments: empty name")
		}
		cc.environments = append(cc.environments, strings.ToLower(env))
	}
	if len(c.Attributes) > 0 {
		empty = false
		cc.attributes = map[string][]string{}
		for k, vs := range c.Attributes {
			if k == "" || len(vs) == 0 {
				return nil, fmt.Errorf("attributes: %q needs at least one value", k)
			}
			cc.attributes[k] = slices.Clone(vs)
			cc.attrKeys = append(cc.attrKeys, k)
		}
		sort.Strings(cc.attrKeys)
	}
	if empty {
		return nil, errors.New("conditions: empty conditions object")
	}
	return cc, nil
}

func parseClock(s string) (int, error) {
	m := clockRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("time: %q is not HH:MM (24h)", s)
	}
	h := int(m[1][0]-'0')*10 + int(m[1][1]-'0')
	mi := int(m[2][0]-'0')*10 + int(m[2][1]-'0')
	return h*60 + mi, nil
}

// condResult is the outcome of evaluating conditions.
type condResult int

const (
	condMatch condResult = iota
	condNoMatch
	condUnknown // a needed attribute was missing from the request
)

func (c *compiledConditions) eval(req *Request, now time.Time) (condResult, string) {
	if c == nil {
		return condMatch, ""
	}
	unknown := ""
	if c.loc != nil {
		t := now.In(c.loc)
		if c.days != nil && !c.days[t.Weekday()] {
			return condNoMatch, "outside allowed days"
		}
		if c.hasClock {
			m := t.Hour()*60 + t.Minute()
			in := m >= c.start && m < c.end
			if c.start > c.end { // wraps midnight
				in = m >= c.start || m < c.end
			}
			if !in {
				return condNoMatch, "outside time window"
			}
		}
	}
	if len(c.workspaces) > 0 {
		if req.Workspace == "" {
			unknown = "workspace"
		} else {
			ws := filepath.ToSlash(filepath.Clean(req.Workspace))
			ok := false
			for _, g := range c.workspaces {
				if m, _ := doublestar.Match(g, ws); m {
					ok = true
					break
				}
			}
			if !ok {
				return condNoMatch, "workspace " + ws + " not matched"
			}
		}
	}
	if len(c.environments) > 0 {
		if req.Environment == "" {
			unknown = "environment"
		} else if !slices.Contains(c.environments, strings.ToLower(req.Environment)) {
			return condNoMatch, "environment " + req.Environment + " not matched"
		}
	}
	for _, k := range c.attrKeys {
		v, ok := req.Attributes[k]
		if !ok && req.Principal != nil {
			v, ok = req.Principal.Attributes[k]
		}
		if !ok {
			unknown = "attribute " + k
			continue
		}
		if !slices.Contains(c.attributes[k], v) {
			return condNoMatch, "attribute " + k + " not matched"
		}
	}
	if unknown != "" {
		return condUnknown, unknown + " missing from request"
	}
	return condMatch, ""
}

// effectiveRoles returns the principal's roles (direct, default and
// inherited) mapped to an explanation of how each was obtained.
func (e *Engine) effectiveRoles(p *Principal) (map[string]string, []string) {
	out := map[string]string{}
	var order []string
	add := func(direct, source string) {
		cr, ok := e.roles[direct]
		if !ok {
			return // roles unknown to the policy grant nothing
		}
		for _, rv := range cr.closure {
			if _, dup := out[rv.role]; dup {
				continue
			}
			how := source
			if rv.via != "" {
				how = fmt.Sprintf("inherited via %q from %s", rv.via, source)
			}
			out[rv.role] = how
			order = append(order, rv.role)
		}
	}
	for _, r := range p.Roles {
		add(r, fmt.Sprintf("assigned role %q", r))
	}
	for _, r := range e.defaultRoles {
		add(r, fmt.Sprintf("default role %q", r))
	}
	return out, order
}

// EffectiveRoles returns every role p holds, including default and
// inherited roles, sorted.
func (e *Engine) EffectiveRoles(p *Principal) []string {
	if p == nil {
		return nil
	}
	m, _ := e.effectiveRoles(p)
	out := make([]string, 0, len(m))
	for r := range m {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// RolesFromClaims maps token claims to roles through the policy's group
// mappings. The result is sorted and de-duplicated.
func (e *Engine) RolesFromClaims(claims map[string]any) []string {
	set := map[string]bool{}
	for _, m := range e.mappings {
		v, ok := LookupClaim(claims, m.claim)
		if !ok {
			continue
		}
		for _, s := range claimStrings(v, false) {
			if m.values[s] {
				for _, r := range m.roles {
					set[r] = true
				}
				break
			}
		}
	}
	out := make([]string, 0, len(set))
	for r := range set {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

func (r *compiledRule) subjectMatches(p *Principal, roles map[string]string) bool {
	if len(r.roles) > 0 || r.anyRole {
		ok := r.anyRole
		for _, role := range r.roles {
			if _, has := roles[role]; has {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if len(r.principals) > 0 && !slices.ContainsFunc(r.principals, func(g string) bool { return globMatch(g, p.Subject) }) {
		return false
	}
	return len(r.types) == 0 || slices.Contains(r.types, p.Type)
}

func (r *compiledRule) permMatches(perm permPattern) (permPattern, bool) {
	for _, pp := range r.perms {
		if pp.matches(perm) {
			return pp, true
		}
	}
	return permPattern{}, false
}

// Authorize decides req. Evaluation order: deny rules (deny overrides; a
// deny whose conditions cannot be evaluated for lack of an attribute still
// applies), then the credential's scopes, then role grants, then allow
// rules (whose conditions must positively hold). Anything else is denied.
func (e *Engine) Authorize(req Request) Decision {
	p := req.Principal
	if p == nil || p.Subject == "" {
		return Decision{Reason: "unauthenticated request"}
	}
	perm, err := parsePermission(req.Permission)
	if err != nil {
		return Decision{Reason: err.Error()}
	}
	now := req.Time
	if now.IsZero() {
		now = time.Now()
	}
	roles, order := e.effectiveRoles(p)

	for _, r := range e.denyRules {
		if !r.subjectMatches(p, roles) {
			continue
		}
		pp, ok := r.permMatches(perm)
		if !ok {
			continue
		}
		res, why := r.cond.eval(&req, now)
		if res == condNoMatch {
			continue
		}
		reason := fmt.Sprintf("%q denied by rule %q (pattern %q)", perm.raw, r.id, pp.raw)
		if res == condUnknown {
			reason += "; applied because " + why
		}
		return Decision{Reason: reason, MatchedRule: r.id}
	}

	if p.Scopes != nil {
		inScope := false
		for _, s := range p.Scopes {
			if sp, err := parsePermissionPattern(s); err == nil && sp.matches(perm) {
				inScope = true
				break
			}
		}
		if !inScope {
			return Decision{Reason: fmt.Sprintf("%q is outside the credential's scopes", perm.raw), MatchedRule: "scope"}
		}
	}

	for _, rn := range order {
		for _, pp := range e.roles[rn].perms {
			if pp.matches(perm) {
				return Decision{
					Allowed:     true,
					Reason:      fmt.Sprintf("%q granted by role %q (%s) via %q", perm.raw, rn, roles[rn], pp.raw),
					MatchedRule: "role:" + rn,
				}
			}
		}
	}

	var notes []string
	for _, r := range e.allowRules {
		if !r.subjectMatches(p, roles) {
			continue
		}
		pp, ok := r.permMatches(perm)
		if !ok {
			continue
		}
		res, why := r.cond.eval(&req, now)
		if res != condMatch {
			notes = append(notes, fmt.Sprintf("rule %q not applicable: %s", r.id, why))
			continue
		}
		return Decision{
			Allowed:     true,
			Reason:      fmt.Sprintf("%q allowed by rule %q (pattern %q)", perm.raw, r.id, pp.raw),
			MatchedRule: r.id,
		}
	}
	reason := fmt.Sprintf("no role or rule grants %q", perm.raw)
	if len(notes) > 0 {
		reason += " (" + strings.Join(notes, "; ") + ")"
	}
	return Decision{Reason: reason}
}

// ReloadableEngine is an Authorizer whose Engine can be swapped atomically
// (for example on SIGHUP or when the policy file changes).
type ReloadableEngine struct {
	p atomic.Pointer[Engine]
}

// NewReloadableEngine returns a holder initialised with e.
func NewReloadableEngine(e *Engine) (*ReloadableEngine, error) {
	if e == nil {
		return nil, errors.New("identity: nil engine")
	}
	r := &ReloadableEngine{}
	r.p.Store(e)
	return r, nil
}

// Store replaces the engine; nil is ignored.
func (r *ReloadableEngine) Store(e *Engine) {
	if e != nil {
		r.p.Store(e)
	}
}

// Load returns the current engine.
func (r *ReloadableEngine) Load() *Engine { return r.p.Load() }

// ReloadFile loads path and, only if it is valid, swaps it in. On error the
// current policy stays in force.
func (r *ReloadableEngine) ReloadFile(path string) error {
	e, err := LoadPolicyFile(path)
	if err != nil {
		return err
	}
	r.Store(e)
	return nil
}

// Authorize implements Authorizer.
func (r *ReloadableEngine) Authorize(req Request) Decision { return r.p.Load().Authorize(req) }

// RolesFromClaims implements Authorizer.
func (r *ReloadableEngine) RolesFromClaims(c map[string]any) []string {
	return r.p.Load().RolesFromClaims(c)
}
