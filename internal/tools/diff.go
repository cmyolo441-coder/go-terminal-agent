package tools

import (
	"fmt"
	"strings"
)

// UnifiedDiff renders a compact unified diff between two texts with three
// lines of context. Very large inputs fall back to a size summary.
func UnifiedDiff(name, a, b string) string {
	if a == b {
		return ""
	}
	al := splitLines(a)
	bl := splitLines(b)
	if len(al)*len(bl) > 6_000_000 {
		return fmt.Sprintf("--- %s\n+++ %s\n(%d → %d lines; too large to diff)\n", name, name, len(al), len(bl))
	}
	ops := lcsOps(al, bl)
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
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", aStart, aN, bStart, bN)
		for _, o := range ops[start:end] {
			out.WriteByte(o.kind)
			out.WriteString(o.text)
			out.WriteByte('\n')
		}
		i = end
	}
	return out.String()
}

type op struct {
	kind   byte
	text   string
	ai, bi int
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

func lcsOps(a, b []string) []op {
	n, m := len(a), len(b)
	dp := make([][]int32, n+1)
	for i := range dp {
		dp[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
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
		case i < n && j < m && a[i] == b[j]:
			ops = append(ops, op{' ', a[i], i, j})
			i++
			j++
		case j < m && (i == n || dp[i][j+1] >= dp[i+1][j]):
			ops = append(ops, op{'+', b[j], i, j})
			j++
		default:
			ops = append(ops, op{'-', a[i], i, j})
			i++
		}
	}
	return ops
}
