package ui

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

func numbered(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("line %d", i+1)
	}
	return out
}

func TestCollapse(t *testing.T) {
	cases := []struct {
		n, limit int
		fromEnd  bool
		first    string
		kept     int
		hidden   int
	}{
		{20, 6, false, "line 1", 6, 14},
		{20, 6, true, "line 15", 6, 14},
		{7, 6, false, "line 1", 7, 0}, // one over: shown whole
		{6, 6, false, "line 1", 6, 0},
		{3, 0, false, "line 1", 3, 0}, // no limit
	}
	for _, c := range cases {
		kept, hidden := collapse(numbered(c.n), c.limit, c.fromEnd)
		if len(kept) != c.kept || hidden != c.hidden || kept[0] != c.first {
			t.Errorf("collapse(%d lines, %d, %v) = %d kept from %q, %d hidden", c.n, c.limit, c.fromEnd, len(kept), kept[0], hidden)
		}
	}
}

func TestHeadAndTailHints(t *testing.T) {
	out := strings.Join(numbered(30), "\n")
	head := headLines(out, 6, 60)
	if len(head) != 7 || stripANSI(head[6]) != "… +24 lines" || stripANSI(head[0]) != "line 1" {
		t.Fatalf("headLines = %q", head)
	}
	tail := tailLines(out, 8, 60, false)
	if len(tail) != 9 || stripANSI(tail[0]) != "… +22 earlier lines" || stripANSI(tail[8]) != "line 30" {
		t.Fatalf("tailLines = %q", tail)
	}
	// ANSI colors in command output are dropped, not shown as garbage.
	colored := tailLines("\x1b[32mok\x1b[0m", 8, 60, false)
	if stripANSI(colored[0]) != "ok" || strings.Contains(colored[0], "␛") {
		t.Fatalf("colored output = %q", colored)
	}
}

func TestToolOutputDensity(t *testing.T) {
	call := &tools.Call{Tool: "bash", Command: "go test ./..."}
	out := strings.Join(numbered(30), "\n")
	compact := stripANSI(renderToolDone(call, out, nil, 80, densityCompact))
	if !strings.Contains(compact, "… +22 earlier lines") || strings.Contains(compact, "line 22\n") || !strings.Contains(compact, "line 30") {
		t.Fatalf("compact = %q", compact)
	}
	expanded := stripANSI(renderToolDone(call, out, nil, 80, densityExpanded))
	if strings.Contains(expanded, "earlier lines") || !strings.Contains(expanded, "line 1\n") || !strings.Contains(expanded, "line 30") {
		t.Fatalf("expanded = %q", expanded)
	}
	grep := &tools.Call{Tool: "grep", Summary: "Grep foo"}
	if g := stripANSI(renderToolDone(grep, out, nil, 80, densityCompact)); !strings.Contains(g, "… +24 lines") {
		t.Fatalf("compact grep = %q", g)
	}
	if g := stripANSI(renderToolDone(grep, out, nil, 80, densityExpanded)); strings.Contains(g, "+") {
		t.Fatalf("expanded grep = %q", g)
	}
}

func TestParseDensity(t *testing.T) {
	for in, want := range map[string]string{"": "focus", "minimal": "focus", "Compact": "compact", "expanded": "expanded", "full": "expanded"} {
		if d, ok := parseDensity(in); !ok || d.name != want {
			t.Errorf("parseDensity(%q) = %q, %v", in, d.name, ok)
		}
	}
	if _, ok := parseDensity("huge"); ok {
		t.Error("unknown density accepted")
	}
	if densityCompact.approvalRows(50) != 18 || densityExpanded.approvalRows(60) != 44 || densityExpanded.approvalRows(0) != 18 {
		t.Error("approval rows")
	}
}

func TestDensityCommand(t *testing.T) {
	m := newModel(Options{Density: "expanded"}, 80)
	if m.density.name != DensityExpanded || len(m.startNotes) != 0 {
		t.Fatalf("density = %q, notes %v", m.density.name, m.startNotes)
	}
	cmdDensity(m, "compact")
	if m.density.name != DensityCompact {
		t.Fatalf("density = %q", m.density.name)
	}
	cmdDensity(m, "bogus")
	if m.density.name != DensityCompact {
		t.Fatal("bad density changed the setting")
	}
	if bad := newModel(Options{Density: "bogus"}, 80); bad.density.name != DensityFocus || len(bad.startNotes) != 1 {
		t.Fatalf("bad option: %q %v", bad.density.name, bad.startNotes)
	}
}

func TestFocusDensityOneLine(t *testing.T) {
	ok := &tools.Call{Tool: "bash", Command: "python3 -m pip --version || true; pip3 --version || true; python3 -c 'import requests'"}
	got := stripANSI(renderToolDone(ok, "line1\nline2\nline3", nil, 60, densityFocus))
	if strings.Contains(got, "\n") || !strings.HasPrefix(got, "✓ python3 -m pip") || !strings.HasSuffix(got, "…") {
		t.Fatalf("focus bash = %q", got)
	}
	bad := stripANSI(renderToolDone(ok, "", errors.New("exit status 1"), 60, densityFocus))
	if !strings.HasPrefix(bad, "✗ ") || !strings.Contains(bad, "exit status 1") {
		t.Fatalf("focus failure = %q", bad)
	}
	edit := &tools.Call{Tool: "edit_file", Summary: "Edit main.go", Preview: "--- a\n+++ b\n@@\n-old\n+new\n+more\n"}
	if e := stripANSI(renderToolDone(edit, "updated", nil, 80, densityFocus)); !strings.Contains(e, "main.go") || !strings.Contains(e, "+2 −1") {
		t.Fatalf("focus edit = %q", e)
	}
	if densityFocus.next() != densityCompact || densityCompact.next() != densityExpanded || densityExpanded.next() != densityFocus {
		t.Fatal("ctrl+o cycle order")
	}
}
