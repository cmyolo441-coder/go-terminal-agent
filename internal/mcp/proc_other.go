//go:build !unix

package mcp

import "os/exec"

// setProcessGroup is a no-op where process groups are not available.
func setProcessGroup(cmd *exec.Cmd) {}

// terminate stops the server; platforms without signals cannot ask politely.
func terminate(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// kill force-stops the server.
func kill(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
