// Package sandbox confines the agent's shell commands with the operating
// system's own sandbox: bubblewrap (bwrap) on Linux and sandbox-exec
// (Seatbelt) on macOS.
//
// A [Policy] says what a command may touch. [Wrap] turns a shell command into
// the argv that runs it under that policy, and [Detect] reports which backend
// would be used. When a policy asks for sandboxing and no backend is
// available, both return an error that names the package to install: a
// command is never run unsandboxed in place of a sandboxed one.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Mode selects how much a sandboxed command may write.
type Mode string

const (
	// ModeOff runs commands without a sandbox.
	ModeOff Mode = "off"
	// ModeWorkspaceWrite lets commands write only inside the workspace root,
	// the extra writable paths and a temporary directory.
	ModeWorkspaceWrite Mode = "workspace-write"
	// ModeReadOnly lets commands write only to a temporary directory.
	ModeReadOnly Mode = "read-only"
)

// Modes lists the valid modes.
var Modes = []Mode{ModeOff, ModeWorkspaceWrite, ModeReadOnly}

// ParseMode validates a mode name. The empty string means ModeOff.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(strings.TrimSpace(strings.ToLower(s))); m {
	case "":
		return ModeOff, nil
	case ModeOff, ModeWorkspaceWrite, ModeReadOnly:
		return m, nil
	}
	return "", fmt.Errorf("sandbox: unknown mode %q (want off, workspace-write or read-only)", s)
}

// Policy describes the sandbox for one workspace.
type Policy struct {
	// Mode selects the sandbox; the zero value is ModeOff.
	Mode Mode
	// AllowNetwork keeps network access. When false the command gets no
	// network at all.
	AllowNetwork bool
	// Root is the absolute workspace root. It is writable in
	// ModeWorkspaceWrite and is the command's working directory.
	Root string
	// ExtraWritable lists further absolute directories that are writable in
	// ModeWorkspaceWrite (for example a build cache). Each must exist. They
	// are ignored in ModeReadOnly.
	ExtraWritable []string
}

// Backend identifies the mechanism that enforces a policy.
type Backend string

const (
	// BackendNone means commands run without a sandbox (ModeOff).
	BackendNone Backend = "none"
	// BackendBubblewrap is Linux bubblewrap (bwrap).
	BackendBubblewrap Backend = "bubblewrap"
	// BackendSandboxExec is macOS sandbox-exec (Seatbelt).
	BackendSandboxExec Backend = "sandbox-exec"
)

// ErrUnavailable is wrapped by every error that means no sandbox backend can
// enforce the requested policy on this machine.
var ErrUnavailable = errors.New("sandbox unavailable")

// platform holds everything Wrap and Detect learn from the host, so tests
// can exercise every operating system's branch on any machine.
type platform struct {
	goos     string
	lookPath func(file string) (string, error)
	tempDir  string // the user's temporary directory, for macOS profiles
}

// host returns the platform the process is running on.
func host() platform {
	return platform{goos: runtime.GOOS, lookPath: exec.LookPath, tempDir: os.TempDir()}
}

// Detect reports the backend that would enforce p on this machine. It
// returns BackendNone for ModeOff and an error wrapping ErrUnavailable when
// sandboxing is requested but impossible.
func Detect(p Policy) (Backend, error) { return detect(host(), p) }

// Wrap returns the argv that runs command with shell (for example "bash";
// empty picks bash, or sh when bash is missing) under policy p. In ModeOff
// it is simply shell -c command (cmd /C command on Windows). The caller
// should still set the process's working directory to p.Root.
func Wrap(p Policy, shell string, command string) ([]string, error) {
	return wrap(host(), p, shell, command)
}

// Probe runs a trivial command under p to prove that the backend actually
// works here: bubblewrap, for example, fails when unprivileged user
// namespaces are disabled. It returns nil in ModeOff.
func Probe(ctx context.Context, p Policy) error {
	if p.mode() == ModeOff {
		return nil
	}
	argv, err := Wrap(p, "", "true")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if p.Root != "" {
		cmd.Dir = p.Root
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%w: %s cannot start a sandbox here: %s", ErrUnavailable, argv[0], msg)
	}
	return nil
}

// Describe summarises the policy and the backend that enforces it, for a
// /sandbox status command.
func (p Policy) Describe() string { return describe(host(), p) }

