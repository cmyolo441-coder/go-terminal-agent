package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/version"
)

// setBuild sets the ldflags-injected values and the toolchain build info for
// one test.
func setBuild(t *testing.T, ver, commit, date string, bi *debug.BuildInfo) {
	t.Helper()
	oldV, oldC, oldD, oldRead := version.Version, version.Commit, version.Date, readBuildInfo
	t.Cleanup(func() {
		version.Version, version.Commit, version.Date, readBuildInfo = oldV, oldC, oldD, oldRead
	})
	version.Version, version.Commit, version.Date = ver, commit, date
	readBuildInfo = func() (*debug.BuildInfo, bool) { return bi, bi != nil }
}

func TestVersionStringReleaseBuild(t *testing.T) {
	setBuild(t, "1.2.3", "abc1234", "2026-11-13T10:00:00Z", &debug.BuildInfo{
		Main:     debug.Module{Version: "v9.9.9"},
		Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "ffff"}, {Key: "vcs.modified", Value: "true"}},
	})
	got := versionString()
	for _, want := range []string{
		"gta 1.2.3\n",
		"commit:  abc1234\n",
		"built:   2026-11-13T10:00:00Z\n",
		"go:      " + runtime.Version() + "\n",
		"os/arch: " + runtime.GOOS + "/" + runtime.GOARCH,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("versionString() = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "ffff") || strings.Contains(got, "9.9.9") || strings.Contains(got, "dirty") {
		t.Errorf("ldflags values must win over toolchain build info: %q", got)
	}
}

func TestVersionFallsBackToToolchainInfo(t *testing.T) {
	setBuild(t, "0.1.0-dev", unsetCommit, unsetDate, &debug.BuildInfo{
		Main: debug.Module{Version: "v0.4.0"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "0123456789abcdef"},
			{Key: "vcs.time", Value: "2026-11-01T00:00:00Z"},
			{Key: "vcs.modified", Value: "true"},
		},
	})
	b := currentBuildInfo()
	if b.Version != "0.4.0" || b.Commit != "0123456789abcdef-dirty" || b.Date != "2026-11-01T00:00:00Z" {
		t.Fatalf("got %+v", b)
	}
}

func TestVersionDevelBuildKeepsDefaultVersion(t *testing.T) {
	setBuild(t, "0.1.0-dev", unsetCommit, unsetDate, &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}})
	if b := currentBuildInfo(); b.Version != "0.1.0-dev" || b.Commit != unsetCommit || b.Date != unsetDate {
		t.Fatalf("got %+v", b)
	}
	setBuild(t, "0.1.0-dev", unsetCommit, unsetDate, nil)
	if b := currentBuildInfo(); b.Version != "0.1.0-dev" {
		t.Fatalf("no build info: got %+v", b)
	}
	// go build in a checkout stamps a pseudo-version: keep the default
	// version, but report the commit.
	setBuild(t, "0.1.0-dev", unsetCommit, unsetDate, &debug.BuildInfo{
		Main:     debug.Module{Version: "v0.0.0-20261113163940-9141eb95a410"},
		Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "9141eb95a41047088c40fc8108b908d307e1ea4a"}},
	})
	if b := currentBuildInfo(); b.Version != "0.1.0-dev" || b.Commit != "9141eb95a41047088c40fc8108b908d307e1ea4a" {
		t.Fatalf("pseudo-version: got %+v", b)
	}
}

func TestIsReleaseVersion(t *testing.T) {
	for v, want := range map[string]bool{
		"v1.2.3":                               true,
		"v1.2.3-rc.1":                          true,
		"(devel)":                              false,
		"":                                     false,
		"v0.0.0-20261113163940-9141eb95a410":   false,
		"v1.2.4-0.20261113163940-9141eb95a410": false,
		"v1.2.4-rc.1.0.20261113163940-9141eb95a410":       false,
		"v2.0.0-20261113163940-9141eb95a410+incompatible": false,
		"v0.0.0-20261113163940-9141eb95a410+dirty":        false,
		"v1.2.3+dirty": false,
	} {
		if got := isReleaseVersion(v); got != want {
			t.Errorf("isReleaseVersion(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestWriteVersion(t *testing.T) {
	setBuild(t, "1.2.3", "abc1234", "2026-11-13T10:00:00Z", nil)

	var out, errOut bytes.Buffer
	if err := writeVersion(&out, &errOut, nil); err != nil {
		t.Fatal(err)
	}
	if out.String() != versionString()+"\n" {
		t.Fatalf("default output %q", out.String())
	}

	out.Reset()
	if err := writeVersion(&out, &errOut, []string{"--short"}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "1.2.3\n" {
		t.Fatalf("--short output %q", out.String())
	}

	out.Reset()
	if err := writeVersion(&out, &errOut, []string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var b buildInfo
	if err := json.Unmarshal(out.Bytes(), &b); err != nil {
		t.Fatalf("--json output %q: %v", out.String(), err)
	}
	if b.Version != "1.2.3" || b.Commit != "abc1234" || b.OS != runtime.GOOS || b.Arch != runtime.GOARCH || b.GoVersion != runtime.Version() {
		t.Fatalf("--json decoded %+v", b)
	}

	for _, args := range [][]string{{"extra"}, {"--short", "--json"}, {"--nope"}} {
		if err := writeVersion(&out, &errOut, args); err == nil {
			t.Errorf("writeVersion(%q) succeeded, want an error", args)
		}
	}
	if err := writeVersion(&out, &errOut, []string{"-h"}); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("-h: got %v, want flag.ErrHelp", err)
	}
}

func TestRunVersionRejectsArguments(t *testing.T) {
	if err := runVersion([]string{"unexpected"}); err == nil {
		t.Fatal("runVersion accepted a positional argument")
	}
}
