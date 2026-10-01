//go:build unix && !(linux && (amd64 || arm64 || riscv64 || loong64))

package cas

import (
	"os"
	"syscall"
)

// dirIdentity returns a directory's device and inode. No generation is
// read here, on other systems and on Linux architectures whose ioctl
// numbers or byte order differ, so the store never prunes its object
// directories, and a directory known by device and inode is the one it
// was.
func dirIdentity(path string) (dirID, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return dirID{}, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return dirID{}, false
	}
	return dirID{dev: uint64(st.Dev), ino: uint64(st.Ino)}, true
}
