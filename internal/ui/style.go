// Package ui holds terminal styling, the spinner and the streaming markdown renderer.
package ui

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// Color is enabled unless NO_COLOR is set or stdout is not a terminal.
var Color = detectColor()

func detectColor() bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	return IsTerminal(os.Stdout)
}

// IsTerminal reports whether f is a character device.
func IsTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func sgr(code, s string) string {
	if !Color || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func Bold(s string) string    { return sgr("1", s) }
func Dim(s string) string     { return sgr("2", s) }
func Italic(s string) string  { return sgr("3", s) }
func Red(s string) string     { return sgr("31", s) }
func Green(s string) string   { return sgr("32", s) }
func Yellow(s string) string  { return sgr("33", s) }
func Blue(s string) string    { return sgr("34", s) }
func Magenta(s string) string { return sgr("35", s) }
func Cyan(s string) string    { return sgr("36", s) }
func Accent(s string) string  { return sgr("38;5;208", s) }
func Gray(s string) string    { return sgr("90", s) }

// Truncate shortens s to at most n runes, adding an ellipsis.
func Truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}

// FirstLines returns up to n lines of s and how many were cut.
func FirstLines(s string, n int) (string, int) {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n"), 0
	}
	return strings.Join(lines[:n], "\n"), len(lines) - n
}

// HumanTokens formats a token count compactly.
func HumanTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprintf("%d", n)
}

// Spinner is a single-line activity indicator written to stderr.
type Spinner struct {
	mu      sync.Mutex
	label   string
	started time.Time
	stop    chan struct{}
	done    chan struct{}
	active  bool
}

var frames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Start shows the spinner with a label. It is a no-op when not on a terminal.
func (s *Spinner) Start(label string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.label = label
	if s.active || !Color || !IsTerminal(os.Stderr) {
		return
	}
	s.active = true
	s.started = time.Now()
	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	go func(stop, done chan struct{}) {
		defer close(done)
		t := time.NewTicker(90 * time.Millisecond)
		defer t.Stop()
		for i := 0; ; i++ {
			s.mu.Lock()
			label := s.label
			el := time.Since(s.started).Round(time.Second)
			s.mu.Unlock()
			fmt.Fprintf(os.Stderr, "\r\x1b[2K%s %s %s", Accent(frames[i%len(frames)]), label,
				Dim(fmt.Sprintf("(%s · ctrl+c to interrupt)", el)))
			select {
			case <-stop:
				fmt.Fprint(os.Stderr, "\r\x1b[2K")
				return
			case <-t.C:
			}
		}
	}(s.stop, s.done)
}

// Stop hides the spinner and waits until the line is cleared.
func (s *Spinner) Stop() {
	s.mu.Lock()
	if !s.active {
		s.mu.Unlock()
		return
	}
	s.active = false
	stop, done := s.stop, s.done
	s.mu.Unlock()
	close(stop)
	<-done
}
