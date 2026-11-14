package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Errors returned by the coordinator, the client and the worker runtime.
// They survive the HTTP round trip: the client maps error codes back to
// these values, so errors.Is works the same locally and remotely.
var (
	ErrNotLeader         = errors.New("fleet: not the leader")
	ErrNotFound          = errors.New("fleet: not found")
	ErrConflict          = errors.New("fleet: conflict")
	ErrInvalid           = errors.New("fleet: invalid request")
	ErrLeaseLost         = errors.New("fleet: lease lost")
	ErrUnknownWorker     = errors.New("fleet: unknown worker")
	ErrUnknownDependency = errors.New("fleet: unknown dependency")
	ErrDependencyFailed  = errors.New("fleet: dependency failed")
	ErrStaleFencingToken = errors.New("fleet: stale fencing token")
	ErrLeaseHeld         = errors.New("fleet: lease held by another node")
	ErrClosed            = errors.New("fleet: closed")
	ErrCorruptLog        = errors.New("fleet: corrupt log")
	ErrUnauthorized      = errors.New("fleet: unauthorized")
	ErrForbidden         = errors.New("fleet: forbidden")
	ErrReplay            = errors.New("fleet: replayed request")
	ErrOverloaded        = errors.New("fleet: overloaded")
	ErrBadSignature      = errors.New("fleet: bad signature")
)

// errorCodes maps sentinel errors to stable wire codes.
var errorCodes = []struct {
	err    error
	code   string
	status int
}{
	{ErrNotLeader, "not_leader", 503},
	{ErrNotFound, "not_found", 404},
	{ErrConflict, "conflict", 409},
	{ErrInvalid, "invalid", 400},
	{ErrLeaseLost, "lease_lost", 409},
	{ErrUnknownWorker, "unknown_worker", 404},
	{ErrUnknownDependency, "unknown_dependency", 400},
	{ErrDependencyFailed, "dependency_failed", 409},
	{ErrStaleFencingToken, "stale_fencing_token", 503},
	{ErrLeaseHeld, "lease_held", 409},
	{ErrClosed, "closed", 503},
	{ErrUnauthorized, "unauthorized", 401},
	{ErrForbidden, "forbidden", 403},
	{ErrReplay, "replay", 401},
	{ErrOverloaded, "overloaded", 429},
	{ErrBadSignature, "bad_signature", 401},
	{ErrCorruptLog, "corrupt_log", 500},
}

func errorCode(err error) (code string, status int) {
	for _, e := range errorCodes {
		if errors.Is(err, e.err) {
			return e.code, e.status
		}
	}
	return "internal", 500
}

func errorFromCode(code string) error {
	for _, e := range errorCodes {
		if e.code == code {
			return e.err
		}
	}
	return nil
}

// Priority orders dispatch: every ready task of a higher priority is
// offered before any task of a lower one.
type Priority int

// Priorities, lowest first.
const (
	PriorityLow Priority = iota
	PriorityNormal
	PriorityHigh
	PriorityCritical
	numPriorities
)

func (p Priority) String() string {
	switch p {
	case PriorityLow:
		return "low"
	case PriorityNormal:
		return "normal"
	case PriorityHigh:
		return "high"
	case PriorityCritical:
		return "critical"
	}
	return "p" + strconv.Itoa(int(p))
}

// ParsePriority accepts a name ("low".."critical") or a number 0..3.
func ParsePriority(s string) (Priority, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	for p := PriorityLow; p < numPriorities; p++ {
		if s == p.String() || s == strconv.Itoa(int(p)) {
			return p, nil
		}
	}
	return 0, fmt.Errorf("%w: unknown priority %q", ErrInvalid, s)
}

// TaskState is where a task is in its lifecycle.
type TaskState string

