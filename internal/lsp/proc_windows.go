//go:build windows

package lsp

import "os/exec"

// configureProcess needs no setup on Windows.
func configureProcess(cmd *exec.Cmd) {}

// killProcess terminates the server process.
func killProcess(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
