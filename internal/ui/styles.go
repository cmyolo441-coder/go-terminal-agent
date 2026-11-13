package ui

import "github.com/charmbracelet/lipgloss"

// Palette: a violet-to-cyan accent on neutral greys, with adaptive variants
// so every element reads on both dark and light terminals.
var (
	cAccent  = lipgloss.AdaptiveColor{Light: "#6D28D9", Dark: "#A78BFA"}
	cAccent2 = lipgloss.AdaptiveColor{Light: "#0E7490", Dark: "#22D3EE"}
	cText    = lipgloss.AdaptiveColor{Light: "#1F2937", Dark: "#E5E7EB"}
	cMuted   = lipgloss.AdaptiveColor{Light: "#6B7280", Dark: "#8B8FA3"}
	cFaint   = lipgloss.AdaptiveColor{Light: "#9CA3AF", Dark: "#5B5F73"}
	cGreen   = lipgloss.AdaptiveColor{Light: "#15803D", Dark: "#4ADE80"}
	cRed     = lipgloss.AdaptiveColor{Light: "#B91C1C", Dark: "#F87171"}
	cAmber   = lipgloss.AdaptiveColor{Light: "#B45309", Dark: "#FBBF24"}
	cBlue    = lipgloss.AdaptiveColor{Light: "#1D4ED8", Dark: "#60A5FA"}
	cAddBg   = lipgloss.AdaptiveColor{Light: "#DCFCE7", Dark: "#13301F"}
	cDelBg   = lipgloss.AdaptiveColor{Light: "#FEE2E2", Dark: "#3A1A1D"}
)

var (
	sAccent   = lipgloss.NewStyle().Foreground(cAccent)
	sAccentB  = lipgloss.NewStyle().Foreground(cAccent).Bold(true)
	sAccent2  = lipgloss.NewStyle().Foreground(cAccent2)
	sText     = lipgloss.NewStyle().Foreground(cText)
	sBold     = lipgloss.NewStyle().Foreground(cText).Bold(true)
	sMuted    = lipgloss.NewStyle().Foreground(cMuted)
	sFaint    = lipgloss.NewStyle().Foreground(cFaint)
	sItalic   = lipgloss.NewStyle().Foreground(cMuted).Italic(true)
	sGreen    = lipgloss.NewStyle().Foreground(cGreen)
	sRed      = lipgloss.NewStyle().Foreground(cRed)
	sAmber    = lipgloss.NewStyle().Foreground(cAmber)
	sBlue     = lipgloss.NewStyle().Foreground(cBlue)
	sDiffAdd  = lipgloss.NewStyle().Foreground(cGreen).Background(cAddBg)
	sDiffDel  = lipgloss.NewStyle().Foreground(cRed).Background(cDelBg)
	sDiffHunk = lipgloss.NewStyle().Foreground(cAccent2)
	sUserBar  = lipgloss.NewStyle().Foreground(cAccent).Bold(true)
	sKey      = lipgloss.NewStyle().Foreground(cAccent2).Bold(true)
)

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

// spinnerFrames is a smooth eight-step orbit.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
