//go:build unix

package cas

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
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
