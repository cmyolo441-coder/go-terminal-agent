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

const richMD = "# Heading\n\nA paragraph long enough to wrap a few times at narrow widths, so the wrapping is exercised properly.\n\n" +
	"```go\nfunc main() {\n\tfmt.Println(\"this is a very long line of code that goes way past sixty columns for sure ok\")\n}\n```\n\n" +
	"| Name | Description | Value |\n| --- | --- | --- |\n| alpha | a fairly long description that will need wrapping in narrow terminals | 1 |\n| beta | short | 2 |\n\n" +
	"- first item with a long text that wraps around because it is long, long, long and longer\n" +
	"  - nested item with even more words so that it also wraps at the narrow width\n" +
	"- third\n\n1. one\n2. two\n"

func TestMarkdownWidthAware(t *testing.T) {
	for _, width := range []int{60, 120} {
		withColor(t)
		out := newMD("dark").render(richMD, width)
		for i, l := range strings.Split(out, "\n") {
			if w := lipglossWidth(l); w > width {
				t.Errorf("width %d: line %d is %d cells: %q", width, i, w, stripANSI(l))
			}
		}
		plain := stripANSI(out)
		for _, want := range []string{"Heading", "func main() {", "for sure ok", "alpha", "│", "• first item", "1. one"} {
			if !strings.Contains(plain, want) {
				t.Errorf("width %d: %q missing from\n%s", width, want, plain)
			}
		}
		for i, l := range strings.Split(plain, "\n") {
			if i > 0 && strings.HasSuffix(l, " ") {
				t.Errorf("width %d: trailing padding on %q", width, l)
			}
		}
	}
}

func TestMarkdownCodeWrapStaysInBlock(t *testing.T) {
	plain := stripANSI(newMD("dark").render(richMD, 60))
	in := false
	for _, l := range strings.Split(plain, "\n") {
		if strings.Contains(l, "func main() {") {
			in = true
			continue
		}
		if in {
			if strings.TrimSpace(l) == "}" {
				return
			}
			if leadingSpaces(l) < 4 {
				t.Fatalf("code continuation escaped the block: %q\n%s", l, plain)
			}
		}
	}
	t.Fatalf("code block missing:\n%s", plain)
}

func TestMarkdownListItemsHang(t *testing.T) {
	plain := stripANSI(newMD("dark").render(richMD, 60))
	lines := strings.Split(plain, "\n")
	for i, l := range lines {
		if strings.Contains(l, "• first item") {
			next := lines[i+1]
			if leadingSpaces(next) != leadingSpaces(l)+2 || strings.TrimSpace(next) == "" {
				t.Fatalf("continuation does not hang under the text:\n%s\n%s", l, next)
			}
			return
		}
	}
	t.Fatalf("list missing:\n%s", plain)
}

func TestMarkdownListFixLeavesCodeAndShortItems(t *testing.T) {
	src := "```yaml\n- name: build\n  run: go build ./...\n- name: test\n```\n\n- short\n- also short\n"
	plain := stripANSI(newMD("dark").render(src, 60))
	for _, want := range []string{"- name: build", "  run: go build ./...", "• short", "• also short"} {
		found := false
		for _, l := range strings.Split(plain, "\n") {
			if strings.Contains(l, want) {
				found = true
			}
		}
		if !found {
			t.Fatalf("%q missing or merged:\n%s", want, plain)
		}
	}
}

func TestWrapCodeBlocks(t *testing.T) {
	prose := "text that is very long but outside of a code block stays as it is, unchanged"
	src := prose + "\n```\n    indented code line that is far too long to fit\nshort\n```\n"
	lines := strings.Split(wrapCodeBlocks(src, 20), "\n")
	if lines[0] != prose {
		t.Fatal("prose was wrapped")
	}
	var code []string
	for _, l := range lines[2:] {
		if l == "short" {
			break
		}
		if len(l) > 20 {
			t.Fatalf("code line %q longer than 20", l)
		}
		code = append(code, l)
	}
	if len(code) < 2 || !strings.HasPrefix(code[1], "      ") {
		t.Fatalf("continuation lost its indent: %q", code)
	}
	if got := strings.Join(strings.Fields(strings.Join(code, " ")), " "); got != "indented code line that is far too long to fit" {
		t.Fatalf("code text changed: %q", got)
	}
}

func TestTrimPadKeepsBackgrounds(t *testing.T) {
	if got := trimPad("\x1b[38;5;252mtext\x1b[0m   \x1b[0m"); got != "\x1b[38;5;252mtext\x1b[0m" {
		t.Fatalf("plain padding kept: %q", got)
	}
	if got := trimPad("\x1b[48;5;63m Title \x1b[0m"); stripANSI(got) != " Title " {
		t.Fatalf("background space trimmed: %q", got)
	}
	if got := trimPad("abc   "); got != "abc" {
		t.Fatalf("trimPad = %q", got)
	}
}

func TestSplitAtColCarriesStyle(t *testing.T) {
	head, tail := splitAtCol("\x1b[1m• \x1b[0m\x1b[31mred words\x1b[0m", 2)
	if stripANSI(head) != "• " || stripANSI(tail) != "red words" || !strings.HasPrefix(tail, "\x1b[31m") {
		t.Fatalf("head %q tail %q", head, tail)
	}
	head, tail = splitAtCol("\x1b[31mabcdef\x1b[0m", 3)
	if stripANSI(head) != "abc" || !strings.HasPrefix(tail, "\x1b[31mdef") {
		t.Fatalf("mid-span cut: head %q tail %q", head, tail)
	}
}
