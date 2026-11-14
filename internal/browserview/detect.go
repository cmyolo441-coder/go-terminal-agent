package browserview

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// Protocol identifies a terminal graphics protocol.
type Protocol int

const (
	// ProtocolAuto asks the renderer to detect the protocol.
	ProtocolAuto Protocol = iota
	// ProtocolHalfBlock renders with "▀" cells and SGR colours.
	ProtocolHalfBlock
	// ProtocolKitty uses the Kitty graphics protocol.
	ProtocolKitty
	// ProtocolITerm2 uses iTerm2 inline images (OSC 1337).
	ProtocolITerm2
	// ProtocolSixel uses DEC Sixel graphics.
	ProtocolSixel
	// ProtocolNone disables image rendering; the pane shows status only.
	ProtocolNone
)

// String returns the short protocol name used in the pane title.
func (p Protocol) String() string {
	switch p {
	case ProtocolHalfBlock:
		return "blocks"
	case ProtocolKitty:
		return "kitty"
	case ProtocolITerm2:
		return "iterm2"
	case ProtocolSixel:
		return "sixel"
	case ProtocolNone:
		return "off"
	}
	return "auto"
}

// IsImage reports whether p places pixel images rather than text.
func (p Protocol) IsImage() bool {
	return p == ProtocolKitty || p == ProtocolITerm2 || p == ProtocolSixel
}

// ParseProtocol parses a protocol name as accepted by GTA_GRAPHICS.
func ParseProtocol(s string) (Protocol, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return ProtocolAuto, true
	case "kitty", "ghostty":
		return ProtocolKitty, true
	case "iterm", "iterm2", "osc1337", "wezterm":
		return ProtocolITerm2, true
	case "sixel", "sixels":
		return ProtocolSixel, true
	case "halfblock", "half-block", "blocks", "block", "ansi", "unicode", "text":
		return ProtocolHalfBlock, true
	case "none", "off", "0", "false", "disabled":
		return ProtocolNone, true
	}
	return ProtocolAuto, false
}

// Capabilities describes what the hosting terminal supports.
type Capabilities struct {
	Protocol  Protocol
	TrueColor bool
	// Tmux is set when running inside tmux; image protocols are wrapped in
	// DCS passthrough (requires `set -g allow-passthrough on`).
	Tmux bool
	// Cell is the character cell size in pixels (zero when unknown).
	Cell CellSize
	// Reason explains how the protocol was chosen.
	Reason string
}

// DetectEnv chooses a protocol from environment variables only (no I/O).
// getenv defaults to os.Getenv. GTA_GRAPHICS overrides detection.
func DetectEnv(getenv func(string) string) Capabilities {
	if getenv == nil {
		getenv = os.Getenv
	}
	term := strings.ToLower(getenv("TERM"))
	prog := strings.ToLower(getenv("TERM_PROGRAM"))
	colorterm := strings.ToLower(getenv("COLORTERM"))
	c := Capabilities{
		Tmux:      getenv("TMUX") != "" || strings.HasPrefix(term, "tmux"),
		TrueColor: colorterm == "truecolor" || colorterm == "24bit",
	}
	if strings.Contains(term, "direct") || strings.Contains(term, "kitty") || strings.Contains(term, "ghostty") ||
		prog == "wezterm" || prog == "iterm.app" || prog == "ghostty" || prog == "vscode" || getenv("KITTY_WINDOW_ID") != "" {
		c.TrueColor = true
	}
	if prog == "apple_terminal" {
		c.TrueColor = false
	}

	if v := getenv("GTA_GRAPHICS"); v != "" {
		if p, ok := ParseProtocol(v); ok && p != ProtocolAuto {
			c.Protocol, c.Reason = p, "GTA_GRAPHICS="+v
			return c
		}
	}

	switch {
	case c.Tmux:
		// Image protocols need explicit passthrough configuration inside
		// tmux; stay with text unless the user opts in via GTA_GRAPHICS.
		c.Protocol, c.Reason = ProtocolHalfBlock, "tmux"
	case getenv("KITTY_WINDOW_ID") != "" || strings.Contains(term, "kitty"):
		c.Protocol, c.Reason = ProtocolKitty, "kitty"
	case prog == "ghostty" || strings.Contains(term, "ghostty"):
		c.Protocol, c.Reason = ProtocolKitty, "ghostty"
	case getenv("WEZTERM_EXECUTABLE") != "" || getenv("WEZTERM_PANE") != "" || prog == "wezterm":
		c.Protocol, c.Reason = ProtocolITerm2, "wezterm"
	case prog == "iterm.app" || strings.EqualFold(getenv("LC_TERMINAL"), "iTerm2"):
		c.Protocol, c.Reason = ProtocolITerm2, "iterm2"
	case prog == "mintty":
		c.Protocol, c.Reason = ProtocolITerm2, "mintty"
	case getenv("KONSOLE_VERSION") != "":
		c.Protocol, c.Reason = ProtocolSixel, "konsole"
	case strings.HasPrefix(term, "foot") || strings.Contains(term, "mlterm") ||
		strings.Contains(term, "yaft") || strings.Contains(term, "contour") || strings.Contains(term, "sixel"):
		c.Protocol, c.Reason = ProtocolSixel, "TERM="+term
	default:
		c.Protocol, c.Reason = ProtocolHalfBlock, "fallback"
	}
	return c
}

// ProbeResult reports what a terminal answered to capability queries.
type ProbeResult struct {
	Kitty   bool     // answered the Kitty graphics query with OK
	Sixel   bool     // DA1 attribute 4
	DA1     []int    // primary device attributes
	Version string   // XTVERSION reply, e.g. "WezTerm 20240203"
	Cell    CellSize // from CSI 16 t, zero when unanswered
	Raw     []byte
}

