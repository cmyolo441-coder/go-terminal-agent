//go:build windows

package autopilot

import "os/exec"

// setProcessGroup is a no-op on Windows; exec.CommandContext kills the
// direct child on cancel.
func setProcessGroup(cmd *exec.Cmd) {}
