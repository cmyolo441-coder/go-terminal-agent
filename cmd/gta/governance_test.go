package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/audit"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/config"
	orgpolicy "github.com/cmyolo441-coder/go-terminal-agent/internal/policy"
)

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
