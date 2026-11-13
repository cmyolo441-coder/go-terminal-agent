package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/audit"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/budget"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/policy"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/secrets"
)

// Governance holds the enterprise controls an agent enforces: a managed
// policy, an append-only audit log, secret redaction for everything sent to
// the model, and spending caps. Every field is optional; a nil Governance
// turns all of them off.
type Governance struct {
	// Policy denies tool calls a managed policy forbids. A user cannot
	// approve past a policy denial.
	Policy *policy.Enforcer
	// Audit records tool calls, approvals, model calls and policy
	// decisions. When AuditRequired is set and a record cannot be written,
	// tools stop running.
	Audit         audit.Logger
	AuditRequired bool
	// Redactor replaces secrets in messages before they reach the model.
	Redactor *secrets.Redactor
	// Budget blocks model calls once a spending cap is reached.
	Budget *budget.Tracker
	// ProviderName labels model-call audit records.
	ProviderName string

	// viaParent marks a child agent's governance: its spend and model
	// calls are recorded by the parent through AddUsage, so it only checks.
	viaParent bool

	mu        sync.Mutex
	redacted  int
	auditErr  error
	lastBlock string
}

// Child returns governance for a sub-agent: the same policy, audit log,
// redaction and caps, with spend recorded once, by the parent.
func (g *Governance) Child() *Governance {
	if g == nil {
		return nil
	}
	return &Governance{Policy: g.Policy, Audit: g.Audit, AuditRequired: g.AuditRequired, Redactor: g.Redactor, Budget: g.Budget, ProviderName: g.ProviderName, viaParent: true}
}

// GovernanceStats is a snapshot of what governance has done this session.
type GovernanceStats struct {
	Redacted  int
	AuditErr  error
	LastBlock string
}

// Stats reports redaction counts and audit health.
func (g *Governance) Stats() GovernanceStats {
	if g == nil {
		return GovernanceStats{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return GovernanceStats{Redacted: g.redacted, AuditErr: g.auditErr, LastBlock: g.lastBlock}
}

// log writes a record; failures are remembered so a required audit log can
// stop tools.
func (g *Governance) log(r audit.Record) {
	if g == nil || g.Audit == nil {
		return
	}
	if err := g.Audit.Log(r); err != nil {
		g.mu.Lock()
		g.auditErr = err
		g.mu.Unlock()
	}
}

// auditBroken returns an error when the audit log is required but failing.
func (g *Governance) auditBroken() error {
	if g == nil || !g.AuditRequired {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.auditErr != nil {
		return fmt.Errorf("the audit log is required but cannot be written: %v", g.auditErr)
	}
	return nil
}

// beforeModel checks the budget and redacts secrets in req. It returns an
// error when the call must not be made.
func (g *Governance) beforeModel(req *llm.Request, contextTokens int64, sink Sink) error {
	if g == nil {
		return nil
	}
	if g.Budget != nil {
		d := g.Budget.Check(req.Model, contextTokens, int64(req.MaxTokens))
		switch d.Verdict {
		case budget.Block:
			g.mu.Lock()
			g.lastBlock = d.Reason
			g.mu.Unlock()
			g.log(audit.Record{Event: audit.EventBudget, Decision: audit.DecisionBlock, Reason: d.Reason, Model: req.Model})
			return fmt.Errorf("budget: %s", d.Reason)
		case budget.Warn:
			sink.Notice("Budget: " + d.Reason)
		}
	}
	if g.Redactor != nil {
		msgs, rep := g.Redactor.RedactMessages(req.Messages)
		req.Messages = msgs
		n := rep.Count()
		// The system prompt carries memory files (AGENTS.md, CLAUDE.md)
		// and hook output, which can hold secrets too.
		if sys, fs := g.Redactor.Redact(req.System); len(fs) > 0 {
			req.System = sys
			n += len(fs)
		}
		if n > 0 {
			g.mu.Lock()
			first := g.redacted == 0
			g.redacted += n
			g.mu.Unlock()
			g.log(audit.Record{Event: audit.EventSecrets, Reason: fmt.Sprintf("%d secret(s) redacted before a model call", n), Model: req.Model})
			if first {
				sink.Notice(fmt.Sprintf("Redacted %d secret(s) before sending to the model; see /audit.", n))
			}
		}
	}
	return nil
}

// afterModel records spend and audits the call.
func (g *Governance) afterModel(model string, u llm.Usage, d time.Duration, err error, sink Sink) {
	if g == nil || (g.viaParent && err == nil) {
		return
	}
	cost := 0.0
	if g.Budget != nil && err == nil {
		res, rerr := g.Budget.Record(model, u)
		cost = res.CostUSD
		if sink != nil {
			for _, c := range res.Crossed {
				sink.Notice("Budget: " + c.Reason)
			}
			if rerr != nil {
				sink.Notice("Budget ledger: " + rerr.Error())
			} else if res.Warning != "" {
				sink.Notice("Budget ledger: " + res.Warning)
			}
		}
	}
	g.log(audit.ModelCall(g.ProviderName, model, u, cost, d, err))
}

// governedStream wraps a provider call with the budget, redaction and audit.
func (a *Agent) governedStream(ctx context.Context, req llm.Request, sink Sink) (*llm.Response, error) {
	g := a.Gov
	if err := g.beforeModel(&req, a.contextTokens(), sink); err != nil {
		return nil, err
	}
	start := time.Now()
	resp, err := a.Provider.Stream(ctx, req, sink.Stream)
	if g != nil {
		model := req.Model
		var u llm.Usage
		if resp != nil {
			u = resp.Usage
			if resp.Model != "" {
				model = resp.Model
			}
		}
		g.afterModel(model, u, time.Since(start), err, sink)
	}
	return resp, err
}

func (a *Agent) contextTokens() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return int64(a.context)
}
