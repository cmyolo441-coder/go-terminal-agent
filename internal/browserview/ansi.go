package browserview

import (
	"strings"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"
)

// escLen returns the length of the escape sequence starting at s[0] (which
// must be ESC). Unterminated sequences extend to the end of s.
func escLen(s string) int {
	if len(s) < 2 {
		return len(s)
	}
	switch s[1] {
	case '[': // CSI: parameters/intermediates then a final byte 0x40-0x7E
		for i := 2; i < len(s); i++ {
			if s[i] >= 0x40 && s[i] <= 0x7E {
				return i + 1
			}
		}
		return len(s)
	case ']', 'P', '_', '^', 'X': // OSC, DCS, APC, PM, SOS: until ST or BEL
		for i := 2; i < len(s); i++ {
			if s[i] == 0x07 && s[1] == ']' {
				return i + 1
			}
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
		}
		return len(s)
	case '(', ')', '*', '+', '#', '%': // charset designation etc.: one more byte
		return min(3, len(s))
	}
	return 2
}

// StringWidth returns the number of terminal cells s occupies, ignoring
// ANSI escape sequences.
func StringWidth(s string) int {
	w := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c == 0x1b {
			i += escLen(s[i:])
			continue
		}
		if c < utf8.RuneSelf {
			if c >= 0x20 && c != 0x7f {
				w++
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		w += runewidth.RuneWidth(r)
		i += size
	}
	return w
}

// TruncateANSI cuts s to at most width cells, keeping escape sequences
// intact. When anything was cut an SGR reset is appended so styles do not
// leak.
func TruncateANSI(s string, width int) string {
	if width <= 0 {
		return ""
	}
	w := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c == 0x1b {
			i += escLen(s[i:])
			continue
		}
		rw, size := 1, 1
		if c >= utf8.RuneSelf {
			var r rune
			r, size = utf8.DecodeRuneInString(s[i:])
			rw = runewidth.RuneWidth(r)
		} else if c < 0x20 || c == 0x7f {
			rw = 0
		}
		if w+rw > width {
			return s[:i] + "\x1b[0m"
		}
		w += rw
		i += size
	}
	return s
}

// PadANSI truncates or right-pads s with spaces to exactly width cells.
func PadANSI(s string, width int) string {
	w := StringWidth(s)
	if w > width {
		s = TruncateANSI(s, width)
		w = StringWidth(s)
	}
	if w < width {
		return s + strings.Repeat(" ", width-w)
	}
	return s
}

// SideBySide joins two ANSI-styled blocks line by line. Every left line is
// truncated or padded to exactly leftWidth cells (with an SGR reset so its
// styling cannot bleed into the right block); the right block is appended
// unchanged. The result has as many lines as the taller block.
func SideBySide(left string, leftWidth int, right string) string {
	ll := strings.Split(left, "\n")
	rl := strings.Split(right, "\n")
	n := max(len(ll), len(rl))
	var sb strings.Builder
	sb.Grow(len(left) + len(right) + n*(leftWidth+8))
	blank := strings.Repeat(" ", max(leftWidth, 0))
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteByte('\n')
		}
		if i < len(ll) {
			sb.WriteString(PadANSI(ll[i], leftWidth))
			if strings.IndexByte(ll[i], 0x1b) >= 0 {
				sb.WriteString("\x1b[0m")
			}
		} else {
			sb.WriteString(blank)
		}
		if i < len(rl) {
			sb.WriteString(rl[i])
		}
	}
	return sb.String()
}
