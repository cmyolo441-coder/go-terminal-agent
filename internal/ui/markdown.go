package ui

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// mdRenderer renders Markdown for the terminal in the active theme,
// rebuilding itself when the width or the theme changes.
type mdRenderer struct {
	theme Theme
	dark  bool
	width int
	r     *glamour.TermRenderer
}

// newMD makes a renderer for a theme name ("dark", "light", "arctic"...).
// Unknown names use the auto theme on a dark background.
func newMD(name string) *mdRenderer {
	t, ok := lookupTheme(name)
	if !ok {
		t, _ = lookupTheme(ThemeAuto)
	}
	return &mdRenderer{theme: t, dark: true}
}

// setTheme switches the theme; termDark is the detected background.
func (m *mdRenderer) setTheme(t Theme, termDark bool) {
	m.theme, m.dark, m.r = t, termDark, nil
}

// Glamour's stock styles indent the document by 2 cells on each side and
// code blocks by 2 more.
const (
	mdDocMargin  = 2
	mdCodeMargin = 2
)

func (m *mdRenderer) render(src string, width int) string {
	// Raw tabs make terminals jump to the next tab stop, which breaks code
	// block backgrounds and alignment; render them as four spaces.
	src = strings.ReplaceAll(src, "\t", "    ")
	if width < 20 {
		width = 20
	}
	if m.r == nil || m.width != width {
		r, err := glamour.NewTermRenderer(
			glamour.WithStyles(m.theme.mdStyle(m.dark)),
			glamour.WithColorProfile(lipgloss.ColorProfile()),
			glamour.WithWordWrap(width),
			glamour.WithEmoji(),
		)
		if err != nil {
			return src
		}
		m.r, m.width = r, width
	}
	// Glamour wraps long code lines back to the document margin, which
	// reads as text that escaped the block; wrap them first, keeping the
	// indentation.
	src = wrapCodeBlocks(src, width-2*mdDocMargin-2*mdCodeMargin)
	out, err := m.r.Render(src)
	if err != nil {
		return src
	}
	lines := strings.Split(out, "\n")
	lines = hangListItems(lines, width-mdDocMargin, codeLines(src))
	limit := lipgloss.NewStyle().MaxWidth(width)
	for i, l := range lines {
		l = trimPad(l)
		if lipgloss.Width(l) > width {
			l = trimPad(limit.Render(l))
		}
		lines[i] = l
	}
	return trimBlankLines(strings.Join(lines, "\n"))
}

// trimBlankLines removes leading and trailing blank lines and trailing
// spaces glamour pads lines with.
func trimBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	start, end := 0, len(lines)
	for start < end && strings.TrimSpace(stripANSI(lines[start])) == "" {
		start++
	}
	for end > start && strings.TrimSpace(stripANSI(lines[end-1])) == "" {
		end--
	}
	return strings.Join(lines[start:end], "\n")
}

// ---------- code blocks ----------

var fenceRe = regexp.MustCompile("^ {0,3}(```+|~~~+)")

// wrapCodeBlocks hard-wraps lines inside fenced code blocks to width cells.
// Continuation lines keep the line's indentation plus two spaces, and
// breaks prefer a space in the second half of the line.
func wrapCodeBlocks(src string, width int) string {
	if width < 10 {
		return src
	}
	lines := strings.Split(src, "\n")
	var out []string
	fence := ""
	for _, l := range lines {
		var isFence bool
		if fence, isFence = nextFence(fence, l); isFence {
			out = append(out, l)
			continue
		}
		if fence == "" || runewidth.StringWidth(l) <= width {
			out = append(out, l)
			continue
		}
		out = append(out, hardWrap(l, width)...)
	}
	return strings.Join(out, "\n")
}

// nextFence tracks fenced code blocks line by line: given the open fence
// ("" outside a block) and a line, it returns the fence after the line and
// whether the line itself is a fence. A block closes on a fence of the
// same character at least as long as the one that opened it.
func nextFence(open, line string) (string, bool) {
	m := fenceRe.FindStringSubmatch(line)
	if m == nil {
		return open, false
	}
	if open == "" {
		return m[1], true
	}
	if t := strings.TrimSpace(line); len(t) >= len(open) && strings.Trim(t, open[:1]) == "" {
		return "", true
	}
	return open, true // a fence-like line inside the block is content
}

