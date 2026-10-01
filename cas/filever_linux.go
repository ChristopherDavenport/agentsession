//go:build linux

package cas

import (
	"os"
	"syscall"
	"time"
	"unsafe"
)

// versionOf returns what tells one version of a file at a path from
// another: its device and inode, which a file made after the old one is
// removed may take over, and its change time, which that file shares
// only if made within the same tick of the filesystem's clock.
func versionOf(fi os.FileInfo) (fileVersion, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fileVersion{}, false
	}
	return fileVersion{dev: uint64(st.Dev), ino: st.Ino, ctime: st.Ctim.Nano(), size: fi.Size()}, true
}

// touchFile sets an open file's times, on the file the descriptor holds
// whatever its path names by then.
func touchFile(f *os.File, t time.Time) error {
	tv := syscall.NsecToTimeval(t.UnixNano())
	return syscall.Futimes(int(f.Fd()), []syscall.Timeval{tv, tv})
}

// renameOpen says a file may be renamed while it is open, so its
// version can be read after the rename from the descriptor that holds
// it.
const renameOpen = true

// fsIocGetversion is FS_IOC_GETVERSION, which reads an inode's
// generation: ext4, XFS and btrfs change it when they reuse the inode.
const fsIocGetversion = 0x80087601

// dirIdentity returns what tells a directory from one made at its path
// after it was removed: its device, inode and the inode's generation.
// It reports false where the filesystem keeps no generation, as tmpfs
// does not, so no directory there is taken for a known one.
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
	var gen int64
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), fsIocGetversion, uintptr(unsafe.Pointer(&gen))); e != 0 {
		return dirID{}, false
	}
	return dirID{dev: uint64(st.Dev), ino: st.Ino, gen: uint32(gen)}, true
}
