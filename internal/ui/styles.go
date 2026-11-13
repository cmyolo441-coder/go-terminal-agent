package ui

// The palette and the styles built from it live in theme.go and are rebuilt
// by applyTheme.

// Glyphs.
const (
	gPrompt   = "❯"
	gBullet   = "●"
	gResult   = "⎿"
	gThink    = "✻"
	gDiamond  = "◆"
	gCheck    = "✓"
	gCross    = "✗"
	gTodo     = "☐"
	gTodoDone = "☒"
	gTodoNow  = "◼"
)

// spinnerFrames is a smooth braille orbit.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
