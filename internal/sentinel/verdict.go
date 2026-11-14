package sentinel

import (
	"fmt"
	"time"
)

// Severity ranks a finding.
type Severity uint8

// Severities, in increasing order.
const (
	SeverityNone Severity = iota
	SeverityInfo
	SeverityLow
	SeverityMedium
	SeverityHigh
	SeverityCritical
	numSeverities
)

var severityNames = [numSeverities]string{"none", "info", "low", "medium", "high", "critical"}

func (s Severity) String() string {
	if s < numSeverities {
		return severityNames[s]
	}
	return fmt.Sprintf("severity(%d)", uint8(s))
}

// MarshalText implements encoding.TextMarshaler.
func (s Severity) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (s *Severity) UnmarshalText(b []byte) error {
	i, err := parseName(severityNames[:], string(b), "severity")
	*s = Severity(i)
	return err
}

// Action is what the sentinel does about a finding.
type Action uint8

// Actions, in increasing order of strictness.
const (
	ActionNone Action = iota
	// ActionLog records an incident and nothing else.
	ActionLog
	// ActionWarn lets the event through with a warning.
	ActionWarn
	// ActionThrottle rate-limits the session with a token bucket.
	ActionThrottle
	// ActionRequireApproval asks the user before the event may proceed.
	ActionRequireApproval
	// ActionPause blocks the session until Resume.
	ActionPause
	// ActionKill engages the global kill switch until ClearKill.
	ActionKill
	numActions
)

var actionNames = [numActions]string{"none", "log", "warn", "throttle", "require_approval", "pause", "kill"}

func (a Action) String() string {
	if a < numActions {
		return actionNames[a]
	}
	return fmt.Sprintf("action(%d)", uint8(a))
}

// MarshalText implements encoding.TextMarshaler.
func (a Action) MarshalText() ([]byte, error) { return []byte(a.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (a *Action) UnmarshalText(b []byte) error {
	i, err := parseName(actionNames[:], string(b), "action")
	*a = Action(i)
	return err
}

// Decision is the gate outcome of a Verdict.
type Decision uint8

// Decisions, in increasing order of strictness.
const (
	// DecisionAllow lets the event proceed.
	DecisionAllow Decision = iota
	// DecisionWarn lets the event proceed; the caller should surface Reason.
	DecisionWarn
	// DecisionThrottle defers the event; retry after RetryAfter (see
	// Sentinel.Recheck and Guard.Wait).
	DecisionThrottle
	// DecisionAsk requires explicit user approval before proceeding. It
	// outranks throttling: a human in the loop already sets the pace.
	DecisionAsk
	// DecisionDeny blocks the event (paused session or open circuit).
	DecisionDeny
	// DecisionKill blocks the event because the kill switch is engaged.
	DecisionKill
	numDecisions
)

var decisionNames = [numDecisions]string{"allow", "warn", "throttle", "ask", "deny", "kill"}

func (d Decision) String() string {
	if d < numDecisions {
		return decisionNames[d]
	}
	return fmt.Sprintf("decision(%d)", uint8(d))
}

// MarshalText implements encoding.TextMarshaler.
func (d Decision) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Decision) UnmarshalText(b []byte) error {
	i, err := parseName(decisionNames[:], string(b), "decision")
	*d = Decision(i)
	return err
}

func parseName(names []string, s, what string) (int, error) {
	for i, n := range names {
		if n == s {
			return i, nil
		}
	}
	return 0, fmt.Errorf("sentinel: unknown %s %q", what, s)
}

// Verdict is the sentinel's answer for one event.
type Verdict struct {
	Decision Decision `json:"decision"`
	// Severity is the highest severity behind the decision.
	Severity Severity `json:"severity,omitempty"`
	// Reason explains a non-allow decision in one line.
	Reason string `json:"reason,omitempty"`
	// Detector names the detector (or "breaker", "posture", "kill") behind it.
	Detector string `json:"detector,omitempty"`
	// Incident is the ID of the incident behind the decision, if any.
	Incident string `json:"incident,omitempty"`
	// RetryAfter is how long to wait before retrying a throttled or
	// circuit-broken event.
	RetryAfter time.Duration `json:"retry_after,omitempty"`
}

// Allowed reports whether the event may proceed without user involvement.
func (v Verdict) Allowed() bool { return v.Decision <= DecisionWarn }

// MayRun reports whether the event may still run now, possibly after the
// user approves it.
func (v Verdict) MayRun() bool { return v.Decision != DecisionThrottle && v.Decision < DecisionDeny }

// Stricter returns whichever of v and o is stricter, preferring v on ties.
func (v Verdict) Stricter(o Verdict) Verdict {
	if o.Decision > v.Decision {
		return o
	}
	if o.Decision == v.Decision && o.Severity > v.Severity {
		return o
	}
	return v
}

func (v Verdict) String() string {
	if v.Reason == "" {
		return v.Decision.String()
	}
	return v.Decision.String() + ": " + v.Reason
}

// decisionFor maps an action to the decision it implies for the event that
// produced it.
func decisionFor(a Action) Decision {
	switch a {
	case ActionWarn:
		return DecisionWarn
	case ActionThrottle:
		return DecisionWarn // the bucket, not the finding, defers events
	case ActionRequireApproval:
		return DecisionAsk
	case ActionPause:
		return DecisionDeny
	case ActionKill:
		return DecisionKill
	}
	return DecisionAllow
}
