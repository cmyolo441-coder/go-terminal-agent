//go:build !windows

package autopilot

import (
	"os/exec"
	"syscall"
)

// setProcessGroup runs the command in its own process group and kills the
// whole group on cancel, so test processes never outlive a timeout.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
