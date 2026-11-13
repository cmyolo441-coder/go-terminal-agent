package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/version"
)

// buildInfo is what `gta version` reports.
type buildInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"go_version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
}

// Defaults of internal/version when no -ldflags were given.
const (
	unsetCommit = "none"
	unsetDate   = "unknown"
)

// readBuildInfo is replaced in tests.
var readBuildInfo = debug.ReadBuildInfo

// currentBuildInfo returns the values injected with -ldflags at release time.
// A binary built without them (go install, go build) falls back to what the
// Go toolchain recorded: the module version and the VCS revision and time.
func currentBuildInfo() buildInfo {
	b := buildInfo{
		Version:   version.Version,
		Commit:    version.Commit,
		Date:      version.Date,
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
	}
	bi, ok := readBuildInfo()
	if !ok {
		return b
	}
	if b.Commit == unsetCommit && strings.HasSuffix(b.Version, "-dev") && isReleaseVersion(bi.Main.Version) {
		// go install module@v1.2.3: the module version is the release.
		b.Version = strings.TrimPrefix(bi.Main.Version, "v")
	}
	modified := false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if b.Commit == unsetCommit && s.Value != "" {
				b.Commit = s.Value
			}
		case "vcs.time":
			if b.Date == unsetDate && s.Value != "" {
				b.Date = s.Value
			}
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if modified && b.Commit != unsetCommit && version.Commit == unsetCommit {
		b.Commit += "-dirty"
	}
	return b
}

// pseudoVersionRE matches the timestamp-and-revision suffix of a Go module
// pseudo-version (v0.0.0-20261113163940-9141eb95a410 and its variants).
var pseudoVersionRE = regexp.MustCompile(`[-.]\d{14}-[0-9a-f]{12}(\+[0-9A-Za-z.-]+)?$`)

// isReleaseVersion reports whether a module version recorded by the Go
// toolchain names a tagged release rather than "(devel)", a pseudo-version
// stamped from an untagged commit, or a build from a modified tree (+dirty).
func isReleaseVersion(v string) bool {
	return strings.HasPrefix(v, "v") && !strings.Contains(v, "+dirty") && !pseudoVersionRE.MatchString(v)
}

// versionString is the multi-line text printed by `gta version` and
// `gta --version`.
func versionString() string {
	b := currentBuildInfo()
	return fmt.Sprintf("gta %s\n  commit:  %s\n  built:   %s\n  go:      %s\n  os/arch: %s/%s",
		b.Version, b.Commit, b.Date, b.GoVersion, b.OS, b.Arch)
}

// runVersion implements `gta version [--short | --json]`.
func runVersion(args []string) error {
	return writeVersion(os.Stdout, os.Stderr, args)
}

func writeVersion(stdout, stderr io.Writer, args []string) error {
	fs := flag.NewFlagSet("gta version", flag.ContinueOnError)
	fs.SetOutput(stderr)
	short := fs.Bool("short", false, "print only the version number")
	asJSON := fs.Bool("json", false, "print the build information as JSON")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), "Usage:\n  gta version [--short | --json]\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("version takes no arguments, got %q", fs.Arg(0))
	}
	if *short && *asJSON {
		return fmt.Errorf("--short and --json cannot be used together")
	}
	switch {
	case *short:
		_, err := fmt.Fprintln(stdout, currentBuildInfo().Version)
		return err
	case *asJSON:
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(currentBuildInfo())
	default:
		_, err := fmt.Fprintln(stdout, versionString())
		return err
	}
}