func describe(pl platform, p Policy) string {
	var b strings.Builder
	mode := p.mode()
	fmt.Fprintf(&b, "Sandbox mode: %s\n", mode)
	if mode == ModeOff {
		b.WriteString("Commands run without a sandbox, with the same access as GTA itself.\n")
		return b.String()
	}
	backend, err := detect(pl, p)
	if err != nil {
		fmt.Fprintf(&b, "Backend: unavailable\nError: %v\nCommands will not run until this is fixed or the mode is set to off.\n", err)
		return b.String()
	}
	fmt.Fprintf(&b, "Backend: %s\n", backend)
	b.WriteString("Writable: ")
	writable := []string{"temporary directory"}
	if mode == ModeWorkspaceWrite {
		paths, _ := p.writable()
		writable = append(paths, writable...)
	}
	b.WriteString(strings.Join(writable, ", "))
	b.WriteString("\nEverything else: read-only\n")
	if p.AllowNetwork {
		b.WriteString("Network: allowed\n")
	} else {
		b.WriteString("Network: blocked\n")
	}
	return b.String()
}

// mode returns the policy's mode, treating the zero value as ModeOff.
func (p Policy) mode() Mode {
	if p.Mode == "" {
		return ModeOff
	}
	return p.Mode
}

// Validate checks the policy's paths. It does not look for a backend.
func (p Policy) Validate() error {
	if _, err := ParseMode(string(p.Mode)); err != nil {
		return err
	}
	if p.mode() == ModeOff {
		return nil
	}
	if p.Root != "" || p.mode() == ModeWorkspaceWrite {
		if _, err := resolveDir("root", p.Root); err != nil {
			return err
		}
	}
	if p.mode() == ModeWorkspaceWrite {
		_, err := p.writable()
		return err
	}
	return nil
}

