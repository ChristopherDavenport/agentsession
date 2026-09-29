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

// TestReusedObjectIsSynced: a durable append that reuses a loose object
// another writer left unsynced makes it durable with its own commit.
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
	body, _ := d.objs.read(spaceContents, e.Base().ContentHash())
	if err := d.objs.write(spaceContents, e.Base().ContentHash(), body, true); err != nil {
		t.Fatal(err)
	}
	if !d.objs.pendFiles[path] || !d.objs.pendDirs[filepath.Dir(path)] {
		t.Error("a reused loose object is not in what the next durable commit syncs")
	}
	// A corrupt loose copy is replaced by the bytes the writer holds.
	os.WriteFile(path, []byte("garbage"), 0o600)
	if err := d.objs.write(spaceContents, e.Base().ContentHash(), body, true); err != nil {
		t.Fatal(err)
	}
	if got, err := d.objs.read(spaceContents, e.Base().ContentHash()); err != nil || string(got) != string(body) {
		t.Errorf("a corrupt loose copy was reused: %v", err)
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

// TestRetiredPackStaysReadable: a pack dropped from the list while a
// reader holds it stays readable until the store closes.
func TestRetiredPackStaysReadable(t *testing.T) {
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
	if _, err := p.read(off, n); err != nil {
		t.Errorf("a located pack closed under its reader: %v", err)
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

// TestLogAheadOfLostLazyRecord: a log line that reached the disk ahead
// of a lazy record and objects a crash took is a lost append, not damage
// that keeps the session from opening.
func TestLogAheadOfLostLazyRecord(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root, WithSync(SyncNever))
	st.Create(ctx, agentsession.Header{ID: "s"})
	id := mustAppend(t, st, "s", item("lazy one"))
	j := filepath.Join(root, "journal")
	data, _ := os.ReadFile(j)
	lines := strings.SplitAfter(string(data), "\n")
	os.WriteFile(j, []byte(strings.Join(lines[:len(lines)-2], "")), 0o600)
	p, _ := st.objs.loosePath(spaceEntries, id)
	os.Remove(p)
	st.objs.pendFiles = map[string]bool{}
	st.journalDirty = false
	st.Release("s")
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
		j := filepath.Join(root, "journal")
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
