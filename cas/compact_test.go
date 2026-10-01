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

// TestListUsesSummaries: a listing of sessions whose logs have not
// changed since they were summarized reads the kept summaries, not the
// objects, and one whose log changed is summarized afresh.
func TestListUsesSummaries(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	for i := range 3 {
		id := fmt.Sprintf("s%d", i)
		st.Create(ctx, agentsession.Header{ID: id})
		mustAppend(t, st, id, item("hello"))
		mustAppend(t, st, id, &agentsession.InfoEntry{Name: "named " + id})
	}
	st.Close()
	c, _ := Open(root)
	if _, err := c.Compact(ctx); err != nil {
		t.Fatal(err)
	}
	c.Close()
	list := func() map[string]agentsession.Summary {
		ro, _ := Open(root, WithReadOnly())
		defer ro.Close()
		out := map[string]agentsession.Summary{}
		for sum, err := range ro.List(ctx, agentsession.ListFilter{WithNames: true}) {
			if err != nil {
				t.Fatal(err)
			}
			out[sum.Header.ID] = sum
		}
		return out
	}
	want := list()
	if len(want) != 3 || want["s1"].Name != "named s1" || want["s1"].Size == 0 {
		t.Fatalf("listing: %+v", want)
	}
	// With the objects gone, only the summaries can answer.
	saved := filepath.Join(t.TempDir(), "objects")
	if err := os.Rename(filepath.Join(root, "objects"), saved); err != nil {
		t.Fatal(err)
	}
	got := list()
	for id, w := range want {
		if g := got[id]; g.Name != w.Name || g.Size != w.Size {
			t.Errorf("%s from its summary: %+v, want %+v", id, g, w)
		}
	}
	os.Rename(saved, filepath.Join(root, "objects"))

	w, _ := Open(root)
	mustAppend(t, w, "s2", &agentsession.InfoEntry{Name: "renamed"})
	w.Close()
	if got := list(); got["s2"].Name != "renamed" || got["s2"].Size <= want["s2"].Size {
		t.Errorf("after an append: %+v", got["s2"])
	}
}

// TestCompactThenTakeUp: a session a compaction dropped and a writer
// takes up again reads whole, settles again at the next compaction,
// and still loses to a crash only the lazy append the crash took.
func TestCompactThenTakeUp(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	st.Create(ctx, agentsession.Header{ID: "s"})
	for i := range 5 {
		mustAppend(t, st, "s", item(fmt.Sprint(i)))
	}
	st.Close()
	compact := func() {
		t.Helper()
		c, _ := Open(root)
		defer c.Close()
		if _, err := c.Compact(ctx); err != nil {
			t.Fatal(err)
		}
	}
	compact()
	if journalSessions(t, root)["s"] != 0 {
		t.Fatal("the settled session was not dropped")
	}

	w, _ := Open(root)
	sixth := mustAppend(t, w, "s", item("5"))
	w.Close()
	scan := func() *journalScan {
		ro, _ := Open(root, WithReadOnly())
		defer ro.Close()
		sc, _ := ro.replay()
		return sc
	}
	ro, _ := Open(root, WithReadOnly())
	v, err := ro.reconcile("s", filepath.Join(root, "sessions", "s"), scan())
	ro.Close()
	if err != nil || v.damaged || len(v.log) != 6 || v.head != sixth {
		t.Fatalf("taken up again: damaged %v, %d entries, head %s: %v", v.damaged, len(v.log), v.head, err)
	}
	compact()
	if journalSessions(t, root)["s"] != 0 {
		t.Error("a session taken up again did not settle at the next compaction")
	}

	// A lazy append whose objects a crash took, its log line kept.
	l, _ := Open(root, WithSync(SyncNever))
	lost := mustAppend(t, l, "s", item("lost"))
	die(l)
	p, _ := l.objs.loosePath(spaceEntries, lost)
	os.Remove(p)
	r, _ := Open(root)
	defer r.Close()
	s, err := r.Open(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 6 || s.Leaf() != sixth {
		t.Errorf("after the crash: %d entries at %s, want 6 at %s", s.Len(), s.Leaf(), sixth)
	}
}

// TestCompactNotStarved: a compaction finishes within its deadline while
// writers in other stores commit durably without a pause, since commits
// that arrive while it waits wait behind it.
func TestCompactNotStarved(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := Open(root)
			if err != nil {
				t.Error(err)
				return
			}
			defer st.Close()
			id := fmt.Sprintf("w%d", w)
			st.Create(ctx, agentsession.Header{ID: id})
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := st.Append(ctx, id, item(fmt.Sprint(i))); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	time.Sleep(200 * time.Millisecond)
	c, _ := Open(root)
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	_, err := c.Compact(cctx)
	cancel()
	c.Close()
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatalf("compaction beside busy writers: %v", err)
	}
}