// writable returns the resolved writable directories of a workspace-write
// policy: Root first, then ExtraWritable without duplicates.
func (p Policy) writable() ([]string, error) {
	root, err := resolveDir("root", p.Root)
	if err != nil {
		return nil, err
	}
	out := []string{root}
	seen := map[string]bool{root: true}
	for _, x := range p.ExtraWritable {
		d, err := resolveDir("extra writable path", x)
		if err != nil {
			return nil, err
		}
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out, nil
}

// resolveDir checks that path is an absolute, existing directory other than
// the filesystem root and returns it with symlinks resolved, since both
// backends match on real paths.
func resolveDir(what, path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("sandbox: %s is required", what)
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("sandbox: %s %q must be an absolute path", what, path)
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("sandbox: %s %q: %w", what, path, err)
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("sandbox: %s %q: %w", what, path, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("sandbox: %s %q is not a directory", what, path)
	}
	if real == string(filepath.Separator) || filepath.Dir(real) == real {
		return "", fmt.Errorf("sandbox: %s %q would make the whole filesystem writable", what, path)
	}
	return real, nil
}

func detect(pl platform, p Policy) (Backend, error) {
	if _, err := ParseMode(string(p.Mode)); err != nil {
		return "", err
	}
	if p.mode() == ModeOff {
		return BackendNone, nil
	}
	switch pl.goos {
	case "linux":
		if _, err := pl.lookPath("bwrap"); err != nil {
			return "", fmt.Errorf("%w: sandbox mode %q needs bubblewrap, but bwrap is not on PATH; "+
				"install the bubblewrap package (apt install bubblewrap, dnf install bubblewrap, "+
				"pacman -S bubblewrap or apk add bubblewrap) or set the sandbox mode to off", ErrUnavailable, p.mode())
		}
		return BackendBubblewrap, nil
	case "darwin":
		if _, err := sandboxExecPath(pl); err != nil {
			return "", fmt.Errorf("%w: sandbox mode %q needs sandbox-exec, which ships with macOS at /usr/bin/sandbox-exec "+
				"but was not found; restore it (it is part of the base system) or set the sandbox mode to off", ErrUnavailable, p.mode())
		}
		return BackendSandboxExec, nil
	default:
		return "", fmt.Errorf("%w: sandbox mode %q is not supported on %s (only Linux with bubblewrap and macOS); set the sandbox mode to off",
			ErrUnavailable, p.mode(), pl.goos)
	}
}

// sandboxExecPath finds sandbox-exec, which may be missing from a trimmed
// PATH even though it is always installed at /usr/bin/sandbox-exec.
func sandboxExecPath(pl platform) (string, error) {
	if path, err := pl.lookPath("sandbox-exec"); err == nil {
		return path, nil
	}
	return pl.lookPath("/usr/bin/sandbox-exec")
}

func wrap(pl platform, p Policy, shell, command string) ([]string, error) {
	backend, err := detect(pl, p)
	if err != nil {
		return nil, err
	}
	if shell == "" {
		shell = defaultShell(pl)
	}
	if backend == BackendNone {
		if pl.goos == "windows" {
			return []string{"cmd", "/C", command}, nil
		}
		return []string{shell, "-c", command}, nil
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	switch backend {
	case BackendBubblewrap:
		return bwrapArgs(pl, p, shell, command)
	case BackendSandboxExec:
		return sandboxExecArgs(pl, p, shell, command)
	}
	return nil, fmt.Errorf("sandbox: unsupported backend %q", backend)
}

// defaultShell prefers bash and falls back to sh.
func defaultShell(pl platform) string {
	if _, err := pl.lookPath("bash"); err == nil {
		return "bash"
	}
	return "sh"
}

// bwrapArgs builds the bubblewrap command line. The order of mounts matters:
// later mounts are stacked on earlier ones, so the writable binds come after
// the private /tmp and still work when the workspace lives under /tmp.
func bwrapArgs(pl platform, p Policy, shell, command string) ([]string, error) {
	bwrap, err := pl.lookPath("bwrap")
	if err != nil {
		return nil, err
	}
	argv := []string{bwrap,
		"--die-with-parent",
		"--new-session",
		"--unshare-pid",
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--tmpfs", "/tmp",
	}
	if p.mode() == ModeWorkspaceWrite {
		dirs, err := p.writable()
		if err != nil {
			return nil, err
		}
		for _, d := range dirs {
			argv = append(argv, "--bind", d, d)
		}
	}
	if !p.AllowNetwork {
		argv = append(argv, "--unshare-net")
	}
	if p.Root != "" {
		root, err := resolveDir("root", p.Root)
		if err != nil {
			return nil, err
		}
		argv = append(argv, "--chdir", root)
	}
	return append(argv, "--", shell, "-c", command), nil
}

// sandboxExecArgs builds the sandbox-exec command line. Paths are passed as
// -D parameters rather than spliced into the profile, so no path can break
// out of its string literal.
func sandboxExecArgs(pl platform, p Policy, shell, command string) ([]string, error) {
	exe, err := sandboxExecPath(pl)
	if err != nil {
		return nil, err
	}
	profile, params, err := seatbeltProfile(pl, p)
	if err != nil {
		return nil, err
	}
	argv := []string{exe, "-p", profile}
	for _, kv := range params {
		argv = append(argv, "-D", kv[0]+"="+kv[1])
	}
	return append(argv, shell, "-c", command), nil
}

// seatbeltProfile generates the SBPL profile for p and the ordered
// parameters it references. Everything is allowed except writing files
// outside the writable directories and, unless allowed, any networking.
// In SBPL the last matching rule wins, so the allows after the blanket
// deny carve out the writable places.
func seatbeltProfile(pl platform, p Policy) (string, [][2]string, error) {
	var params [][2]string
	var writable []string
	add := func(name, path string) {
		params = append(params, [2]string{name, path})
		writable = append(writable, fmt.Sprintf("    (subpath (param %q))", name))
	}
	if p.mode() == ModeWorkspaceWrite {
		dirs, err := p.writable()
		if err != nil {
			return "", nil, err
		}
		add("ROOT", dirs[0])
		for i, d := range dirs[1:] {
			add(fmt.Sprintf("WRITABLE_%d", i), d)
		}
	}
	add("TMP", "/private/tmp")
	if tmp := darwinTempDir(pl.tempDir); tmp != "" && tmp != "/private/tmp" {
		add("USER_TMP", tmp)
	}

	var b strings.Builder
	b.WriteString("(version 1)\n")
	b.WriteString("(allow default)\n")
	b.WriteString("(deny file-write*)\n")
	b.WriteString("(allow file-write*\n")
	for _, w := range writable {
		b.WriteString(w + "\n")
	}
	b.WriteString("    (literal \"/dev/null\")\n")
	b.WriteString("    (literal \"/dev/zero\")\n")
	b.WriteString("    (literal \"/dev/dtracehelper\")\n")
	b.WriteString("    (regex #\"^/dev/tty\")\n")
	b.WriteString("    (regex #\"^/dev/fd/\"))\n")
	if !p.AllowNetwork {
		b.WriteString("(deny network*)\n")
	}
	return b.String(), params, nil
}

// darwinTempDir returns the real path of the user's temporary directory
// ($TMPDIR, normally under /private/var/folders), or "" when it cannot be
// resolved. Symlinks are resolved because Seatbelt matches real paths.
func darwinTempDir(dir string) string {
	if dir == "" || !filepath.IsAbs(dir) {
		return ""
	}
	dir = filepath.Clean(dir)
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	for _, prefix := range []string{"/var/", "/tmp/"} {
		if dir == strings.TrimSuffix(prefix, "/") || strings.HasPrefix(dir, prefix) {
			dir = "/private" + dir
			break
		}
	}
	if dir == "/" || filepath.Dir(dir) == dir {
		return ""
	}
	return dir
}
