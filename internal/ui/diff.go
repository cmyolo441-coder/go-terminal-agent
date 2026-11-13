package ui

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// Row kinds of a parsed diff.
const (
	rowAdd     = '+'
	rowDel     = '-'
	rowCtx     = ' '
	rowHunk    = '@'
	rowNoEOL   = '\\'
	rowFile    = 'F'
	rowComment = '?'
)

// diffRow is one displayable line of a unified diff.
type diffRow struct {
	kind byte
	text string // content without the +/-/space marker
	num  int    // gutter line number: old for deletions, new otherwise; 0 = none
	// Hunk header fields.
	oldStart, oldN, newStart, newN int
	section                        string
	// segs splits text into unchanged and changed runs when the line is
	// paired with a similar line on the other side; nil means the line
	// has no intra-line highlighting.
	segs []seg
}

// seg is a run of text inside a changed line; hi marks changed words.
type seg struct {
	text string
	hi   bool
}

var hunkRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@ ?(.*)$`)

// parseDiff turns a unified diff (ours or git's) into rows, numbering lines
// from the hunk headers. The header counts say where a hunk ends, so a
// deleted line "-- x" (shown as "--- x") is content, not a file header.
func parseDiff(diff string) []diffRow {
	var rows []diffRow
	var oldN, newN, oldLeft, newLeft int
	sawHunk := false
	for _, l := range strings.Split(strings.TrimRight(diff, "\n"), "\n") {
		l = strings.TrimSuffix(l, "\r")
		if m := hunkRe.FindStringSubmatch(l); m != nil {
			r := diffRow{kind: rowHunk, section: strings.TrimSpace(m[5])}
			r.oldStart, r.oldN = atoi(m[1]), countOr1(m[2])
			r.newStart, r.newN = atoi(m[3]), countOr1(m[4])
			rows = append(rows, r)
			oldN, newN = r.oldStart, r.newStart
			if r.oldN == 0 {
				oldN++ // "-5,0" names the line before an insertion
			}
			if r.newN == 0 {
				newN++
			}
			oldLeft, newLeft = r.oldN, r.newN
			sawHunk = true
			continue
		}
		inHunk := sawHunk && (oldLeft > 0 || newLeft > 0)
		if strings.HasPrefix(l, "\\") {
			rows = append(rows, diffRow{kind: rowNoEOL, text: l})
			continue
		}
		if !inHunk {
			switch {
			case strings.HasPrefix(l, "+++ "):
				name := strings.TrimSpace(strings.TrimPrefix(l, "+++ "))
				if i := strings.IndexByte(name, '\t'); i >= 0 {
					name = name[:i]
				}
				rows = append(rows, diffRow{kind: rowFile, text: strings.TrimPrefix(name, "b/")})
				continue
			case l == "", strings.HasPrefix(l, "--- "), strings.HasPrefix(l, "diff --git "),
				strings.HasPrefix(l, "index "), strings.HasPrefix(l, "new file mode"),
				strings.HasPrefix(l, "deleted file mode"), strings.HasPrefix(l, "old mode"),
				strings.HasPrefix(l, "new mode"), strings.HasPrefix(l, "similarity index"):
				continue
			case sawHunk:
				// Between hunks anything else is commentary.
				rows = append(rows, diffRow{kind: rowComment, text: l})
				continue
			}
		}
		if l == "" {
			l = " " // some tools strip the marker of empty context lines
		}
		switch l[0] {
		case '+':
			rows = append(rows, diffRow{kind: rowAdd, text: l[1:], num: newN})
			newN++
			newLeft--
		case '-':
			rows = append(rows, diffRow{kind: rowDel, text: l[1:], num: oldN})
			oldN++
			oldLeft--
		case ' ':
			rows = append(rows, diffRow{kind: rowCtx, text: l[1:], num: newN})
			oldN++
			newN++
			oldLeft--
			newLeft--
		default:
			rows = append(rows, diffRow{kind: rowComment, text: l})
		}
	}
	if !sawHunk {
		for i := range rows {
			rows[i].num = 0 // no header, no trustworthy numbers
		}
	}
	// A single file needs no title: the tool header already names it.
	if countKind(rows, rowFile) <= 1 {
		rows = dropKind(rows, rowFile)
	}
	highlightPairs(rows)
	return rows
}

func countKind(rows []diffRow, k byte) int {
	n := 0
	for _, r := range rows {
		if r.kind == k {
			n++
		}
	}
	return n
}

func dropKind(rows []diffRow, k byte) []diffRow {
	kept := rows[:0]
	for _, r := range rows {
		if r.kind != k {
			kept = append(kept, r)
		}
	}
	return kept
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func countOr1(s string) int {
	if s == "" {
		return 1
	}
	return atoi(s)
}

// diffStat counts added and removed lines.
func diffStat(diff string) (add, del int) {
	for _, r := range parseDiff(diff) {
		switch r.kind {
		case rowAdd:
			add++
		case rowDel:
			del++
		}
	}
	return
}

// ---------- intra-line highlighting ----------

// highlightPairs pairs each run of deletions with the run of additions
// right after it, line by line, and marks the words that changed.
func highlightPairs(rows []diffRow) {
	for i := 0; i < len(rows); {
		if rows[i].kind != rowDel {
			i++
			continue
		}
		d0 := i
		for i < len(rows) && rows[i].kind == rowDel {
			i++
		}
		a0 := i
		for i < len(rows) && rows[i].kind == rowAdd {
			i++
		}
		for k := 0; k < a0-d0 && k < i-a0; k++ {
			ds, as := wordDiff(sanitize(rows[d0+k].text), sanitize(rows[a0+k].text))
			if ds != nil {
				rows[d0+k].segs, rows[a0+k].segs = ds, as
			}
		}
	}
}

const maxDiffTokens = 400

// wordDiff compares two lines word by word and splits each into changed and
// unchanged runs. It returns nil when the lines share too little for the
// highlight to help, or are too long to compare cheaply.
func wordDiff(a, b string) ([]seg, []seg) {
	if a == b {
		return nil, nil
	}
	at, bt := tokenize(a), tokenize(b)
	n, m := len(at), len(bt)
	if n == 0 || m == 0 || n > maxDiffTokens || m > maxDiffTokens {
		return nil, nil
	}
	dp := make([][]int32, n+1)
	for i := range dp {
		dp[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case at[i] == bt[j]:
				dp[i][j] = dp[i+1][j+1] + 1
			case dp[i+1][j] >= dp[i][j+1]:
				dp[i][j] = dp[i+1][j]
			default:
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	aSame, bSame := make([]bool, n), make([]bool, m)
	common := 0
	for i, j := 0, 0; i < n && j < m; {
		switch {
		case at[i] == bt[j]:
			aSame[i], bSame[j] = true, true
			common += visibleLen(at[i])
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			i++
		default:
			j++
		}
	}
	// Highlight only when the shorter line mostly survives and the shared
	// text is a fair part of the longer one; when nearly everything
	// changed, plain line coloring says it better.
	la, lb := visibleLen(a), visibleLen(b)
	shortest, longest := min(la, lb), max(la, lb)
	if shortest == 0 || float64(common)/float64(shortest) < 0.5 || float64(common)/float64(longest) < 0.2 {
		return nil, nil
	}
	return segments(at, aSame), segments(bt, bSame)
}

func visibleLen(s string) int {
	n := 0
	for _, r := range s {
		if !unicode.IsSpace(r) {
			n++
		}
	}
	return n
}

// segments joins tokens into runs. A lone unchanged space between two
// changed tokens joins the change, so a changed phrase reads as one block.
func segments(toks []string, same []bool) []seg {
	hi := make([]bool, len(toks))
	for i := range toks {
		hi[i] = !same[i]
	}
	for i := 1; i+1 < len(toks); i++ {
		if !hi[i] && hi[i-1] && hi[i+1] && strings.TrimSpace(toks[i]) == "" {
			hi[i] = true
		}
	}
	var out []seg
	for i, t := range toks {
		if len(out) > 0 && out[len(out)-1].hi == hi[i] {
			out[len(out)-1].text += t
			continue
		}
		out = append(out, seg{text: t, hi: hi[i]})
	}
	return out
}

// tokenize splits a line into words (letters, digits, underscores), runs of
// spaces, and single other characters.
func tokenize(s string) []string {
	class := func(r rune) int {
		switch {
		case r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
			return 1
		case unicode.IsSpace(r):
			return 2
		default:
			return 3
		}
	}
	var out []string
	rs := []rune(s)
	for i := 0; i < len(rs); {
		c := class(rs[i])
		j := i + 1
		if c != 3 {
			for j < len(rs) && class(rs[j]) == c {
				j++
			}
		}
		out = append(out, string(rs[i:j]))
		i = j
	}
	return out
}

// ---------- rendering ----------

// renderDiff renders a unified diff for a box width cells wide. Added and
// removed rows fill exactly width cells (so their backgrounds line up) and
// no row is wider, so nothing wraps. Rows carry line numbers and
// word-level highlights; after max rows a "+K diff lines" hint follows.
func renderDiff(diff string, width, max int) []string {
	if width < 12 {
		width = 12
	}
	rows := parseDiff(diff)
	// With one hunk the line numbers already say where it is.
	if countKind(rows, rowHunk) == 1 && rows[0].kind == rowHunk {
		rows = rows[1:]
	}
	maxNum := 0
	for _, r := range rows {
		maxNum = maxInt(maxNum, r.num)
	}
	numW := 0
	if maxNum > 0 {
		numW = maxInt(2, len(strconv.Itoa(maxNum)))
	}
	if width-(numW+3) < 16 {
		numW = 0 // narrow: give the content the room
	}
	var out []string
	for i, r := range rows {
		if max > 0 && i == max && len(rows)-max > 1 {
			out = append(out, sFaint.Render(fmt.Sprintf("… +%d diff lines", len(rows)-max)))
			break
		}
		out = append(out, renderDiffRow(r, width, numW))
	}
	return out
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// gutterWidth is the width of the line number column plus the marker.
func gutterWidth(numW int) int {
	if numW == 0 {
		return 2
	}
	return numW + 3
}

func renderDiffRow(r diffRow, width, numW int) string {
	switch r.kind {
	case rowHunk:
		return renderHunkHeader(r, width)
	case rowFile:
		name := clip(sanitize(r.text), width-2)
		rule := strings.Repeat("─", maxInt(0, width-runewidth.StringWidth(name)-1))
		return sBold.Render(name) + " " + sFaint.Render(rule)
	case rowNoEOL:
		note := strings.TrimSpace(strings.TrimPrefix(r.text, "\\"))
		return sFaint.Render(clip(strings.Repeat(" ", gutterWidth(numW))+"⏎ "+sanitize(note), width))
	case rowComment:
		return sMuted.Render(clip(sanitize(r.text), width))
	}
	gutter := ""
	if numW > 0 {
		n := ""
		if r.num > 0 {
			n = strconv.Itoa(r.num)
		}
		gutter = strings.Repeat(" ", maxInt(0, numW-len(n))) + n + " "
	}
	sign, base, hi, num := " ", sMuted, sMuted, sFaint
	switch r.kind {
	case rowAdd:
		sign, base, hi, num = "+", sDiffAdd, sDiffAddHi, sDiffAddNum
	case rowDel:
		sign, base, hi, num = "-", sDiffDel, sDiffDelHi, sDiffDelNum
	}
	cw := width - gutterWidth(numW)
	segs := r.segs
	if segs == nil {
		segs = []seg{{text: sanitize(r.text)}}
	}
	body, used := fitSegments(segs, cw, base, hi)
	line := num.Render(gutter) + base.Render(sign+" ") + body
	if r.kind != rowCtx && used < cw {
		line += base.Render(strings.Repeat(" ", cw-used))
	}
	return line
}

// renderHunkHeader draws "@@ -12,5 +12,7 @@ section ─────" across width.
func renderHunkHeader(r diffRow, width int) string {
	rng := fmt.Sprintf("-%d,%d +%d,%d", r.oldStart, r.oldN, r.newStart, r.newN)
	used := len(rng) + 6
	if used > width {
		return sDiffHunk.Render(clip("@@ "+rng+" @@", width))
	}
	head := sFaint.Render("@@ ") + sDiffHunk.Render(rng) + sFaint.Render(" @@")
	if r.section != "" && width-used > 8 {
		sec := clip(sanitize(r.section), width-used-3)
		head += " " + sMuted.Render(sec)
		used += 1 + runewidth.StringWidth(sec)
	}
	if rest := width - used - 1; rest > 0 {
		head += " " + sFaint.Render(strings.Repeat("─", rest))
	}
	return head
}

// fitSegments renders segments (already sanitized) into at most w cells,
// ending with an ellipsis when cut, and returns the text and cells used.
func fitSegments(segs []seg, w int, base, hi lipgloss.Style) (string, int) {
	total := 0
	for _, s := range segs {
		total += runewidth.StringWidth(s.text)
	}
	limit, cut := w, total > w
	if cut {
		limit = w - 1
	}
	var b strings.Builder
	used := 0
	for _, s := range segs {
		var part strings.Builder
		full := true
		for _, r := range s.text {
			rw := runewidth.RuneWidth(r)
			if used+rw > limit {
				full = false
				break
			}
			part.WriteRune(r)
			used += rw
		}
		if part.Len() > 0 {
			st := base
			if s.hi {
				st = hi
			}
			b.WriteString(st.Render(part.String()))
		}
		if !full {
			break
		}
	}
	if cut {
		// A wide rune that did not fit leaves a cell to pad.
		if used < limit {
			b.WriteString(base.Render(strings.Repeat(" ", limit-used)))
			used = limit
		}
		b.WriteString(base.Render("…"))
		used++
	}
	return b.String(), used
}

// sanitize makes file content safe to draw: tabs become four spaces,
// carriage returns go, and other control characters (escape sequences
// included) become a visible placeholder, so content can never move the
// cursor or change colors.
func sanitize(s string) string {
	isCtl := func(r rune) bool { return r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) }
	if !strings.ContainsFunc(s, isCtl) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\t':
			b.WriteString("    ")
		case r == '\r':
		case isCtl(r):
			b.WriteRune('␛')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