// TestCompactHeldSession: a session a writer holds across a compaction
// keeps a settled record in its place, so the writer's next append is
// recovered in full after a crash that took its log line and HEAD, and
// nothing reads as damage.
func TestCompactHeldSession(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	w, _ := Open(root)
	w.Create(ctx, agentsession.Header{ID: "s"})
	mustAppend(t, w, "s", item("a"))
	mustAppend(t, w, "s", item("b"))
	c, _ := Open(root)
	if _, err := c.Compact(ctx); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if n := journalSessions(t, root)["s"]; n != 1 {
		t.Fatalf("a held session left %d records, want its one settled record", n)
	}
	dir := filepath.Join(root, "sessions", "s")
	logBefore, _ := os.ReadFile(filepath.Join(dir, "log"))
	headBefore, _ := os.ReadFile(filepath.Join(dir, "HEAD"))
	acked := mustAppend(t, w, "s", item("c"))
	die(w)
	// The crash took the indexes the append wrote after its commit.
	os.WriteFile(filepath.Join(dir, "log"), logBefore, 0o644)
	os.WriteFile(filepath.Join(dir, "HEAD"), headBefore, 0o644)
	r, _ := Open(root)
	defer r.Close()
	s, err := r.Open(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 3 || s.Leaf() != acked {
		t.Errorf("after the crash: %d entries at %s, want 3 at %s", s.Len(), s.Leaf(), acked)
	}
	if rep, err := r.Verify(ctx); err != nil || !rep.OK() {
		t.Errorf("verify: %v %v", err, rep.Problems)
	}
}

// TestAutoCompactHeld: a long-lived writer's session settles at each
// automatic compaction rather than piling up its records.
func TestAutoCompactHeld(t *testing.T) {
	old := compactAt
	compactAt = 4096
	t.Cleanup(func() { compactAt = old })
	ctx := context.Background()
	root := t.TempDir()
	w, _ := Open(root)
	w.Create(ctx, agentsession.Header{ID: "s"})
	for i := range 60 {
		mustAppend(t, w, "s", item(fmt.Sprint(i)))
	}
	if n := journalSessions(t, root)["s"]; n > 25 {
		t.Errorf("a held session has %d records after automatic compactions", n)
	}
	w.Close()
	ro, _ := Open(root, WithReadOnly())
	defer ro.Close()
	if rep, err := ro.Verify(ctx); err != nil || !rep.OK() {
		t.Errorf("verify: %v %v", err, rep.Problems)
	}
	if s, err := ro.Open(ctx, "s"); err != nil || s.Len() != 60 {
		t.Errorf("open: %v", err)
	}
}

// TestTakeUpLosesOnlyTheLazy: a lazy append to a session taken up after
// a compaction, whose record a crash took and whose log line it kept, is
// found lost, since the settled record before it is durable.
func TestTakeUpLosesOnlyTheLazy(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	st.Create(ctx, agentsession.Header{ID: "s"})
	kept := mustAppend(t, st, "s", item("kept"))
	st.Close()
	c, _ := Open(root)
	c.Compact(ctx)
	c.Close()
	l, _ := Open(root, WithSync(SyncNever))
	if _, err := l.Open(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	journal, _ := os.ReadFile(filepath.Join(root, "journal"))
	lost := mustAppend(t, l, "s", item("lost"))
	die(l)
	// The crash took the lazy record and the objects, not the log line.
	os.WriteFile(filepath.Join(root, "journal"), journal, 0o600)
	p, _ := l.objs.loosePath(spaceEntries, lost)
	os.Remove(p)
	r, _ := Open(root)
	defer r.Close()
	s, err := r.Open(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 1 || s.Leaf() != kept {
		t.Errorf("after the crash: %d entries at %s, want 1 at %s", s.Len(), s.Leaf(), kept)
	}
}

// TestCompactDuringImport: a compaction that finds a session committed
// and its header not yet written keeps its records, so the import
// survives a crash that took its unsynced log.
func TestCompactDuringImport(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	a, _ := Open(src)
	a.Create(ctx, agentsession.Header{ID: "s"})
	mustAppend(t, a, "s", item("x"))
	mustAppend(t, a, "s", item("y"))
	var file bytes.Buffer
	a.Project(ctx, &file, "s")
	a.Close()

	root := t.TempDir()
	st, _ := Open(root)
	if _, err := st.Import(ctx, &file, true); err != nil {
		t.Fatal(err)
	}
	st.Release("s")
	dir := filepath.Join(root, "sessions", "s")
	header, _ := os.ReadFile(filepath.Join(dir, "header"))
	os.Remove(filepath.Join(dir, "header")) // as between the commit and the header
	if _, err := st.Compact(ctx); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "header"), header, 0o644)
	die(st)
	os.Remove(filepath.Join(dir, "log"))
	r, _ := Open(root)
	defer r.Close()
	s, err := r.Open(ctx, "s")
	if err != nil || s.Len() != 2 {
		t.Fatalf("the import after a compaction and a crash: %v", err)
	}
}

// TestReplayAcrossGenerations: a store that read one journal reads a
// compacted one afresh even when the new file has the old one's inode
// and has grown past where the old read stopped.
func TestReplayAcrossGenerations(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	w, _ := Open(root)
	w.Create(ctx, agentsession.Header{ID: "x"})
	mustAppend(t, w, "x", item("old"))
	if err := w.Delete(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	p, _ := Open(root)
	defer p.Close()
	if _, err := p.replay(); err != nil {
		t.Fatal(err)
	}
	end := p.scan.end
	w.Create(ctx, agentsession.Header{ID: "x"})
	mustAppend(t, w, "x", item("new"))
	w.Release("x")
	for range 2 {
		if _, err := w.Compact(ctx); err != nil {
			t.Fatal(err)
		}
	}
	w.Create(ctx, agentsession.Header{ID: "pad"})
	for i := 0; ; i++ {
		info, _ := os.Stat(filepath.Join(root, "journal"))
		if info.Size() >= end {
			break
		}
		mustAppend(t, w, "pad", item(fmt.Sprint(i)))
	}
	w.Close()
	s, err := p.Open(ctx, "x")
	if err != nil || s.Len() != 1 {
		t.Fatalf("the recreated session through a store that read the old journal: %v", err)
	}
}

// TestCompactPaysForItself: when another process's unsynced lazy
// appends are more than compactAt, a compaction that must carry them is
// not followed by another until compactAt more has been committed.
func TestCompactPaysForItself(t *testing.T) {
	old := compactAt
	compactAt = 4096
	t.Cleanup(func() { compactAt = old })
	ctx := context.Background()
	root := t.TempDir()
	lazy, _ := Open(root, WithSync(SyncNever))
	defer lazy.Close()
	lazy.Create(ctx, agentsession.Header{ID: "lazy"})
	for i := range 60 {
		mustAppend(t, lazy, "lazy", item(fmt.Sprint(i)))
	}
	w, _ := Open(root)
	defer w.Close()
	w.Create(ctx, agentsession.Header{ID: "w"})
	const appends = 100
	for i := range appends {
		mustAppend(t, w, "w", item(fmt.Sprint(i)))
	}
	if limit := appends*260/int(compactAt) + 2; w.compactions > limit {
		t.Errorf("%d compactions for %d appends, want at most %d", w.compactions, appends, limit)
	}
}
