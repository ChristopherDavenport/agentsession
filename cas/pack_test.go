package cas

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
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

// looseCount counts loose objects in both spaces.
func looseCount(t *testing.T, st *Store) int {
	t.Helper()
	n := 0
	for _, sp := range []space{spaceEntries, spaceContents} {
		st.objs.eachLoose(sp, func(_, _ string, _ os.FileInfo, tmp bool) error {
			if !tmp {
				n++
			}
			return nil
		})
	}
	return n
}

func packFiles(t *testing.T, st *Store) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(st.Root(), "objects", "pack", "pack-*.idx"))
	return m
}

func fill(t *testing.T, st *Store, id string, n int) []string {
	t.Helper()
	ctx := context.Background()
	if _, err := st.Create(ctx, agentsession.Header{ID: id}); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i := range n {
		ids = append(ids, mustAppend(t, st, id, agentsession.NewItemEntry(openresponses.UserText(fmt.Sprintf("%s %d", id, i)))))
	}
	return ids
}

// TestPack moves loose objects into a pack: the loose files go, every
// session reads and projects as before, and new appends go loose again.
func TestPack(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(t.TempDir())
	defer st.Close()
	fill(t, st, "a", 5)
	fill(t, st, "b", 3)
	st.Release("a")
	before := projectFile(t, st.Root(), "a")
	loose := looseCount(t, st)
	n, err := st.Pack(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != loose || looseCount(t, st) != 0 || len(packFiles(t, st)) != 1 {
		t.Fatalf("packed %d of %d; %d loose left, %d packs", n, loose, looseCount(t, st), len(packFiles(t, st)))
	}
	if after := projectFile(t, st.Root(), "a"); !bytes.Equal(before, after) {
		t.Error("the projection changed when its objects were packed")
	}
	// A body a packed entry holds is not written loose again.
	mustAppend(t, st, "b", agentsession.NewItemEntry(openresponses.UserText("a 0")))
	if got := looseCount(t, st); got != 1 {
		t.Errorf("an append after packing wrote %d loose objects, want 1 (the envelope; its body is packed)", got)
	}
	// Another store on the root finds packed objects, including a pack
	// written after it opened.
	other, err := Open(st.Root(), WithReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := st.Pack(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := other.Open(ctx, "b")
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 4 {
		t.Errorf("read %d entries through two packs, want 4", s.Len())
	}
}

// TestSweepRepacks is git's gc: after a delete, one pack holds what is
// held, no loose object is left, and the deleted session's own objects
// are gone from the packs as well as from the loose files.
func TestSweepRepacks(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(t.TempDir())
	defer st.Close()
	a := fill(t, st, "o", 3)
	if _, err := st.Create(ctx, agentsession.Header{ID: "f", Base: a[0]}); err != nil {
		t.Fatal(err)
	}
	mustAppend(t, st, "f", agentsession.NewItemEntry(openresponses.UserText("the fork's own")))
	fill(t, st, "gone", 2)
	if _, err := st.Pack(ctx); err != nil { // the deleted session's objects end up packed
		t.Fatal(err)
	}
	if err := st.Delete(ctx, "gone"); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, "o"); err != nil {
		t.Fatal(err)
	}
	swept, err := st.Sweep(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	// o's two entries off the fork's prefix, and gone's two: an envelope
	// and a content each.
	if swept != 8 {
		t.Errorf("swept %d objects, want 8", swept)
	}
	if looseCount(t, st) != 0 || len(packFiles(t, st)) != 1 {
		t.Errorf("after the sweep: %d loose, %d packs; want 0 and 1", looseCount(t, st), len(packFiles(t, st)))
	}
	st.Release("f")
	f, err := st.Open(ctx, "f")
	if err != nil {
		t.Fatal(err)
	}
	if f.Len() != 2 {
		t.Errorf("fork len %d after the sweep, want 2", f.Len())
	}
	// A second sweep finds nothing more and still leaves one pack.
	if swept, err := st.Sweep(ctx, 0); err != nil || swept != 0 || len(packFiles(t, st)) != 1 {
		t.Errorf("second sweep: %d, %v, %d packs", swept, err, len(packFiles(t, st)))
	}
}

// TestSweepSparesYoungPackedOrphans keeps an unneeded object of a young
// pack, loose and with the pack's age, so it still expires later.
func TestSweepSparesYoungPackedOrphans(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(t.TempDir())
	defer st.Close()
	fill(t, st, "gone", 1)
	if _, err := st.Pack(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, "gone"); err != nil {
		t.Fatal(err)
	}
	if swept, err := st.Sweep(ctx, time.Hour); err != nil || swept != 0 {
		t.Fatalf("young orphans swept: %d, %v", swept, err)
	}
	if looseCount(t, st) != 2 {
		t.Fatalf("young orphans not written back loose: %d loose", looseCount(t, st))
	}
	if swept, err := st.Sweep(ctx, -time.Hour); err != nil || swept != 2 {
		t.Errorf("once old, the orphans went: %d, %v; want 2", swept, err)
	}
}

// TestSweepWaitsForWriter has the sweep wait for a writer between an
// object write and its commit, rather than stop, and a second sweep
// refused with its own error.
func TestSweepWaitsForWriter(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(t.TempDir())
	defer st.Close()
	fill(t, st, "s", 2)
	writer, err := lockShared(ctx, filepath.Join(st.Root(), "sweep.lock"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := st.Sweep(ctx, 0)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("the sweep did not wait for the writer: %v", err)
	default:
	}
	if _, err := st.Pack(ctx); !errors.Is(err, ErrSweepRunning) {
		t.Errorf("a pack beside a sweep: %v, want ErrSweepRunning", err)
	}
	writer.release()
	if err := <-done; err != nil {
		t.Errorf("sweep after the writer committed: %v", err)
	}
	// The sweep does not hold the store's mutex while it builds its
	// keep set and pack; an append beside a waiting sweep completes.
	writer, _ = lockShared(ctx, filepath.Join(st.Root(), "sweep.lock"))
	go func() {
		_, err := st.Sweep(ctx, 0)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	appended := make(chan error, 1)
	go func() {
		_, err := st.Append(ctx, "s", agentsession.NewItemEntry(openresponses.UserText("beside a sweep")))
		appended <- err
	}()
	select {
	case err := <-appended:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(2 * time.Second):
		t.Error("an append waited on a sweep that was waiting on a writer")
	}
	writer.release()
	if err := <-done; err != nil {
		t.Error(err)
	}
	// Cancelled while waiting: the context's error, nothing removed.
	writer, _ = lockShared(ctx, filepath.Join(st.Root(), "sweep.lock"))
	defer writer.release()
	cctx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if _, err := st.Sweep(cctx, 0); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a sweep past its deadline: %v", err)
	}
}

// TestSweepRescuesCommittedSince commits an entry whose objects lie only
// in a pack the sweep is replacing, after the sweep took its keep set:
// the last step writes it back before the old pack goes.
func TestSweepRescuesCommittedSince(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(t.TempDir())
	defer st.Close()
	fill(t, st, "gone", 1)
	st.Create(ctx, agentsession.Header{ID: "s"})
	if _, err := st.Pack(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, "gone"); err != nil {
		t.Fatal(err)
	}
	keep, marks, err := st.keepAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(keep.entries) != 0 {
		t.Fatalf("keep set %v", keep.entries)
	}
	// The same body appended again to another session: it is packed, so
	// only the new envelope is written loose.
	e := agentsession.NewItemEntry(openresponses.UserText("gone 0"))
	id := mustAppend(t, st, "s", e)
	if looseCount(t, st) != 1 {
		t.Fatalf("%d loose objects, want the envelope alone", looseCount(t, st))
	}
	since := keepSet{entries: map[string]bool{}, contents: map[string]bool{}}
	if err := st.keepLogs(since, marks); err != nil {
		t.Fatal(err)
	}
	if !since.entries[id] || !since.contents[e.ContentHash()] {
		t.Fatal("the logs read on from the keep set's marks do not name the new append")
	}
	// A sweep with a grace that makes the packed objects old.
	if _, err := st.Sweep(ctx, -time.Hour); err != nil {
		t.Fatal(err)
	}
	st.Release("s")
	s, err := st.Open(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 1 {
		t.Errorf("len %d", s.Len())
	}
}

// TestSummarySize is the bytes of the session's own objects, not the
// log's, before and after packing, and for a log line written before
// lines carried sizes.
func TestSummarySize(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(t.TempDir())
	defer st.Close()
	ids := fill(t, st, "z", 4)
	var want int64
	for _, id := range ids {
		env, _ := st.objs.read(spaceEntries, id)
		c, _ := st.contentOf(id)
		body, _ := st.objs.read(spaceContents, c)
		want += int64(len(env) + len(body))
	}
	size := func() int64 {
		for sum, err := range st.List(ctx, agentsession.ListFilter{}) {
			if err != nil {
				t.Fatal(err)
			}
			return sum.Size
		}
		return -1
	}
	if got := size(); got != want {
		t.Errorf("Size %d, want %d", got, want)
	}
	st.Pack(ctx)
	if got := size(); got != want {
		t.Errorf("Size after packing %d, want %d", got, want)
	}
	// Append records without sizes, as a writer may leave them.
	var recs []logRecord
	for i, id := range ids {
		recs = append(recs, logRecord{Op: opAppend, Session: "z", Entry: id, Seq: i + 1})
	}
	data, _ := encodeRecords(true, recs)
	st.Release("z")
	os.WriteFile(filepath.Join(st.Root(), "sessions", "z", logName), data, 0o600)
	if got := size(); got != want {
		t.Errorf("Size from a log without sizes %d, want %d", got, want)
	}
}

// TestPackFormat reads back what writePack wrote, refuses an index that
// fails its checksum, and finds a flipped byte in a pack.
func TestPackFormat(t *testing.T) {
	dir := t.TempDir()
	objs := []packObject{
		{sp: spaceContents, hash: hashBytes([]byte("b")), data: []byte("b")},
		{sp: spaceEntries, hash: hashBytes([]byte("a")), data: []byte("a")},
		{sp: spaceContents, hash: hashBytes([]byte("b")), data: []byte("b")}, // a duplicate
	}
	name, err := writePack(nil, dir, objs)
	if err != nil {
		t.Fatal(err)
	}
	p, err := openPack(dir, name)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(p.idx) / idxRecord; n != 2 {
		t.Errorf("%d objects indexed, want 2", n)
	}
	d, _ := digestOf(hashBytes([]byte("a")))
	if _, _, ok := p.find(spaceContents, d); ok {
		t.Error("an envelope found in the content space")
	}
	off, n, ok := p.find(spaceEntries, d)
	if data, _ := p.read(off, n); !ok || string(data) != "a" {
		t.Errorf("read %q", data)
	}
	if errs := verifyPack(p); len(errs) != 0 {
		t.Errorf("a sound pack: %v", errs)
	}
	p.close()
	raw, _ := os.ReadFile(filepath.Join(dir, name+".pack"))
	raw[len(packMagic)+8+1+32+1] ^= 0xff // the first object's byte
	os.WriteFile(filepath.Join(dir, name+".pack"), raw, 0o600)
	p, _ = openPack(dir, name)
	if errs := verifyPack(p); len(errs) != 2 { // the checksum and the object
		t.Errorf("a flipped byte: %v", errs)
	}
	p.close()
	idx, _ := os.ReadFile(filepath.Join(dir, name+".idx"))
	idx[len(idxMagic)+9] ^= 0xff
	os.WriteFile(filepath.Join(dir, name+".idx"), idx, 0o600)
	// A damaged record opens, as git's does, and Verify finds it.
	p, err = openPack(dir, name)
	if err != nil {
		t.Fatalf("a damaged index record: %v", err)
	}
	defer p.close()
	found := false
	for _, err := range verifyPack(p) {
		found = found || errors.Is(err, ErrCorrupt) && strings.Contains(err.Error(), ".idx fails its checksum")
	}
	if !found {
		t.Errorf("verify of a damaged index: %v", verifyPack(p))
	}
	// A damaged header does not open.
	idx[0] ^= 0xff
	os.WriteFile(filepath.Join(dir, name+".idx"), idx, 0o600)
	if _, err := openPack(dir, name); !errors.Is(err, ErrCorrupt) {
		t.Errorf("a damaged index header: %v", err)
	}
}

// TestPackConsolidates: packing after every few appends leaves a number
// of packs that grows with the logarithm of the store, and every entry
// still reads.
func TestPackConsolidates(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	st.Create(ctx, agentsession.Header{ID: "s"})
	var ids []string
	for round := range 64 {
		for i := range 4 {
			ids = append(ids, mustAppend(t, st, "s", item(fmt.Sprintf("%d-%d", round, i))))
		}
		if _, err := st.Pack(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(st.objs.packList()); n > 8 {
		t.Errorf("%d packs after 64 packings, want a handful", n)
	}
	st.Close()
	ro, _ := Open(root, WithReadOnly())
	defer ro.Close()
	s, err := ro.Open(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if _, ok := s.Entry(id); !ok {
			t.Fatalf("entry %s lost to consolidation", id)
		}
	}
	if rep, err := ro.Verify(ctx); err != nil || !rep.OK() {
		t.Errorf("verify after consolidation: %v %v", err, rep.Problems)
	}
}

// TestSweepFallsBack: a sweep that finds the first copy of an object it
// keeps corrupt takes another, loose or packed, rather than losing it.
func TestSweepFallsBack(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []string{"corrupt loose, good packed", "corrupt in one pack, good in another"} {
		t.Run(tc, func(t *testing.T) {
			root := t.TempDir()
			st, _ := Open(root)
			st.Create(ctx, agentsession.Header{ID: "s"})
			id := mustAppend(t, st, "s", item("precious"))
			st.Release("s")
			if _, err := st.Pack(ctx); err != nil {
				t.Fatal(err)
			}
			lp, _ := st.objs.loosePath(spaceEntries, id)
			os.MkdirAll(filepath.Dir(lp), 0o755)
			if tc == "corrupt loose, good packed" {
				os.WriteFile(lp, []byte("not the envelope"), 0o600)
			} else {
				// A second pack holding the same envelope, then the first
				// copy broken in place.
				first := st.objs.packList()[0]
				data, _ := st.objs.read(spaceEntries, id)
				if _, err := writePack(nil, st.objs.packDir(), []packObject{{sp: spaceEntries, hash: id, data: data}}); err != nil {
					t.Fatal(err)
				}
				d, _ := digestOf(id)
				off, _, _ := first.find(spaceEntries, d)
				f, _ := os.OpenFile(first.path, os.O_WRONLY, 0)
				f.WriteAt([]byte{'!'}, off)
				f.Close()
				st.objs.reloadPacks(true)
			}
			if _, err := st.Sweep(ctx, 0); err != nil {
				t.Fatal(err)
			}
			st.Close()
			ro, _ := Open(root, WithReadOnly())
			defer ro.Close()
			if _, err := ro.objs.read(spaceEntries, id); err != nil {
				t.Errorf("the kept envelope after the sweep: %v", err)
			}
			if _, err := ro.Open(ctx, "s"); err != nil {
				t.Errorf("the session after the sweep: %v", err)
			}
		})
	}
}

// TestAutoPack: a writing store packs its loose objects on its own.
func TestAutoPack(t *testing.T) {
	old := autoPackLoose
	autoPackLoose = 0
	t.Cleanup(func() { autoPackLoose = old })
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	st.Create(ctx, agentsession.Header{ID: "s"})
	for i := range 20 {
		mustAppend(t, st, "s", item(fmt.Sprint(i)))
	}
	st.Close()
	loose := 0
	for _, sp := range []space{spaceEntries, spaceContents} {
		st.objs.eachLoose(sp, func(string, string, os.FileInfo, bool) error { loose++; return nil })
	}
	packs, _ := filepath.Glob(filepath.Join(st.objs.packDir(), "*.idx"))
	if loose != 0 || len(packs) == 0 {
		t.Errorf("after Close: %d loose objects, %d packs", loose, len(packs))
	}
}

// TestPackCloseWhileReading: a pack closed while lookups and walks are
// in its mapped index waits for them, and a lookup after finds nothing.
func TestPackCloseWhileReading(t *testing.T) {
	dir := t.TempDir()
	var objs []packObject
	for i := range 2000 {
		b := []byte(fmt.Sprint(i))
		objs = append(objs, packObject{sp: spaceContents, hash: hashBytes(b), data: b})
	}
	name, err := writePack(nil, dir, objs)
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		p, err := openPack(dir, name)
		if err != nil {
			t.Fatal(err)
		}
		d, _ := digestOf(objs[1234].hash)
		var wg sync.WaitGroup
		for range 4 {
			wg.Add(2)
			go func() {
				defer wg.Done()
				for range 200 {
					p.find(spaceContents, d)
				}
			}()
			go func() {
				defer wg.Done()
				p.each(func(space, string, int64, int64) error { return nil })
			}()
		}
		p.close()
		wg.Wait()
		if _, _, ok := p.find(spaceContents, d); ok {
			t.Fatal("a closed pack answered a lookup")
		}
	}
}

// TestDamagedIndexLength: an index record whose length or offset lies
// outside its pack is reported as corrupt, not allocated, by a read and
// by Verify.
func TestDamagedIndexLength(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	ids := fill(t, st, "s", 2)
	st.Close()
	st, _ = Open(root)
	if _, err := st.Pack(ctx); err != nil {
		t.Fatal(err)
	}
	st.Close()
	idxs, _ := filepath.Glob(filepath.Join(root, "objects", "pack", "*.idx"))
	for _, p := range idxs {
		data, _ := os.ReadFile(p)
		for i := len(idxMagic) + 8; i+idxRecord <= len(data)-2*sha256.Size; i += idxRecord {
			for k := 0; k < 8; k++ {
				data[i+1+sha256.Size+8+k] = 0x7f // a length of about 2^63
			}
		}
		os.WriteFile(p, data, 0o600)
	}
	r, _ := Open(root, WithReadOnly())
	defer r.Close()
	if _, err := r.objs.read(spaceEntries, ids[0]); !errors.Is(err, ErrCorrupt) {
		t.Errorf("a read through a damaged length: %v", err)
	}
	if rep, err := r.Verify(ctx); err != nil || rep.OK() {
		t.Errorf("verify: %v %v", err, rep.Problems)
	}
}
