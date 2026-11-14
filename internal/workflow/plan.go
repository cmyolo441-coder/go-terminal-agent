package workflow

import (
	"container/heap"
	"fmt"
	"io"
	"maps"
	"os"
	"strings"
	"time"
)

// Plan is a validated workflow with its inputs bound and matrix steps
// expanded into instances, in deterministic topological order.
type Plan struct {
	c           *compiled
	nodes       []*pnode // topological order; pnode.seq is the index
	byID        map[string]*pnode
	instances   map[string][]*pnode // step ID -> instances in matrix order
	inputs      map[string]any
	env         map[string]any
	groupLimits map[string]int
}

type pnode struct {
	seq        int
	id         string
	cs         *compiledStep
	matrix     map[string]any
	matrixIdx  int
	deps       []*pnode
	dependents []*pnode
	// rank is the longest estimated duration from the start of this node
	// to the end of the workflow; it orders the ready queue.
	rank time.Duration
}

// PlannedStep describes one node of a Plan.
type PlannedStep struct {
	// ID is the instance ID; Step is the spec step ID.
	ID        string
	Step      string
	Type      string
	DependsOn []string
	Matrix    map[string]any
	Groups    []string
	Estimate  time.Duration
	// Rank is the critical-path length from this step to the end.
	Rank time.Duration
}

// Compile validates spec, binds inputs and expands matrices. lookupEnv
// resolves ${env.NAME}; nil means os.LookupEnv. Only variables the spec
// references are read.
func Compile(spec *Spec, reg *Registry, inputs map[string]any, lookupEnv func(string) (string, bool)) (*Plan, error) {
	c, err := compileSpec(spec, reg)
	if err != nil {
		return nil, err
	}
	bound, err := BindInputs(spec, inputs)
	if err != nil {
		return nil, err
	}
	return buildPlan(c, bound, lookupEnv)
}

func snapshotEnv(names []string, lookup func(string) (string, bool)) map[string]any {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	env := make(map[string]any, len(names))
	for _, n := range names {
		if v, ok := lookup(n); ok {
			env[n] = v
		}
	}
	return env
}

func buildPlan(c *compiled, inputs map[string]any, lookupEnv func(string) (string, bool)) (*Plan, error) {
	p := &Plan{
		c:           c,
		byID:        map[string]*pnode{},
		instances:   map[string][]*pnode{},
		inputs:      inputs,
		env:         snapshotEnv(c.envNames, lookupEnv),
		groupLimits: maps.Clone(c.groups),
	}
	vars := map[string]any{"inputs": inputs, "env": p.env}
	v := &validator{}
	var all []*pnode
	for _, cs := range c.steps {
		if cs.spec.MaxParallel > 0 {
			p.groupLimits[matrixGroup(cs.spec.ID)] = cs.spec.MaxParallel
		}
		combos, err := expandMatrix(cs, vars)
		if err != nil {
			v.add(cs.path+".matrix", "%v", err)
			continue
		}
		for k, combo := range combos {
			n := &pnode{id: instanceID(cs.spec.ID, cs.axes, combo), cs: cs, matrix: combo, matrixIdx: k}
			if _, dup := p.byID[n.id]; dup {
				v.add(cs.path+".matrix", "instance id %q collides with another step", n.id)
				continue
			}
			p.byID[n.id] = n
			p.instances[cs.spec.ID] = append(p.instances[cs.spec.ID], n)
			all = append(all, n)
		}
	}
	if err := v.err(); err != nil {
		return nil, err
	}
	for _, n := range all {
		for _, d := range n.cs.spec.DependsOn {
			for _, dn := range p.instances[d] {
				n.deps = append(n.deps, dn)
				dn.dependents = append(dn.dependents, n)
			}
		}
	}
	p.nodes = topoSort(all)
	for i, n := range p.nodes {
		n.seq = i
	}
	for i := len(p.nodes) - 1; i >= 0; i-- {
		n := p.nodes[i]
		var tail time.Duration
		for _, d := range n.dependents {
			tail = max(tail, d.rank)
		}
		n.rank = n.cs.estimate + tail
	}
	return p, nil
}

// expandMatrix returns the axis-value combinations of a step, last axis
// varying fastest; a step without a matrix has one empty combination.
func expandMatrix(cs *compiledStep, vars map[string]any) ([]map[string]any, error) {
	if len(cs.axes) == 0 {
		return []map[string]any{nil}, nil
	}
	values := make([][]any, len(cs.axes))
	size := 1
	for i, axis := range cs.axes {
		vals := cs.matrixLit[axis]
		if t := cs.matrixTpl[axis]; t != nil {
			raw, err := t.Eval(vars)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", axis, err)
			}
			list, ok := raw.([]any)
			if !ok {
				return nil, fmt.Errorf("%s: expression yielded a %s, want a list", axis, typeName(raw))
			}
			if vals, err = checkMatrixValues(list); err != nil {
				return nil, fmt.Errorf("%s: %w", axis, err)
			}
		}
		values[i] = vals
		size *= len(vals)
		if size > MaxMatrixInstances {
			return nil, fmt.Errorf("expands to more than %d instances", MaxMatrixInstances)
		}
	}
	out := make([]map[string]any, 0, size)
	idx := make([]int, len(cs.axes))
	for {
		combo := make(map[string]any, len(cs.axes))
		for i, axis := range cs.axes {
			combo[axis] = values[i][idx[i]]
		}
		out = append(out, combo)
		i := len(idx) - 1
		for ; i >= 0; i-- {
			idx[i]++
			if idx[i] < len(values[i]) {
				break
			}
			idx[i] = 0
		}
		if i < 0 {
			return out, nil
		}
	}
}

