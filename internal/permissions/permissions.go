// Package permissions decides whether a tool call may run, must be confirmed, or is blocked.
package permissions

import (
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Mode is the session-wide permission posture.
type Mode string

const (
	ModeDefault     Mode = "default"
	ModeAcceptEdits Mode = "acceptEdits"
	ModePlan        Mode = "plan"
	ModeBypass      Mode = "bypassPermissions"
)

// ParseMode validates a mode name.
func ParseMode(s string) (Mode, bool) {
	switch Mode(s) {
	case ModeDefault, ModeAcceptEdits, ModePlan, ModeBypass:
		return Mode(s), true
	case "":
		return ModeDefault, true
	}
	return ModeDefault, false
}

// Decision is the outcome of a permission check.
type Decision int

const (
	Allow Decision = iota
	Ask
	Deny
)

// Request describes one tool call for permission purposes.
type Request struct {
	Tool     string // tool name, e.g. "Bash"
	Key      string // matchable content: the command, file path or URL
	ReadOnly bool   // tool never changes anything
	IsEdit   bool   // tool edits files (covered by acceptEdits)
}

// Rule is a parsed "Tool" or "Tool(pattern)" entry.
type Rule struct {
	Raw     string
	Tool    string
	Pattern string
}

// ParseRule parses a settings rule string.
func ParseRule(s string) Rule {
	s = strings.TrimSpace(s)
	r := Rule{Raw: s, Tool: s}
	if i := strings.IndexByte(s, '('); i > 0 && strings.HasSuffix(s, ")") {
		r.Tool, r.Pattern = s[:i], s[i+1:len(s)-1]
	}
	return r
}

// Checker evaluates requests against rules and the current mode.
type Checker struct {
	mu    sync.RWMutex
	mode  Mode
	allow []Rule
	deny  []Rule
	ask   []Rule
	cwd   string
	extra []string // additional directories treated like cwd
}

// New builds a checker from rule strings.
func New(mode Mode, cwd string, allow, deny, ask []string) *Checker {
	c := &Checker{mode: mode, cwd: cwd}
	for _, s := range allow {
		c.allow = append(c.allow, ParseRule(s))
	}
	for _, s := range deny {
		c.deny = append(c.deny, ParseRule(s))
	}
	for _, s := range ask {
		c.ask = append(c.ask, ParseRule(s))
	}
	return c
}

func (c *Checker) Mode() Mode { c.mu.RLock(); defer c.mu.RUnlock(); return c.mode }

func (c *Checker) SetMode(m Mode) { c.mu.Lock(); c.mode = m; c.mu.Unlock() }

// AddAllow adds a session allow rule.
func (c *Checker) AddAllow(rule string) {
	c.mu.Lock()
	c.allow = append(c.allow, ParseRule(rule))
	c.mu.Unlock()
}

// AddDeny adds a session deny rule.
func (c *Checker) AddDeny(rule string) {
	c.mu.Lock()
	c.deny = append(c.deny, ParseRule(rule))
	c.mu.Unlock()
}

// AddDir lets edits in another directory be covered by acceptEdits.
func (c *Checker) AddDir(dir string) { c.mu.Lock(); c.extra = append(c.extra, dir); c.mu.Unlock() }

// Rules returns the raw allow, deny and ask rules.
func (c *Checker) Rules() (allow, deny, ask []string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, r := range c.allow {
		allow = append(allow, r.Raw)
	}
	for _, r := range c.deny {
		deny = append(deny, r.Raw)
	}
	for _, r := range c.ask {
		ask = append(ask, r.Raw)
	}
	return
}

// Check returns the decision for a request plus a short reason for denials.
func (c *Checker) Check(r Request) (Decision, string) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if rule, ok := c.matchAny(c.deny, r, true); ok {
		return Deny, "blocked by deny rule " + rule
	}
	if c.mode == ModeBypass {
		return Allow, ""
	}
	if c.mode == ModePlan && !r.ReadOnly {
		return Deny, "plan mode is active: only read-only tools may run. Present the plan with ExitPlanMode and wait for approval before changing anything."
	}
	if _, ok := c.matchAny(c.ask, r, true); ok {
		return Ask, ""
	}
	if r.ReadOnly {
		return Allow, ""
	}
	if _, ok := c.matchAny(c.allow, r, false); ok {
		return Allow, ""
	}
	if r.Tool == "Bash" && c.bashAllowed(r.Key) {
		return Allow, ""
	}
	if r.IsEdit && c.mode == ModeAcceptEdits && c.inWorkspace(r.Key) {
		return Allow, ""
	}
	return Ask, ""
}

