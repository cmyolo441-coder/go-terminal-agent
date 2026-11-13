package main

import (
	"context"

	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/agent"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/audit"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/budget"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/config"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permission"
	orgpolicy "github.com/cmyolo441-coder/go-terminal-agent/internal/policy"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/secrets"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/ui"
)

// governance is the enterprise layer for one process: the managed policy
// and what it clamped, plus the audit log, redactor and budget tracker.
type governance struct {
	policy   *orgpolicy.Enforcer
	changes  []orgpolicy.Change
	gov      *agent.Governance
	logger   *audit.FileLogger
	auditDir string
	// hmacKey is the policy's audit HMAC key, for /audit verify.
	hmacKey  []byte
	warnings []string
}

// loadPolicy reads the managed policy and clamps cfg to it. Violations it
// cannot fix (a forbidden provider or model) stop GTA from starting.
func loadPolicy(cfg *config.Config) (*governance, error) {
	pol, err := orgpolicy.Load()
	if err != nil {
		return nil, fmt.Errorf("managed policy: %w", err)
	}
	g := &governance{policy: pol}
	if g.changes, err = pol.ClampConfig(cfg); err != nil {
		return nil, fmt.Errorf("managed policy: %w", err)
	}
	limits, more := pol.ClampBudget(cfg.Budget)
	cfg.Budget = limits
	g.changes = append(g.changes, more...)
	for _, c := range g.changes {
		fmt.Fprintln(os.Stderr, "gta: policy:", c.String())
	}
	return g, nil
}

// checkModel re-checks the model after the provider picked its default.
func (g *governance) checkModel(cfg config.Config) error {
	if d := g.policy.CheckModel(cfg.Provider, cfg.Model); !d.Allowed {
		return fmt.Errorf("managed policy: %s", d.UserText())
	}
	return nil
}

// checkProviderModel vets a provider and model against the policy.
func (g *governance) checkProviderModel(provider, model string) error {
	if d := g.policy.CheckModel(provider, model); !d.Allowed {
		return fmt.Errorf("managed policy: %s", d.UserText())
	}
	return nil
}

// build opens the audit log and builds the redactor and budget tracker.
func (g *governance) build(cfg config.Config, root, sessionID string) error {
	gov := &agent.Governance{Policy: g.policy, ProviderName: cfg.Provider}

	polRedact, rules, err := g.policy.Secrets()
	if err != nil {
		return fmt.Errorf("managed policy: %w", err)
	}
	if cfg.RedactSecrets || polRedact || len(rules) > 0 {
		r, err := secrets.New(secrets.Options{ExtraRules: rules, AllowValues: cfg.SecretsAllow})
		if err != nil {
			return fmt.Errorf("secret redaction: %w", err)
		}
		gov.Redactor = r
	}

	tr, err := budget.New(budget.Options{
		Limits:        cfg.Budget,
		Path:          filepath.Join(config.UserDir(), budget.LedgerFile),
		BlockUnpriced: g.policy.Budget().Active(),
	})
	if err != nil {
		g.warnings = append(g.warnings, "budget tracking off: "+err.Error())
	} else {
		gov.Budget = tr
	}

	req := g.policy.Audit()
	if cfg.AuditLog || req.Required {
		dir := firstNonEmpty(req.Dir, expandHome(cfg.AuditDir), filepath.Join(config.UserDir(), "audit"))
		sync := req.Sync
		if sync == "" && cfg.AuditSync != "" {
			if sync, err = audit.ParseSyncMode(cfg.AuditSync); err != nil {
				return err
			}
		}
		var key []byte
		if req.HMACKeyFile != "" {
			if key, err = os.ReadFile(expandHome(req.HMACKeyFile)); err != nil {
				return fmt.Errorf("audit HMAC key: %w", err)
			}
			key = []byte(strings.TrimSpace(string(key)))
		}
		lg, err := audit.Open(audit.Options{Dir: dir, Sync: sync, HMACKey: key, Redactor: gov.Redactor, Session: sessionID, Workspace: root})
		switch {
		case err != nil && req.Required:
			return fmt.Errorf("the managed policy requires an audit log, but it cannot be opened: %w", err)
		case err != nil:
			g.warnings = append(g.warnings, "audit log off: "+err.Error())
		default:
			g.logger, g.auditDir, g.hmacKey = lg, dir, key
			gov.Audit, gov.AuditRequired = lg, req.Required
			_ = lg.Log(audit.Record{Event: audit.EventSessionStart, Provider: cfg.Provider, Model: cfg.Model})
			for _, c := range g.changes {
				_ = lg.Log(audit.Record{Event: audit.EventConfigChange, Reason: c.String()})
			}
		}
	}
	g.gov = gov
	for _, w := range g.warnings {
		fmt.Fprintln(os.Stderr, "gta:", w)
	}
	return nil
}

