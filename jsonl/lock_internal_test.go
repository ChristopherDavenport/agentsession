package jsonl

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
)

// TestStaleLockTakeoverIsExclusive: several takers find the lock a dead
// process left. Taking it over reads the holder, removes the file and
// creates a new one, and a taker that read the stale lock must not
// remove the fresh one another created in its place; at most one holds
// the lock at a time, however many race for it.
func TestStaleLockTakeoverIsExclusive(t *testing.T) {
	dir := t.TempDir()
	session := filepath.Join(dir, "s.jsonl")
	host, _ := os.Hostname()
	dead, _ := json.Marshal(LockInfo{PID: 2147483000, Host: host, Since: time.Now()})
	for trial := range 60 {
		if err := os.WriteFile(lockPath(session), dead, 0o600); err != nil {
			t.Fatal(err)
		}
		var holders, worst atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for range 6 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for {
					err := acquireLock(session, nil)
					if errors.Is(err, agentsession.ErrSessionLocked) {
						time.Sleep(50 * time.Microsecond)
						continue
					}
					if err != nil {
						t.Error(err)
						return
					}
					break
				}
				n := holders.Add(1)
				for {
					w := worst.Load()
					if n <= w || worst.CompareAndSwap(w, n) {
						break
					}
				}
				time.Sleep(200 * time.Microsecond)
				holders.Add(-1)
				releaseLock(session)
			}()
		}
		close(start)
		wg.Wait()
		if worst.Load() > 1 {
			t.Fatalf("trial %d: %d takers held the lock at once", trial, worst.Load())
		}
		os.Remove(lockPath(session))
	}
}
