//go:build unix

package jsonl

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// guardName is the file in a directory whose flock keeps the takers of
// that directory's lock files in turn.
const guardName = ".agentsession-guard"

// flock is syscall.Flock, a variable so a test can fail it.
var flock = syscall.Flock

// noFlock reports whether err says the file system keeps no flock: an
// NFS mount without a lock daemon, some FUSE and network file systems.
func noFlock(err error) bool {
	return errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) ||
		errors.Is(err, syscall.ENOLCK) || errors.Is(err, syscall.ENOSYS)
}

// guarded runs f holding an exclusive flock on the directory's guard
// file, which the kernel drops with the process however it dies, so
// the guard is never stale. Everything acquireLock decides, creating
// the lock, reading its holder and taking over a stale one, is f, so
// two takers cannot interleave: a taker that read a stale lock cannot
// remove the fresh one another took over it. The guard is held for the
// length of that decision, and never while a session is held. Where the
// file system keeps no flock the store is no less usable than before:
// f runs unguarded, with the race an unguarded takeover has.
func guarded(lockFile string, f func() error) error {
	g, err := os.OpenFile(filepath.Join(filepath.Dir(lockFile), guardName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("jsonl: lock guard: %w", err)
	}
	defer g.Close()
	for {
		err := flock(int(g.Fd()), syscall.LOCK_EX)
		if err == syscall.EINTR {
			continue
		}
		if noFlock(err) {
			return f()
		}
		if err != nil {
			return fmt.Errorf("jsonl: lock guard: %w", err)
		}
		break
	}
	defer flock(int(g.Fd()), syscall.LOCK_UN)
	return f()
}
