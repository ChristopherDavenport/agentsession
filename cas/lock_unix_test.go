//go:build unix

package cas

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
)

// TestSweepLetsWritersIn: a sweep retaking its lock exclusive for batch
// after batch lets a writer waiting for it shared in between batches,
// rather than through nearly all of them (#141).
func TestSweepLetsWritersIn(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sweep.lock")
	const batches, hold = 100, 3 * time.Millisecond
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range batches {
			lk, err := lockExclusive(ctx, path)
			if err != nil {
				t.Error(err)
				return
			}
			time.Sleep(hold)
			lk.release()
		}
	}()
	time.Sleep(10 * hold)
	var longest time.Duration
	for {
		select {
		case <-done:
			if longest > batches*hold/4 {
				t.Errorf("a writer waited %v through a sweep of %v", longest, batches*hold)
			}
			return
		default:
		}
		start := time.Now()
		lk, err := lockShared(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		longest = max(longest, time.Since(start))
		lk.release()
		time.Sleep(time.Millisecond)
	}
}

// TestSweepBesideSteadyWriters: writers appending durably back to back,
// their holds of the sweep's lock overlapping, do not keep a sweep of
// many unneeded objects from taking it batch after batch (#168).
func TestSweepBesideSteadyWriters(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const unneeded = 4096 // 16 batches
	old := time.Now().Add(-2 * time.Hour)
	for i := range unneeded {
		data := []byte(fmt.Sprintf("unneeded %d", i))
		hash := hashBytes(data)
		if err := st.objs.write(spaceContents, hash, data, false); err != nil {
			t.Fatal(err)
		}
		p, _ := st.objs.loosePath(spaceContents, hash)
		os.Chtimes(p, old, old)
	}
	stop := make(chan struct{})
	var writers sync.WaitGroup
	var appends atomic.Int64
	for w := range 3 {
		id := fmt.Sprintf("w%d", w)
		if _, err := st.Create(ctx, agentsession.Header{ID: id}); err != nil {
			t.Fatal(err)
		}
		writers.Add(1)
		go func() {
			defer writers.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := st.Append(ctx, id, item(fmt.Sprintf("%s %d", id, i))); err != nil {
					t.Error(err)
					return
				}
				appends.Add(1)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	start := time.Now()
	n, err := st.Sweep(sctx, time.Hour)
	took := time.Since(start)
	close(stop)
	writers.Wait()
	if err != nil {
		t.Fatalf("the sweep: removed %d in %v: %v", n, took, err)
	}
	if n != unneeded {
		t.Errorf("removed %d, want %d", n, unneeded)
	}
	if took > 10*time.Second {
		t.Errorf("the sweep took %v beside %d appends", took, appends.Load())
	}
	t.Logf("removed %d in %v beside %d appends", n, took, appends.Load())
}

// holdShared holds the lock at path shared through an open file
// description of its own, as a writer in another process that has
// stopped, inside its commit, would: alive, holding, and making no
// progress. release lets it go.
func holdShared(t *testing.T, path string) (release func()) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			f.Close()
		})
	}
}

