//go:build !windows

package cas

import "os"

// openSync opens a file to fsync it.
func openSync(path string) (*os.File, error) { return os.Open(path) }

// dirSync says a directory is fsynced to make the names in it durable.
const dirSync = true