func (c *Checker) inWorkspace(path string) bool {
	for _, root := range append([]string{c.cwd}, c.extra...) {
		rel, err := filepath.Rel(root, path)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// matchAny reports the first rule matching r. With anyPart set, a Bash rule
// matches when any sub-command matches (used for deny and ask lists).
func (c *Checker) matchAny(rules []Rule, r Request, anyPart bool) (string, bool) {
	for _, rule := range rules {
		if !toolMatches(rule.Tool, r.Tool) {
			continue
		}
		if rule.Pattern == "" {
			return rule.Raw, true
		}
		if r.Tool == "Bash" {
			if anyPart {
				for _, part := range SplitCommand(r.Key) {
					if bashPatternMatch(rule.Pattern, part) {
						return rule.Raw, true
					}
				}
			}
			continue // allow rules for Bash are handled per sub-command
		}
		if c.patternMatch(rule.Pattern, r.Key) {
			return rule.Raw, true
		}
	}
	return "", false
}

func toolMatches(ruleTool, tool string) bool {
	if ruleTool == tool || ruleTool == "*" {
		return true
	}
	// "mcp__server" covers every tool of that server.
	return strings.HasPrefix(ruleTool, "mcp__") && strings.HasPrefix(tool, ruleTool+"__")
}

func (c *Checker) patternMatch(pattern, key string) bool {
	if d, ok := strings.CutPrefix(pattern, "domain:"); ok {
		host := hostOf(key)
		return host == d || strings.HasSuffix(host, "."+d)
	}
	if pattern == key {
		return true
	}
	if !filepath.IsAbs(pattern) && !strings.HasPrefix(pattern, "**") {
		pattern = filepath.Join(c.cwd, pattern)
	}
	return globMatch(pattern, key)
}

func hostOf(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	if i := strings.IndexAny(u, "/?#"); i >= 0 {
		u = u[:i]
	}
	if i := strings.LastIndexByte(u, '@'); i >= 0 {
		u = u[i+1:]
	}
	if i := strings.LastIndexByte(u, ':'); i >= 0 {
		u = u[:i]
	}
	return strings.ToLower(u)
}

// globMatch supports *, ? and ** (any number of path segments).
func globMatch(pattern, s string) bool {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		ch := pattern[i]
		switch {
		case ch == '*' && i+1 < len(pattern) && pattern[i+1] == '*':
			b.WriteString(".*")
			i++
			if i+1 < len(pattern) && pattern[i+1] == '/' {
				i++
			}
		case ch == '*':
			b.WriteString("[^/]*")
		case ch == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(ch)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	return err == nil && re.MatchString(s)
}

// bashPatternMatch matches "prefix:*", "prefix *", globs with * and exact commands.
func bashPatternMatch(pattern, cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	if p, ok := strings.CutSuffix(pattern, ":*"); ok {
		return cmd == p || strings.HasPrefix(cmd, p+" ")
	}
	if !strings.Contains(pattern, "*") {
		return cmd == pattern
	}
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(cmd, parts[0]) {
		return false
	}
	rest := cmd[len(parts[0]):]
	for i, p := range parts[1:] {
		last := i == len(parts)-2
		if last {
			return strings.HasSuffix(rest, p)
		}
		j := strings.Index(rest, p)
		if j < 0 {
			return false
		}
		rest = rest[j+len(p):]
	}
	return true
}

// Commands that only read state. Checked per sub-command, by leading words.
var safeCommands = []string{
	"ls", "pwd", "cat", "head", "tail", "wc", "echo", "which", "whoami", "date", "uname",
	"file", "stat", "du", "df", "tree", "grep", "rg", "true", "basename", "dirname", "realpath",
	"sort", "uniq", "cut", "diff", "printenv", "id", "hostname",
	"git status", "git diff", "git log", "git show", "git branch", "git remote -v", "git rev-parse",
	"git ls-files", "git blame", "git describe", "git stash list", "git tag",
	"go version", "go env", "go list", "go doc", "go vet",
	"node --version", "npm --version", "python --version", "python3 --version", "cargo --version",
}

// bashAllowed reports whether every sub-command is covered by an allow rule or
// the built-in read-only list. Anything with substitution or redirection to a
// file needs confirmation.
func (c *Checker) bashAllowed(cmd string) bool {
	if hasRiskySyntax(cmd) {
		return false
	}
	parts := SplitCommand(cmd)
	if len(parts) == 0 {
		return false
	}
	for _, part := range parts {
		if !c.partAllowed(part) {
			return false
		}
	}
	return true
}

func (c *Checker) partAllowed(part string) bool {
	for _, rule := range c.allow {
		if rule.Tool != "Bash" {
			continue
		}
		if rule.Pattern == "" || bashPatternMatch(rule.Pattern, part) {
			return true
		}
	}
	for _, s := range safeCommands {
		if part == s || strings.HasPrefix(part, s+" ") {
			// `git branch -D x`, `git tag -d x` and friends are not read-only.
			if (s == "git branch" || s == "git tag") && strings.ContainsAny(strings.TrimPrefix(part, s), "-") &&
				!regexp.MustCompile(`^git (branch|tag)( (-a|-r|-v|-vv|--list|-l|--show-current))*$`).MatchString(part) {
				return false
			}
			return true
		}
	}
	return false
}

func hasRiskySyntax(cmd string) bool {
	if strings.Contains(cmd, "$(") || strings.Contains(cmd, "`") || strings.Contains(cmd, "<(") {
		return true
	}
	// Output redirection, ignoring fd duplication such as 2>&1 and >/dev/null.
	clean := regexp.MustCompile(`\d?>&\d|\d?>>?\s*/dev/null`).ReplaceAllString(cmd, "")
	return strings.Contains(stripQuoted(clean), ">")
}

func stripQuoted(s string) string {
	var b strings.Builder
	var q byte
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case q != 0:
			if ch == '\\' && q == '"' && i+1 < len(s) {
				i++
			} else if ch == q {
				q = 0
			}
		case ch == '\'' || ch == '"':
			q = ch
		default:
			b.WriteByte(ch)
		}
	}
	return b.String()
}

// SplitCommand splits a shell command on &&, ||, ; | and newlines, respecting quotes.
func SplitCommand(cmd string) []string {
	var parts []string
	var cur strings.Builder
	var q byte
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			parts = append(parts, s)
		}
		cur.Reset()
	}
	for i := 0; i < len(cmd); i++ {
		ch := cmd[i]
		if q != 0 {
			cur.WriteByte(ch)
			if ch == '\\' && q == '"' && i+1 < len(cmd) {
				i++
				cur.WriteByte(cmd[i])
			} else if ch == q {
				q = 0
			}
			continue
		}
		switch ch {
		case '\'', '"':
			q = ch
			cur.WriteByte(ch)
		case '\\':
			cur.WriteByte(ch)
			if i+1 < len(cmd) {
				i++
				cur.WriteByte(cmd[i])
			}
		case ';', '\n':
			flush()
		case '&':
			if i+1 < len(cmd) && cmd[i+1] == '&' {
				i++
				flush()
			} else if i > 0 && cmd[i-1] == '>' || i+1 < len(cmd) && cmd[i+1] == '>' {
				cur.WriteByte(ch) // part of a redirection such as 2>&1 or &>
			} else {
				flush() // background operator
			}
		case '|':
			if i+1 < len(cmd) && cmd[i+1] == '|' {
				i++
			}
			flush()
		default:
			cur.WriteByte(ch)
		}
	}
	flush()
	return parts
}

var multiWord = map[string]bool{"git": true, "go": true, "npm": true, "yarn": true, "pnpm": true, "cargo": true,
	"docker": true, "kubectl": true, "pip": true, "pip3": true, "make": true, "npx": true, "gh": true, "brew": true, "apt": true}

// SuggestRule proposes an allow rule that would cover this request in future.
func SuggestRule(r Request) string {
	if r.Tool != "Bash" {
		if strings.HasPrefix(r.Tool, "mcp__") || r.Key == "" {
			return r.Tool
		}
		if r.Tool == "WebFetch" {
			return "WebFetch(domain:" + hostOf(r.Key) + ")"
		}
		return r.Tool + "(" + r.Key + ")"
	}
	parts := SplitCommand(r.Key)
	if len(parts) != 1 || hasRiskySyntax(r.Key) {
		return "Bash(" + r.Key + ")"
	}
	words := strings.Fields(parts[0])
	if len(words) == 0 {
		return "Bash(" + r.Key + ")"
	}
	prefix := words[0]
	if multiWord[words[0]] && len(words) > 1 && !strings.HasPrefix(words[1], "-") {
		prefix += " " + words[1]
	}
	return "Bash(" + prefix + ":*)"
}
