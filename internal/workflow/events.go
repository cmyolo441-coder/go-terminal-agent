package workflow

import (
	"fmt"
	"time"
)

// EventType is the kind of an observability event.
type EventType string

// Event types.
const (
	EventRunStarted          EventType = "run_started"
	EventRunResumed          EventType = "run_resumed"
	EventStepStarted         EventType = "step_started"
	EventStepRetrying        EventType = "step_retrying"
	EventStepSucceeded       EventType = "step_succeeded"
	EventStepFailed          EventType = "step_failed"
	EventStepSkipped         EventType = "step_skipped"
	EventStepCancelled       EventType = "step_cancelled"
	EventCompensationStarted EventType = "compensation_started"
	EventStepCompensated     EventType = "step_compensated"
	EventCompensationFailed  EventType = "compensation_failed"
	EventRunFinished         EventType = "run_finished"
)

// Event is a run or step lifecycle notification.
type Event struct {
	Type  EventType
	Time  time.Time
	RunID string
	// Step is the step instance ID; empty for run events.
	Step    string
	Attempt int
	// Err is set for failures and retries.
	Err   error
	Class ErrorClass
	// Delay is the backoff before the next attempt, for EventStepRetrying.
	Delay time.Duration
	// Duration is the step's elapsed time for terminal step events.
	Duration time.Duration
	// Reason explains skips and cancellations.
	Reason string
	// Status is the run outcome, for EventRunFinished.
	Status RunStatus
}

func (e Event) String() string {
	s := fmt.Sprintf("%s %s", e.Time.Format(time.RFC3339Nano), e.Type)
	if e.Step != "" {
		s += " " + e.Step
	}
	if e.Attempt > 0 {
		s += fmt.Sprintf(" attempt=%d", e.Attempt)
	}
	if e.Delay > 0 {
		s += " delay=" + e.Delay.String()
	}
	if e.Status != "" {
		s += " status=" + string(e.Status)
	}
	if e.Reason != "" {
		s += " reason=" + e.Reason
	}
	if e.Err != nil {
		s += fmt.Sprintf(" class=%s err=%q", e.Class, e.Err.Error())
	}
	return s
}