// hardWrap splits one code line into pieces of at most width cells.
func hardWrap(l string, width int) []string {
	indent := len(l) - len(strings.TrimLeft(l, " "))
	if indent > width/3 {
		indent = width / 3
	}
	cont := strings.Repeat(" ", indent+2)
	var out []string
	rest := []rune(l)
	prefix := ""
	for len(rest) > 0 {
		room := width - runewidth.StringWidth(prefix)
		if runewidth.StringWidth(string(rest)) <= room {
			out = append(out, prefix+string(rest))
			break
		}
		// Longest prefix of rest that fits.
		n, w := 0, 0
		for n < len(rest) && w+runewidth.RuneWidth(rest[n]) <= room {
			w += runewidth.RuneWidth(rest[n])
			n++
		}
		if n == 0 {
			n = 1 // a rune wider than the room: take it anyway
		}
		// Prefer breaking after a space in the second half.
		for k := n; k > n/2; k-- {
			if rest[k-1] == ' ' && strings.TrimSpace(string(rest[:k])) != "" {
				n = k
				break
			}
		}
		out = append(out, strings.TrimRight(prefix+string(rest[:n]), " "))
		rest = []rune(strings.TrimLeft(string(rest[n:]), " "))
		prefix = cont
	}
	return out
}

// ---------- lists ----------

var itemRe = regexp.MustCompile(`^( *)(• |\* |- |\d{1,3}\. |\[[ ✓xX]\] )`)

// codeLines collects the trimmed lines of fenced code blocks, so the list
// fix-up never mistakes code (say, a YAML list) for list items.
func codeLines(src string) map[string]bool {
	set := map[string]bool{}
	fence := ""
	for _, l := range strings.Split(src, "\n") {
		var isFence bool
		if fence, isFence = nextFence(fence, l); isFence {
			continue
		}
		if t := strings.TrimSpace(l); fence != "" && t != "" {
			set[t] = true
		}
	}
	return set
}

// hangListItems re-wraps list items so wrapped lines hang under the item's
// text instead of falling back to the bullet's margin. right is the last
// column glamour wraps text at; a line counts as wrapped only when the
// next line's first word could not have fit after it. code holds the
// lines of code blocks, which are never touched; neither are tables.
func hangListItems(lines []string, right int, code map[string]bool) []string {
	skip := func(plain string) bool {
		return code[strings.TrimSpace(plain)] || strings.Contains(plain, "│")
	}
	wrappedBefore := func(prev, cur string) bool {
		first := strings.Fields(cur)
		return len(first) > 0 && runewidth.StringWidth(strings.TrimRight(prev, " "))+1+runewidth.StringWidth(first[0]) > right
	}
	var out []string
	for i := 0; i < len(lines); i++ {
		plain := stripANSI(lines[i])
		m := itemRe.FindStringSubmatch(plain)
		if m == nil || skip(plain) {
			out = append(out, lines[i])
			continue
		}
		bulletCol := len(m[1])
		textCol := bulletCol + runewidth.StringWidth(m[2])
		j := i + 1
		for prev := plain; j < len(lines); j++ {
			p := stripANSI(lines[j])
			if strings.TrimSpace(p) == "" || itemRe.MatchString(p) || leadingSpaces(p) > bulletCol || skip(p) || !wrappedBefore(prev, p) {
				break
			}
			prev = p
		}
		avail := right - textCol
		if j == i+1 || avail < 12 {
			out = append(out, lines[i])
			continue
		}
		head, first := splitAtCol(lines[i], textCol)
		if strings.Contains(head, "\x1b") {
			head += "\x1b[0m" // the text after it restores its own styles
		}
		parts := []string{trimPad(first)}
		for k := i + 1; k < j; k++ {
			_, t := splitAtCol(lines[k], leadingSpaces(stripANSI(lines[k])))
			parts = append(parts, trimPad(t))
		}
		wrapped := strings.Split(lipgloss.NewStyle().Width(avail).Render(strings.Join(parts, " ")), "\n")
		for k, w := range wrapped {
			w = trimPad(w)
			if k == 0 {
				out = append(out, head+w)
			} else {
				out = append(out, strings.Repeat(" ", textCol)+w)
			}
		}
		i = j - 1
	}
	return out
}

func leadingSpaces(s string) int { return len(s) - len(strings.TrimLeft(s, " ")) }

