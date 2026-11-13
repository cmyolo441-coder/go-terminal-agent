//go:build unix

package plugin

import (
	"os/exec"
	"syscall"
)

// setProcessGroup starts the command in its own process group and makes
// cancellation kill the whole group, so helpers a plugin spawns never
// outlive a timeout.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}
