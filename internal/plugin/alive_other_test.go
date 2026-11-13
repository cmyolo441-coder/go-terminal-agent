//go:build !unix

package plugin

func processAlive(int) bool { return false }
