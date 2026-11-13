//go:build windows

package hooks

import "os/exec"

// setProcessGroup keeps the default behaviour on Windows, where cancellation
// kills the hook process itself.
func setProcessGroup(cmd *exec.Cmd) {}
