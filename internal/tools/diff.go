package tools

import (
	"fmt"
	"strings"
)

// noNewline marks a last line without a trailing newline, as diff and
// patch write it.
const noNewline = `\ No newline at end of file`

// UnifiedDiff renders a compact unified diff between two texts with three
// lines of context. Very large inputs fall back to a size summary. A last
// line without a trailing newline differs from the same line with one, and
// is marked the way patch expects, so a change that only adds or removes
// the final newline still shows up.
func UnifiedDiff(name, a, b string) string {
	if a == b {
		return ""
	}
	al := splitLines(a)
	bl := splitLines(b)
	if len(al)*len(bl) > 6_000_000 {
		return fmt.Sprintf("--- %s\n+++ %s\n(%d → %d lines; too large to diff)\n", name, name, len(al), len(bl))
	}
	aNoEOL := a != "" && !strings.HasSuffix(a, "\n")
	bNoEOL := b != "" && !strings.HasSuffix(b, "\n")
	ops := lcsOps(al, bl, aNoEOL, bNoEOL)
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", name, name)
	const ctx = 3
	i := 0
	for i < len(ops) {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		start := i - ctx
		if start < 0 {
			start = 0
		}
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
			if run-end > 2*ctx || run == len(ops) {
				end += min(ctx, run-end)
				break
			}
			end = run
		}
		aStart, bStart := ops[start].ai+1, ops[start].bi+1
		var aN, bN int
		for _, o := range ops[start:end] {
			if o.kind != '+' {
				aN++
			}
			if o.kind != '-' {
				bN++
			}
		}
		// An empty range names the line before it, so a hunk that only
		// adds lines to an empty file reads "-0,0".
		if aN == 0 {
			aStart--
		}
		if bN == 0 {
			bStart--
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", aStart, aN, bStart, bN)
		for _, o := range ops[start:end] {
			out.WriteByte(o.kind)
			out.WriteString(o.text)
			out.WriteByte('\n')
			if o.noEOL {
				out.WriteString(noNewline + "\n")
			}
		}
		i = end
	}
	return out.String()
}

type op struct {
	kind   byte
	text   string
	ai, bi int
	// noEOL marks the last line of a text that does not end in a newline.
	noEOL bool
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

func lcsOps(a, b []string, aNoEOL, bNoEOL bool) []op {
	n, m := len(a), len(b)
	aLast := func(i int) bool { return aNoEOL && i == n-1 }
	bLast := func(j int) bool { return bNoEOL && j == m-1 }
	eq := func(i, j int) bool { return a[i] == b[j] && aLast(i) == bLast(j) }
	dp := make([][]int32, n+1)
	for i := range dp {
		dp[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if eq(i, j) {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	var ops []op
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && eq(i, j):
			ops = append(ops, op{' ', a[i], i, j, aLast(i)})
			i++
			j++
		case i < n && (j == m || dp[i+1][j] >= dp[i][j+1]):
			// Deletions before insertions, as diff tools show them.
			ops = append(ops, op{'-', a[i], i, j, aLast(i)})
			i++
		default:
			ops = append(ops, op{'+', b[j], i, j, bLast(j)})
			j++
		}
	}
	return ops
}
