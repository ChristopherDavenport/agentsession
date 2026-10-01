//go:build linux && (amd64 || arm64 || riscv64 || loong64)

package cas

import (
	"encoding/binary"
	"os"
	"syscall"
	"unsafe"
)

// fsIocGetversion is FS_IOC_GETVERSION, which reads an inode's
// generation: ext4, XFS and btrfs change it when they reuse the inode.
// Its number declares a long, so it is this on 64-bit architectures
// with the generic ioctl encoding; the kernel writes a 4-byte int.
const fsIocGetversion = 0x80087601

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
