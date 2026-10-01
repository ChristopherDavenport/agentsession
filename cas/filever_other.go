//go:build !linux

package cas

import (
	"os"
	"time"
)

// versionOf reports no version where the change time is not read, so no
// file is taken on trust: an object found in place gets a copy of this
// store's own, written durably.
func versionOf(os.FileInfo) (fileVersion, bool) { return fileVersion{}, false }

func touchFile(f *os.File, t time.Time) error { return os.Chtimes(f.Name(), t, t) }

const renameOpen = false

// dirIdentity reports false: no generation is read here, so no object
// directory is taken for a known one, and a commit that names an object
// syncs its space's directory too.
func dirIdentity(string) (dirID, bool) { return dirID{}, false }
