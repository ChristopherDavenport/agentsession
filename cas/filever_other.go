//go:build !linux

package cas

import (
	"os"
	"time"
)

// versionOf reports no version where the change time is not read, so no
// file is taken on trust and each reuse reads and compares it.
func versionOf(os.FileInfo) (fileVersion, bool) { return fileVersion{}, false }

func touchFile(f *os.File, t time.Time) error { return os.Chtimes(f.Name(), t, t) }

const renameOpen = false