// attach installs governance on ag, including the policy's mode limit.
func (g *governance) attach(ag *agent.Agent) {
	ag.Gov = g.gov
	if g.policy.Active() {
		ag.Gate.SetClamp(g.modeClamp)
	}
}

func (g *governance) modeClamp(m permission.Mode) (permission.Mode, string) {
	c, d := g.policy.ClampMode(m)
	if d.Allowed {
		return m, ""
	}
	return c, d.UserText()
}

// close ends the audit log with a session_end record.
func (g *governance) close() {
	if g == nil || g.logger == nil {
		return
	}
	_ = g.logger.Log(audit.Record{Event: audit.EventSessionEnd})
	_ = g.logger.Close()
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// registerGovernanceCommands adds /policy, /audit and /budget.
func registerGovernanceCommands(g *governance) {
	ui.Register(
		ui.Extra{Name: "policy", Help: "Show the managed policy in force", Run: func(ctx context.Context, arg string) (string, error) {
			return g.policyReport(), nil
		}},
		ui.Extra{Name: "audit", Args: "[verify | tail [n]]", Help: "Audit log status, verify its hash chain, or show recent records", Run: func(ctx context.Context, arg string) (string, error) {
			return g.auditReport(arg)
		}},
		ui.Extra{Name: "budget", Help: "Spend against the session, daily and monthly caps", Run: func(ctx context.Context, arg string) (string, error) {
			return g.budgetReport()
		}},
	)
}

func (g *governance) policyReport() string {
	if !g.policy.Active() {
		return "No managed policy. An administrator can install one at `" + orgpolicy.SystemPath() + "` (or point `GTA_POLICY_FILE` at a file) to restrict tools, commands, paths, models, modes and spend."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "**Managed policy** from %s\n\n", strings.Join(g.policy.Sources(), ", "))
	for _, l := range g.policy.Describe() {
		fmt.Fprintf(&b, "- %s\n", l)
	}
	if len(g.changes) > 0 {
		b.WriteString("\nChanged at startup:\n")
		for _, c := range g.changes {
			fmt.Fprintf(&b, "- %s\n", c.String())
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func (g *governance) auditReport(arg string) (string, error) {
	if g.logger == nil {
		return "The audit log is off. Set `\"audit_log\": true` in your config to record every tool call, approval and model call in a tamper-evident log.", nil
	}
	fields := strings.Fields(arg)
	switch {
	case len(fields) == 0:
		seq, hash := g.logger.Head()
		return fmt.Sprintf("Audit log: `%s`\n\n- records this chain: %d\n- head hash: `%s`\n- secrets redacted this session: %d\n\n`/audit verify` checks the hash chain; `/audit tail [n]` shows recent records.", g.auditDir, seq, clipHash(hash), g.gov.Stats().Redacted), nil
	case fields[0] == "verify":
		if err := g.logger.Flush(); err != nil {
			return "", err
		}
		// With a key, unsigned records are rejected too: otherwise anyone
		// who can write the files could rewrite the log without the key.
		keyed := len(g.hmacKey) > 0
		rep, err := audit.Verify(g.auditDir, audit.VerifyOptions{HMACKey: g.hmacKey, RequireHMAC: keyed})
		if err != nil {
			return "", err
		}
		if !rep.OK() {
			msg := fmt.Sprintf("⚠ The audit log is **broken** at %s", rep.Broken.String())
			if keyed && strings.Contains(rep.Broken.Reason, "not HMAC") {
				msg += "\n\nThe policy sets an HMAC key, so every record must be HMAC-signed. Records written before the key was configured are not; archive those segments, or check them with the verification script in docs/security.md."
			}
			return msg, nil
		}
		alg := "SHA-256"
		if keyed {
			alg = "HMAC-SHA256"
		}
		return fmt.Sprintf("✓ Hash chain intact (%s): %d records in %d file(s), seq %d–%d, head `%s`.", alg, rep.Records, rep.Files, rep.FirstSeq, rep.LastSeq, clipHash(rep.LastHash)), nil
	case fields[0] == "tail":
		n := 15
		if len(fields) > 1 {
			if _, err := fmt.Sscanf(fields[1], "%d", &n); err != nil || n < 1 {
				return "", fmt.Errorf("usage: /audit tail [n]")
			}
		}
		if err := g.logger.Flush(); err != nil {
			return "", err
		}
		var recs []audit.Record
		if err := audit.Read(g.auditDir, func(r audit.Record) error {
			recs = append(recs, r)
			if len(recs) > n {
				recs = recs[1:]
			}
			return nil
		}); err != nil {
			return "", err
		}
		var b strings.Builder
		for _, r := range recs {
			what := firstNonEmpty(r.Tool, r.Model, r.Reason)
			extra := firstNonEmpty(r.Decision, r.Error)
			fmt.Fprintf(&b, "- `%d` %s **%s** %s %s\n", r.Seq, r.Time.Local().Format("15:04:05"), r.Event, oneLine(what, 60), oneLine(extra, 40))
		}
		if b.Len() == 0 {
			return "No records yet.", nil
		}
		return strings.TrimRight(b.String(), "\n"), nil
	}
	return "", fmt.Errorf("usage: /audit [verify | tail [n]]")
}

func (g *governance) budgetReport() (string, error) {
	if g.gov == nil || g.gov.Budget == nil {
		return "Budget tracking is off.", nil
	}
	st, err := g.gov.Budget.Status()
	if err != nil {
		return "", err
	}
	line := func(name string, t budget.Totals, limit float64) string {
		s := fmt.Sprintf("- **%s**: $%.4f over %d call(s)", name, t.CostUSD, t.Calls)
		if limit > 0 {
			s += fmt.Sprintf(" of $%.2f (%.0f%%)", limit, 100*t.CostUSD/limit)
		} else {
			s += " · no cap"
		}
		if t.UnpricedCalls > 0 {
			s += fmt.Sprintf(" · %d unpriced", t.UnpricedCalls)
		}
		return s
	}
	out := []string{
		line("Session", st.Session, st.Limits.SessionUSD),
		line("Today ("+st.Day+")", st.Today, st.Limits.DailyUSD),
		line("This month ("+st.Month+")", st.ThisMonth, st.Limits.MonthlyUSD),
	}
	if !st.Limits.Active() {
		out = append(out, "", "Set caps with `\"budget\": {\"session_usd\": 5, \"daily_usd\": 20, \"monthly_usd\": 200}` in your config.")
	}
	return strings.Join(out, "\n"), nil
}

func clipHash(h string) string {
	if len(h) > 16 {
		return h[:16] + "…"
	}
	return h
}

// capabilities reports governance for the banner, /status and /cost.
func (g *governance) capabilities() []ui.Capability {
	var out []ui.Capability
	add := func(group, name, value string, state ui.CapState, banner bool) {
		out = append(out, ui.Capability{Group: group, Name: name, Value: value, State: state, Banner: banner})
	}
	if g.policy.Active() {
		add("Safety", "policy", "managed · "+strings.Join(g.policy.Sources(), ", "), ui.CapOn, true)
	} else {
		add("Safety", "policy", "none", ui.CapOff, false)
	}
	st := g.gov.Stats()
	switch {
	case g.logger == nil:
		add("Safety", "audit", "off (\"audit_log\": true enables it)", ui.CapOff, true)
	case st.AuditErr != nil:
		add("Safety", "audit", "failing · "+st.AuditErr.Error(), ui.CapWarn, true)
	default:
		seq, _ := g.logger.Head()
		add("Safety", "audit", fmt.Sprintf("hash-chained · %d records", seq), ui.CapOn, true)
	}
	if g.gov.Redactor != nil {
		add("Safety", "secrets", fmt.Sprintf("redacted before sending · %d this session", st.Redacted), ui.CapOn, true)
	} else {
		add("Safety", "secrets", "redaction off", ui.CapWarn, true)
	}
	if b := g.gov.Budget; b != nil {
		bs, err := b.Status()
		if err == nil {
			state := ui.CapOn
			val := fmt.Sprintf("session $%.4f · today $%.4f · month $%.4f", bs.Session.CostUSD, bs.Today.CostUSD, bs.ThisMonth.CostUSD)
			if bs.Limits.Active() {
				val += " · caps " + capsText(bs.Limits)
			} else {
				val += " · no caps"
				state = ui.CapOff
			}
			if st.LastBlock != "" {
				state, val = ui.CapWarn, "blocked · "+st.LastBlock
			}
			add("Budget", "budget", val, state, false)
			add("Safety", "budget", capsText(bs.Limits), state, false)
		}
	}
	return out
}

func capsText(l budget.Limits) string {
	var p []string
	if l.SessionUSD > 0 {
		p = append(p, fmt.Sprintf("$%.2f/session", l.SessionUSD))
	}
	if l.DailyUSD > 0 {
		p = append(p, fmt.Sprintf("$%.2f/day", l.DailyUSD))
	}
	if l.MonthlyUSD > 0 {
		p = append(p, fmt.Sprintf("$%.2f/month", l.MonthlyUSD))
	}
	if len(p) == 0 {
		return "none"
	}
	return strings.Join(p, " · ")
}
