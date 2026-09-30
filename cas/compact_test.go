package cas

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
)

// projectRO projects a session through a read-only store, which writes
// nothing and so leaves the journal as it is.
func projectRO(t *testing.T, root, id string) []byte {
	t.Helper()
	st, err := Open(root, WithReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var buf bytes.Buffer
	if err := st.Project(context.Background(), &buf, id); err != nil {
		t.Fatalf("project %s: %v", id, err)
	}
	return buf.Bytes()
}

func journalSessions(t *testing.T, root string) map[string]int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, line := range strings.SplitAfter(string(data), "\n") {
		if recs, _ := decodeLine([]byte(line)); len(recs) > 0 {
			for _, r := range recs {
				out[r.Session]++
			}
		}
	}
	return out
}

// TestCompact: a compaction drops the records of every session whose
// files stand for them, keeps those of a session another process holds
// lazy appends in and of a delete whose directory is still there, and
// every session reads as it did.
func TestCompact(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root, WithSync(SyncOnResponse))
	st.Create(ctx, agentsession.Header{ID: "a"})
	a1 := mustAppend(t, st, "a", item("a1"))
	if _, err := st.Write(ctx, "a", &agentsession.ResponseEntry{ResponseID: "r", Status: openresponses.ResponseStatusCompleted}); err != nil {
		t.Fatal(err)
	}
	st.Create(ctx, agentsession.Header{ID: "f", Base: a1, ParentSession: "a"})
	mustAppend(t, st, "f", item("f1"))
	st.Create(ctx, agentsession.Header{ID: "gone"})
	mustAppend(t, st, "gone", item("x"))
	if err := st.Delete(ctx, "gone"); err != nil {
		t.Fatal(err)
	}
	st.Close()

	// Another process holds "busy" with lazy appends it has not synced.
	busy, _ := Open(root, WithSync(SyncNever))
	busy.Create(ctx, agentsession.Header{ID: "busy"})
	mustAppend(t, busy, "busy", item("b1"))
	mustAppend(t, busy, "busy", item("b2"))

	before := map[string][]byte{"a": projectRO(t, root, "a"), "f": projectRO(t, root, "f")}

	c, _ := Open(root)
	n, err := c.Compact(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if n != 3 {
		t.Errorf("dropped %d sessions' records, want 3 (a, f, gone)", n)
	}
	left := journalSessions(t, root)
	if left["a"] != 0 || left["f"] != 0 || left["gone"] != 0 || left["busy"] == 0 || left[journalSession] != 1 {
		t.Errorf("journal after compaction holds %v", left)
	}
	for id, want := range before {
		if got := projectRO(t, root, id); !bytes.Equal(got, want) {
			t.Errorf("%s reads differently after compaction:\n%s\nwant\n%s", id, got, want)
		}
	}
	ro, _ := Open(root, WithReadOnly())
	if _, err := ro.Open(ctx, "gone"); err == nil {
		t.Error("a deleted session came back")
	}
	ro.Close()

	// The holder goes on in the new journal, and its lazy appends,
	// carried, still stand.
	b3 := mustAppend(t, busy, "busy", item("b3"))
	busy.Close()
	s, err := func() (*agentsession.Session, error) {
		ro, _ := Open(root, WithReadOnly())
		defer ro.Close()
		return ro.Open(ctx, "busy")
	}()
	if err != nil || s.Len() != 3 || s.Leaf() != b3 {
		t.Fatalf("busy after compaction: %v", err)
	}

	// A second compaction drops what is settled now.
	c, _ = Open(root)
	if _, err := c.Compact(ctx); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if left := journalSessions(t, root); len(left) != 1 {
		t.Errorf("journal after everything settled holds %v", left)
	}
	for id, want := range before {
		if got := projectRO(t, root, id); !bytes.Equal(got, want) {
			t.Errorf("%s reads differently after the second compaction", id)
		}
	}
}

// TestCompactCarriesLoss: a lazy append a crash takes after its records
// were carried into a compacted journal is found lost, as in the old.
func TestCompactCarriesLoss(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root, WithSync(SyncNever))
	st.Create(ctx, agentsession.Header{ID: "l"})
	kept := mustAppend(t, st, "l", item("kept"))
	st.Sync(ctx)
	lost := mustAppend(t, st, "l", item("lost"))
	c, _ := Open(root)
	if _, err := c.Compact(ctx); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if journalSessions(t, root)["l"] == 0 {
		t.Fatal("a session with unsynced lazy appends was dropped")
	}
	die(st)
	p, _ := st.objs.loosePath(spaceEntries, lost)
	os.Remove(p)
	st2, _ := Open(root)
	defer st2.Close()
	s, err := st2.Open(ctx, "l")
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 1 || s.Leaf() != kept {
		t.Errorf("after compaction and a crash: len %d, leaf %s; want 1, %s", s.Len(), s.Leaf(), kept)
	}
}

