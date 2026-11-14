//go:build !unix

package workflow

import "os/exec"

// setProcessGroup is a no-op where process groups are unavailable; the
// shell process itself is killed on cancellation.
func setProcessGroup(*exec.Cmd) {}
