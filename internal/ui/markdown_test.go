package ui

import (
	"strings"
	"testing"
)

func TestFlushPoint(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"hello", 0},
		{"para one\n\npara two", len("para one\n\n")},
		{"intro\n\n```go\nfunc a() {}\n\nfunc b() {}\n", len("intro\n\n")},
		{"```go\nx\n```\nafter", len("```go\nx\n```\n")},
		{"a\n\nb\n\nc", len("a\n\nb\n\n")},
	}
	for _, c := range cases {
		if got := flushPoint(c.in); got != c.want {
			t.Errorf("flushPoint(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestRenderMarkdown(t *testing.T) {
	md := newMD("dark")
	out := stripANSI(md.render("# Title\n\nSome `code` and **bold**.", 60))
	if out == "" || !strings.Contains(out, "Title") || !strings.Contains(out, "code") {
		t.Fatalf("unexpected render: %q", out)
	}
}

