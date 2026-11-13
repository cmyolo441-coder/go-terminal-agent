//go:build !windows

package lsp

import (
	"os/exec"
	"syscall"
)

// configureProcess puts the server in its own process group so helpers it
// spawns (tsserver, cargo check) are cleaned up with it.
func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcess kills the server's whole process group.
func killProcess(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}
