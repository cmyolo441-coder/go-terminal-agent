//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package browserview

// TerminalCellSize is unavailable on this platform; callers fall back to
// DefaultCellSize or a CSI 16 t probe (see Probe).
func TerminalCellSize(fd uintptr) (CellSize, bool) { return CellSize{}, false }
