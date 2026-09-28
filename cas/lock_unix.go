//go:build unix

package cas

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// dirLock is an exclusive lock on a directory, held through flock on a
// file in it. The kernel drops it when the process exits, however it
// exits, so there is no stale lock to detect and no PID to trust.
type dirLock struct {
	f *os.File
}

// lockDir takes the lock, or returns ErrSessionLocked when another
// holder has it.
func lockDir(dir string) (*dirLock, error) {
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("cas: lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrSessionLocked, dir)
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
