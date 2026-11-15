package ui

import (
	"io"
	"regexp"
	"strings"
)

// Markdown renders streamed markdown line by line with light ANSI styling.
// Feed it deltas with Write and call Flush at the end of a block.
type Markdown struct {
	W       io.Writer
	Prefix  string // printed before the very first line
	Indent  string // printed before every following line
	buf     strings.Builder
	inCode  bool
	started bool
}

var (
	reBold   = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	reCode   = regexp.MustCompile("`([^`\n]+)`")
	reLink   = regexp.MustCompile(`\[([^\]\n]+)\]\(([^)\s]+)\)`)
	reHeader = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	reBullet = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
	reNum    = regexp.MustCompile(`^(\s*)(\d+)[.)]\s+(.*)$`)
	reRule   = regexp.MustCompile(`^\s*([-*_])(\s*[-*_]){2,}\s*$`)
)

// Write consumes a streamed chunk, emitting every complete line.
func (m *Markdown) Write(s string) {
	m.buf.WriteString(s)
	all := m.buf.String()
	i := strings.LastIndexByte(all, '\n')
	if i < 0 {
		return
	}
	m.buf.Reset()
	m.buf.WriteString(all[i+1:])
	for _, line := range strings.Split(all[:i], "\n") {
		m.line(line)
	}
}

// Flush emits any buffered partial line and resets the renderer.
func (m *Markdown) Flush() {
	if m.buf.Len() > 0 {
		rest := m.buf.String()
		m.buf.Reset()
		m.line(rest)
	}
	m.inCode = false
	m.started = false
}

// Started reports whether anything was written since the last Flush.
func (m *Markdown) Started() bool { return m.started || m.buf.Len() > 0 }

func (m *Markdown) line(line string) {
	lead := m.Indent
	if !m.started {
		if strings.TrimSpace(line) == "" {
			return // skip leading blank lines
		}
		lead = m.Prefix
		m.started = true
	}
	io.WriteString(m.W, lead+m.render(line)+"\n")
}

func (m *Markdown) render(line string) string {
	trim := strings.TrimSpace(line)
	if strings.HasPrefix(trim, "```") {
		m.inCode = !m.inCode
		if m.inCode && len(trim) > 3 {
			return Dim("─── " + strings.TrimPrefix(trim, "```") + " ───")
		}
		return Dim("───")
	}
	if m.inCode {
		return Cyan(line)
	}
	if !Color {
		return line
	}
	if h := reHeader.FindStringSubmatch(line); h != nil {
		return Bold(Accent(inline(h[2])))
	}
	if reRule.MatchString(line) {
		return Dim(strings.Repeat("─", 40))
	}
	if b := reBullet.FindStringSubmatch(line); b != nil {
		return b[1] + Accent("•") + " " + inline(b[2])
	}
	if n := reNum.FindStringSubmatch(line); n != nil {
		return n[1] + Accent(n[2]+".") + " " + inline(n[3])
	}
	if strings.HasPrefix(trim, ">") {
		return Dim("│ " + strings.TrimSpace(strings.TrimPrefix(trim, ">")))
	}
	return inline(line)
}

func inline(s string) string {
	s = reCode.ReplaceAllStringFunc(s, func(m string) string { return Cyan(strings.Trim(m, "`")) })
	s = reBold.ReplaceAllStringFunc(s, func(m string) string { return Bold(strings.Trim(m, "*")) })
	s = reLink.ReplaceAllString(s, "$1 ("+"$2"+")")
	return s
}
