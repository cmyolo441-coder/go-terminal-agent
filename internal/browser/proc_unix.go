//go:build unix

package browser

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// configureProcess starts the browser as the leader of a new process group
// so Close can signal every helper process it spawned.
func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessTree sends SIGKILL to the browser's process group. A group
// that no longer exists is not an error.
func killProcessTree(p *os.Process) error {
	if p == nil {
		return nil
	}
	err := syscall.Kill(-p.Pid, syscall.SIGKILL)
	if err != nil && !errors.Is(err, syscall.ESRCH) {
		// Fall back to the leader alone (for example when the group is
		// not ours to signal).
		if kerr := p.Kill(); kerr != nil && !errors.Is(kerr, os.ErrProcessDone) {
			return err
		}
	}
	return nil
}

// runningAsRoot reports whether the effective user is root, in which case
// Chromium refuses to start with its sandbox enabled.
func runningAsRoot() bool { return os.Geteuid() == 0 }

// signalGroupAlive probes process group pgid with signal 0.
func signalGroupAlive(pgid int) bool {
	return syscall.Kill(-pgid, 0) == nil
}
