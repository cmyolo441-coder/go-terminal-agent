package ui

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/glamour/ansi"
	"github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/lipgloss"
)

// swatch is one palette color with a variant for light and dark terminal
// backgrounds. A swatch whose variants are equal is a fixed color.
type swatch struct{ light, dark string }

func fixed(hex string) swatch { return swatch{hex, hex} }

// color returns the lipgloss color: adaptive when the variants differ.
func (s swatch) color() lipgloss.TerminalColor {
	if s.light == s.dark {
		return lipgloss.Color(s.dark)
	}
	return lipgloss.AdaptiveColor{Light: s.light, Dark: s.dark}
}

// hex returns the variant for a dark or light background.
func (s swatch) hex(dark bool) string {
	if dark {
		return s.dark
	}
	return s.light
}

// palette is the full set of colors a theme defines.
type palette struct {
	Accent, Accent2         swatch
	Text, Muted, Faint      swatch
	Green, Red, Amber, Blue swatch
	// Diff line backgrounds, and the stronger backgrounds that mark the
	// changed words inside a changed line.
	AddBg, DelBg, AddHiBg, DelHiBg swatch
}

// themeBase says which background a theme is designed for, which also
// picks the Markdown base style.
type themeBase int

const (
	baseAuto themeBase = iota // follows the detected terminal background
	baseDark
	baseLight
	baseNone // no colors at all
)

// Theme is a named color scheme for the whole UI.
type Theme struct {
	Name        string
	Description string
	base        themeBase
	pal         palette
	// mdTint recolors headings, links and inline code in Markdown with the
	// palette; the stock themes keep glamour's own look.
	mdTint bool
}

// ThemeAuto is the default theme name: today's palette, adapting to the
// terminal background.
const ThemeAuto = "auto"

// noColorTheme is forced when NO_COLOR is set.
const noColorTheme = "mono"

// classic is the original violet-to-cyan palette.
var classic = palette{
	Accent:  swatch{"#6D28D9", "#A78BFA"},
	Accent2: swatch{"#0E7490", "#22D3EE"},
	Text:    swatch{"#1F2937", "#E5E7EB"},
	Muted:   swatch{"#6B7280", "#8B8FA3"},
	Faint:   swatch{"#9CA3AF", "#5B5F73"},
	Green:   swatch{"#15803D", "#4ADE80"},
	Red:     swatch{"#B91C1C", "#F87171"},
	Amber:   swatch{"#B45309", "#FBBF24"},
	Blue:    swatch{"#1D4ED8", "#60A5FA"},
	AddBg:   swatch{"#DCFCE7", "#13301F"},
	DelBg:   swatch{"#FEE2E2", "#3A1A1D"},
	AddHiBg: swatch{"#86EFAC", "#1F6B3A"},
	DelHiBg: swatch{"#FCA5A5", "#7A2A30"},
}

// only keeps one side of an adaptive palette, for the fixed dark and
// light themes.
func (p palette) only(dark bool) palette {
	pick := func(s swatch) swatch { return fixed(s.hex(dark)) }
	return palette{
		Accent: pick(p.Accent), Accent2: pick(p.Accent2),
		Text: pick(p.Text), Muted: pick(p.Muted), Faint: pick(p.Faint),
		Green: pick(p.Green), Red: pick(p.Red), Amber: pick(p.Amber), Blue: pick(p.Blue),
		AddBg: pick(p.AddBg), DelBg: pick(p.DelBg), AddHiBg: pick(p.AddHiBg), DelHiBg: pick(p.DelHiBg),
	}
}

