//go:build windows

package tools

import "os/exec"

func setProcessGroup(cmd *exec.Cmd) {}