// Task states. Succeeded, Dead and Canceled are terminal.
const (
	StatePending   TaskState = "pending"   // waiting for dependencies
	StateScheduled TaskState = "scheduled" // waiting for its start time or retry backoff
	StateReady     TaskState = "ready"     // dispatchable
	StateLeased    TaskState = "leased"    // held by a worker
	StateSucceeded TaskState = "succeeded"
	StateDead      TaskState = "dead" // in the dead-letter queue
	StateCanceled  TaskState = "canceled"
)

// AllStates lists every state in lifecycle order.
var AllStates = []TaskState{StatePending, StateScheduled, StateReady, StateLeased, StateSucceeded, StateDead, StateCanceled}

// Terminal reports whether no further transitions happen on their own.
func (s TaskState) Terminal() bool {
	return s == StateSucceeded || s == StateDead || s == StateCanceled
}

func (s TaskState) valid() bool { return slices.Contains(AllStates, s) }

// Duration is a time.Duration that encodes as a Go duration string ("30s")
// and decodes from a string or a number of nanoseconds.
type Duration time.Duration

// D returns the time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		v, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("%w: duration %q", ErrInvalid, s)
		}
		*d = Duration(v)
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("%w: duration %s", ErrInvalid, b)
	}
	*d = Duration(n)
	return nil
}

// Requirements constrain which workers may run a task. Every field that is
// set must be satisfied.
//
// A label requirement "k": "v" needs the worker label k to equal v;
// "k": "" needs k to be present with any value; "k": "!v" needs k to be
// absent or different from v.
type Requirements struct {
	OS     string            `json:"os,omitempty"`
	Arch   string            `json:"arch,omitempty"`
	Tools  []string          `json:"tools,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
}

// Capabilities describe what a worker can run.
type Capabilities struct {
	OS             string            `json:"os,omitempty"`
	Arch           string            `json:"arch,omitempty"`
	Tools          []string          `json:"tools,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
	MaxConcurrency int               `json:"max_concurrency"`
}

// Satisfies reports whether a worker with c can run a task requiring r.
func (c Capabilities) Satisfies(r Requirements) bool {
	if r.OS != "" && !strings.EqualFold(r.OS, c.OS) {
		return false
	}
	if r.Arch != "" && !strings.EqualFold(r.Arch, c.Arch) {
		return false
	}
	for _, t := range r.Tools {
		if !slices.Contains(c.Tools, t) {
			return false
		}
	}
	for k, want := range r.Labels {
		have, ok := c.Labels[k]
		switch {
		case strings.HasPrefix(want, "!"):
			if ok && have == want[1:] {
				return false
			}
		case want == "":
			if !ok {
				return false
			}
		default:
			if !ok || have != want {
				return false
			}
		}
	}
	return true
}

// TaskSpec is what a client submits.
type TaskSpec struct {
	// ID is optional; one is generated when empty.
	ID string `json:"id,omitempty"`
	// Tenant groups tasks for fair sharing; "default" when empty.
	Tenant   string   `json:"tenant,omitempty"`
	Priority Priority `json:"priority"`
	// Kind names what the worker should do, e.g. "agent.prompt".
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload,omitempty"`
	// Requirements select capable workers.
	Requirements Requirements `json:"requirements,omitzero"`
	// IdempotencyKey deduplicates submissions per tenant: a second
	// submission with the same key returns the first task.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	// DependsOn lists tasks that must succeed first. If one fails or is
	// canceled, this task is canceled.
	DependsOn []string `json:"depends_on,omitempty"`
	// NotBefore delays dispatch until the given time; Delay is relative to
	// submission. The later of the two wins.
	NotBefore time.Time `json:"not_before,omitzero"`
	Delay     Duration  `json:"delay,omitempty"`
	// MaxAttempts bounds deliveries before dead-lettering.
	MaxAttempts int `json:"max_attempts,omitempty"`
	// LeaseTimeout is the visibility timeout of each delivery.
	LeaseTimeout Duration `json:"lease_timeout,omitempty"`
	// Labels are free-form metadata echoed back on the task.
	Labels map[string]string `json:"labels,omitempty"`
}

