package browsertools

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseChord(t *testing.T) {
	ok := []struct {
		in   string
		mods []string
		key  string
		str  string
	}{
		{"Enter", nil, "Enter", "Enter"},
		{"enter", nil, "Enter", "Enter"},
		{"esc", nil, "Escape", "Escape"},
		{"a", nil, "a", "a"},
		{"A", nil, "A", "A"},
		{"Control+A", []string{"Control"}, "a", "Control+a"},
		{"ctrl+shift+k", []string{"Control", "Shift"}, "K", "Control+Shift+K"},
		{"Cmd+K", []string{"Meta"}, "k", "Meta+k"},
		{"Shift+Tab", []string{"Shift"}, "Tab", "Shift+Tab"},
		{"Alt+ArrowDown", []string{"Alt"}, "ArrowDown", "Alt+ArrowDown"},
		{"PageDown", nil, "PageDown", "PageDown"},
		{"F5", nil, "F5", "F5"},
		{"space", nil, " ", "Space"},
		{"+", nil, "+", "+"},
		{"Control++", []string{"Control"}, "+", "Control++"},
		{"Control+Plus", []string{"Control"}, "+", "Control++"},
		{"Shift", nil, "Shift", "Shift"},
		{"Control+Shift", []string{"Control"}, "Shift", "Control+Shift"},
		{" Control + c ", []string{"Control"}, "c", "Control+c"},
		{"é", nil, "é", "é"},
	}
	for _, c := range ok {
		got, err := ParseChord(c.in)
		if err != nil {
			t.Errorf("ParseChord(%q): %v", c.in, err)
			continue
		}
		if !reflect.DeepEqual(got.Modifiers, c.mods) || got.Key != c.key {
			t.Errorf("ParseChord(%q) = %+v, want %v %q", c.in, got, c.mods, c.key)
		}
		if got.String() != c.str {
			t.Errorf("ParseChord(%q).String() = %q, want %q", c.in, got.String(), c.str)
		}
	}
	bad := []string{"", "  ", "Foo", "Control+", "Control+Control+A", "A+B", "Hyper+A", "Enterr", "\x01"}
	for _, in := range bad {
		if _, err := ParseChord(in); err == nil {
			t.Errorf("ParseChord(%q) should fail", in)
		}
	}
}

func TestTypingDelays(t *testing.T) {
	text := strings.Repeat("the quick brown fox jumps over the lazy dog. ", 20)
	d := TypingDelays(seeded(9), text, 60)
	if len(d) != len([]rune(text)) {
		t.Fatalf("got %d delays for %d runes", len(d), len([]rune(text)))
	}
	if d[0] != 0 {
		t.Fatalf("first delay = %v", d[0])
	}
	base := time.Minute / 300 // 60 wpm
	var sum time.Duration
	distinct := map[time.Duration]bool{}
	var afterPeriod, plain []time.Duration
	runes := []rune(text)
	for i, x := range d[1:] {
		if x < base*3/10 || x > base*8 {
			t.Fatalf("delay %v out of range for base %v", x, base)
		}
		sum += x
		distinct[x] = true
		switch runes[i] {
		case '.':
			afterPeriod = append(afterPeriod, x)
		case ' ':
		default:
			plain = append(plain, x)
		}
	}
	mean := sum / time.Duration(len(d)-1)
	if mean < base*8/10 || mean > base*16/10 {
		t.Fatalf("mean delay %v far from base %v", mean, base)
	}
	if len(distinct) < len(d)/2 {
		t.Fatal("cadence is not randomized")
	}
	if avg(afterPeriod) <= avg(plain) {
		t.Fatalf("no pause after sentences: %v vs %v", avg(afterPeriod), avg(plain))
	}
	// Deterministic for a seed; faster typing means shorter delays.
	if !reflect.DeepEqual(d, TypingDelays(seeded(9), text, 60)) {
		t.Fatal("same seed gave different cadence")
	}
	fast := TypingDelays(seeded(9), text, 120)
	if avg(fast[1:]) >= avg(d[1:]) {
		t.Fatal("higher WPM should type faster")
	}
	if len(TypingDelays(seeded(1), "", 60)) != 0 {
		t.Fatal("empty text should have no delays")
	}
}

func avg(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	var s time.Duration
	for _, d := range ds {
		s += d
	}
	return s / time.Duration(len(ds))
}
