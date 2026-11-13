//go:build !unix

package plugin

import "os/exec"

// setProcessGroup relies on the default cancellation, which kills the
// process itself; Windows has no process groups to signal.
func setProcessGroup(cmd *exec.Cmd) {}
