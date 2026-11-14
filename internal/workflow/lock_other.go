//go:build !unix

package workflow

import "os"

// lockFile is a no-op where advisory file locks are unavailable; callers
// must not run one journal from two processes.
func lockFile(*os.File) error { return nil }

func syncDir(string) {}