// LeaseInfo describes a task's current lease.
type LeaseInfo struct {
	ID       string    `json:"id"`
	WorkerID string    `json:"worker_id"`
	Granted  time.Time `json:"granted"`
	Expires  time.Time `json:"expires"`
}

// Task is a unit of work and its current state.
type Task struct {
	ID             string            `json:"id"`
	Tenant         string            `json:"tenant"`
	Priority       Priority          `json:"priority"`
	Kind           string            `json:"kind"`
	Payload        json.RawMessage   `json:"payload,omitempty"`
	Requirements   Requirements      `json:"requirements,omitzero"`
	IdempotencyKey string            `json:"idempotency_key,omitempty"`
	DependsOn      []string          `json:"depends_on,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
	MaxAttempts    int               `json:"max_attempts"`
	LeaseTimeout   Duration          `json:"lease_timeout"`

	State    TaskState `json:"state"`
	Attempts int       `json:"attempts"`
	// Seq is the submission order; it breaks ties within a priority band.
	Seq uint64 `json:"seq"`
	// Version increases on every recorded change.
	Version uint64 `json:"version"`

	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	NotBefore   time.Time `json:"not_before,omitzero"`
	ReadyAt     time.Time `json:"ready_at,omitzero"`
	LeasedAt    time.Time `json:"leased_at,omitzero"`
	CompletedAt time.Time `json:"completed_at,omitzero"`

	Lease *LeaseInfo `json:"lease,omitempty"`
	// Closure records how the last lease ended ("complete:<lease>",
	// "fail:<lease>", "release:<lease>", "expired:<lease>",
	// "revoked:<lease>"); it makes worker acknowledgements idempotent.
	Closure string `json:"closure,omitempty"`

	Result     json.RawMessage `json:"result,omitempty"`
	LastError  string          `json:"last_error,omitempty"`
	DeadReason string          `json:"dead_reason,omitempty"`
}

func (t *Task) clone() *Task {
	if t == nil {
		return nil
	}
	c := *t
	c.Payload = slices.Clone(t.Payload)
	c.Result = slices.Clone(t.Result)
	c.DependsOn = slices.Clone(t.DependsOn)
	c.Labels = maps.Clone(t.Labels)
	c.Requirements.Tools = slices.Clone(t.Requirements.Tools)
	c.Requirements.Labels = maps.Clone(t.Requirements.Labels)
	if t.Lease != nil {
		l := *t.Lease
		c.Lease = &l
	}
	return &c
}

// SubmitResult is the outcome of one submission.
type SubmitResult struct {
	Task *Task `json:"task"`
	// Duplicate is true when the idempotency key matched an existing task,
	// which is returned unchanged.
	Duplicate bool `json:"duplicate"`
}

// ListFilter selects tasks for List. Results are in submission order;
// pass the last returned Seq as After to page.
type ListFilter struct {
	Tenant string    `json:"tenant,omitempty"`
	State  TaskState `json:"state,omitempty"`
	After  uint64    `json:"after,omitempty"`
	Limit  int       `json:"limit,omitempty"`
}

// WorkerAPI is what a worker needs from a coordinator. Both *Coordinator
// and *Client implement it.
type WorkerAPI interface {
	Register(ctx context.Context, req RegisterRequest) (RegisterResponse, error)
	Heartbeat(ctx context.Context, req HeartbeatRequest) (HeartbeatResponse, error)
	Lease(ctx context.Context, req LeaseRequest) (LeaseResponse, error)
	Complete(ctx context.Context, req CompleteRequest) error
	Fail(ctx context.Context, req FailRequest) error
	Release(ctx context.Context, req ReleaseRequest) error
	Drain(ctx context.Context, workerID string) error
	Deregister(ctx context.Context, workerID string) error
}

// RegisterRequest announces a worker.
type RegisterRequest struct {
	// WorkerID is stable across restarts of the same machine; generated
	// when empty.
	WorkerID string `json:"worker_id,omitempty"`
	// Incarnation is unique per worker process. A registration with a new
	// incarnation tells the coordinator the previous process died, so its
	// leases are reclaimed at once instead of waiting for expiry.
	Incarnation  string       `json:"incarnation,omitempty"`
	Capabilities Capabilities `json:"capabilities"`
	Version      string       `json:"version,omitempty"`
}

// RegisterResponse tells the worker how to behave.
type RegisterResponse struct {
	WorkerID          string   `json:"worker_id"`
	HeartbeatInterval Duration `json:"heartbeat_interval"`
	MaxLongPoll       Duration `json:"max_long_poll"`
}

// LeaseRef names one lease held by a worker.
type LeaseRef struct {
	TaskID  string `json:"task_id"`
	LeaseID string `json:"lease_id"`
}

// HeartbeatRequest reports a worker alive and the leases it still holds.
type HeartbeatRequest struct {
	WorkerID string     `json:"worker_id"`
	Leases   []LeaseRef `json:"leases,omitempty"`
}

// HeartbeatResponse lists leases the worker must abandon (expired,
// canceled or reassigned) and the worker's administrative state.
type HeartbeatResponse struct {
	Extended    int         `json:"extended"`
	Revoked     []LeaseRef  `json:"revoked,omitempty"`
	State       WorkerState `json:"state"`
	Quarantined bool        `json:"quarantined"`
	Draining    bool        `json:"draining"`
}

// LeaseRequest asks for up to Max tasks, waiting up to Wait for one to
// become available.
type LeaseRequest struct {
	WorkerID string   `json:"worker_id"`
	Max      int      `json:"max"`
	Wait     Duration `json:"wait,omitempty"`
}

// Assignment is a leased task.
type Assignment struct {
	Task    *Task     `json:"task"`
	LeaseID string    `json:"lease_id"`
	Expires time.Time `json:"expires"`
	Attempt int       `json:"attempt"`
	// FencingToken is the coordinator's leadership token at grant time.
	// Side effects that support it (e.g. compare-and-set writes) can use it
	// to reject work from deposed coordinators.
	FencingToken uint64 `json:"fencing_token"`
}

// LeaseResponse carries granted assignments.
type LeaseResponse struct {
	Assignments []Assignment `json:"assignments,omitempty"`
	Draining    bool         `json:"draining,omitempty"`
	Quarantined bool         `json:"quarantined,omitempty"`
	// RetryAfter suggests when to poll again if nothing was granted.
	RetryAfter Duration `json:"retry_after,omitempty"`
}

// CompleteRequest finishes a leased task successfully.
type CompleteRequest struct {
	WorkerID string          `json:"worker_id"`
	TaskID   string          `json:"task_id"`
	LeaseID  string          `json:"lease_id"`
	Result   json.RawMessage `json:"result,omitempty"`
}

// FailRequest reports a failed attempt.
type FailRequest struct {
	WorkerID string `json:"worker_id"`
	TaskID   string `json:"task_id"`
	LeaseID  string `json:"lease_id"`
	Error    string `json:"error"`
	// Permanent sends the task straight to the dead-letter queue.
	Permanent bool `json:"permanent,omitempty"`
	// RetryAfter overrides the retry backoff (bounded by the coordinator).
	RetryAfter Duration `json:"retry_after,omitempty"`
}

// ReleaseRequest returns a lease unused; the attempt is not counted.
type ReleaseRequest struct {
	WorkerID string `json:"worker_id"`
	TaskID   string `json:"task_id"`
	LeaseID  string `json:"lease_id"`
}

var (
	tenantRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	idRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
)

func validTenant(s string) bool { return tenantRE.MatchString(s) }
func validID(s string) bool     { return idRE.MatchString(s) }
