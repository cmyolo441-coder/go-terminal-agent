//go:build unix && !linux

package browser

// processGroupAlive reports whether any process remains in process group
// pgid. Without /proc, zombies awaiting reaping by init count as alive.
func processGroupAlive(pgid int) bool { return signalGroupAlive(pgid) }