// splitAtCol cuts an ANSI-styled line after col visible cells. The tail
// starts with the styles still active at the cut, so it renders the same.
func splitAtCol(s string, col int) (string, string) {
	active := ""
	w := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			j := escEnd(s, i)
			seq := s[i:j]
			if isReset(seq) {
				active = ""
			} else if strings.HasSuffix(seq, "m") {
				active += seq
			}
			i = j
			continue
		}
		if w >= col {
			return s[:i], active + s[i:]
		}
		r, size := decodeRune(s[i:])
		w += runewidth.RuneWidth(r)
		i += size
	}
	return s, ""
}

// escEnd returns the index just past the escape sequence starting at i.
func escEnd(s string, i int) int {
	j := i + 1
	if j < len(s) && s[j] == '[' {
		j++
		for j < len(s) && !(s[j] >= 0x40 && s[j] <= 0x7e) {
			j++
		}
		return min(j+1, len(s))
	}
	if j < len(s) && s[j] == ']' {
		// OSC: ends with BEL or ST.
		for j < len(s) {
			if s[j] == 0x07 {
				return j + 1
			}
			if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
				return j + 2
			}
			j++
		}
		return len(s)
	}
	return min(j+1, len(s))
}

func isReset(seq string) bool { return seq == "\x1b[0m" || seq == "\x1b[m" }

func decodeRune(s string) (rune, int) { return utf8.DecodeRuneInString(s) }

// trimPad drops trailing spaces from an ANSI-styled line, except spaces
// drawn on a background color (a heading's colored block must keep its
// shape). It closes any style the cut left open.
func trimPad(s string) string {
	if !strings.Contains(s, "\x1b") {
		return strings.TrimRight(s, " ")
	}
	keep := 0
	bg := false
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			j := escEnd(s, i)
			if seq := s[i:j]; strings.HasPrefix(seq, "\x1b[") && strings.HasSuffix(seq, "m") {
				bg = sgrBackground(seq[2:len(seq)-1], bg)
			}
			i = j
			continue
		}
		_, size := decodeRune(s[i:])
		if s[i] != ' ' || bg {
			keep = i + size
		}
		i += size
	}
	if keep == 0 {
		return ""
	}
	tail := s[keep:]
	if !strings.Contains(tail, "\x1b") {
		return strings.TrimRight(s, " ")
	}
	return s[:keep] + "\x1b[0m"
}

// sgrBackground updates whether a background (or reverse video) is active
// after an SGR parameter list.
func sgrBackground(params string, bg bool) bool {
	ps := strings.Split(params, ";")
	for i := 0; i < len(ps); i++ {
		switch p := ps[i]; {
		case p == "" || p == "0" || p == "49" || p == "27":
			bg = false
		case p == "7":
			bg = true
		case len(p) == 2 && p[0] == '4' && p[1] >= '0' && p[1] <= '7':
			bg = true
		case len(p) == 3 && strings.HasPrefix(p, "10") && p[2] >= '0' && p[2] <= '7':
			bg = true
		case p == "48":
			bg = true
			if i+1 < len(ps) && ps[i+1] == "5" {
				i += 2
			} else if i+1 < len(ps) && ps[i+1] == "2" {
				i += 4
			}
		case p == "38" || p == "58":
			if i+1 < len(ps) && ps[i+1] == "5" {
				i += 2
			} else if i+1 < len(ps) && ps[i+1] == "2" {
				i += 4
			}
		}
	}
	return bg
}

// flushPoint returns how much of a streaming Markdown buffer can be rendered
// now: everything up to the last blank line that is not inside a fenced code
// block. It returns 0 when nothing is safe to flush yet.
func flushPoint(buf string) int {
	inFence := false
	last := 0
	pos := 0
	lines := strings.SplitAfter(buf, "\n")
	for i, line := range lines {
		pos += len(line)
		if !strings.HasSuffix(line, "\n") {
			break // incomplete final line
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			if !inFence {
				last = pos // a closed fence is a safe boundary
			}
			continue
		}
		if !inFence && trimmed == "" && i > 0 {
			last = pos
		}
	}
	return last
}

// stripANSI removes terminal escape sequences.
func stripANSI(s string) string {
	var b strings.Builder
	esc := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if esc {
			if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
				esc = false
			}
			continue
		}
		if c == 0x1b {
			esc = true
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}
