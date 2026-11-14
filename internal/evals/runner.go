package evals

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"sync"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/pricing"
)

// ResultSchema identifies the JSON result format.
const ResultSchema = "gta-eval-result/v1"

// WorkspacePlaceholder is the stable token cassettes use for the run's
// workspace path (see RunContext.Session).
const WorkspacePlaceholder = "$WORKSPACE"

// RunContext describes one run to a ProviderFactory.
type RunContext struct {
	Suite      string
	Case       *Case
	Repetition int
	Workspace  string
}

// Session is a convenience for cassette-backed factories: a recorder
// session that maps this run's workspace path to WorkspacePlaceholder.
func (rc RunContext) Session(r *Recorder) *Session {
	reps := []Replacement{{Actual: rc.Workspace, Placeholder: WorkspacePlaceholder}}
	if real, err := filepath.EvalSymlinks(rc.Workspace); err == nil && real != rc.Workspace {
		reps = append(reps, Replacement{Actual: real, Placeholder: WorkspacePlaceholder})
	}
	return r.Session(reps...)
}

// ProviderFactory returns the provider for one run. Returning a fresh
// value per run (for example a cassette Session) keeps runs independent.
type ProviderFactory func(rc RunContext) (llm.Provider, error)

// StaticProvider is a ProviderFactory that always returns p.
func StaticProvider(p llm.Provider) ProviderFactory {
	return func(RunContext) (llm.Provider, error) { return p, nil }
}

// CostFunc prices token usage on a model; ok is false for unknown models.
type CostFunc func(model string, u llm.Usage) (usd float64, ok bool)

// Runner executes suites.
type Runner struct {
	// Agent runs the agent under test. Required.
	Agent AgentFunc
	// Provider supplies each run's model provider. Required.
	Provider ProviderFactory
	// Commands runs command_succeeds graders; nil makes them error.
	Commands CommandRunner
	// Judge runs llm_judge graders; nil makes them error.
	Judge JudgeFunc
	// Concurrency is the number of runs in flight (default GOMAXPROCS).
	Concurrency int
	// TempDir is where workspaces are created (default os.TempDir()).
	TempDir string
	// KeepWorkspaces leaves every workspace on disk; KeepFailed only
	// those of failed runs. Kept paths are reported in RunResult.
	KeepWorkspaces bool
	KeepFailed     bool
	// Cost prices usage for max_cost_usd and reports (default
	// pricing.Cost).
	Cost CostFunc
	// Filter selects cases; nil runs all.
	Filter func(*Case) bool
	// OnRun is called (serially) after each run completes, for progress.
	OnRun func(RunResult)
	// Seed makes bootstrap intervals reproducible.
	Seed uint64
	// BootstrapSamples per interval (default DefaultBootstrapSamples).
	BootstrapSamples int
	// Meta is copied into the result (model, commit, etc.).
	Meta map[string]string
	// Now is the clock (default time.Now).
	Now func() time.Time
}

// LimitError is returned by the metering provider once a run exceeds a
// limit, so the agent stops making calls.
type LimitError struct {
	Limit string
	Used  float64
	Max   float64
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("evals: %s limit reached (%g of %g)", e.Limit, e.Used, e.Max)
}

// ErrLimitExceeded matches every LimitError with errors.Is.
var ErrLimitExceeded = errors.New("evals: run limit exceeded")

// Is makes errors.Is(err, ErrLimitExceeded) true.
func (e *LimitError) Is(target error) bool { return target == ErrLimitExceeded }

// meter enforces turn, token and cost limits around a provider and
// independently records what the run consumed.
type meter struct {
	inner  llm.Provider
	limits Limits
	cost   CostFunc

	mu         sync.Mutex
	turns      int
	usage      llm.Usage
	costUSD    float64
	model      string
	violations []string
}

func (m *meter) Name() string { return m.inner.Name() }

