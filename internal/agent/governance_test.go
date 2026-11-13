package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/audit"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/budget"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/permission"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/policy"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/secrets"
)

func readAudit(t *testing.T, dir string) []audit.Record {
	t.Helper()
	var out []audit.Record
	if err := audit.Read(dir, func(r audit.Record) error { out = append(out, r); return nil }); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestGovernancePolicyDenyIsFinalAndAudited(t *testing.T) {
	p := &scripted{responses: []*llm.Response{
		reply(llm.StopToolUse, toolUse("t1", "bash", map[string]any{"command": "curl https://example.com"})),
		reply(llm.StopToolUse, toolUse("t2", "write_file", map[string]any{"path": "ok.txt", "content": "fine"})),
		reply(llm.StopEndTurn, llm.Block{Type: llm.BlockText, Text: "done"}),
	}}
	a, root := newAgent(t, p, permission.ModeYolo)
	pol, err := policy.Parse([]byte(`{"version":1,"message":"Ask IT.","commands":{"deny":["curl"]}}`), "test-policy.json")
	if err != nil {
		t.Fatal(err)
	}
	enf, err := policy.NewEnforcer(pol)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "audit")
	lg, err := audit.Open(audit.Options{Dir: dir, Session: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	a.Gov = &Governance{Policy: enf, Audit: lg, ProviderName: "anthropic"}
	s := &sink{decision: permission.DecisionAllowOnce}
	if err := a.Run(context.Background(), llm.UserText("go"), s); err != nil {
		t.Fatal(err)
	}
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}
	res := a.History()[2].Blocks[0]
	if !res.IsError || !strings.Contains(res.Text, "Ask IT.") {
		t.Fatalf("model did not get the policy denial: %+v", res)
	}
	if len(s.asked) != 0 {
		t.Fatalf("a policy denial must not prompt, asked %v", s.asked)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "ok.txt")); string(b) != "fine" {
		t.Fatal("allowed write did not run")
	}
	counts := map[audit.EventType]int{}
	for _, r := range readAudit(t, dir) {
		counts[r.Event]++
		if r.Event == audit.EventToolCall && r.Tool != "write_file" {
			t.Fatalf("unexpected tool record %+v", r)
		}
	}
	if counts[audit.EventPolicyDecision] != 1 || counts[audit.EventToolCall] != 1 || counts[audit.EventModelCall] != 3 {
		t.Fatalf("audit events = %v", counts)
	}
	if v, err := audit.Verify(dir, audit.VerifyOptions{}); err != nil || v.Broken != nil {
		t.Fatalf("chain does not verify: %+v %v", v, err)
	}
}

func TestGovernanceRedactsSecretsBeforeTheModel(t *testing.T) {
	p := &scripted{responses: []*llm.Response{reply(llm.StopEndTurn, llm.Block{Type: llm.BlockText, Text: "ok"})}}
	a, _ := newAgent(t, p, permission.ModeAsk)
	a.Gov = &Governance{Redactor: secrets.Default()}
	key := "nvapi-" + strings.Repeat("Ab3dE", 12)
	s := &sink{}
	if err := a.Run(context.Background(), llm.UserText("my key is "+key), s); err != nil {
		t.Fatal(err)
	}
	sent := p.requests[0].Messages[0].Text()
	if strings.Contains(sent, key) || !strings.Contains(sent, "[REDACTED:") {
		t.Fatalf("secret reached the model: %q", sent)
	}
	if !strings.Contains(a.History()[0].Text(), key) {
		t.Fatal("local history should keep the user's own text")
	}
	if a.Gov.Stats().Redacted != 1 || len(s.notices) != 1 {
		t.Fatalf("stats=%+v notices=%v", a.Gov.Stats(), s.notices)
	}
}

func TestGovernanceBudgetBlocksModelCalls(t *testing.T) {
	p := &scripted{responses: []*llm.Response{
		reply(llm.StopEndTurn, llm.Block{Type: llm.BlockText, Text: "one"}),
		reply(llm.StopEndTurn, llm.Block{Type: llm.BlockText, Text: "two"}),
	}}
	a, _ := newAgent(t, p, permission.ModeAsk)
	tr, err := budget.New(budget.Options{Limits: budget.Limits{SessionUSD: 0.0001}})
	if err != nil {
		t.Fatal(err)
	}
	a.Gov = &Governance{Budget: tr}
	if err := a.Run(context.Background(), llm.UserText("first"), &sink{}); err != nil {
		t.Fatal(err)
	}
	err = a.Run(context.Background(), llm.UserText("second"), &sink{})
	if err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("second call should be blocked by the budget, got %v", err)
	}
	if len(p.requests) != 1 {
		t.Fatalf("blocked call reached the provider: %d requests", len(p.requests))
	}
}
