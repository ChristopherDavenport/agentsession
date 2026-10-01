//go:build unix

package cas

import (
	"context"
	"path/filepath"
	"testing"
	"time"
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
