package autopilot

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
)

// ErrNoTestCommand is returned when no test command could be detected.
var ErrNoTestCommand = errors.New("no test command detected (looked for go.mod, Cargo.toml, package.json, pytest config, pom.xml, Gradle and a Makefile test target); pass one explicitly")

// TestCommand is a shell command that runs a project's tests.
type TestCommand struct {
	// Command is run by the shell in the workspace root (or Dir).
	Command string `json:"command"`
	// Framework selects the output parser. Empty means guess from Command.
	Framework Framework `json:"framework,omitempty"`
	// Dir is an optional working directory relative to the workspace root.
	Dir string `json:"dir,omitempty"`
	// Source says why the command was chosen, e.g. "go.mod".
	Source string `json:"source,omitempty"`
}

func (c TestCommand) String() string {
	if c.Dir != "" && c.Dir != "." {
		return c.Command + " (in " + c.Dir + ")"
	}
	return c.Command
}

// npmDefaultTest is the placeholder script `npm init` writes.
const npmDefaultTest = `echo "Error: no test specified" && exit 1`

var makeTestTargetRe = regexp.MustCompile(`(?m)^test\s*:([^=]|$)`)

// DetectTestCommand returns the most likely test command for root.
func DetectTestCommand(root string) (TestCommand, error) {
	all := DetectTestCommands(root)
	if len(all) == 0 {
		return TestCommand{}, ErrNoTestCommand
	}
	return all[0], nil
}

// DetectTestCommands returns every plausible test command for root, most
// likely first. Language-native commands come before a Makefile target
// because their output can be parsed precisely.
func DetectTestCommands(root string) []TestCommand {
	var out []TestCommand
	has := func(name string) bool {
		_, err := os.Stat(filepath.Join(root, name))
		return err == nil
	}
	if has("go.mod") {
		out = append(out, TestCommand{Command: "go test ./...", Framework: FrameworkGo, Source: "go.mod"})
	}
	if has("Cargo.toml") {
		out = append(out, TestCommand{Command: "cargo test", Framework: FrameworkCargo, Source: "Cargo.toml"})
	}
	if c, ok := detectNode(root); ok {
		out = append(out, c)
	}
	if c, ok := detectPython(root); ok {
		out = append(out, c)
	}
	if has("pom.xml") {
		cmd := "mvn -q test"
		if has("mvnw") && runtime.GOOS != "windows" {
			cmd = "./mvnw -q test"
		}
		out = append(out, TestCommand{Command: cmd, Framework: FrameworkMaven, Source: "pom.xml"})
	}
	for _, f := range []string{"build.gradle", "build.gradle.kts", "settings.gradle", "settings.gradle.kts"} {
		if has(f) {
			cmd := "gradle test"
			if has("gradlew") && runtime.GOOS != "windows" {
				cmd = "./gradlew test"
			}
			out = append(out, TestCommand{Command: cmd, Framework: FrameworkGradle, Source: f})
			break
		}
	}
	for _, f := range []string{"Makefile", "makefile", "GNUmakefile"} {
		data, err := os.ReadFile(filepath.Join(root, f))
		if err == nil && makeTestTargetRe.Match(data) {
			out = append(out, TestCommand{Command: "make test", Framework: FrameworkAuto, Source: f + " (test target)"})
			break
		}
	}
	return out
}

