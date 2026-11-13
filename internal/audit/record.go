// Package audit keeps an append-only, tamper-evident log of what an agent
// session did: every tool call, approval decision, model call and policy
// decision.
//
// The log is JSON Lines, one segment file per UTC day
// (audit-2026-11-13.jsonl), rotated by size into numbered segments
// (audit-2026-11-13.0001.jsonl). Files are created 0600 in a 0700
// directory. Every record carries the hash of the record before it, across
// segments, so editing, inserting, deleting or reordering a record breaks
// the chain at that point; Verify finds the first broken link. With an
// HMAC key the chain also resists an attacker who recomputes every hash.
//
// Several gta processes can share one log directory: appends take a
// cross-process file lock and re-read the chain head, so the chain stays
// linear. Free-text fields pass through the secrets redactor before they
// are written; the log never holds a credential.
package audit

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

// EventType classifies a record.
type EventType string

const (
	EventSessionStart   EventType = "session_start"
	EventSessionEnd     EventType = "session_end"
	EventToolCall       EventType = "tool_call"
	EventApproval       EventType = "approval"
	EventModelCall      EventType = "model_call"
	EventPolicyDecision EventType = "policy_decision"
	EventConfigChange   EventType = "config_change"
	EventBudget         EventType = "budget"
	EventSecrets        EventType = "secrets_redacted"
	// EventRecovered is written by the logger itself when it finds a torn
	// final line left by a crash mid-write and cuts it off.
	EventRecovered EventType = "log_recovered"
)

// Decision values used in records.
const (
	DecisionAllow   = "allow"
	DecisionAsk     = "ask"
	DecisionDeny    = "deny"
	DecisionOnce    = "allow_once"
	DecisionSession = "allow_session"
	DecisionDecline = "declined"
	DecisionWarn    = "warn"
	DecisionBlock   = "block"
)

// RecordVersion is the record format version.
const RecordVersion = 1

// Record is one audit event. The logger fills V, Seq, Time (when zero),
// the identity fields (when empty), Alg, PrevHash and Hash.
type Record struct {
	V         int       `json:"v"`
	Seq       uint64    `json:"seq"`
	Time      time.Time `json:"time"`
	Event     EventType `json:"event"`
	Session   string    `json:"session,omitempty"`
	User      string    `json:"user,omitempty"`
	Host      string    `json:"host,omitempty"`
	PID       int       `json:"pid,omitempty"`
	Workspace string    `json:"workspace,omitempty"`

	Tool      string `json:"tool,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`
	Kind      string `json:"kind,omitempty"`
	// Input is a redacted, truncated summary of the tool input.
	Input    string `json:"input,omitempty"`
	Decision string `json:"decision,omitempty"`
	Reason   string `json:"reason,omitempty"`
	// DurationMS is how long the call took.
	DurationMS int64  `json:"duration_ms,omitempty"`
	Error      string `json:"error,omitempty"`

	Provider         string  `json:"provider,omitempty"`
	Model            string  `json:"model,omitempty"`
	InputTokens      int64   `json:"input_tokens,omitempty"`
	OutputTokens     int64   `json:"output_tokens,omitempty"`
	CacheReadTokens  int64   `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64   `json:"cache_write_tokens,omitempty"`
	CostUSD          float64 `json:"cost_usd,omitempty"`

	// Redactions counts secrets removed from this record's fields.
	Redactions int               `json:"redactions,omitempty"`
	Details    map[string]string `json:"details,omitempty"`

	Alg      string `json:"alg"`
	PrevHash string `json:"prev_hash"`
	// Hash must stay the last field: it is appended to the serialized
	// record, and Verify strips it to recompute the digest.
	Hash string `json:"hash,omitempty"`
}

// ToolCall builds a tool_call record. input is the raw tool input; the
// logger redacts and truncates it.
func ToolCall(tool, toolUseID, kind string, input json.RawMessage, decision string, d time.Duration, err error) Record {
	r := Record{Event: EventToolCall, Tool: tool, ToolUseID: toolUseID, Kind: kind,
		Input: string(input), Decision: decision, DurationMS: d.Milliseconds()}
	if err != nil {
		r.Error = err.Error()
	}
	return r
}

// Approval builds an approval record for the user's answer to a prompt.
func Approval(tool, toolUseID, decision, reason string) Record {
	return Record{Event: EventApproval, Tool: tool, ToolUseID: toolUseID, Decision: decision, Reason: reason}
}

// ModelCall builds a model_call record.
func ModelCall(provider, model string, u llm.Usage, costUSD float64, d time.Duration, err error) Record {
	r := Record{Event: EventModelCall, Provider: provider, Model: model,
		InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
		CacheReadTokens: u.CacheReadTokens, CacheWriteTokens: u.CacheWriteTokens,
		CostUSD: costUSD, DurationMS: d.Milliseconds()}
	if err != nil {
		r.Error = err.Error()
	}
	return r
}

// PolicyDecision builds a policy_decision record. subject names what was
// checked (a tool, a model, a config field).
func PolicyDecision(subject, decision, reason string, details map[string]string) Record {
	return Record{Event: EventPolicyDecision, Tool: subject, Decision: decision, Reason: reason, Details: details}
}

// truncate shortens s to at most n bytes on a UTF-8 boundary and says how
// much was cut.
func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && cut < len(s) && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	var b strings.Builder
	b.WriteString(s[:cut])
	b.WriteString("…[truncated ")
	b.WriteString(strconv.Itoa(len(s) - cut))
	b.WriteString(" bytes]")
	return b.String()
}
