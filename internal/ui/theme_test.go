package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/lipgloss"
)

// withColor renders with 24-bit color for the test (termenv.TrueColor is
// profile 0) and restores the profile and the theme afterwards.
func withColor(t *testing.T) {
	t.Helper()
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(0)
	t.Cleanup(func() {
		lipgloss.SetColorProfile(old)
		applyTheme(themes[0])
	})
}

// withTheme applies a theme for the test and restores the default after.
func withTheme(t *testing.T, name string) {
	t.Helper()
	th, ok := lookupTheme(name)
	if !ok {
		t.Fatalf("theme %q missing", name)
	}
	applyTheme(th)
	t.Cleanup(func() { applyTheme(themes[0]) })
}

func TestThemeRegistry(t *testing.T) {
	want := []string{"auto", "dark", "light", "high-contrast", "arctic", "solar", "mono"}
	got := ThemeNames()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ThemeNames() = %v, want %v", got, want)
	}
	seen := map[string]bool{}
	for _, th := range themes {
		if seen[th.Name] || th.Description == "" {
			t.Errorf("theme %q duplicated or undocumented", th.Name)
		}
		seen[th.Name] = true
		if lt, ok := lookupTheme(strings.ToUpper(th.Name)); !ok || lt.Name != th.Name {
			t.Errorf("lookupTheme(%q) failed", th.Name)
		}
	}
	for alias, name := range map[string]string{"": "auto", "default": "auto", "hc": "high-contrast", "no-color": "mono"} {
		if th, ok := lookupTheme(alias); !ok || th.Name != name {
			t.Errorf("lookupTheme(%q) = %q, want %q", alias, th.Name, name)
		}
	}
	if _, ok := lookupTheme("neon-unicorn"); ok {
		t.Error("unknown theme resolved")
	}
}

func TestResolveTheme(t *testing.T) {
	if th, err := resolveTheme("arctic", false); err != nil || th.Name != "arctic" {
		t.Fatalf("resolveTheme(arctic) = %q, %v", th.Name, err)
	}
	if th, err := resolveTheme("auto", false); err != nil || th.Name != "auto" {
		t.Fatalf("resolveTheme(auto) = %q, %v", th.Name, err)
	}
	if th, err := resolveTheme("bogus", false); err == nil || th.Name != "auto" {
		t.Fatalf("resolveTheme(bogus) = %q, %v; want auto and an error", th.Name, err)
	}
	// NO_COLOR wins over any configured theme.
	if th, err := resolveTheme("arctic", true); err != nil || th.Name != noColorTheme {
		t.Fatalf("NO_COLOR resolved to %q, %v", th.Name, err)
	}
}

func TestAutoThemeKeepsAdaptivePalette(t *testing.T) {
	th, _ := lookupTheme("auto")
	if _, ok := th.pal.Accent.color().(lipgloss.AdaptiveColor); !ok {
		t.Fatal("auto theme should adapt to the terminal background")
	}
	if !th.isDark(true) || th.isDark(false) {
		t.Fatal("auto theme should follow the detected background")
	}
	dark, _ := lookupTheme("dark")
	light, _ := lookupTheme("light")
	if !dark.isDark(false) || light.isDark(true) {
		t.Fatal("fixed themes must ignore the detected background")
	}
	if dark.pal.Accent.color() != lipgloss.Color(classic.Accent.dark) || light.pal.Accent.color() != lipgloss.Color(classic.Accent.light) {
		t.Fatal("dark and light themes should pin one side of the classic palette")
	}
}

func TestEveryThemeRenders(t *testing.T) {
	withColor(t)
	for _, th := range themes {
		applyTheme(th)
		out := sAccent.Render("x") + sDiffAdd.Render("+") + sDiffDelHi.Render("-")
		hasColor := strings.Contains(out, "\x1b[")
		if th.base == baseNone {
			if strings.Contains(out, "38;2") || strings.Contains(out, "48;2") {
				t.Errorf("%s: mono theme emitted colors: %q", th.Name, out)
			}
		} else if !hasColor {
			t.Errorf("%s: no colors rendered", th.Name)
		}
		md := &mdRenderer{}
		md.setTheme(th, true)
		if r := stripANSI(md.render("# Title\n\n- [link](https://example.com) and `code`", 60)); !strings.Contains(r, "Title") || !strings.Contains(r, "code") {
			t.Errorf("%s: markdown render = %q", th.Name, r)
		}
		if themeSwatches(th) == "" {
			t.Errorf("%s: empty swatches", th.Name)
		}
	}
}

func TestThemeSwitchChangesStyles(t *testing.T) {
	withColor(t)
	withTheme(t, "dark")
	dark := sAccent.Render("x")
	withTheme(t, "arctic")
	arctic := sAccent.Render("x")
	if dark == arctic {
		t.Fatal("switching theme did not change the accent color")
	}
}

func TestMarkdownTintDoesNotMutateStockStyles(t *testing.T) {
	before := *styles.DarkStyleConfig.Heading.Color
	th, _ := lookupTheme("arctic")
	cfg := th.mdStyle(true)
	if *cfg.Heading.Color == before {
		t.Fatal("arctic should tint headings")
	}
	if *styles.DarkStyleConfig.Heading.Color != before {
		t.Fatal("tinting changed glamour's shared dark style")
	}
	mono, _ := lookupTheme("mono")
	if mono.mdStyle(true).Heading.Color != nil {
		t.Fatal("mono markdown should carry no colors")
	}
}

func TestThemeCommand(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Cleanup(func() { applyTheme(themes[0]) })
	m := newModel(Options{}, 80)
	queued := func() string {
		m.pr.mu.Lock()
		defer m.pr.mu.Unlock()
		s := strings.Join(m.pr.queue, "\n")
		m.pr.queue = nil
		return stripANSI(s)
	}

	cmdTheme(m, "")
	if out := queued(); !strings.Contains(out, "arctic") || !strings.Contains(out, "high-contrast") {
		t.Fatalf("theme list = %q", out)
	}
	cmdTheme(m, "Solar")
	if currentTheme.Name != "solar" || m.md.theme.Name != "solar" {
		t.Fatalf("current theme = %q, markdown theme = %q", currentTheme.Name, m.md.theme.Name)
	}
	if out := queued(); !strings.Contains(out, "Theme set to solar") || !strings.Contains(out, "hello, world") {
		t.Fatalf("switch output = %q", out)
	}
	cmdTheme(m, "nope")
	if out := queued(); !strings.Contains(out, "Unknown theme") || currentTheme.Name != "solar" {
		t.Fatalf("bad name output = %q, theme %q", out, currentTheme.Name)
	}
	t.Setenv("NO_COLOR", "1")
	cmdTheme(m, "arctic")
	if out := queued(); !strings.Contains(out, "NO_COLOR") || currentTheme.Name != "solar" {
		t.Fatalf("NO_COLOR output = %q, theme %q", out, currentTheme.Name)
	}
	if _, ok := findCommand("theme"); !ok {
		t.Fatal("/theme is not registered")
	}
}
