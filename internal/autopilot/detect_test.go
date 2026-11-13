package autopilot

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeFiles creates files under root; content "" makes an empty file.
func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDetectTestCommand(t *testing.T) {
	cases := []struct {
		name      string
		files     map[string]string
		command   string // substring
		framework Framework
	}{
		{"go", map[string]string{"go.mod": "module x\n"}, "go test ./...", FrameworkGo},
		{"cargo", map[string]string{"Cargo.toml": "[package]\n"}, "cargo test", FrameworkCargo},
		{"npm jest", map[string]string{"package.json": `{"scripts": {"test": "jest --ci"}}`}, "npm test", FrameworkJest},
		{"pnpm vitest", map[string]string{"package.json": `{"scripts": {"test": "vitest run"}}`, "pnpm-lock.yaml": ""}, "pnpm test", FrameworkVitest},
		{"yarn dep", map[string]string{"package.json": `{"scripts": {"test": "node scripts/test.js"}, "devDependencies": {"jest": "29"}}`, "yarn.lock": ""}, "yarn test", FrameworkJest},
		{"bun", map[string]string{"package.json": `{"scripts": {"test": "vitest"}}`, "bun.lockb": ""}, "bun run test", FrameworkVitest},
		{"packageManager field", map[string]string{"package.json": `{"packageManager": "pnpm@9.1.0", "scripts": {"test": "mocha"}}`}, "pnpm test", FrameworkAuto},
		{"pytest ini", map[string]string{"pytest.ini": "[pytest]\n"}, "pytest", FrameworkPytest},
		{"pyproject", map[string]string{"pyproject.toml": "[tool.pytest.ini_options]\n"}, "pytest", FrameworkPytest},
		{"python tests dir", map[string]string{"tests/test_app.py": ""}, "pytest", FrameworkPytest},
		{"maven", map[string]string{"pom.xml": "<project/>"}, "mvn -q test", FrameworkMaven},
		{"gradle kts", map[string]string{"build.gradle.kts": ""}, "gradle test", FrameworkGradle},
		{"make", map[string]string{"Makefile": "build:\n\tgo build\n\ntest: build\n\t./run-tests\n"}, "make test", FrameworkAuto},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			writeFiles(t, root, c.files)
			got, err := DetectTestCommand(root)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(got.Command, c.command) || got.Framework != c.framework || got.Source == "" {
				t.Fatalf("got %+v, want command containing %q and framework %q", got, c.command, c.framework)
			}
		})
	}
}

func TestDetectTestCommandWrappers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("wrapper scripts are not used on Windows")
	}
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"pom.xml": "<project/>", "mvnw": "#!/bin/sh\n"})
	if got, _ := DetectTestCommand(root); got.Command != "./mvnw -q test" {
		t.Fatalf("maven wrapper not preferred: %+v", got)
	}
	root = t.TempDir()
	writeFiles(t, root, map[string]string{"build.gradle": "", "gradlew": "#!/bin/sh\n"})
	if got, _ := DetectTestCommand(root); got.Command != "./gradlew test" {
		t.Fatalf("gradle wrapper not preferred: %+v", got)
	}
}

func TestDetectTestCommandsOrderAndSkips(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"go.mod":       "module x\n",
		"Makefile":     "test:\n\tgo test ./...\nTEST_FLAGS := -v\n",
		"package.json": `{"scripts": {"test": "echo \"Error: no test specified\" && exit 1"}}`,
	})
	all := DetectTestCommands(root)
	if len(all) != 2 || all[0].Framework != FrameworkGo || all[1].Command != "make test" {
		t.Fatalf("got %+v: want go first, then make, and the npm placeholder skipped", all)
	}

	root = t.TempDir()
	writeFiles(t, root, map[string]string{"Makefile": "TEST := 1\ntest-unit:\n\techo\n", "package.json": "{not json"})
	if _, err := DetectTestCommand(root); !errors.Is(err, ErrNoTestCommand) {
		t.Fatalf("want ErrNoTestCommand, got %v", err)
	}
}

func TestGuessFramework(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"package.json": `{"scripts": {"test": "vitest run"}}`})
	cases := map[string]Framework{
		"go test -race ./...":       FrameworkGo,
		"python -m pytest -x":       FrameworkPytest,
		"npx jest src":              FrameworkJest,
		"npx vitest run":            FrameworkVitest,
		"cargo test --workspace":    FrameworkCargo,
		"cargo nextest run":         FrameworkCargo,
		"./mvnw -q test":            FrameworkMaven,
		"./gradlew :app:test":       FrameworkGradle,
		"npm test":                  FrameworkVitest,
		"pnpm run test -- --silent": FrameworkVitest,
		"make check":                FrameworkAuto,
		"bash scripts/run_tests.sh": FrameworkAuto,
	}
	for cmd, want := range cases {
		if got := GuessFramework(cmd, root); got != want {
			t.Errorf("GuessFramework(%q) = %q, want %q", cmd, got, want)
		}
	}
}
