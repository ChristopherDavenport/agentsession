package cas

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
)

// These reproduce what an independent review of the store found, each
// as the probe that showed it.

func ageAllPacks(root string) {
	old := time.Now().Add(-2 * time.Hour)
	m, _ := filepath.Glob(filepath.Join(root, "objects", "pack", "pack-*"))
	for _, p := range m {
		os.Chtimes(p, old, old)
	}
}

// TestStalePackAfterSweep: a writer whose pack list still names a pack
// another store's sweep removed finds its object there, and must write
// it loose rather than commit an entry whose body no longer exists.
func TestStalePackAfterSweep(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	w, _ := Open(root)
	w.Create(ctx, agentsession.Header{ID: "a"})
	mustAppend(t, w, "a", item("shared body"))
	w.Delete(ctx, "a")
	w.Pack(ctx)
	ageAllPacks(root)
	sw, _ := Open(root)
	if _, err := sw.Sweep(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	sw.Close()
	w.Create(ctx, agentsession.Header{ID: "b"})
	mustAppend(t, w, "b", item("shared body"))
	w.Close()
	r, _ := Open(root)
	defer r.Close()
	if _, err := r.Open(ctx, "b"); err != nil {
		t.Fatalf("an entry committed over a removed pack: %v", err)
	}
	if rep, _ := r.Verify(ctx); !rep.OK() {
		t.Errorf("Verify: %v", rep.Problems)
	}
}

// TestPutBlobFreshensPack: a blob put again while it lies in an old pack
// freshens the pack, so a sweep with a grace keeps it.
func TestPutBlobFreshensPack(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	defer st.Close()
	h, _ := st.PutBlob(ctx, []byte("blob bytes"))
	st.Pack(ctx)
	ageAllPacks(root)
	if _, err := st.PutBlob(ctx, []byte("blob bytes")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Sweep(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Blob(ctx, h); err != nil {
		t.Errorf("a blob just put, after a sweep with an hour's grace: %v", err)
	}
}

// TestReusedObjectIsSynced: a durable write of a loose object another
// writer left unsynced writes a copy of its own, durably, rather than
// take the other's file on the word of an fsync of it.
func TestReusedObjectIsSynced(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	lazy, _ := Open(root, WithSync(SyncNever))
	defer lazy.Close()
	lazy.Create(ctx, agentsession.Header{ID: "l"})
	e := item("shared")
	mustAppend(t, lazy, "l", e)
	d, _ := Open(root)
	defer d.Close()
	path, _ := d.objs.loosePath(spaceContents, e.Base().ContentHash())
	before, _ := os.Stat(path)
	body, _ := d.objs.read(spaceContents, e.Base().ContentHash())
	if err := d.objs.write(spaceContents, e.Base().ContentHash(), body, true); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.Stat(path); os.SameFile(before, after) {
		t.Error("another writer's unsynced object was taken rather than written again")
	}
	if !d.objs.pendDirs[filepath.Dir(path)] {
		t.Error("the object's directory is not in what the next durable commit syncs")
	}
}

// TestPackedWithinOneTick: a pack another store writes within the pack
// directory's timestamp granularity is still found, so recovery does
// not take its objects for lost and cut the session.
func TestPackedWithinOneTick(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	w, _ := Open(root, WithSync(SyncOnResponse))
	w.Create(ctx, agentsession.Header{ID: "first"})
	mustAppend(t, w, "first", item("first"))
	w.Pack(ctx)
	w.Create(ctx, agentsession.Header{ID: "s"})
	mustAppend(t, w, "s", item("lazy"))
	d := mustAppend(t, w, "s", agentsession.NewItemEntry(&openresponses.FunctionCallOutput{CallID: "c1", Output: openresponses.FunctionCallOutputData{Text: "ok"}}))
	w.Close()
	re, _ := Open(root)
	defer re.Close()
	stamp := re.objs.packStat
	p, _ := Open(root)
	p.Pack(ctx)
	p.Close()
	os.Chtimes(filepath.Join(root, "objects", "pack"), stamp, stamp)
	s, err := re.Open(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 2 || s.Leaf() != d {
		t.Errorf("after a pack in the same tick: len %d", s.Len())
	}
}

// TestDroppedPackReadAgain: a read that located an object in a pack
// dropped from the list since finds it closed and looks again, and the
// dropped pack's file is closed rather than held open.
func TestDroppedPackReadAgain(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	a, _ := Open(root)
	defer a.Close()
	b, _ := Open(root)
	defer b.Close()
	a.Create(ctx, agentsession.Header{ID: "x"})
	id := mustAppend(t, a, "x", item("hi"))
	a.Pack(ctx)
	dg, _ := digestOf(id)
	p := a.objs.packList()[0]
	off, n, _ := p.find(spaceEntries, dg)
	b.Create(ctx, agentsession.Header{ID: "y"})
	mustAppend(t, b, "y", item("other"))
	b.Sweep(ctx, time.Hour)
	a.objs.reloadPacks(true)
	if _, err := p.read(off, n); !errors.Is(err, errPackClosed) {
		t.Errorf("a dropped pack: %v", err)
	}
	if _, err := a.objs.read(spaceEntries, id); err != nil {
		t.Errorf("a read after its pack was replaced: %v", err)
	}
}

// TestHandoverNeedsTheHead: a handover whose compare-and-swap fails
// lands its entries and changes no mark.
func TestHandoverNeedsTheHead(t *testing.T) {
	ctx := context.Background()
	rec, mir := twoStores(t)
	fill(t, rec, "s", 2)
	rec.Push(ctx, mir, "s", PushOptions{})
	more := mustAppend(t, rec, "s", item("more"))
	x, err := rec.Push(ctx, mir, "s", PushOptions{Expected: "sha256:" + strings.Repeat("0", 64), Handover: true})
	if !errors.Is(err, ErrHeadMoved) || x.Handover || x.Admitted != 1 {
		t.Errorf("a handover with the wrong head: %+v, %v", x, err)
	}
	if m, _ := rec.Mark(ctx, "s"); m != MarkRecord {
		t.Errorf("the sender's mark is %s", m)
	}
	if m, _ := mir.Mark(ctx, "s"); m != MarkMirror {
		t.Errorf("the receiver's mark is %s", m)
	}
	s, _ := mir.Open(ctx, "s")
	if _, ok := s.Entry(more); !ok {
		t.Error("the entries did not land")
	}
}

// TestLogAheadOfLostLazyObjects: a lazy record that reached the disk
// ahead of objects a crash took is a lost append, not damage that keeps
// the session from opening.
func TestLogAheadOfLostLazyObjects(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root, WithSync(SyncNever))
	st.Create(ctx, agentsession.Header{ID: "s"})
	id := mustAppend(t, st, "s", item("lazy one"))
	p, _ := st.objs.loosePath(spaceEntries, id)
	os.Remove(p)
	die(st)
	re, _ := Open(root)
	defer re.Close()
	s, err := re.Open(ctx, "s")
	if err != nil || s.Len() != 0 {
		t.Errorf("open after a lost lazy append: %v", err)
	}
}

// TestNewlineFlip: a record whose newline was damaged into another byte
// is still read, and reported; so is one at the journal's end.
func TestNewlineFlip(t *testing.T) {
	ctx := context.Background()
	for _, last := range []bool{false, true} {
		root := t.TempDir()
		w, _ := Open(root)
		w.Create(ctx, agentsession.Header{ID: "s"})
		a := mustAppend(t, w, "s", item("one"))
		mustAppend(t, w, "s", item("two"))
		w.SetHead(ctx, "s", w.open["s"].head, a)
		if !last {
			mustAppend(t, w, "s", item("three"))
		}
		w.Close()
		j := filepath.Join(root, "sessions", "s", logName)
		data, _ := os.ReadFile(j)
		lines := strings.SplitAfter(string(data), "\n")
		i := 4 // the head record
		lines[i] = strings.TrimSuffix(lines[i], "\n")
		if !last {
			lines[i] += "\x0b"
		}
		os.WriteFile(j, []byte(strings.Join(lines, "")), 0o600)
		os.WriteFile(filepath.Join(root, "sessions", "s", "HEAD"), nil, 0o600)
		r, _ := Open(root)
		s, err := r.Open(ctx, "s")
		if err != nil {
			t.Fatal(err)
		}
		want := a
		if !last {
			want = s.Entries()[s.Len()-1].Base().ID
		}
		if s.Leaf() != want {
			t.Errorf("last=%v: the head record was lost", last)
		}
		if rep, _ := r.Verify(ctx); !last && rep.OK() {
			t.Error("a damaged newline was not reported")
		}
		r.Close()
	}
}

// TestSweepFailsClosed: a keep set it cannot finish reading stops the
// sweep rather than removing what it could not follow.
func TestSweepFailsClosed(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads a file without permission")
	}
	ctx := context.Background()
	root := t.TempDir()
	w, _ := Open(root)
	w.Create(ctx, agentsession.Header{ID: "s"})
	id := mustAppend(t, w, "s", item("precious"))
	ch, _ := w.contentOf(id)
	w.Close()
	ep, _ := w.objs.loosePath(spaceEntries, id)
	os.Chmod(ep, 0)
	sw, _ := Open(root)
	defer sw.Close()
	_, err := sw.Sweep(ctx, 0)
	os.Chmod(ep, 0o644)
	if err == nil {
		t.Error("a sweep that could not read an envelope went on")
	}
	if _, err := sw.objs.read(spaceContents, ch); err != nil {
		t.Errorf("the content of a committed entry: %v", err)
	}
}

// TestImportOverStalePack: an import whose objects this store sees only
// in a pack another store's sweep removed writes them, as an append
// does.
func TestImportOverStalePack(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	src, _ := Open(t.TempDir())
	fill(t, src, "s", 2)
	src.Release("s")
	file := projectFile(t, src.Root(), "s")
	src.Close()
	a, _ := Open(root)
	if _, err := a.Import(ctx, strings.NewReader(string(file)), true); err != nil {
		t.Fatal(err)
	}
	a.Delete(ctx, "s")
	a.Pack(ctx)
	ageAllPacks(root)
	sw, _ := Open(root)
	sw.Sweep(ctx, time.Hour)
	sw.Close()
	if _, err := a.Import(ctx, strings.NewReader(string(file)), true); err != nil {
		t.Fatal(err)
	}
	a.Close()
	r, _ := Open(root)
	defer r.Close()
	if _, err := r.Open(ctx, "s"); err != nil {
		t.Errorf("an import over a removed pack: %v", err)
	}
}

// TestRecoveryKeepsUnreadable: an append whose envelope is corrupt, not
// absent, is kept and the open fails, rather than the record being
// removed for good.
func TestRecoveryKeepsUnreadable(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	ids := fill(t, st, "a", 2)
	st.Close()
	ep, _ := st.objs.loosePath(spaceEntries, ids[1])
	os.WriteFile(ep, []byte("garbage"), 0o600)
	logBefore, _ := os.ReadFile(filepath.Join(root, "sessions", "a", "log"))
	r, _ := Open(root)
	defer r.Close()
	if _, err := r.Open(ctx, "a"); !errors.Is(err, ErrCorrupt) {
		t.Errorf("open: %v", err)
	}
	if logAfter, _ := os.ReadFile(filepath.Join(root, "sessions", "a", "log")); string(logAfter) != string(logBefore) {
		t.Error("recovery removed a line whose objects it could not read")
	}
}

// TestTornInChecksum: a record a crash cut inside its checksum, with the
// next record on the same line, is torn, not damage.
func TestTornInChecksum(t *testing.T) {
	r1, _ := logRecord{Op: "append", Session: "s", Entry: "sha256:x"}.encode()
	r2, _ := logRecord{Op: "head", Session: "s", Head: "sha256:x"}.encode()
	line := append(append([]byte{}, r1[:len(r1)-5]...), r2...)
	recs, err := decodeLine(line)
	if err != nil || len(recs) != 1 || recs[0].Op != "head" {
		t.Errorf("%v, %v", recs, err)
	}
}

// TestBadIndexIsolated: a pack whose index does not read is skipped and
// reported by Verify; the store still opens.
func TestBadIndexIsolated(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	fill(t, st, "a", 1)
	st.Close()
	os.MkdirAll(filepath.Join(root, "objects", "pack"), 0o755)
	os.WriteFile(filepath.Join(root, "objects", "pack", "pack-"+strings.Repeat("0", 64)+".idx"), []byte("junk"), 0o600)
	r, err := Open(root)
	if err != nil {
		t.Fatalf("a bad index closed the store: %v", err)
	}
	defer r.Close()
	if _, err := r.Open(ctx, "a"); err != nil {
		t.Error(err)
	}
	if rep, _ := r.Verify(ctx); rep.OK() {
		t.Error("Verify did not report the bad index")
	}
}

// TestReleaseSyncs: a store that releases a session it appended to
// lazily makes those appends durable first.
func TestReleaseSyncs(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(t.TempDir(), WithSync(SyncNever))
	defer st.Close()
	st.Create(ctx, agentsession.Header{ID: "l"})
	mustAppend(t, st, "l", item("lazy"))
	if err := st.Release("l"); err != nil {
		t.Fatal(err)
	}
	if len(st.objs.pendFiles) != 0 {
		t.Error("Release left lazy appends unsynced")
	}
	lines, _ := journalLines(t, st.Root(), "l")
	if last := lines[len(lines)-2]; !strings.Contains(last, `"op":"sync"`) {
		t.Errorf("Release committed no sync record: %s", last)
	}
}

// TestTornObjectRewritten: a durable append that reuses a loose object a
// crash left short writes it again from the bytes it holds.
func TestTornObjectRewritten(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	defer st.Close()
	st.Create(ctx, agentsession.Header{ID: "s"})
	e := item("body")
	p, _ := st.objs.loosePath(spaceContents, e.Base().ContentHash())
	mustAppend(t, st, "s", e)
	os.WriteFile(p, nil, 0o600) // torn by a crash
	mustAppend(t, st, "s", item("body"))
	st.Release("s")
	if _, err := st.Open(ctx, "s"); err != nil {
		t.Errorf("a torn object reused: %v", err)
	}
}

// TestLazyAdopted: a store that recovers a session holding another
// process's unsynced lazy appends syncs their objects and journals that
// it did, so a durable append it makes next is not cut with them; and a
// lazy record whose object a crash left empty is a lost append.
func TestLazyAdopted(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	d, _ := Open(root)
	d.Create(ctx, agentsession.Header{ID: "s"})
	mustAppend(t, d, "s", item("durable zero"))
	d.Close()
	a, _ := Open(root, WithSync(SyncNever))
	e1 := mustAppend(t, a, "s", item("lazy one"))
	a.open["s"].lock.release() // the process dies; nothing is flushed
	b, _ := Open(root)
	defer b.Close()
	if _, err := b.Open(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	lines, _ := journalLines(t, root, "s")
	if last := lines[len(lines)-2]; !strings.Contains(last, `"op":"sync"`) {
		t.Errorf("no sync record after adopting: %s", last)
	}
	b.Release("s")
	// A later lazy append whose envelope a crash left empty.
	a2, _ := Open(root, WithSync(SyncNever))
	e2 := mustAppend(t, a2, "s", item("lazy two"))
	a2.open["s"].lock.release()
	pe, _ := a2.objs.loosePath(spaceEntries, e2)
	os.WriteFile(pe, nil, 0o600)
	c, _ := Open(root)
	defer c.Close()
	s, err := c.Open(ctx, "s")
	if err != nil {
		t.Fatalf("a torn lazy object: %v", err)
	}
	if _, ok := s.Entry(e1); !ok || s.Len() != 2 {
		t.Errorf("after a torn lazy append: len %d, adopted entry held %v", s.Len(), ok)
	}
}