// themes is the registry, in display order.
var themes = []Theme{
	{Name: ThemeAuto, Description: "violet and cyan, follows the terminal's light or dark background", base: baseAuto, pal: classic},
	{Name: "dark", Description: "violet and cyan tuned for dark backgrounds", base: baseDark, pal: classic.only(true)},
	{Name: "light", Description: "violet and cyan tuned for light backgrounds", base: baseLight, pal: classic.only(false)},
	{Name: "high-contrast", Description: "maximum legibility: pure text, saturated status colors", base: baseAuto, mdTint: true, pal: palette{
		Accent:  swatch{"#7A00B8", "#FF66FF"},
		Accent2: swatch{"#005A87", "#00FFFF"},
		Text:    swatch{"#000000", "#FFFFFF"},
		Muted:   swatch{"#262626", "#DADADA"},
		Faint:   swatch{"#4E4E4E", "#A8A8A8"},
		Green:   swatch{"#005F00", "#00FF5F"},
		Red:     swatch{"#AF0000", "#FF5F5F"},
		Amber:   swatch{"#7A4F00", "#FFFF00"},
		Blue:    swatch{"#0000AF", "#5FAFFF"},
		AddBg:   swatch{"#D7FFD7", "#003800"},
		DelBg:   swatch{"#FFD7D7", "#4A0000"},
		AddHiBg: swatch{"#87FF87", "#007A00"},
		DelHiBg: swatch{"#FF8787", "#990000"},
	}},
	{Name: "arctic", Description: "cool polar night: frosted blues on slate, soft aurora accents", base: baseDark, mdTint: true, pal: palette{
		Accent:  fixed("#88C0D0"),
		Accent2: fixed("#8FBCBB"),
		Text:    fixed("#E5E9F0"),
		Muted:   fixed("#A0A8B7"),
		Faint:   fixed("#616E88"),
		Green:   fixed("#A3BE8C"),
		Red:     fixed("#D08770"),
		Amber:   fixed("#EBCB8B"),
		Blue:    fixed("#81A1C1"),
		AddBg:   fixed("#2B3A34"),
		DelBg:   fixed("#3D2C31"),
		AddHiBg: fixed("#45604F"),
		DelHiBg: fixed("#6A4048"),
	}},
	{Name: "solar", Description: "warm low-glare tones with balanced contrast, light or dark", base: baseAuto, mdTint: true, pal: palette{
		Accent:  swatch{"#6C71C4", "#8D92E0"},
		Accent2: swatch{"#2AA198", "#2AA198"},
		Text:    swatch{"#3B4F56", "#C3CFCF"},
		Muted:   swatch{"#657B83", "#93A1A1"},
		Faint:   swatch{"#93A1A1", "#586E75"},
		Green:   swatch{"#738A00", "#859900"},
		Red:     swatch{"#C8312E", "#DC322F"},
		Amber:   swatch{"#A07800", "#B58900"},
		Blue:    swatch{"#1F78BE", "#268BD2"},
		AddBg:   swatch{"#EAEFD0", "#0E3A2C"},
		DelBg:   swatch{"#F7DFD6", "#3E1F22"},
		AddHiBg: swatch{"#CFDB97", "#1F6045"},
		DelHiBg: swatch{"#EFB5A3", "#6E2D2F"},
	}},
	{Name: noColorTheme, Description: "no colors, only bold and dim text (used when NO_COLOR is set)", base: baseNone},
}

// ThemeNames lists the theme names the config field "theme" accepts.
func ThemeNames() []string {
	out := make([]string, len(themes))
	for i, t := range themes {
		out[i] = t.Name
	}
	return out
}

// themeAliases accept a few natural spellings.
var themeAliases = map[string]string{
	"default": ThemeAuto, "hc": "high-contrast", "contrast": "high-contrast",
	"highcontrast": "high-contrast", "high_contrast": "high-contrast",
	"none": noColorTheme, "no-color": noColorTheme, "nocolor": noColorTheme, "plain": noColorTheme,
}

// lookupTheme finds a theme by name or alias, ignoring case.
func lookupTheme(name string) (Theme, bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		n = ThemeAuto
	}
	if a, ok := themeAliases[n]; ok {
		n = a
	}
	for _, t := range themes {
		if t.Name == n {
			return t, true
		}
	}
	return Theme{}, false
}

// noColor reports whether the user asked for no colors (https://no-color.org).
func noColor() bool { return os.Getenv("NO_COLOR") != "" }

// resolveTheme picks the theme for a configured name. NO_COLOR always wins.
// An unknown name falls back to auto and returns an error to show.
func resolveTheme(name string, noColor bool) (Theme, error) {
	if noColor {
		t, _ := lookupTheme(noColorTheme)
		return t, nil
	}
	t, ok := lookupTheme(name)
	if !ok {
		def, _ := lookupTheme(ThemeAuto)
		return def, fmt.Errorf("unknown theme %q; choose one of %s", name, strings.Join(ThemeNames(), ", "))
	}
	return t, nil
}

// isDark says whether the theme renders for a dark background, given what
// the terminal reported.
func (t Theme) isDark(termDark bool) bool {
	switch t.base {
	case baseDark:
		return true
	case baseLight:
		return false
	default:
		return termDark
	}
}

// mdStyle returns the glamour style configuration for the theme.
func (t Theme) mdStyle(termDark bool) ansi.StyleConfig {
	if t.base == baseNone {
		return styles.NoTTYStyleConfig
	}
	dark := t.isDark(termDark)
	cfg := styles.LightStyleConfig
	if dark {
		cfg = styles.DarkStyleConfig
	}
	if !t.mdTint {
		return cfg
	}
	// The struct copy is shallow, so only replace pointers, never write
	// through them: the stock configs are shared.
	p := t.pal
	str := func(s string) *string { return &s }
	cfg.Document.Color = str(p.Text.hex(dark))
	cfg.Heading.Color = str(p.Accent.hex(dark))
	cfg.H1.Color = str(onAccent(dark))
	cfg.H1.BackgroundColor = str(p.Accent.hex(dark))
	cfg.H6.Color = str(p.Muted.hex(dark))
	cfg.Link.Color = str(p.Accent2.hex(dark))
	cfg.LinkText.Color = str(p.Accent2.hex(dark))
	// Inline code reads as a chip: accent text on a faint background,
	// padded by a space on each side.
	cfg.Code.Color = str(p.Accent2.hex(dark))
	codeBg := "#E6EDF3"
	if dark {
		codeBg = "#243240"
	}
	cfg.Code.BackgroundColor = str(codeBg)
	cfg.Code.Prefix = " "
	cfg.Code.Suffix = " "
	cfg.Item.Color = str(p.Accent.hex(dark))
	cfg.Enumeration.Color = str(p.Accent.hex(dark))
	cfg.BlockQuote.Color = str(p.Muted.hex(dark))
	cfg.HorizontalRule.Color = str(p.Faint.hex(dark))
	return cfg
}

