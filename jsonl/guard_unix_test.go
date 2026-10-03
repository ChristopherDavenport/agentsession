//go:build unix

package jsonl

import (
	"context"
	"syscall"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
)

// TestGuardFallsBackWithoutFlock: on a file system that keeps no flock
// the store still creates, opens and appends, and takes a ref's lock,
// as it did before the guard, rather than fail.
func TestGuardFallsBackWithoutFlock(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.ENOTSUP, syscall.EOPNOTSUPP, syscall.ENOLCK, syscall.ENOSYS} {
		old := flock
		calls := 0
		flock = func(fd, how int) error {
			calls++
			return errno
		}
		func() {
			defer func() { flock = old }()
			ctx := context.Background()
			st, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			s, err := st.Create(ctx, agentsession.Header{})
			if err != nil {
				t.Fatalf("%v: Create: %v", errno, err)
			}
			if _, err := st.Append(ctx, s.ID(), &agentsession.InfoEntry{Name: "n"}); err != nil {
				t.Fatalf("%v: Append: %v", errno, err)
			}
			st.Release(s.ID())
			if _, err := st.Open(ctx, s.ID()); err != nil {
				t.Fatalf("%v: Open: %v", errno, err)
			}
			if err := st.UpdateRef(ctx, "r", agentsession.RefTarget{}, agentsession.RefTarget{Session: s.ID()}, ""); err != nil {
				t.Fatalf("%v: UpdateRef: %v", errno, err)
			}
			if calls == 0 {
				t.Errorf("%v: the guard never tried flock", errno)
			}
		}()
	}
	// Any other flock error is still an error.
	old := flock
	flock = func(fd, how int) error { return syscall.EIO }
	defer func() { flock = old }()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(context.Background(), agentsession.Header{}); err == nil {
		t.Error("Create went on over a guard that failed with EIO")
	}
}