type packageJSON struct {
	Scripts         map[string]string `json:"scripts"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
	PackageManager  string            `json:"packageManager"`
}

func readPackageJSON(root string) (*packageJSON, bool) {
	data, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return nil, false
	}
	var p packageJSON
	if json.Unmarshal(data, &p) != nil {
		return nil, false
	}
	return &p, true
}

// detectNode picks the package manager from packageManager or the lockfile
// and the framework from the test script and dependencies.
func detectNode(root string) (TestCommand, bool) {
	pkg, ok := readPackageJSON(root)
	if !ok {
		return TestCommand{}, false
	}
	script := strings.TrimSpace(pkg.Scripts["test"])
	if script == "" || script == npmDefaultTest {
		return TestCommand{}, false
	}
	pm := nodePackageManager(root, pkg)
	cmd := pm + " test"
	if pm == "bun" {
		// "bun test" runs Bun's own runner; the script is what the project means.
		cmd = "bun run test"
	}
	return TestCommand{Command: cmd, Framework: jsFramework(script, pkg), Source: "package.json scripts.test"}, true
}

func nodePackageManager(root string, pkg *packageJSON) string {
	if pm, _, _ := strings.Cut(pkg.PackageManager, "@"); pm != "" {
		switch pm {
		case "npm", "pnpm", "yarn", "bun":
			return pm
		}
	}
	for _, c := range []struct{ file, pm string }{
		{"pnpm-lock.yaml", "pnpm"}, {"yarn.lock", "yarn"}, {"bun.lockb", "bun"}, {"bun.lock", "bun"}, {"package-lock.json", "npm"},
	} {
		if _, err := os.Stat(filepath.Join(root, c.file)); err == nil {
			return c.pm
		}
	}
	return "npm"
}

func jsFramework(script string, pkg *packageJSON) Framework {
	s := strings.ToLower(script)
	switch {
	case strings.Contains(s, "vitest"):
		return FrameworkVitest
	case strings.Contains(s, "jest"):
		return FrameworkJest
	}
	if pkg != nil {
		for _, deps := range []map[string]string{pkg.DevDependencies, pkg.Dependencies} {
			if _, ok := deps["vitest"]; ok {
				return FrameworkVitest
			}
			if _, ok := deps["jest"]; ok {
				return FrameworkJest
			}
		}
	}
	return FrameworkAuto
}

// detectPython looks for pytest configuration or test files.
func detectPython(root string) (TestCommand, bool) {
	read := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(root, name))
		return string(b)
	}
	source := ""
	switch {
	case fileExists(filepath.Join(root, "pytest.ini")):
		source = "pytest.ini"
	case strings.Contains(read("pyproject.toml"), "[tool.pytest"):
		source = "pyproject.toml [tool.pytest]"
	case strings.Contains(read("setup.cfg"), "[tool:pytest]"):
		source = "setup.cfg [tool:pytest]"
	case strings.Contains(read("tox.ini"), "[pytest]"):
		source = "tox.ini [pytest]"
	case fileExists(filepath.Join(root, "conftest.py")):
		source = "conftest.py"
	case hasPythonTests(root):
		source = "python test files"
	default:
		return TestCommand{}, false
	}
	return TestCommand{Command: pytestCommand(), Framework: FrameworkPytest, Source: source}, true
}

func hasPythonTests(root string) bool {
	for _, dir := range []string{root, filepath.Join(root, "tests"), filepath.Join(root, "test")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			n := e.Name()
			if !e.IsDir() && strings.HasSuffix(n, ".py") && (strings.HasPrefix(n, "test_") || strings.HasSuffix(n, "_test.py")) {
				return true
			}
		}
	}
	return false
}

func pytestCommand() string {
	if _, err := exec.LookPath("pytest"); err == nil {
		return "pytest"
	}
	if _, err := exec.LookPath("python3"); err == nil {
		return "python3 -m pytest"
	}
	return "python -m pytest"
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// GuessFramework infers the parser for a user-supplied command. root is
// consulted for package scripts (npm test and friends).
func GuessFramework(command, root string) Framework {
	c := strings.ToLower(command)
	words := strings.FieldsFunc(c, func(r rune) bool {
		return r == ' ' || r == '\t' || r == ';' || r == '&' || r == '|' || r == '(' || r == ')'
	})
	switch {
	case hasCommand(words, "go", "test"):
		return FrameworkGo
	case hasCommand(words, "cargo", "test"), hasCommand(words, "cargo", "nextest"):
		return FrameworkCargo
	case strings.Contains(c, "pytest"):
		return FrameworkPytest
	case strings.Contains(c, "vitest"):
		return FrameworkVitest
	case strings.Contains(c, "jest"):
		return FrameworkJest
	case hasCommand(words, "mvn", ""), hasCommand(words, "mvnw", ""):
		return FrameworkMaven
	case hasCommand(words, "gradle", ""), hasCommand(words, "gradlew", ""):
		return FrameworkGradle
	}
	for _, pm := range []string{"npm", "pnpm", "yarn", "bun"} {
		if hasCommand(words, pm, "test") || hasCommand(words, pm, "t") || (hasCommand(words, pm, "run") && slices.Contains(words, "test")) {
			if pkg, ok := readPackageJSON(root); ok {
				return jsFramework(pkg.Scripts["test"], pkg)
			}
		}
	}
	return FrameworkAuto
}

// hasCommand reports whether words contain program (by base name) followed
// by sub, or just program when sub is empty.
func hasCommand(words []string, program, sub string) bool {
	for i, w := range words {
		base := w[strings.LastIndexAny(w, "/\\")+1:]
		base = strings.TrimSuffix(strings.TrimSuffix(base, ".cmd"), ".bat")
		if base != program {
			continue
		}
		if sub == "" || (i+1 < len(words) && words[i+1] == sub) {
			return true
		}
	}
	return false
}
