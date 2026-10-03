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
// while it tries; lockExclusive holds next exclusive while it waits,
// for at most turnstileBound at a time. A holder arriving once a sweep
// waits therefore queues behind it, and writers whose holds overlap, as
// durable appends' fsyncs do, cannot keep the lock from ever being free
// for the sweep; and a holder of the lock that has stopped, keeping the
// sweep waiting, keeps those behind the sweep no longer than the bound
// before they take the lock shared beside it. No caller may hold the
// lock while it takes it again, nor wait, holding it, on one that takes
// it: behind a waiting sweep, that is a deadlock.
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

// turnstileBound is the longest a caller of lockExclusive holds path's
// next lock, keeping every holder arriving at the lock behind it, while
// it waits for the lock itself. A holder of the lock that stays alive
// and makes no progress, a process stopped by SIGSTOP or a debugger or
// a paused container, keeps it for as long as it is stopped; with no
// bound a sweep waiting on it held next through all of that, and every
// writer of every process waited behind the sweep (#188). The bound is
// set against what a live holder's hold lasts, so a sweep beside steady
// writers still passes the turnstile in one turn: a durable append holds
// the lock through its two object writes and their directories' fsyncs,
// its record's fsync and a commit pack's three, some eight fsyncs, which
// a disk taking 100 ms for each, a loaded network filesystem or an SD
// card, serves in under a second; two seconds leaves that a margin. A
// hold that outlasts it, an import packing a large session, costs the
// sweep a turn and the writers nothing. Tests shorten it.
var turnstileBound = 2 * time.Second

// lockExclusive takes an exclusive lock at path, waiting for its
// holders to let it go until ctx ends. It first waits for each holder
// waiting in lockShared to take the lock shared, so a caller that takes
// it again and again lets those waiting in between. It holds path's
// next lock exclusive from before that wait until it has the lock, so
// no holder arriving meanwhile takes the lock ahead of it, but for no
// longer than turnstileBound at a time: a holder of the lock may be
// stopped, and the holders queued on next must not wait on it too. When
// the bound passes it lets next go, so they take the lock shared beside
// the stopped holder, and waits off the turnstile, polling the lock
// alone as v0.0.18 did, for a back-off interval that doubles up to
// sixteen times the bound; then it takes next again and tries the
// turnstile once more. Each try costs the holders behind it at most the
// bound, and the back-off spaces the tries out. Nothing here asks
// whether a holder is stopped: the kernel drops a dead holder's lock,
// and a live one is waited for, within the bound, for as long as the
// caller's ctx allows.
func lockExclusive(ctx context.Context, path string) (*dirLock, error) {
	backoff := turnstileBound
	for {
		next, err := lockPoll(ctx, path+".next", syscall.LOCK_EX, 2*time.Millisecond)
		if err != nil {
			return nil, err
		}
		turn, cancel := context.WithTimeout(ctx, turnstileBound)
		lk, err := func() (*dirLock, error) {
			// The want lock stays, though next covers this release's
			// writers: writers of v0.0.17 and v0.0.18 sharing the store
			// know only it. Its wait is under the bound too, since one of
			// them may be stopped while it waits.
			want, err := lockPoll(turn, path+".want", syscall.LOCK_EX, 2*time.Millisecond)
			if err != nil {
				return nil, err
			}
			want.release()
			// Holders arriving now wait on this caller, so it polls at a
			// short interval rather than lockWait's longest.
			return lockPoll(turn, path, syscall.LOCK_EX, 2*time.Millisecond)
		}()
		cancel()
		next.release()
		if err == nil {
			return lk, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		// The bound passed with the lock still held: off the turnstile,
		// the lock is polled alone, taken if its holders let go.
		off, cancel := context.WithTimeout(ctx, backoff)
		lk, err = lockWait(off, path, syscall.LOCK_EX)
		cancel()
		if err == nil {
			return lk, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		backoff = min(2*backoff, 16*turnstileBound)
	}
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

// lockBlocking takes an exclusive lock at path, waiting for its holder
// until ctx ends. A ref's lock is held for one update, a few file
// writes and an fsync, so a caller waits for it where a session's
// holder is refused.
func lockBlocking(ctx context.Context, path string) (*dirLock, error) {
	return lockPoll(ctx, path, syscall.LOCK_EX, 5*time.Millisecond)
}