func totalTokens(u llm.Usage) int64 {
	return u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

// exceeded reports the first limit already used up. Called with mu held.
func (m *meter) exceeded() *LimitError {
	l := m.limits
	switch {
	case l.MaxTurns > 0 && m.turns >= l.MaxTurns:
		return &LimitError{Limit: "max_turns", Used: float64(m.turns), Max: float64(l.MaxTurns)}
	case l.MaxTokens > 0 && totalTokens(m.usage) >= l.MaxTokens:
		return &LimitError{Limit: "max_tokens", Used: float64(totalTokens(m.usage)), Max: float64(l.MaxTokens)}
	case l.MaxCostUSD > 0 && m.costUSD >= l.MaxCostUSD:
		return &LimitError{Limit: "max_cost_usd", Used: m.costUSD, Max: l.MaxCostUSD}
	}
	return nil
}

func (m *meter) violate(e *LimitError) {
	for _, v := range m.violations {
		if v == e.Limit {
			return
		}
	}
	m.violations = append(m.violations, e.Limit)
}

func (m *meter) Stream(ctx context.Context, req llm.Request, onEvent func(llm.Event)) (*llm.Response, error) {
	m.mu.Lock()
	if e := m.exceeded(); e != nil {
		m.violate(e)
		m.mu.Unlock()
		return nil, e
	}
	m.mu.Unlock()
	resp, err := m.inner.Stream(ctx, req, onEvent)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.turns++
	if resp != nil {
		m.usage.Add(resp.Usage)
		model := resp.Model
		if model == "" {
			model = req.Model
		}
		if model != "" {
			m.model = model
		}
		if m.cost != nil {
			if c, ok := m.cost(model, resp.Usage); ok {
				m.costUSD += c
			}
		}
	}
	// Overshooting tokens or cost on this call is a violation even though
	// the response is still delivered.
	l := m.limits
	if l.MaxTokens > 0 && totalTokens(m.usage) > l.MaxTokens {
		m.violate(&LimitError{Limit: "max_tokens"})
	}
	if l.MaxCostUSD > 0 && m.costUSD > l.MaxCostUSD {
		m.violate(&LimitError{Limit: "max_cost_usd"})
	}
	return resp, err
}

// RunResult is the outcome of one repetition of one case.
type RunResult struct {
	CaseID     string        `json:"case_id"`
	Repetition int           `json:"repetition"`
	Passed     bool          `json:"passed"`
	Score      float64       `json:"score"`
	Grades     []GradeResult `json:"grades"`
	// Error is the agent's error, if it returned one or panicked.
	Error string `json:"error,omitempty"`
	// Violations lists exceeded limits (max_turns, max_tokens,
	// max_cost_usd, timeout).
	Violations      []string  `json:"violations,omitempty"`
	DurationSeconds float64   `json:"duration_seconds"`
	Turns           int       `json:"turns"`
	ToolCalls       int       `json:"tool_calls"`
	Usage           llm.Usage `json:"usage"`
	CostUSD         float64   `json:"cost_usd"`
	Model           string    `json:"model,omitempty"`
	Output          string    `json:"output,omitempty"`
	Workspace       string    `json:"workspace,omitempty"`
}

// FailureReason summarises why a run failed ("" when it passed).
func (r RunResult) FailureReason() string {
	if r.Passed {
		return ""
	}
	if r.Error != "" {
		return "agent error: " + r.Error
	}
	if len(r.Violations) > 0 {
		return fmt.Sprintf("limit exceeded: %v", r.Violations)
	}
	for _, g := range r.Grades {
		if g.Required && !g.Passed {
			return fmt.Sprintf("required grader %s failed: %s", g.Name, g.Detail)
		}
	}
	for _, g := range r.Grades {
		if !g.Passed && g.Weight > 0 {
			return fmt.Sprintf("score %.3f below threshold; %s failed: %s", r.Score, g.Name, g.Detail)
		}
	}
	return fmt.Sprintf("score %.3f below threshold", r.Score)
}

// KMetric holds pass@k and pass^k for one k.
type KMetric struct {
	K       int     `json:"k"`
	PassAt  float64 `json:"pass_at_k"`
	PassHat float64 `json:"pass_hat_k"`
}

// CaseResult aggregates the runs of one case.
type CaseResult struct {
	ID              string      `json:"id"`
	Description     string      `json:"description,omitempty"`
	Tags            []string    `json:"tags,omitempty"`
	Threshold       float64     `json:"threshold"`
	Runs            []RunResult `json:"runs"`
	N               int         `json:"n"`
	Passes          int         `json:"passes"`
	PassRate        float64     `json:"pass_rate"`
	K               []KMetric   `json:"k"`
	MeanScore       float64     `json:"mean_score"`
	StdDev          float64     `json:"stddev"`
	CI95            Interval    `json:"ci95"`
	MeanDurationSec float64     `json:"mean_duration_seconds"`
	CostUSD         float64     `json:"cost_usd"`
	Usage           llm.Usage   `json:"usage"`
}

// PassAt returns pass@k (NaN when k is out of range).
func (c CaseResult) PassAt(k int) float64 { return PassAtK(c.N, c.Passes, k) }

// PassHat returns pass^k (NaN when k is out of range).
func (c CaseResult) PassHat(k int) float64 { return PassHatK(c.N, c.Passes, k) }

// Summary aggregates a whole suite.
type Summary struct {
	Cases      int     `json:"cases"`
	Runs       int     `json:"runs"`
	PassedRuns int     `json:"passed_runs"`
	PassRate   float64 `json:"pass_rate"`
	// CasesAllPassed counts cases whose every run passed; CasesAnyPassed
	// those with at least one passing run.
	CasesAllPassed int `json:"cases_all_passed"`
	CasesAnyPassed int `json:"cases_any_passed"`
	// MeanScore and StdDev are over per-case mean scores (each case counts
	// once, whatever its repetitions); CI95 is the matching cluster
	// bootstrap.
	MeanScore  float64   `json:"mean_score"`
	StdDev     float64   `json:"stddev"`
	CI95       Interval  `json:"ci95"`
	AgentError int       `json:"agent_errors"`
	Violations int       `json:"limit_violations"`
	CostUSD    float64   `json:"cost_usd"`
	Usage      llm.Usage `json:"usage"`
}

// SuiteResult is the full outcome of a suite run.
type SuiteResult struct {
	Schema          string            `json:"schema"`
	Suite           string            `json:"suite"`
	Description     string            `json:"description,omitempty"`
	StartedAt       time.Time         `json:"started_at"`
	DurationSeconds float64           `json:"duration_seconds"`
	Meta            map[string]string `json:"meta,omitempty"`
	Cases           []CaseResult      `json:"cases"`
	Summary         Summary           `json:"summary"`
}

// maxStoredOutput caps the final answer kept in results.
const maxStoredOutput = 4000

type job struct {
	c   *Case
	rep int
}

// Run executes every selected case of the suite and aggregates results.
// Cancelling ctx stops new runs from starting and interrupts running ones;
// the partial result is returned with ctx's error.
func (r *Runner) Run(ctx context.Context, s *Suite) (*SuiteResult, error) {
	if r.Agent == nil {
		return nil, errors.New("evals: Runner.Agent is required")
	}
	if r.Provider == nil {
		return nil, errors.New("evals: Runner.Provider is required")
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	now := r.Now
	if now == nil {
		now = time.Now
	}
	cost := r.Cost
	if cost == nil {
		cost = pricing.Cost
	}

	var cases []*Case
	for _, c := range s.Cases {
		if r.Filter == nil || r.Filter(c) {
			cases = append(cases, c)
		}
	}
	if len(cases) == 0 {
		return nil, errors.New("evals: no cases selected")
	}
	var jobs []job
	for _, c := range cases {
		for rep := 0; rep < c.reps; rep++ {
			jobs = append(jobs, job{c: c, rep: rep})
		}
	}

	workers := r.Concurrency
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	workers = min(workers, len(jobs))

	start := now()
	results := make([]RunResult, len(jobs))
	done := make([]bool, len(jobs))
	next := make(chan int)
	var (
		wg     sync.WaitGroup
		progMu sync.Mutex
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				res := r.runOne(ctx, s, jobs[i].c, jobs[i].rep, cost, now)
				results[i] = res
				done[i] = true
				if r.OnRun != nil {
					progMu.Lock()
					r.OnRun(res)
					progMu.Unlock()
				}
			}
		}()
	}
feed:
	for i := range jobs {
		select {
		case next <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(next)
	wg.Wait()

	out := &SuiteResult{
		Schema:          ResultSchema,
		Suite:           s.Name,
		Description:     s.Description,
		StartedAt:       start.UTC(),
		DurationSeconds: now().Sub(start).Seconds(),
		Meta:            r.Meta,
	}
	byCase := map[string][]RunResult{}
	for i, res := range results {
		if done[i] {
			byCase[res.CaseID] = append(byCase[res.CaseID], res)
		}
	}
	for _, c := range cases {
		runs := byCase[c.ID]
		if len(runs) == 0 {
			continue
		}
		out.Cases = append(out.Cases, aggregateCase(c, runs, r.Seed, r.BootstrapSamples))
	}
	out.Summary = summarizeSuite(out.Cases, r.Seed, r.BootstrapSamples)
	return out, ctx.Err()
}

// runOne executes a single repetition. It never panics.
func (r *Runner) runOne(ctx context.Context, s *Suite, c *Case, rep int, cost CostFunc, now func() time.Time) (res RunResult) {
	res = RunResult{CaseID: c.ID, Repetition: rep}
	start := now()
	defer func() {
		res.DurationSeconds = now().Sub(start).Seconds()
	}()

	ws, fixture, err := r.materialize(c, rep)
	if err != nil {
		res.Error = "workspace: " + err.Error()
		return res
	}
	keep := r.KeepWorkspaces
	defer func() {
		if keep || (r.KeepFailed && !res.Passed) {
			res.Workspace = ws
			return
		}
		_ = os.RemoveAll(ws)
	}()

	rc := RunContext{Suite: s.Name, Case: c, Repetition: rep, Workspace: ws}
	prov, err := r.Provider(rc)
	if err != nil {
		res.Error = "provider: " + err.Error()
		return res
	}
	if prov == nil {
		res.Error = "provider: factory returned nil"
		return res
	}
	m := &meter{inner: prov, limits: c.limits, cost: cost}

	runCtx := ctx
	cancel := context.CancelFunc(func() {})
	if t := c.limits.Timeout.Std(); t > 0 {
		runCtx, cancel = context.WithTimeout(ctx, t)
	}
	tr, agentErr := r.callAgent(runCtx, ws, c.Prompt, m)
	timedOut := runCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil
	cancel()

	m.mu.Lock()
	violations := append([]string(nil), m.violations...)
	metered := struct {
		turns int
		usage llm.Usage
		cost  float64
		model string
	}{m.turns, m.usage, m.costUSD, m.model}
	m.mu.Unlock()
	if timedOut {
		violations = append(violations, "timeout")
	}

	tr.Normalize()
	// The meter is authoritative: it saw every model call.
	if metered.turns > 0 {
		tr.Turns = metered.turns
		tr.Usage = metered.usage
		if metered.model != "" {
			tr.Model = metered.model
		}
		tr.CostUSD = metered.cost
	} else if tr.CostUSD == 0 && tr.Model != "" {
		if usd, ok := cost(tr.Model, tr.Usage); ok {
			tr.CostUSD = usd
		}
	}
	res.Turns = tr.Turns
	res.ToolCalls = len(tr.ToolCalls)
	res.Usage = tr.Usage
	res.CostUSD = tr.CostUSD
	res.Model = tr.Model
	res.Output = truncateRunes(tr.Output, maxStoredOutput)
	res.Violations = violations
	if agentErr != nil && !(errors.Is(agentErr, ErrLimitExceeded) || (timedOut && errors.Is(agentErr, context.DeadlineExceeded))) {
		res.Error = agentErr.Error()
	}

	gc := &GradeContext{
		Workspace:  ws,
		Fixture:    fixture,
		Prompt:     c.Prompt,
		Transcript: &tr,
		Commands:   r.Commands,
		Judge:      r.Judge,
	}
	res.Grades = make([]GradeResult, len(c.Graders))
	for i, g := range c.Graders {
		res.Grades[i] = safeGrade(ctx, g, gc)
	}
	res.Score = weightedScore(res.Grades)
	res.Passed = res.Error == "" && len(res.Violations) == 0 && res.Score >= c.threshold-1e-9
	for _, g := range res.Grades {
		if g.Required && !g.Passed {
			res.Passed = false
		}
	}
	return res
}

func (r *Runner) callAgent(ctx context.Context, ws, prompt string, p llm.Provider) (tr Transcript, err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("agent panicked: %v\n%s", v, debug.Stack())
		}
	}()
	return r.Agent(ctx, ws, prompt, p)
}

