//go:build linux

package browser

import (
	"bytes"
	"os"
	"strconv"
)

// processGroupAlive reports whether a live (non-zombie) process remains in
// process group pgid. Zombies are excluded because orphaned helpers are
// reaped by init on its own schedule, and they hold no resources.
func processGroupAlive(pgid int) bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return signalGroupAlive(pgid)
	}
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		stat, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue // exited meanwhile
		}
		// Format: pid (comm) state ppid pgrp ...; comm may contain spaces
		// and parentheses, so parse from the last ')'.
		i := bytes.LastIndexByte(stat, ')')
		if i < 0 {
			continue
		}
		fields := bytes.Fields(stat[i+1:])
		if len(fields) < 3 {
			continue
		}
		if string(fields[0]) == "Z" || string(fields[0]) == "X" {
			continue
		}
		if g, err := strconv.Atoi(string(fields[2])); err == nil && g == pgid {
			return true
		}
	}
	return false
}
