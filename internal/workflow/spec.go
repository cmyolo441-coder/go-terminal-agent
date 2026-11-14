package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// SpecVersion is the only spec format version this package understands.
const SpecVersion = 1

// Limits that keep a single spec from exhausting the host.
const (
	MaxSteps           = 1000
	MaxMatrixInstances = 256
	MaxRetryAttempts   = 100
	maxSpecBytes       = 8 << 20
)

// Spec is a declarative workflow definition.
type Spec struct {
	// Version is the spec format version; 0 is read as SpecVersion.
	Version int `json:"version,omitempty"`
	// Name identifies the workflow in events and reports.
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	// Inputs declares the run parameters, available as ${inputs.NAME}.
	Inputs map[string]InputSpec `json:"inputs,omitempty"`
	// Concurrency caps how many steps run at once; 0 means unlimited.
	Concurrency int `json:"concurrency,omitempty"`
	// Groups declares named concurrency groups and their limits.
	Groups map[string]int `json:"groups,omitempty"`
	// ContinueOnFailure keeps independent branches running after a step
	// fails. The default is fail-fast: the first unrecoverable failure
	// cancels running steps and stops dispatch.
	ContinueOnFailure bool `json:"continue_on_failure,omitempty"`
	// Timeout bounds the whole run, measured from its first start.
	Timeout Duration   `json:"timeout,omitempty"`
	Steps   []StepSpec `json:"steps"`
	// Outputs are templates evaluated once every step succeeded.
	Outputs map[string]string `json:"outputs,omitempty"`
}

