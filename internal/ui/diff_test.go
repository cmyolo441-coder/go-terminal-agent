package ui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

const sampleOld = "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hello\")\n\tx := compute(alpha, beta)\n\treturn\n}\n"
const sampleNew = "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hello, world\")\n\tx := compute(alpha, gamma) // a trailing comment long enough to overflow a narrow terminal\n\treturn\n}\n"

func sampleDiff() string { return tools.UnifiedDiff("main.go", sampleOld, sampleNew) }

func TestParseDiffLineNumbers(t *testing.T) {
	rows := parseDiff(sampleDiff())
	var got []string
	for _, r := range rows {
		got = append(got, fmt.Sprintf("%c%d", r.kind, r.num))
	}
	want := "@0 3 4 5-6-7+6+7 8 9"
	if strings.Join(got, "") != want {
		t.Fatalf("rows = %q, want %q", strings.Join(got, ""), want)
	}
	if add, del := diffStat(sampleDiff()); add != 2 || del != 2 {
		t.Fatalf("diffStat = +%d -%d", add, del)
	}
}

func TestParseDiffKeepsDashContentInsideHunks(t *testing.T) {
	// A deleted SQL comment "-- old" appears as "--- old" in the diff.
	d := "--- q.sql\n+++ q.sql\n@@ -1,2 +1,2 @@\n--- old\n+++ new\n select 1;\n"
	rows := parseDiff(d)
	if len(rows) != 4 || rows[1].kind != rowDel || rows[1].text != "-- old" || rows[2].kind != rowAdd || rows[2].text != "++ new" {
		t.Fatalf("rows = %+v", rows)
	}
	if add, del := diffStat(d); add != 1 || del != 1 {
		t.Fatalf("diffStat = +%d -%d", add, del)
	}
}

