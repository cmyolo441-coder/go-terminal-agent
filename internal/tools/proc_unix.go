//go:build !windows

package tools

import (
	"os/exec"
	"syscall"
)

// setProcGroup puts the child in its own process group so the whole tree can be killed.
func setProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killProcGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

func shellCommand(command string) (string, []string) { return "bash", []string{"-c", command} }
