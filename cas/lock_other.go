//go:build !unix

package cas

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// sharedLocks says whether the locks here are real across processes:
// without flock nothing keeps other processes' commits out of a journal being compacted, so a store does not compact.
const sharedLocks = false

// dirLock on a platform without flock is a file created exclusively; a
// lock a crashed process left behind has to be removed by hand, and
// release removes it, so this fallback does unlink. The
// unix build uses flock, which the kernel releases on exit.
type dirLock struct {
	path string
}

func lockFile(path string) (*dirLock, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("%w: %s", ErrSessionLocked, path)
		}
		return nil, fmt.Errorf("cas: lock: %w", err)
	}
	f.Close()
	return &dirLock{path: path}, nil
}

func (l *dirLock) release() error {
	if l == nil || l.path == "" {
		return nil
	}
	err := os.Remove(l.path)
	l.path = ""
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// lockShared on a platform without flock takes nothing: the sweep and a
// writer are not kept apart there, and the grace period is the only
// protection. The unix build holds a real shared lock.
func lockShared(ctx context.Context, path string) (*dirLock, error) {
	return &dirLock{}, ctx.Err()
}

// lockExclusive on a platform without flock takes nothing, for the same
// reason.
func lockExclusive(ctx context.Context, path string) (*dirLock, error) {
	return &dirLock{}, ctx.Err()
}