func TestParseDiffGitMultiFile(t *testing.T) {
	d := "diff --git a/a.go b/a.go\nindex 1..2 100644\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@ func A()\n-a\n+b\n\\ No newline at end of file\ndiff --git a/b.go b/b.go\n--- a/b.go\n+++ b/b.go\n@@ -10,0 +11,2 @@\n+x\n+y\n"
	rows := parseDiff(d)
	var kinds strings.Builder
	for _, r := range rows {
		kinds.WriteByte(r.kind)
	}
	if kinds.String() != "F@-+\\F@++" {
		t.Fatalf("kinds = %q", kinds.String())
	}
	if rows[0].text != "a.go" || rows[1].section != "func A()" || rows[7].num != 11 || rows[8].num != 12 {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestWordDiffHighlightsChangedWords(t *testing.T) {
	a, b := wordDiff(`fmt.Println("hello")`, `fmt.Println("hello, world")`)
	if a == nil || b == nil {
		t.Fatal("similar lines should be highlighted")
	}
	if hiText(a) != "" {
		t.Fatalf("pure insertion highlighted old text %q", hiText(a))
	}
	if hiText(b) != ", world" {
		t.Fatalf("new side highlight = %q", hiText(b))
	}
	a, b = wordDiff("x := compute(alpha, beta)", "x := compute(alpha, gamma)")
	if hiText(a) != "beta" || hiText(b) != "gamma" {
		t.Fatalf("highlights = %q / %q", hiText(a), hiText(b))
	}
	// Joined segments always rebuild the line.
	if joinSegs(a) != "x := compute(alpha, beta)" || joinSegs(b) != "x := compute(alpha, gamma)" {
		t.Fatal("segments lost text")
	}
	// Unrelated lines are not worth highlighting word by word.
	if a, b := wordDiff("return nil", "for i := range items {"); a != nil || b != nil {
		t.Fatal("dissimilar lines highlighted")
	}
}

func hiText(segs []seg) string {
	var b strings.Builder
	for _, s := range segs {
		if s.hi {
			b.WriteString(s.text)
		}
	}
	return b.String()
}

func joinSegs(segs []seg) string {
	var b strings.Builder
	for _, s := range segs {
		b.WriteString(s.text)
	}
	return b.String()
}

func TestRenderDiffIntraLineStyles(t *testing.T) {
	withColor(t)
	withTheme(t, "dark")
	out := strings.Join(renderDiff(sampleDiff(), 100, 0), "\n")
	if !strings.Contains(out, sDiffAddHi.Render(", world")) {
		t.Fatal("added words are not emphasized")
	}
	if !strings.Contains(out, sDiffDelHi.Render("beta")) || !strings.Contains(out, sDiffAddHi.Render("gamma")) {
		t.Fatal("replaced words are not emphasized on both sides")
	}
}

func TestRenderDiffWidths(t *testing.T) {
	for _, theme := range []string{"auto", "arctic", "mono"} {
		for _, width := range []int{60, 120} {
			t.Run(fmt.Sprintf("%s/%d", theme, width), func(t *testing.T) {
				withColor(t)
				withTheme(t, theme)
				inner := width - 6 // as renderToolDone gives it
				lines := renderDiff(sampleDiff(), inner, 0)
				rows := parseDiff(sampleDiff())[1:] // the lone hunk header is dropped
				if len(lines) != len(rows) {
					t.Fatalf("%d lines for %d rows", len(lines), len(rows))
				}
				for i, l := range lines {
					w := lipgloss.Width(l)
					if w > inner {
						t.Errorf("line %d is %d cells, wider than %d: %q", i, w, inner, stripANSI(l))
					}
					if k := rows[i].kind; (k == rowAdd || k == rowDel) && w != inner {
						t.Errorf("changed line %d is %d cells, want exactly %d", i, w, inner)
					}
					if strings.Contains(l, "\n") {
						t.Errorf("line %d contains a newline", i)
					}
				}
				// The whole tool block must fit the terminal too.
				call := &tools.Call{Tool: "edit_file", Summary: "Edit main.go", Preview: sampleDiff()}
				for _, l := range strings.Split(renderToolDone(call, "updated main.go", nil, width, densityCompact), "\n") {
					if w := lipgloss.Width(l); w > width {
						t.Errorf("tool block line %d cells > %d: %q", w, width, stripANSI(l))
					}
				}
				long := stripANSI(lines[6])
				if width == 60 && !strings.HasSuffix(long, "…") {
					t.Errorf("long line not cut with an ellipsis: %q", long)
				}
				if width == 120 && !strings.HasSuffix(strings.TrimRight(long, " "), "terminal") {
					t.Errorf("long line cut at 120 columns: %q", long)
				}
			})
		}
	}
}

func TestRenderDiffGutter(t *testing.T) {
	lines := renderDiff(sampleDiff(), 60, 0)
	if got := stripANSI(lines[3]); !strings.HasPrefix(got, " 6 -     fmt.Println") {
		t.Fatalf("gutter = %q", got)
	}
	// Too narrow for numbers: content keeps the room.
	narrow := renderDiff(sampleDiff(), 16, 0)
	if got := stripANSI(narrow[3]); !strings.HasPrefix(got, "-     fmt") {
		t.Fatalf("narrow gutter = %q", got)
	}
	for _, l := range narrow {
		if w := lipgloss.Width(l); w > 16 {
			t.Fatalf("narrow line %d cells: %q", w, stripANSI(l))
		}
	}
}

func TestRenderDiffWideAndControlCharacters(t *testing.T) {
	d := "@@ -1,1 +1,1 @@\n-世界世界世界世界世界世界世界世界世界世界世界世界\n+世界世界世界\x1b[31m red\tTab\r\n"
	for _, width := range []int{25, 26, 60} {
		for _, l := range renderDiff(d, width, 0) {
			if w := lipgloss.Width(l); w != width {
				t.Errorf("width %d: line is %d cells: %q", width, w, stripANSI(l))
			}
			if strings.ContainsAny(stripANSI(l), "\x1b\t\r") {
				t.Errorf("control characters leaked: %q", l)
			}
		}
	}
}

func TestRenderDiffHunkHeaderAndCollapse(t *testing.T) {
	var b strings.Builder
	b.WriteString("@@ -1,1 +1,1 @@ func a()\n-a\n+b\n@@ -40,0 +40,50 @@\n")
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&b, "+line %d\n", i)
	}
	lines := renderDiff(b.String(), 60, 10)
	if len(lines) != 11 {
		t.Fatalf("got %d lines, want 10 and a hint", len(lines))
	}
	head := stripANSI(lines[0])
	if !strings.HasPrefix(head, "@@ -1,1 +1,1 @@ func a() ─") || lipgloss.Width(lines[0]) != 60 {
		t.Fatalf("hunk header = %q", head)
	}
	if hint := stripANSI(lines[10]); hint != "… +44 diff lines" {
		t.Fatalf("hint = %q", hint)
	}
	// One row over the limit is shown rather than hidden behind a hint.
	if got := renderDiff("@@ -1,2 +1,2 @@\n-a\n-b\n+c\n+d\n", 60, 3); len(got) != 4 {
		t.Fatalf("one-over: %d lines", len(got))
	}
}

func TestRenderDiffWithoutHeaders(t *testing.T) {
	d := "--- big.txt\n+++ big.txt\n(10 → 20 lines; too large to diff)\n"
	lines := renderDiff(d, 60, 0)
	if len(lines) != 1 || stripANSI(lines[0]) != "(10 → 20 lines; too large to diff)" {
		t.Fatalf("lines = %q", lines)
	}
}
