package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/audit"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/budget"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/codeintel"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/config"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	orgpolicy "github.com/cmyolo441-coder/go-terminal-agent/internal/policy"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

// In gta serve each API session gets its own session_usd cap.
func TestServeSessionBudgetIsPerSession(t *testing.T) {
	cfg := config.Default()
	cfg.AuditLog, cfg.AuditDir = true, t.TempDir()
	cfg.Budget = budget.Limits{SessionUSD: 1}
	g := testGovernance(t, nil, cfg)
	defer g.close()
	a, b := g.sessionGov(), g.sessionGov()
	if a.Budget == nil || b.Budget == nil || a.Budget == b.Budget || a.Budget == g.gov.Budget {
		t.Fatal("sessions share a budget tracker")
	}
	if a.Audit != g.gov.Audit || a.Policy != g.gov.Policy {
		t.Fatal("sessions must share the audit log and policy")
	}
	if _, err := a.Budget.Record(config.DefaultModel, llm.Usage{InputTokens: 2_000_000, OutputTokens: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	if d := a.Budget.Check(config.DefaultModel, 0, 0); d.Verdict != budget.Block {
		t.Fatalf("spent session not blocked: %+v", d)
	}
	if d := b.Budget.Check(config.DefaultModel, 0, 0); d.Verdict == budget.Block {
		t.Fatalf("another session blocked by the first one's spend: %+v", d)
	}
}

// Policy path rules reach the directory-walking tools and the code index,
// and command rules reach test commands run outside the bash tool.
func TestPolicyGuardsWalksIndexAndTestCommands(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"main.go":             "package demo\n\nfunc VisibleThing() {} // needle\n",
		".env":                "TOKEN=needle\n",
		"secrets/keys.go":     "package secrets\n\nfunc SecretKeys() {} // needle\n",
		"secrets/sub/deep.go": "package sub\n\nfunc DeepSecret() {}\n",
	}
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pol := map[string]any{
		"version":  1,
		"paths":    map[string]any{"deny": []string{".env"}, "deny_read": []string{"secrets"}},
		"commands": map[string]any{"deny": []string{"make"}},
		"audit":    map[string]any{"dir": t.TempDir()},
	}
	cfg := config.Default()
	cfg.AuditLog = true
	g := testGovernance(t, pol, cfg)
	defer g.close()

	env, err := tools.NewEnv(root)
	if err != nil {
		t.Fatal(err)
	}
	g.guardEnv(env)
	for _, c := range []struct {
		tool  tools.Tool
		input string
	}{
		{tools.Grep{}, `{"pattern":"needle"}`},
		{tools.Glob{}, `{"pattern":"**/*"}`},
		{tools.ListDir{}, `{"depth":5}`},
	} {
		call, err := c.tool.Prepare(env, json.RawMessage(c.input))
		if err != nil {
			t.Fatal(err)
		}
		out, err := call.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, ".env") || strings.Contains(out, "secrets") || !strings.Contains(out, "main.go") {
			t.Errorf("%s revealed a denied path or lost main.go:\n%s", c.tool.Spec().Name, out)
		}
	}

	cfg.LSP = false
	ci := startCodeIntel(cfg, root, g.codeExclude(root))
	defer ci.close()
	if m := ci.systemPrompt(); strings.Contains(m, "secrets") || !strings.Contains(m, "main.go") {
		t.Errorf("repository map reveals denied files:\n%s", m)
	}
	ms, err := ci.ix.Symbols(context.Background(), codeintel.SymbolQuery{Name: "SecretKeys"})
	if err != nil || len(ms) != 0 {
		t.Errorf("symbols found a denied file: %v %v", ms, err)
	}

	if err := env.CheckCommand("make test"); err == nil || !strings.Contains(err.Error(), "denied by policy") {
		t.Errorf("make test allowed: %v", err)
	}
	if err := env.CheckCommand("go test ./..."); err != nil {
		t.Errorf("go test denied: %v", err)
	}

	// Without read rules no per-file check is installed.
	g2 := testGovernance(t, map[string]any{"version": 1, "audit": map[string]any{"dir": t.TempDir()}}, cfg)
	defer g2.close()
	if g2.hidden(root) != nil || g2.codeExclude(root) != nil {
		t.Error("per-file path checks installed without read rules")
	}
}

// testGovernance builds governance from a policy (nil for none) and cfg,
// writing a few records to the audit log.
func testGovernance(t *testing.T, pol map[string]any, cfg config.Config) *governance {
	t.Helper()
	t.Setenv("GTA_CONFIG_DIR", t.TempDir())
	var ps []*orgpolicy.Policy
	if pol != nil {
		data, _ := json.Marshal(pol)
		p, err := orgpolicy.Parse(data, "test-policy.json")
		if err != nil {
			t.Fatal(err)
		}
		ps = append(ps, p)
	}
	enf, err := orgpolicy.NewEnforcer(ps...)
	if err != nil {
		t.Fatal(err)
	}
	g := &governance{policy: enf}
	if err := g.build(cfg, t.TempDir(), "test-session"); err != nil {
		t.Fatal(err)
	}
	if g.logger == nil {
		t.Fatal("audit log not opened")
	}
	for range 3 {
		if err := g.logger.Log(audit.Record{Event: audit.EventConfigChange, Reason: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	return g
}

func TestAuditVerifyUnkeyedLog(t *testing.T) {
	cfg := config.Default()
	cfg.AuditLog, cfg.AuditDir = true, t.TempDir()
	g := testGovernance(t, nil, cfg)
	defer g.close()
	out, err := g.auditReport("verify")
	if err != nil || !strings.Contains(out, "intact (SHA-256): 4 records") {
		t.Fatalf("verify = %q, %v", out, err)
	}
}

func TestAuditVerifyUsesPolicyHMACKey(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(t.TempDir(), "audit.key")
	if err := os.WriteFile(keyFile, []byte("not-a-real-key-just-for-tests\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pol := map[string]any{"version": 1, "audit": map[string]any{"required": true, "dir": dir, "hmac_key_file": keyFile}}
	g := testGovernance(t, pol, config.Default())
	defer g.close()
	out, err := g.auditReport("verify")
	if err != nil || !strings.Contains(out, "intact (HMAC-SHA256): 4 records") {
		t.Fatalf("verify of an HMAC-keyed log = %q, %v", out, err)
	}
}

// Unsigned records in a keyed log are reported, since anyone who can write
// the files could otherwise rewrite it without the key.
func TestAuditVerifyKeyedRejectsUnsignedRecords(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.AuditLog, cfg.AuditDir = true, dir
	testGovernance(t, nil, cfg).close()

	keyFile := filepath.Join(t.TempDir(), "audit.key")
	if err := os.WriteFile(keyFile, []byte("not-a-real-key-just-for-tests"), 0o600); err != nil {
		t.Fatal(err)
	}
	pol := map[string]any{"version": 1, "audit": map[string]any{"dir": dir, "hmac_key_file": keyFile}}
	g := testGovernance(t, pol, cfg)
	defer g.close()
	out, err := g.auditReport("verify")
	if err != nil || !strings.Contains(out, "broken") || !strings.Contains(out, "not HMAC-signed") || !strings.Contains(out, "before the key was configured") {
		t.Fatalf("verify = %q, %v", out, err)
	}
}
