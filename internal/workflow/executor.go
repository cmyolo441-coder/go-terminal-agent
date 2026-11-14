package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"sync"
	"time"
)

// Outputs are the named results of a step. Values must be representable as
// JSON; they are normalized (numbers become float64, slices []any, maps
// map[string]any) before anything downstream sees them, and they are
// written to the journal.
type Outputs = map[string]any

// StepContext is everything an executor knows about the attempt it runs.
type StepContext struct {
	RunID string
	// StepID is the instance ID: the step ID, plus the matrix values for
	// a matrix instance, as in build[os=linux].
	StepID string
	// BaseStepID is the step ID from the spec.
	BaseStepID string
	Type       string
	// Attempt is 1 for the first attempt.
	Attempt     int
	MaxAttempts int
	// IdempotencyKey is stable for this step instance across attempts and
	// resumes of the run. Executors with external side effects should
	// pass it on (or deduplicate on it) so a re-run after a crash does not
	// repeat the effect.
	IdempotencyKey string
	// Inputs are the expanded step inputs. The map is the executor's own
	// copy.
	Inputs map[string]any
	// Matrix holds this instance's matrix values; nil outside a matrix.
	Matrix map[string]any
	// Resumed is true when the run crashed or was interrupted while this
	// attempt was in flight, so it may have partly or fully happened.
	Resumed bool
	// Compensating is true when the executor runs a step's compensation;
	// Outputs then holds the outputs of the step being compensated.
	Compensating bool
	Outputs      Outputs
	// Clock is the engine's clock; use it rather than time.Sleep so tests
	// can drive time.
	Clock Clock
}

// Executor runs one kind of step.
//
// Execute must honour ctx: cancellation means the run is stopping or the
// attempt timed out. Return a *StepError (see Transient and Permanent) to
// control retries; other errors are ClassError. Panics are recovered and
// reported as permanent failures.
type Executor interface {
	Execute(ctx context.Context, sc StepContext) (Outputs, error)
}

// ExecutorFunc adapts a function to Executor.
type ExecutorFunc func(ctx context.Context, sc StepContext) (Outputs, error)

// Execute implements Executor.
func (f ExecutorFunc) Execute(ctx context.Context, sc StepContext) (Outputs, error) {
	return f(ctx, sc)
}

// InputDecl describes one input an executor accepts.
type InputDecl struct {
	Name     string
	Required bool
}

// InputDeclarer is implemented by executors with a fixed set of inputs.
// Validation then rejects unknown input names and missing required inputs
// statically, before the run starts.
type InputDeclarer interface {
	DeclaredInputs() []InputDecl
}

// Registry maps step types to executors. It is safe for concurrent use.
type Registry struct {
	mu sync.RWMutex
	m  map[string]Executor
}

var typeNameRE = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{m: map[string]Executor{}} }

// Register adds an executor. Names are lower case letters, digits, '_', '.'
// and '-', starting with a letter; registering a name twice is an error.
func (r *Registry) Register(name string, ex Executor) error {
	if !typeNameRE.MatchString(name) {
		return fmt.Errorf("invalid executor name %q", name)
	}
	if ex == nil {
		return fmt.Errorf("executor %q is nil", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.m[name]; dup {
		return fmt.Errorf("executor %q is already registered", name)
	}
	r.m[name] = ex
	return nil
}

// MustRegister is Register that panics on error, for program setup.
func (r *Registry) MustRegister(name string, ex Executor) {
	if err := r.Register(name, ex); err != nil {
		panic(err)
	}
}

// Lookup returns the executor registered under name.
func (r *Registry) Lookup(name string) (Executor, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ex, ok := r.m[name]
	return ex, ok
}

// Names returns the registered names, sorted.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return sortedKeys(r.m)
}

func idempotencyKey(runID, stepID, purpose string) string {
	h := sha256.New()
	for _, s := range []string{runID, stepID, purpose} {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// IdempotencyStore remembers the outputs of completed executions by
// idempotency key.
type IdempotencyStore interface {
	Load(ctx context.Context, key string) (Outputs, bool, error)
	Save(ctx context.Context, key string, out Outputs) error
}

// MemoryIdempotencyStore is an in-process IdempotencyStore.
type MemoryIdempotencyStore struct {
	mu sync.Mutex
	m  map[string]Outputs
}

// NewMemoryIdempotencyStore returns an empty store.
func NewMemoryIdempotencyStore() *MemoryIdempotencyStore {
	return &MemoryIdempotencyStore{m: map[string]Outputs{}}
}

// Load implements IdempotencyStore.
func (s *MemoryIdempotencyStore) Load(_ context.Context, key string) (Outputs, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out, ok := s.m[key]
	if !ok {
		return nil, false, nil
	}
	return deepCopy(out).(map[string]any), true, nil
}

// Save implements IdempotencyStore.
func (s *MemoryIdempotencyStore) Save(_ context.Context, key string, out Outputs) error {
	n, err := normalizeMap(out)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = n
	return nil
}

// Idempotent wraps an executor so that an execution whose key already has
// saved outputs returns them without running again. It closes the window
// in which a step finished but the journal record of that was lost.
// Effects of an execution that crashed before finishing are not covered:
// the executor (or the system it calls) must deduplicate on the key itself.
func Idempotent(ex Executor, store IdempotencyStore) Executor {
	return ExecutorFunc(func(ctx context.Context, sc StepContext) (Outputs, error) {
		if out, ok, err := store.Load(ctx, sc.IdempotencyKey); err != nil {
			return nil, Transient(fmt.Errorf("idempotency store: %w", err))
		} else if ok {
			return out, nil
		}
		out, err := ex.Execute(ctx, sc)
		if err != nil {
			return nil, err
		}
		norm, err := normalizeMap(out)
		if err != nil {
			return nil, Permanent(fmt.Errorf("outputs are not JSON values: %w", err))
		}
		if err := store.Save(ctx, sc.IdempotencyKey, norm); err != nil {
			return nil, Transient(fmt.Errorf("idempotency store: %w", err))
		}
		return norm, nil
	})
}

// sleepCtx waits for d on clock or until ctx is done.
func sleepCtx(ctx context.Context, clock Clock, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := clock.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func declaredNames(decls []InputDecl) []string {
	out := make([]string, len(decls))
	for i, d := range decls {
		out[i] = d.Name
	}
	slices.Sort(out)
	return out
}
