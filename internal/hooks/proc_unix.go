//go:build !windows

package hooks

import (
	"os/exec"
	"syscall"
)

// setProcessGroup starts the hook in its own process group and makes
// cancellation kill the whole group, so a hook's children never outlive its
// timeout.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
