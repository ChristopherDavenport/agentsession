package cas

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
)

// TestScale builds a store the way many agents running at once do, and
// measures what a new agent pays to use it: opening the store, opening
// a session, listing, and what the process holds after. It is opt-in:
//
//	AGENTSESSION_SCALE=1 go test ./cas -run TestScale -v -timeout 2h
//
// AGENTSESSION_SCALE_RECORDS sets how many appends to make (default
// 300000), in sessions of 3000, by 8 writers, each a store of its own
// as a process would be.
func TestScale(t *testing.T) {
	if os.Getenv("AGENTSESSION_SCALE") == "" {
		t.Skip("set AGENTSESSION_SCALE=1 to run")
	}
	records := 300000
	if v, err := strconv.Atoi(os.Getenv("AGENTSESSION_SCALE_RECORDS")); err == nil {
		records = v
	}
	const perSession, writers = 3000, 8
	sessions := records / perSession
	ctx := context.Background()
	root := os.Getenv("AGENTSESSION_SCALE_DIR")
	if root == "" {
		root = t.TempDir()
	}
	output := strings.Repeat("a line of tool output, say a compiler error\n", 20)

	start := time.Now()
	var next, done atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	var maxJournal atomic.Int64
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				n := next.Add(1) - 1
				if n >= int64(sessions) {
					return
				}
				// A store per session, as an agent process has.
				st, err := Open(root, WithSync(SyncOnResponse))
				if err != nil {
					errs <- err
					return
				}
				id := fmt.Sprintf("s%06d", n)
				if _, err := st.Create(ctx, agentsession.Header{ID: id}); err != nil {
					errs <- err
					return
				}
				for i := 0; i < perSession/4; i++ {
					cid, rid := fmt.Sprintf("c%d", i), fmt.Sprintf("r%d", i)
					for _, e := range []agentsession.Entry{
						&agentsession.ItemEntry{Item: &openresponses.Message{Role: openresponses.RoleUser, Content: openresponses.Contents{&openresponses.InputText{Text: "go on"}}}},
						&agentsession.ItemEntry{Item: &openresponses.FunctionCall{CallID: cid, Name: "bash", Arguments: `{"cmd":"make"}`}, ResponseID: rid},
						&agentsession.ResponseEntry{ResponseID: rid, Model: "m", Status: openresponses.ResponseStatusCompleted},
						&agentsession.ItemEntry{Item: &openresponses.FunctionCallOutput{CallID: cid, Output: openresponses.FunctionCallOutputData{Text: fmt.Sprint(i) + output}}},
					} {
						if _, err := st.Append(ctx, id, e); err != nil {
							errs <- err
							return
						}
					}
				}
				if err := st.Close(); err != nil {
					errs <- err
					return
				}
				if info, err := os.Stat(filepath.Join(root, "journal")); err == nil && info.Size() > maxJournal.Load() {
					maxJournal.Store(info.Size())
				}
				if d := done.Add(1); d%10 == 0 {
					t.Logf("  %d sessions, %d appends, %v", d, d*perSession, time.Since(start).Round(time.Second))
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	t.Logf("built %d sessions, %d appends in %v (%v per append across %d writers)",
		sessions, sessions*perSession, time.Since(start).Round(time.Second), time.Since(start)/time.Duration(sessions*perSession), writers)

	size := func(name string) int64 {
		info, err := os.Stat(filepath.Join(root, name))
		if err != nil {
			return 0
		}
		return info.Size()
	}
	packs, _ := filepath.Glob(filepath.Join(root, "objects", "pack", "*.pack"))
	var loose int
	for _, sp := range []string{"entries", "contents"} {
		filepath.Walk(filepath.Join(root, "objects", sp), func(_ string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				loose++
			}
			return nil
		})
	}
	t.Logf("journal now %d bytes, largest seen %d; checkpoint %d bytes; %d packs, %d loose objects",
		size("journal"), maxJournal.Load(), size(checkpointName), len(packs), loose)

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	t0 := time.Now()
	st, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	openStore := time.Since(t0)
	t0 = time.Now()
	s, err := st.Open(ctx, fmt.Sprintf("s%06d", sessions/2))
	if err != nil {
		t.Fatal(err)
	}
	openSession := time.Since(t0)
	t0 = time.Now()
	sess, err := st.Create(ctx, agentsession.Header{ID: "new"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, sess.Header().ID, item("first")); err != nil {
		t.Fatal(err)
	}
	firstAppend := time.Since(t0)
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	t.Logf("a new agent: store open %v, a %d-entry session open %v, create and first append %v, heap %+.1f MB",
		openStore, s.Len(), openSession, firstAppend, (float64(after.HeapAlloc)-float64(before.HeapAlloc))/1e6)
	runtime.KeepAlive(s)
	st.Close()

	ro, _ := Open(root, WithReadOnly())
	t0 = time.Now()
	if _, err := ro.Open(ctx, fmt.Sprintf("s%06d", sessions/3)); err != nil {
		t.Fatal(err)
	}
	t.Logf("a reader: a session open %v", time.Since(t0))
	t0 = time.Now()
	n := 0
	for _, err := range ro.List(ctx, agentsession.ListFilter{}) {
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	t.Logf("List of %d sessions: %v", n, time.Since(t0))
	ro.Close()
}
