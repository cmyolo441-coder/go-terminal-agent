package ui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

// cmdTheme lists the themes with a swatch of each, or switches to one and
// prints a preview in it.
func cmdTheme(m *model, arg string) tea.Cmd {
	if arg == "" {
		m.emit(themeList(m.width))
		return nil
	}
	t, ok := lookupTheme(arg)
	if !ok {
		m.emit(renderError(fmt.Sprintf("Unknown theme %q. Choose one of %s.", arg, strings.Join(ThemeNames(), ", "))))
		return nil
	}
	if noColor() && t.Name != noColorTheme {
		m.emit(renderNotice("NO_COLOR is set, so colors stay off. Unset it to use the " + t.Name + " theme."))
		return nil
	}
	applyTheme(t)
	m.restyle()
	m.emit(renderInfo("Theme set to "+t.Name+" · "+t.Description) + "\n" + themePreview(m.width) + "\n" +
		sFaint.Render(fmt.Sprintf(`  This session only; set "theme": %q in your config to keep it.`, t.Name)))
	return nil
}

// themeList renders every theme with its color chips; the active one is
// marked.
func themeList(width int) string {
	var b strings.Builder
	b.WriteString(renderInfo("Theme: " + currentTheme.Name))
	for _, t := range themes {
		mark := "  "
		if t.Name == currentTheme.Name {
			mark = sAccent.Render(gPrompt) + " "
		}
		b.WriteString("\n" + mark + sText.Render(padRight(t.Name, 15)) + themeSwatches(t) + "  " + sMuted.Render(clip(t.Description, max(10, width-38))))
	}
	if noColor() {
		b.WriteString("\n" + sFaint.Render("  NO_COLOR is set, so colors are off."))
	}
	b.WriteString("\n" + sFaint.Render("  /theme <name> to switch and preview"))
	return b.String()
}

// previewDiff is the sample edit /theme shows.
const previewDiff = `--- main.go
+++ main.go
@@ -3,5 +3,5 @@ import "fmt"
 func main() {
-	fmt.Println("hello")
+	fmt.Println("hello, world")
 	os.Exit(0)
 }
`

// themePreview shows a tool result, a diff, Markdown and status colors in
// the active theme.
func themePreview(width int) string {
	call := &tools.Call{Tool: "edit_file", Kind: tools.KindWrite, Summary: "Edit main.go", Preview: previewDiff}
	parts := []string{
		renderToolDone(call, "updated main.go", nil, min(width, 72), densityCompact),
		"  " + sGreen.Render(gCheck+" passed") + sFaint.Render(" · ") + sAmber.Render("! warning") + sFaint.Render(" · ") +
			sRed.Render(gCross+" failed") + sFaint.Render(" · ") + sBlue.Render("⏸ plan") + sFaint.Render(" · ") + sAccent2.Render("/command"),
	}
	return strings.Join(parts, "\n")
}

// cmdDensity shows or sets how much tool output the transcript shows.
func cmdDensity(m *model, arg string) tea.Cmd {
	if arg == "" {
		m.emit(renderInfo("Density: " + m.density.name + " · " + densityHelp(m.density)))
		return nil
	}
	d, ok := parseDensity(arg)
	if !ok {
		m.emit(renderError(fmt.Sprintf("Unknown density %q. Choose %s.", arg, strings.Join(DensityNames(), " or "))))
		return nil
	}
	m.density = d
	m.emit(renderInfo("Density set to " + d.name + " · " + densityHelp(d)))
	return nil
}

func densityHelp(d density) string {
	if d.name == DensityExpanded {
		return fmt.Sprintf("tool output up to %d lines, diffs up to %d", d.bashTail, d.diffMax)
	}
	return fmt.Sprintf("long tool output collapses to %d lines with a +N lines hint", d.bashTail)
}
