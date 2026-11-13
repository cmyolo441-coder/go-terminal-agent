package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// fakePlatform pretends to be goos with exactly the named programs on PATH.
func fakePlatform(goos string, programs ...string) platform {
	return platform{
		goos:    goos,
		tempDir: "/var/folders/xy/T",
		lookPath: func(file string) (string, error) {
			name := filepath.Base(file)
			if slices.Contains(programs, name) {
				if filepath.IsAbs(file) {
					return file, nil
				}
				return "/usr/bin/" + name, nil
			}
			return "", exec.ErrNotFound
		},
	}
}

func realDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// indexSeq returns the index of seq inside argv, or -1.
func indexSeq(argv []string, seq ...string) int {
	for i := 0; i+len(seq) <= len(argv); i++ {
		if slices.Equal(argv[i:i+len(seq)], seq) {
			return i
		}
	}
	return -1
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"": ModeOff, "off": ModeOff, "Workspace-Write": ModeWorkspaceWrite, " read-only ": ModeReadOnly} {
		got, err := ParseMode(in)
		if err != nil || got != want {
			t.Errorf("ParseMode(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := ParseMode("strict"); err == nil {
		t.Error("ParseMode accepted an unknown mode")
	}
}

func TestWrapOff(t *testing.T) {
	argv, err := wrap(fakePlatform("linux"), Policy{}, "bash", "echo hi")
	if err != nil || !slices.Equal(argv, []string{"bash", "-c", "echo hi"}) {
		t.Fatalf("argv = %q, %v", argv, err)
	}
	argv, err = wrap(fakePlatform("linux"), Policy{Mode: ModeOff}, "", "ls")
	if err != nil || !slices.Equal(argv, []string{"sh", "-c", "ls"}) {
		t.Fatalf("no bash: argv = %q, %v", argv, err)
	}
	argv, err = wrap(fakePlatform("linux", "bash"), Policy{Mode: ModeOff}, "", "ls")
	if err != nil || !slices.Equal(argv, []string{"bash", "-c", "ls"}) {
		t.Fatalf("bash default: argv = %q, %v", argv, err)
	}
	argv, err = wrap(fakePlatform("windows"), Policy{Mode: ModeOff}, "", "dir")
	if err != nil || !slices.Equal(argv, []string{"cmd", "/C", "dir"}) {
		t.Fatalf("windows: argv = %q, %v", argv, err)
	}
}

func TestBubblewrapWorkspaceWrite(t *testing.T) {
	root, extra := realDir(t), realDir(t)
	p := Policy{Mode: ModeWorkspaceWrite, Root: root, ExtraWritable: []string{extra, extra, root}}
	argv, err := wrap(fakePlatform("linux", "bwrap"), p, "bash", "make test")
	if err != nil {
		t.Fatal(err)
	}
	if argv[0] != "/usr/bin/bwrap" {
		t.Fatalf("argv[0] = %q", argv[0])
	}
	for _, seq := range [][]string{
		{"--die-with-parent"}, {"--new-session"},
		{"--ro-bind", "/", "/"}, {"--dev", "/dev"}, {"--proc", "/proc"},
		{"--unshare-net"}, {"--chdir", root},
	} {
		if indexSeq(argv, seq...) < 0 {
			t.Errorf("missing %q in %q", seq, argv)
		}
	}
	ro := indexSeq(argv, "--ro-bind", "/", "/")
	tmp := indexSeq(argv, "--tmpfs", "/tmp")
	bindRoot := indexSeq(argv, "--bind", root, root)
	bindExtra := indexSeq(argv, "--bind", extra, extra)
	if !(ro < tmp && tmp < bindRoot && bindRoot < bindExtra) {
		t.Fatalf("mount order wrong (ro=%d tmp=%d root=%d extra=%d): %q", ro, tmp, bindRoot, bindExtra, argv)
	}
	if n := strings.Count(strings.Join(argv, "\x00"), "--bind\x00"); n != 2 {
		t.Fatalf("want 2 writable binds without duplicates, got %d: %q", n, argv)
	}
	tail := argv[len(argv)-4:]
	if !slices.Equal(tail, []string{"--", "bash", "-c", "make test"}) {
		t.Fatalf("tail = %q", tail)
	}
}

func TestBubblewrapNetworkAndReadOnly(t *testing.T) {
	root, extra := realDir(t), realDir(t)
	p := Policy{Mode: ModeReadOnly, Root: root, ExtraWritable: []string{extra}, AllowNetwork: true}
	argv, err := wrap(fakePlatform("linux", "bwrap", "bash"), p, "", "cat go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(argv, "--unshare-net") {
		t.Fatalf("network allowed but --unshare-net present: %q", argv)
	}
	if slices.Contains(argv, "--bind") {
		t.Fatalf("read-only mode must not bind anything writable: %q", argv)
	}
	if indexSeq(argv, "--tmpfs", "/tmp") < 0 || indexSeq(argv, "--chdir", root) < 0 {
		t.Fatalf("missing private /tmp or chdir: %q", argv)
	}
	if !slices.Equal(argv[len(argv)-4:], []string{"--", "bash", "-c", "cat go.mod"}) {
		t.Fatalf("tail = %q", argv[len(argv)-4:])
	}

	argv, err = wrap(fakePlatform("linux", "bwrap"), Policy{Mode: ModeReadOnly}, "sh", "ls")
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(argv, "--chdir") || !slices.Contains(argv, "--unshare-net") {
		t.Fatalf("read-only without root: %q", argv)
	}
}

func TestSandboxExecArgs(t *testing.T) {
	root, extra := realDir(t), realDir(t)
	p := Policy{Mode: ModeWorkspaceWrite, Root: root, ExtraWritable: []string{extra}}
	argv, err := wrap(fakePlatform("darwin", "sandbox-exec"), p, "bash", "go test ./...")
	if err != nil {
		t.Fatal(err)
	}
	if argv[0] != "/usr/bin/sandbox-exec" || argv[1] != "-p" {
		t.Fatalf("argv = %q", argv)
	}
	profile := argv[2]
	for _, kv := range []string{"ROOT=" + root, "WRITABLE_0=" + extra, "TMP=/private/tmp", "USER_TMP=/private/var/folders/xy/T"} {
		if indexSeq(argv, "-D", kv) < 0 {
			t.Errorf("missing -D %s in %q", kv, argv)
		}
	}
	if !slices.Equal(argv[len(argv)-3:], []string{"bash", "-c", "go test ./..."}) {
		t.Fatalf("tail = %q", argv[len(argv)-3:])
	}
	if strings.Contains(profile, root) {
		t.Fatal("paths must be passed as parameters, not spliced into the profile")
	}
	for _, want := range []string{`(subpath (param "ROOT"))`, `(subpath (param "WRITABLE_0"))`, "(deny network*)"} {
		if !strings.Contains(profile, want) {
			t.Errorf("profile lacks %s:\n%s", want, profile)
		}
	}

	// sandbox-exec missing from PATH is still found at its fixed location.
	pl := fakePlatform("darwin")
	pl.lookPath = func(file string) (string, error) {
		if file == "/usr/bin/sandbox-exec" {
			return file, nil
		}
		return "", exec.ErrNotFound
	}
	if b, err := detect(pl, p); err != nil || b != BackendSandboxExec {
		t.Fatalf("fixed-path detect: %v %v", b, err)
	}
}

func TestSeatbeltProfile(t *testing.T) {
	root := realDir(t)
	pl := fakePlatform("darwin", "sandbox-exec")

	profile, params, err := seatbeltProfile(pl, Policy{Mode: ModeWorkspaceWrite, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	wantHead := "(version 1)\n(allow default)\n(deny file-write*)\n(allow file-write*\n"
	if !strings.HasPrefix(profile, wantHead) {
		t.Fatalf("profile head:\n%s", profile)
	}
	if strings.Index(profile, "(deny file-write*)") > strings.Index(profile, "(allow file-write*") {
		t.Fatal("allow rules must follow the blanket deny (last match wins)")
	}
	if strings.Count(profile, "(") != strings.Count(profile, ")") {
		t.Fatalf("unbalanced profile:\n%s", profile)
	}
	if params[0] != [2]string{"ROOT", root} {
		t.Fatalf("params = %q", params)
	}

	profile, params, err = seatbeltProfile(pl, Policy{Mode: ModeReadOnly, Root: root, AllowNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(profile, `"ROOT"`) || strings.Contains(profile, "network") {
		t.Fatalf("read-only + network profile:\n%s", profile)
	}
	if len(params) != 2 || params[0][0] != "TMP" || params[1][0] != "USER_TMP" {
		t.Fatalf("read-only params = %q", params)
	}
}

func TestDarwinTempDir(t *testing.T) {
	cases := map[string]string{
		"/var/folders/ab/T/": "/private/var/folders/ab/T",
		"/tmp":               "/private/tmp",
		"/tmpx/a":            "/tmpx/a",
		"relative":           "",
		"":                   "",
	}
	for in, want := range cases {
		if got := darwinTempDir(in); got != want {
			t.Errorf("darwinTempDir(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUnsupportedPlatforms(t *testing.T) {
	root := realDir(t)
	_, err := wrap(fakePlatform("windows"), Policy{Mode: ModeWorkspaceWrite, Root: root}, "", "dir")
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "windows") {
		t.Fatalf("windows err = %v", err)
	}
	_, err = wrap(fakePlatform("freebsd"), Policy{Mode: ModeReadOnly}, "sh", "ls")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("freebsd err = %v", err)
	}
}

func TestPolicyValidation(t *testing.T) {
	root := realDir(t)
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(realDir(t), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	pl := fakePlatform("linux", "bwrap")
	cases := []struct {
		name string
		p    Policy
		want string
	}{
		{"no root", Policy{Mode: ModeWorkspaceWrite}, "root is required"},
		{"relative root", Policy{Mode: ModeWorkspaceWrite, Root: "src"}, "absolute"},
		{"missing root", Policy{Mode: ModeWorkspaceWrite, Root: filepath.Join(root, "nope")}, "no such file"},
		{"file root", Policy{Mode: ModeWorkspaceWrite, Root: file}, "not a directory"},
		{"slash root", Policy{Mode: ModeWorkspaceWrite, Root: "/"}, "whole filesystem"},
		{"bad extra", Policy{Mode: ModeWorkspaceWrite, Root: root, ExtraWritable: []string{"cache"}}, "extra writable path"},
		{"bad mode", Policy{Mode: "jail", Root: root}, "unknown mode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := wrap(pl, tc.p, "sh", "true")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	// A symlinked root is resolved, because both backends match real paths.
	argv, err := wrap(pl, Policy{Mode: ModeWorkspaceWrite, Root: link}, "sh", "true")
	if err != nil || indexSeq(argv, "--bind", root, root) < 0 {
		t.Fatalf("symlinked root: %q %v", argv, err)
	}
}

func TestDetectWithFakePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as a fake bwrap")
	}
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	pl := platform{goos: "linux", lookPath: exec.LookPath, tempDir: "/tmp"}
	p := Policy{Mode: ModeWorkspaceWrite, Root: realDir(t)}

	if b, err := detect(pl, Policy{}); err != nil || b != BackendNone {
		t.Fatalf("off: %v %v", b, err)
	}
	_, err := detect(pl, p)
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "install the bubblewrap package") {
		t.Fatalf("missing bwrap err = %v", err)
	}
	if _, err := wrap(pl, p, "sh", "true"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Wrap must refuse to run unsandboxed, got %v", err)
	}
	if d := describe(pl, p); !strings.Contains(d, "Backend: unavailable") || !strings.Contains(d, "bubblewrap") {
		t.Fatalf("describe:\n%s", d)
	}

	fake := filepath.Join(dir, "bwrap")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if b, err := detect(pl, p); err != nil || b != BackendBubblewrap {
		t.Fatalf("with bwrap: %v %v", b, err)
	}
	argv, err := wrap(pl, p, "sh", "true")
	if err != nil || argv[0] != fake {
		t.Fatalf("argv = %q, %v", argv, err)
	}
}

func TestDescribe(t *testing.T) {
	root := realDir(t)
	pl := fakePlatform("linux", "bwrap")
	if d := describe(pl, Policy{}); !strings.Contains(d, "Sandbox mode: off") || !strings.Contains(d, "without a sandbox") {
		t.Fatalf("off:\n%s", d)
	}
	d := describe(pl, Policy{Mode: ModeWorkspaceWrite, Root: root})
	for _, want := range []string{"Sandbox mode: workspace-write", "Backend: bubblewrap", "Writable: " + root + ", temporary directory", "Network: blocked"} {
		if !strings.Contains(d, want) {
			t.Errorf("describe lacks %q:\n%s", want, d)
		}
	}
	d = describe(pl, Policy{Mode: ModeReadOnly, Root: root, AllowNetwork: true})
	if !strings.Contains(d, "Writable: temporary directory\n") || !strings.Contains(d, "Network: allowed") {
		t.Fatalf("read-only:\n%s", d)
	}
}

// TestBubblewrapIntegration proves real confinement when bwrap is installed
// and usable on this machine; it is skipped everywhere else.
func TestBubblewrapIntegration(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bubblewrap is Linux-only")
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not installed")
	}
	root := realDir(t)
	p := Policy{Mode: ModeWorkspaceWrite, Root: root}
	if err := Probe(context.Background(), p); err != nil {
		t.Skipf("bwrap present but unusable here: %v", err)
	}
	// Outside the workspace and outside /tmp, which is private in the sandbox.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	outside, err := os.MkdirTemp(wd, "sandbox-outside-")
	if err != nil {
		t.Skipf("cannot create a directory outside /tmp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(outside) })

	run := func(command string) error {
		argv, err := Wrap(p, "sh", command)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Logf("%s: %v: %s", command, err, out)
		}
		return err
	}
	if err := run("echo ok > inside.txt"); err != nil {
		t.Fatalf("write inside root failed: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "inside.txt")); err != nil || string(data) != "ok\n" {
		t.Fatalf("inside.txt = %q, %v", data, err)
	}
	target := filepath.Join(outside, "escape.txt")
	if err := run("echo bad > '" + target + "'"); err == nil {
		t.Fatal("write outside root succeeded")
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("file outside root was created")
	}
	if err := run("echo tmp > /tmp/scratch && cat /tmp/scratch"); err != nil {
		t.Fatalf("private /tmp not writable: %v", err)
	}
	if err := run(`test "$(pwd -P)" = '` + root + `'`); err != nil {
		t.Fatalf("working directory is not the root: %v", err)
	}
	// With the network unshared only the loopback interface exists.
	if err := run(`! awk -F: 'NR > 2 { gsub(/ /, "", $1); if ($1 != "lo") found = 1 } END { exit !found }' /proc/net/dev`); err != nil {
		t.Fatalf("network was not isolated: %v", err)
	}
}
