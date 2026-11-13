package ui

import "strings"

// Layout densities for Options.Density and /density.
const (
	DensityCompact  = "compact"
	DensityExpanded = "expanded"
)

// density says how much tool output the transcript shows before
// collapsing the rest behind a "+K lines" hint.
type density struct {
	name      string
	toolHead  int // first lines of generic tool output
	bashTail  int // last lines of a command's output
	shellTail int // last lines of a ! shell command
	diffMax   int // diff rows under an edit
	approval  int // diff rows in the approval dialog; 0 fits the screen
	thinking  int // lines of a reasoning summary
}

var (
	densityCompact  = density{name: DensityCompact, toolHead: 6, bashTail: 8, shellTail: 20, diffMax: 40, approval: 18, thinking: 16}
	densityExpanded = density{name: DensityExpanded, toolHead: 60, bashTail: 60, shellTail: 200, diffMax: 400, approval: 0, thinking: 64}
)

// DensityNames lists the accepted density values.
func DensityNames() []string { return []string{DensityCompact, DensityExpanded} }

// parseDensity reads a density name; empty means compact.
func parseDensity(s string) (density, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", DensityCompact, "dense", "short":
		return densityCompact, true
	case DensityExpanded, "expand", "full", "long", "verbose":
		return densityExpanded, true
	}
	return densityCompact, false
}

// approvalRows is how many diff rows the approval dialog shows for a
// terminal height.
func (d density) approvalRows(height int) int {
	if d.approval > 0 {
		return d.approval
	}
	return max(18, height-16)
}

// collapse keeps at most n of lines: the first n, or the last n when
// fromEnd is set. It returns the kept lines and how many it hid. Hiding a
// single line would save nothing (the hint takes its place), so a list
// only one over the limit is kept whole. n <= 0 keeps everything.
func collapse(lines []string, n int, fromEnd bool) ([]string, int) {
	if n <= 0 || len(lines) <= n+1 {
		return lines, 0
	}
	hidden := len(lines) - n
	if fromEnd {
		return lines[hidden:], hidden
	}
	return lines[:n], hidden
}