// Protocol returns the best protocol the probe proved, or ProtocolAuto when
// it proved none.
func (r ProbeResult) Protocol() Protocol {
	switch {
	case r.Kitty:
		return ProtocolKitty
	case strings.Contains(strings.ToLower(r.Version), "iterm2") || strings.Contains(strings.ToLower(r.Version), "wezterm"):
		return ProtocolITerm2
	case r.Sixel:
		return ProtocolSixel
	}
	return ProtocolAuto
}

// probeQuery asks, in order: Kitty graphics support (1x1 query that is never
// displayed), XTVERSION, cell size in pixels, and DA1. DA1 is answered by
// every VT-compatible terminal and therefore acts as the end sentinel.
const probeQuery = "\x1b_Gi=4294967295,s=1,v=1,a=q,t=d,f=24;AAAA\x1b\\" + "\x1b[>0q" + "\x1b[16t" + "\x1b[c"

// ErrProbeTimeout is returned when the terminal did not answer DA1 in time.
var ErrProbeTimeout = errors.New("browserview: terminal probe timed out")

type readDeadliner interface {
	SetReadDeadline(time.Time) error
}

// Probe queries the terminal over rw (normally the tty in raw mode) and
// parses its answers. It waits at most timeout for the DA1 reply.
//
// If rw implements SetReadDeadline (e.g. *os.File for a tty) the deadline is
// used and no goroutine outlives the call. Otherwise reads happen on a
// helper goroutine; on timeout that goroutine stays blocked until rw
// yields data or is closed, so callers should pass a deadline-capable
// reader for interactive terminals.
func Probe(rw io.ReadWriter, timeout time.Duration) (ProbeResult, error) {
	if _, err := io.WriteString(rw, probeQuery); err != nil {
		return ProbeResult{}, err
	}
	deadline := time.Now().Add(timeout)
	var buf []byte
	chunk := make([]byte, 512)
	if d, ok := rw.(readDeadliner); ok {
		if err := d.SetReadDeadline(deadline); err == nil {
			defer func() { _ = d.SetReadDeadline(time.Time{}) }()
			for {
				n, err := rw.Read(chunk)
				buf = append(buf, chunk[:n]...)
				if r, done := parseProbe(buf); done {
					return r, nil
				}
				if err != nil {
					r, _ := parseProbe(buf)
					if errors.Is(err, os.ErrDeadlineExceeded) {
						return r, ErrProbeTimeout
					}
					return r, err
				}
			}
		}
	}
	type res struct {
		b   []byte
		err error
	}
	ch := make(chan res, 16)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			c := make([]byte, 512)
			n, err := rw.Read(c)
			select {
			case ch <- res{c[:n], err}:
			case <-stop:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for {
		select {
		case r := <-ch:
			buf = append(buf, r.b...)
			if pr, done := parseProbe(buf); done {
				return pr, nil
			}
			if r.err != nil {
				pr, _ := parseProbe(buf)
				return pr, r.err
			}
		case <-timer.C:
			pr, _ := parseProbe(buf)
			return pr, ErrProbeTimeout
		}
	}
}

// parseProbe extracts replies from buf; done is true once DA1 was seen.
func parseProbe(buf []byte) (r ProbeResult, done bool) {
	r.Raw = append([]byte(nil), buf...)
	for i := 0; i < len(buf); i++ {
		if buf[i] != 0x1b || i+1 >= len(buf) {
			continue
		}
		switch buf[i+1] {
		case '_': // APC: Kitty reply "\x1b_Gi=...;OK\x1b\\"
			end := bytes.Index(buf[i:], []byte("\x1b\\"))
			if end < 0 {
				return r, false
			}
			body := buf[i+2 : i+end]
			if bytes.HasPrefix(body, []byte("G")) && bytes.Contains(body, []byte(";OK")) {
				r.Kitty = true
			}
			i += end + 1
		case 'P': // DCS: XTVERSION "\x1bP>|name\x1b\\"
			end := bytes.Index(buf[i:], []byte("\x1b\\"))
			if end < 0 {
				return r, false
			}
			body := buf[i+2 : i+end]
			if v, ok := bytes.CutPrefix(body, []byte(">|")); ok {
				r.Version = string(v)
			}
			i += end + 1
		case '[':
			j := i + 2
			for j < len(buf) && (buf[j] < 0x40 || buf[j] > 0x7e) {
				j++
			}
			if j >= len(buf) {
				return r, false
			}
			params := string(buf[i+2 : j])
			switch buf[j] {
			case 'c':
				if p, ok := strings.CutPrefix(params, "?"); ok {
					for _, f := range strings.Split(p, ";") {
						if n, err := strconv.Atoi(f); err == nil {
							r.DA1 = append(r.DA1, n)
							if n == 4 {
								r.Sixel = true
							}
						}
					}
					done = true
				}
			case 't':
				f := strings.Split(params, ";")
				if len(f) == 3 && f[0] == "6" {
					h, e1 := strconv.Atoi(f[1])
					w, e2 := strconv.Atoi(f[2])
					if e1 == nil && e2 == nil {
						r.Cell = CellSize{W: w, H: h}
					}
				}
			}
			i = j
		}
	}
	return r, done
}

// Detect combines environment detection with the terminal-reported cell
// size of fd (when it is a tty). It performs no terminal queries; use Probe
// for active detection.
func Detect(fd uintptr) Capabilities {
	c := DetectEnv(nil)
	if cs, ok := TerminalCellSize(fd); ok {
		c.Cell = cs
	}
	return c
}
