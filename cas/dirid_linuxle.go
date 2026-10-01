//go:build linux && (amd64 || arm64 || riscv64 || loong64 || 386 || arm)

package cas

import (
	"encoding/binary"
	"os"
	"syscall"
	"unsafe"
)

// dirIdentity returns what tells a directory from one made at its path
// after it was removed: its device, inode and the inode's generation.
// Where the filesystem keeps no generation, as tmpfs and overlayfs do
// not, it is device and inode alone, and the store never prunes.
func dirIdentity(path string) (dirID, bool) {
	f, err := os.Open(path)
	if err != nil {
		return dirID{}, false
	}
	defer f.Close()
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		return dirID{}, false
	}
	id := dirID{dev: uint64(st.Dev), ino: st.Ino}
	var buf [8]byte
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), fsIocGetversion, uintptr(unsafe.Pointer(&buf))); e == 0 {
		id.gen, id.hasGen = binary.NativeEndian.Uint32(buf[:4]), true
	}
	return id, true
}
