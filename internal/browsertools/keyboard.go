package browsertools

import (
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// modifierKeys maps accepted modifier spellings to DOM key values.
var modifierKeys = map[string]string{
	"control": "Control", "ctrl": "Control", "ctl": "Control",
	"shift": "Shift",
	"alt":   "Alt", "option": "Alt", "opt": "Alt",
	"meta": "Meta", "cmd": "Meta", "command": "Meta", "super": "Meta", "win": "Meta",
}

// namedKeys maps accepted key spellings (lower case) to DOM key values.
var namedKeys = map[string]string{
	"enter": "Enter", "return": "Enter",
	"tab":    "Tab",
	"escape": "Escape", "esc": "Escape",
	"backspace": "Backspace",
	"delete":    "Delete", "del": "Delete",
	"insert": "Insert", "ins": "Insert",
	"space": " ", "spacebar": " ",
	"arrowup": "ArrowUp", "up": "ArrowUp",
	"arrowdown": "ArrowDown", "down": "ArrowDown",
	"arrowleft": "ArrowLeft", "left": "ArrowLeft",
	"arrowright": "ArrowRight", "right": "ArrowRight",
	"home": "Home", "end": "End",
	"pageup": "PageUp", "pgup": "PageUp",
	"pagedown": "PageDown", "pgdn": "PageDown",
	"contextmenu": "ContextMenu", "menu": "ContextMenu",
	"capslock": "CapsLock",
}

func init() {
	for i := 1; i <= 12; i++ {
		namedKeys[fmt.Sprintf("f%d", i)] = fmt.Sprintf("F%d", i)
	}
}

// Chord is a key press with modifiers held, e.g. Control+Shift+K.
type Chord struct {
	Modifiers []string // DOM key values, in press order
	Key       string   // DOM key value
}

// String renders the chord as Mod+Mod+Key.
func (c Chord) String() string {
	k := c.Key
	if k == " " {
		k = "Space"
	}
	return strings.Join(append(append([]string(nil), c.Modifiers...), k), "+")
}

// ParseChord parses a key or chord such as "Enter", "a", "Control+A",
// "Shift+Tab" or "Meta+Shift+K". Names are case-insensitive; a single
// character stands for itself ("+" is written "Plus"). With a modifier
// other than Shift, a letter key is lower-cased so Control+A selects all.
func ParseChord(s string) (Chord, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Chord{}, fmt.Errorf("empty key")
	}
	if s == "+" {
		return Chord{Key: "+"}, nil
	}
	parts := strings.Split(s, "+")
	// "Control++" means Control and the plus key.
	if strings.HasSuffix(s, "++") {
		parts = append(strings.Split(strings.TrimSuffix(s, "++"), "+"), "+")
	}
	var c Chord
	seen := map[string]bool{}
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return Chord{}, fmt.Errorf("invalid key %q: empty part", s)
		}
		last := i == len(parts)-1
		if m, ok := modifierKeys[strings.ToLower(p)]; ok && !last {
			if seen[m] {
				return Chord{}, fmt.Errorf("invalid key %q: %s repeated", s, m)
			}
			seen[m] = true
			c.Modifiers = append(c.Modifiers, m)
			continue
		}
		if !last {
			return Chord{}, fmt.Errorf("invalid key %q: %q is not a modifier (use Control, Shift, Alt or Meta)", s, p)
		}
		k, err := normalizeKey(p)
		if err != nil {
			return Chord{}, fmt.Errorf("invalid key %q: %w", s, err)
		}
		c.Key = k
	}
	if utf8.RuneCountInString(c.Key) == 1 && len(c.Modifiers) > 0 {
		r, _ := utf8.DecodeRuneInString(c.Key)
		if unicode.IsLetter(r) {
			if seen["Shift"] {
				c.Key = string(unicode.ToUpper(r))
			} else {
				c.Key = string(unicode.ToLower(r))
			}
		}
	}
	return c, nil
}

func normalizeKey(p string) (string, error) {
	if m, ok := modifierKeys[strings.ToLower(p)]; ok {
		return m, nil // a lone modifier press
	}
	if k, ok := namedKeys[strings.ToLower(p)]; ok {
		return k, nil
	}
	if strings.EqualFold(p, "plus") {
		return "+", nil
	}
	if utf8.RuneCountInString(p) == 1 {
		r, _ := utf8.DecodeRuneInString(p)
		if unicode.IsPrint(r) {
			return p, nil
		}
	}
	return "", fmt.Errorf("unknown key %q (use names like Enter, Tab, Escape, Backspace, ArrowDown, PageDown, F5, or a single character)", p)
}

// TypingDelays returns the pause before each rune of text for a typist at
// wpm words (five characters) per minute: log-normal variation around the
// mean interval, longer after word and sentence boundaries and now and
// then a short hesitation. The first rune has no delay.
func TypingDelays(rng *rand.Rand, text string, wpm int) []time.Duration {
	if wpm <= 0 {
		wpm = DefaultWPM
	}
	base := float64(time.Minute) / float64(wpm*5)
	const sigma = 0.35
	out := make([]time.Duration, 0, utf8.RuneCountInString(text))
	var prev rune
	for i, r := range []rune(text) {
		if i == 0 {
			out = append(out, 0)
			prev = r
			continue
		}
		// Log-normal with mean 1.
		f := math.Exp(rng.NormFloat64()*sigma - sigma*sigma/2)
		d := base * f
		switch {
		case prev == '.' || prev == '!' || prev == '?' || prev == '\n':
			d += base * (1 + rng.Float64())
		case prev == ',' || prev == ';' || prev == ':':
			d += base * 0.6 * rng.Float64()
		case unicode.IsSpace(prev):
			d += base * 0.4 * rng.Float64()
		}
		if rng.Float64() < 0.03 {
			d += base * (2 + 2*rng.Float64())
		}
		d = math.Min(math.Max(d, base*0.3), base*8)
		out = append(out, time.Duration(d))
		prev = r
	}
	return out
}