func safeGrade(ctx context.Context, g *GraderSpec, gc *GradeContext) (res GradeResult) {
	defer func() {
		if v := recover(); v != nil {
			res = GradeResult{Name: g.Name, Type: g.Type, Weight: g.weight(), Required: g.Required,
				Error: fmt.Sprintf("grader panicked: %v", v), Detail: fmt.Sprintf("grader panicked: %v", v)}
		}
	}()
	return g.Grade(ctx, gc)
}

// weightedScore is the weight-averaged grade score; graders with zero
// weight are advisory and do not count.
func weightedScore(gs []GradeResult) float64 {
	var num, den float64
	for _, g := range gs {
		if g.Weight <= 0 {
			continue
		}
		num += g.Weight * g.Score
		den += g.Weight
	}
	if den == 0 {
		return 0
	}
	return num / den
}

// materialize creates an isolated workspace for one run and returns the
// fixture snapshot used by file_unchanged.
func (r *Runner) materialize(c *Case, rep int) (string, map[string]string, error) {
	base := r.TempDir
	if base == "" {
		base = os.TempDir()
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", nil, err
	}
	ws, err := os.MkdirTemp(base, fmt.Sprintf("gta-eval-%s-%d-", sanitizeForPath(c.ID), rep))
	if err != nil {
		return "", nil, err
	}
	fail := func(err error) (string, map[string]string, error) {
		_ = os.RemoveAll(ws)
		return "", nil, err
	}
	if c.fixDir != "" {
		if err := copyTree(c.fixDir, ws); err != nil {
			return fail(err)
		}
	}
	for _, p := range sortedKeys(c.Fixture.Files) {
		rel, err := cleanRelPath(p)
		if err != nil {
			return fail(err)
		}
		full, err := resolveInside(ws, rel)
		if err != nil {
			return fail(err)
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fail(err)
		}
		if err := os.WriteFile(full, []byte(c.Fixture.Files[p]), 0o644); err != nil {
			return fail(err)
		}
	}
	snap, err := snapshotTree(ws)
	if err != nil {
		return fail(err)
	}
	return ws, snap, nil
}

