package main

import "testing"

func TestSummaryMarkdown(t *testing.T) {
	in := "Autopilot done: x\nSteps: 1/1 completed\n  ✓ 1. Create\n  - extra\nTests: ok\n"
	want := "Autopilot done: x\n\nSteps: 1/1 completed\n- ✓ 1. Create\n- extra\n\nTests: ok"
	if got := summaryMarkdown(in); got != want {
		t.Fatalf("got %q", got)
	}
}
