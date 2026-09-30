//go:build unix

package cas

import (
	"os"
	"syscall"
)

// mapFile maps a file read-only, as git maps its pack indexes: the pages
// a lookup touches are read, and the kernel may drop them again, so a
// store's indexes cost a process nothing in proportion to their size.
func mapFile(f *os.File, size int) ([]byte, func() error, error) {
	if size == 0 {
		return nil, func() error { return nil }, nil
	}
	b, err := syscall.Mmap(int(f.Fd()), 0, size, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, nil, err
	}
	return b, func() error { return syscall.Munmap(b) }, nil
}