func instanceID(base string, axes []string, combo map[string]any) string {
	if len(axes) == 0 {
		return base
	}
	parts := make([]string, len(axes))
	for i, a := range axes {
		parts[i] = a + "=" + stringify(combo[a])
	}
	return base + "[" + strings.Join(parts, ",") + "]"
}

// nodeHeap orders nodes by spec position, then matrix position.
type nodeHeap []*pnode

func (h nodeHeap) Len() int { return len(h) }
func (h nodeHeap) Less(i, j int) bool {
	if h[i].cs.index != h[j].cs.index {
		return h[i].cs.index < h[j].cs.index
	}
	return h[i].matrixIdx < h[j].matrixIdx
}
func (h nodeHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *nodeHeap) Push(x any)   { *h = append(*h, x.(*pnode)) }
func (h *nodeHeap) Pop() any {
	old := *h
	n := old[len(old)-1]
	old[len(old)-1] = nil
	*h = old[:len(old)-1]
	return n
}

// topoSort is Kahn's algorithm with ties broken by spec order, so the
// order is a deterministic function of the spec.
func topoSort(nodes []*pnode) []*pnode {
	indeg := make(map[*pnode]int, len(nodes))
	h := &nodeHeap{}
	for _, n := range nodes {
		indeg[n] = len(n.deps)
		if len(n.deps) == 0 {
			heap.Push(h, n)
		}
	}
	out := make([]*pnode, 0, len(nodes))
	for h.Len() > 0 {
		n := heap.Pop(h).(*pnode)
		out = append(out, n)
		for _, d := range n.dependents {
			indeg[d]--
			if indeg[d] == 0 {
				heap.Push(h, d)
			}
		}
	}
	return out
}

// Steps returns the plan's nodes in topological order.
func (p *Plan) Steps() []PlannedStep {
	out := make([]PlannedStep, len(p.nodes))
	for i, n := range p.nodes {
		deps := make([]string, len(n.deps))
		for j, d := range n.deps {
			deps[j] = d.id
		}
		out[i] = PlannedStep{
			ID: n.id, Step: n.cs.spec.ID, Type: n.cs.spec.Type, DependsOn: deps,
			Matrix: deepCopyMap(n.matrix), Groups: append([]string(nil), n.cs.groups...),
			Estimate: n.cs.estimate, Rank: n.rank,
		}
	}
	return out
}

// CriticalPath returns the chain of steps with the longest total estimate
// and that total: a lower bound on the run's duration with unlimited
// concurrency.
func (p *Plan) CriticalPath() ([]string, time.Duration) {
	var cur *pnode
	for _, n := range p.nodes {
		if len(n.deps) == 0 && (cur == nil || n.rank > cur.rank) {
			cur = n
		}
	}
	if cur == nil {
		return nil, 0
	}
	total := cur.rank
	var path []string
	for cur != nil {
		path = append(path, cur.id)
		var next *pnode
		for _, d := range cur.dependents {
			if next == nil || d.rank > next.rank || d.rank == next.rank && d.seq < next.seq {
				next = d
			}
		}
		cur = next
	}
	return path, total
}

// Levels groups the steps into waves: every step's dependencies are in
// earlier waves, so each wave could run in parallel.
func (p *Plan) Levels() [][]string {
	level := make([]int, len(p.nodes))
	var out [][]string
	for _, n := range p.nodes {
		l := 0
		for _, d := range n.deps {
			l = max(l, level[d.seq]+1)
		}
		level[n.seq] = l
		for len(out) <= l {
			out = append(out, nil)
		}
		out[l] = append(out[l], n.id)
	}
	return out
}

// Describe writes a human-readable dry-run summary of the plan.
func (p *Plan) Describe(w io.Writer) error {
	var b strings.Builder
	name := p.c.spec.Name
	if name == "" {
		name = "workflow"
	}
	fmt.Fprintf(&b, "%s: %d step(s)", name, len(p.nodes))
	if lim := p.c.spec.Concurrency; lim > 0 {
		fmt.Fprintf(&b, ", concurrency %d", lim)
	}
	if p.c.spec.ContinueOnFailure {
		b.WriteString(", continue on failure")
	} else {
		b.WriteString(", fail fast")
	}
	b.WriteString("\n")
	for i, lvl := range p.Levels() {
		fmt.Fprintf(&b, "  wave %d: %s\n", i+1, strings.Join(lvl, ", "))
	}
	path, total := p.CriticalPath()
	fmt.Fprintf(&b, "  critical path (~%s): %s\n", formatSeconds(total), strings.Join(path, " -> "))
	_, err := io.WriteString(w, b.String())
	return err
}

func deepCopyMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	return deepCopy(m).(map[string]any)
}
