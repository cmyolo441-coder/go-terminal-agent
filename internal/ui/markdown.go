package ui

import (
	"strings"

	"github.com/charmbracelet/glamour"
)

// mdRenderer renders Markdown for the terminal, rebuilding itself when the
// width changes.
type mdRenderer struct {
	style string
	width int
	r     *glamour.TermRenderer
}

func newMD(style string) *mdRenderer { return &mdRenderer{style: style} }

func (m *mdRenderer) render(src string, width int) string {
	// Raw tabs make terminals jump to the next tab stop, which breaks code
	// block backgrounds and alignment; render them as four spaces.
	src = strings.ReplaceAll(src, "\t", "    ")
	if width < 20 {
		width = 20
	}
	if m.r == nil || m.width != width {
		r, err := glamour.NewTermRenderer(
			glamour.WithStandardStyle(m.style),
			glamour.WithWordWrap(width),
			glamour.WithEmoji(),
		)
		if err != nil {
			return src
		}
		m.r, m.width = r, width
	}
	out, err := m.r.Render(src)
	if err != nil {
		return src
	}
	return trimBlankLines(out)
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
