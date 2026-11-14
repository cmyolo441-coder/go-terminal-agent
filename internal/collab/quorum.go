package collab

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// QuorumRule is satisfied by Count votes from participants whose role is
// at least MinRole.
type QuorumRule struct {
	MinRole Role `json:"min_role"`
	Count   int  `json:"count"`
}

func (q QuorumRule) String() string { return fmt.Sprintf("%d %s", q.Count, q.MinRole) }

// QuorumPolicy decides tool-call approvals. A request is approved as soon
// as the approving votes satisfy any Approve rule, denied as soon as the
// denying votes satisfy any Deny rule, denied when every eligible voter
// has voted without either happening, and expires after Timeout.
type QuorumPolicy struct {
	Approve []QuorumRule  `json:"approve"`
	Deny    []QuorumRule  `json:"deny"`
	Timeout time.Duration `json:"timeout"`
}

// DefaultQuorumPolicy approves with any owner or two editors, denies the
// same way, and expires after two minutes.
func DefaultQuorumPolicy() QuorumPolicy {
	rules := []QuorumRule{{MinRole: RoleOwner, Count: 1}, {MinRole: RoleEditor, Count: 2}}
	return QuorumPolicy{Approve: rules, Deny: slices.Clone(rules), Timeout: 2 * time.Minute}
}

// Validate checks the policy.
func (p QuorumPolicy) Validate() error {
	if len(p.Approve) == 0 {
		return errors.New("collab: quorum policy needs at least one approve rule")
	}
	for _, r := range slices.Concat(p.Approve, p.Deny) {
		if !r.MinRole.AtLeast(RoleEditor) || r.Count < 1 || r.Count > 1000 {
			return fmt.Errorf("collab: quorum rule %q must need 1..1000 votes from editors or owners", r)
		}
	}
	if p.Timeout < 0 {
		return errors.New("collab: quorum timeout must not be negative")
	}
	return nil
}

func (p QuorumPolicy) String() string {
	parts := make([]string, len(p.Approve))
	for i, r := range p.Approve {
		parts[i] = r.String()
	}
	return "approve with " + strings.Join(parts, " or ")
}

// Decision is an approval outcome.
type Decision string

// Decisions.
const (
	DecisionPending   Decision = "pending"
	DecisionApproved  Decision = "approved"
	DecisionDenied    Decision = "denied"
	DecisionExpired   Decision = "expired"
	DecisionCancelled Decision = "cancelled"
)

// Vote is one participant's vote.
type Vote struct {
	Participant string    `json:"participant"`
	Name        string    `json:"name,omitempty"`
	Role        Role      `json:"role"`
	Approve     bool      `json:"approve"`
	Comment     string    `json:"comment,omitempty"`
	Time        time.Time `json:"time"`
}

func satisfied(rules []QuorumRule, votes []Vote, approve bool) bool {
	for _, r := range rules {
		n := 0
		for _, v := range votes {
			if v.Approve == approve && v.Role.AtLeast(r.MinRole) {
				n++
			}
		}
		if n >= r.Count {
			return true
		}
	}
	return false
}

// Evaluate returns the decision the votes reach. eligible is the number of
// participants allowed to vote; when all of them have voted without
// reaching a quorum the request is denied.
func (p QuorumPolicy) Evaluate(votes []Vote, eligible int) Decision {
	switch {
	case satisfied(p.Approve, votes, true):
		return DecisionApproved
	case satisfied(p.Deny, votes, false):
		return DecisionDenied
	case eligible > 0 && len(votes) >= eligible:
		return DecisionDenied
	}
	return DecisionPending
}

// ApprovalRequest asks the room to approve a tool call.
type ApprovalRequest struct {
	// ID is optional; the room assigns one when empty.
	ID      string          `json:"id"`
	Tool    string          `json:"tool"`
	Summary string          `json:"summary,omitempty"`
	Input   json.RawMessage `json:"input,omitempty"`
}

// ApprovalView is an approval as clients see it.
type ApprovalView struct {
	ApprovalRequest
	Requested time.Time `json:"requested"`
	Deadline  time.Time `json:"deadline,omitzero"`
	Votes     []Vote    `json:"votes"`
	Decision  Decision  `json:"decision"`
	Reason    string    `json:"reason,omitempty"`
}

// Outcome is the result of RequestApproval.
type Outcome struct {
	ID       string   `json:"id"`
	Decision Decision `json:"decision"`
	Reason   string   `json:"reason,omitempty"`
	Votes    []Vote   `json:"votes"`
}

// Approved reports whether the tool call may run.
func (o Outcome) Approved() bool { return o.Decision == DecisionApproved }

// approval is a pending request inside the room actor.
type approval struct {
	view ApprovalView
	done chan Outcome // buffered(1); receives exactly once
}

// vote records v, replacing an earlier vote by the same participant.
func (a *approval) vote(v Vote) {
	for i := range a.view.Votes {
		if a.view.Votes[i].Participant == v.Participant {
			a.view.Votes[i] = v
			return
		}
	}
	a.view.Votes = append(a.view.Votes, v)
}

func (a *approval) resolve(d Decision, reason string) Outcome {
	a.view.Decision, a.view.Reason = d, reason
	o := Outcome{ID: a.view.ID, Decision: d, Reason: reason, Votes: slices.Clone(a.view.Votes)}
	select {
	case a.done <- o:
	default:
	}
	return o
}
