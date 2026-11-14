//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package browserview

import (
	"syscall"
	"unsafe"
)

type winsize struct {
	Row, Col, Xpixel, Ypixel uint16
}

// TerminalCellSize returns the character cell size in pixels of the tty
// referred to by fd using TIOCGWINSZ. ok is false when fd is not a tty or
// the terminal does not report pixel dimensions.
func TerminalCellSize(fd uintptr) (CellSize, bool) {
	var ws winsize
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	if errno != 0 || ws.Col == 0 || ws.Row == 0 || ws.Xpixel == 0 || ws.Ypixel == 0 {
		return CellSize{}, false
	}
	cs := CellSize{W: int(ws.Xpixel) / int(ws.Col), H: int(ws.Ypixel) / int(ws.Row)}
	return cs, cs.valid()
}
