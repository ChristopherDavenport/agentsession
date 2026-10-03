//go:build unix

package jsonl

import (
	"os"
	"syscall"
)

// inode returns the file's inode number, which names the file a rename
// replaced; 0 when the platform does not say.
func inode(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}
