package checkpoint

import (
	"fmt"
	"strings"
)

// maxDiffCells bounds the LCS table (after trimming the common prefix and
// suffix) so a preview of a huge rewrite cannot exhaust memory.
const maxDiffCells = 4_000_000

// diffContext is the number of unchanged lines shown around a change.
const diffContext = 3

type edit struct {
	kind byte // ' ', '-' or '+'
	text string
	// ai and bi are the 0-based positions in a and b before this edit.
	ai, bi int
}

// unifiedDiff renders a unified diff from a to b, labelled with the old and
// new names. Identical inputs give "". Lines keep exact newline semantics:
// a missing final newline is shown with git's "\ No newline" marker.
func unifiedDiff(oldName, newName, a, b string) string {
	if a == b {
		return ""
	}
	al := splitKeep(a)
	bl := splitKeep(b)
	ops, ok := lineEdits(al, bl)
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", oldName, newName)
	if !ok {
		fmt.Fprintf(&out, "(%d → %d lines; too large to diff)\n", len(al), len(bl))
		return out.String()
	}
	for i := 0; i < len(ops); {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		start := max(0, i-diffContext)
		// Extend the hunk while the next change is close enough that the
		// context windows would touch.
		end := i
		for end < len(ops) {
			if ops[end].kind != ' ' {
				end++
				continue
			}
			run := end
			for run < len(ops) && ops[run].kind == ' ' {
				run++
			}
			if run == len(ops) || run-end > 2*diffContext {
				end = min(len(ops), end+diffContext)
				break
			}
			end = run
		}
		var aN, bN int
		for _, o := range ops[start:end] {
			if o.kind != '+' {
				aN++
			}
			if o.kind != '-' {
				bN++
			}
		}
		aStart, bStart := ops[start].ai+1, ops[start].bi+1
		if aN == 0 {
			aStart--
		}
		if bN == 0 {
			bStart--
		}
		fmt.Fprintf(&out, "@@ -%s +%s @@\n", hunkRange(aStart, aN), hunkRange(bStart, bN))
		for _, o := range ops[start:end] {
			out.WriteByte(o.kind)
			if text, ok := strings.CutSuffix(o.text, "\n"); ok {
				out.WriteString(text)
				out.WriteByte('\n')
			} else {
				out.WriteString(o.text)
				out.WriteString("\n\\ No newline at end of file\n")
			}
		}
		i = end
	}
	return out.String()
}

func hunkRange(start, n int) string {
	if n == 1 {
		return fmt.Sprint(start)
	}
	return fmt.Sprintf("%d,%d", start, n)
}

// splitKeep splits s into lines that keep their trailing "\n".
func splitKeep(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// lineEdits computes a minimal edit script from a to b using the longest
// common subsequence of the region between their common prefix and suffix.
// It reports false when that region is too large to diff.
func lineEdits(a, b []string) ([]edit, bool) {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	am, bm := a[pre:len(a)-suf], b[pre:len(b)-suf]
	n, m := len(am), len(bm)
	if (n+1)*(m+1) > maxDiffCells {
		return nil, false
	}
	ops := make([]edit, 0, len(a)+len(b))
	for i := 0; i < pre; i++ {
		ops = append(ops, edit{' ', a[i], i, i})
	}
	// lcs[i*(m+1)+j] is the LCS length of am[i:] and bm[j:].
	lcs := make([]int32, (n+1)*(m+1))
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if am[i] == bm[j] {
				lcs[i*(m+1)+j] = lcs[(i+1)*(m+1)+j+1] + 1
			} else {
				lcs[i*(m+1)+j] = max(lcs[(i+1)*(m+1)+j], lcs[i*(m+1)+j+1])
			}
		}
	}
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && am[i] == bm[j]:
			ops = append(ops, edit{' ', am[i], pre + i, pre + j})
			i++
			j++
		case j < m && (i == n || lcs[i*(m+1)+j+1] >= lcs[(i+1)*(m+1)+j]):
			ops = append(ops, edit{'+', bm[j], pre + i, pre + j})
			j++
		default:
			ops = append(ops, edit{'-', am[i], pre + i, pre + j})
			i++
		}
	}
	for k := 0; k < suf; k++ {
		ai, bi := len(a)-suf+k, len(b)-suf+k
		ops = append(ops, edit{' ', a[ai], ai, bi})
	}
	return reorderDeletesFirst(ops), true
}

// reorderDeletesFirst puts deletions before insertions within each run of
// changes, the conventional and most readable order.
func reorderDeletesFirst(ops []edit) []edit {
	for i := 0; i < len(ops); {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		j := i
		for j < len(ops) && ops[j].kind != ' ' {
			j++
		}
		run := make([]edit, 0, j-i)
		for _, o := range ops[i:j] {
			if o.kind == '-' {
				run = append(run, o)
			}
		}
		for _, o := range ops[i:j] {
			if o.kind == '+' {
				run = append(run, o)
			}
		}
		// Positions at the start of the run stay valid for the header.
		ai, bi := ops[i].ai, ops[i].bi
		for k := range run {
			run[k].ai, run[k].bi = ai, bi
			if run[k].kind == '-' {
				ai++
			} else {
				bi++
			}
		}
		copy(ops[i:j], run)
		i = j
	}
	return ops
}
