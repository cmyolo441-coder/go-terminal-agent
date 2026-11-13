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

func TestRenderExpandsTabs(t *testing.T) {
	out := stripANSI(newMD("dark").render("```go\nfunc f() {\n\treturn\n}\n```\n", 80))
	if strings.Contains(out, "\t") {
		t.Fatalf("tab survived rendering: %q", out)
	}
}

func TestClipMiddle(t *testing.T) {
	s := "/tmp/a-very-long-directory-name/with/many/segments/work"
	got := clipMiddle(s, 30)
	if w := len([]rune(got)); w > 30 || !strings.HasSuffix(got, "segments/work") || !strings.HasPrefix(got, "/tmp") {
		t.Fatalf("clipMiddle = %q (%d)", got, w)
	}
	if clipMiddle("short", 30) != "short" {
		t.Fatal("short string changed")
	}
}

func TestMeterBounds(t *testing.T) {
	for _, f := range []float64{-1, 0, 0.5, 1, 2} {
		if got := lipglossWidth(meter(f, 10)); got != 10 {
			t.Fatalf("meter(%v) width %d", f, got)
		}
	}
}
