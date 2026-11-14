package browser

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"
)

// Modifier bits for CDP Input events.
const (
	ModAlt     = 1
	ModControl = 2
	ModMeta    = 4
	ModShift   = 8
)

// Key locations (KeyboardEvent.location).
const (
	locStandard = 0
	locLeft     = 1
	locRight    = 2
	locNumpad   = 3
)

// keyDefinition describes one key of a US keyboard layout, as needed by
// Input.dispatchKeyEvent. Shift* fields apply while Shift is held.
type keyDefinition struct {
	Key          string
	Code         string
	KeyCode      int
	Text         string
	ShiftKey     string
	ShiftText    string
	ShiftKeyCode int
	Location     int
}

// keyDefinitions maps DOM key values and physical key codes ("Enter",
// "a", "KeyA", "ArrowDown", "ShiftLeft", ...) to their definitions.
var keyDefinitions = buildKeyDefinitions()

func buildKeyDefinitions() map[string]keyDefinition {
	m := make(map[string]keyDefinition, 400)
	add := func(name string, d keyDefinition) { m[name] = d }

	// Letters: "KeyA" and "a" type a, "A" types A.
	for c := 'a'; c <= 'z'; c++ {
		upper := string(c - 'a' + 'A')
		d := keyDefinition{Key: string(c), Code: "Key" + upper, KeyCode: int(c - 'a' + 'A'), ShiftKey: upper}
		add("Key"+upper, d)
		add(string(c), d)
		add(upper, keyDefinition{Key: upper, Code: "Key" + upper, KeyCode: d.KeyCode})
	}

	// Digit row and its shifted symbols.
	digitShift := ")!@#$%^&*("
	for i := 0; i <= 9; i++ {
		digit := string(rune('0' + i))
		shifted := string(digitShift[i])
		d := keyDefinition{Key: digit, Code: "Digit" + digit, KeyCode: 48 + i, ShiftKey: shifted}
		add("Digit"+digit, d)
		add(digit, d)
		add(shifted, keyDefinition{Key: shifted, Code: "Digit" + digit, KeyCode: 48 + i})
	}

	// Punctuation keys: code, unshifted, shifted, keyCode.
	for _, p := range []struct {
		code          string
		key, shiftKey string
		keyCode       int
	}{
		{"Semicolon", ";", ":", 186},
		{"Equal", "=", "+", 187},
		{"Comma", ",", "<", 188},
		{"Minus", "-", "_", 189},
		{"Period", ".", ">", 190},
		{"Slash", "/", "?", 191},
		{"Backquote", "`", "~", 192},
		{"BracketLeft", "[", "{", 219},
		{"Backslash", `\`, "|", 220},
		{"BracketRight", "]", "}", 221},
		{"Quote", "'", `"`, 222},
	} {
		d := keyDefinition{Key: p.key, Code: p.code, KeyCode: p.keyCode, ShiftKey: p.shiftKey}
		add(p.code, d)
		add(p.key, d)
		add(p.shiftKey, keyDefinition{Key: p.shiftKey, Code: p.code, KeyCode: p.keyCode})
	}

	// Editing, whitespace and navigation keys.
	enter := keyDefinition{Key: "Enter", Code: "Enter", KeyCode: 13, Text: "\r"}
	add("Enter", enter)
	add("\r", enter)
	add("\n", enter)
	tab := keyDefinition{Key: "Tab", Code: "Tab", KeyCode: 9}
	add("Tab", tab)
	add("\t", tab)
	space := keyDefinition{Key: " ", Code: "Space", KeyCode: 32}
	add("Space", space)
	add(" ", space)
	for _, d := range []keyDefinition{
		{Key: "Backspace", Code: "Backspace", KeyCode: 8},
		{Key: "Delete", Code: "Delete", KeyCode: 46},
		{Key: "Escape", Code: "Escape", KeyCode: 27},
		{Key: "Insert", Code: "Insert", KeyCode: 45},
		{Key: "Home", Code: "Home", KeyCode: 36},
		{Key: "End", Code: "End", KeyCode: 35},
		{Key: "PageUp", Code: "PageUp", KeyCode: 33},
		{Key: "PageDown", Code: "PageDown", KeyCode: 34},
		{Key: "ArrowLeft", Code: "ArrowLeft", KeyCode: 37},
		{Key: "ArrowUp", Code: "ArrowUp", KeyCode: 38},
		{Key: "ArrowRight", Code: "ArrowRight", KeyCode: 39},
		{Key: "ArrowDown", Code: "ArrowDown", KeyCode: 40},
		{Key: "CapsLock", Code: "CapsLock", KeyCode: 20},
		{Key: "NumLock", Code: "NumLock", KeyCode: 144},
		{Key: "ScrollLock", Code: "ScrollLock", KeyCode: 145},
		{Key: "Pause", Code: "Pause", KeyCode: 19},
		{Key: "PrintScreen", Code: "PrintScreen", KeyCode: 44},
		{Key: "ContextMenu", Code: "ContextMenu", KeyCode: 93},
		{Key: "Help", Code: "Help", KeyCode: 6},
		{Key: "AudioVolumeMute", Code: "AudioVolumeMute", KeyCode: 173},
		{Key: "AudioVolumeDown", Code: "AudioVolumeDown", KeyCode: 174},
		{Key: "AudioVolumeUp", Code: "AudioVolumeUp", KeyCode: 175},
		{Key: "MediaTrackNext", Code: "MediaTrackNext", KeyCode: 176},
		{Key: "MediaTrackPrevious", Code: "MediaTrackPrevious", KeyCode: 177},
		{Key: "MediaStop", Code: "MediaStop", KeyCode: 178},
		{Key: "MediaPlayPause", Code: "MediaPlayPause", KeyCode: 179},
	} {
		add(d.Key, d)
	}
	add("Esc", m["Escape"])
	add("Up", m["ArrowUp"])
	add("Down", m["ArrowDown"])
	add("Left", m["ArrowLeft"])
	add("Right", m["ArrowRight"])
	add("Del", m["Delete"])

	// Function keys F1-F24.
	for i := 1; i <= 24; i++ {
		name := fmt.Sprintf("F%d", i)
		add(name, keyDefinition{Key: name, Code: name, KeyCode: 111 + i})
	}

	// Modifiers. The bare names press the left-hand key.
	for _, mod := range []struct {
		key     string
		keyCode int
	}{{"Shift", 16}, {"Control", 17}, {"Alt", 18}, {"Meta", 91}} {
		left := keyDefinition{Key: mod.key, Code: mod.key + "Left", KeyCode: mod.keyCode, Location: locLeft}
		rightCode := mod.keyCode
		if mod.key == "Meta" {
			rightCode = 92
		}
		right := keyDefinition{Key: mod.key, Code: mod.key + "Right", KeyCode: rightCode, Location: locRight}
		add(mod.key, left)
		add(mod.key+"Left", left)
		add(mod.key+"Right", right)
	}
	add("Ctrl", m["Control"])
	add("AltGraph", keyDefinition{Key: "AltGraph", Code: "AltRight", KeyCode: 225, Location: locRight})

	// Numeric keypad. Without NumLock semantics the digits type digits.
	for i := 0; i <= 9; i++ {
		digit := string(rune('0' + i))
		add("Numpad"+digit, keyDefinition{Key: digit, Code: "Numpad" + digit, KeyCode: 96 + i, Location: locNumpad})
	}
	for _, d := range []keyDefinition{
		{Key: "*", Code: "NumpadMultiply", KeyCode: 106, Location: locNumpad},
		{Key: "+", Code: "NumpadAdd", KeyCode: 107, Location: locNumpad},
		{Key: "-", Code: "NumpadSubtract", KeyCode: 109, Location: locNumpad},
		{Key: ".", Code: "NumpadDecimal", KeyCode: 110, Location: locNumpad},
		{Key: "/", Code: "NumpadDivide", KeyCode: 111, Location: locNumpad},
		{Key: "=", Code: "NumpadEqual", KeyCode: 187, Location: locNumpad},
		{Key: "Enter", Code: "NumpadEnter", KeyCode: 13, Text: "\r", Location: locNumpad},
	} {
		add(d.Code, d)
	}
	return m
}

// modifierBit returns the modifier mask bit for a modifier key value.
func modifierBit(key string) int {
	switch key {
	case "Alt":
		return ModAlt
	case "Control":
		return ModControl
	case "Meta":
		return ModMeta
	case "Shift":
		return ModShift
	}
	return 0
}

// keyDescription is a resolved key press under the current modifiers.
type keyDescription struct {
	Key      string
	Code     string
	KeyCode  int
	Text     string
	Location int
}

// describeKey resolves key under modifiers the way a US keyboard would:
// Shift selects the shifted key value, and Alt, Control or Meta suppress
// text so shortcuts do not type characters.
func describeKey(key string, modifiers int) (keyDescription, error) {
	def, ok := keyDefinitions[key]
	if !ok {
		if utf8.RuneCountInString(key) != 1 {
			return keyDescription{}, fmt.Errorf("browser: unknown key %q", key)
		}
		// Any other single character (é, €, ß ...) is sent as text with no
		// physical key, like an IME or layout-specific key would.
		def = keyDefinition{Key: key}
	}
	shift := modifiers&ModShift != 0
	d := keyDescription{Key: def.Key, Code: def.Code, KeyCode: def.KeyCode, Location: def.Location}
	if shift && def.ShiftKey != "" {
		d.Key = def.ShiftKey
	}
	if shift && def.ShiftKeyCode != 0 {
		d.KeyCode = def.ShiftKeyCode
	}
	if utf8.RuneCountInString(d.Key) == 1 {
		d.Text = d.Key
	}
	if def.Text != "" {
		d.Text = def.Text
	}
	if shift && def.ShiftText != "" {
		d.Text = def.ShiftText
	}
	if modifiers&^ModShift != 0 {
		d.Text = ""
	}
	return d, nil
}

// inputState is the keyboard and mouse state of a page, guarded by its
// own mutex so input is serialized independently of other page calls.
type inputState struct {
	mu        chanMutex
	modifiers int
	buttons   int
	pressed   map[string]bool // physical codes (or keys) held down
}

// chanMutex is a mutex whose Lock can be abandoned when ctx ends, so a
// stuck input call cannot block later callers that have deadlines.
type chanMutex struct{ ch chan struct{} }

func (m *chanMutex) lock(ctx context.Context) error {
	if m.ch == nil {
		panic("browser: chanMutex used before init")
	}
	select {
	case m.ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *chanMutex) unlock() { <-m.ch }

// Modifiers returns the bitmask of modifier keys currently held down
// through KeyDown (ModAlt, ModControl, ModMeta, ModShift).
func (p *Page) Modifiers() int {
	ctx := context.Background()
	_ = p.in.mu.lock(ctx)
	defer p.in.mu.unlock()
	return p.in.modifiers
}

// KeyDown presses key, given as a DOM key value ("Enter", "Tab",
// "Backspace", "ArrowDown", "a", "A", "!") or a physical code ("KeyA",
// "Digit1", "ShiftLeft"). Modifier keys ("Shift", "Control", "Alt",
// "Meta") are held until the matching KeyUp and apply to later key and
// mouse events. Printable keys produce text input unless Control, Alt or
// Meta is held.
func (p *Page) KeyDown(ctx context.Context, key string) error {
	if err := p.in.mu.lock(ctx); err != nil {
		return fmt.Errorf("browser: key down: %w", err)
	}
	defer p.in.mu.unlock()
	d, err := describeKey(key, p.in.modifiers)
	if err != nil {
		return err
	}
	id := d.Code
	if id == "" {
		id = d.Key
	}
	autoRepeat := p.in.pressed[id]
	mods := p.in.modifiers | modifierBit(d.Key)
	typ := "rawKeyDown"
	if d.Text != "" {
		typ = "keyDown"
	}
	params := map[string]any{
		"type":                  typ,
		"modifiers":             mods,
		"windowsVirtualKeyCode": d.KeyCode,
		"nativeVirtualKeyCode":  d.KeyCode,
		"code":                  d.Code,
		"key":                   d.Key,
		"text":                  d.Text,
		"unmodifiedText":        d.Text,
		"autoRepeat":            autoRepeat,
		"location":              d.Location,
		"isKeypad":              d.Location == locNumpad,
	}
	if cmds := editingCommands(runtime.GOOS, p.in.modifiers, d.Code); len(cmds) > 0 {
		params["commands"] = cmds
	}
	if err := p.call(ctx, "Input.dispatchKeyEvent", params, nil); err != nil {
		return fmt.Errorf("browser: key down %q: %w", key, err)
	}
	p.in.pressed[id] = true
	p.in.modifiers = mods
	return nil
}

// KeyUp releases a key pressed with KeyDown.
func (p *Page) KeyUp(ctx context.Context, key string) error {
	if err := p.in.mu.lock(ctx); err != nil {
		return fmt.Errorf("browser: key up: %w", err)
	}
	defer p.in.mu.unlock()
	d, err := describeKey(key, p.in.modifiers)
	if err != nil {
		return err
	}
	id := d.Code
	if id == "" {
		id = d.Key
	}
	mods := p.in.modifiers &^ modifierBit(d.Key)
	params := map[string]any{
		"type":                  "keyUp",
		"modifiers":             mods,
		"windowsVirtualKeyCode": d.KeyCode,
		"nativeVirtualKeyCode":  d.KeyCode,
		"code":                  d.Code,
		"key":                   d.Key,
		"location":              d.Location,
		"isKeypad":              d.Location == locNumpad,
	}
	if err := p.call(ctx, "Input.dispatchKeyEvent", params, nil); err != nil {
		return fmt.Errorf("browser: key up %q: %w", key, err)
	}
	delete(p.in.pressed, id)
	p.in.modifiers = mods
	return nil
}

// Mouse button bits for the "buttons" field of mouse events.
var mouseButtons = map[string]int{
	"none":    0,
	"left":    1,
	"right":   2,
	"middle":  4,
	"back":    8,
	"forward": 16,
}

func parseButton(button string) (string, int, error) {
	b := strings.ToLower(strings.TrimSpace(button))
	if b == "" {
		b = "left"
	}
	bit, ok := mouseButtons[b]
	if !ok {
		return "", 0, fmt.Errorf("browser: unknown mouse button %q", button)
	}
	return b, bit, nil
}

// pressedButtonName names one held button for mouseMoved events.
func pressedButtonName(buttons int) string {
	for _, name := range []string{"left", "right", "middle", "back", "forward"} {
		if buttons&mouseButtons[name] != 0 {
			return name
		}
	}
	return "none"
}

func (p *Page) dispatchMouse(ctx context.Context, params map[string]any) error {
	params["modifiers"] = p.in.modifiers
	params["pointerType"] = "mouse"
	return p.call(ctx, "Input.dispatchMouseEvent", params, nil)
}

// MouseMove moves the pointer to (x, y) in CSS pixels relative to the
// viewport, carrying any held buttons (a drag).
func (p *Page) MouseMove(ctx context.Context, x, y float64) error {
	if err := p.in.mu.lock(ctx); err != nil {
		return fmt.Errorf("browser: mouse move: %w", err)
	}
	defer p.in.mu.unlock()
	err := p.dispatchMouse(ctx, map[string]any{
		"type": "mouseMoved", "x": x, "y": y,
		"button": pressedButtonName(p.in.buttons), "buttons": p.in.buttons,
	})
	if err != nil {
		return fmt.Errorf("browser: mouse move: %w", err)
	}
	return nil
}

// MouseDown presses button ("left" by default, "right", "middle", "back",
// "forward") at (x, y). clickCount is 1 for a single click, 2 for the
// second press of a double click; values below 1 mean 1.
func (p *Page) MouseDown(ctx context.Context, x, y float64, button string, clickCount int) error {
	return p.mouseButton(ctx, "mousePressed", x, y, button, clickCount)
}

// MouseUp releases button at (x, y); see MouseDown.
func (p *Page) MouseUp(ctx context.Context, x, y float64, button string, clickCount int) error {
	return p.mouseButton(ctx, "mouseReleased", x, y, button, clickCount)
}

func (p *Page) mouseButton(ctx context.Context, typ string, x, y float64, button string, clickCount int) error {
	name, bit, err := parseButton(button)
	if err != nil {
		return err
	}
	if clickCount < 1 {
		clickCount = 1
	}
	if err := p.in.mu.lock(ctx); err != nil {
		return fmt.Errorf("browser: %s: %w", typ, err)
	}
	defer p.in.mu.unlock()
	buttons := p.in.buttons
	if typ == "mousePressed" {
		buttons |= bit
	} else {
		buttons &^= bit
	}
	err = p.dispatchMouse(ctx, map[string]any{
		"type": typ, "x": x, "y": y, "button": name,
		"buttons": buttons, "clickCount": clickCount,
	})
	if err != nil {
		return fmt.Errorf("browser: %s: %w", typ, err)
	}
	p.in.buttons = buttons
	return nil
}

// Wheel scrolls by (dx, dy) CSS pixels with the pointer at (x, y).
func (p *Page) Wheel(ctx context.Context, x, y, dx, dy float64) error {
	if err := p.in.mu.lock(ctx); err != nil {
		return fmt.Errorf("browser: wheel: %w", err)
	}
	defer p.in.mu.unlock()
	// Chrome acknowledges a wheel event only after the compositor has
	// handled it; headless Chrome on macOS sometimes never does. Bound the
	// wait and scroll from script instead, so callers never hang.
	wctx, cancel := context.WithTimeout(ctx, wheelAckTimeout)
	err := p.dispatchMouse(wctx, map[string]any{
		"type": "mouseWheel", "x": x, "y": y, "deltaX": dx, "deltaY": dy,
		"button": "none", "buttons": p.in.buttons,
	})
	timedOut := wctx.Err() != nil && ctx.Err() == nil
	cancel()
	if err != nil && timedOut {
		err = p.scrollFromScript(ctx, x, y, dx, dy)
	}
	if err != nil {
		return fmt.Errorf("browser: wheel: %w", err)
	}
	return nil
}

// wheelAckTimeout bounds how long Wheel waits for Chrome to acknowledge a
// wheel event before falling back to scrolling from script.
const wheelAckTimeout = 4 * time.Second

// scrollFromScript scrolls the innermost scrollable element under (x, y),
// or the page, by (dx, dy) — what the wheel event would have done.
func (p *Page) scrollFromScript(ctx context.Context, x, y, dx, dy float64) error {
	expr := fmt.Sprintf(`(() => {
  const dx = %g, dy = %g;
  const can = (n, axis) => {
    const s = getComputedStyle(n);
    return axis === "y"
      ? n.scrollHeight > n.clientHeight && /(auto|scroll|overlay)/.test(s.overflowY)
      : n.scrollWidth > n.clientWidth && /(auto|scroll|overlay)/.test(s.overflowX);
  };
  for (let n = document.elementFromPoint(%g, %g); n && n !== document.body && n !== document.documentElement; n = n.parentElement) {
    if ((dy && can(n, "y")) || (dx && can(n, "x"))) { n.scrollBy(dx, dy); return true; }
  }
  window.scrollBy(dx, dy);
  return true;
})()`, dx, dy, x, y)
	_, err := p.Evaluate(ctx, expr)
	return err
}

// editingCommands returns the editor commands a shortcut must carry on
// macOS. There, synthesized key events never reach the native key
// bindings, so Cmd+A (or Ctrl+A, which agents use on every platform)
// would not select anything; Chrome runs the commands named in the event
// instead. Other platforms handle the shortcuts natively.
func editingCommands(goos string, mods int, code string) []string {
	if goos != "darwin" || mods&(ModMeta|ModControl) == 0 || mods&ModAlt != 0 {
		return nil
	}
	shift := mods&ModShift != 0
	switch code {
	case "KeyA":
		return []string{"selectAll"}
	case "KeyC":
		return []string{"copy"}
	case "KeyX":
		return []string{"cut"}
	case "KeyV":
		return []string{"paste"}
	case "KeyZ":
		if shift {
			return []string{"redo"}
		}
		return []string{"undo"}
	}
	return nil
}
