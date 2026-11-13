//go:build !unix

package plugin

import "os"

func checkNotWorldWritable(string, os.FileInfo) error { return nil }

func checkExecutable(string, os.FileInfo) error { return nil }
