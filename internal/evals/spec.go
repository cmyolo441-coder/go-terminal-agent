package evals

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Duration is a time.Duration that reads from JSON as a Go duration string
// ("90s", "2m30s") or a number of seconds, and writes as a string.
type Duration time.Duration

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if bytes.Equal(b, []byte("null")) {
		*d = 0
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if s == "" {
			*d = 0
			return nil
		}
		v, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", s, err)
		}
		if v < 0 {
			return fmt.Errorf("negative duration %q", s)
		}
		*d = Duration(v)
		return nil
	}
	f, err := strconv.ParseFloat(string(b), 64)
	if err != nil {
		return fmt.Errorf("invalid duration %s: want a string like \"90s\" or a number of seconds", b)
	}
	if f < 0 || math.IsNaN(f) || f > math.MaxInt64/float64(time.Second) {
		return fmt.Errorf("duration %s out of range", b)
	}
	*d = Duration(f * float64(time.Second))
	return nil
}

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// Std returns the value as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Limits bound one agent run. Zero means unlimited.
type Limits struct {
	MaxTurns   int      `json:"max_turns,omitempty"`
	MaxTokens  int64    `json:"max_tokens,omitempty"`
	MaxCostUSD float64  `json:"max_cost_usd,omitempty"`
	Timeout    Duration `json:"timeout,omitempty"`
}

// merge returns l with zero fields taken from def.
func (l Limits) merge(def Limits) Limits {
	if l.MaxTurns == 0 {
		l.MaxTurns = def.MaxTurns
	}
	if l.MaxTokens == 0 {
		l.MaxTokens = def.MaxTokens
	}
	if l.MaxCostUSD == 0 {
		l.MaxCostUSD = def.MaxCostUSD
	}
	if l.Timeout == 0 {
		l.Timeout = def.Timeout
	}
	return l
}

func (l Limits) validate() error {
	switch {
	case l.MaxTurns < 0:
		return errors.New("max_turns must not be negative")
	case l.MaxTokens < 0:
		return errors.New("max_tokens must not be negative")
	case l.MaxCostUSD < 0 || math.IsNaN(l.MaxCostUSD) || math.IsInf(l.MaxCostUSD, 0):
		return errors.New("max_cost_usd must be a non-negative number")
	}
	return nil
}

// Fixture is the starting workspace of a case: an optional directory
// copied first, then inline files written over it.
type Fixture struct {
	Dir   string            `json:"dir,omitempty"`
	Files map[string]string `json:"files,omitempty"`
}

// Defaults apply to every case that does not set its own value.
type Defaults struct {
	Limits        Limits   `json:"limits,omitempty"`
	Repetitions   int      `json:"repetitions,omitempty"`
	PassThreshold *float64 `json:"pass_threshold,omitempty"`
}

// Case is one evaluation scenario.
type Case struct {
	ID          string   `json:"id"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Fixture     Fixture  `json:"fixture,omitempty"`
	Prompt      string   `json:"prompt"`
	Limits      Limits   `json:"limits,omitempty"`
	// Repetitions is how many independent runs the case gets (default 1).
	Repetitions int `json:"repetitions,omitempty"`
	// PassThreshold is the weighted score a run needs to pass (default 1,
	// i.e. every grader must pass).
	PassThreshold *float64      `json:"pass_threshold,omitempty"`
	Graders       []*GraderSpec `json:"graders"`

	limits    Limits
	reps      int
	threshold float64
	fixDir    string
}

// EffectiveLimits are the case limits merged with the suite defaults.
func (c *Case) EffectiveLimits() Limits { return c.limits }

// EffectiveRepetitions is the number of runs after defaults.
func (c *Case) EffectiveRepetitions() int { return c.reps }

// EffectiveThreshold is the pass threshold after defaults.
func (c *Case) EffectiveThreshold() float64 { return c.threshold }

// Suite is a named set of cases.
type Suite struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Defaults    Defaults `json:"defaults,omitempty"`
	Cases       []*Case  `json:"cases"`

	baseDir string
}

// MaxRepetitions bounds repetitions per case.
const MaxRepetitions = 10000

var caseIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._\-/]*$`)