func sanitizeForPath(s string) string {
	b := []byte(s)
	for i, ch := range b {
		ok := ch == '-' || ch == '_' || ch == '.' || (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z')
		if !ok {
			b[i] = '_'
		}
	}
	if len(b) > 40 {
		b = b[:40]
	}
	return string(b)
}

func aggregateCase(c *Case, runs []RunResult, seed uint64, samples int) CaseResult {
	sort.Slice(runs, func(i, j int) bool { return runs[i].Repetition < runs[j].Repetition })
	cr := CaseResult{
		ID:          c.ID,
		Description: c.Description,
		Tags:        c.Tags,
		Threshold:   c.threshold,
		Runs:        runs,
		N:           len(runs),
	}
	scores := make([]float64, len(runs))
	var dur float64
	for i, r := range runs {
		if r.Passed {
			cr.Passes++
		}
		scores[i] = r.Score
		dur += r.DurationSeconds
		cr.CostUSD += r.CostUSD
		cr.Usage.Add(r.Usage)
	}
	cr.PassRate = float64(cr.Passes) / float64(cr.N)
	for k := 1; k <= cr.N; k++ {
		cr.K = append(cr.K, KMetric{K: k, PassAt: PassAtK(cr.N, cr.Passes, k), PassHat: PassHatK(cr.N, cr.Passes, k)})
	}
	cr.MeanScore = Mean(scores)
	cr.StdDev = StdDev(scores)
	cr.CI95 = BootstrapMeanCI(scores, 0.95, samples, seedFor(seed, c.ID))
	cr.MeanDurationSec = dur / float64(cr.N)
	return cr
}

func summarizeSuite(cases []CaseResult, seed uint64, samples int) Summary {
	var s Summary
	s.Cases = len(cases)
	var caseMeans []float64
	for _, c := range cases {
		caseMeans = append(caseMeans, c.MeanScore)
		if c.Passes == c.N {
			s.CasesAllPassed++
		}
		if c.Passes > 0 {
			s.CasesAnyPassed++
		}
		s.CostUSD += c.CostUSD
		s.Usage.Add(c.Usage)
		for _, r := range c.Runs {
			s.Runs++
			if r.Passed {
				s.PassedRuns++
			}
			if r.Error != "" {
				s.AgentError++
			}
			if len(r.Violations) > 0 {
				s.Violations++
			}
		}
	}
	if s.Runs > 0 {
		s.PassRate = float64(s.PassedRuns) / float64(s.Runs)
	}
	s.MeanScore = Mean(caseMeans)
	s.StdDev = StdDev(caseMeans)
	s.CI95 = BootstrapMeanCI(caseMeans, 0.95, samples, seedFor(seed, "\x00suite"))
	return s
}
