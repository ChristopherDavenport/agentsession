//go:build unix

package cas

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// dirLock is an exclusive lock held through flock on a file that is
// never unlinked, so no holder can be left locking an inode nothing
// points at. The kernel drops it when the process exits, however it
// exits, so there is no stale lock to detect and no PID to trust.
type dirLock struct {
	f *os.File
}

// lockFile takes the lock at path, or returns ErrSessionLocked when
// another holder has it.
func lockFile(path string) (*dirLock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("cas: lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrSessionLocked, path)
		}
		return nil, fmt.Errorf("cas: lock: %w", err)
	}
	return &dirLock{f: f}, nil
}

// release gives the lock up.
func (l *dirLock) release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}

// lockShared takes a shared lock at path: many holders at once, none
// while an exclusive holder has it. Object writers hold the sweep's
// lock shared from the object write through the journal commit, so a
// sweep never removes between the two. The lock is tried without
// blocking and retried until ctx ends, so a writer waits on a sweep
// only as long as its own deadline allows; the sweep holds the lock
// exclusive for one object at a time, so the wait is short.
func lockShared(ctx context.Context, path string) (*dirLock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("cas: lock: %w", err)
	}
	wait := time.Millisecond
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
		if err == nil {
			return &dirLock{f: f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("cas: lock: %w", err)
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		if wait < 50*time.Millisecond {
			wait *= 2
		}
	}
}
