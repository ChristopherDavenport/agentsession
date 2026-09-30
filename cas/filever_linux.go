//go:build linux

package cas

import (
	"os"
	"syscall"
	"time"
)

// versionOf returns what tells one version of a file at a path from
// another: its device and inode, which a file made after the old one is
// removed may take over, and its change time, which it cannot.
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