// InputSpec declares one workflow input.
type InputSpec struct {
	// Type is one of string, number, bool, list, object or any (the
	// default).
	Type        string `json:"type,omitempty"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Default     any    `json:"default,omitempty"`
}

// StepSpec is one node of the workflow graph.
type StepSpec struct {
	// ID is unique within the workflow: a letter or underscore followed by
	// letters, digits, underscores or hyphens. Reference an ID containing
	// a hyphen with an index, as in steps['run-tests'].
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	// Type names the registered Executor that runs the step.
	Type string `json:"type"`
	// Inputs are passed to the executor after template expansion. Strings
	// anywhere in the tree may embed ${...} expressions; a string that is
	// exactly one ${...} keeps the expression's type.
	Inputs    map[string]any `json:"inputs,omitempty"`
	DependsOn []string       `json:"depends_on,omitempty"`
	// When is a condition expression (with or without ${ }). Unless it
	// calls success(), failure() or always(), it is implicitly
	// success() && (when).
	When string `json:"when,omitempty"`
	// Matrix fans the step out into one instance per combination of axis
	// values. Each axis is a non-empty list of scalars, or a template that
	// evaluates to one from inputs and env.
	Matrix map[string]any `json:"matrix,omitempty"`
	// MaxParallel caps how many instances of a matrix step run at once.
	MaxParallel int `json:"max_parallel,omitempty"`
	// Retry is the retry policy; nil means a single attempt.
	Retry *RetrySpec `json:"retry,omitempty"`
	// Timeout bounds each attempt.
	Timeout Duration `json:"timeout,omitempty"`
	// Compensate undoes the step's effect when the run fails (saga).
	Compensate *CompensationSpec `json:"compensate,omitempty"`
	// Group puts the step in a concurrency group declared in Spec.Groups.
	Group string `json:"group,omitempty"`
	// ContinueOnError tolerates the step's failure: the run does not fail
	// and dependents see it as successful.
	ContinueOnError bool `json:"continue_on_error,omitempty"`
	// Outputs declares the outputs the executor must produce. When set,
	// references to other outputs of this step are rejected.
	Outputs []string `json:"outputs,omitempty"`
	// Estimate is the expected duration used for critical-path ranking;
	// 0 means one second.
	Estimate Duration `json:"estimate,omitempty"`
}

// RetrySpec is a step's retry policy.
type RetrySpec struct {
	// MaxAttempts counts the first attempt; default 3.
	MaxAttempts int `json:"max_attempts,omitempty"`
	// InitialBackoff is the backoff ceiling after the first failure;
	// default 1s.
	InitialBackoff Duration `json:"initial_backoff,omitempty"`
	// MaxBackoff caps the backoff ceiling; default 1m.
	MaxBackoff Duration `json:"max_backoff,omitempty"`
	// Multiplier grows the ceiling per attempt; default 2.
	Multiplier float64 `json:"multiplier,omitempty"`
	// RetryOn lists the retryable error classes (transient, timeout,
	// error, or * for all three); default transient and timeout.
	// Permanent errors are never retried.
	RetryOn []string `json:"retry_on,omitempty"`
}

// CompensationSpec is the step that undoes a completed step.
type CompensationSpec struct {
	Type string `json:"type"`
	// Inputs are templated like step inputs and may also reference the
	// compensated step's outputs as ${self.outputs.NAME}.
	Inputs  map[string]any `json:"inputs,omitempty"`
	Retry   *RetrySpec     `json:"retry,omitempty"`
	Timeout Duration       `json:"timeout,omitempty"`
}

// Duration is a time.Duration that reads from JSON as a Go duration string
// ("1m30s") or a number of seconds, and writes as a string.
type Duration time.Duration

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		v, err := time.ParseDuration(strings.TrimSpace(s))
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", s, err)
		}
		if v < 0 {
			return fmt.Errorf("invalid duration %q: negative", s)
		}
		*d = Duration(v)
		return nil
	}
	var secs float64
	if err := json.Unmarshal(b, &secs); err != nil {
		return fmt.Errorf("invalid duration %s: want a string like \"30s\" or a number of seconds", b)
	}
	if secs < 0 || math.IsNaN(secs) || secs > math.MaxInt64/float64(time.Second) {
		return fmt.Errorf("invalid duration %s", b)
	}
	*d = Duration(secs * float64(time.Second))
	return nil
}

// ParseSpec decodes a JSON spec strictly: unknown fields, trailing data and
// oversized documents are errors. It does not validate the graph; see
// Validate.
func ParseSpec(data []byte) (*Spec, error) {
	if len(data) > maxSpecBytes {
		return nil, fmt.Errorf("workflow spec is %d bytes; the limit is %d", len(data), maxSpecBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var s Spec
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("decode workflow spec: %w", describeJSONError(data, err))
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("decode workflow spec: unexpected data after the top-level object")
	}
	return &s, nil
}

// LoadSpec reads and decodes a spec file.
func LoadSpec(path string) (*Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s, err := ParseSpec(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// describeJSONError adds a line:column position to JSON syntax and type
// errors.
func describeJSONError(data []byte, err error) error {
	var off int64 = -1
	var se *json.SyntaxError
	var te *json.UnmarshalTypeError
	switch {
	case errors.As(err, &se):
		off = se.Offset
	case errors.As(err, &te):
		off = te.Offset
	}
	if off < 0 || off > int64(len(data)) {
		return err
	}
	line, col := 1, 1
	for _, c := range data[:off] {
		if c == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return fmt.Errorf("line %d, column %d: %w", line, col, err)
}

// StepStatus is the state of a step (or matrix instance).
type StepStatus string

// Step states.
const (
	StepPending   StepStatus = "pending"
	StepRunning   StepStatus = "running"
	StepRetrying  StepStatus = "retrying"
	StepSucceeded StepStatus = "succeeded"
	StepFailed    StepStatus = "failed"
	StepSkipped   StepStatus = "skipped"
	StepCancelled StepStatus = "cancelled"
)

// Terminal reports whether the status is final.
func (s StepStatus) Terminal() bool {
	switch s {
	case StepSucceeded, StepFailed, StepSkipped, StepCancelled:
		return true
	}
	return false
}

// RunStatus is the outcome of a run.
type RunStatus string

// Run outcomes.
const (
	RunRunning     RunStatus = "running"
	RunSucceeded   RunStatus = "succeeded"
	RunFailed      RunStatus = "failed"
	RunInterrupted RunStatus = "interrupted"
)

// CompensationStatus is the saga state of a step.
type CompensationStatus string

// Compensation states.
const (
	CompensationNone      CompensationStatus = ""
	CompensationRunning   CompensationStatus = "running"
	CompensationSucceeded CompensationStatus = "succeeded"
	CompensationFailed    CompensationStatus = "failed"
)

// ErrorClass classifies a step failure for retry decisions.
type ErrorClass string

// Error classes.
const (
	// ClassTransient marks failures expected to go away on retry.
	ClassTransient ErrorClass = "transient"
	// ClassTimeout marks an attempt that exceeded its timeout.
	ClassTimeout ErrorClass = "timeout"
	// ClassError is the class of unclassified errors.
	ClassError ErrorClass = "error"
	// ClassPermanent marks failures that must never be retried.
	ClassPermanent ErrorClass = "permanent"
	// classCancelled is internal: the attempt stopped because the run was
	// cancelled, which is not a failure of the step.
	classCancelled ErrorClass = "cancelled"
)

// StepError attaches an ErrorClass to an executor error.
type StepError struct {
	Class ErrorClass
	Err   error
}

func (e *StepError) Error() string {
	if e.Err == nil {
		return string(e.Class) + " error"
	}
	return e.Err.Error()
}

func (e *StepError) Unwrap() error { return e.Err }

// Transient wraps err as retryable.
func Transient(err error) error { return &StepError{Class: ClassTransient, Err: err} }

// Permanent wraps err as never retryable.
func Permanent(err error) error { return &StepError{Class: ClassPermanent, Err: err} }

// Classify returns the class of an executor error: the innermost
// StepError's class, ClassTimeout for a deadline, or ClassError.
func Classify(err error) ErrorClass {
	var se *StepError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &se) && se.Class != "":
		return se.Class
	case errors.Is(err, context.DeadlineExceeded):
		return ClassTimeout
	}
	return ClassError
}

func parseErrorClass(s string) (ErrorClass, bool) {
	switch c := ErrorClass(s); c {
	case ClassTransient, ClassTimeout, ClassError, ClassPermanent:
		return c, true
	}
	return "", false
}

func formatSeconds(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', 2, 64) + "s"
}
