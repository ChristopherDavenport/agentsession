//go:build !unix

package cas

import (
	"io"
	"os"
)

// mapFile reads a file whole where there is no mmap.
func mapFile(f *os.File, size int) ([]byte, func() error, error) {
	b := make([]byte, size)
	if _, err := io.ReadFull(f, b); err != nil {
		return nil, nil, err
	}
	return b, func() error { return nil }, nil
}
