package browser

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// EnvBrowser names the environment variable that overrides browser
// discovery with an executable path (or a name looked up on PATH).
const EnvBrowser = "GTA_BROWSER"

// pathNames are the executable names tried on PATH, in order.
var pathNames = []string{
	"chromium", "chromium-browser", "google-chrome", "google-chrome-stable",
	"chrome", "msedge", "microsoft-edge", "microsoft-edge-stable",
}

// FindExecutable locates a Chromium-family browser: the GTA_BROWSER
// environment variable first, then standard install locations for the
// current OS (including Playwright's browser cache), then a PATH lookup of
// chromium, chromium-browser, google-chrome, chrome and msedge. It returns
// ErrNotFound when nothing usable exists.
func FindExecutable() (string, error) {
	return findExecutable(os.Getenv, runtime.GOOS, exec.LookPath)
}

func findExecutable(getenv func(string) string, goos string, lookPath func(string) (string, error)) (string, error) {
	if v := strings.TrimSpace(getenv(EnvBrowser)); v != "" {
		if p, err := resolveExecutable(v, lookPath); err == nil {
			return p, nil
		} else {
			return "", fmt.Errorf("browser: %s=%q: %w", EnvBrowser, v, err)
		}
	}
	for _, p := range candidatePaths(getenv, goos) {
		if isExecutableFile(p) {
			return p, nil
		}
	}
	for _, name := range pathNames {
		if p, err := lookPath(name); err == nil {
			return p, nil
		}
	}
	return "", ErrNotFound
}

// resolveExecutable accepts a path to an executable file or a bare name
// found on PATH.
func resolveExecutable(v string, lookPath func(string) (string, error)) (string, error) {
	if strings.ContainsRune(v, os.PathSeparator) || strings.ContainsRune(v, '/') {
		if !isExecutableFile(v) {
			return "", fmt.Errorf("not an executable file")
		}
		return v, nil
	}
	return lookPath(v)
}

func isExecutableFile(p string) bool {
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return fi.Mode().Perm()&0o111 != 0
}

// candidatePaths lists well-known install locations for goos, most
// preferred first.
func candidatePaths(getenv func(string) string, goos string) []string {
	home := getenv("HOME")
	var out []string
	switch goos {
	case "darwin":
		apps := []string{
			"Google Chrome.app/Contents/MacOS/Google Chrome",
			"Chromium.app/Contents/MacOS/Chromium",
			"Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary",
			"Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing",
			"Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"Brave Browser.app/Contents/MacOS/Brave Browser",
		}
		for _, a := range apps {
			out = append(out, filepath.Join("/Applications", a))
			if home != "" {
				out = append(out, filepath.Join(home, "Applications", a))
			}
		}
		out = append(out, playwrightPaths(getenv, goos, home)...)
	case "windows":
		var roots []string
		for _, env := range []string{"LOCALAPPDATA", "PROGRAMFILES", "PROGRAMFILES(X86)", "ProgramW6432"} {
			if v := getenv(env); v != "" && !slices.Contains(roots, v) {
				roots = append(roots, v)
			}
		}
		rels := []string{
			`Google\Chrome\Application\chrome.exe`,
			`Chromium\Application\chrome.exe`,
			`Google\Chrome SxS\Application\chrome.exe`,
			`Microsoft\Edge\Application\msedge.exe`,
			`BraveSoftware\Brave-Browser\Application\brave.exe`,
		}
		for _, rel := range rels {
			for _, root := range roots {
				out = append(out, root+`\`+rel)
			}
		}
		out = append(out, playwrightPaths(getenv, goos, home)...)
	default: // linux and the BSDs
		out = append(out,
			"/usr/bin/chromium",
			"/usr/bin/chromium-browser",
			"/usr/bin/google-chrome",
			"/usr/bin/google-chrome-stable",
			"/opt/google/chrome/chrome",
			"/usr/local/bin/chromium",
			"/usr/local/bin/chrome",
			"/snap/bin/chromium",
			"/usr/bin/microsoft-edge",
			"/usr/bin/microsoft-edge-stable",
			"/opt/microsoft/msedge/msedge",
		)
		out = append(out, playwrightPaths(getenv, goos, home)...)
	}
	return out
}

// playwrightPaths returns Chromium builds installed by Playwright, newest
// revision first, from PLAYWRIGHT_BROWSERS_PATH, /opt/pw-browsers and the
// per-user cache.
func playwrightPaths(getenv func(string) string, goos, home string) []string {
	var roots []string
	if v := getenv("PLAYWRIGHT_BROWSERS_PATH"); v != "" && v != "0" {
		roots = append(roots, v)
	}
	switch goos {
	case "darwin":
		if home != "" {
			roots = append(roots, filepath.Join(home, "Library", "Caches", "ms-playwright"))
		}
	case "windows":
		if v := getenv("LOCALAPPDATA"); v != "" {
			roots = append(roots, filepath.Join(v, "ms-playwright"))
		}
	default:
		roots = append(roots, "/opt/pw-browsers")
		if home != "" {
			roots = append(roots, filepath.Join(home, ".cache", "ms-playwright"))
		}
	}
	var rel []string
	switch goos {
	case "darwin":
		rel = []string{
			"chrome-mac*/Chromium.app/Contents/MacOS/Chromium",
			"chrome-mac*/Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing",
		}
	case "windows":
		rel = []string{"chrome-win*/chrome.exe"}
	default:
		rel = []string{"chrome-linux*/chrome"}
	}
	var out []string
	for _, root := range roots {
		dirs, _ := filepath.Glob(filepath.Join(root, "chromium-*"))
		// Newest revision first: chromium-1243 sorts after chromium-1200
		// only when compared numerically.
		slices.SortFunc(dirs, func(a, b string) int { return compareRevision(b, a) })
		for _, d := range dirs {
			for _, r := range rel {
				matches, _ := filepath.Glob(filepath.Join(d, r))
				out = append(out, matches...)
			}
		}
	}
	return out
}

// compareRevision orders "chromium-<n>" directory names by n.
func compareRevision(a, b string) int {
	ra, rb := revisionOf(a), revisionOf(b)
	if ra != rb {
		if ra < rb {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

func revisionOf(dir string) int {
	base := filepath.Base(dir)
	i := strings.LastIndexByte(base, '-')
	if i < 0 {
		return -1
	}
	n := 0
	for _, r := range base[i+1:] {
		if r < '0' || r > '9' {
			return -1
		}
		n = n*10 + int(r-'0')
	}
	return n
}
