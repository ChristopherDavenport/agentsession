//go:build unix

package jsonl

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// guardName is the file in a directory whose flock keeps the takers of
// that directory's lock files in turn.
const guardName = ".agentsession-guard"

// guarded runs f holding an exclusive flock on the directory's guard
// file, which the kernel drops with the process however it dies, so
// the guard is never stale. Everything acquireLock decides, creating
// the lock, reading its holder and taking over a stale one, is f, so
// two takers cannot interleave: a taker that read a stale lock cannot
// remove the fresh one another took over it. The guard is held for the
// length of that decision, and never while a session is held.
func guarded(lockFile string, f func() error) error {
	g, err := os.OpenFile(filepath.Join(filepath.Dir(lockFile), guardName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("jsonl: lock guard: %w", err)
	}
	defer g.Close()
	for {
		err := syscall.Flock(int(g.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			if err != nil {
				return fmt.Errorf("jsonl: lock guard: %w", err)
			}
			break
		}
	}
	defer syscall.Flock(int(g.Fd()), syscall.LOCK_UN)
	return f()
}
