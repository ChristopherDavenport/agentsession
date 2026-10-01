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
//
// The turnstile has a second side. Every holder first takes path's next
// lock shared, and lets it go once it has the lock, so it holds it only
// while it tries; lockExclusive holds next exclusive while it waits. A
// holder arriving once a sweep waits therefore queues behind it, and
// writers whose holds overlap, as durable appends' fsyncs do, cannot
// keep the lock from ever being free for the sweep. No caller may hold
// the lock while it takes it again, nor wait, holding it, on one that
// takes it: behind a waiting sweep, that is a deadlock.
func lockShared(ctx context.Context, path string) (*dirLock, error) {
	next, err := lockPoll(ctx, path+".next", syscall.LOCK_SH, 2*time.Millisecond)
	if err != nil {
		return nil, err
	}
	defer next.release()
	if lk, ok, err := lockTry(path, syscall.LOCK_SH); ok || err != nil {
		return lk, err
	}
	want, err := lockWait(ctx, path+".want", syscall.LOCK_SH)
	if err != nil {
		return nil, err
	}
	defer want.release()
	// The sweep waits on this writer before its next batch, so it polls
	// at a short interval rather than lockWait's longest.
	return lockPoll(ctx, path, syscall.LOCK_SH, 2*time.Millisecond)
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
// it again and again lets those waiting in between. It holds path's
// next lock exclusive from before that wait until it has the lock, so
// no holder arriving meanwhile takes the lock ahead of it.
func lockExclusive(ctx context.Context, path string) (*dirLock, error) {
	next, err := lockPoll(ctx, path+".next", syscall.LOCK_EX, 2*time.Millisecond)
	if err != nil {
		return nil, err
	}
	defer next.release()
	want, err := lockPoll(ctx, path+".want", syscall.LOCK_EX, 2*time.Millisecond)
	if err != nil {
		return nil, err
	}
	want.release()
	// Holders arriving now wait on this caller, so it polls at a short
	// interval rather than lockWait's longest.
	return lockPoll(ctx, path, syscall.LOCK_EX, 2*time.Millisecond)
}

// lockWait tries the lock without blocking and retries until ctx ends,
// so a caller waits only as long as its own deadline allows.
func lockWait(ctx context.Context, path string, how int) (*dirLock, error) {
	return lockPoll(ctx, path, how, 50*time.Millisecond)
}

// lockPoll is lockWait backing off to at most longest between tries.
func lockPoll(ctx context.Context, path string, how int, longest time.Duration) (*dirLock, error) {
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
		wait = min(2*wait, longest)
	}
}