// LoadSuite reads and validates a suite file. Relative fixture directories
// resolve against the file's directory.
func LoadSuite(path string) (*Suite, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	s, err := ParseSuite(data, filepath.Dir(abs))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// ParseSuite parses and validates a suite. Unknown fields are rejected so
// typos in grader options do not silently weaken a test.
func ParseSuite(data []byte, baseDir string) (*Suite, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var s Suite
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("parse suite: %w", err)
	}
	if dec.More() {
		return nil, errors.New("parse suite: trailing data after the suite object")
	}
	s.baseDir = baseDir
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// Validate checks the suite, resolves defaults and compiles graders. It is
// idempotent and must be called on suites built in code before running.
func (s *Suite) Validate() error {
	var errs []error
	if strings.TrimSpace(s.Name) == "" {
		errs = append(errs, errors.New("suite name is required"))
	}
	if err := s.Defaults.Limits.validate(); err != nil {
		errs = append(errs, fmt.Errorf("defaults: %w", err))
	}
	if s.Defaults.Repetitions < 0 || s.Defaults.Repetitions > MaxRepetitions {
		errs = append(errs, fmt.Errorf("defaults: repetitions must be between 1 and %d", MaxRepetitions))
	}
	if t := s.Defaults.PassThreshold; t != nil && !validUnit(*t) {
		errs = append(errs, errors.New("defaults: pass_threshold must be between 0 and 1"))
	}
	if len(s.Cases) == 0 {
		errs = append(errs, errors.New("suite has no cases"))
	}
	seen := map[string]bool{}
	for i, c := range s.Cases {
		if c == nil {
			errs = append(errs, fmt.Errorf("case %d is null", i))
			continue
		}
		label := fmt.Sprintf("case %d", i)
		if c.ID != "" {
			label = fmt.Sprintf("case %q", c.ID)
		}
		if seen[c.ID] && c.ID != "" {
			errs = append(errs, fmt.Errorf("%s: duplicate id", label))
		}
		seen[c.ID] = true
		if err := s.validateCase(c); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", label, err))
		}
	}
	return errors.Join(errs...)
}

func validUnit(f float64) bool { return f >= 0 && f <= 1 && !math.IsNaN(f) }

func (s *Suite) validateCase(c *Case) error {
	var errs []error
	if !caseIDPattern.MatchString(c.ID) {
		errs = append(errs, errors.New("id is required and may contain only letters, digits, '.', '_', '-' and '/'"))
	}
	if strings.TrimSpace(c.Prompt) == "" {
		errs = append(errs, errors.New("prompt is required"))
	}
	if err := c.Limits.validate(); err != nil {
		errs = append(errs, err)
	}
	c.limits = c.Limits.merge(s.Defaults.Limits)

	c.reps = c.Repetitions
	if c.reps == 0 {
		c.reps = s.Defaults.Repetitions
	}
	if c.reps == 0 {
		c.reps = 1
	}
	if c.Repetitions < 0 || c.reps > MaxRepetitions {
		errs = append(errs, fmt.Errorf("repetitions must be between 1 and %d", MaxRepetitions))
	}

	c.threshold = 1
	if s.Defaults.PassThreshold != nil {
		c.threshold = *s.Defaults.PassThreshold
	}
	if c.PassThreshold != nil {
		c.threshold = *c.PassThreshold
		if !validUnit(c.threshold) {
			errs = append(errs, errors.New("pass_threshold must be between 0 and 1"))
		}
	}

	c.fixDir = ""
	if c.Fixture.Dir != "" {
		dir := c.Fixture.Dir
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(s.baseDir, dir)
		}
		if st, err := os.Stat(dir); err != nil {
			errs = append(errs, fmt.Errorf("fixture dir: %w", err))
		} else if !st.IsDir() {
			errs = append(errs, fmt.Errorf("fixture dir %s is not a directory", dir))
		}
		c.fixDir = dir
	}
	for p := range c.Fixture.Files {
		if _, err := cleanRelPath(p); err != nil {
			errs = append(errs, fmt.Errorf("fixture file: %w", err))
		}
	}

	if len(c.Graders) == 0 {
		errs = append(errs, errors.New("at least one grader is required"))
	}
	var total float64
	names := map[string]bool{}
	for i, g := range c.Graders {
		if g == nil {
			errs = append(errs, fmt.Errorf("grader %d is null", i))
			continue
		}
		if err := g.compile(); err != nil {
			errs = append(errs, fmt.Errorf("grader %d (%s): %w", i, g.Type, err))
			continue
		}
		if g.Name == "" {
			g.Name = fmt.Sprintf("%s#%d", g.Type, i+1)
		}
		if names[g.Name] {
			errs = append(errs, fmt.Errorf("grader %d: duplicate name %q", i, g.Name))
		}
		names[g.Name] = true
		total += g.weight()
	}
	if len(c.Graders) > 0 && total <= 0 && len(errs) == 0 {
		errs = append(errs, errors.New("grader weights must sum to a positive number"))
	}
	return errors.Join(errs...)
}