// TestCompactSeenByReaders: a store that read the old journal reads the
// new one once another process compacts, and its checkpoint of the old
// one is not taken for one of the new.
func TestCompactSeenByReaders(t *testing.T) {
	lowCheckpoints(t)
	ctx := context.Background()
	root := t.TempDir()
	w, _ := Open(root)
	w.Create(ctx, agentsession.Header{ID: "s"})
	for i := range 5 {
		mustAppend(t, w, "s", item(fmt.Sprint(i)))
	}
	w.Release("s")
	reader, _ := Open(root)
	defer reader.Close()
	if _, err := reader.replay(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Compact(ctx); err != nil {
		t.Fatal(err)
	}
	mustAppend(t, w, "s", item("after"))
	w.Close()
	got, err := reader.replay()
	if err != nil {
		t.Fatal(err)
	}
	want, _ := reader.replayFrom(0)
	if d := sameScan(got, want); d != "" {
		t.Errorf("a reader of the old journal: %s", d)
	}
	s, err := reader.Open(ctx, "s")
	if err != nil || s.Len() != 6 {
		t.Fatalf("reader after compaction: %v", err)
	}
}

// TestCompactAutomatic: a store compacts on its own once the journal
// passes compactAt, and the journal stays near that size however much
// is appended.
func TestCompactAutomatic(t *testing.T) {
	old := compactAt
	compactAt = 8 << 10
	t.Cleanup(func() { compactAt = old })
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	var want []string
	for i := range 20 {
		id := fmt.Sprintf("s%d", i)
		st.Create(ctx, agentsession.Header{ID: id})
		for j := range 10 {
			mustAppend(t, st, id, item(fmt.Sprint(j)))
		}
		st.Release(id)
		want = append(want, id)
	}
	st.Close()
	info, _ := os.Stat(filepath.Join(root, "journal"))
	if info.Size() > 2*compactAt {
		t.Errorf("journal is %d bytes, past twice compactAt", info.Size())
	}
	for _, id := range want {
		if n := bytes.Count(projectRO(t, root, id), []byte("\n")); n != 11 {
			t.Errorf("%s has %d lines after automatic compactions, want 11", id, n)
		}
	}
}

// TestCompactUnderLoad: writers in other stores, standing for other
// processes, append while one compacts again and again; every append
// they were told of is there afterwards.
func TestCompactUnderLoad(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	const writers, appends = 4, 40
	var wg sync.WaitGroup
	acked := make([][]string, writers)
	errs := make(chan error, writers+1)
	stop := make(chan struct{})
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			policy := SyncEveryAppend
			if w%2 == 1 {
				policy = SyncNever
			}
			st, err := Open(root, WithSync(policy))
			if err != nil {
				errs <- err
				return
			}
			defer st.Close()
			id := fmt.Sprintf("w%d", w)
			if _, err := st.Create(ctx, agentsession.Header{ID: id}); err != nil {
				errs <- err
				return
			}
			for i := range appends {
				eid, err := st.Append(ctx, id, item(fmt.Sprintf("%d-%d", w, i)))
				if err != nil {
					errs <- err
					return
				}
				acked[w] = append(acked[w], eid)
			}
		}()
	}
	compactions := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := Open(root)
		if err != nil {
			errs <- err
			return
		}
		defer c.Close()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := c.Compact(ctx); err == nil {
				compactions++
			} else if err != ErrSweepRunning {
				errs <- err
				return
			}
			// Compactions come far apart in use; back to back, the
			// exclusive holder would keep the writers' polls waiting.
			time.Sleep(5 * time.Millisecond)
		}
	}()
	wg.Wait()
	close(stop)
	<-done
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if compactions == 0 {
		t.Fatal("no compaction ran alongside the writers")
	}
	for w := range writers {
		id := fmt.Sprintf("w%d", w)
		ro, _ := Open(root, WithReadOnly())
		s, err := ro.Open(ctx, id)
		ro.Close()
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		for _, eid := range acked[w] {
			if _, ok := s.Entry(eid); !ok {
				t.Errorf("%s lost acknowledged append %s over %d compactions", id, eid, compactions)
			}
		}
		if s.Len() != appends {
			t.Errorf("%s holds %d entries, want %d", id, s.Len(), appends)
		}
	}
}

// TestCompactKeepsDamage: a compaction of a journal with a damaged line
// keeps the old journal aside, and Verify reports it.
func TestCompactKeepsDamage(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	st.Create(ctx, agentsession.Header{ID: "s"})
	mustAppend(t, st, "s", item("a"))
	f, _ := os.OpenFile(filepath.Join(root, "journal"), os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString("{\"op\":\"append\",\"session\":\"s\",\"crc\":\"0\"}\n")
	f.Close()
	if _, err := st.Compact(ctx); err != nil {
		t.Fatal(err)
	}
	rep, err := st.Verify(ctx)
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range rep.Problems {
		found = found || p.Kind == "journal" && strings.Contains(p.Err.Error(), damagedPrefix)
	}
	if !found {
		t.Errorf("Verify after compacting a damaged journal: %v", rep.Problems)
	}
}
