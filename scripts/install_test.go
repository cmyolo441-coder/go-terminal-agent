package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeCurl answers the installer's "latest release" lookup with the URL in
// $FAKE_EFFECTIVE_URL and fails every download, so no network is used.
const fakeCurl = `#!/bin/sh
for a in "$@"; do
	case "$a" in
	*url_effective*) printf '%s' "$FAKE_EFFECTIVE_URL"; exit 0 ;;
	esac
done
exit 22
`

func runInstaller(t *testing.T, effectiveURL string, args ...string) (string, error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is a POSIX shell script")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(fakeCurl), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sh, append([]string{"install.sh"}, args...)...)
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_EFFECTIVE_URL="+effectiveURL,
		"GTA_INSTALL_DIR="+t.TempDir(),
		"GTA_VERSION=",
		"HOME="+t.TempDir(),
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestInstallNoReleaseYet(t *testing.T) {
	for _, u := range []string{
		"https://github.com/cmyolo441-coder/go-terminal-agent/releases",
		"https://github.com/cmyolo441-coder/go-terminal-agent/releases/",
	} {
		out, err := runInstaller(t, u)
		if err == nil {
			t.Fatalf("%s: installer succeeded with no release", u)
		}
		if !strings.Contains(out, "no release of cmyolo441-coder/go-terminal-agent has been published yet") ||
			!strings.Contains(out, "go install github.com/cmyolo441-coder/go-terminal-agent/cmd/gta@latest") {
			t.Fatalf("%s: unhelpful message:\n%s", u, out)
		}
		if strings.Contains(out, "not a release version") {
			t.Fatalf("%s: misparsed the redirect as a version:\n%s", u, out)
		}
	}
}

func TestInstallLatestTagIsUsed(t *testing.T) {
	out, err := runInstaller(t, "https://github.com/cmyolo441-coder/go-terminal-agent/releases/tag/v0.4.0")
	if err == nil {
		t.Fatal("fake download should fail")
	}
	if !strings.Contains(out, "downloading gta v0.4.0") || !strings.Contains(out, "does release v0.4.0 exist?") {
		t.Fatalf("latest tag not used:\n%s", out)
	}
}

func TestInstallRejectsBadVersion(t *testing.T) {
	out, err := runInstaller(t, "", "v1.x;rm")
	if err == nil || !strings.Contains(out, "not a release version") {
		t.Fatalf("bad version accepted:\n%s", out)
	}
}

func TestInstallUnknownRedirect(t *testing.T) {
	out, err := runInstaller(t, "https://github.com/login")
	if err == nil || !strings.Contains(out, "could not find the latest release") {
		t.Fatalf("unexpected redirect not reported:\n%s", out)
	}
}