// onAccent is the text color drawn on an accent background.
func onAccent(dark bool) string {
	if dark {
		return "#111111"
	}
	return "#FFFFFF"
}

// current is the active theme; set only from the UI goroutine.
var (
	currentTheme = themes[0]
	termDark     = true
)

// Palette colors and the styles built from them. applyTheme rebuilds all
// of them; they are only read and written on the UI goroutine.
var (
	cAccent, cAccent2, cText, cMuted, cFaint lipgloss.TerminalColor
	cGreen, cRed, cAmber, cBlue              lipgloss.TerminalColor

	sAccent, sAccentB, sAccent2, sText, sBold lipgloss.Style
	sMuted, sFaint, sItalic                   lipgloss.Style
	sGreen, sRed, sAmber, sBlue               lipgloss.Style
	sUserBar, sKey                            lipgloss.Style
	// sChip is a small label on a coloured background (the session title).
	sChip lipgloss.Style

	sDiffAdd, sDiffDel, sDiffAddHi, sDiffDelHi lipgloss.Style
	sDiffAddNum, sDiffDelNum, sDiffHunk        lipgloss.Style
)

func init() { applyTheme(themes[0]) }

// applyTheme makes t the active theme for everything rendered afterwards.
func applyTheme(t Theme) {
	currentTheme = t
	ns := lipgloss.NewStyle
	if t.base == baseNone {
		none := lipgloss.NoColor{}
		cAccent, cAccent2, cText, cMuted, cFaint = none, none, none, none, none
		cGreen, cRed, cAmber, cBlue = none, none, none, none
		sAccent, sAccentB, sAccent2 = ns(), ns().Bold(true), ns()
		sText, sBold = ns(), ns().Bold(true)
		sMuted, sFaint, sItalic = ns(), ns().Faint(true), ns().Italic(true)
		sGreen, sRed, sAmber, sBlue = ns(), ns(), ns(), ns()
		sUserBar, sKey = ns().Bold(true), ns().Bold(true)
		sChip = ns().Reverse(true)
		sDiffAdd, sDiffDel = ns(), ns().Faint(true)
		sDiffAddHi, sDiffDelHi = ns().Reverse(true), ns().Reverse(true)
		sDiffAddNum, sDiffDelNum = ns().Faint(true), ns().Faint(true)
		sDiffHunk = ns().Bold(true)
		return
	}
	p := t.pal
	cAccent, cAccent2 = p.Accent.color(), p.Accent2.color()
	cText, cMuted, cFaint = p.Text.color(), p.Muted.color(), p.Faint.color()
	cGreen, cRed, cAmber, cBlue = p.Green.color(), p.Red.color(), p.Amber.color(), p.Blue.color()
	addBg, delBg := p.AddBg.color(), p.DelBg.color()

	sAccent = ns().Foreground(cAccent)
	sAccentB = ns().Foreground(cAccent).Bold(true)
	sAccent2 = ns().Foreground(cAccent2)
	sText = ns().Foreground(cText)
	sBold = ns().Foreground(cText).Bold(true)
	sMuted = ns().Foreground(cMuted)
	sFaint = ns().Foreground(cFaint)
	sItalic = ns().Foreground(cMuted).Italic(true)
	sGreen = ns().Foreground(cGreen)
	sRed = ns().Foreground(cRed)
	sAmber = ns().Foreground(cAmber)
	sBlue = ns().Foreground(cBlue)
	sUserBar = ns().Foreground(cAccent).Bold(true)
	sKey = ns().Foreground(cAccent2).Bold(true)
	sChip = ns().Foreground(cText).Background(cFaint)
	sDiffAdd = ns().Foreground(cGreen).Background(addBg)
	sDiffDel = ns().Foreground(cRed).Background(delBg)
	sDiffAddHi = ns().Foreground(cText).Background(p.AddHiBg.color()).Bold(true)
	sDiffDelHi = ns().Foreground(cText).Background(p.DelHiBg.color()).Bold(true)
	sDiffAddNum = ns().Foreground(cFaint).Background(addBg)
	sDiffDelNum = ns().Foreground(cFaint).Background(delBg)
	sDiffHunk = ns().Foreground(cAccent2)
}

// themeSwatches renders a row of color chips in t's own colors, for /theme.
func themeSwatches(t Theme) string {
	if t.base == baseNone {
		return lipgloss.NewStyle().Bold(true).Render("Aa") + " " + lipgloss.NewStyle().Faint(true).Render("Aa")
	}
	dark := t.isDark(termDark)
	chip := func(s swatch) string {
		return lipgloss.NewStyle().Foreground(lipgloss.Color(s.hex(dark))).Render("██")
	}
	p := t.pal
	return chip(p.Accent) + chip(p.Accent2) + chip(p.Text) + chip(p.Muted) + chip(p.Green) + chip(p.Red) + chip(p.Amber) + chip(p.Blue)
}
