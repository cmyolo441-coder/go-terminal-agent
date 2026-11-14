//go:build windows

package browser

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// configureProcess starts the browser in a new process group, detached
// from our console's Ctrl+C handling.
func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// killProcessTree terminates the browser and every process it started.
// taskkill /T walks the process tree, which Windows does not tie to a
// process group the way Unix does.
func killProcessTree(p *os.Process) error {
	if p == nil {
		return nil
	}
	kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(p.Pid))
	kill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := kill.Run(); err != nil {
		if kerr := p.Kill(); kerr != nil && !errors.Is(kerr, os.ErrProcessDone) {
			return kerr
		}
	}
	return nil
}

// processGroupAlive is not observable without extra APIs on Windows; the
// tree kill above is authoritative.
func processGroupAlive(int) bool { return false }

// runningAsRoot is always false on Windows, where Chromium's sandbox does
// not depend on the user.
func runningAsRoot() bool { return false }