// TestExclusiveWaitBoundsSharedTakers: a caller of lockExclusive waiting
// on a holder that makes no progress holds the turnstile for no longer
// than the bound at a time, so a caller of lockShared arriving meanwhile
// takes the lock, shared beside the stopped holder, within the bound;
// once the holder lets go the exclusive caller takes the lock (#188).
func TestExclusiveWaitBoundsSharedTakers(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sweep.lock")
	old := turnstileBound
	turnstileBound = 20 * time.Millisecond
	defer func() { turnstileBound = old }()
	release := holdShared(t, path)
	defer release()

	sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		lk, err := lockExclusive(sctx, path)
		if err == nil {
			lk.release()
		}
		done <- err
	}()
	// The exclusive caller is on the turnstile once a shared try of next
	// fails; it may be off it again by the time this looks.
	for start := time.Now(); time.Since(start) < time.Second; {
		lk, ok, err := lockTry(path+".next", syscall.LOCK_SH)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		lk.release()
		time.Sleep(time.Millisecond)
	}
	var longest time.Duration
	for start := time.Now(); time.Since(start) < 300*time.Millisecond; {
		wctx, wcancel := context.WithTimeout(ctx, 3*time.Second)
		began := time.Now()
		lk, err := lockShared(wctx, path)
		wcancel()
		if err != nil {
			t.Fatalf("a shared taker waited %v behind an exclusive caller waiting on a stopped holder: %v", time.Since(began), err)
		}
		longest = max(longest, time.Since(began))
		lk.release()
		time.Sleep(time.Millisecond)
	}
	if longest > 10*turnstileBound {
		t.Errorf("a shared taker waited %v, the bound being %v", longest, turnstileBound)
	}
	select {
	case err := <-done:
		t.Fatalf("the exclusive caller finished while the holder held: %v", err)
	default:
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("the exclusive caller once the holder let go: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the exclusive caller did not take the lock once the holder let go")
	}
	t.Logf("a shared taker waited at most %v with the bound %v", longest, turnstileBound)
}

// TestSweepWaitingOnAStoppedWriterLetsWritersIn: a sweep waiting for
// sweep.lock on a writer that holds it shared and makes no progress
// keeps only itself waiting: a durable append through another store
// succeeds while the sweep still waits, within the bound, and once the
// holder lets go the sweep finishes and the store verifies clean (#188).
func TestSweepWaitingOnAStoppedWriterLetsWritersIn(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	old := turnstileBound
	turnstileBound = 50 * time.Millisecond
	defer func() { turnstileBound = old }()
	st, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fill(t, st, "w", 2)
	other, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.Create(ctx, agentsession.Header{ID: "x"}); err != nil {
		t.Fatal(err)
	}
	mustAppend(t, other, "x", item("before"))

	lock := filepath.Join(root, "sweep.lock")
	release := holdShared(t, lock)
	defer release()
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	swept := make(chan error, 1)
	go func() {
		_, err := st.Sweep(sctx, time.Hour)
		swept <- err
	}()
	// The sweep is waiting once a shared try of next fails; with the
	// bound it may be off the turnstile again when the append arrives,
	// which the append must survive either way.
	caught := false
	for start := time.Now(); time.Since(start) < 5*time.Second && !caught; {
		lk, ok, err := lockTry(lock+".next", syscall.LOCK_SH)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			caught = true
			continue
		}
		lk.release()
		time.Sleep(time.Millisecond)
	}
	actx, acancel := context.WithTimeout(ctx, 5*time.Second)
	defer acancel()
	began := time.Now()
	r, err := other.Write(actx, "x", item("while the sweep waits"))
	took := time.Since(began)
	if err != nil {
		t.Fatalf("an append while a sweep waits on a stopped holder, after %v: %v", took, err)
	}
	if !r.Durable {
		t.Errorf("the append was not durable: %+v", r)
	}
	if took > 2*time.Second {
		t.Errorf("the append waited %v, the bound being %v", took, turnstileBound)
	}
	select {
	case err := <-swept:
		t.Fatalf("the sweep finished while the holder held sweep.lock: %v", err)
	default:
	}
	release()
	select {
	case err := <-swept:
		if err != nil {
			t.Fatalf("the sweep once the holder let go: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the sweep did not finish once the holder let go")
	}
	rep, err := other.Verify(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Errorf("verify after the sweep: %v", rep.Problems)
	}
	s, err := other.Open(ctx, "x")
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 2 || s.Leaf() != r.ID {
		t.Errorf("x: len %d leaf %s, want 2 and %s", s.Len(), s.Leaf(), r.ID)
	}
	t.Logf("the append took %v (the sweep caught on the turnstile: %v)", took, caught)
}
