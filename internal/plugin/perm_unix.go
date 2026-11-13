//go:build unix

package plugin

import (
	"fmt"
	"os"
)

// checkNotWorldWritable refuses files any local user could rewrite, since
// GTA would then run code chosen by someone else.
func checkNotWorldWritable(path string, st os.FileInfo) error {
	if st.Mode().Perm()&0o002 != 0 {
		return fmt.Errorf("%s is writable by every user; run chmod o-w on it", path)
	}
	return nil
}

// checkExecutable requires an execute bit on a plugin's own programs.
func checkExecutable(path string, st os.FileInfo) error {
	if st.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s is not executable; run chmod +x on it", path)
	}
	return nil
}
