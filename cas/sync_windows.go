//go:build windows

package cas

import "os"

// openSync opens a file to fsync it: FlushFileBuffers needs the file
// open for writing.
func openSync(path string) (*os.File, error) { return os.OpenFile(path, os.O_RDWR, 0) }

// dirSync is false where a directory cannot be fsynced: NTFS makes a
// rename durable through its own journal, and has no call that flushes
// a directory.
const dirSync = false
