package workflow

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Problem is one validation finding. Path locates it in the spec, as in
// steps.build.inputs.cmd.
type Problem struct {
	Path string
	Msg  string
}

func (p Problem) String() string {
	if p.Path == "" {
		return p.Msg
	}
	return p.Path + ": " + p.Msg
}

// ValidationError lists every problem found in a spec.
type ValidationError struct {
	Problems []Problem
}

func (e *ValidationError) Error() string {
	if len(e.Problems) == 1 {
		return "invalid workflow: " + e.Problems[0].String()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "invalid workflow (%d problems):", len(e.Problems))
	for _, p := range e.Problems {
		b.WriteString("\n  - " + p.String())
	}
	return b.String()
}

var (
	stepIDRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,63}$`)
	nameRE   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
)

var inputTypes = map[string]bool{"": true, "any": true, "string": true, "number": true, "bool": true, "list": true, "object": true}

// compiledStep is a validated step with its templates parsed.
type compiledStep struct {
	spec       *StepSpec
	index      int
	path       string
	inputs     valueTemplate
	when       *Expr
	whenStatus bool // the condition calls a status function itself
	axes       []string
	matrixLit  map[string][]any
	matrixTpl  map[string]*Template
	retry      retryPolicy
	timeout    time.Duration
	estimate   time.Duration
	groups     []string
	outputs    map[string]bool // declared outputs; nil when undeclared
	comp       *compiledComp
	ancestors  bitset
}

type compiledComp struct {
	typ     string
	inputs  valueTemplate
	retry   retryPolicy
	timeout time.Duration
}

// compiled is a validated spec.
type compiled struct {
	spec     *Spec
	steps    []*compiledStep
	byID     map[string]*compiledStep
	outputs  map[string]*Template
	envNames []string
	groups   map[string]int
}

// Validate checks a spec without running it. reg may be nil to skip
// executor checks. The error is a *ValidationError listing every problem.
func Validate(spec *Spec, reg *Registry) error {
	_, err := compileSpec(spec, reg)
	return err
}

type validator struct {
	problems []Problem
}

func (v *validator) add(path, format string, args ...any) {
	v.problems = append(v.problems, Problem{Path: path, Msg: fmt.Sprintf(format, args...)})
}

func (v *validator) err() error {
	if len(v.problems) == 0 {
		return nil
	}
	return &ValidationError{Problems: v.problems}
}

func compileSpec(spec *Spec, reg *Registry) (*compiled, error) {
	if spec == nil {
		return nil, &ValidationError{Problems: []Problem{{Msg: "spec is nil"}}}
	}
	v := &validator{}
	c := &compiled{spec: spec, byID: map[string]*compiledStep{}, groups: map[string]int{}}

	if spec.Version != 0 && spec.Version != SpecVersion {
		v.add("version", "unsupported version %d (this engine reads version %d)", spec.Version, SpecVersion)
	}
	if spec.Concurrency < 0 {
		v.add("concurrency", "must not be negative")
	}
	if spec.Timeout < 0 {
		v.add("timeout", "must not be negative")
	}
	for _, g := range sortedKeys(spec.Groups) {
		if !stepIDRE.MatchString(g) {
			v.add("groups."+g, "invalid group name")
		}
		if spec.Groups[g] < 1 {
			v.add("groups."+g, "limit must be at least 1")
		}
		c.groups[g] = spec.Groups[g]
	}
	for _, name := range sortedKeys(spec.Inputs) {
		in := spec.Inputs[name]
		path := "inputs." + name
		if !nameRE.MatchString(name) {
			v.add(path, "invalid input name (letters, digits and underscores, not starting with a digit)")
		}
		if !inputTypes[in.Type] {
			v.add(path+".type", "unknown type %q (want string, number, bool, list, object or any)", in.Type)
			continue
		}
		if in.Default != nil {
			if in.Required {
				v.add(path, "a required input cannot have a default")
			}
			d, err := normalizeValue(in.Default)
			if err != nil {
				v.add(path+".default", "%v", err)
			} else if err := checkInputType(in.Type, d); err != nil {
				v.add(path+".default", "%v", err)
			}
		}
	}

	switch {
	case len(spec.Steps) == 0:
		v.add("steps", "workflow has no steps")
	case len(spec.Steps) > MaxSteps:
		v.add("steps", "workflow has %d steps; the limit is %d", len(spec.Steps), MaxSteps)
		return nil, v.err()
	}

	for i := range spec.Steps {
		cs := compileStep(v, c, reg, &spec.Steps[i], i)
		if cs == nil {
			continue
		}
		if _, dup := c.byID[cs.spec.ID]; dup {
			v.add(cs.path+".id", "duplicate step id %q", cs.spec.ID)
			continue
		}
		c.byID[cs.spec.ID] = cs
		c.steps = append(c.steps, cs)
	}

	depsOK := true
	for _, cs := range c.steps {
		for _, d := range cs.spec.DependsOn {
			if d == cs.spec.ID {
				depsOK = false // reported by compileStep
				continue
			}
			if _, ok := c.byID[d]; !ok {
				depsOK = false
				msg := fmt.Sprintf("depends on unknown step %q", d)
				if s := suggest(d, sortedKeys(c.byID)); s != "" {
					msg += fmt.Sprintf(" (did you mean %q?)", s)
				}
				v.add(cs.path+".depends_on", "%s", msg)
			}
		}
	}
	env := map[string]bool{}
	if depsOK {
		if cycle := findCycle(c); cycle != nil {
			v.add("steps", "dependency cycle: %s (each step depends on the next)", strings.Join(cycle, " -> "))
		} else {
			computeAncestors(c)
			for _, cs := range c.steps {
				checkStepRefs(v, c, cs, env)
			}
		}
	}
	c.outputs = map[string]*Template{}
	for _, name := range sortedKeys(spec.Outputs) {
		path := "outputs." + name
		if !nameRE.MatchString(name) {
			v.add(path, "invalid output name")
		}
		t, err := ParseTemplate(spec.Outputs[name])
		if err != nil {
			v.add(path, "%v", err)
			continue
		}
		for _, e := range t.Exprs() {
			checkRefs(v, c, e, refScope{path: path, steps: true}, env)
		}
		c.outputs[name] = t
	}
	c.envNames = sortedKeys(env)
	if err := v.err(); err != nil {
		return nil, err
	}
	return c, nil
}

func compileStep(v *validator, c *compiled, reg *Registry, s *StepSpec, i int) *compiledStep {
	path := fmt.Sprintf("steps[%d]", i)
	if stepIDRE.MatchString(s.ID) {
		path = "steps." + s.ID
	}
	cs := &compiledStep{spec: s, index: i, path: path}
	switch {
	case s.ID == "":
		v.add(path+".id", "missing step id")
	case !stepIDRE.MatchString(s.ID):
		v.add(path+".id", "invalid step id %q (a letter or underscore, then letters, digits, '_' or '-'; at most 64)", s.ID)
	}

	checkType(v, reg, path+".type", s.Type, s.Inputs, path+".inputs")

	seen := map[string]bool{}
	for _, d := range s.DependsOn {
		switch {
		case d == s.ID:
			v.add(path+".depends_on", "step depends on itself")
		case seen[d]:
			v.add(path+".depends_on", "duplicate dependency %q", d)
		}
		seen[d] = true
	}

	if s.Group != "" {
		if _, ok := c.spec.Groups[s.Group]; !ok {
			v.add(path+".group", "unknown concurrency group %q (declare it under \"groups\")", s.Group)
		}
		cs.groups = append(cs.groups, s.Group)
	}

	cs.retry = compileRetry(s.Retry, func(field, msg string) { v.add(path+".retry."+field, "%s", msg) })
	if s.Timeout < 0 {
		v.add(path+".timeout", "must not be negative")
	}
	cs.timeout = s.Timeout.D()
	if s.Estimate < 0 {
		v.add(path+".estimate", "must not be negative")
	}
	cs.estimate = s.Estimate.D()
	if cs.estimate <= 0 {
		cs.estimate = time.Second
	}

	compileMatrix(v, cs, path)
	if s.MaxParallel < 0 {
		v.add(path+".max_parallel", "must not be negative")
	}
	if s.MaxParallel > 0 {
		if len(s.Matrix) == 0 {
			v.add(path+".max_parallel", "only applies to matrix steps")
		}
		cs.groups = append(cs.groups, matrixGroup(s.ID))
	}

	if strings.TrimSpace(s.When) != "" {
		e, err := parseCondition(s.When)
		if err != nil {
			v.add(path+".when", "%v", err)
		} else {
			cs.when = e
			for _, fn := range e.Calls() {
				if builtinFuncs[fn].status {
					cs.whenStatus = true
				}
			}
		}
	}

	inputs := s.Inputs
	if inputs == nil {
		inputs = map[string]any{}
	}
	if t, err := compileValue(inputs); err != nil {
		v.add(path+".inputs", "%v", err)
	} else {
		cs.inputs = t
	}

	if s.Outputs != nil {
		cs.outputs = map[string]bool{}
		for _, o := range s.Outputs {
			if !stepIDRE.MatchString(o) {
				v.add(path+".outputs", "invalid output name %q", o)
			}
			if cs.outputs[o] {
				v.add(path+".outputs", "duplicate output %q", o)
			}
			cs.outputs[o] = true
		}
	}

	if comp := s.Compensate; comp != nil {
		cp := path + ".compensate"
		checkType(v, reg, cp+".type", comp.Type, comp.Inputs, cp+".inputs")
		cc := &compiledComp{typ: comp.Type, timeout: comp.Timeout.D()}
		cc.retry = compileRetry(comp.Retry, func(field, msg string) { v.add(cp+".retry."+field, "%s", msg) })
		if comp.Timeout < 0 {
			v.add(cp+".timeout", "must not be negative")
		}
		ci := comp.Inputs
		if ci == nil {
			ci = map[string]any{}
		}
		if t, err := compileValue(ci); err != nil {
			v.add(cp+".inputs", "%v", err)
		} else {
			cc.inputs = t
		}
		cs.comp = cc
	}
	if s.ID == "" || !stepIDRE.MatchString(s.ID) {
		return nil
	}
	return cs
}

func matrixGroup(id string) string { return "matrix:" + id }

// checkType validates a step type and, for executors that declare their
// inputs, the input names.
func checkType(v *validator, reg *Registry, path, typ string, inputs map[string]any, inputsPath string) {
	if typ == "" {
		v.add(path, "missing step type")
		return
	}
	if reg == nil {
		return
	}
	ex, ok := reg.Lookup(typ)
	if !ok {
		v.add(path, "unknown step type %q (registered: %s)", typ, strings.Join(reg.Names(), ", "))
		return
	}
	d, ok := ex.(InputDeclarer)
	if !ok {
		return
	}
	decls := d.DeclaredInputs()
	known := map[string]bool{}
	for _, in := range decls {
		known[in.Name] = true
		if _, present := inputs[in.Name]; in.Required && !present {
			v.add(inputsPath, "missing required input %q for type %q", in.Name, typ)
		}
	}
	for _, k := range sortedKeys(inputs) {
		if !known[k] {
			v.add(inputsPath+"."+k, "unknown input for type %q (accepted: %s)", typ, strings.Join(declaredNames(decls), ", "))
		}
	}
}

func compileMatrix(v *validator, cs *compiledStep, path string) {
	if len(cs.spec.Matrix) == 0 {
		return
	}
	cs.matrixLit = map[string][]any{}
	cs.matrixTpl = map[string]*Template{}
	size := 1
	for _, axis := range sortedKeys(cs.spec.Matrix) {
		ap := path + ".matrix." + axis
		if !nameRE.MatchString(axis) {
			v.add(ap, "invalid matrix axis name")
		}
		cs.axes = append(cs.axes, axis)
		switch val := cs.spec.Matrix[axis].(type) {
		case string:
			t, err := ParseTemplate(val)
			switch {
			case err != nil:
				v.add(ap, "%v", err)
			case t.IsLiteral():
				v.add(ap, "must be a list of values or a ${...} expression that yields one")
			default:
				cs.matrixTpl[axis] = t
			}
		case []any:
			vals, err := checkMatrixValues(val)
			if err != nil {
				v.add(ap, "%v", err)
				continue
			}
			cs.matrixLit[axis] = vals
			size *= len(vals)
			if size > MaxMatrixInstances {
				v.add(path+".matrix", "expands to more than %d instances", MaxMatrixInstances)
				return
			}
		default:
			v.add(ap, "must be a list of values or a ${...} expression, not a %s", typeName(val))
		}
	}
}

// checkMatrixValues validates and normalizes one axis: a non-empty list of
// distinct scalars.
func checkMatrixValues(vals []any) ([]any, error) {
	if len(vals) == 0 {
		return nil, errors.New("matrix axis has no values")
	}
	out := make([]any, 0, len(vals))
	seen := map[string]bool{}
	for _, raw := range vals {
		x, err := normalizeValue(raw)
		if err != nil {
			return nil, err
		}
		switch x.(type) {
		case string, float64, bool:
		default:
			return nil, fmt.Errorf("matrix values must be strings, numbers or booleans, not a %s", typeName(x))
		}
		key := stringify(x)
		if seen[key] {
			return nil, fmt.Errorf("duplicate matrix value %q", key)
		}
		seen[key] = true
		out = append(out, x)
	}
	return out, nil
}

// findCycle returns a dependency cycle as a list of step IDs whose first
// and last elements are equal, or nil. The search is deterministic: steps
// in spec order, dependencies in declared order.
func findCycle(c *compiled) []string {
	const (
		white = iota
		grey
		black
	)
	color := map[string]int{}
	var stack []string
	var cycle []string
	var visit func(id string) bool
	visit = func(id string) bool {
		color[id] = grey
		stack = append(stack, id)
		for _, d := range c.byID[id].spec.DependsOn {
			switch color[d] {
			case grey:
				start := slices.Index(stack, d)
				cycle = append(slices.Clone(stack[start:]), d)
				return true
			case white:
				if visit(d) {
					return true
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = black
		return false
	}
	for _, cs := range c.steps {
		if color[cs.spec.ID] == white && visit(cs.spec.ID) {
			return cycle
		}
	}
	return nil
}

type bitset []uint64

func newBitset(n int) bitset    { return make(bitset, (n+63)/64) }
func (b bitset) set(i int)      { b[i/64] |= 1 << (uint(i) % 64) }
func (b bitset) has(i int) bool { return b[i/64]&(1<<(uint(i)%64)) != 0 }
func (b bitset) union(o bitset) {
	for i := range b {
		b[i] |= o[i]
	}
}

// computeAncestors fills every step's transitive dependency set. The graph
// is known to be acyclic.
func computeAncestors(c *compiled) {
	n := len(c.spec.Steps) // indexes are spec positions; c.steps may omit invalid ones
	done := make([]bool, n)
	var visit func(cs *compiledStep)
	visit = func(cs *compiledStep) {
		if done[cs.index] {
			return
		}
		done[cs.index] = true
		cs.ancestors = newBitset(n)
		for _, d := range cs.spec.DependsOn {
			dep := c.byID[d]
			visit(dep)
			cs.ancestors.set(dep.index)
			cs.ancestors.union(dep.ancestors)
		}
	}
	for _, cs := range c.steps {
		visit(cs)
	}
}

// refScope says which roots an expression may read.
type refScope struct {
	path   string
	step   *compiledStep // the step the expression belongs to; nil for workflow outputs
	steps  bool
	matrix bool
	self   bool
	status bool
	// envOnly restricts the expression to inputs and env (matrix axes).
	envOnly bool
}

func checkStepRefs(v *validator, c *compiled, cs *compiledStep, env map[string]bool) {
	matrix := len(cs.spec.Matrix) > 0
	for _, axis := range cs.axes {
		if t := cs.matrixTpl[axis]; t != nil {
			for _, e := range t.Exprs() {
				checkRefs(v, c, e, refScope{path: cs.path + ".matrix." + axis, step: cs, envOnly: true}, env)
			}
		}
	}
	if cs.when != nil {
		checkRefs(v, c, cs.when, refScope{path: cs.path + ".when", step: cs, steps: true, matrix: matrix, status: true}, env)
	}
	if cs.inputs != nil {
		cs.inputs.exprs(cs.path+".inputs", func(p string, e *Expr) {
			checkRefs(v, c, e, refScope{path: p, step: cs, steps: true, matrix: matrix}, env)
		})
	}
	if cs.comp != nil && cs.comp.inputs != nil {
		cs.comp.inputs.exprs(cs.path+".compensate.inputs", func(p string, e *Expr) {
			checkRefs(v, c, e, refScope{path: p, step: cs, steps: true, matrix: matrix, self: true}, env)
		})
	}
}

func checkRefs(v *validator, c *compiled, e *Expr, sc refScope, env map[string]bool) {
	for _, fn := range e.Calls() {
		if builtinFuncs[fn].status && !sc.status {
			v.add(sc.path, "%s() is only allowed in `when` conditions", fn)
		}
	}
	for _, r := range e.Refs() {
		where := fmt.Sprintf("reference %q", r.String())
		if sc.envOnly && r.Root != "inputs" && r.Root != "env" {
			v.add(sc.path, "%s: matrix expressions may only read inputs and env", where)
			continue
		}
		switch r.Root {
		case "inputs":
			if len(r.Path) > 0 {
				if _, ok := c.spec.Inputs[r.Path[0]]; !ok {
					v.add(sc.path, "%s: undeclared input %q", where, r.Path[0])
				}
			}
		case "env":
			if len(r.Path) == 0 {
				v.add(sc.path, "%s: read environment variables by name, as in env.HOME", where)
				continue
			}
			env[r.Path[0]] = true
		case "run":
			if len(r.Path) > 0 && r.Path[0] != "id" && r.Path[0] != "name" {
				v.add(sc.path, "%s: run has only id and name", where)
			}
		case "matrix":
			if !sc.matrix {
				v.add(sc.path, "%s: the step has no matrix", where)
			} else if len(r.Path) > 0 && !slices.Contains(sc.step.axes, r.Path[0]) {
				v.add(sc.path, "%s: unknown matrix axis %q", where, r.Path[0])
			}
		case "self":
			if !sc.self {
				v.add(sc.path, "%s: self is only available in compensation inputs", where)
			} else if len(r.Path) > 0 {
				if r.Path[0] != "outputs" {
					v.add(sc.path, "%s: self has only outputs", where)
				} else if len(r.Path) > 1 && sc.step.outputs != nil && !sc.step.outputs[r.Path[1]] {
					v.add(sc.path, "%s: step %q does not declare output %q", where, sc.step.spec.ID, r.Path[1])
				}
			}
		case "steps":
			if !sc.steps {
				v.add(sc.path, "%s: steps are not available here", where)
				continue
			}
			checkStepRef(v, c, sc, r, where)
		default:
			msg := fmt.Sprintf("%s: unknown identifier %q (want inputs, steps, env, matrix or run)", where, r.Root)
			v.add(sc.path, "%s", msg)
		}
	}
}

func checkStepRef(v *validator, c *compiled, sc refScope, r Reference, where string) {
	if len(r.Path) == 0 {
		v.add(sc.path, "%s: name a step, as in steps.build.outputs.x", where)
		return
	}
	target, ok := c.byID[r.Path[0]]
	if !ok {
		msg := fmt.Sprintf("%s: unknown step %q", where, r.Path[0])
		if s := suggest(r.Path[0], sortedKeys(c.byID)); s != "" {
			msg += fmt.Sprintf(" (did you mean %q?)", s)
		}
		v.add(sc.path, "%s", msg)
		return
	}
	if sc.step != nil && !sc.step.ancestors.has(target.index) {
		v.add(sc.path, "%s: step %q is not an upstream dependency of %q; add it to depends_on", where, target.spec.ID, sc.step.spec.ID)
		return
	}
	if len(r.Path) < 2 {
		return
	}
	switch r.Path[1] {
	case "outputs":
		if len(r.Path) > 2 && target.outputs != nil && !target.outputs[r.Path[2]] {
			v.add(sc.path, "%s: step %q does not declare output %q", where, target.spec.ID, r.Path[2])
		}
	case "status":
	case "instances":
		if len(target.spec.Matrix) == 0 {
			v.add(sc.path, "%s: step %q is not a matrix step", where, target.spec.ID)
		}
	default:
		v.add(sc.path, "%s: a step has outputs, status and (for matrix steps) instances", where)
	}
}

// checkInputType checks a normalized value against a declared input type.
func checkInputType(typ string, v any) error {
	ok := true
	switch typ {
	case "string":
		_, ok = v.(string)
	case "number":
		_, ok = v.(float64)
	case "bool":
		_, ok = v.(bool)
	case "list":
		_, ok = v.([]any)
	case "object":
		_, ok = v.(map[string]any)
	}
	if !ok {
		return fmt.Errorf("want a %s, got a %s", typ, typeName(v))
	}
	return nil
}

// BindInputs checks provided inputs against the spec's declarations and
// applies defaults. Unknown and missing required inputs are errors.
func BindInputs(spec *Spec, provided map[string]any) (map[string]any, error) {
	v := &validator{}
	out := map[string]any{}
	for _, k := range sortedKeys(provided) {
		in, ok := spec.Inputs[k]
		if !ok {
			v.add("inputs."+k, "unknown input (declared: %s)", strings.Join(sortedKeys(spec.Inputs), ", "))
			continue
		}
		if provided[k] == nil && !in.Required {
			out[k] = nil
			continue
		}
		val, err := normalizeValue(provided[k])
		if err != nil {
			v.add("inputs."+k, "%v", err)
			continue
		}
		if err := checkInputType(in.Type, val); err != nil {
			v.add("inputs."+k, "%v", err)
			continue
		}
		out[k] = val
	}
	for _, k := range sortedKeys(spec.Inputs) {
		if _, ok := out[k]; ok {
			continue
		}
		if _, ok := provided[k]; ok {
			continue
		}
		in := spec.Inputs[k]
		switch {
		case in.Required:
			v.add("inputs."+k, "required input is missing")
		case in.Default != nil:
			d, err := normalizeValue(in.Default)
			if err != nil {
				v.add("inputs."+k, "%v", err)
				continue
			}
			out[k] = d
		default:
			out[k] = nil
		}
	}
	if err := v.err(); err != nil {
		return nil, err
	}
	return out, nil
}

// suggest returns the candidate closest to s by edit distance, if close.
func suggest(s string, candidates []string) string {
	best, bestD := "", 3
	for _, c := range candidates {
		if d := editDistance(s, c); d < bestD {
			best, bestD = c, d
		}
	}
	return best
}

func editDistance(a, b string) int {
	if len(a) > 64 || len(b) > 64 {
		return 99
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
