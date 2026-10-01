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
// lock shared from the object write through its log record, so a
// sweep's last step never runs between the two. The sweep holds it
// exclusive only for short steps, so the wait is short.
//
// A holder that finds the lock taken says it is waiting by holding
// path's want lock shared until it has the lock, and lockExclusive
// lets every such waiter in before it takes the lock again: a sweep
// retaking the lock for batch after batch would otherwise starve a
// writer polling for it through nearly all of them, since flock does
// not queue a non-blocking caller.
func lockShared(ctx context.Context, path string) (*dirLock, error) {
	if lk, ok, err := lockTry(path, syscall.LOCK_SH); ok || err != nil {
		return lk, err
	}
	want, err := lockWait(ctx, path+".want", syscall.LOCK_SH)
	if err != nil {
		return nil, err
	}
	defer want.release()
	return lockWait(ctx, path, syscall.LOCK_SH)
}

// lockTry takes the lock at path if no other holder keeps it from
// being taken now.
func lockTry(path string, how int) (*dirLock, bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("cas: lock: %w", err)
	}
	err = syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
	if err == nil {
		return &dirLock{f: f}, true, nil
	}
	f.Close()
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("cas: lock: %w", err)
}

// lockExclusive takes an exclusive lock at path, waiting for its
// holders to let it go until ctx ends. It first waits for each holder
// waiting in lockShared to take the lock shared, so a caller that takes
// it again and again lets those waiting in between.
func lockExclusive(ctx context.Context, path string) (*dirLock, error) {
	want, err := lockWait(ctx, path+".want", syscall.LOCK_EX)
	if err != nil {
		return nil, err
	}
	want.release()
	return lockWait(ctx, path, syscall.LOCK_EX)
}

// lockWait tries the lock without blocking and retries until ctx ends,
// so a caller waits only as long as its own deadline allows.
func lockWait(ctx context.Context, path string, how int) (*dirLock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("cas: lock: %w", err)
	}
	wait := time.Millisecond
	for {
		err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
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
