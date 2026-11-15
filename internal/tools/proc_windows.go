//go:build windows

package tools

import "os/exec"

func setProcGroup(cmd *exec.Cmd) {}

func killProcGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func shellCommand(command string) (string, []string) {
	if _, err := exec.LookPath("bash"); err == nil {
		return "bash", []string{"-c", command}
	}
	return "cmd", []string{"/C", command}
}
